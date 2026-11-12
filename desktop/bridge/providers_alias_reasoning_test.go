package main

import (
	"testing"

	"github.com/seven7628/hai-harness/provider"
)

// 网关别名模型的推理元数据补全（2026-09-10 400 实证回归）：
// command-code-2 这类网关把上游模型以 "deepseek/deepseek-v4.1-flash" 别名暴露，
// providers.json 没有该 provider 条目 → overrideRegistryLocked 的基底元数据为零值，
// Reasoning=false 会盖掉 ShouldRetainReasoning 的模型名兜底：既不回传推理，
// 也不在 assistant 消息上补 reasoning_content 占位 → 上游一旦无法自补即
// 400 "The `reasoning_content` in the thinking mode must be passed back to the API"。
// 断言：别名 deepseek 模型经注册表同步后 retain=true（回传）+ rrc=true（恒带字段）；
// 非推理族别名（gpt-5.6-luna）保持 false（不多传占位）。
func TestOverrideRegistryDerivesAliasReasoningMeta(t *testing.T) {
	pc := &providerConfig{
		registry: provider.NewRegistry(),
		// syncRegistry 按 baseURL 枚举已配置 provider（与生产路径一致）
		baseURL: map[string]string{"command-code-2": "https://api.commandcode.ai/provider/v1"},
		// settings 只带窗口/上限（无内置元数据），复刻 command-code-2 的实际配置形状
		models: map[string]map[string]int64{
			"command-code-2": {"deepseek/deepseek-v4.1-flash": 1_000_000, "gpt-5.6-luna": 128_000},
		},
		maxTokens: map[string]map[string]int64{
			"command-code-2": {"deepseek/deepseek-v4.1-flash": 380_000, "gpt-5.6-luna": 8_192},
		},
	}
	pc.syncRegistry()

	alias := "deepseek/deepseek-v4.1-flash"
	if !pc.registry.ShouldRetainReasoning("command-code-2", alias, nil) {
		t.Fatal("别名 deepseek 模型必须回传推理（否则工具轮缺 reasoning_content → 上游 400）")
	}
	if !pc.registry.RequiresReasoningContent("command-code-2", alias) {
		t.Fatal("别名 deepseek 模型必须恒带 reasoning_content 字段（空推理 → 空格占位）")
	}
	// 二次同步（幂等路径）不得因首次 override 残留而丢失推导结果
	pc.syncRegistry()
	if !pc.registry.RequiresReasoningContent("command-code-2", alias) {
		t.Fatal("二次 sync 后推导结果丢失")
	}

	// 非思考族别名：不推导（保持保守，避免给非推理模型多发字段）
	if pc.registry.ShouldRetainReasoning("command-code-2", "gpt-5.6-luna", nil) {
		t.Fatal("gpt 系别名不应推导为回传推理")
	}
	if pc.registry.RequiresReasoningContent("command-code-2", "gpt-5.6-luna") {
		t.Fatal("gpt 系别名不应补 reasoning_content 占位")
	}

	// 内置元数据优先：deepseek 官方端点模型不受本兜底影响（窗口/上限/价 仍来自内置表）
	if !pc.registry.RequiresReasoningContent("deepseek", "deepseek-v4-flash") {
		t.Fatal("内置 deepseek 模型元数据不应被兜底逻辑破坏")
	}
	p, ok := pc.registry.PriceFor("deepseek", "deepseek-v4-flash")
	if !ok || p.Input == 0 {
		t.Fatalf("内置价不应受影响, got %+v ok=%v", p, ok)
	}
}
