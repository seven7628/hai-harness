package provider

import (
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/seven7628/hai-harness/core"
)

// ModelPrice 每百万 token 单价（USD）。CacheRead 用低价（缓存命中价远低于未命中）。
// CacheWrite 为缓存写入价（Anthropic/OpenAI 长缓存 1h 有独立写入价；多数厂商无 = 0）。
// 从 desktop/bridge/metrics.go 的 ModelPrice 下沉为公共类型（bridge 保留宿主价覆盖）。
type ModelPrice struct {
	Input      float64 `json:"input"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Output     float64 `json:"output"`
}

// ModelInfo 厂商无关的模型元数据（参考 pi 的 Model 类型，按本项目裁剪）。
// 每个 Provider 的模型常量表定义在 provider/models/<id>.go（纯数据，无副作用），
// 由 BuiltinModels() 汇总进 Registry。
type ModelInfo struct {
	ID       string
	Provider string // deepseek / openai / opencode / kimi / zhipu / anthropic / 自定义

	// 窗口 / 单次输出上限（tokens）—— 真实模型数据（models.dev 2026-08），非统一 128k/8192。
	ContextWindow int64
	MaxTokens     int64

	// 输出上限字段名：chat-completions 用（OpenAI "max_completion_tokens" / DeepSeek·智谱·Kimi "max_tokens"）。
	// responses 恒 "max_output_tokens"；anthropic 恒 "max_tokens"（两者不进此字段）。
	MaxTokensField string

	// 价表（$/M tokens）。bridge 宿主价存在时以宿主价为准（Override 覆盖）。
	Cost ModelPrice

	// 推理模型标记 + 思考档位映射（统一档位 → 厂商档位；nil = 透传/厂商默认）。
	Reasoning      bool
	ThinkingLevels map[ReasoningEffortLevel]string

	// ThinkingFormat 思考参数格式（chat-completions 用；对齐 pi thinkingFormat）。
	// 空 = openai 默认（reasoning_effort）；deepseek/zai/qwen/openrouter/together 等见
	// OpenAICompatCapabilities.ThinkingFormat 注释。注册表模型数据按 pi 标注。
	ThinkingFormat string

	// RequiresReasoningContent 回传要求（对齐 pi requiresReasoningContentOnAssistantMessages）：
	// deepseek 系端点要求所有 assistant 消息都带 reasoning_content 字段（思考开启时，
	// 空串也行；缺失可能 400）。与 RetainReasoning 的区别：后者只在有推理内容时回传，
	// 此字段强制空字段也存在。
	RequiresReasoningContent bool

	// 兼容开关（对齐 pi OpenAICompletionsCompat；零值 = 默认语义）：
	SupportsDeveloperRole      *bool // 推理模型用 developer role（nil = 推理模型开）
	RequiresAssistantAfterTool *bool // tool_result 后必须跟 assistant 消息（nil = false）
	RequiresToolResultName     *bool // tool_result 消息需带 name（nil = false）
	RequiresThinkingAsText     *bool // thinking 块转 <thinking> 文本（nil = false）
	// 厂商扩展（对齐 pi OpenAICompletionsCompat）：
	ZaiToolStream     *bool             // z.ai 工具流式（tool_stream:true）
	ChatTemplateArgs  map[string]string // baseten chat_template_args（thinking 开关）
	DeferredToolsMode string            // kimi 延迟工具模式（"kimi"）

	// 思考能力（2026-08 新增，对齐 pi thinkingLevelMap + models.dev reasoning_options）：
	//   ThinkingToggleOnly：只支持开/关、无档位（glm-4.6 / kimi-k2.6 / minimax 等）——
	//     请求思考时只发 thinking 开关，不发 effort 字段。
	//   ThinkingDefaultOff：默认不开思考（gpt-4o 等非推理模型；nil/false = 默认开）。
	//   ThinkingForceOn：强制思考、不可关闭（GLM-5.3 / GLM-5.3-FLASH —— 官方文档明确
	//     thinking.type=disabled 会报错）——请求侧不发送 disabled（省略 thinking 字段 =
	//     默认开启）。
	//   ThinkingSupportedEfforts：模型支持的归一档位列表（models.dev reasoning_options 映射）；
	//     请求档位不在其中时 clamp 到最接近的有效档位（避免 400）。
	ThinkingToggleOnly       bool
	ThinkingDefaultOff       bool
	ThinkingForceOn          bool
	ThinkingSupportedEfforts []ReasoningEffortLevel

	// 输入模态（对齐 pi 的 input: ("text"|"image")[]）。"text" 恒有。
	// SupportsImage 由 BuiltinModels() 从 Inputs 派生（无需手填）。
	Inputs []string

	// 能力
	SupportsImage       bool  // 派生：Inputs 含 "image"（前端上传门控 + 翻译层降级）
	SupportsThinking    *bool // nil = 按模型自适应（Anthropic adaptive / OpenAI 自动）
	SupportsTemperature *bool // nil = true（Anthropic Opus 4.7+ 拒绝非默认 temperature 时置 false）
	SupportsStreaming   bool  // 预留：恒 true（本项目三协议均流式）；非流式端点时置 false
	// UsageInputIncludesCache usage 口径标注：该端点的 input_tokens 是否**已含**缓存
	// 命中/写入（OpenAI/DeepSeek 语义）—— nil = 未标注，由调用方按协议默认 + 响应形状判定。
	//   true  = 已含（OpenAI 形状网关：input_tokens 即 prompt_tokens，不能再加缓存）
	//   false = 未含（Anthropic 官方语义：总输入 = input_tokens + cache_read + cache_write）
	// 判定错会让上下文占用/命中率整体偏一倍（2026-09 用户实测），故给显式标注而非只靠嗅探：
	// 协议字段定默认（anthropic 协议 = 未含），端点违背协议时由用户在 Provider 面板标注。
	UsageInputIncludesCache *bool

	// CacheTTL1h Anthropic 缓存断点 TTL 标注：true = 请求 1h extended TTL（写入价 2×
	// 基础输入价），false = 5m 默认（写入价 1.25×）。nil = 未标注 → 按端点默认
	// （官方 api.anthropic.com = 1h；兼容端点 = 5m，它们只实现 Messages 基础协议、
	// ttl 是 Claude 扩展字段）。2026-09-21 用户决策：默认短缓存对本 Harness 的工具批 /
	// 后台任务 / 用户思考太短，跨轮必过期。
	CacheTTL1h *bool

	// SupportsCacheControl Anthropic system/tools 的 cache_control 缓存断点是否可用。
	// nil = 默认开启（官方 + 第三方聚合平台均支持）；false = 官方直连不支持
	//（GLM / Kimi 官方 Anthropic 端点只支持通用 Messages 协议，发送会 400）。
	SupportsCacheControl *bool

	// 协议（chat_completions / responses / anthropic）—— 注册表按协议分区
	Protocol Protocol
}

// supportsImageOf 从 Inputs 派生 SupportsImage。
func supportsImageOf(inputs []string) bool {
	for _, in := range inputs {
		if in == "image" {
			return true
		}
	}
	return false
}

// boolPtr 便捷构造 *bool（模型常量表里显式声明 SupportsThinking/SupportsTemperature 用）。
func boolPtr(b bool) *bool { return &b }

// Registry 模型元数据注册表（普通结构体，依赖注入 —— 不是包级单例）。
// 由 bridge 构造（持有 settings 数据）并经 Dependencies 注入各 provider；
// 测试可构造空 Registry 或注入 mock 数据，无全局状态。
type Registry struct {
	mu       sync.RWMutex
	builtin  map[string]ModelInfo // key = provider + "\x00" + model
	override map[string]ModelInfo
	// usageIncl provider 级 usage 口径标注（见 SetProviderUsageInputIncludesCache）：
	// 存在即生效，优先于模型级标注。端点特性属于 provider（同一网关下所有模型的计数
	// 形状一致），且用户配置在 Provider 面板 —— 模型级字段留给 providers.json 内置数据。
	usageIncl map[string]bool
	// cacheTTL provider 级缓存 TTL 标注（见 SetProviderCacheTTL1h）：同 usageIncl 的
	// 「provider 特性 + 面板可配」定位，语义见 ModelInfo.CacheTTL1h。
	cacheTTL map[string]bool
}

// NewRegistry 构造注册表（内置默认表来自 BuiltinModels() 汇总）。
// S1 修复：深拷贝 builtin map —— BuiltinModels() 返回包级共享缓存（builtinModelsMap），
// 直接赋值会让多个 Registry 共享同一 map，RegisterBuiltin 并发写即数据竞争。
func NewRegistry() *Registry {
	return &Registry{builtin: cloneModelMap(BuiltinModels())}
}

// cloneModelMap 深拷贝模型 map（S1：Registry 实例间不共享底层 map）。
func cloneModelMap(src map[string]ModelInfo) map[string]ModelInfo {
	dst := make(map[string]ModelInfo, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// SetProviderUsageInputIncludesCache 标注某 provider 的 usage 口径（nil = 清除标注/回到
// 协议默认 + 响应形状判定）。Provider 面板「usage 口径」三项（自动/已含/未含）即写这里，
// 与 settings.json 的 usage_input_includes_cache 字段一一对应。
func (r *Registry) SetProviderUsageInputIncludesCache(provider string, inclusive *bool) {
	if provider == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if inclusive == nil {
		delete(r.usageIncl, provider)
		return
	}
	if r.usageIncl == nil {
		r.usageIncl = map[string]bool{}
	}
	r.usageIncl[provider] = *inclusive
}

// SetProviderCacheTTL1h 标注某 provider 的 Anthropic 缓存 TTL（nil = 清除标注 → 回端点默认：
// 官方 1h / 兼容 5m）。Provider 面板「缓存 TTL」三项（自动/5m/1h）即写这里，与 settings.json
// 的 cache_ttl_1h 字段一一对应。
func (r *Registry) SetProviderCacheTTL1h(provider string, oneHour *bool) {
	if provider == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if oneHour == nil {
		delete(r.cacheTTL, provider)
		return
	}
	if r.cacheTTL == nil {
		r.cacheTTL = map[string]bool{}
	}
	r.cacheTTL[provider] = *oneHour
}

// CacheTTL1h 解析 (provider, model) 的缓存 TTL 标注：模型级（providers.json / 设置覆盖）
// 优先，其次 provider 级；第二返回值为 false 表示未标注（调用方按端点默认处理）。
func (r *Registry) CacheTTL1h(provider, model string) (bool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if info, ok := r.lookupLocked(provider, model); ok && info.CacheTTL1h != nil {
		return *info.CacheTTL1h, true
	}
	if v, ok := r.cacheTTL[provider]; ok {
		return v, true
	}
	return false, false
}

// UsageInputIncludesCache 解析 (provider, model) 的 usage 口径标注：
// 模型级（providers.json / 设置覆盖）优先，其次 provider 级；第二返回值为 false 表示
// 「未标注」—— 调用方此时按协议默认处理，并可参考响应自报形状（respjson 保留字段）。
func (r *Registry) UsageInputIncludesCache(provider, model string) (bool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if info, ok := r.lookupLocked(provider, model); ok && info.UsageInputIncludesCache != nil {
		return *info.UsageInputIncludesCache, true
	}
	if v, ok := r.usageIncl[provider]; ok {
		return v, true
	}
	return false, false
}

// RegisterBuiltin 注册内置表条目（幂等；一般由 NewRegistry 经 BuiltinModels 汇总，运行时很少调用）。
func (r *Registry) RegisterBuiltin(info ModelInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := info.Provider + "\x00" + info.ID
	if r.builtin == nil {
		r.builtin = map[string]ModelInfo{}
	}
	r.builtin[key] = info
}

// Lookup 查模型元数据：override 优先（用户显式配置），其次内置，最后零值（走调用方默认）。
// provider 为空时按模型名全局查找（找第一个匹配）—— 兼容旧调用（StreamRequest.Provider 未填充）。
func (r *Registry) Lookup(provider, model string) (ModelInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lookupLocked(provider, model)
}

// lookupLocked Lookup 的持锁实现：查价路径（PriceFor）在同一次读锁内复用 ——
// sync.RWMutex 不可重入，嵌套 RLock 在有写者排队时会死锁，故查价只取一次锁。
func (r *Registry) lookupLocked(provider, model string) (ModelInfo, bool) {
	m, _, ok := r.lookupWithSourceLocked(provider, model)
	return m, ok
}

// lookupWithSourceLocked 与 lookupLocked 同口径（含 provider=="" 的全局分支及其
// 字母序确定性），额外报告命中来自哪张表（CostSourceOverride / CostSourceBuiltin）
// —— 价表来源审计用（Cost.Source）。
func (r *Registry) lookupWithSourceLocked(provider, model string) (ModelInfo, string, bool) {
	key := provider + "\x00" + model
	if m, ok := r.override[key]; ok {
		return m, core.CostSourceOverride, true
	}
	if m, ok := r.builtin[key]; ok {
		return m, core.CostSourceBuiltin, true
	}
	if provider == "" {
		// 全局查找：任意 provider 下该模型（旧调用兼容）。
		// map 遍历无序 → 排序取第一个，保证确定性（deepseek < opencode 字母序 → deepseek 优先）。
		prefix := "\x00" + model
		var bestKey, bestSource string
		var best ModelInfo
		found := false
		consider := func(m map[string]ModelInfo, source string) {
			for k, info := range m {
				if len(k) > len(prefix) && k[len(k)-len(prefix):] == prefix {
					if !found || k < bestKey {
						bestKey, best, bestSource, found = k, info, source, true
					}
				}
			}
		}
		consider(r.override, core.CostSourceOverride)
		consider(r.builtin, core.CostSourceBuiltin)
		if found {
			return best, bestSource, true
		}
	}
	return ModelInfo{}, "", false
}

// RegistryLookupBuiltin 查某 provider 的内置模型元数据（不含 override；bridge 默认表
// 派生用——只要 JSON 内置真实数据，不要被用户 settings 覆盖影响）。
func RegistryLookupBuiltin(provider, model string) (ModelInfo, bool) {
	m := BuiltinModels()
	info, ok := m[provider+"\x00"+model]
	return info, ok
}

// Override 注入/覆盖（bridge 启动 + set_provider 热更新调用；幂等；用户配置优先）。
func (r *Registry) Override(info ModelInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := info.Provider + "\x00" + info.ID
	if r.override == nil {
		r.override = map[string]ModelInfo{}
	}
	r.override[key] = info
}

// --- 现有硬编码逻辑迁移为 Registry 方法（依赖注入：方法挂在 Registry 上，无全局状态） ---

// ShouldRetainReasoning 推理回传决策：显式配置优先 → 注册表 Reasoning → 模型名兜底。
func (r *Registry) ShouldRetainReasoning(provider, model string, cfg *RequestConfig) bool {
	if cfg != nil && cfg.RetainReasoning != nil {
		return *cfg.RetainReasoning
	}
	if info, ok := r.Lookup(provider, model); ok {
		return info.Reasoning
	}
	return autoRetainReasoning(model) // 现有字符串匹配兜底（行为兼容）
}

// MapEffort 统一档位 → 厂商档位。分层（自上而下，第一层命中即返回）：
//  1. 本 provider 声明的 ThinkingLevels（显式映射，如 deepseek xhigh→max）；
//  2. 按**模型名**跨 provider 的 ThinkingLevels（自建 provider / 网关别名：自定义 provider
//     下的 "gpt-5.6-luna" 沿用 openai 系档位表，不因 provider 名不同而降档）；
//  3. 按**名称族**继承同族内置声明（familyInfo：网关新版本号 / 别名，如
//     opencode2 + "deepseek-v4.1-flash" 继承 deepseek 系 flash 档位表 —— 客户端选 Max
//     不再被钳成 high）；
//  4. 声明的 ThinkingSupportedEfforts（models.dev reasoning_options）钳最近档；
//  5. 无任何档位数据 → 经典集合兜底（clampCompatEffort）+ deepseek 名族表（mapEffort）。
func (r *Registry) MapEffort(provider, model string, e ReasoningEffortLevel) ReasoningEffortLevel {
	info, declared := r.effortInfo(provider, model)
	if declared && len(info.ThinkingLevels) > 0 {
		if v, ok := info.ThinkingLevels[e]; ok {
			return ReasoningEffortLevel(v)
		}
	}
	if declared && len(info.ThinkingSupportedEfforts) > 0 {
		return clampEffort(e, info.ThinkingSupportedEfforts)
	}
	return clampCompatEffort(mapEffort(model, e))
}

// effortInfo 取档位声明：本 provider 优先，其次按模型名跨 provider，最后名称族兜底。
func (r *Registry) effortInfo(provider, model string) (ModelInfo, bool) {
	return r.Resolve(provider, model)
}

// Resolve 思考路径的模型元数据解析（自上而下，第一级命中即返回）：
//  1. 本 provider 的 override，**且确实声明了思考能力**（宿主显式配置的声明优先）；
//  2. 本 provider 的内置条目（权威：即使声明为空也按它的语义走，经典集合钳制）；
//  3. vendor 前缀直指内置 provider（网关别名 "xiaomi/mimo-v2.6-pro" → 内置 xiaomi 条目）；
//  4. 内置表按模型名跨 provider（忽略 override —— 宿主为自建模型合成的基底思考声明为空，
//     看它没有意义，还会挡住真正的同名内置条目）；
//  5. 名称族兜底（familyInfo：网关新版本号 / 别名，如 deepseek-v4.1-flash）。
//
// 为什么 override 要「有声明才算命中」（2026-09 用户实测二次）：bridge 的
// overrideRegistryLocked 会为**内置表没有的模型**（用户自建 provider 手加 / 网关别名）
// 合成一份 override（窗口/上限/价表/推理标记），思考声明是零值 —— 若把它当作权威命中，
// MapEffort 会落在经典集合兜底：客户端选 Max 实际发 high（opencode2 + deepseek-v4.1-flash
// 实测，请求日志 effort=high 与选择不符），族兜底永远走不到。
//
// 为什么第 3 步必须在第 4 步之前（2026-09，mimo-v2.6 对齐）：聚合网关（command-code /
// api-codewith）把上游模型以 "<vendor>/<model>" 暴露，代理的是**上游协议**；而 openrouter
// 的 id 恰好也是 "xiaomi/mimo-v2.6-pro"，先查全局就命中 openrouter 条目的平台形态
// （reasoning:{effort}）→ 网关收到不认识的字段（上游要 thinking:{type}）。与价表兜底 ②
// 「vendor 前缀直指内置 provider，上游原名优先」同一口径。
//
// 只用于**思考/档位**判定（ResolveThinking / MapEffort / thinkingFormat 分支）——
// 窗口、价表、模态仍走各自的精确查询，避免这里改写成上下文预算与成本。
func (r *Registry) Resolve(provider, model string) (ModelInfo, bool) {
	r.mu.RLock()
	key := provider + "\x00" + model
	if info, ok := r.override[key]; ok && hasThinkingDecl(info) {
		r.mu.RUnlock()
		return info, true
	}
	if info, ok := r.builtin[key]; ok {
		r.mu.RUnlock()
		return info, true
	}
	if info, ok := r.vendorEntryLocked(model, true); ok {
		r.mu.RUnlock()
		return info, true
	}
	inherited, ok := r.builtinGlobalLocked(model)
	r.mu.RUnlock()
	if ok && hasThinkingDecl(inherited) {
		return inherited, true
	}
	return r.familyInfo(model)
}

// vendorEntryLocked 网关别名的 vendor 前缀解析（调用方持读锁）：
// "xiaomi/mimo-v2.6-pro" → 内置 provider "xiaomi" 下的 "mimo-v2.6-pro" 条目。
// 大小写不敏感（网关常改大小写：Kimi-K3 / MiMo-V2.6-Pro）；多命中取键序最小者，
// 与 builtinGlobalLocked 同口径（map 遍历无序 → 保证确定性）。
// requireThinking=true 时只认**有思考声明**的条目（思考路径：无声明等于没数据）。
func (r *Registry) vendorEntryLocked(model string, requireThinking bool) (ModelInfo, bool) {
	i := strings.Index(model, "/")
	if i <= 0 || i+1 >= len(model) {
		return ModelInfo{}, false
	}
	vendor, rest := model[:i], model[i+1:]
	var bestKey string
	var best ModelInfo
	found := false
	for k, info := range r.builtin {
		j := strings.Index(k, "\x00")
		if j <= 0 {
			continue
		}
		if !strings.EqualFold(k[:j], vendor) || !strings.EqualFold(k[j+1:], rest) {
			continue
		}
		if requireThinking && !hasThinkingDecl(info) {
			continue
		}
		if !found || k < bestKey {
			bestKey, best, found = k, info, true
		}
	}
	return best, found
}

// LookupAlias 网关别名的元数据兜底（窗口/上限/模态/价）：精确命中 → vendor 前缀直指
// 内置 provider（"xiaomi/mimo-v2.6-pro" → 内置 xiaomi 条目）→ 内置表全局按名
// （大小写不敏感）。与思考路径 Resolve、价表兜底 priceFallbackLocked 同一口径 ——
// 聚合网关（command-code / api-codewith）把上游模型换名暴露，精确查必然 miss。
//
// **只用于宿主展示/预填**（bridge modelCapabilities 的「拉取模型列表」）：请求侧语义
// 仍走精确查询（ContextWindowFor / MaxTokensDefaultFor / InputsFor）+ 各自兜底，
// 避免一次拉取就把网关模型的上下文预算改写成上游口径。
func (r *Registry) LookupAlias(provider, model string) (ModelInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if info, ok := r.lookupLocked(provider, model); ok {
		return info, true
	}
	if info, ok := r.vendorEntryLocked(model, false); ok {
		return info, true
	}
	return r.builtinGlobalFoldLocked(model)
}

// builtinGlobalFoldLocked 内置表全局按名查找（大小写不敏感；调用方持读锁）。
// 与 builtinGlobalLocked 的区别：容忍网关改大小写（"Kimi-K3" → kimi-k3）；
// 多命中取键序最小者保证确定性。
func (r *Registry) builtinGlobalFoldLocked(model string) (ModelInfo, bool) {
	if model == "" {
		return ModelInfo{}, false
	}
	var bestKey string
	var best ModelInfo
	found := false
	for k, info := range r.builtin {
		i := strings.Index(k, "\x00")
		if i <= 0 {
			continue
		}
		if !strings.EqualFold(k[i+1:], model) {
			continue
		}
		if !found || k < bestKey {
			bestKey, best, found = k, info, true
		}
	}
	return best, found
}

// hasThinkingDecl 该条目是否**声明了**思考能力（格式/档位表/支持档位/开关类标记任一）。
// false = 无声明：内置表未标注的模型，或宿主为自建模型合成的基底（思考能力未知）。
func hasThinkingDecl(info ModelInfo) bool {
	return info.ThinkingFormat != "" || len(info.ThinkingLevels) > 0 ||
		len(info.ThinkingSupportedEfforts) > 0 || info.ThinkingToggleOnly ||
		info.ThinkingDefaultOff || info.ThinkingForceOn
}

// builtinGlobalLocked 内置表全局按名查找（**不含 override**；调用方持读锁）。
// map 遍历无序 → 排序取第一个，保证确定性（与 lookupLocked 的全局分支同口径）。
func (r *Registry) builtinGlobalLocked(model string) (ModelInfo, bool) {
	prefix := "\x00" + model
	var bestKey string
	var best ModelInfo
	found := false
	for k, info := range r.builtin {
		if len(k) > len(prefix) && k[len(k)-len(prefix):] == prefix {
			if !found || k < bestKey {
				bestKey, best, found = k, info, true
			}
		}
	}
	return best, found
}

// familyInfo 名称族兜底：内置表完全没有该模型时，按「vendor 前缀 + 去版本号」的族键，
// 在同族内置 provider 的条目里找**唯一且声明一致**的同族条目，继承其思考能力与档位表。
//
// 为什么需要（2026-09 用户实测）：opencode2 + "deepseek-v4.1-flash"（网关把上游以新版本号
// 暴露）不在内置表里 → MapEffort 落到经典集合兜底 → 客户端选 Max 实际发 high，用户看到
// 的请求日志与选择不符。同族 deepseek flash 在内置表里有明确档位 {low,high,max}，
// 档位能力不该因为版本号变化就凭空降级。族表口径与 mapEffort /
// RequiresReasoningContentByName 一致（同一批「内置表没有元数据」的真实场景）。
//
// 保守约束（防误伤）：
//  1. 只认「模型名前缀 = 内置 provider id」的族（deepseek/openai/opencode/…）——
//     前缀不是 provider id（如 glm / mimo / llama）一律不兜底；
//  2. 族键必须**唯一命中且思考声明一致**（同族多条目声明冲突 → 放弃兜底，回落经典集合钳制）。
//     因此 glm-4.6（toggle-only）与 glm-5.3（有档位）这类同族异构不会互相污染。
func (r *Registry) familyInfo(model string) (ModelInfo, bool) {
	vendor, key := familyVendor(model), normalizeFamilyKey(model)
	if vendor == "" || key == "" || vendor == key {
		return ModelInfo{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var (
		found    ModelInfo
		n        int
		conflict bool
	)
	for k, info := range r.builtin {
		prov, id, ok := strings.Cut(k, "\x00")
		if !ok || prov != vendor || normalizeFamilyKey(id) != key {
			continue
		}
		if n == 0 {
			found = info
		} else if !sameThinkingDecl(found, info) {
			conflict = true
		}
		n++
	}
	if n == 0 || conflict {
		return ModelInfo{}, false
	}
	return found, true
}

// familyVersionRe 版本号片段（v4 / 4.1 / 0731 …）：族键归一化时剔除。
var familyVersionRe = regexp.MustCompile(`v?\d+(?:\.\d+)*`)

// normalizeFamilyKey 模型名 → 族键：去 vendor 路径前缀（"deepseek/deepseek-v4.1-flash"）+
// 去版本号 + 折叠分隔符。例：deepseek-v4.1-flash 与 deepseek-v4-flash 同为 "deepseek-flash"。
func normalizeFamilyKey(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	m = familyVersionRe.ReplaceAllString(m, "")
	for strings.Contains(m, "--") {
		m = strings.ReplaceAll(m, "--", "-")
	}
	return strings.Trim(m, "-_. ")
}

// familyVendor 模型名的族前缀：去 vendor 路径后首个分隔符之前的一段（"deepseek-v4.1-flash" →
// "deepseek"）。是否真是内置 provider 由 familyInfo 的查表决定（查不到即不兜底）。
func familyVendor(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if i := strings.IndexAny(m, "-_. "); i > 0 {
		return m[:i]
	}
	return ""
}

// sameThinkingDecl 两份元数据的「思考声明」是否等价（familyInfo 的冲突判定用）。
func sameThinkingDecl(a, b ModelInfo) bool {
	if a.Reasoning != b.Reasoning || a.ThinkingToggleOnly != b.ThinkingToggleOnly ||
		a.ThinkingDefaultOff != b.ThinkingDefaultOff || a.ThinkingForceOn != b.ThinkingForceOn ||
		a.ThinkingFormat != b.ThinkingFormat || len(a.ThinkingLevels) != len(b.ThinkingLevels) ||
		len(a.ThinkingSupportedEfforts) != len(b.ThinkingSupportedEfforts) {
		return false
	}
	for k, v := range a.ThinkingLevels {
		if b.ThinkingLevels[k] != v {
			return false
		}
	}
	for i, v := range a.ThinkingSupportedEfforts {
		if b.ThinkingSupportedEfforts[i] != v {
			return false
		}
	}
	return true
}

// SupportsImageInput 模型是否支持图片输入（前端上传按钮门控 + 翻译层降级判定）。
func (r *Registry) SupportsImageInput(provider, model string) bool {
	if info, ok := r.Lookup(provider, model); ok {
		return info.SupportsImage
	}
	// 未知模型：默认不支持（保守 —— 避免配错 400；前端显示无上传按钮）
	return false
}

// RequiresReasoningContent 是否要求 assistant 消息恒带 reasoning_content 字段
// （对齐 pi requiresReasoningContentOnAssistantMessages：deepseek 系端点思考开启时缺失 400）。
// 内置/override 无该 (provider, model) 元数据时按模型名族兜底（deepseek 族）——
// 网关别名模型（"deepseek/..." 且内置表无该 provider 条目）此前恒为 false →
// 空推理轮不带字段 → 上游 400 实证（见 RequiresReasoningContentByName）。
func (r *Registry) RequiresReasoningContent(provider, model string) bool {
	info, ok := r.Lookup(provider, model)
	if !ok {
		return RequiresReasoningContentByName(model)
	}
	return info.RequiresReasoningContent && info.Reasoning
}

// SupportsDeveloperRole 推理模型是否用 developer role（对齐 pi：supportsDeveloperRole =
// !isNonStandard && !isOpenRouter —— 仅 OpenAI 官方端点（openai.com）推理模型用 developer；
// deepseek/zai/kimi/together/nvidia/xai/cerebras 等非标准端点恒 system）。
func (r *Registry) SupportsDeveloperRole(provider, model string) bool {
	info, ok := r.Lookup(provider, model)
	if !ok {
		return false
	}
	if info.SupportsDeveloperRole != nil {
		return *info.SupportsDeveloperRole
	}
	// 默认：仅 openai 官方端点（pi isNonStandard 排除 deepseek/zai/kimi/together/nvidia/xai/cerebras/opencode 等）
	switch provider {
	case "openai", "openai-codex":
		return info.Reasoning
	default:
		return false
	}
}

// RequiresAssistantAfterToolResult tool_result 后是否必须跟 assistant 消息
// （对齐 pi requiresAssistantAfterToolResult；默认 false）。
func (r *Registry) RequiresAssistantAfterToolResult(provider, model string) bool {
	info, ok := r.Lookup(provider, model)
	if !ok {
		return false
	}
	return info.RequiresAssistantAfterTool != nil && *info.RequiresAssistantAfterTool
}

// RequiresToolResultName tool_result 消息是否需带 name 字段（对齐 pi；默认 false）。
func (r *Registry) RequiresToolResultName(provider, model string) bool {
	info, ok := r.Lookup(provider, model)
	if !ok {
		return false
	}
	return info.RequiresToolResultName != nil && *info.RequiresToolResultName
}

// RequiresThinkingAsText thinking 块是否需转 <thinking> 文本（对齐 pi；默认 false）。
func (r *Registry) RequiresThinkingAsText(provider, model string) bool {
	info, ok := r.Lookup(provider, model)
	if !ok {
		return false
	}
	return info.RequiresThinkingAsText != nil && *info.RequiresThinkingAsText
}

// ZaiToolStream z.ai 端点工具流式（对齐 pi zaiToolStream；默认 false）。
func (r *Registry) ZaiToolStream(provider, model string) bool {
	info, ok := r.Lookup(provider, model)
	if !ok {
		return false
	}
	return info.ZaiToolStream != nil && *info.ZaiToolStream
}

// ChatTemplateArgs baseten 端点的 chat_template_args（对齐 pi；nil = 无）。
func (r *Registry) ChatTemplateArgs(provider, model string) map[string]string {
	info, ok := r.Lookup(provider, model)
	if !ok {
		return nil
	}
	return info.ChatTemplateArgs
}

// DeferredToolsMode kimi 延迟工具模式（对齐 pi deferredToolsMode；"" = 不启用）。
func (r *Registry) DeferredToolsMode(provider, model string) string {
	info, ok := r.Lookup(provider, model)
	if !ok {
		return ""
	}
	return info.DeferredToolsMode
}

// MaxTokensFieldFor 输出上限字段名（chat-completions 用）。
func (r *Registry) MaxTokensFieldFor(provider, model string) string {
	if info, ok := r.Lookup(provider, model); ok && info.MaxTokensField != "" {
		return info.MaxTokensField
	}
	return "max_completion_tokens" // 默认（现状行为）
}

// MaxTokensDefaultFor 未显式指定时的默认输出上限（真实模型数据，非 8192 兜底）。
func (r *Registry) MaxTokensDefaultFor(provider, model string) int64 {
	if info, ok := r.Lookup(provider, model); ok && info.MaxTokens > 0 {
		return info.MaxTokens
	}
	return 8192 // 未知模型兜底（对齐 bridge defaultMaxTokens）
}

// ContextWindowFor 上下文窗口（压缩器 / 前端展示用；0 = 未知走调用方默认）。
func (r *Registry) ContextWindowFor(provider, model string) int64 {
	if info, ok := r.Lookup(provider, model); ok {
		return info.ContextWindow
	}
	return 0
}

// PriceFor 价表（$/M tokens；宿主 bridge priceTable 覆盖后返回覆盖值）。
// 精确 (provider, model) 无价（未收录，或条目价全 0）时走归一兜底
// （priceFallbackLocked）：聚合型网关把上游模型以别名暴露，精确查必然 miss ——
// 价表条目里 opencode 的 id 是 "deepseek-v4-flash"，网关却按 "deepseek/deepseek-v4-flash"
// 暴露；provider 名也会带序号（"opencode2" 与 "opencode" 同端点）。
// 兜底只解析价格，不改动 Lookup 的窗口/上限/模态语义，且仅在精确路径无价时启用
// —— 效果只能是「0 → 非 0」，不会改写任何已有价格。
func (r *Registry) PriceFor(provider, model string) (ModelPrice, bool) {
	p, _, ok := r.PriceForWithSource(provider, model)
	return p, ok
}

// PriceForWithSource 与 PriceFor 同口径（含归一兜底），额外返回价表来源（core.Cost.Source）：
//   - CostSourceBuiltin / CostSourceOverride：命中**可用**价（Priced=true）
//   - CostSourceExplicitZero：精确条目存在但价全 0，且归一兜底也没找到可用价
//     （内置数据里 68 个模型如此 —— 语义是「价源未收录/未知价」，**不是免费**）
//   - CostSourceNone：表里没有这个 (provider, model)
//
// 后两种 ok=false（不计费）。存在的理由：把「无价」与「免费」分开 —— 两者此前都是
// Cost=0，成本静默少算且报表看不出来（2026-09 实测仅 33.4% 调用有价）。
func (r *Registry) PriceForWithSource(provider, model string) (ModelPrice, string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, source, entryExists := r.lookupWithSourceLocked(provider, model)
	if entryExists {
		if p, usable := priceOf(info); usable {
			return p, source, true
		}
	}
	// 精确条目无可用价（不存在 / 价全 0）→ 仍走归一兜底（与 PriceFor 旧行为一致：
	// 兜底只能把 0 变成非 0，不改写任何已有价）。
	if p, fsource, ok := r.priceFallbackWithSourceLocked(provider, model); ok {
		return p, fsource, true
	}
	if entryExists {
		return ModelPrice{}, core.CostSourceExplicitZero, false
	}
	return ModelPrice{}, core.CostSourceNone, false
}

// priceOf 条目是否带可用价（Input/Output 至少一个有值 —— 全 0 = 无价表，不计费）。
func priceOf(info ModelInfo) (ModelPrice, bool) {
	return info.Cost, info.Cost.Input > 0 || info.Cost.Output > 0
}

// FillUsageCost 按注册表价表把单轮用量折算为实际支出金额，填入 u.Cost（µUSD int64）。
// 设计：LLMEndEvent.Usage 从 provider 吐出即归一（Input 总输入口径 + Cost 金额），
// 下游（agent/session/subagent/前端）零协议特判、零再计算。
//
// 单位换算：价表 p.Input 等为「美元 / 百万 token」。
//
//	金额(USD) = tokens × 单价 / 1e6；µUSD = 金额 × 1e6 = tokens × 单价（直接相乘）。
//
// 一次调用通常 <1 美分，µUSD 整数可精确表达且便于 int64 累计（addUsage → Session
// 持久化 → MaxCost 熔断）。
//
// 分类：Input = 未命中输入（CacheMiss()）按全价；CacheRead 按命中低价；CacheWrite 按写价
// （多数厂商 0）；Output 按输出价。Total = 四段之和（自洽，与 costUsd 的 float 结果一致，
// µUSD 取整）。Reasoning 是 Output 的拆分视图（同价折出），**不参与求和**。
// 无价表（未知 provider/模型，或价全 0）→ Cost 保持零值（不计费，不阻塞），但**留下
// Priced=false + Source**（explicit_zero / none）—— 「不知道花了多少」与「免费」不再同形，
// 报表可统计无价调用占比（2026-09-23 起）。
// 查价先精确、再归一兜底（见 PriceForWithSource）—— 网关别名模型（"deepseek/…"、provider
// 名带序号）因此同样计入成本；兜底命中不了才零值。
func (r *Registry) FillUsageCost(provider, model string, u *core.Usage) {
	if u == nil {
		return
	}
	p, source, ok := r.PriceForWithSource(provider, model)
	if !ok {
		u.Cost = core.Cost{Priced: false, Source: source}
		return
	}
	u.Cost = core.Cost{
		Input:     int64(float64(u.CacheMiss()) * p.Input),
		CacheRead: int64(float64(u.CacheRead) * p.CacheRead),
		// CacheWrite 含 1h 档（价表只有一个写入价；1h 的 2× 修正在知道 TTL 的 provider 层做）
		CacheWrite: int64(float64(u.CacheWrite) * p.CacheWrite),
		Output:     int64(float64(u.Output) * p.Output),
		// Reasoning 是 Output 的**拆分视图**（同一输出价）：回答「思考花了多少钱」，
		// 不参与 Total 求和（否则双计）。上游不报 reasoning 时恒 0（如部分网关）。
		Reasoning: int64(float64(u.Reasoning) * p.Output),
		Priced:    true,
		Source:    source,
	}
	u.Cost.Total = u.Cost.Input + u.Cost.CacheRead + u.Cost.CacheWrite + u.Cost.Output
}

// ── 价表归一兜底 ─────────────────────────────────────────────────────────────
// 背景（实测 2026-09-21）：metrics.jsonl 96,738 次调用只有 33.4% 有价，缺失几乎全在
// 聚合型网关 —— command-code（71 个上游模型以 "deepseek/…" 形式暴露）/ api-codewith /
// opencode2（同端点换名）。精确 (provider, model) 查价对这些形态天生 miss，而
// providers.json 里其实**已经有**对应价（openai/anthropic/deepseek/openrouter/opencode…）。
// 归一命中率实测：command-code 71 个模型可救 59 个、api-codewith 39 个可救 26 个。
//
// 归一候选与优先级（前命中即返回；全部 miss 才维持零值）：
// ① 同 provider（含 provider 名去序号：opencode2 → opencode）+ 模型名归一；
// ② vendor 前缀直指内置 provider（"deepseek/deepseek-v4-flash" → (deepseek,
// deepseek-v4-flash)）—— 优先上游原名而非转售商价：同模型在不同转售商价不同
// （openrouter 0.08092 / deepseek 官方 0.14 / opencode 0.22），上游名更可解释；
// ③ 全局按归一模型名：override 优先（用户显式配置），再内置；键字母序最小者（确定性）。
//
// 不做的事（宁可无价也不虚增成本）：前缀/子串模糊匹配（"deepseek-v4-flash-fast" 不认成
// "deepseek-v4-flash"）；":free"/":nitro" 等变体族（真实价与基础模型不同，:free 恒 0）。
func (r *Registry) priceFallbackLocked(provider, model string) (ModelPrice, bool) {
	p, _, ok := r.priceFallbackWithSourceLocked(provider, model)
	return p, ok
}

// priceFallbackWithSourceLocked 与 priceFallbackLocked 同口径（同 provider 归一 →
// vendor 前缀 → 全局归一），额外报告命中条目的价表来源。
func (r *Registry) priceFallbackWithSourceLocked(provider, model string) (ModelPrice, string, bool) {
	variants := modelVariants(model)
	if len(variants) == 0 {
		return ModelPrice{}, core.CostSourceNone, false
	}
	// ① 同 provider（provider 名别名经 providerVariants 展开）
	for _, p := range providerVariants(provider) {
		for _, pid := range r.providerIDsLocked(p) {
			for _, m := range variants {
				if price, source, ok := r.priceAtWithSourceLocked(pid, m); ok {
					return price, source, true
				}
			}
		}
	}
	// ② vendor 前缀提示（"<vendor>/<rest>"）
	if i := strings.LastIndex(model, "/"); i > 0 {
		vendor, rest := strings.TrimSpace(model[:i]), model[i+1:]
		for _, pid := range r.providerIDsLocked(vendor) {
			for _, m := range modelVariants(rest) {
				if price, source, ok := r.priceAtWithSourceLocked(pid, m); ok {
					return price, source, true
				}
			}
		}
	}
	// ③ 全局按归一模型名
	return r.priceGlobalWithSourceLocked(variants)
}

// priceAtLocked 取某 (provider, model) 的可用价（无条目 / 价全 0 → false）。
func (r *Registry) priceAtLocked(provider, model string) (ModelPrice, bool) {
	p, _, ok := r.priceAtWithSourceLocked(provider, model)
	return p, ok
}

// priceAtWithSourceLocked 与 priceAtLocked 同口径，额外报告来源（价全 0 → false）。
func (r *Registry) priceAtWithSourceLocked(provider, model string) (ModelPrice, string, bool) {
	info, source, ok := r.lookupWithSourceLocked(provider, model)
	if !ok {
		return ModelPrice{}, core.CostSourceNone, false
	}
	p, usable := priceOf(info)
	if !usable {
		return ModelPrice{}, core.CostSourceExplicitZero, false
	}
	return p, source, true
}

// priceGlobalLocked 全局按归一模型名查价：override（用户/宿主配置）优先，其次内置；
// 同一 map 内多 provider 命中 → 键字母序最小者（与 Lookup 的全局分支同口径，确定性）。
func (r *Registry) priceGlobalLocked(variants []string) (ModelPrice, bool) {
	p, _, ok := r.priceGlobalWithSourceLocked(variants)
	return p, ok
}

// priceGlobalWithSourceLocked 与 priceGlobalLocked 同口径，额外报告来源。
func (r *Registry) priceGlobalWithSourceLocked(variants []string) (ModelPrice, string, bool) {
	best := func(m map[string]ModelInfo, source string) (ModelPrice, string, bool) {
		var bestKey string
		var bestPrice ModelPrice
		found := false
		for k, info := range m {
			i := strings.Index(k, "\x00")
			if i < 0 {
				continue
			}
			mid := k[i+1:]
			match := false
			for _, v := range variants {
				if strings.EqualFold(mid, v) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
			price, ok := priceOf(info)
			if !ok {
				continue
			}
			if !found || k < bestKey {
				bestKey, bestPrice, found = k, price, true
			}
		}
		if !found {
			return ModelPrice{}, "", false
		}
		return bestPrice, source, true
	}
	if p, source, ok := best(r.override, core.CostSourceOverride); ok {
		return p, source, true
	}
	if p, source, ok := best(r.builtin, core.CostSourceBuiltin); ok {
		return p, source, true
	}
	return ModelPrice{}, core.CostSourceNone, false
}

// providerIDsLocked 把 provider 名解析为注册表里的真实 provider id（大小写不敏感）。
// 未收录的 provider 名（command-code / api-codewith 等自定义网关）→ nil，该阶段跳过。
// 扫的是 map 键前缀：provider 维度只有几十个，且查价每轮一次，无需额外索引。
func (r *Registry) providerIDsLocked(name string) []string {
	if name == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	collect := func(m map[string]ModelInfo) {
		for k := range m {
			i := strings.Index(k, "\x00")
			if i <= 0 {
				continue
			}
			pid := k[:i]
			if strings.EqualFold(pid, name) && !seen[pid] {
				seen[pid] = true
				out = append(out, pid)
			}
		}
	}
	collect(r.builtin)
	collect(r.override)
	sort.Strings(out) // map 遍历无序 → 排序保证确定性
	return out
}

// providerVariants provider 名候选（优先级序）：原名 → 去尾部别名序号。
// 观测到的真实用法：同一端点配多套配置（opencode / opencode2、command-code-2/-3）——
// "opencode2" 去掉尾号即回到内置 provider "opencode"（价表命中）。
func providerVariants(provider string) []string {
	p := strings.TrimSpace(provider)
	if p == "" {
		return nil
	}
	out := []string{p}
	trimmed := strings.TrimRight(strings.TrimRight(p, "0123456789"), "-_ ")
	if trimmed != "" && !strings.EqualFold(trimmed, p) {
		out = append(out, trimmed)
	}
	return out
}

// modelVariants 模型名归一候选（优先级序）：原名 → 去 vendor 前缀 → 去 "[…]" 变体后缀。
// 返回 nil = 该名字属于「变体定价」族，不做兜底：
//   - ":free" / ":nitro" 等（最后一段含 ':'）真实价与基础模型不同（:free 恒 0），
//     兜底到基础模型价会**虚增成本**（注册表里 minimax/minimax-m2.7:free 价为 0，
//     若归一成 minimax-m2.7 会按 0.3/1.2 计费）。
//
// 去 "[…]" 是近似：api-codewith 把 1M 上下文变体暴露为 "claude-opus-4-6[1m]"，
// 这里按同价档假设（注册表 ModelPrice 是扁平四元组，本就无法表达分层定价）。
func modelVariants(model string) []string {
	m := strings.TrimSpace(model)
	if m == "" {
		return nil
	}
	last := m
	if i := strings.LastIndex(last, "/"); i >= 0 {
		last = last[i+1:]
	}
	if strings.Contains(last, ":") {
		return nil
	}
	base := []string{m}
	if s := stripVendorPrefix(m); s != "" {
		base = append(base, s)
	}
	var out []string
	seen := map[string]bool{}
	add := func(v string) {
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	for _, v := range base {
		add(v)
	}
	for _, v := range base {
		add(stripBracketSuffix(v))
	}
	return out
}

// stripVendorPrefix 去 vendor 前缀（"deepseek/deepseek-v4-flash" → "deepseek-v4-flash"）。
// 无 "/" → 空串（调用方跳过该候选）。
func stripVendorPrefix(model string) string {
	if i := strings.LastIndex(model, "/"); i >= 0 {
		return strings.TrimSpace(model[i+1:])
	}
	return ""
}

// stripBracketSuffix 去 "[…]" 变体后缀（"claude-opus-4-6[1m]" → "claude-opus-4-6"）。
// 非该形态 → 空串（调用方跳过该候选）。
func stripBracketSuffix(model string) string {
	if !strings.HasSuffix(model, "]") {
		return ""
	}
	if i := strings.LastIndex(model, "["); i > 0 {
		return strings.TrimSpace(model[:i])
	}
	return ""
}

// SupportsAdaptiveThinking 是否用 adaptive thinking（Anthropic 新模型；注册表 SupportsThinking 派生）。
// nil/true + 模型 reasoning → adaptive；显式 false → budget-based。
func (r *Registry) SupportsAdaptiveThinking(provider, model string) bool {
	info, ok := r.Lookup(provider, model)
	if !ok {
		return false // 未知模型走 budget-based（保守）
	}
	if info.SupportsThinking != nil {
		return *info.SupportsThinking && info.Reasoning
	}
	return false // 默认 budget-based（首期行为保守；Anthropic 新模型在注册表显式置 true）
}

// SupportsTemperature 模型是否接受 temperature 字段（Anthropic Opus 4.7+ 拒绝非默认值）。
// 默认 true；注册表可显式置 false。
func (r *Registry) SupportsTemperature(provider, model string) bool {
	info, ok := r.Lookup(provider, model)
	if !ok {
		return true
	}
	if info.SupportsTemperature != nil {
		return *info.SupportsTemperature
	}
	return true
}

// ThinkingForceOn 模型是否强制思考不可关闭（GLM-5.3/5.3-FLASH：官方明确
// thinking.type=disabled 会报错）。请求侧对这类模型不发 disabled（省略 = 默认开）。
func (r *Registry) ThinkingForceOn(provider, model string) bool {
	info, ok := r.Lookup(provider, model)
	if !ok {
		return false
	}
	return info.ThinkingForceOn
}

// SupportsCacheControl 模型/端点的 Anthropic cache_control 缓存断点是否可用。
// nil = 默认开（官方 + 第三方聚合平台）；显式 false = 官方直连不支持
// （GLM / Kimi —— 只支持通用 Messages 协议，发送 cache_control 会 400）。
// 未知模型（自定义 provider）默认也开 —— 第三方聚合平台（new-api 类）普遍接受，
// 用户可按 provider 在 settings 里显式关（SupportsCacheControl=false）兜底。
func (r *Registry) SupportsCacheControl(provider, model string) bool {
	info, ok := r.Lookup(provider, model)
	if !ok {
		return true
	}
	if info.SupportsCacheControl == nil {
		return true
	}
	return *info.SupportsCacheControl
}

// ThinkingDecision 思考解析结果：是否开思考 + 归一档位（已 clamp 到模型支持范围）。
type ThinkingDecision struct {
	Enabled bool
	Effort  ReasoningEffortLevel
	// ToggleOnly 模型只支持开/关、无档位（glm-4.6/kimi/小米 MiMo 等）——
	// 请求时只发 thinking 开关，不发 effort 字段。**模型属性，开关两侧都置位**：
	// 关思考时也据此不发档位（deepseek/zai 形态用 thinking.type=disabled 表达关闭）。
	ToggleOnly bool
	// ThinkingUnsupported 模型本身没有思考能力（ThinkingDefaultOff 且调用方未显式开思考）：
	// 思考参数无意义 —— 请求侧**什么都不发**（既不发开关也不发档位）。区分「显式关思考」
	//（reasoning 模型 + Thinking=false / effort=none，要发 none/disabled 表达关闭意图）。
	// 为什么必须区分（2026-09）：兼容端点（minimal）省略 reasoning_effort 的历史策略取消后，
	// 非推理模型会收到 reasoning_effort=none —— 严格端点对不支持思考的模型可能直接 400。
	ThinkingUnsupported bool
}

// ResolveThinking 解析思考请求（归一档位 → 模型能力）：
//   - cfg.Thinking 显式 false → 关闭；
//   - 模型 ThinkingDefaultOff（非推理模型）且未显式开 → 关闭；
//   - 其余 → 开启，档位 clamp 到模型支持的最近档位（ThinkingSupportedEfforts 非空时）。
//   - ToggleOnly 模型 → 只开/关，Effort 忽略。
func (r *Registry) ResolveThinking(provider, model string, cfg *RequestConfig) ThinkingDecision {
	// 元数据解析三级：本 provider → 跨 provider 按名 → 名称族（见 Resolve）。自建 provider 下
	// 的已知模型名 / 网关新版本号（deepseek-v4.1-flash）都与内置同名模型同语义 ——
	// 否则会掉进「未知模型」分支：force-on 关不掉、toggle-only 被发档位、非推理模型被发思考参数。
	info, ok := r.Resolve(provider, model)
	if !ok {
		// 未知模型：保持现状行为（默认开 high，可显式关）
		enabled := true
		if cfg != nil && cfg.Thinking != nil {
			enabled = *cfg.Thinking
		}
		effort := cfg.Effort
		if effort == "" {
			effort = ReasoningEffortLevelHigh
		}
		return ThinkingDecision{Enabled: enabled, Effort: effort}
	}

	// 显式开关优先；effort=none 视为显式关思考（对齐 pi：reasoningEffort 为空/off 档 → 关）
	enabled := true
	if cfg != nil {
		if cfg.Thinking != nil {
			enabled = *cfg.Thinking
		} else if cfg.Effort == ReasoningEffortLevelNone {
			enabled = false // 用户显式选 none（关闭思考）
		}
	}
	// 强制思考模型（GLM-5.3/5.3-FLASH 等官方明确 thinking.type=disabled 会报错）：
	// 显式关思考被忽略——始终开启（请求侧省略 thinking 字段 = 默认开启）。
	if info.ThinkingForceOn {
		enabled = true
	}
	if !enabled {
		// 非推理模型（ThinkingDefaultOff）即便用户选了「关思考」也不发思考参数：对没有思考
		// 能力的模型发 none/disabled 无意义，且严格端点可能直接 400（minimal 不再省略
		// reasoning_effort 后这是新暴露面，2026-09）。
		// ToggleOnly 照常带上（它是**模型属性**，与开关状态无关）：请求侧据此不发档位 ——
		// 小米 MiMo 官方只认 thinking.type（deepseek 形态），关思考时发 reasoning_effort=none
		// 属于没人认得的字段（2026-09 对齐）。
		return ThinkingDecision{Enabled: false, ToggleOnly: info.ThinkingToggleOnly,
			ThinkingUnsupported: info.ThinkingDefaultOff}
	}
	// 非推理模型（ThinkingDefaultOff，reasoning:false）默认关思考；显式 Thinking=true 才开
	//（对齐 pi：reasoning:false 模型不参与 thinkingFormat 分支，effort 传了也不开）。
	if info.ThinkingDefaultOff && (cfg == nil || cfg.Thinking == nil) {
		return ThinkingDecision{Enabled: false, ThinkingUnsupported: true}
	}
	if info.ThinkingToggleOnly {
		return ThinkingDecision{Enabled: true, ToggleOnly: true}
	}

	// 档位：显式指定 → 先查 ThinkingLevels 映射（厂商档位表），再 clamp；未指定 → 模型默认
	effort := ReasoningEffortLevelHigh
	if cfg != nil && cfg.Effort != "" {
		effort = cfg.Effort
	}
	// ThinkingLevels 显式映射优先（deepseek-v4-pro xhigh→max 等）
	if mapped, ok := info.ThinkingLevels[effort]; ok && mapped != "" {
		effort = ReasoningEffortLevel(mapped)
		return ThinkingDecision{Enabled: true, Effort: effort}
	}
	if len(info.ThinkingSupportedEfforts) > 0 {
		effort = clampEffort(effort, info.ThinkingSupportedEfforts)
	}
	return ThinkingDecision{Enabled: true, Effort: effort}
}

// clampEffort 把请求档位 clamp 到模型支持的档位列表（按归一化顺序取最近）。
// 支持的档位来自 models.dev reasoning_options；请求档位不在其中时取最接近的。
var effortOrder = []ReasoningEffortLevel{
	ReasoningEffortLevelNone, ReasoningEffortLevelMinimal, ReasoningEffortLevelLow,
	ReasoningEffortLevelMedium, ReasoningEffortLevelHigh, ReasoningEffortLevelXHigh, ReasoningEffortLevelMax,
}

func clampEffort(e ReasoningEffortLevel, supported []ReasoningEffortLevel) ReasoningEffortLevel {
	// 支持列表里直接命中
	for _, s := range supported {
		if s == e {
			return e
		}
	}
	// 取支持列表中与请求档位「最接近」（归一化序）的档位；
	// 距离相同时取更高档（medium → high 而非 low，思考更充分）
	idx := indexOf(effortOrder, e)
	if idx < 0 {
		idx = indexOf(effortOrder, ReasoningEffortLevelHigh) // 未知档位默认 high
	}
	best := supported[0]
	bestDist := 1 << 30
	for _, s := range supported {
		si := indexOf(effortOrder, s)
		if si < 0 {
			continue
		}
		d := si - idx
		if d < 0 {
			d = -d
		}
		if d < bestDist || (d == bestDist && si > idx) {
			bestDist = d
			best = s
		}
	}
	return best
}

func indexOf(ss []ReasoningEffortLevel, e ReasoningEffortLevel) int {
	for i, s := range ss {
		if s == e {
			return i
		}
	}
	return -1
}

// BuiltinModels 汇总全部 Provider 的内置模型常量表。
// 数据源：providers.json（单一事实源，embed 编译期快照；见 registry_json.go）。
// 纯函数，无全局状态；由 NewRegistry() 调用。
func BuiltinModels() map[string]ModelInfo {
	return builtinModelsFromJSON()
}
