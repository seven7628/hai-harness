package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/session"
)

// TestTurnsWindowAndLog 锁住 turns 分块的三条不变量：
//   - body 只带尾部窗口（turnsBodyWindow 条），turns_total 是历史总数；
//   - 没写进 body 的历史轮次按批进 <record>.turns（无损）；
//   - 恢复后总数不丢（UI 的「回合数」不能因为只加载窗口而变少）。
func TestTurnsWindowAndLog(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now()
	const n = turnsBodyWindow + 137
	for i := 0; i < n; i++ {
		r.ApplyPlain(&events.LLMEnd{
			RunId: "root", RequestId: fmt.Sprintf("q-%d", i), Model: "m",
			Content: "x", Usage: &core.Usage{Input: 1, Output: 1},
			FinishReason: core.FinishReasonStop, Timestamp: ts.Add(time.Duration(i) * time.Millisecond),
			EventType: events.LLMEndType,
		})
	}

	spans, err := r.MarshalBodyWithSpans()
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Turns      []Turn `json:"turns"`
		TurnsTotal int    `json:"turns_total"`
	}
	if err := json.Unmarshal(spans.Body, &body); err != nil {
		t.Fatalf("body 解析失败: %v", err)
	}
	if len(body.Turns) != turnsBodyWindow {
		t.Fatalf("body 里的 turns 应是窗口 %d 条，得 %d", turnsBodyWindow, len(body.Turns))
	}
	if body.TurnsTotal != n {
		t.Fatalf("turns_total 应是 %d，得 %d", n, body.TurnsTotal)
	}

	// 历史按批进日志（模拟提交路径）
	root := t.TempDir()
	recPath := filepath.Join(root, "s.record.json")
	pending := r.TakePendingTurns()
	if len(pending) != n {
		t.Fatalf("待落盘轮次应是 %d，得 %d", n, len(pending))
	}
	raw, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendTurnChunk(recPath, raw, len(pending)); err != nil {
		t.Fatal(err)
	}
	if cnt, err := session.TurnLogCount(recPath); err != nil || cnt != n {
		t.Fatalf("日志条数 want %d got %d (err=%v)", n, cnt, err)
	}
	if again := r.TakePendingTurns(); len(again) != 0 {
		t.Fatalf("取走后再取应为空，得 %d", len(again))
	}

	// 恢复：窗口 + 总数
	cp := &Checkpoint{SchemaVersion: checkpointSchemaVersion, Blocks: []Block{}, Turns: body.Turns, TurnsTotal: body.TurnsTotal}
	r2 := NewViewReducer()
	if err := r2.Restore(cp); err != nil {
		t.Fatal(err)
	}
	if got := len(r2.turns); got != turnsBodyWindow {
		t.Fatalf("恢复后内存里应是窗口 %d 条，得 %d", turnsBodyWindow, got)
	}
	if r2.turnsTotal != n {
		t.Fatalf("恢复后总数应保持 %d，得 %d", n, r2.turnsTotal)
	}
	// 恢复后再追加一轮：总数与待落盘队列都要对
	r2.ApplyPlain(&events.LLMEnd{
		RunId: "root", RequestId: "q2", Model: "m", Content: "y",
		Usage: &core.Usage{Input: 1, Output: 1}, FinishReason: core.FinishReasonStop,
		Timestamp: ts.Add(time.Second), EventType: events.LLMEndType,
	})
	if r2.turnsTotal != n+1 {
		t.Fatalf("追先后总数应是 %d，得 %d", n+1, r2.turnsTotal)
	}
	if got := len(r2.TakePendingTurns()); got != 1 {
		t.Fatalf("恢复后只应待落盘新追加的 1 条，得 %d", got)
	}
}

// TestTurnsLogAppendIsResumable 日志只追加：多次 append 后条数累加，
// 且读取侧能还原出每批（未来分页依赖）。
func TestTurnsLogAppendIsResumable(t *testing.T) {
	recPath := filepath.Join(t.TempDir(), "s.record.json")
	batch1 := []Turn{{Model: "a", Input: 1}, {Model: "b", Input: 2}}
	batch2 := []Turn{{Model: "c", Input: 3}}
	for _, b := range [][]Turn{batch1, batch2} {
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.AppendTurnChunk(recPath, raw, len(b)); err != nil {
			t.Fatal(err)
		}
	}
	if cnt, err := session.TurnLogCount(recPath); err != nil || cnt != 3 {
		t.Fatalf("want 3 got %d (err=%v)", cnt, err)
	}
	chunks, err := session.ReadTurnChunks(recPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks got %d", len(chunks))
	}
	var got []Turn
	for _, c := range chunks {
		var part []Turn
		if err := json.Unmarshal(c, &part); err != nil {
			t.Fatal(err)
		}
		got = append(got, part...)
	}
	if len(got) != 3 || got[2].Model != "c" {
		t.Fatalf("还原结果不对: %+v", got)
	}
}
