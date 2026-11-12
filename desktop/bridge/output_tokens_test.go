package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
)

// TestReducerOutputTokensPersist 输出速度（tokens/s）的分子必须进 checkpoint。
//
// 背景（2026-09-10 报障）：对话记录底部的「N tokens/s」刷新后就没了。该数字的分子
// 是 usage.Output（含 reasoning），此前只存在 renderer 内存里、没落 view_checkpoint，
// 于是刷新（冷恢复走 checkpoint hydrate）后无法复原。
//
// 覆盖：
//
//	T1 主 agent 轮：assistant 块的 output_tokens/reasoning_tokens 落到 checkpoint，且
//	   经 json 往返（磁盘 record → 冷恢复）仍在
//	T2 口径：Output 含 Reasoning 时两者都要存（分母由渲染层按 Thinking 是否流式决定，
//	   故只落分子；这里同时校验 reasoning_tokens 也在，供分母判据使用）
//	T3 非 reasoning 模型：reasoning_tokens 不出现（omitempty），output_tokens 照存
func TestReducerOutputTokensPersist(t *testing.T) {
	ts := time.Now()

	t.Run("T1 主 agent 轮的 output/reasoning 落 checkpoint 且可 json 往返", func(t *testing.T) {
		r := NewViewReducer()
		// llm_start → content_chunk（结算 TTFT）→ llm_end（2236 out 含 2028 reasoning，
		// 与报障那一轮的真实数字一致）
		_ = r.ApplyPlain(evLLMStart(t, "r1", "q1", "deepseek/deepseek-v4-flash", ts))
		_ = r.ApplyPlain(evContentChunk(t, "r1", "q1", "OK", 0, ts.Add(11600*time.Millisecond)))
		_ = r.ApplyPlain(evLLMEnd(t, "r1", "q1", "deepseek/deepseek-v4-flash", "OK",
			&core.Usage{Input: 86845, Output: 2236, Reasoning: 2028}, ts.Add(11900*time.Millisecond)))

		cp := r.Snapshot()
		var got int64
		var reason int64
		for _, b := range cp.Blocks {
			if b.Kind == blockKindAssistant && b.Text != "" {
				got = b.OutputTokens
				reason = b.ReasoningTokens
			}
		}
		if got != 2236 {
			t.Fatalf("checkpoint 的 output_tokens 应为 2236，实际 %d", got)
		}
		if reason != 2028 {
			t.Fatalf("checkpoint 的 reasoning_tokens 应为 2028，实际 %d", reason)
		}

		// json 往返：磁盘 record → 冷恢复（hydrate）路径必须保住字段
		raw, err := json.Marshal(cp)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(raw), `"output_tokens":2236`) {
			t.Fatalf("序列化后缺 output_tokens: %s", string(raw))
		}
		var back Checkpoint
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		var backOut int64
		for _, b := range back.Blocks {
			if b.Kind == blockKindAssistant && b.Text != "" {
				backOut = b.OutputTokens
			}
		}
		if backOut != 2236 {
			t.Fatalf("json 往返后 output_tokens 丢失，实际 %d", backOut)
		}
	})

	t.Run("T3 非 reasoning 模型：reasoning_tokens 省略", func(t *testing.T) {
		r := NewViewReducer()
		_ = r.ApplyPlain(evLLMStart(t, "r1", "q1", "m", ts))
		_ = r.ApplyPlain(evLLMEnd(t, "r1", "q1", "m", "hi",
			&core.Usage{Input: 10, Output: 300}, ts.Add(2*time.Second)))

		cp := r.Snapshot()
		raw, _ := json.Marshal(cp)
		if !strings.Contains(string(raw), `"output_tokens":300`) {
			t.Fatalf("非 reasoning 也应存 output_tokens: %s", string(raw))
		}
		if strings.Contains(string(raw), `"reasoning_tokens"`) {
			t.Fatalf("reasoning=0 时应 omitempty: %s", string(raw))
		}
	})
}

// TestColdRestoreKeepsOutputTokens 冷恢复（刷新）后 output_tokens 仍在。
//
// 报障二：数字在实时流里正常，一刷新就没了。刷新 = 新 manager 读磁盘 record 的
// view_checkpoint 回给 renderer（restored=checkpoint）。若该字段没落盘/没进
// checkpoint，渲染层 `out == null` 直接不渲染 → 整行消失。
//
// 与 TestReducerOutputTokensPersist 的区别：那条验 reducer→Snapshot；这条走完整
// 落盘 → 新 manager 读盘 → new_session 响应，覆盖「刷新」的真实链路。
func TestColdRestoreKeepsOutputTokens(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("HOME", home)
	ws := t.TempDir()

	// —— 实例 1：跑一轮，产出带 output_tokens 的 checkpoint 并落盘 ——
	m1, _ := newManagerWithAppData(t, dataDir)
	bs1, err := m1.create(ws, "s1", "")
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(-time.Minute)
	bs1.emit(evLLMStart(t, "r1", "q1", "deepseek/deepseek-v4-flash", ts))
	bs1.emit(evContentChunk(t, "r1", "q1", "OK", 0, ts.Add(11600*time.Millisecond)))
	bs1.emit(evLLMEnd(t, "r1", "q1", "deepseek/deepseek-v4-flash", "OK",
		&core.Usage{Input: 86845, Output: 2236, Reasoning: 2028}, ts.Add(11900*time.Millisecond)))
	waitRecord(t, workspaceKey(ws), "s1", 5*time.Second)

	// —— 实例 2：模拟刷新（新 manager 读同一数据目录）——
	m2, buf2 := newManagerWithAppData(t, dataDir)
	m2.dispatch(cmd(0, "new_session", map[string]any{"workspace": ws, "id": "s1", "activate": true}))
	var resp map[string]any
	for _, l := range readLines(buf2) {
		if l["event_type"] == "command_response" && l["id"] == float64(0) {
			resp = l
			break
		}
	}
	if resp == nil {
		t.Fatal("new_session 无响应（冷恢复失败）")
	}
	data, _ := resp["data"].(map[string]any)
	if data["restored"] != "checkpoint" {
		t.Fatalf("restored = %v, want checkpoint（刷新应走 checkpoint 冷恢复）", data["restored"])
	}
	raw, _ := data["view_checkpoint"].(json.RawMessage)
	if len(raw) == 0 {
		raw, _ = json.Marshal(data["view_checkpoint"])
	}
	var cp Checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		t.Fatalf("view_checkpoint 解析失败: %v", err)
	}
	var found bool
	for _, b := range cp.Blocks {
		if b.Kind != blockKindAssistant {
			continue
		}
		if b.OutputTokens == 2236 && b.ReasoningTokens == 2028 {
			found = true
		}
	}
	if !found {
		t.Fatalf("刷新后 checkpoint 丢了 output_tokens/reasoning_tokens（Tokens/s 会消失）: %s", string(raw))
	}
}

// TestRestoreBackfillsOutputTokensFromTurns 旧 checkpoint（块上无 output_tokens）冷恢复
// 时，用 turns 回填 —— 历史轮次的 Tokens/s 不再消失。
//
// 用户 2026-09-10 报障：「重启后最近一条 Agent End 就没有 tokens/s」。
// 原因是 output_tokens 字段晚于 turns 才加进 Block：字段上线前落盘的 checkpoint 里
// 块上没有它，而 turns 里一直有 output/reasoning（旧版本也写），故可无损回填。
func TestRestoreBackfillsOutputTokensFromTurns(t *testing.T) {
	r := NewViewReducer()
	// 模拟「旧版本落盘」的 checkpoint：块有 dur_ms/ttft_ms 但**没有** output_tokens；
	// turns 里带着该轮的 output/reasoning（request_id 与块一致）。
	cp := &Checkpoint{
		SchemaVersion: checkpointSchemaVersion,
		Blocks: []Block{
			{Kind: "user", ID: 1, Text: "hi"},
			{
				Kind: blockKindAssistant, ID: 2, Text: "回复", Model: "deepseek/deepseek-v4-flash",
				RequestID: "gen_legacy_1", DurMs: 7371, TtftMs: 3987,
				// OutputTokens 故意为 0（旧数据）
			},
			{
				Kind: blockKindAssistant, ID: 4, Text: "带思考的一轮", Model: "m",
				RequestID: "gen_legacy_2", DurMs: 11900, TtftMs: 11600,
			},
		},
		Turns: []Turn{
			{Model: "deepseek/deepseek-v4-flash", Input: 100, Output: 788, Reasoning: 0, RequestID: "gen_legacy_1", DurMs: 7371, TtftMs: 3987},
			{Model: "m", Input: 100, Output: 2236, Reasoning: 2028, RequestID: "gen_legacy_2", DurMs: 11900, TtftMs: 11600},
		},
	}
	if err := r.Restore(cp); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	cp2 := r.Snapshot()
	got := map[string][2]int64{}
	for _, b := range cp2.Blocks {
		if b.Kind == blockKindAssistant {
			got[b.RequestID] = [2]int64{b.OutputTokens, b.ReasoningTokens}
		}
	}
	if got["gen_legacy_1"] != [2]int64{788, 0} {
		t.Fatalf("块1 回填错误: %v（用户报障那条应得 788 out / 0 reasoning）", got["gen_legacy_1"])
	}
	if got["gen_legacy_2"] != [2]int64{2236, 2028} {
		t.Fatalf("块2 回填错误: %v", got["gen_legacy_2"])
	}

	// 幂等：再次 Restore 不得改变已有值（也不得把 0 覆盖成别的）
	if err := r.Restore(cp2); err != nil {
		t.Fatalf("二次 Restore: %v", err)
	}
	for _, b := range r.Snapshot().Blocks {
		if b.Kind != blockKindAssistant {
			continue
		}
		if b.RequestID == "gen_legacy_1" && b.OutputTokens != 788 {
			t.Fatalf("二次 Restore 非幂等: %d", b.OutputTokens)
		}
	}
}

// TestRestoreBackfillSkipsUnmatched 无对应 turn（request_id 对不上 / 空）时不得瞎填。
func TestRestoreBackfillSkipsUnmatched(t *testing.T) {
	r := NewViewReducer()
	cp := &Checkpoint{
		SchemaVersion: checkpointSchemaVersion,
		Blocks: []Block{
			{Kind: blockKindAssistant, ID: 2, Text: "无匹配", RequestID: "gen_missing", DurMs: 1000},
			{Kind: blockKindAssistant, ID: 3, Text: "空 request_id", DurMs: 1000},
		},
		Turns: []Turn{
			{Model: "m", Output: 999, RequestID: "gen_other"},
		},
	}
	if err := r.Restore(cp); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for _, b := range r.Snapshot().Blocks {
		if b.Kind == blockKindAssistant && b.OutputTokens != 0 {
			t.Fatalf("不应为对不上的块填值: id=%d out=%d", b.ID, b.OutputTokens)
		}
	}
}
