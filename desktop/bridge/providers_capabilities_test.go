package main

import (
	"testing"
)

// TestModelCapabilitiesGatewayAlias 「拉取模型列表」的别名兜底（2026-09-23）：
// 聚合网关（command-code）把上游模型换名暴露（"xiaomi/mimo-v2.6-pro"），精确查必然
// miss —— 不兜底就会把上游 1M 窗口 / 128K 输出的模型填成默认窗口 / 8192 上限
// （用户实测：command-code 的 xiaomi 系模型拉下来全是 128000/8192，拉完还得手改）。
func TestModelCapabilitiesGatewayAlias(t *testing.T) {
	assertNoUserPrices(t)
	t.Setenv("HOME", t.TempDir()) // 不读用户真实 settings.json
	pc := loadProviderConfig()

	caps := pc.modelCapabilities("command-code", []string{"xiaomi/mimo-v2.6-pro", "xiaomi/mimo-v2.5", "nope-model"})

	pro, ok := caps["xiaomi/mimo-v2.6-pro"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 xiaomi/mimo-v2.6-pro 能力: %#v", caps)
	}
	if got, _ := pro["context_window"].(int64); got != 1048576 {
		t.Errorf("别名窗口 = %v, want 1048576（上游 xiaomi 条目）", pro["context_window"])
	}
	if got, _ := pro["max_tokens"].(int64); got != 131072 {
		t.Errorf("别名输出上限 = %v, want 131072", pro["max_tokens"])
	}
	if inputs, _ := pro["inputs"].([]string); len(inputs) < 2 || inputs[1] != "image" {
		t.Errorf("别名模态 = %v, want 含 image（全模态）", pro["inputs"])
	}
	if cost, _ := pro["cost"].(map[string]any); cost == nil {
		t.Error("别名应带内置价（前端价格编辑器回显）")
	}

	// 未知模型仍走默认（不虚报）
	unknown, _ := caps["nope-model"].(map[string]any)
	if got := numOf(unknown["max_tokens"]); got != int64(defaultMaxTokens) {
		t.Errorf("未知模型输出上限 = %v, want %d", unknown["max_tokens"], defaultMaxTokens)
	}
	if got := numOf(unknown["context_window"]); got != int64(defaultContextWindow) {
		t.Errorf("未知模型窗口 = %v, want %d", unknown["context_window"], defaultContextWindow)
	}
}

// numOf 把 map[string]any 里的数值字段统一成 int64（默认值路径是 int，注册表路径是 int64）。
func numOf(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return -1
}
