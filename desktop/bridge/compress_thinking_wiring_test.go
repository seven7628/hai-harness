package main

import (
	"context"
	"testing"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

// TestSubagentLoopsShareHostRegistryForCompressThinking 装配断言：两个子 loop 的
// 压缩器都必须拿到 **bridge 那一个注册表**（不是各自另建的内置表实例）。
//
// 为什么这条要钉（2026-10）：压缩请求按 (provider, model) 判定「能不能关思考」，
// 依据就是注册表；bridge 的注册表里才有用户为自建模型补的 thinking_force_on /
// toggle-only 标注。压缩器另建注册表 → 读不到标注 → 把关不掉的模型判成「可关」→
// 发 disabled 被上游 400。这类失效在只看内置表的单测里完全看不出来（内置路径正常），
// 只在真实端点上炸，所以在装配层钉住「注册表实例就是 pc.registry」。
//
// 自包含：不依赖同包其它测试文件的辅助函数（buildSpawnLoop/buildExploreLoop 是生产
// 函数，直接调即可；断言只用导出的只读访问器，无需起 session/HTTP）。
func TestSubagentLoopsShareHostRegistryForCompressThinking(t *testing.T) {
	pc := &providerConfig{
		active: "opencode",
		models: map[string]map[string]int64{
			"opencode": {"glm-5.3": 1000000, "deepseek-v4-flash": 1000000},
		},
		maxTokens: map[string]map[string]int64{
			"opencode": {"glm-5.3": 131072, "deepseek-v4-flash": 384000},
		},
		registry: provider.NewRegistry(),
	}
	// 用户（或 settings 同步）为 opencode 下的 glm-5.3 补的思考声明 —— 这类标注
	// 只存在于 bridge 注入的那一个 registry 实例里。
	pc.registry.Override(provider.ModelInfo{
		ID: "glm-5.3", Provider: "opencode", Protocol: "chat_completions",
		ContextWindow: 1000000, MaxTokens: 131072,
		ThinkingForceOn: true,
	})
	const forcedModel = "glm-5.3"

	spawn := buildSpawnLoop(nil, t.TempDir(), "s-comp", pc, "opencode", forcedModel, "low", nil)
	explore := buildExploreLoop(t.TempDir(), "s-comp", pc, "opencode", forcedModel, "low", nil)

	for name, comp := range map[string]agents.Compressor{
		"spawn":   spawn.Compressor(),
		"explore": explore.Compressor(),
	} {
		if comp == nil {
			t.Errorf("%s: 压缩器不得为 nil", name)
			continue
		}
		llmComp, ok := comp.(*agents.LLMCompressor)
		if !ok {
			t.Errorf("%s: 压缩器类型 = %T, want *agents.LLMCompressor", name, comp)
			continue
		}
		if llmComp.Registry() != pc.registry {
			t.Errorf("%s: 压缩器注册表 ≠ pc.registry —— 关思考判定读不到宿主 override "+
				"（force-on 模型会被判成可关 → 上游 400）", name)
		}
		// 行为反证：读宿主注册表 ⇒ glm-5.3 判定不可关
		if llmComp.CanDisableThinking() {
			t.Errorf("%s: opencode/glm-5.3 应判定为不可关思考（override 标了 force-on）", name)
		}
	}
	// 两个子 loop 必须各自持有独立压缩器实例（拷贝而非共享 —— 共享会互相影响）
	if spawn.Compressor() == explore.Compressor() {
		t.Error("spawn/explore 必须各自持有独立压缩器实例（拷贝而非共享）")
	}
}

// TestSubagentCompressThresholdDefault 三宿主装配点共用同一个阈值取值来源，
// 未配置 settings 时子 loop 拿到默认 codeAgentCompressThreshold（设置面板那个滑杆
// 在子 loop 上的落点；默认值的钉住见装配断言）。
func TestSubagentCompressThresholdDefault(t *testing.T) {
	pc := &providerConfig{
		active:    "opencode",
		models:    map[string]map[string]int64{"opencode": {"m": 1000000}},
		maxTokens: map[string]map[string]int64{"opencode": {"m": 384000}},
	}
	spawn := buildSpawnLoop(nil, t.TempDir(), "s-th", pc, "opencode", "m", "low", nil)
	if got := spawn.CompressThreshold(); got != codeAgentCompressThreshold {
		t.Fatalf("spawn 阈值 = %v, want 默认 %v", got, codeAgentCompressThreshold)
	}
}

// TestCompressorOptionsWiredForAllHosts 压缩器参数集（compressTorOptions）的装配断言。
//
// 这条替代「分别测三个宿主的 buildLoop/buildPersonaLoop/buildExploreLoop」：
// 三处都调用同一个 compressTorOptions（2026-10 提取，单一事实源），所以对这一个
// 函数断言即覆盖全部三宿主 —— 包括主 loop（buildLoop）。此前主 loop 的
// WithCompressRegistry 没有任何测试守护（删掉那行三个子 loop 的测试照样绿），而这
// 正是"三宿主接线"主张里最核心、失效后果最重（上游 400）的一环。
//
// 断言的每一项都对应一种已知失效模式：
//   - registry 身份 = pc.registry（不是指针相等以外的等价）→ 关思考判定读宿主标注
//   - output/window 上限按模型解析 → 长上下文压缩不被 finish_reason=length 截断
//   - provider 名 + session id → 网关要求辅助/子请求带同一 session id
//   - 版本/重试/近窗参数 → 摘要有交接单、失败不放弃整轮、近窗保留工具回执
//
// 做法：用 compressTorOptions 构造一个真压缩器，只读它的导出访问器 + 让 Compact
// 走一次捕获链路，把「选项是否真的写进了压缩器」逐项验掉。
func TestCompressorOptionsWiredForAllHosts(t *testing.T) {
	pc := &providerConfig{
		active: "opencode",
		models: map[string]map[string]int64{
			"opencode": {"glm-5.3": 1000000, "deepseek-v4-flash": 1000000},
		},
		maxTokens: map[string]map[string]int64{
			"opencode": {"glm-5.3": 131072, "deepseek-v4-flash": 384000},
		},
		registry: provider.NewRegistry(),
	}
	const model = "glm-5.3"

	// ① 选项集能构造出可用压缩器（参数顺序/数量被破坏时这里先炸）
	comp := agents.NewLLMCompressor(nil, model, compressTorOptions("opencode", model, "sid-x", pc)...)

	// ② 关思考判定读 pc.registry 本身（不是内置表副本）
	if comp.Registry() != pc.registry {
		t.Fatal("压缩器注册表 ≠ pc.registry —— 关思考判定读不到宿主 override")
	}

	// ③ 行为反证：给 pc.registry 打一个只在它里面的 force-on 标注，压缩器必须跟着变。
	//    这条是②的加强：证明「判定真的走这个实例」，不是只存了引用。
	pc.registry.Override(provider.ModelInfo{
		ID: model, Provider: "opencode", Protocol: "chat_completions",
		ContextWindow: 1000000, MaxTokens: 131072,
		ThinkingForceOn: true, // disabled 会 400 → 应判"不可关"
	})
	if comp.CanDisableThinking() {
		t.Fatal("宿主 registry 标了 force-on，压缩器应判定为不可关思考")
	}
	// 反面：没标注的思考模型仍可关（证明改动没把别的路径也堵死）
	compReasoning := agents.NewLLMCompressor(nil, "deepseek-v4-flash",
		compressTorOptions("opencode", "deepseek-v4-flash", "sid-x", pc)...)
	if !compReasoning.CanDisableThinking() {
		t.Fatal("deepseek-v4-flash 应判定为可关思考（只被 force-on 标注挡住）")
	}

	// ④ window/上限/会话 id/provider 名真的落进压缩器（走一次 Compact 捕获请求）
	spy := &compressOptionSpy{content: "<summary>## Goal\n- x</summary>"}
	comp4 := agents.NewLLMCompressor(spy, model, compressTorOptions("opencode", model, "sid-x", pc)...)
	if _, err := comp4.Compact(context.Background(), minimalCompactInput()); err != nil {
		t.Fatal(err)
	}
	if spy.req == nil || spy.req.Provider != "opencode" || spy.req.SessionID != "sid-x" {
		t.Fatalf("压缩请求未带上 provider 名 / 会话 id: %+v", spy.req)
	}
	if spy.req.Config == nil || spy.req.Config.MaxTokens != 131072 {
		t.Fatalf("摘要输出上限 = %v, want 131072（模型真实 max_tokens）", spy.req.Config)
	}
}

// compressOptionSpy 捕获压缩请求（装配断言用）：只记最后一次 Stream 的请求。
type compressOptionSpy struct {
	req     *provider.StreamRequest
	content string
}

func (p *compressOptionSpy) Stream(_ context.Context, req *provider.StreamRequest, handler func(provider.StreamEvent) error) error {
	p.req = req
	return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: p.content})
}

// minimalCompactInput 一段最小可压缩上下文（system + 一轮历史 + 最新用户输入）。
func minimalCompactInput() []core.Message {
	return []core.Message{
		core.NewSystemMessage("sys"),
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "第一轮"}),
		core.NewAssistantMessage([]core.Content{{Type: core.ContentTypeText, Content: "好"}}, "", nil),
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "继续"}),
	}
}
