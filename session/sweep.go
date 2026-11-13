package session

// 会话文件的**维护清扫**（2026-11-11）。
//
// ## 为什么需要
//
// 会话的自压缩是**被动**的：只在 SaveState 追加 state 行时才检查。
// 于是「已经结束、不会再被写入」的文件永远等不到压缩：
//
//   · 子智能体 journal：实测 2258 个文件 / **13.4 GB**，其中 997 个 > 4MB。
//     每个文件是 52 条 state 行 × 319KB 的**全量上下文快照**（O(N²)），
//     而自压缩原来只看文件体积（24MB）→ 16–22MB 的 journal 永远触发不了。
//   · 主会话 jsonl：实测 498 个 / **6.8 GB**，history 行（86MB/会话）要等到
//     下次 SaveState 才会被「压缩窗口」裁掉。
//
// ⇒ 清扫 = 对**所有**会话文件跑一次既有的压缩逻辑（原子替换、内容不变），
// 把已经攒下的 O(N²) 一次性收掉。
//
// ## 安全性
//
//   · 复用 compactFile —— 它是「临时文件 + fsync + rename」的原子替换，
//     失败时原文件保持不变；
//   · 压缩后**恢复内容逐字节一致**（只保留最后 K 条 state 行 + 首行 +
//     压缩窗口内的 history 行）；
//   · 逐个文件处理 + 限速，不与正常使用抢 IO。

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SweepResult 是清扫的结果。
type SweepResult struct {
	Files     int   // 处理的文件数（超阈值的）
	Compacted int   // 实际压缩了的文件数
	Skipped   int   // 索引判定「清过且没动过」而跳过的文件数
	Before    int64 // 压缩前总字节
	After     int64 // 压缩后总字节
	Elapsed   time.Duration
}

// Saved 返回省下的字节数。
func (r SweepResult) Saved() int64 { return r.Before - r.After }

// SweepSessionFiles 扫描 root 下所有会话 jsonl，对超过 minBytes 的文件执行一次压缩。
//
// 参数：
//   - root：会话根目录（~/.go-code/sessions）
//   - minBytes：文件体积下限（小于它的不碰：重写不划算）
//   - keep：保留最后 K 条 state 行
//   - keepHistory：true = 保留压缩窗口之外的 history 行（不裁剪）
//   - pause：每个文件之间的间隔（限速，避免与正常使用抢 IO）
//   - ix：清扫索引（可为 nil）。非 nil 时，「上次清过且此后没动过」的
//     文件直接跳过 —— 连文件都不打开（见 sweep_index.go）。
//
// 遍历的是 root 下**所有** .jsonl：主会话的 <sid>.jsonl 与子智能体的
// agents/<taskId>.jsonl 用同一套压缩逻辑 ✓。
func SweepSessionFiles(root string, minBytes int64, keep int, keepHistory bool, pause time.Duration, ix *SweepIndex) (SweepResult, error) {
	res := SweepResult{}
	start := time.Now()
	if root == "" {
		return res, fmt.Errorf("sweep: empty root")
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 跳过不可读的目录项，不中断整轮清扫
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		fi, serr := os.Stat(path)
		if serr != nil {
			return nil
		}
		// ① 索引：清过且没动过 → 跳过（连文件都不打开）。
		if ix.Skip(path, fi.Size(), fi.ModTime().UnixNano()) {
			res.Skipped++
			return nil
		}
		if fi.Size() < minBytes {
			return nil
		}
		res.Files++
		res.Before += fi.Size()

		dir, _ := filepath.Split(path)
		fs, ferr := NewFileStore(dir, WithAutoCompact(keep, 0), WithKeepHistory(keepHistory), WithTrimAllHistory())
		if ferr != nil {
			return nil
		}
		ok, cerr := fs.compactFile(path, keep)
		if cerr != nil {
			return nil // 单个文件失败不影响整轮
		}
		if ok {
			res.Compacted++
		}
		if nfi, nerr := os.Stat(path); nerr == nil {
			res.After += nfi.Size()
			// ② 处理完记指纹（compactFile 可能原子替换了文件 → 新 size/mtime）。
			ix.Mark(path, nfi.Size(), nfi.ModTime().UnixNano())
		}
		if pause > 0 {
			time.Sleep(pause)
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	res.Elapsed = time.Since(start)
	return res, nil
}
