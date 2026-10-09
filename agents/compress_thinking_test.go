package agents

import (
	"context"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

// thinkingProbeProvider 可控的压缩 provider：捕获请求，按预设返回摘要。
// 自包含（不依赖同包其它测试文件的同名类型 compressProvider）：本文件只验压缩的
// 关思考行为，helper 放这里让它在任何基线上都能独立编译通过。
type thinkingProbeProvider struct {
	req     *provider.StreamRequest
	content string
	err     error
}

func (p *thinkingProbeProvider) Stream(_ context.Context, req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	p.req = req
	if p.err != nil {
		return p.err
	}
	return handler(provider.LLMEndEvent{
		FinishReason: core.FinishReasonStop,
		Content:      p.content,
	})
}

// compactMessages 构造一段可压缩的最小上下文（system + 两轮历史 + 最新用户输入）。
func compactMessages() []core.Message {
	return []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "第一轮任务"}),
		core.NewAssistantMessage([]core.Content{{Type: core.ContentTypeText, Content: "好"}}, "", nil),
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "继续，帮我改这个 bug"}),
	}
}

// TestCompressDisablesThinkingForThinkingModel 思考模型：压缩请求显式关思考
// （Effort 仍固定 high —— 关思考后 provider 侧把档位表达成 none/disabled，见
// ResolveThinking）。省的是「跑不跑思考链」，不是档位本身。
func TestCompressDisablesThinkingForThinkingModel(t *testing.T) {
	p := &thinkingProbeProvider{content: "<summary>## Goal\n- x</summary>"}
	c := NewLLMCompressor(p, "deepseek-v4-flash", WithCompressProviderName("deepseek"))
	if _, err := c.Compact(context.Background(), compactMessages()); err != nil {
		t.Fatal(err)
	}
	cfg := p.req.Config
	if cfg == nil || cfg.Thinking == nil {
		t.Fatalf("思考模型的压缩请求应显式带 thinking 开关（关思考）：%+v", cfg)
	}
	if *cfg.Thinking {
		t.Fatalf("thinking = true, want false（压缩应关思考）")
	}
	if cfg.Effort != provider.ReasoningEffortLevelHigh {
		t.Fatalf("摘要档位仍应固定 high（关思考由开关表达，不靠降档）：%q", cfg.Effort)
	}
}

// TestCompressLeavesThinkingOffModelsAlone 不可关思考的模型：压缩请求**不带**开关。
// 两类各一个样本，都走同一条防线「不带 = 不发模型不认识的字段」：
//   - 强制思考（glm-5.3：官方明确 disabled 会 400）
//   - 非推理模型（gpt-4o：没有思考能力，发 none 也无意义）
func TestCompressLeavesThinkingOffModelsAlone(t *testing.T) {
	for _, tc := range []struct{ providerName, model, why string }{
		{"zhipu", "glm-5.3", "强制思考模型：disabled 会 400"},
		{"openai", "gpt-4o", "非推理模型：没有思考可关"},
	} {
		p := &thinkingProbeProvider{content: "<summary>## Goal\n- x</summary>"}
		c := NewLLMCompressor(p, tc.model, WithCompressProviderName(tc.providerName))
		if _, err := c.Compact(context.Background(), compactMessages()); err != nil {
			t.Fatalf("%s/%s: %v", tc.providerName, tc.model, err)
		}
		if got := p.req.Config; got == nil || got.Thinking != nil {
			t.Errorf("%s/%s（%s）：压缩请求不应带 thinking 开关，want nil，got %+v",
				tc.providerName, tc.model, tc.why, got)
		}
	}
}

// TestCompressThinkingOffEscapeHatch 逃生舱：WithCompressThinkingOff 显式关掉
// 「压缩关思考」策略后，连思考模型也不发开关（回到跟随厂商默认的旧行为）。
func TestCompressThinkingOffEscapeHatch(t *testing.T) {
	p := &thinkingProbeProvider{content: "<summary>## Goal\n- x</summary>"}
	c := NewLLMCompressor(p, "deepseek-v4-flash",
		WithCompressProviderName("deepseek"), WithCompressThinkingOff())
	if _, err := c.Compact(context.Background(), compactMessages()); err != nil {
		t.Fatal(err)
	}
	if got := p.req.Config; got == nil || got.Thinking != nil {
		t.Fatalf("关掉策略后不应带 thinking 开关：%+v", got)
	}
}

// TestCompressRegistryOverrideDecidesThinking 关思考判定必须读**注入的**注册表，
// 而不是另建一个只看内置表的注册表：用户在 settings 里为自建模型补的思考声明
// 只存在于 bridge 那一个实例 —— 判错方向时线上才会 400，本地一片绿。
//
// 三个方向一起钉（2026-10 线上 400 后加固）：
//  1. 未知模型 **不注入** registry → 判不可关 → 不发开关（这条是 400 的修复本身：
//     内置表没有 step5-preview-free 这类网关模型时，从前会判成可关并发 disabled）；
//  2. 未知模型 + 注入标了 force-on 的 registry → 同样不发开关（读到标注）；
//  3. 补一条"不是 force-on"的 override → 判定转正成可关（证明逃生路径有效，
//     用户确实知道某模型能关时有办法让它关）。
func TestCompressRegistryOverrideDecidesThinking(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Override(provider.ModelInfo{
		ID: "my-thinking-model", Provider: "custom", Protocol: "chat_completions",
		ContextWindow: 200000, MaxTokens: 8192,
		ThinkingForceOn: true, // 用户标注：这个模型 disabled 会 400
	})
	reg.Override(provider.ModelInfo{
		ID: "my-toggle-model", Provider: "custom", Protocol: "chat_completions",
		ContextWindow: 200000, MaxTokens: 8192,
		Reasoning: true, ThinkingToggleOnly: true, // 有思考能力、可关
	})

	// ① 未知模型、不注入 registry：内置表查不到 → 判不可关 → 不发开关
	pNoReg := &thinkingProbeProvider{content: "<summary>## Goal\n- x</summary>"}
	cNoReg := NewLLMCompressor(pNoReg, "my-thinking-model", WithCompressProviderName("custom"))
	if _, err := cNoReg.Compact(context.Background(), compactMessages()); err != nil {
		t.Fatal(err)
	}
	if got := pNoReg.req.Config; got == nil || got.Thinking != nil {
		t.Fatalf("未知模型应判不可关、不发 thinking 开关（判成可关就是线上那个 400）: %+v", got)
	}

	// ② 注入 bridge 的 registry：读到 force-on 标注 → 同样不发开关
	pWithReg := &thinkingProbeProvider{content: "<summary>## Goal\n- x</summary>"}
	cWithReg := NewLLMCompressor(pWithReg, "my-thinking-model",
		WithCompressProviderName("custom"), WithCompressRegistry(reg))
	if _, err := cWithReg.Compact(context.Background(), compactMessages()); err != nil {
		t.Fatal(err)
	}
	if got := pWithReg.req.Config; got == nil || got.Thinking != nil {
		t.Fatalf("注入的 registry 标了 force-on，压缩请求不得带 thinking 开关：%+v", got)
	}

	// ③ 逃生路径：补一条"有思考能力且非 force-on"的 override → 判定转正，发开关。
	// 证明"未知模型不可关"不会把用户知道能关的模型也堵死。
	pToggle := &thinkingProbeProvider{content: "<summary>## Goal\n- x</summary>"}
	cToggle := NewLLMCompressor(pToggle, "my-toggle-model",
		WithCompressProviderName("custom"), WithCompressRegistry(reg))
	if _, err := cToggle.Compact(context.Background(), compactMessages()); err != nil {
		t.Fatal(err)
	}
	if got := pToggle.req.Config; got == nil || got.Thinking == nil || *got.Thinking {
		t.Fatalf("补了 toggle-only override 后应判可关并发关（逃逸通道必须有效）: %+v", got)
	}
}

// TestCompressRequestIntactWhenThinkingNotDisabled 不可关思考的模型上，压缩请求的
// **其余部分必须完好** —— 修"不发 thinking 开关"时不能顺手把别的也弄丢。
//
// 这条与上面几条是互补关系：上面断言"开关不该发"，这里断言"除了开关都还在"。
// 线上 400 的修复冷不丁把这整个请求削弱（session id 丢了 → 网关 400 另一种错、
// MaxTokens 归零 → 截断），也是回归，且更难查。
func TestCompressRequestIntactWhenThinkingNotDisabled(t *testing.T) {
	p := &thinkingProbeProvider{content: "<summary>## Goal\n- x</summary>"}
	c := NewLLMCompressor(p, "step5-preview-free",
		WithCompressProviderName("opencode"), WithCompressSessionID("sid-1"))
	res, err := c.Compact(context.Background(), compactMessages())
	if err != nil {
		t.Fatalf("不可关思考的模型上压缩应正常完成（回到旧行为，不是失败）: %v", err)
	}
	if res.Summary == "" {
		t.Error("摘要不应为空")
	}
	if p.req.SessionID != "sid-1" {
		t.Errorf("session id 应照常下发（OpenCode Go 网关缺它直接 400）: %q", p.req.SessionID)
	}
	if p.req.Config.MaxTokens == 0 {
		t.Error("MaxTokens 不应归零（0 = 上限丢失，摘要会被截断）")
	}
	if p.req.Config.Effort != provider.ReasoningEffortLevelHigh {
		t.Errorf("摘要档位仍应固定 high: %q", p.req.Config.Effort)
	}
	if p.req.Provider != "opencode" || p.req.Model != "step5-preview-free" {
		t.Errorf("provider/model 不得被改写: %q/%q", p.req.Provider, p.req.Model)
	}
}

// TestCompressThinkingOffOnUnknownProvider provider 名为空（零值装配 / 旧调用）时
// 判定退化为「按模型名全局解析」：内置表里有的模型名（deepseek 系）照样判可关并发开关
// —— 说明"未知模型不可关"只影响真的查不到的模型，不会把跨 provider 解析也一起堵死。
func TestCompressThinkingOffOnUnknownProvider(t *testing.T) {
	p := &thinkingProbeProvider{content: "<summary>## Goal\n- x</summary>"}
	c := NewLLMCompressor(p, "deepseek-v4-flash") // 不注入 WithCompressProviderName
	if _, err := c.Compact(context.Background(), compactMessages()); err != nil {
		t.Fatal(err)
	}
	if got := p.req.Config; got == nil || got.Thinking == nil || *got.Thinking {
		t.Fatalf("provider 名为空时按名解析应仍判可关并发开关：%+v", got)
	}
}
