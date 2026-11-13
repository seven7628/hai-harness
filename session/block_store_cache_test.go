package session

// blockStore 缓存的生命周期（2026-11-12，P2）的回归。
//
// 三条不变量：
//
//  1. **用完就归还** —— 清扫会对**每个** record 打开 sidecar，不归还就全常开。
//     实测（改之前）：bridge 打开 **395 个 sidecar / 1184 个 fd**，
//     而 UI 只开了 15 个会话 ⇒ 不是会话多，是**清扫开了不还**。
//  2. **pin 住的实例不可淘汰** —— 后台 compact 在释放 record 锁之后继续用它
//     （compact_scheduler.go 的 rewriteCompact 阶段），被关掉就是 use-after-close。
//  3. **超预算按 LRU 淘汰未 pin 的**，且淘汰在后台做（Close 会 fsync 整个
//     sidecar —— 提交路径延迟敏感）。
//
// 第 1 条是这个文件的主目标：它把「fd 与索引内存无上限增长」变成有界。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cacheSize 返回当前缓存的实例数（测试用）。
func cacheSize() int {
	blockStoreMu.Lock()
	defer blockStoreMu.Unlock()
	return len(blockStoreCache)
}

// cachePin 返回某条目的 pin 计数（测试用）。
func cachePin(recordPath string) int {
	blockStoreMu.Lock()
	defer blockStoreMu.Unlock()
	if e, ok := blockStoreCache[recordPath]; ok {
		return e.pin
	}
	return -1
}

// makeStore 造一个带几个块的 sidecar，返回 record 路径。
func makeStore(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name+".record.json")
	bs, err := openBlockStore(blockStorePath(path))
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		if err := bs.put(fmt.Sprintf("%d", i), []byte("payload-"+name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	if err := bs.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBlockStoreCacheAcquireRelease 归还后实例仍可复用，但 pin 归零。
func TestBlockStoreCacheAcquireRelease(t *testing.T) {
	dir := t.TempDir()
	path := makeStore(t, dir, "s1")

	bs, err := acquireBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cachePin(path); got != 1 {
		t.Fatalf("acquire 后 pin=%d want 1", got)
	}
	// 再 acquire 同一个 → pin 递增、实例复用（不重开）。
	bs2, err := acquireBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if bs2 != bs {
		t.Fatal("同一路径应复用同一实例（重开会白扫索引）")
	}
	if got := cachePin(path); got != 2 {
		t.Fatalf("两次 acquire 后 pin=%d want 2", got)
	}
	releaseBlockStore(path, bs)
	releaseBlockStore(path, bs2)
	if got := cachePin(path); got != 0 {
		t.Fatalf("归还后 pin=%d want 0", got)
	}
	// 实例仍在缓存里（复用收益），但已可被淘汰。
	if cacheSize() == 0 {
		t.Fatal("归还后实例应留在缓存里复用")
	}
}

// TestBlockStoreCachePinnedNotEvicted pin 住的实例不会被后台淘汰。
func TestBlockStoreCachePinnedNotEvicted(t *testing.T) {
	dir := t.TempDir()
	path := makeStore(t, dir, "s1")

	bs, err := acquireBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// 制造「超预算」：把预算临时压到 0（测试专用）。
	old := blockStoreCacheBudgetBytes
	blockStoreCacheBudgetBytes = 0
	defer func() { blockStoreCacheBudgetBytes = old }()

	evictBlockStoresIfNeeded()
	if cacheSize() != 1 {
		t.Fatalf("pin 住的实例被淘汰了（剩 %d 个）", cacheSize())
	}
	if cachePin(path) != 1 {
		t.Fatalf("pin 计数被改动：%d", cachePin(path))
	}
	releaseBlockStore(path, bs)
}

// TestBlockStoreCacheEvictsLRU 超预算时按 LRU 淘汰未 pin 的。
func TestBlockStoreCacheEvictsLRU(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		p := makeStore(t, dir, fmt.Sprintf("s%d", i))
		paths = append(paths, p)
		bs, err := acquireBlockStore(p)
		if err != nil {
			t.Fatal(err)
		}
		releaseBlockStore(p, bs)
	}
	if cacheSize() < 4 {
		t.Fatalf("夹具没建够：%d", cacheSize())
	}

	old := blockStoreCacheBudgetBytes
	blockStoreCacheBudgetBytes = 0
	defer func() { blockStoreCacheBudgetBytes = old }()
	evictBlockStoresIfNeeded()

	if cacheSize() != 0 {
		t.Fatalf("预算为 0 时应淘汰全部未 pin 实例，剩 %d 个", cacheSize())
	}
}

// TestSweepDoesNotLeakSidecarHandles 清扫**不留下**永久占着的 sidecar。
//
// 这是本轮修复的主目标：改之前，446 个 record ⇒ 446 个 sidecar 常开
// （实测 395 个 sidecar / 1184 个 fd，而 UI 只开了 15 个会话）。
//
// 注意语义：**unpin ≠ 出缓存** —— 实例留在缓存里是为了复用（重开要白扫索引，
// 实测 78ms）。真正的修复是「用完就 unpin」⇒ 它们**可以被 LRU 淘汰**。
// 改之前的 pin 语义不存在 ⇒ 实例永远出不去 ⇒ 无上限增长。
func TestSweepDoesNotLeakSidecarHandles(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	const n = 6
	paths := make([]string, 0, n)
	for i := 0; i < n; i++ {
		wsKey, sid := "ws", fmt.Sprintf("s%d", i)
		path, err := store.RecordPath(wsKey, sid)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		// 造一个「旧形态」record：**带内嵌块**（会真的走「搬进 sidecar」那条路
		// ⇒ 真的会开 sidecar）+ 带 messages + 旧的 v1 checksum 口径。
		//
		// 注意：blocks 必须是**非空**数组 —— 空数组时 needMigrate 为假，
		// 根本不会开 sidecar ⇒ 测不到泄漏（这坑踩过一次）。
		big := strings.Repeat("内容", 256)
		view := []byte(`{"schema_version":1,"blocks":[{"kind":"assistant","id":1,"text":"` + big + `"},` +
			`{"kind":"tool","id":2,"name":"bash","args":"{}","result":"` + big + `"}],"run_nodes":[],"turns":[]}`)
		old := NewSessionRecord(wsKey, sid, 7, 42, 0,
			State{Messages: bigMessages(8)}, view)
		old.Checksum = ""
		raw, err := marshalRecordEnvelope(old)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	res, err := SweepRecordFiles(root, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stripped != n {
		t.Fatalf("瘦身了 %d 个 want %d", res.Stripped, n)
	}
	// ① 每个被清扫打开的实例都必须已 unpin（pin>0 ⇒ LRU 永远淘汰不掉 ⇒ 无上限增长）。
	for _, p := range paths {
		if pin := cachePin(p); pin != 0 {
			t.Fatalf("清扫留下了 pin=%d 的实例（%s）—— 永远淘汰不掉", pin, filepath.Base(p))
		}
	}
	// ② 它们必须真的能被淘汰掉（把预算压到 0 强制淘汰）。
	old := blockStoreCacheBudgetBytes
	blockStoreCacheBudgetBytes = 0
	defer func() { blockStoreCacheBudgetBytes = old }()
	evictBlockStoresIfNeeded()
	if cacheSize() != 0 {
		t.Fatalf("清扫打开的实例淘汰不掉（剩 %d 个）—— 句柄泄漏没修好", cacheSize())
	}
}

// TestCommitPathDoesNotLeakHandles 提交路径同样不留下常开句柄。
func TestCommitPathDoesNotLeakHandles(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	wsKey, sid := "ws", "s1"
	path, err := store.RecordPath(wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	view := []byte(`{"schema_version":1,"blocks":[],"run_nodes":[],"turns":[],"usage":{},"todos":[]}`)
	for i := 0; i < 3; i++ {
		rec := NewSessionRecord(wsKey, sid, uint64(i+1), 1, 0,
			State{Messages: bigMessages(4)}, view)
		if err := store.CommitRecord(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
	}
	// 提交后实例可留在缓存里复用（这是刻意的：重开要白扫索引），
	// 但 **pin 必须归零** —— 否则它永远不会被淘汰。
	if got := cachePin(path); got != 0 {
		t.Fatalf("提交后 pin=%d want 0（pin 归零才可能被淘汰）", got)
	}
}
