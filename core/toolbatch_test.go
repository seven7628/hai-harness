package core

import "testing"

func tc(id, name string) ToolCall { return ToolCall{Id: id, Name: name} }

func roles(msgs []Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = string(m.Role)
		if m.Role == Tool {
			out[i] += "(" + m.ToolCallId + ")"
		}
	}
	return out
}

// TestNormalizeToolBatches_RealIncident 复刻 2026-11-04 的真实破损形状：
// assistant(3 个 tool_calls) → tool → tool → assistant([TodoSnapshot]) → tool。
// 上游对这种形状直接 400（insufficient tool messages following tool_calls message）。
func TestNormalizeToolBatches_RealIncident(t *testing.T) {
	in := []Message{
		NewAssistantMessage([]Content{{Type: ContentTypeText, Content: ""}}, "", nil), // 前一轮空 assistant（无 tool_calls，原样保留）
		NewAssistantMessageWithSignature([]Content{{Type: ContentTypeText, Content: ""}}, "", "",
			[]ToolCall{tc("c1", "read_file"), tc("c2", "read_file"), tc("c3", "read_file")}),
		NewToolMessage("c1", "lab-390.png"),
		NewToolMessage("c2", "detail-1440.png"),
		NewAssistantMessage([]Content{{Type: ContentTypeText, Content: "[TodoSnapshot] (authoritative session task list)"}}, "", nil),
		NewToolMessage("c3", "detail-390.png"),
		NewUserMessage(Content{Type: ContentTypeText, Content: "继续"}),
	}
	if ToolBatchesIntact(in) {
		t.Fatal("破损形状不该被判为合法")
	}
	out := NormalizeToolBatches(in)
	if !ToolBatchesIntact(out) {
		t.Fatalf("修复后仍不合法: %v", roles(out))
	}
	// 三条回执必须紧邻 assistant，且顺序保持 c1,c2,c3
	want := []string{"assistant", "assistant", "tool(c1)", "tool(c2)", "tool(c3)", "assistant", "user"}
	got := roles(out)
	if len(got) != len(want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("roles = %v, want %v", got, want)
		}
	}
	// 失败现场的消息不能丢（TodoSnapshot 那条 assistant 仍在，只是挪到批次之后）
	found := false
	for _, m := range out {
		if m.Role == Assistant && len(m.Content) == 1 && m.Content[0].Content == "[TodoSnapshot] (authoritative session task list)" {
			found = true
		}
	}
	if !found {
		t.Fatal("TodoSnapshot assistant 消息被丢了")
	}
}

// TestNormalizeToolBatches_LegalIsNoop 合法上下文必须原样返回（不分配、不动顺序——
// 缓存前缀稳定，避免每轮请求重排导致 prompt cache 全失效）。
func TestNormalizeToolBatches_LegalIsNoop(t *testing.T) {
	in := []Message{
		NewSystemMessage("sys"),
		NewUserMessage(Content{Type: ContentTypeText, Content: "hi"}),
		NewAssistantMessageWithSignature([]Content{{Type: ContentTypeText, Content: ""}}, "", "",
			[]ToolCall{tc("c1", "a"), tc("c2", "b")}),
		NewToolMessage("c1", "r1"),
		NewToolMessage("c2", "r2"),
		NewAssistantMessage([]Content{{Type: ContentTypeText, Content: "done"}}, "", nil),
	}
	if !ToolBatchesIntact(in) {
		t.Fatal("合法形状误判为破损")
	}
	out := NormalizeToolBatches(in)
	if &out[0] != &in[0] {
		t.Fatal("合法上下文不该重新分配")
	}
}

// TestNormalizeToolBatches_MissingAndOrphan 缺回执补占位；孤儿/重复回执丢弃。
func TestNormalizeToolBatches_MissingAndOrphan(t *testing.T) {
	in := []Message{
		NewToolMessage("ghost", "没有 assistant 认领的孤儿回执"),
		NewAssistantMessageWithSignature([]Content{{Type: ContentTypeText, Content: ""}}, "", "",
			[]ToolCall{tc("c1", "a"), tc("c2", "b")}),
		NewToolMessage("c1", "r1"),
		NewToolMessage("c1", "重复回执"),
		NewUserMessage(Content{Type: ContentTypeText, Content: "next"}),
	}
	out := NormalizeToolBatches(in)
	if !ToolBatchesIntact(out) {
		t.Fatalf("修复后仍不合法: %v", roles(out))
	}
	want := []string{"assistant", "tool(c1)", "tool(c2)", "user"}
	got := roles(out)
	if len(got) != len(want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("roles = %v, want %v", got, want)
		}
	}
	// 缺失的那条是占位文本（不是空串，也不是重复回执的内容）
	if out[2].Content[0].Content != missingToolResultText {
		t.Fatalf("missing result placeholder = %q", out[2].Content[0].Content)
	}
}

// TestNormalizeToolBatches_Idempotent 二次修复必须无变化（幂等）。
func TestNormalizeToolBatches_Idempotent(t *testing.T) {
	in := []Message{
		NewAssistantMessageWithSignature([]Content{{Type: ContentTypeText, Content: ""}}, "", "",
			[]ToolCall{tc("c1", "a"), tc("c2", "b")}),
		NewToolMessage("c2", "out of order"),
		NewAssistantMessage([]Content{{Type: ContentTypeText, Content: "intruder"}}, "", nil),
		NewToolMessage("c1", "r1"),
	}
	once := NormalizeToolBatches(in)
	twice := NormalizeToolBatches(once)
	// 归位后按 assistant 声明的 tool_calls 顺序排列（c1,c2），而不是它们在历史里
	// 被打乱的顺序 —— 与协议要求的「按 id 逐一应答」一致。
	if roles(once)[1] != "tool(c1)" || roles(once)[2] != "tool(c2)" {
		t.Fatalf("回执应按 tool_calls 顺序归位: %v", roles(once))
	}
	if len(once) != len(twice) {
		t.Fatalf("幂等失败: %v vs %v", roles(once), roles(twice))
	}
	for i := range once {
		if roles(once)[i] != roles(twice)[i] {
			t.Fatalf("幂等失败: %v vs %v", roles(once), roles(twice))
		}
	}
}
