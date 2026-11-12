package session

// 清扫索引（2026-11-12）。
//
// ## 为什么需要
//
// 清扫本质是**一次性形态迁移**（把 P0–P5' 之前的旧格式搬成新格式），
// 不是常驻工序。但当前实现每次 bridge 启动都全量扫一遍：
//
//	step1 jsonl    walk 3389 个文件 → 124 个 ≥4MB 要整扫（1.28 GB 读）
//	step2 sidecar 477 个文件全 open+Close（Close = fsync + 重写 .idx）
//	step3 record  450 个文件全 ReadFile + json.Unmarshal（167 MB）
//	               限速 sleep 合计 ≈ 105 秒
//
// 这些内容**上一次已经清过了** —— 每启动一次白烧一遍。
//
// ## 判据：指纹
//
// 每个清过的文件记 (size, mtime_ns)。下次清扫先 stat：
//
//	指纹一致 → 自上次清扫以来没动过 → 直接跳过（连文件都不打开）
//	指纹不同 → 动过 → 照常处理
//
// 为什么可靠：jsonl / sidecar 是 append-only（size 变了就是有新内容）；
// record 是原子替换（rename → 新 inode → mtime 必变）。两者合起来足以
// 判定「没动过」。
//
// ## 边界
//
//   · 指纹只用于**跳过**，不用于**决定写什么** —— 真正的幂等判据仍在
//     compactFile / sweepOneBlockStore / migrateRecordFile 内部。索引算错
//     最多多扫一遍，**不会写坏数据**。
//   · version 是**清扫器**的版本（不是数据的版本）：清扫逻辑（判据、
//     幂等规则、产出形态）变更时 +1 —— 旧指纹全部失效，触发一次全量，
//     避免「换判据后老指纹把该清的挡掉」。
//   · 索引文件缺失 / 损坏 / 版本不同 → 视作全部未清过 → 本轮全量（安全）。
//   · 索引只在整轮清扫**结束后**原子落盘一次（不是每文件一次 —— 4300 个
//     文件写 4300 次索引比全扫还贵）。中途退出 → 指纹丢失 → 下次重扫，
//     可接受。
//   · 索引**共享**给三个清扫步骤（键是绝对路径 → 天然不撞）。每步只
//     处理自己那类文件，所以同一个文件不会被两步同时记账。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SweepIndexVersion 是清扫索引的格式版本。
//
// 语义：**清扫器**的版本。清扫逻辑变更时 +1 —— 旧指纹全部作废，触发
// 一次全量重扫（换判据时不会漏）。
const SweepIndexVersion = 1

// sweepFingerprint 是一个文件的身份指纹。
//
// size + mtime_ns 一起用：只比 size 不够（原子替换可能大小不变），
// 只比 mtime 不够（有些文件系统 mtime 粗到秒）。
type sweepFingerprint struct {
	Size    int64 `json:"size"`
	MtimeNs int64 `json:"mtime_ns"`
}

// sweepIndexFile 是索引文件的落盘形态。
type sweepIndexFile struct {
	Version   int                         `json:"version"`
	UpdatedAt string                      `json:"updated_at"`
	Entries   map[string]sweepFingerprint `json:"entries"`
}

// SweepIndex 是清扫索引：记「哪些文件已经清过、且自那时起没动过」。
//
// 用 OpenSweepIndex 构造；nil 指针也可安全使用（所有方法都是 no-op →
// 等价于「没有索引」，即每次全量）。
type SweepIndex struct {
	mu      sync.Mutex
	path    string
	version int
	entries map[string]sweepFingerprint
	seen    map[string]struct{}
}

// OpenSweepIndex 打开（或准备创建）位于 path 的清扫索引。
//
// 读不到 / 解析失败 / 版本不同 → 返回一个**空索引**（视作全部未清过 →
// 本轮全量，安全）。**不返回 error**：索引坏了不该让清扫失败。
func OpenSweepIndex(path string, version int) *SweepIndex {
	ix := &SweepIndex{
		path:    path,
		version: version,
		entries: map[string]sweepFingerprint{},
		seen:    map[string]struct{}{},
	}
	if path == "" {
		return ix
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ix
	}
	var f sweepIndexFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return ix // 坏文件 → 空索引
	}
	if f.Version != version {
		return ix // 版本不同 → 旧指纹全部作废
	}
	for k, v := range f.Entries {
		ix.entries[k] = v
	}
	return ix
}

// Skip 报告该文件是否可以跳过：索引里有它、指纹一致（自上次清扫以来
// 没动过）。
//
// 命中时把路径记进 seen（保证 Flush 时不会被当成「已消失」而剪掉）。
func (ix *SweepIndex) Skip(path string, size int64, mtimeNs int64) bool {
	if ix == nil {
		return false
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	fp, ok := ix.entries[path]
	if !ok || fp.Size != size || fp.MtimeNs != mtimeNs {
		return false
	}
	ix.seen[path] = struct{}{}
	return true
}

// Mark 记下「这个文件已经清过」+ 它的指纹。
//
// 调用点在**处理完成之后** —— 处理可能原子替换文件（新的 size/mtime）。
func (ix *SweepIndex) Mark(path string, size int64, mtimeNs int64) {
	if ix == nil {
		return
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.entries[path] = sweepFingerprint{Size: size, MtimeNs: mtimeNs}
	ix.seen[path] = struct{}{}
}

// Flush 把索引原子落盘。
//
// 只写 entries：本轮没看到的条目要先确认文件还在 —— 在就留（可能是别的
// 清扫种类管的，索引是共享的），不在才剪（会话被删了）。
func (ix *SweepIndex) Flush() error {
	if ix == nil || ix.path == "" {
		return nil
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	out := make(map[string]sweepFingerprint, len(ix.entries))
	for k, v := range ix.entries {
		if _, ok := ix.seen[k]; ok {
			out[k] = v
			continue
		}
		if _, err := os.Stat(k); err == nil {
			out[k] = v
		}
	}
	blob, err := json.Marshal(sweepIndexFile{
		Version:   ix.version,
		UpdatedAt: time.Now().Format(time.RFC3339),
		Entries:   out,
	})
	if err != nil {
		return err
	}
	return writeAtomicFile(filepath.Dir(ix.path), filepath.Base(ix.path), blob, 0o600)
}

// Stats 返回索引里的条目数（测试/日志用）。
func (ix *SweepIndex) Stats() (entries int, seen int) {
	if ix == nil {
		return 0, 0
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return len(ix.entries), len(ix.seen)
}
