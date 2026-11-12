package session

// record 的**瘦身 + 形态迁移清扫**（2026-11-11，CHECKPOINT_V3_DESIGN §6 P3-1）。
//
// ## 为什么需要
//
// P3-1 起新写入的 record 不再携带 SDK 上下文（`sdk_state.messages`）——
// 权威来源改成了**会话 jsonl 的最后一条 state 行**（SDK 自己在每个轮次检查点
// 落盘，且尊重压缩点）。
//
// 但**存量** record 仍带着那份副本，而且块还是**内嵌形态**。实测全量 447 个会话：
//
//	                          formal      .bak       合计
//	sdk_state.messages        0.61 GB    0.61 GB    1.22 GB   ← 纯冗余，权威在 jsonl
//	view_checkpoint.blocks    1.30 GB    1.29 GB    2.59 GB   ← 内嵌形态 = formal/.bak 各一份
//
// 死会话不会再被提交 → 那 4GB 会永远留在盘上。
//
// ## 清扫做两件事
//
// ① **去掉 SDK 上下文**：record 只留 manifest（revision/seq/…）+ 标量
//    （usage/cost/todo/pending）。恢复改读会话 jsonl。
//
// ② **内嵌块 → sidecar 形态**：把 `view_checkpoint.blocks` 的内嵌数组搬进
//    `<sid>.record.json.blocks`，record 只留引用（几十字节）。
//    内嵌形态的代价是**块内容在 formal 和 .bak 里各存一份** —— 搬进 sidecar
//    后只存一份。
//
// ## 安全性
//
//   · **按路径加锁**（与提交路径同一把 `lockRecord`）→ 不会与并发提交交错；
//   · ① 只改一个字段；② 走的是提交路径同一条编码路径（`encodeRecordEnvelope`
//     + `checksumFieldSpan`）→ 产出字节与「用同一份 record 提交一次」逐字节相同；
//   · **revision 不变** → 不与「同 revision 不同 payload」的幂等判定冲突；
//   · checksum 按字节口径重算（v2）→ 读侧的完整性校验照常通过；
//   · 原子替换（临时文件 + fsync + rename），失败保持原文件不动；
//   · `.bak` 改写成 formal 的**副本**（KB 级）：它退化成「envelope 的冗余副本」，
//     保护的是「formal 这一份文件被磁盘损坏」；内容层面的回退由**事件日志重放**
//     承担（desktop/bridge 的 migrateLegacyToCheckpoint 已经在跑这条路）。
//
// ## 不做什么
//
//   · 不动 sidecar 里的块内容（那是 UI 的物化视图，与本轮无关）；
//   · 不动会话 jsonl：它的 state 行才是权威，删不得；
//   · 不删 `.bak`：它仍是单文件损坏时的兜底。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RecordSweepResult 是 record 清扫的结果。
type RecordSweepResult struct {
	Files    int   // 扫到的 record 文件数
	Stripped int   // 实际改写的（含 .bak）
	Migrated int   // 内嵌块搬进 sidecar 的个数
	Before   int64 // 清扫前总字节
	After    int64 // 清扫后总字节
	Elapsed  time.Duration
}

// Saved 返回省下的字节数。
func (r RecordSweepResult) Saved() int64 { return r.Before - r.After }

// SweepRecordFiles 扫描 root 下所有 *.record.json，把它们瘦身为
// 「manifest + 标量 + 块引用」，并同步瘦身 .bak。
//
// 参数：
//   - root：会话根目录（~/.go-code/sessions）
//   - pause：每个文件之间的间隔（限速，避免与正常使用抢 IO）
//
// 遍历 root 下**所有** .record.json（递归）—— 主会话的 <sid>.record.json 与
// （可能的）子目录形式都在内。
func SweepRecordFiles(root string, pause time.Duration) (RecordSweepResult, error) {
	res := RecordSweepResult{}
	start := time.Now()
	if root == "" {
		return res, fmt.Errorf("sweep record: empty root")
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 跳过不可读的目录项，不中断整轮清扫
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".record.json") {
			return nil // .bak 跟着 formal 一起处理
		}
		res.Files++
		fi, serr := os.Stat(path)
		if serr != nil {
			return nil
		}
		res.Before += fi.Size()

		changed, migrated, merr := migrateRecordFile(path)
		if merr != nil {
			return nil // 单个文件失败不影响整轮
		}
		if changed {
			res.Stripped++
		}
		if migrated {
			res.Migrated++
		}
		if nfi, nerr := os.Stat(path); nerr == nil {
			res.After += nfi.Size()
		} else {
			res.After += fi.Size()
		}
		// .bak 的体积也算进账（它随 formal 一起被改写）。
		if bfi, berr := os.Stat(path + ".bak"); berr == nil {
			res.Before += bfi.Size()
			if nfi, nerr := os.Stat(path + ".bak"); nerr == nil {
				res.After += nfi.Size()
			}
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

// migrateRecordFile 把一个 record 瘦身为「manifest + 标量 + 块引用」。
//
// 返回 (是否改写, 是否把内嵌块搬进了 sidecar, 错误)。
func migrateRecordFile(path string) (bool, bool, error) {
	// 与提交路径同一把锁：不会和并发提交交错。
	unlock := lockRecord(path)
	defer unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return false, false, err
	}
	// 直接解到 SessionRecord：ViewCheckpoint 是 json.RawMessage → 块引用
	// 形态原样保留（不会被展开成完整数组）。
	var rec SessionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return false, false, err // 结构异常 → 不动（宁可留大文件，不冒损坏的风险）
	}
	needStrip := len(rec.SDKState.Messages) > 0
	elems, inline := findBlocksField(rec.ViewCheckpoint)
	needMigrate := inline && len(elems) > 0
	if !needStrip && !needMigrate {
		return false, false, nil // 已经瘦身过了
	}

	// ① 去掉 SDK 上下文（权威在会话 jsonl 的最后 state 行）。
	rec.SDKState.Messages = nil

	// ② 内嵌块 → sidecar（走提交路径同一条路径：refBlocks 负责写块 + 建引用）。
	if needMigrate {
		bs, berr := acquireBlockStore(path)
		if berr != nil {
			return false, false, berr
		}
		newView, _, rerr := refBlocks(bs, rec.ViewCheckpoint)
		if rerr != nil {
			return false, false, rerr
		}
		if ferr := bs.flush(); ferr != nil {
			return false, false, ferr
		}
		rec.ViewCheckpoint = newView
	}

	out, err := encodeStrippedRecord(&rec)
	if err != nil {
		return false, false, err
	}
	dir := filepath.Dir(path)
	if err := writeAtomicFile(dir, filepath.Base(path), out, 0o600); err != nil {
		return false, false, err
	}

	// ③ .bak 改成 formal 的副本（KB 级）。
	//
	// 为什么不是「保留它自己的内嵌块」：那样 .bak 还是 MB 级（省不下），
	// 而且内嵌块在 formal/.bak 各一份本来就是纯浪费。
	// 为什么不是「保留上一代语义」：sidecar 按块 id 存**最新版**，
	// .bak 的引用必然解析到当前版本的块 —— 与其让它指向一堆解析不到的东西，
	// 不如让它成为 formal 的冗余副本：它保护的是「formal 这一份文件被损坏」。
	// 内容层面的回退由事件日志重放承担（更权威，也更完整）。
	if _, serr := os.Stat(path + ".bak"); serr == nil {
		if werr := writeAtomicFile(dir, filepath.Base(path+".bak"), out, 0o600); werr != nil {
			return true, needMigrate, werr
		}
	}
	return true, needMigrate, nil
}

// encodeStrippedRecord 重新编码 record 并按字节口径重算 checksum。
//
// 产出的字节与「用同一份 record 走一次提交」**逐字节相同** —— 走的是同一条
// 编码路径（encodeRecordEnvelope + checksumFieldSpan）。
func encodeStrippedRecord(rec *SessionRecord) ([]byte, error) {
	prefix, view, suffix, err := encodeRecordEnvelope(rec)
	if err != nil {
		return nil, err
	}
	span, ok := checksumFieldSpan(suffix)
	if !ok {
		return nil, fmt.Errorf("%w: cannot locate checksum field", ErrRecordChecksum)
	}
	// checksum 口径与提交路径逐字节一致：prefix + view + suffix[:字段起点] + '}'
	sumWriter := sha256.New()
	for _, part := range [][]byte{prefix, view, suffix[:span.fieldStart], {'}'}} {
		if _, err := sumWriter.Write(part); err != nil {
			return nil, fmt.Errorf("hash session record: %w", err)
		}
	}
	sum := checksumV2Prefix + hex.EncodeToString(sumWriter.Sum(nil))

	out := make([]byte, 0, len(prefix)+len(view)+len(suffix)+len(sum)+16)
	out = append(out, prefix...)
	out = append(out, view...)
	out = append(out, `,"checksum":"`...)
	out = append(out, sum...)
	out = append(out, `"}`...)
	out = append(out, '\n')
	return out, nil
}
