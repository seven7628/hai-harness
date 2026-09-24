package provider

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

// providers.json 是 Provider 预设 + 模型元数据的单一事实源（统一管理）：
//   - presets：内置 Provider 预设（name/base_url/protocol/env_keys/oauth）
//   - models：全部内置模型的完整元数据（窗口/上限/模态/推理/思考档位/价格/协议）
//
// 生成方式：scripts/gen_providers_json.py（从旧 models_*.go / presets.go 提取，人工校验后
// 固化；后续只改 JSON 不再改 Go 代码）。Go 侧 embed 加载，前端经 bridge
// list_provider_presets 命令读取同一数据源。
//
// 注意：providers.json 修改后需要重新 build（embed 是编译期快照）。
//
//go:embed providers.json
var providersJSON []byte

// jsonModel 与 providers.json 的 models 段对应（JSON 字段 → ModelInfo）。
type jsonModel struct {
	ID                       string            `json:"id"`
	ContextWindow            int64             `json:"context_window"`
	MaxTokens                int64             `json:"max_tokens"`
	Input                    []string          `json:"input"`
	MaxTokensField           string            `json:"max_tokens_field,omitempty"`
	Reasoning                *bool             `json:"reasoning,omitempty"`
	ThinkingFormat           string            `json:"thinking_format,omitempty"`
	RequiresReasoningContent *bool             `json:"requires_reasoning_content,omitempty"`
	ThinkingToggleOnly       *bool             `json:"thinking_toggle_only,omitempty"`
	ThinkingDefaultOff       *bool             `json:"thinking_default_off,omitempty"`
	ThinkingForceOn          *bool             `json:"thinking_force_on,omitempty"`
	SupportsThinking         *bool             `json:"supports_thinking,omitempty"`
	SupportsTemperature      *bool             `json:"supports_temperature,omitempty"`
	SupportsCacheControl     *bool             `json:"supports_cache_control,omitempty"`
	UsageInputIncludesCache  *bool             `json:"usage_input_includes_cache,omitempty"`
	CacheTTL1h               *bool             `json:"cache_ttl_1h,omitempty"`
	ThinkingSupportedEfforts []string          `json:"thinking_supported_efforts,omitempty"`
	ThinkingLevels           map[string]string `json:"thinking_levels,omitempty"`
	Cost                     *jsonCost         `json:"cost,omitempty"`
	Protocol                 string            `json:"protocol,omitempty"`
	// 兼容开关（对齐 pi OpenAICompletionsCompat；缺省 = 默认语义）：
	SupportsDeveloperRole      *bool `json:"supports_developer_role,omitempty"`
	RequiresAssistantAfterTool *bool `json:"requires_assistant_after_tool_result,omitempty"`
	RequiresToolResultName     *bool `json:"requires_tool_result_name,omitempty"`
	RequiresThinkingAsText     *bool `json:"requires_thinking_as_text,omitempty"`
	// 厂商扩展（对齐 pi OpenAICompletionsCompat）：
	ZaiToolStream     *bool             `json:"zai_tool_stream,omitempty"`
	ChatTemplateArgs  map[string]string `json:"chat_template_args,omitempty"`
	DeferredToolsMode string            `json:"deferred_tools_mode,omitempty"`
}

type jsonCost struct {
	Input      float64 `json:"input"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Output     float64 `json:"output"`
}

// jsonPreset 与 providers.json 的 presets 段对应。
type jsonPreset struct {
	Name     string     `json:"name"`
	ID       string     `json:"id"`
	BaseURL  string     `json:"base_url"`
	Protocol string     `json:"protocol"`
	Models   []string   `json:"models,omitempty"`
	EnvKeys  []string   `json:"env_keys,omitempty"`
	OAuth    *jsonOAuth `json:"oauth,omitempty"`
}

type jsonOAuth struct {
	Name           string `json:"name,omitempty"`
	IsSubscription bool   `json:"is_subscription,omitempty"`
	LoginLabel     string `json:"login_label,omitempty"`
	Flow           string `json:"flow,omitempty"`
	KeyInstead     bool   `json:"key_instead,omitempty"`
}

type providersFile struct {
	SchemaVersion int                    `json:"schema_version"`
	Models        map[string][]jsonModel `json:"models"`
	Presets       []jsonPreset           `json:"presets"`
}

var (
	loadOnce      sync.Once
	loadedModels  map[string][]ModelInfo // provider → 模型列表（JSON 加载）
	loadedPresets []ProviderPreset
	loadErr       error
)

// loadProvidersJSON 解析 embed 的 providers.json（惰性一次；错误返回给调用方）。
func loadProvidersJSON() (map[string][]ModelInfo, []ProviderPreset, error) {
	loadOnce.Do(func() {
		var f providersFile
		if err := json.Unmarshal(providersJSON, &f); err != nil {
			loadErr = fmt.Errorf("providers.json 解析失败: %w", err)
			return
		}
		models := make(map[string][]ModelInfo, len(f.Models))
		for prov, list := range f.Models {
			ms := make([]ModelInfo, 0, len(list))
			for _, jm := range list {
				ms = append(ms, jsonModelToInfo(prov, jm))
			}
			models[prov] = ms
		}
		presets := make([]ProviderPreset, 0, len(f.Presets))
		for _, jp := range f.Presets {
			presets = append(presets, jsonPresetToPreset(jp))
		}
		loadedModels = models
		loadedPresets = presets
	})
	return loadedModels, loadedPresets, loadErr
}

func boolOrNil(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

func jsonModelToInfo(prov string, jm jsonModel) ModelInfo {
	info := ModelInfo{
		ID:                         jm.ID,
		Provider:                   prov,
		ContextWindow:              jm.ContextWindow,
		MaxTokens:                  jm.MaxTokens,
		Inputs:                     jm.Input,
		MaxTokensField:             jm.MaxTokensField,
		Reasoning:                  boolOrNil(jm.Reasoning, false),
		ThinkingFormat:             jm.ThinkingFormat,
		RequiresReasoningContent:   boolOrNil(jm.RequiresReasoningContent, false),
		ThinkingToggleOnly:         boolOrNil(jm.ThinkingToggleOnly, false),
		ThinkingDefaultOff:         boolOrNil(jm.ThinkingDefaultOff, false),
		ThinkingForceOn:            boolOrNil(jm.ThinkingForceOn, false),
		SupportsThinking:           jm.SupportsThinking,
		SupportsTemperature:        jm.SupportsTemperature,
		SupportsCacheControl:       jm.SupportsCacheControl,
		UsageInputIncludesCache:    jm.UsageInputIncludesCache,
		CacheTTL1h:                 jm.CacheTTL1h,
		SupportsDeveloperRole:      jm.SupportsDeveloperRole,
		RequiresAssistantAfterTool: jm.RequiresAssistantAfterTool,
		RequiresToolResultName:     jm.RequiresToolResultName,
		RequiresThinkingAsText:     jm.RequiresThinkingAsText,
		ZaiToolStream:              jm.ZaiToolStream,
		ChatTemplateArgs:           jm.ChatTemplateArgs,
		DeferredToolsMode:          jm.DeferredToolsMode,
		Protocol:                   Protocol(jm.Protocol),
	}
	if len(jm.Input) == 0 {
		info.Inputs = []string{"text"}
	}
	if info.MaxTokensField == "" {
		info.MaxTokensField = "max_completion_tokens"
	}
	if info.Protocol == "" {
		info.Protocol = ProtocolChatCompletions
	}
	for _, e := range jm.ThinkingSupportedEfforts {
		info.ThinkingSupportedEfforts = append(info.ThinkingSupportedEfforts, ReasoningEffortLevel(e))
	}
	if len(jm.ThinkingLevels) > 0 {
		info.ThinkingLevels = make(map[ReasoningEffortLevel]string, len(jm.ThinkingLevels))
		for k, v := range jm.ThinkingLevels {
			info.ThinkingLevels[ReasoningEffortLevel(k)] = v
		}
	}
	if jm.Cost != nil {
		info.Cost = ModelPrice{
			Input: jm.Cost.Input, CacheRead: jm.Cost.CacheRead,
			CacheWrite: jm.Cost.CacheWrite, Output: jm.Cost.Output,
		}
	}
	return info
}

func jsonPresetToPreset(jp jsonPreset) ProviderPreset {
	p := ProviderPreset{
		Name:     jp.Name,
		ID:       jp.ID,
		BaseURL:  jp.BaseURL,
		Protocol: Protocol(jp.Protocol),
		Models:   jp.Models,
		EnvKeys:  jp.EnvKeys,
	}
	if jp.OAuth != nil {
		p.OAuth = &OAuthSpec{
			Name:           jp.OAuth.Name,
			IsSubscription: jp.OAuth.IsSubscription,
			LoginLabel:     jp.OAuth.LoginLabel,
			Flow:           jp.OAuth.Flow,
			KeyInstead:     jp.OAuth.KeyInstead,
		}
	}
	return p
}

// builtinModelsFromJSON 从 providers.json 构建注册表模型（BuiltinModels 的数据源）。
var (
	builtinModelsOnce sync.Once
	builtinModelsMap  map[string]ModelInfo
)

func builtinModelsFromJSON() map[string]ModelInfo {
	builtinModelsOnce.Do(func() {
		models, _, err := loadProvidersJSON()
		if err != nil {
			// embed 数据损坏是编程错误：panic 暴露（启动即失败，不静默降级）
			panic(err)
		}
		out := map[string]ModelInfo{}
		for prov, list := range models {
			for _, info := range list {
				info.Provider = prov
				info.SupportsImage = supportsImageOf(info.Inputs)
				out[prov+"\x00"+info.ID] = info
			}
		}
		builtinModelsMap = out
	})
	return builtinModelsMap
}

// builtinPresetsFromJSON 从 providers.json 构建预设列表（BuiltinProviderPresets 的数据源）。
func builtinPresetsFromJSON() []ProviderPreset {
	_, presets, err := loadProvidersJSON()
	if err != nil {
		panic(err)
	}
	return presets
}
