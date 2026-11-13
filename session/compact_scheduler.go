package session

// 后台 sidecar 回收（compact）调度。
//
// 为什么必须挪出提交路径：compact 要顺序重写「每个 id 的最新版本」，成本
// O(有效字节)。真实会话实测 0.45GB/185MB 有效 → 939ms；3.45GB 约 7s。它原来
// 内联在 CommitRecord 里，且持着（旧实现的）全局锁 —— 一次回收会把**所有
// 会话**的提交同时挡住 1–7s，这是 30s 超时里最大的单次尖峰之一。
//
// 现在分两段：
//   - 提交路径只做 O(1) 的阈值判断并投递任务；
//   - 回收在三段里跑（见 compact_concurrent.go）：快照（持锁）→ 无锁重写 →
//     合流增量 + 原子切换（持锁）。锁只在两端被持有，中间不挡提交。
//
// 失败语义：回收失败不影响任何提交的正确性（sidecar 原样保留，下次再试）；
// 进程中途退出会留下 <sidecar>.compact.tmp，下次回收 O_TRUNC 覆盖它。

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var compactInFlight = struct {
	sync.Mutex
	m map[string]bool
}{m: make(map[string]bool, 8)}

// SetBackgroundCompact 控制是否把 sidecar 回收放到后台（默认开）。
// 关闭后回到「提交内同步回收」，用于灰度/回退。
func (s *FileStorage) SetBackgroundCompact(on bool) {
	if s == nil {
		return
	}
	s.bgCompact.Store(on)
}

func (s *FileStorage) backgroundCompactEnabled() bool {
	return s == nil || s.bgCompact.Load()
}

// scheduleCompact 为该 record 投递一次后台回收（同 record 单飞）。
func scheduleCompact(recordPath string) {
	key := filepath.Clean(recordPath)

	compactInFlight.Lock()
	if compactInFlight.m[key] {
		compactInFlight.Unlock()
		return
	}
	compactInFlight.m[key] = true
	compactInFlight.Unlock()

	go func() {
		defer func() {
			compactInFlight.Lock()
			delete(compactInFlight.m, key)
			compactInFlight.Unlock()
		}()
		runBackgroundCompact(key)
	}()
}

// runBackgroundCompact 执行一次并发回收：快照 → 无锁重写 → 合流 + 切换。
func runBackgroundCompact(key string) {
	// ① 快照（持锁，µs 级）
	unlock := lockRecord(key)
	bs, err := acquireBlockStore(key)
	if err != nil || bs == nil || !bs.needsCompact() {
		if bs != nil {
			releaseBlockStore(key, bs)
		}
		unlock()
		return
	}
	// **bs 在下面会被锁外使用**（② 阶段）⇒ 必须一直 pin 着，
	// 否则后台回收器可能在这期间把它关掉 ⇒ use-after-close。
	snap, err := bs.beginCompact()
	if err != nil {
		releaseBlockStore(key, bs)
		unlock()
		return
	}
	unlock()

	// ② 重写（不持锁）：提交可以继续往旧文件追加
	cw, err := bs.rewriteCompact(snap)
	if err != nil {
		releaseBlockStore(key, bs)
		return // rewriteCompact 内部已清理临时文件
	}

	// ③ 合流 + 原子切换（持锁，O(增量)）
	unlock = lockRecord(key)
	defer unlock()

	// 身份校验：期间实例被换掉（会话被删除/外部改动）→ 放弃，正式文件不动。
	if !blockStoreStillCached(key, bs) {
		cw.abort()
		releaseBlockStore(key, bs)
		return
	}
	before := bs.nBytes
	start := time.Now()
	if ferr := bs.finishCompact(snap, cw); ferr != nil {
		if !isCompactAborted(ferr) {
			fmt.Fprintf(os.Stderr, "[checkpoint] background compact %s: %v\n", filepath.Base(key), ferr)
		}
		releaseBlockStore(key, bs)
		return
	}
	// 读完之后才归还：归还后实例可能被后台回收器关掉 ⇒ 不能再碰 bs。
	after := bs.nBytes
	releaseBlockStore(key, bs)
	fmt.Fprintf(os.Stderr, "[checkpoint] background compact %s: %s → %s (%.1fx) in %s\n",
		filepath.Base(key), humanBytes(before), humanBytes(after),
		float64(before)/float64(max64(after, 1)), time.Since(start).Round(time.Millisecond))
}

func isCompactAborted(err error) bool {
	for err != nil {
		if err == errCompactAborted {
			return true
		}
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}

// WaitBackgroundCompacts 等待所有在跑的后台回收结束（进程退出/测试清理用）。
// 超时返回 false（回收本身幂等，被杀掉只会留下一个临时文件）。
func WaitBackgroundCompacts(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		compactInFlight.Lock()
		n := len(compactInFlight.m)
		compactInFlight.Unlock()
		if n == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// compactPending 仅供测试：该 record 是否有回收任务在跑。
func compactPending(recordPath string) bool {
	compactInFlight.Lock()
	defer compactInFlight.Unlock()
	return compactInFlight.m[filepath.Clean(recordPath)]
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
