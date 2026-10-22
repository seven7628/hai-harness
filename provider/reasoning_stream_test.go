package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// 思考文本字段名兼容（2026-10-20 opendesign 真机实证）回归锚点。
//
// 现场：opendesign 网关（deepseek-v4.1-flash / deepseek-v4-pro / kimi-k2.7-code）把流式
// 思考放在 choices[].delta.**reasoning**，SDK 结构体只认 **reasoning_content** →
// unmarshal 阶段丢弃，表现为「reasoning delta 事件 0 个、LLMEnd.Reasoning 恒空」，而
// usage.reasoning_tokens 照常有值（计费走另一个字段，所以不报错，只是文本没了）。
//
// 修复在传输层（reasoning_stream.go）：两个发送通道（SDK / minimal）唯一共同交汇处是
// resp.Body。以下测试同时覆盖两条通道 —— 只修 SDK 通道的改动会让 minimal 那组红。

// reasoningSSE 构造一段 SSE：思考分片走 name 指定的字段（模拟上游真实形态）。
func reasoningSSE(name string, parts []string, content string) string {
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(`data: {"id":"c1","choices":[{"index":0,"delta":{"` + name + `":` + jsonString(p) + `}}]}` + "\n\n")
	}
	if content != "" {
		sb.WriteString(`data: {"id":"c1","choices":[{"index":0,"delta":{"content":` + jsonString(content) + `}}]}` + "\n\n")
	}
	sb.WriteString(`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30,` +
		`"completion_tokens_details":{"reasoning_tokens":12}}}` + "\n\n")
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// streamOnce 用给定 SSE 响应跑一次 Stream，返回聚合结果。
func streamOnce(t *testing.T, caps OpenAICompatCapabilities, sse string, model string) (string, []string, core.Usage) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sse)
	}))
	defer srv.Close()

	p := NewOpenAIProviderWithDeps(Dependencies{Registry: NewRegistry()}, srv.URL, "sk-test", caps)
	var (
		end     *LLMEndEvent
		reasons []string
	)
	err := p.Stream(context.Background(), &StreamRequest{
		Provider: "opendesign",
		Model:    model,
		Config:   &RequestConfig{},
		Messages: []core.Message{core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "hi"})},
	}, func(e StreamEvent) error {
		switch ev := e.(type) {
		case LLMReasoningDeltaEvent:
			reasons = append(reasons, ev.Delta)
		case LLMEndEvent:
			c := ev
			end = &c
		}
		return nil
	})
	if err != nil {
		t.Fatalf("流应正常结束，got err: %v", err)
	}
	if end == nil {
		t.Fatal("必须发出 llm_end")
	}
	return end.Reasoning, reasons, end.Usage
}

// 两条通道都要过：SDK 通道（minimal=false）与 minimal 自建通道。
var reasoningChannelCases = []struct {
	name string
	caps OpenAICompatCapabilities
}{
	{"SDK通道", CustomOpenAICompatCapabilities()},
	{"minimal通道", MinimalOpenAICompatCapabilities()},
}

// TestReasoningAliasFieldCaptured 三种常见形态都要进 LLMEnd.Reasoning + 逐片事件。
// reasoning_content 是 SDK 原生字段（对照组，证明兼容层没有破坏既有端点）。
func TestReasoningAliasFieldCaptured(t *testing.T) {
	const want = "先算 9.90 再比较"
	parts := []string{"先算", " 9.90", " 再比较"}
	for _, ch := range reasoningChannelCases {
		for _, field := range []string{"reasoning_content", "reasoning", "thinking"} {
			t.Run(ch.name+"/"+field, func(t *testing.T) {
				full, deltas, _ := streamOnce(t, ch.caps, reasoningSSE(field, parts, "9.9 大"), "deepseek-v4.1-flash")
				if full != want {
					t.Fatalf("LLMEnd.Reasoning = %q, want %q（%s 形态未被捕获）", full, want, field)
				}
				if len(deltas) != len(parts) {
					t.Fatalf("reasoning delta 事件数 = %d, want %d（流式渲染依赖逐片事件）", len(deltas), len(parts))
				}
				if strings.Join(deltas, "") != want {
					t.Fatalf("逐片拼接 = %q, want %q", strings.Join(deltas, ""), want)
				}
			})
		}
	}
}

// TestReasoningDetailsTextCaptured reasoning_details[].text（openrouter 形态）取明文；
// 只带密文的条目（type=reasoning.encrypted，gpt-6-luna 实测）不得伪造文本。
func TestReasoningDetailsTextCaptured(t *testing.T) {
	plain := `data: {"id":"c1","choices":[{"index":0,"delta":{"reasoning_details":` +
		`[{"index":0,"type":"reasoning.text","text":"明文一"},{"index":0,"type":"reasoning.text","text":"明文二"}]}}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	encrypted := `data: {"id":"c1","choices":[{"index":0,"delta":{"reasoning_details":` +
		`[{"index":0,"type":"reasoning.encrypted","data":"gAAAAABxxxx"}]}}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	for _, ch := range reasoningChannelCases {
		t.Run(ch.name+"/plaintext", func(t *testing.T) {
			full, _, _ := streamOnce(t, ch.caps, plain, "glm-5.3-flash")
			if full != "明文一明文二" {
				t.Fatalf("reasoning_details 明文应拼接进 LLMEnd.Reasoning, got %q", full)
			}
		})
		t.Run(ch.name+"/encrypted", func(t *testing.T) {
			full, deltas, _ := streamOnce(t, ch.caps, encrypted, "gpt-6-luna")
			if full != "" || len(deltas) != 0 {
				t.Fatalf("只有密文时不得伪造思考文本: Reasoning=%q deltas=%d", full, len(deltas))
			}
		})
	}
}

// TestReasoningStandardShapeUntouched 已是标准形态的端点（含 OpenAI 官方）行为不变：
// 内容与用量逐字节等价，且帧体不被改写（快速路径零成本跳过）。
func TestReasoningStandardShapeUntouched(t *testing.T) {
	line := `data: {"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"原生"}}]}` + "\n"
	got := rewriteSSELine([]byte(line))
	if string(got) != line {
		t.Fatalf("标准形态被改写了（应逐字节透传）:\n got %q\nwant %q", string(got), line)
	}

	full, _, usage := streamOnce(t, CustomOpenAICompatCapabilities(),
		reasoningSSE("reasoning_content", []string{"原生"}, "答"), "deepseek-v4-flash")
	if full != "原生" {
		t.Fatalf("reasoning_content 端点应行为不变, got %q", full)
	}
	if usage.Reasoning != 12 {
		t.Fatalf("usage.Reasoning 口径不得被兼容层影响, got %d", usage.Reasoning)
	}
}

// TestReasoningAliasKeepsOriginalFields 只增不改：原字段（含 reasoning_details）必须保留，
// 未知扩展键与数字原样不动 —— 审计/排查仍能看到上游真实形态。
func TestReasoningAliasKeepsOriginalFields(t *testing.T) {
	payload := []byte(`{"id":"c1","choices":[{"index":0,"delta":{"reasoning":"R",` +
		`"reasoning_details":[{"index":0,"type":"reasoning.text","text":"R"}]},` +
		`"custom_big":12345678901234567890}],"vendor":{"x":1.50}}`)
	out, changed := normalizeStreamPayload(payload, true, false)
	if !changed {
		t.Fatal("带 reasoning 的帧应被归一化")
	}
	s := string(out)
	for _, want := range []string{`"reasoning":"R"`, `"reasoning_content":"R"`, `"reasoning_details"`,
		`"custom_big":12345678901234567890`, `"vendor":{"x":1.50}`} {
		if !strings.Contains(s, want) {
			t.Fatalf("归一化后丢失 %s:\n%s", want, s)
		}
	}
}

// TestReasoningSSELineBoundaries 分帧边界：无尾换行的残留收尾帧、非 data 行、
// [DONE]、残缺 JSON 都不得被吞掉或改坏（流末尾丢帧会让 finish_reason/usage 归零）。
func TestReasoningSSELineBoundaries(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "" = 原样返回
	}{
		{"[DONE]", "data: [DONE]", ""},
		{"空帧", "data: ", ""},
		{"事件名行", "event: message", ""},
		{"注释行", ": keep-alive", ""},
		{"残缺 JSON", `data: {"choices":[{"delta":{"reasoning":"R"`, ""},
		{"非 choices 帧", `data: {"id":"c1","usage":null}`, ""},
		{"无 delta 形态", `data: {"choices":[{"message":{"reasoning":"R"}}]}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(rewriteSSELine([]byte(c.in)))
			if c.want == "" {
				if got != c.in {
					t.Fatalf("应原样返回:\n got %q\nwant %q", got, c.in)
				}
				return
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}

	// 无尾换行的思考帧仍要被改写（且不擅自补换行 —— 由调用方的分行逻辑负责）。
	got := string(rewriteSSELine([]byte(`data: {"choices":[{"delta":{"reasoning":"R"}}]}`)))
	if !strings.Contains(got, `"reasoning_content":"R"`) || strings.HasSuffix(got, "\n") {
		t.Fatalf("残留行改写错误: %q", got)
	}
}

// TestReasoningStreamNonSSEUntouched 非 SSE 响应（模型列表 / 非流式 JSON / 空 Content-Type）
// 必须不做任何包装：那些路径不该被改写，也不该多一次缓冲。
func TestReasoningStreamNonSSEUntouched(t *testing.T) {
	for _, ct := range []string{"application/json", "text/plain", ""} {
		t.Run("ct="+ct, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if ct != "" {
					w.Header().Set("Content-Type", ct)
				}
				io.WriteString(w, `{"data":[{"id":"deepseek-v4-flash"}]}`)
			}))
			defer srv.Close()

			c := WrapReasoningStreamClient(srv.Client())
			resp, err := c.Get(srv.URL)
			if err != nil {
				t.Fatalf("请求失败: %v", err)
			}
			defer resp.Body.Close()
			if _, ok := resp.Body.(*reasoningStreamBody); ok {
				t.Fatal("非 SSE 响应不得被包装")
			}
			body, _ := io.ReadAll(resp.Body)
			if string(body) != `{"data":[{"id":"deepseek-v4-flash"}]}` {
				t.Fatalf("非 SSE 响应体被改动: %q", body)
			}
		})
	}
}

// TestReasoningStreamWrapperIdempotent 重复包装幂等；nil 进 nil 出；
// BaseTransport 能穿透本层（网关准入检查依赖它按类型断言底层 transport）。
func TestReasoningStreamWrapperIdempotent(t *testing.T) {
	if WrapReasoningStreamClient(nil) != nil {
		t.Fatal("nil 客户端应原样返回 nil")
	}
	c := &http.Client{}
	once := WrapReasoningStreamClient(c)
	twice := WrapReasoningStreamClient(once)
	if once.Transport != twice.Transport {
		t.Fatal("重复包装应幂等（构造点在热更新路径上会被重复调用）")
	}
	if got := BaseTransport(twice.Transport); got != http.RoundTripper(http.DefaultTransport) {
		t.Fatalf("BaseTransport 应穿透到 DefaultTransport（与 WrapUpstreamCostClient 同约定）, got %T", got)
	}
	// 底层是具体 transport 时要能原样取回
	sentinel := &http.Transport{MaxIdleConns: 7}
	wrapped := WrapReasoningStreamClient(&http.Client{Transport: sentinel})
	if got := BaseTransport(wrapped.Transport); got != http.RoundTripper(sentinel) {
		t.Fatalf("BaseTransport 应返回原始 transport, got %T", got)
	}
}

// TestBaseTransportUnwrapsBothLayers 构造点把两层包在一起（尾部回看 + 思考归一化），
// BaseTransport 必须一次穿透到原始 transport —— 网关准入检查靠它做类型断言。
func TestBaseTransportUnwrapsBothLayers(t *testing.T) {
	sentinel := &http.Transport{MaxIdleConns: 3}
	for _, order := range []string{"cost外层", "reasoning外层"} {
		t.Run(order, func(t *testing.T) {
			base := http.RoundTripper(sentinel)
			var layered http.RoundTripper
			if order == "cost外层" {
				layered = WrapReasoningStreamClient(WrapUpstreamCostClient(&http.Client{Transport: base})).Transport
			} else {
				wrapped := WrapReasoningStreamClient(&http.Client{Transport: base})
				layered = WrapUpstreamCostClient(wrapped).Transport
			}
			if got := BaseTransport(layered); got != base {
				t.Fatalf("BaseTransport 未穿透两层（%s）, got %T", order, got)
			}
		})
	}
}

// TestReasoningUsageUnaffected 兼容层不得影响计费口径：reasoning_tokens 仍来自
// completion_tokens_details（文本与 token 是两条独立路径）。
func TestReasoningUsageUnaffected(t *testing.T) {
	_, _, usage := streamOnce(t, CustomOpenAICompatCapabilities(),
		reasoningSSE("reasoning", []string{"思考"}, "答"), "deepseek-v4.1-flash")
	if usage.Reasoning != 12 {
		t.Fatalf("usage.Reasoning = %d, want 12（计费口径与字段名兼容互不影响）", usage.Reasoning)
	}
	if usage.Output != 20 || usage.Input != 10 {
		t.Fatalf("用量被兼容层影响: %+v", usage)
	}
}

// usage 归一（2026-10）：字段名与位置都因端点而异，SDK 只认标准形态。
// 实测：DeepSeek 两个字段都发（cached_tokens=3840 / prompt_cache_hit_tokens=3840），
// 但「只发原生名」的网关会把缓存命中记成 miss（成本虚高）；Moonshot 把 usage
// 放进 choices[].usage 则会被整帧丢弃（usage 归零）。
func TestUsageAliasNormalization(t *testing.T) {
	t.Run("只发 prompt_cache_hit_tokens → 补进明细", func(t *testing.T) {
		in := `data: {"id":"c","choices":[],"usage":{"prompt_tokens":4000,"completion_tokens":8,` +
			`"total_tokens":4008,"prompt_cache_hit_tokens":3840}}`
		out := string(rewriteSSELine([]byte(in)))
		for _, want := range []string{
			`"cached_tokens":3840`,
			`"prompt_cache_hit_tokens":3840`, // 原字段保留（只增不改）
			`"prompt_tokens":4000`,
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("缺少 %s:\n%s", want, out)
			}
		}
	})

	t.Run("明细已给出（含 0）→ 不改写", func(t *testing.T) {
		in := `data: {"choices":[],"usage":{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":0},` +
			`"prompt_cache_hit_tokens":3840}}`
		if out := string(rewriteSSELine([]byte(in))); out != in {
			t.Fatalf("明细存在时不应改写:\n in=%s\nout=%s", in, out)
		}
	})

	t.Run("choices[].usage（Moonshot 位置）→ 提升到顶层", func(t *testing.T) {
		in := `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop",` +
			`"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}]}`
		out := string(rewriteSSELine([]byte(in)))
		if !strings.Contains(out, `"usage":{"prompt_tokens":11`) {
			t.Fatalf("顶层 usage 未提升:\n%s", out)
		}
		if !strings.Contains(out, `"choices"`) {
			t.Fatalf("choices 被吞:\n%s", out)
		}
	})

	t.Run("顶层已有 usage → 不动 choices 里的", func(t *testing.T) {
		in := `data: {"choices":[{"index":0,"usage":{"prompt_tokens":99}}],` +
			`"usage":{"prompt_tokens":11}}`
		out := string(rewriteSSELine([]byte(in)))
		if !strings.Contains(out, `"usage":{"prompt_tokens":11}`) {
			t.Fatalf("顶层 usage 被覆盖:\n%s", out)
		}
	})

	t.Run("usage 为 null 的普通帧 → 一字节不动", func(t *testing.T) {
		in := `data: {"choices":[{"index":0,"delta":{"content":"hi"}}],"usage":null}`
		if out := string(rewriteSSELine([]byte(in))); out != in {
			t.Fatalf("普通帧不应改写:\n in=%s\nout=%s", in, out)
		}
	})
}
