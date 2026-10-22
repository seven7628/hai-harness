package provider

import (
	"net/url"
	"strings"
)

// 端点指纹：判断「这个端点收哪个字段名 / 哪种思考格式」这类**协议形状**问题。
//
// 为什么需要（2026-10 实测）：输出上限字段名是**端点**属性，不是模型属性 ——
//   - providers.json 里 18 家 chat-completions provider，每家内部字段名 100% 一致
//     （deepseek/kimi/zhipu/opencode/together/baseten/nvidia → max_tokens；
//      openai/groq/mistral/qwen/xiaomi/openrouter/… → max_completion_tokens）；
//   - 反过来，同一个模型 id 跨 provider 有 50/57 不一致（deepseek-v4-flash 在
//     deepseek/opencode=max_tokens，在 qwen=max_completion_tokens；mimo-v2.6 在
//     opencode=max_tokens、在 xiaomi=max_completion_tokens）。
//
// 所以「模型名」不能决定字段名，只能由端点决定；模型名只在端点完全未知时做最后兜底
// （同族官方口径），且证据最弱。
//
// 对齐 pi 的实现（packages/ai/src/api/openai-completions.ts 的 detectCompat）：
// pi 用 provider 名 + baseUrl 子串给出一组默认值，再由每个模型的 compat 字段逐项覆盖
// （getCompat：model.compat ?? detected）。我们的模型元数据已经是 providers.json 单一
// 事实源，所以反过来用：**数据里的精确条目优先**，这里只负责补「数据没有」的那部分。
//
// 不做的事（明确记录，避免反复讨论）：
//   - 不发探针请求：会消耗额度，且探针失败（Cloudflare 403 / 模型不可用）时结论不可信；
//   - 不让用户选：字段名不该成为配置项（用户不知道自己的网关内部用哪个名字）；
//   - 不同时发两个字段：严格端点会对未知字段 400。

// DetectMaxTokensField 推断输出上限字段名（"max_tokens" / "max_completion_tokens"）；
// 返回 "" 表示无信号，调用方保持既有默认（max_completion_tokens）。
//
// 层次（第一层命中即返回）：
//  1. provider 名即内置 provider（用户自建也可同名）→ 该 provider chat-completions
//     条目的字段名（要求内部一致，否则放弃 —— 不猜）；
//  2. base_url host 命中内置预设 → 同上（用户自起名字但端点是同一家：opendesign2 /
//     opencode2 / opencode3 都是这种情形）；
//  3. 模型名的 vendor 前缀即内置 provider（"deepseek/…" 或 "deepseek-v4.1-flash"）→
//     该 provider 的字段名（端点完全未知时的最后兜底）。
func DetectMaxTokensField(providerName, baseURL, model string) string {
	if f, ok := uniformChatMaxTokensField(providerName); ok {
		return f
	}
	if id, ok := ProviderIDForBaseURL(baseURL); ok {
		if f, ok := uniformChatMaxTokensField(id); ok {
			return f
		}
	}
	if v := familyVendor(model); v != "" && !strings.EqualFold(v, providerName) {
		if f, ok := uniformChatMaxTokensField(v); ok {
			return f
		}
	}
	return ""
}

// uniformChatMaxTokensField 某内置 provider 的 chat-completions 条目字段名。
// 只在该 provider 至少有一个 chat-completions 条目、且字段名**完全一致**时返回 ok
// （混用 = 数据本身没有口径，不该拿来推断）。anthropic / responses 协议的 provider
// （anthropic / xai / fireworks …）没有 chat-completions 条目 → ok=false。
func uniformChatMaxTokensField(prov string) (string, bool) {
	if prov == "" {
		return "", false
	}
	want := strings.ToLower(prov)
	field := ""
	n := 0
	for k, info := range BuiltinModels() {
		i := strings.Index(k, "\x00")
		if i <= 0 || !strings.EqualFold(k[:i], want) {
			continue
		}
		if info.Protocol != ProtocolChatCompletions {
			continue
		}
		if info.MaxTokensField == "" {
			return "", false // 有条目没标注 → 口径不全，不推断
		}
		if n > 0 && info.MaxTokensField != field {
			return "", false // 混用 → 不猜
		}
		field = info.MaxTokensField
		n++
	}
	return field, n > 0
}

// ProviderIDForBaseURL base_url → 内置预设 id（按 host 比较，忽略协议/路径/端口）。
// 用于「用户自起 provider 名，但端点是内置某家或同一聚合网关」的场景：
//   - https://opencode.ai/zen/go/v1 → opencode（opencode2 / opencode3 命中）
//   - https://amr-link.open-design.ai/v1 → opendesign（opendesign2 / opendesign3qq 命中）
//
// 只比 host 不比路径：同一网关的不同版本根（/v1、/api/paas/v4）仍是同一端点。
func ProviderIDForBaseURL(raw string) (string, bool) {
	host := hostOf(raw)
	if host == "" {
		return "", false
	}
	for i := range BuiltinProviderPresets {
		p := &BuiltinProviderPresets[i]
		if p.ID != "" && hostOf(p.BaseURL) == host {
			return p.ID, true
		}
	}
	return "", false
}

// hostOf 取 URL 的 host（小写、去端口、去尾点）；解析失败或空串返回 ""。
func hostOf(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	h := strings.ToLower(u.Hostname()) // Hostname() 已去端口
	return strings.TrimSuffix(h, ".")
}
