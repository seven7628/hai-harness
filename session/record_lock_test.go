package session

// 锁粒度回归：不同 record 之间不得互相阻塞。
//
// 旧实现用 FileStorage 上的一把 sync.Mutex 串行化所有会话的提交与读取，
// 真实故障里表现为「邻座一次 LoadRecord 让本会话提交多等 810ms」。

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestRecordLockIsolatesSessions(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionRecordStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ws := "ws-lock"
	for _, sid := range []string{"s-a", "s-b"} {
		v := testView(testBlock(1, sid))
		if err := store.CommitRecord(ctx, NewSessionRecord(ws, sid, 1, 1, 0, State{}, v)); err != nil {
			t.Fatal(err)
		}
	}
	pathA, err := store.RecordPath(ws, "s-a")
	if err != nil {
		t.Fatal(err)
	}

	// 有人正占着 A 的锁（等价于 A 在跑一次慢提交/LoadRecord）。
	unlock := lockRecord(pathA)

	blocked := make(chan struct{})
	go func() {
		// 读 A 必须排队。
		if _, err := store.LoadRecord(ctx, ws, "s-a"); err != nil {
			t.Errorf("LoadRecord(A): %v", err)
		}
		close(blocked)
	}()
	time.Sleep(20 * time.Millisecond)
	select {
	case <-blocked:
		t.Fatal("A 的锁被占着时，读 A 不该完成")
	default:
	}

	// 读 B 必须不受影响。
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := store.LoadRecord(ctx, ws, "s-b"); err != nil {
			t.Errorf("LoadRecord(B): %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("B 被 A 的锁阻塞了：锁粒度仍然是全局的")
	}

	unlock()
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("释放 A 的锁后，读 A 仍未恢复")
	}

	// 锁表不能泄漏。
	deadline := time.Now().Add(time.Second)
	for recordLockRefs() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := recordLockRefs(); n != 0 {
		t.Fatalf("锁表泄漏 %d 条（引用计数没归零）", n)
	}
}

// TestRecordLockSerializesSameRecord 钉住另一半：同一条 record 必须互斥。
func TestRecordLockSerializesSameRecord(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionRecordStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.RecordPath("ws", "s")
	if err != nil {
		t.Fatal(err)
	}
	unlock := lockRecord(path)
	held := make(chan struct{})
	go func() {
		u := lockRecord(path)
		close(held)
		u()
	}()
	select {
	case <-held:
		t.Fatal("同一条 record 的锁没有互斥")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case <-held:
	case <-time.After(3 * time.Second):
		t.Fatal("释放后第二个持有者仍未拿到锁")
	}
}

// 后台回收：提交只做 O(1) 判断，真正的重写在 goroutine 里完成；
// 关掉开关则回到同步回收（灰度/回退路径）。
func TestCompactRunsInBackground(t *testing.T) {
	t.Cleanup(func() { WaitBackgroundCompacts(30 * time.Second) })
	oldMin := compactMinBytes.Load()
	compactMinBytes.Store(1 << 20)
	defer func() {
		WaitBackgroundCompacts(30 * time.Second) // 等回收跑完再恢复全局门槛（否则是竞态）
		compactMinBytes.Store(oldMin)
	}()

	root := t.TempDir()
	store, err := NewSessionRecordStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	ws, sid := "ws-bg", "s-bg"
	path, err := store.RecordPath(ws, sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	// 构型：200 个小块各写一次 + 一个 1MB 大块写 3 次（条数废弃约 1%，字节废弃约 60%）。
	payloads := make([]string, 0, 201)
	bs, err := openBlockStore(path + ".blocks")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 200; i++ {
		p := testBlock(i, "small")
		payloads = append(payloads, p)
		if err := bs.put(strconv.Itoa(i), []byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	// 内容**不可压缩**（见 incompressible 的注释）：判据是字节占比，压缩会改字节量。
	big := testBlock(1000000, incompressible(1<<20))
	payloads = append(payloads, big)
	for i := 0; i < 3; i++ {
		if err := bs.put("1000000", []byte(big)); err != nil {
			t.Fatal(err)
		}
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	if err := bs.Close(); err != nil {
		t.Fatal(err)
	}
	before := fileSize(t, path+".blocks")

	ct, err := store.CommitRecordTraced(context.Background(), NewSessionRecord(ws, sid, 1, 1, 0, State{}, testView(payloads...)))
	if err != nil {
		t.Fatal(err)
	}
	if !ct.CompactScheduled || ct.Compact != 0 {
		t.Fatalf("回收没有交给后台: scheduled=%v compact=%s", ct.CompactScheduled, ct.Compact)
	}
	// 最终必须被回收（后台跑完）。
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if fileSize(t, path+".blocks") < before*3/4 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("后台回收没有发生：%.1fMB → %.1fMB", float64(before)/1e6, float64(fileSize(t, path+".blocks"))/1e6)
}

// 关掉开关：回收在提交内同步完成。
func TestCompactSynchronousWhenBackgroundDisabled(t *testing.T) {
	oldMin := compactMinBytes.Load()
	compactMinBytes.Store(1 << 20)
	defer func() {
		WaitBackgroundCompacts(30 * time.Second) // 等回收跑完再恢复全局门槛（否则是竞态）
		compactMinBytes.Store(oldMin)
	}()

	root := t.TempDir()
	store, err := NewSessionRecordStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	store.SetBackgroundCompact(false)
	ws, sid := "ws-sync", "s-sync"
	path, err := store.RecordPath(ws, sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	bs, err := openBlockStore(path + ".blocks")
	if err != nil {
		t.Fatal(err)
	}
	payloads := []string{testBlock(1, "small")}
	if err := bs.put("1", []byte(payloads[0])); err != nil {
		t.Fatal(err)
	}
	// 内容**不可压缩**（见 incompressible 的注释）：判据是字节占比，压缩会改字节量。
	big := testBlock(1000000, incompressible(1<<20))
	payloads = append(payloads, big)
	for i := 0; i < 3; i++ {
		if err := bs.put("1000000", []byte(big)); err != nil {
			t.Fatal(err)
		}
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	if err := bs.Close(); err != nil {
		t.Fatal(err)
	}
	ct, err := store.CommitRecordTraced(context.Background(), NewSessionRecord(ws, sid, 1, 1, 0, State{}, testView(payloads...)))
	if err != nil {
		t.Fatal(err)
	}
	if ct.CompactScheduled || ct.Compact == 0 {
		t.Fatalf("关闭后台回收后应当同步回收: scheduled=%v compact=%s", ct.CompactScheduled, ct.Compact)
	}
}
