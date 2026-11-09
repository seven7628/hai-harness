package session

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// TestReadTurnChunksTail 只读尾部若干块（不解 JSON 扫全文件），并给出总数。
func TestReadTurnChunksTail(t *testing.T) {
	recPath := filepath.Join(t.TempDir(), "s.record.json")
	// 3 批：2 + 3 + 1 条
	batches := [][]map[string]any{
		{{"model": "a"}, {"model": "b"}},
		{{"model": "c"}, {"model": "d"}, {"model": "e"}},
		{{"model": "f"}},
	}
	for _, b := range batches {
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		if err := AppendTurnChunk(recPath, raw, len(b)); err != nil {
			t.Fatal(err)
		}
	}
	chunks, total, err := ReadTurnChunksTail(recPath, 2)
	if err != nil {
		t.Fatal(err)
	}
	if total != 6 {
		t.Fatalf("总数应为 6，得 %d", total)
	}
	var got []map[string]any
	for _, c := range chunks {
		var part []map[string]any
		if err := json.Unmarshal(c, &part); err != nil {
			t.Fatal(err)
		}
		got = append(got, part...)
	}
	// 按块边界返回：n=2 会拿到最后一块(1 条) + 前一块(3 条) = 4 条，但必须覆盖末尾
	if len(got) < 2 || got[len(got)-1]["model"] != "f" {
		t.Fatalf("尾部必须覆盖最后一条 f，得 %+v", got)
	}
	if len(got) != 4 {
		t.Fatalf("按块边界应返回 4 条（1+3），得 %d", len(got))
	}

	// n 超过总数：全部返回
	all, total2, err := ReadTurnChunksTail(recPath, 100)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range all {
		var part []map[string]any
		_ = json.Unmarshal(c, &part)
		n += len(part)
	}
	if n != 6 || total2 != 6 {
		t.Fatalf("n 超总数时应全给: n=%d total=%d", n, total2)
	}

	// 日志不存在：空 + 0，不报错
	if chunks, total, err := ReadTurnChunksTail(filepath.Join(t.TempDir(), "none"), 5); err != nil || total != 0 || len(chunks) != 0 {
		t.Fatalf("不存在的日志应返回空: %v %d %d", err, total, len(chunks))
	}
}
