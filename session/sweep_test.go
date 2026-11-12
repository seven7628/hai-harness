package session

// 维护清扫（sweep.go）的回归：
//   1. 只碰超阈值的文件；
//   2. 压缩后恢复内容逐字节一致；
//   3. **并发追加不丢**（清扫会碰正在使用的会话文件）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
)

// TestSweepSessionFilesCompactsOversized 清扫只处理超阈值的文件。
func TestSweepSessionFilesCompactsOversized(t *testing.T) {
	root := t.TempDir()
	// 一个超阈值的会话（多轮 state 行 → O(N²)）。
	bigDir := filepath.Join(root, "ws1")
	if err := os.MkdirAll(bigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	big, err := NewFileStore(bigDir)
	if err != nil {
		t.Fatal(err)
	}
	bigID := "s-big"
	bigMsg := []core.Message{{Role: core.User, Content: []core.Content{{Type: core.ContentTypeText, Content: strings.Repeat("x", 200*1024)}}}}
	for i := 0; i < 12; i++ {
		if err := big.SaveState(context.Background(), bigID, &State{Messages: bigMsg}); err != nil {
			t.Fatal(err)
		}
	}
	// 一个很小的会话（不该被碰）。
	smallDir := filepath.Join(root, "ws2")
	if err := os.MkdirAll(smallDir, 0o700); err != nil {
		t.Fatal(err)
	}
	small, err := NewFileStore(smallDir)
	if err != nil {
		t.Fatal(err)
	}
	smallID := "s-small"
	if err := small.SaveState(context.Background(), smallID, &State{Messages: []core.Message{{Role: core.User, Content: []core.Content{{Type: core.ContentTypeText, Content: "hi"}}}}}); err != nil {
		t.Fatal(err)
	}
	smallBefore := fileSize(t, filepath.Join(smallDir, smallID+".jsonl"))

	bigBefore := fileSize(t, filepath.Join(bigDir, bigID+".jsonl"))
	res, err := SweepSessionFiles(root, 1<<20, 3, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Compacted == 0 {
		t.Fatalf("应当有文件被压缩（结果 %+v）", res)
	}
	bigAfter := fileSize(t, filepath.Join(bigDir, bigID+".jsonl"))
	if bigAfter >= bigBefore {
		t.Fatalf("超阈文件没变小：%d → %d", bigBefore, bigAfter)
	}
	if got := fileSize(t, filepath.Join(smallDir, smallID+".jsonl")); got != smallBefore {
		t.Fatalf("小文件不该被碰：%d → %d", smallBefore, got)
	}
	// 恢复内容不变
	data, err := big.Load(context.Background(), bigID)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.State.Messages) != len(bigMsg) {
		t.Fatalf("恢复消息数变了：%d", len(data.State.Messages))
	}
	t.Logf("清扫 %d 个文件（压缩 %d 个），%d → %d 字节", res.Files, res.Compacted, res.Before, res.After)
}

// TestSweepConcurrentAppendsAreNotLost 清扫与追加并发时，追加的行一条都不能丢。
//
// 这是清扫敢在后台跑的前提：压缩是「读 → 写临时文件 → rename」，期间到达的
// 追加若写进旧 inode 就会丢（appendLine 与 compactFile 用同一把进程级按路径锁）。
//
// **为什么 keepHistory=true**（2026-11-11，P5'）：清扫默认会**裁掉 history 行**
// （除首行外）—— 那是**有意的裁剪**，不是竞态丢失。本测试断言的是「追加一条
// 都不能少」，所以必须把裁剪关掉，让断言重新变成精确的。
// 裁剪行为另有测试（TestSweepTrimsHistoryExceptFirstLine）。
func TestSweepConcurrentAppendsAreNotLost(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "ws")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fs, err := NewFileStore(dir, WithKeepHistory(true))
	if err != nil {
		t.Fatal(err)
	}
	id := "s1"
	p, err := fs.path(id)
	if err != nil {
		t.Fatal(err)
	}
	// 先攒够体积（让清扫会碰它）
	big := []core.Message{{Role: core.User, Content: []core.Content{{Type: core.ContentTypeText, Content: strings.Repeat("y", 200*1024)}}}}
	for i := 0; i < 8; i++ {
		if err := fs.SaveState(context.Background(), id, &State{Messages: big}); err != nil {
			t.Fatal(err)
		}
	}

	const writers = 4
	const perWriter = 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				msg := core.Message{Role: core.User, Content: []core.Content{{Type: core.ContentTypeText, Content: fmt.Sprintf("w%d-%d", w, i)}}}
				if err := fs.AppendHistory(context.Background(), id, []core.Message{msg}); err != nil {
					t.Errorf("AppendHistory: %v", err)
					return
				}
			}
		}(w)
	}
	// 并发清扫（同一目录，反复几轮）
	sweeperDone := make(chan struct{})
	go func() {
		defer close(sweeperDone)
		for i := 0; i < 5; i++ {
			_, _ = SweepSessionFiles(root, 1<<20, 3, true, 0)
		}
	}()
	wg.Wait()
	<-sweeperDone

	// 所有追加的 history 行都必须在（一条都不能丢）
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	missing := 0
	for w := 0; w < writers; w++ {
		for i := 0; i < perWriter; i++ {
			tag := fmt.Sprintf(`"w%d-%d"`, w, i)
			if !strings.Contains(string(data), tag) {
				missing++
			}
		}
	}
	if missing > 0 {
		t.Fatalf("丢了 %d/%d 条追加（并发追加与压缩的竞态）", missing, writers*perWriter)
	}
	// 恢复仍可用
	st, err := fs.Load(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(st.State)
	if len(b) == 0 {
		t.Fatal("恢复为空")
	}
}

// TestSweepTrimsHistoryExceptFirstLine 清扫把 history 行裁到只剩首行（P5'）。
//
// 依据：history 行的消息就在**最后一条 state 行**里（state 行存的是有效
// 上下文 = 同一批消息）—— 边界之后的那些同样冗余。实测清扫后最大的 40 个
// 会话文件里 history 行占 **30%**。
//
// 首行必须留 —— 会话标题的冷磁盘回退读它（firstUserTextFromFile）。
func TestSweepTrimsHistoryExceptFirstLine(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "ws")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := "s1"
	p, err := fs.path(id)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// 首行（标题来源）。
	if err := fs.AppendHistory(ctx, id, []core.Message{{
		Role: core.User, Content: []core.Content{{Type: core.ContentTypeText, Content: "title"}},
	}}); err != nil {
		t.Fatal(err)
	}
	// 攒体积（让清扫会碰它）：8 条大 state 行。
	big := []core.Message{{Role: core.User, Content: []core.Content{{Type: core.ContentTypeText, Content: strings.Repeat("y", 200*1024)}}}}
	for i := 0; i < 8; i++ {
		if err := fs.SaveState(ctx, id, &State{Messages: big}); err != nil {
			t.Fatal(err)
		}
	}
	// 压缩点之后的 history 行（也该被裁）。
	if err := fs.AppendCompaction(ctx, id, &CompactionRecord{Summary: "s", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := fs.AppendHistory(ctx, id, []core.Message{{
			Role: core.User, Content: []core.Content{{Type: core.ContentTypeText, Content: "after"}},
		}}); err != nil {
			t.Fatal(err)
		}
	}

	res, err := SweepSessionFiles(root, 1<<20, 2, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Compacted != 1 {
		t.Fatalf("应压缩 1 个文件，实际 %d", res.Compacted)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, "title") {
		t.Fatal("首行（标题来源）必须保留")
	}
	if strings.Contains(body, "after") {
		t.Fatal("压缩点之后的 history 行应被裁掉")
	}
	// 恢复仍可用（尾扫最后一条 state 行）。
	st, err := fs.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if st.State == nil || len(st.State.Messages) != len(big) {
		t.Fatalf("恢复失败: %+v", st.State)
	}
}
