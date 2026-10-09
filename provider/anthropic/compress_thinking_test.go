package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

// captureCompressReqBody 起 httptest 非官方端点（自动 compat 模式），捕获一次 Stream
// 请求体。自包含（不依赖同包其它测试文件的辅助函数）：本文件只做压缩「关思考」决策的
// 线上形状验证，helper 放这里让它在任何基线上都能独立编译通过。
func captureCompressReqBody(t *testing.T, req *provider.StreamRequest) map[string]any {
	t.Helper()
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"role\":\"assistant\",\"model\":\"x\",\"content\":[],\"usage\":{\"input_tokens\":1}}}\n\n"))
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	p := NewAnthropicProvider(provider.Dependencies{Registry: provider.NewRegistry(), HTTPClient: srv.Client()}, srv.URL, "k")
	if !p.compat {
		t.Fatalf("httptest URL（非官方）应判 compat 模式: %s", srv.URL)
	}
	_ = p.Stream(context.Background(), req, func(provider.StreamEvent) error { return nil })
	if body == nil {
		t.Fatal("未捕获到请求体")
	}
	return body
}

// TestCompressThinkingOffRequestShape 压缩请求「关思考」决策在三种模型上的**线上形状**：
// 这条用例把 agents 侧的策略判定与 provider 侧的发送行为接起来跑，防止「判定说可关、
// 发出去却被网关 400」这类只在真实端点暴露的口径漂移。
//
//   - minimax-m3（推理模型、非 force-on）：请求体带 thinking.type=disabled；
//   - glm-5.3（ThinkingForceOn）：即便调用方显式要求关，请求体也不出现 disabled
//     （省略 = 端点默认开）。
//
// 非推理模型不在此列：agents 侧已判定「不发开关」，provider 侧收不到也就无从表达。
func TestCompressThinkingOffRequestShape(t *testing.T) {
	userMsg := []core.Message{core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "hi"})}
	off := false

	// ① 推理模型：显式关思考 → thinking.type=disabled
	body := captureCompressReqBody(t, &provider.StreamRequest{
		Provider: "minimax", Model: "minimax-m3",
		Config:   &provider.RequestConfig{Thinking: &off},
		Messages: userMsg,
	})
	if th, ok := body["thinking"].(map[string]any); !ok || th["type"] != "disabled" {
		t.Errorf("推理模型关思考应发 thinking.type=disabled: %+v", body["thinking"])
	}

	// ② 强制思考模型：调用方显式要求关也忽略（省略 = 默认开；发 disabled 会 400）
	body2 := captureCompressReqBody(t, &provider.StreamRequest{
		Provider: "zhipu", Model: "glm-5.3",
		Config:   &provider.RequestConfig{Thinking: &off},
		Messages: userMsg,
	})
	if th, ok := body2["thinking"].(map[string]any); ok && th["type"] == "disabled" {
		t.Errorf("ThinkingForceOn 模型不得发 disabled（兼容端点会 400）: %+v", th)
	}

	// ③ 不发开关（agents 对 force-on / 非推理模型的选择）→ 请求体无 thinking 字段
	body3 := captureCompressReqBody(t, &provider.StreamRequest{
		Provider: "zhipu", Model: "glm-5.3",
		Messages: userMsg,
	})
	if _, has := body3["thinking"]; has {
		t.Errorf("不发开关时请求体不应出现 thinking 字段: %+v", body3["thinking"])
	}
}

// TestCompressThinkingOffMaxTokensStaysWithinCap 压缩请求关思考后 max_tokens **仍不超
// 模型内置上限**（全 anthropic 协议推理模型扫描）。
//
// 为什么值得扫全表（2026-10）：关思考时不走 budget-based 分支，因此
// AdjustMaxTokensForThinking / ClampMaxTokensToContext 都不执行 —— 只靠请求体构造
// 开头那次「clamp 到 RegistryLookupBuiltin 的内置上限」。这条断言把那个前提钉住：
// 谁要是把开头那次 clamp 挪走/改条件，全表立刻红，而不是等到某个模型 400。
//
// thinking 形状一并断言：非 force-on 的推理模型必须显式发 disabled（发不出 disabled
// 的端点压根不该被判成"可关"）。
func TestCompressThinkingOffMaxTokensStaysWithinCap(t *testing.T) {
	off := false
	msgs := []core.Message{core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "hi"})}
	var scanned int
	for _, info := range provider.BuiltinModels() {
		if info.Protocol != "anthropic" || !info.Reasoning || info.ThinkingDefaultOff || info.ThinkingForceOn {
			continue // 只看「该发 disabled」的那一类（CanDisableThinking 判 true 的范围）
		}
		scanned++
		// 刻意传一个远超各家上限的值：检验 clamp 真的在起作用，而不是碰巧没超。
		const oversized = 1 << 40
		body := captureCompressReqBody(t, &provider.StreamRequest{
			Provider: info.Provider, Model: info.ID,
			Config: &provider.RequestConfig{
				Thinking:  &off,
				Effort:    provider.ReasoningEffortLevelHigh,
				MaxTokens: oversized,
			},
			Messages: msgs,
		})
		mt, _ := body["max_tokens"].(float64)
		if info.MaxTokens > 0 && int64(mt) > info.MaxTokens {
			t.Errorf("%s/%s: 关思考后 max_tokens=%v 超模型上限 %d（clamp 失效）",
				info.Provider, info.ID, mt, info.MaxTokens)
		}
		if th, ok := body["thinking"].(map[string]any); !ok || th["type"] != "disabled" {
			t.Errorf("%s/%s: 关思考应发 thinking.type=disabled: %+v", info.Provider, info.ID, body["thinking"])
		}
	}
	if scanned == 0 {
		t.Fatal("一个模型都没扫到 —— providers.json 的协议/推理标注变了？")
	}
	t.Logf("scanned %d anthropic 协议推理模型", scanned)
}
