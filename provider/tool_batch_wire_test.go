package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// 请求组装修复的**线上 JSON 断言**（不依赖任何 mock helper）。
//
// 单独成文件的理由：本仓多数 *_test.go 被 .git/info/exclude 排除（见项目记忆
// clean-checkout-test-breakage），写进 provider/openai_test.go 的用例不会随提交进库。

// captureServer 起一个只回一帧 SSE 的假上游，并捕获请求体。
func captureServer(t *testing.T) (*httptest.Server, *map[string]any) {
	t.Helper()
	got := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("unmarshal body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := map[string]any{
			"id": "c1", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "ok"}, "finish_reason": "stop",
			}},
		}
		b, _ := json.Marshal(chunk)
		if _, err := w.Write([]byte("data: " + string(b) + "\n\n")); err != nil {
			t.Logf("write: %v", err)
		}
	}))
	return srv, &got
}

// TestWireRepairsBrokenToolBatch 回归 2026-11-04 真实 400（request_id e262be2f…）：
// 上下文里 assistant(tool_calls) 与回执被别的消息顶开时，发出的 JSON 必须先修好
// 批次 —— 上游判 "insufficient tool messages following tool_calls message" 会直接
// 杀掉整轮 run（permanent，不重试）。
func TestWireRepairsBrokenToolBatch(t *testing.T) {
	srv, gotBody := captureServer(t)
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL, "test-key")
	msgs := []core.Message{
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "go"}),
		core.NewAssistantMessage([]core.Content{{Type: core.ContentTypeText, Content: ""}}, "",
			[]core.ToolCall{{Id: "c1", Name: "read_file"}, {Id: "c2", Name: "read_file"}}),
		core.NewToolMessage("c1", "r1"),
		core.NewAssistantMessage([]core.Content{{Type: core.ContentTypeText, Content: "[TodoSnapshot] intruder"}}, "", nil),
		core.NewToolMessage("c2", "r2"),
	}
	if err := p.Stream(context.Background(), &StreamRequest{Model: "gpt-5", Messages: msgs}, func(StreamEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}

	raw, ok := (*gotBody)["messages"].([]any)
	if !ok {
		t.Fatalf("messages not translated: %v", (*gotBody)["messages"])
	}
	var seq []string
	for _, m := range raw {
		mm := m.(map[string]any)
		label, _ := mm["role"].(string)
		if id, ok := mm["tool_call_id"].(string); ok {
			label += "(" + id + ")"
		}
		seq = append(seq, label)
	}
	want := []string{"user", "assistant", "tool(c1)", "tool(c2)", "assistant"}
	if len(seq) != len(want) {
		t.Fatalf("wire messages = %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("wire messages = %v, want %v（回执必须紧跟其 tool_calls）", seq, want)
		}
	}
}

// TestWireDropsOversizeImage 单边超 core.MaxImageSidePx 的图片不会上线（否则整轮 400），
// 而是变成一句说明尺寸与上限的文本；同一条消息里的文本与合规图片保持原样。
func TestWireDropsOversizeImage(t *testing.T) {
	srv, gotBody := captureServer(t)
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL, "test-key")
	huge := mkPNGDataURL(t, 390, 8193)
	msgs := []core.Message{
		core.NewUserMessage(
			core.Content{Type: core.ContentTypeText, Content: "看这张全页截图"},
			core.Content{Type: core.ContentTypeImage, Content: huge, MimeType: "image/png"},
		),
	}
	if err := p.Stream(context.Background(), &StreamRequest{Model: "gpt-5", Messages: msgs}, func(StreamEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}

	blob, err := json.Marshal(*gotBody)
	if err != nil {
		t.Fatal(err)
	}
	s := string(blob)
	if !strings.Contains(s, "image omitted") || !strings.Contains(s, "390x8193") {
		t.Fatalf("超限图片应被替换成文本说明: %s", s[:600])
	}
	if strings.Contains(s, "data:image/png;base64") {
		t.Fatal("超限图片的 data URL 不该出现在请求体里")
	}
	if !strings.Contains(s, "看这张全页截图") {
		t.Fatal("同一条消息的文本块不该被动")
	}
}
