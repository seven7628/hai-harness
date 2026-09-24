package provider

// OAuthSpec 描述一个 provider 的 OAuth 订阅登录能力（对齐 pi Provider.auth.oauth）。
// 只在 ProviderPreset.OAuth 非 nil 时前端显示「OAuth 登录」入口。
type OAuthSpec struct {
	Name           string `json:"name"`            // 显示名，如 "Anthropic (Claude Pro/Max)"
	IsSubscription bool   `json:"is_subscription"` // 是否订阅制登录
	LoginLabel     string `json:"login_label"`     // 登录按钮文案（如 "Sign in with SuperGrok or X Premium"）
	Flow           string `json:"flow"`            // 对应 auth/oauth registry 的 flow id
	KeyInstead     bool   `json:"key_instead"`     // 登录产物是永久 key → 落 api_key 通道（OpenRouter）
}

// ProviderPreset 内置 Provider 预设（前端下拉可选；用户添加后才写入 settings 生效）。
// 对齐 pi providers/<id>.ts 的 {id, name, baseUrl, api, models}。
type ProviderPreset struct {
	Name     string     `json:"name"`               // 显示名（如 "DeepSeek"）
	ID       string     `json:"id"`                 // provider 键（deepseek/groq/...）
	BaseURL  string     `json:"base_url"`           // 官方端点（可被用户覆盖）
	Protocol Protocol   `json:"protocol"`           // chat_completions / responses / anthropic
	Models   []string   `json:"models"`             // 预设模型清单（注册表内置；可被 list_models 拉取扩充）
	EnvKeys  []string   `json:"env_keys,omitempty"` // 读取的 env 变量名（keyLocked 用）
	OAuth    *OAuthSpec `json:"oauth,omitempty"`    // OAuth 订阅登录声明（nil = 仅 API key）
}

// BuiltinProviderPresets 全部内置 Provider 预设。
// 数据源：providers.json（单一事实源，embed 编译期快照；见 registry_json.go）。
// 用户添加后：bridge set_provider 写入 baseURL/protocol/models；模型元数据来自注册表（真实常量）。
var BuiltinProviderPresets = builtinPresetsFromJSON()

// FindProviderPreset 按 ID 查预设；找不到返回 nil。
func FindProviderPreset(id string) *ProviderPreset {
	for i := range BuiltinProviderPresets {
		if BuiltinProviderPresets[i].ID == id {
			return &BuiltinProviderPresets[i]
		}
	}
	return nil
}
