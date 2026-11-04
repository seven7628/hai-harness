package agents

import (
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// 压缩输入注入位不得切开工具批次。
//
// 单独成文件的理由：本仓多数 *_test.go 被 .git/info/exclude 排除（见项目记忆
// clean-checkout-test-breakage），写进那些文件的用例不会随提交进库。本文件只依赖
// 生产代码（injectPreCompactContext + core.NormalizeToolBatches），可独立编译。

// TestInjectPreCompactContextDoesNotSplitToolBatch 回归 2026-11-04 真实 400：
// 压缩输入末尾常是「工具批 + 用户插话」，注入位 start-1 正好落在同批回执中间 ——
// 注入串（assistant）夹在回执之间就变成
//
//	assistant(3 个 tool_calls) → tool → tool → assistant([TodoSnapshot]) → tool
//
// 上游判 "insufficient tool messages following tool_calls message" 直接 400；
// 且这一批会随近窗（KeepRecentMessages）原样保留，压完整轮都不可运行。
func TestInjectPreCompactContextDoesNotSplitToolBatch(t *testing.T) {
	msgs := []core.Message{
		core.NewSystemMessage("sys"),
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "看这三个截图"}),
		core.NewAssistantMessage([]core.Content{{Type: core.ContentTypeText, Content: ""}}, "",
			[]core.ToolCall{{Id: "c1", Name: "read_file"}, {Id: "c2", Name: "read_file"}, {Id: "c3", Name: "read_file"}}),
		core.NewToolMessage("c1", "lab-390.png"),
		core.NewToolMessage("c2", "detail-1440.png"),
		core.NewToolMessage("c3", "detail-390.png"),
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "继续"}),
	}
	out := injectPreCompactContext(msgs, "[TodoSnapshot] (authoritative session task list)")
	if !core.ToolBatchesIntact(out) {
		roles := make([]string, len(out))
		for i, m := range out {
			roles[i] = string(m.Role)
			if m.Role == core.Tool {
				roles[i] += "(" + m.ToolCallId + ")"
			}
		}
		t.Fatalf("注入切开了工具批次: %v", roles)
	}
	// 注入串落在整批之前（assistant 之前），批次本身保持完整
	injectAt := -1
	for i, m := range out {
		if m.Role == core.Assistant && len(m.Content) == 1 && strings.Contains(m.Content[0].Content, "[TodoSnapshot]") {
			injectAt = i
		}
	}
	if injectAt < 0 {
		t.Fatal("注入串不见了")
	}
	if out[injectAt+1].Role != core.Assistant || len(out[injectAt+1].ToolCalls) != 3 {
		t.Fatalf("注入串应紧邻工具批之前，实际后一条是 %+v", out[injectAt+1].Role)
	}
}

// TestInjectPreCompactContextKeepsSystemFirst 走批次回退后仍要保住「首条 system 不变」
// （splitMessages 只保留 i==0 的 system；system 后紧跟无主回执的极端形状也要兜住）。
func TestInjectPreCompactContextKeepsSystemFirst(t *testing.T) {
	msgs := []core.Message{
		core.NewSystemMessage("sys"),
		core.NewToolMessage("orphan", "无主回执"),
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "继续"}),
	}
	out := injectPreCompactContext(msgs, "INJECT")
	if out[0].Role != core.System || out[0].Content[0].Content != "sys" {
		t.Fatalf("system 必须在 index 0，实际 %+v", out[0])
	}
	if out[1].Role != core.Assistant || out[1].Content[0].Content != "INJECT" {
		t.Fatalf("注入串应在 system 之后，实际 %+v", out[1])
	}
}
