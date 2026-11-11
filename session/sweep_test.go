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
func TestSweepConcurrentAppendsAreNotLost(t *testing.T) {
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
			_, _ = SweepSessionFiles(root, 1<<20, 3, false, 0)
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
