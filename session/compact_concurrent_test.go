package session

// 并发回收（3.3）的回归：重写期间发生的提交一条都不能丢。

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestCompactDoesNotDropConcurrentWrites 直接驱动三段式回收：
// 快照 → 重写 → **期间写入** → 合流，验证尾部记录被平移进新文件且可读。
func TestCompactDoesNotDropConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/x.record.json.blocks"
	bs, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Close()

	// 有效内容：50 个小块 + 一个 1MB 大块。
	for i := 1; i <= 50; i++ {
		if err := bs.put(fmt.Sprintf("%d", i), []byte(testBlock(i, "small"))); err != nil {
			t.Fatal(err)
		}
	}
	big := []byte(testBlock(1000, strings.Repeat("y", 1<<20)))
	// 大块写 3 次：快照里只有 1 份有效，另 2 份是要被回收的垃圾。
	for i := 0; i < 3; i++ {
		if err := bs.put("1000", big); err != nil {
			t.Fatal(err)
		}
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	before := fileSize(t, path)

	snap, err := bs.beginCompact()
	if err != nil {
		t.Fatal(err)
	}
	cw, err := bs.rewriteCompact(snap)
	if err != nil {
		t.Fatal(err)
	}

	// 重写期间的「提交」：新块 + 覆盖写一个大块。
	tailNew := []byte(testBlock(2000, "tail-new"))
	if err := bs.put("2000", tailNew); err != nil {
		t.Fatal(err)
	}
	if err := bs.put("1000", append([]byte(nil), big...)); err != nil { // 同 id 覆盖
		t.Fatal(err)
	}
	if err := bs.put("3", []byte(testBlock(3, "rewritten-during-compact"))); err != nil {
		t.Fatal(err)
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}

	if err := bs.finishCompact(snap, cw); err != nil {
		t.Fatalf("合流失败: %v", err)
	}

	// 1) 尾部新块必须在（且能读出正确内容）。
	got, ok := bs.readAndVerify("2000")
	if !ok || string(got) != string(tailNew) {
		t.Fatalf("合流丢了重写期间写入的新块: ok=%v payload=%q", ok, truncateForLog(got))
	}
	// 2) 同 id 覆盖写必须取最新版本。
	got3, ok := bs.readAndVerify("3")
	if !ok || !strings.Contains(string(got3), "rewritten-during-compact") {
		t.Fatalf("合流后同 id 的内容不是最新版本: %q", truncateForLog(got3))
	}
	// 3) 快照里的块仍然可读。
	if _, ok := bs.readAndVerify("1000"); !ok {
		t.Fatal("大块在合流后不可读")
	}
	if bs.Len() != 52 {
		t.Fatalf("块数不对: %d（应为 50 + 1000 + 2000）", bs.Len())
	}
	// 4) 文件应当被回收：快照里的 2 份废弃大块没了（尾部那 1 份是重写期新追加的，
	//    必须原样保留，所以只要求总体变小）。
	if after := fileSize(t, path); after >= before {
		t.Fatalf("回收后文件没有变小: %d → %d", before, after)
	}
	// 5) 索引可重放：关掉再用新实例打开，内容一致。
	bs2, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bs2.Close()
	if got, ok := bs2.readAndVerify("2000"); !ok || string(got) != string(tailNew) {
		t.Fatal("重新打开后丢块：合流后的索引/文件不一致")
	}
	if bs2.Len() != 52 {
		t.Fatalf("重开后块数不对: %d", bs2.Len())
	}
}

// TestCompactBackgroundUnderLoad 压力版：后台回收与连续提交并行，最后一条
// 记录必须完整可读（不能出现「回收把提交吃掉」）。
func TestCompactBackgroundUnderLoad(t *testing.T) {
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
	ws, sid := "ws-load", "s-load"
	ctx := context.Background()

	// 造出「字节废弃高」的初始形态：大块被反复重写。
	payloads := []string{testBlock(1, "one"), testBlock(2, "two")}
	for i := 0; i < 6; i++ {
		payloads = append(payloads, testBlock(100+i, strings.Repeat("z", 512<<10)))
	}
	blockPayloads := map[int]string{}
	for _, p := range payloads {
		var id int
		if _, err := fmt.Sscanf(p, `{"id":%d`, &id); err != nil {
			t.Fatalf("解析 id: %v", err)
		}
		blockPayloads[id] = p
	}
	view := func(ids ...int) []byte {
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			out = append(out, blockPayloads[id])
		}
		return testView(out...)
	}
	all := []int{1, 2, 100, 101, 102, 103, 104, 105}

	rev := uint64(0)
	for k := 0; k < 12; k++ {
		rev++
		if _, err := store.CommitRecordTraced(ctx, NewSessionRecord(ws, sid, rev, rev, 0, State{}, view(all...))); err != nil {
			t.Fatalf("第 %d 次提交失败: %v", k, err)
		}
	}

	// 最终记录必须可读，且块数齐全。
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !compactPending(mustRecordPath(t, store, ws, sid)) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec, err := store.LoadRecord(ctx, ws, sid)
	if err != nil {
		t.Fatalf("并发回收后最后一条记录不可读: %v", err)
	}
	spans := labSpans(t, rec.ViewCheckpoint)
	if len(spans) != len(all) {
		t.Fatalf("读回块数 %d != %d", len(spans), len(all))
	}
	if rec.Revision != rev {
		t.Fatalf("读回 revision %d != %d", rec.Revision, rev)
	}
}

func mustRecordPath(t *testing.T, store *FileStorage, ws, sid string) string {
	t.Helper()
	p, err := store.RecordPath(ws, sid)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
