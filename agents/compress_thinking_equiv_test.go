package agents

import (
	"context"
	"reflect"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

// equivProbe 记录整份 StreamRequest（含 Config），供等价性对比。
// 自包含，不依赖同包其它测试文件的 probe 类型。
type equivProbe struct{ req *provider.StreamRequest }

func (p *equivProbe) Stream(_ context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	p.req = req
	return h(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "<summary>ok</summary>"})
}

// compactRequestWithOpts 跑一次压缩并返回捕获到的请求。
func compactRequestWithOpts(t *testing.T, model string, opts ...LLMCompressorOption) *provider.StreamRequest {
	t.Helper()
	p := &equivProbe{}
	c := NewLLMCompressor(p, model, opts...)
	if _, err := c.Compact(context.Background(), compactMessages()); err != nil {
		t.Fatalf("Compact(%s): %v", model, err)
	}
	return p.req
}

// normalize 把请求拍平成可 reflect.DeepEqual 比较的值（Config 指针解引用）——
// 指针地址不同但值相同即视为等价。
func normalize(r *provider.StreamRequest) provider.StreamRequest {
	out := *r
	if r.Config != nil {
		cfg := *r.Config
		if cfg.Thinking != nil {
			b := *cfg.Thinking
			cfg.Thinking = &b
		}
		out.Config = &cfg
	}
	return out
}

// baseCompressOpts 两条路径共用的装配（只差 thinking 策略那一项）。
func baseCompressOpts(prov, model, sid string) []LLMCompressorOption {
	return []LLMCompressorOption{
		WithCompressProviderName(prov),
		WithCompressSessionID(sid),
		WithSummaryMaxTokens(8192),
		WithSummaryVersion(3),
		WithKeepRecentMessages(10),
	}
}

// TestUnknownModelRequestFieldsPinned 未识别模型：请求参数**逐字段钉死**成改动前的值。
//
// 为什么需要这条（而不是只靠等价性测试）：等价性测试让新旧两条路径都过
// compressConfig()，任何加在 compressConfig() 里、对两条路径一视同仁的改动
// （比如 MaxTokens 被顺手除 2）会让两边一起变，仍然"相等"—— 测试抓不住。
// 这条改用**独立于实现的常量基线**，那种"顺手改了别的"立刻红。
//
// 基线 = 本 PR 之前的产物：Thinking 不带，Effort 固定 high，MaxTokens 透传压缩器配置。
func TestUnknownModelRequestFieldsPinned(t *testing.T) {
	const (
		prov  = "opencode"
		model = "step5-preview-free" // 内置表没有 → 未知模型
		sid   = "sid-pinned"
	)
	req := compactRequestWithOpts(t, model,
		WithCompressProviderName(prov), WithCompressSessionID(sid),
		WithSummaryMaxTokens(8192), WithSummaryVersion(3), WithKeepRecentMessages(10))

	// 请求头
	if req.Provider != prov || req.Model != model || req.SessionID != sid {
		t.Errorf("请求头被改：provider=%q model=%q sid=%q，want %q/%q/%q",
			req.Provider, req.Model, req.SessionID, prov, model, sid)
	}
	// Config：Thinking 必须 nil；其余逐字段等于基线
	if req.Config == nil {
		t.Fatal("Config 不得为 nil")
	}
	if req.Config.Thinking != nil {
		t.Errorf("未识别模型不得带 thinking 开关：%+v", req.Config.Thinking)
	}
	if req.Config.Effort != provider.ReasoningEffortLevelHigh {
		t.Errorf("Effort = %q, want high（基线值）", req.Config.Effort)
	}
	if req.Config.MaxTokens != 8192 {
		t.Errorf("MaxTokens = %d, want 8192（压缩器配置原样透传）", req.Config.MaxTokens)
	}
	// 消息体仍是 [压缩指令, 渲染后的历史] 两条
	if len(req.Messages) != 2 || req.Messages[0].Role != core.System || req.Messages[1].Role != core.User {
		t.Errorf("压缩请求消息体形状被改：%d 条（want 2：system 指令 + user 历史）", len(req.Messages))
	}
}

// TestUnknownModelRequestIdenticalToPreChange 未识别模型：压缩请求与**改动前逐字段相同**。
//
// 这是用户的口径承诺（2026-10-09）：「providers.json 里未识别的模型，保持模型请求参数
// 不变，至少保证能正常做上下文压缩」。所以不能只断言"没发 thinking 开关"—— 得证明整份
// 请求（Model/Provider/SessionID/Config 全部字段/Messages）和旧行为**等价**。
//
// 注意覆盖边界：这条只对"仅影响 thinking 分支"的改动敏感；对 compressConfig() 里
// 一视同仁的改动要靠上面那条 TestUnknownModelRequestFieldsPinned（常量基线）。
//
// 旧行为的等价物 = WithCompressThinkingOff()（彻底关掉策略 = 本 PR 之前的代码路径）。
// 两条路径跑同一段上下文，产物必须 DeepEqual。
func TestUnknownModelRequestIdenticalToPreChange(t *testing.T) {
	const (
		prov  = "opencode"
		model = "step5-preview-free" // 内置表没有 → 未知模型
		sid   = "sid-equiv"
	)
	base := baseCompressOpts(prov, model, sid)

	// 新路径：默认策略（会尝试关思考），模型未知 → 判不可关 → 不带开关
	newPath := compactRequestWithOpts(t, model, base...)

	// 旧路径：显式关掉策略（= PR 之前的代码）
	oldOpts := append(append([]LLMCompressorOption(nil), base...), WithCompressThinkingOff())
	oldPath := compactRequestWithOpts(t, model, oldOpts...)

	if newPath.Config.Thinking != nil {
		t.Fatalf("未识别模型不得带 thinking 开关（= 参数被改过）: %+v", newPath.Config.Thinking)
	}
	if !reflect.DeepEqual(normalize(newPath), normalize(oldPath)) {
		t.Fatalf("未识别模型的压缩请求与改动前不一致：\n新 = %+v / %+v\n旧 = %+v / %+v",
			normalize(newPath), *newPath.Config, normalize(oldPath), *oldPath.Config)
	}
	t.Logf("✓ 未识别模型请求与改动前逐字段一致（Thinking=<nil>，其余不变）")
}

// TestRecognizedModelRequestDoesChangeThinking 反向对照：内置表里的模型**确实**会带
// 开关 —— 证明等价性断言不是"策略根本没接上"的恒真。
func TestRecognizedModelRequestDoesChangeThinking(t *testing.T) {
	const (
		prov  = "opencode"
		model = "deepseek-v4-flash" // 内置表有、非 force-on → 可关
		sid   = "sid-equiv"
	)
	base := baseCompressOpts(prov, model, sid)

	newPath := compactRequestWithOpts(t, model, base...)
	oldOpts := append(append([]LLMCompressorOption(nil), base...), WithCompressThinkingOff())
	oldPath := compactRequestWithOpts(t, model, oldOpts...)

	if reflect.DeepEqual(normalize(newPath), normalize(oldPath)) {
		t.Fatal("内置表模型的新旧请求竟然完全相同 —— 关思考策略没生效（等价性测试会恒真）")
	}
	if newPath.Config.Thinking == nil {
		t.Fatal("deepseek-v4-flash 应带 thinking 开关")
	}
	t.Logf("✓ 内置表模型确实带开关（Thinking=%v）→ 等价性断言有区分力", *newPath.Config.Thinking)
}
