package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadRecordForViewMapsBlocksWithoutAssembly 锁住映射路径的三条不变量：
//   - ViewCheckpoint 不展开（不入 209MB 拼装）；
//   - spans 指向 mmap（不复制），各自带期望摘要；
//   - ReleaseView 幂等，释放后再读 spans 不再是我们的责任（调用方约定）。
func TestLoadRecordForViewMapsBlocksWithoutAssembly(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileStorage(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	wsKey := "ws"
	sid := "s1"

	blocks := make([]map[string]any, 0, 32)
	for i := 1; i <= 32; i++ {
		blocks = append(blocks, map[string]any{"kind": "assistant", "id": i, "text": "hello"})
	}
	// schema_version 必须排在最前：校验只看前 256 字节（真实编码器按结构体
	// 字段顺序输出，map 的随机顺序会被这条快检拒掉）。
	blocksJSON, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(fmt.Sprintf(`{"schema_version":1,"blocks":%s,"run_nodes":[],"turns":[],"usage":{},"todos":[]}`, blocksJSON))
	rec := NewSessionRecord(wsKey, sid, 1, 1, 0, State{}, raw)
	if err := store.CommitRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadRecordForView(context.Background(), wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.ReleaseView()

	if len(loaded.ViewCheckpoint) != 0 {
		t.Fatalf("映射路径不应展开 view，得 %d 字节", len(loaded.ViewCheckpoint))
	}
	if len(loaded.BodyView) == 0 {
		t.Fatal("映射路径应保留 BodyView（引用形态）")
	}
	if len(loaded.LoadedBlockSpans) != len(blocks) {
		t.Fatalf("spans=%d want %d", len(loaded.LoadedBlockSpans), len(blocks))
	}
	if len(loaded.LoadedBlockHashes) != len(blocks) {
		t.Fatalf("hashes=%d want %d", len(loaded.LoadedBlockHashes), len(blocks))
	}
	for i, span := range loaded.LoadedBlockSpans {
		// 惰性解压：压缩块的 Raw 是压缩字节，先取逻辑内容。
		logical, lerr := span.Logical()
		if lerr != nil {
			t.Fatal(lerr)
		}
		if d := PayloadDigest(logical); d != loaded.LoadedBlockHashes[i] {
			t.Fatalf("块 %d 摘要与索引不符", i)
		}
		var b struct {
			ID uint64 `json:"id"`
		}
		if err := json.Unmarshal(logical, &b); err != nil {
			t.Fatalf("块 %d 解码失败: %v", i, err)
		}
		if b.ID != uint64(i+1) {
			t.Fatalf("块 %d 顺序错位：id=%d", i, b.ID)
		}
	}
	loaded.ReleaseView()
	loaded.ReleaseView() // 幂等
}

// TestLoadRecordForViewFallsBackWithoutIndex 索引缺失时映射不可用，必须回退到
// 拼装路径（功能不变），而不是报错或给出空 blocks。
func TestLoadRecordForViewFallsBackWithoutIndex(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileStorage(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	wsKey, sid := "ws", "s1"
	raw2, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"blocks":         []any{map[string]any{"kind": "assistant", "id": 1, "text": "x"}},
		"run_nodes":      []any{}, "turns": []any{}, "usage": map[string]any{}, "todos": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitRecord(context.Background(), NewSessionRecord(wsKey, sid, 1, 1, 0, State{}, raw2)); err != nil {
		t.Fatal(err)
	}
	// 伪造「索引丢失」：删掉 .idx，映射路径必须自动回退。
	path, err := store.RecordPath(wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blockIndexPath(path + ".blocks")); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadRecordForView(context.Background(), wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.ReleaseView()
	if len(loaded.LoadedBlockSpans) != 1 {
		t.Fatalf("回退路径应带回 1 个块切片，得 %d", len(loaded.LoadedBlockSpans))
	}
	if len(loaded.ViewCheckpoint) == 0 {
		t.Fatal("回退路径应展开完整 view")
	}
}
