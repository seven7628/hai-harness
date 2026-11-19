package session

// sidecar **死字节回收**（2026-11-19，爬 3）。
//
// ## 为什么需要
//
// sidecar 是 **append-only** 的块仓：同一 id 的**新版本**追加到文件尾，旧版本留在原地。
// 子智能体卡片在 running 期每次事件都重写同一 block ⇒ 同一 id 的历史版本无限堆积。
//
// 实测（2026-11-19）：一个大会话 828.7MB 里只有 111.2MB 是活的 —— **死字节 86.6%**；
// 另两个样本 0.0%（压过）与 21.1%。
//
// 回收机制**本来就有**（`blockStore.needsCompactAt`：文件 ≥32MB、死 ≥32MB、死 ≥ 活×0.5），
// 但**只在提交时触发**（`store_record.go` 的 needsCompact → scheduleCompact）
// ⇒ **冷会话 / 不再提交的永远没人管**。
//
// ## 为什么不影响 checkpoint / 会话恢复 / UI 视图
//
//  1. `compact()` **逐字节搬运**被引用的块：
//     压缩块不解压不校验（字节级搬运），未压缩块先校验摘要再搬
//     ⇒ 被引用块的 payload 摘要**不变**
//     ⇒ record 的 `$blocks_ref`（ids + `blockset1:` sum）**不变**
//     ⇒ checkpoint / 恢复 / 视图**零影响**。
//  2. `compact()` 是 tmp → fsync → **原子 rename** → 重建索引 → 再落索引
//     （索引在 sidecar 之后写 ⇒ 崩溃时索引超前 ⇒ 下次打开安全回退）。
//  3. 回收后**用真实读路径重读一次 record**（`LoadRecordForView`）：
//     读路径会校验 count 与 `blockset1:` sum
//     （`resolveBlocksRefMapped` / `resolveBlocksRefSpans`）
//     ⇒ 不符就报错。**这不是「我们觉得没问题」，是跑一遍真实校验。**
//
// ## 为什么不复用 runBackgroundCompact
//
// 三段式（快照 → 无锁重写 → 合流切换）是为了**不挡提交**（真实会话 3.45GB 约 7s）。
// 清扫在启动后台跑，这里**持 record 锁**跑完整个重写 ⇒ 提交最多等这一次（实测 ~2s，
// 提交超时是 30s），换来「重写期间不可能有并发提交」这个更强的保证。
// 提交路径那边仍走 `runBackgroundCompact`，不受影响。
//
// ## 幂等
//
// 回收后 `needsCompact()` 变 false ⇒ 下一轮直接跳过 ⇒ 不需要额外索引来记「清过了」。

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SweepCompactResult 是死字节回收的结果。
type SweepCompactResult struct {
	Files     int   // 回收过的 sidecar 数
	Skipped   int   // 索引判定「清过且没动过」而跳过的
	Before    int64 // 回收前总字节
	After     int64 // 回收后总字节
	OrphanTmp int   // 清掉的孤儿 .compact.tmp 数
	VerifyErr int   // 回收后重读校验失败的文件数（数据没坏，但这一版 record 引用有问题）
	Elapsed   time.Duration
}

// Saved 返回省下的字节数。
func (r SweepCompactResult) Saved() int64 { return r.Before - r.After }

// SweepCompactMinBytesDefault 是「值得回收」的体积下限（默认 32MB）。
//
// 复用 block store 自己的门槛（`compactMinBytes`）：`needsCompactAt` 本来就要求
// 文件 ≥ 它，小于它的 sidecar 连打开都不必（打开可能要全量扫一遍重建索引）。
const SweepCompactMinBytesDefault = 32 << 20

// sweepOrphanTmpAge 是「孤儿 .compact.tmp」的可删年龄。
//
// 为什么要有年龄门槛：`.compact.tmp` 可能是**正在跑**的回收写的
// ⇒ 删掉会让那次回收失败（它最后要 rename 那个 tmp）。
// 只删**很久以前**的 ⇒ 正在跑的不会被误伤。
const sweepOrphanTmpAge = 2 * time.Hour

// SweepBlockStoreDeadBytes 回收 sidecar 里的死字节（被后续版本覆盖的历史记录）。
//
// 参数：
//   - root：会话根目录（~/.go-code/sessions）
//   - minBytes：体积下限（小于它的不碰：打开可能要全量扫一遍重建索引，不划算）
//   - pause：每个文件之间的间隔（限速，避免与正常使用抢 IO）
//   - ix：清扫索引（可为 nil）。非 nil 时「上次清过且此后没动过」的直接跳过。
//
// 返回值里的 VerifyErr > 0 表示**有文件回收后重读校验没过** —— 数据本身没坏
// （compact 是原子的，失败时原文件还在），但那一版 record 引用的块集合有问题，
// 需要人工看一眼。调用方应打日志。
func SweepBlockStoreDeadBytes(root string, minBytes int64, pause time.Duration, ix *SweepIndex) (SweepCompactResult, error) {
	res := SweepCompactResult{}
	start := time.Now()
	if root == "" {
		return res, fmt.Errorf("sweep compact: empty root")
	}
	st, err := NewSessionRecordStorage(root)
	if err != nil {
		return res, fmt.Errorf("sweep compact: open storage: %w", err)
	}
	ctx := context.Background()

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 跳过不可读的目录项，不中断整轮
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		// ① 孤儿 .compact.tmp（回收被中断的残留）
		if strings.HasSuffix(name, ".record.json.blocks.compact.tmp") {
			if fi, serr := os.Stat(path); serr == nil && time.Since(fi.ModTime()) > sweepOrphanTmpAge {
				if rerr := os.Remove(path); rerr == nil {
					res.OrphanTmp++
				}
			}
			return nil
		}
		if !strings.HasSuffix(name, ".record.json.blocks") {
			return nil
		}
		fi, serr := os.Stat(path)
		if serr != nil {
			return nil
		}
		// ② 体积门槛（避免为小文件付「打开 = 全量扫一遍重建索引」的代价）
		if fi.Size() < minBytes {
			return nil
		}
		// ③ 索引：清过且没动过 → 跳过（连文件都不打开）
		if ix.Skip(path, fi.Size(), fi.ModTime().UnixNano()) {
			res.Skipped++
			return nil
		}

		recordPath := strings.TrimSuffix(path, blockStoreSuffix)
		before, after, cerr := compactOneSidecar(recordPath)
		if cerr != nil {
			return nil // 单个文件失败不影响整轮
		}
		if before == after {
			return nil // 回收没省下东西（本来就没有死字节）
		}
		res.Files++
		res.Before += before
		res.After += after

		// ④ 验证：用**真实读路径**重读一次 record。
		//    读路径内部会校验 count 与 blockset1 sum ⇒ 不符就报错。
		//    （这一步在锁外做：LoadRecordForView 自己会取锁，锁不可重入。）
		wsKey, sid := splitRecordPath(root, recordPath)
		if wsKey == "" || sid == "" {
			return nil
		}
		rec, verr := st.LoadRecordForView(ctx, wsKey, sid)
		if verr != nil {
			res.VerifyErr++
			fmt.Fprintf(os.Stderr, "[checkpoint] 回收后重读校验失败: %s: %v\n", recordPath, verr)
			return nil
		}
		rec.ReleaseView()

		if nfi, nerr := os.Stat(path); nerr == nil {
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

// compactOneSidecar 回收单个 sidecar 的死字节，返回回收前后的字节数。
//
// 锁的粒度必须是 **record 路径**（不是 sidecar 路径）：
// `acquireBlockStore` / `lockRecord` 都以 record 路径为 key
// ⇒ 用 sidecar 路径加锁会与提交路径**用不同的锁**，等于没锁。
func compactOneSidecar(recordPath string) (before, after int64, err error) {
	unlock := lockRecord(recordPath)
	defer unlock()

	bs, berr := acquireBlockStore(recordPath)
	if berr != nil {
		return 0, 0, berr
	}
	defer releaseBlockStore(recordPath, bs)

	if !bs.needsCompact() {
		return bs.nBytes, bs.nBytes, nil
	}
	before = bs.nBytes
	if cerr := bs.compact(); cerr != nil {
		return before, before, cerr
	}
	return before, bs.nBytes, nil
}

// splitRecordPath 从 record 路径拆出 (workspaceKey, sessionID)。
//
// 路径形如 `<root>/<wsKey>/<sid>.record.json` ⇒ wsKey = 父目录名，
// sid = 文件名去掉 `.record.json`。
func splitRecordPath(root, recordPath string) (wsKey, sid string) {
	abs, err := filepath.Abs(recordPath)
	if err != nil {
		return "", ""
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", ""
	}
	rel, err := filepath.Rel(rootAbs, abs)
	if err != nil {
		return "", ""
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 {
		return "", ""
	}
	name := strings.TrimSuffix(parts[1], ".record.json")
	if name == parts[1] || name == "" {
		return "", ""
	}
	return parts[0], name
}
