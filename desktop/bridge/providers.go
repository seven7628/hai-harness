// provider 配置（桌面端多 provider 热切换）。
// 非密钥部分（active / base_url / models 窗口）来自 ~/.go-code/settings.json；
// API key 来自 Electron 注入的环境变量（boot）或 set_provider 命令（运行中热切换）。
// 热切换：set_provider 更新配置 → rebuildAll 重建全部会话 loop（复用 switch_persona 的 rebuild 模式）。
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/provider/anthropic"
	"github.com/seven7628/hai-harness/provider/auth"
	"github.com/seven7628/hai-harness/provider/auth/oauth"
	"github.com/seven7628/hai-harness/provider/responses"
	"github.com/seven7628/hai-harness/session/remote"
	"github.com/seven7628/hai-harness/subagent"
)

const defaultContextWindow = 128000

// sandboxSettings settings.json 顶层 sandbox 段（桌面端命令沙箱）。
// Mode: "none"（进程隔离直通）| "seatbelt"（macOS 沙箱，缺省）。
// SensitivePaths: 追加进 seatbelt 策略的强制 deny 路径（绝对路径或相对 home）。
// WriteHome: 允许写入 home（用户级包安装：pip/npm/go/cargo）。默认 true（缺省放宽，
// 否则命令装不了已装包）；安全兜底 = bash 静态高危拦截 + 敏感路径 deny 读写 + WithRelease 放行门。
type sandboxSettings struct {
	Mode           string   `json:"mode"`
	SensitivePaths []string `json:"sensitive_paths,omitempty"`
	WriteHome      *bool    `json:"write_home,omitempty"` // 指针：区分「未配置」与「false」
}

type agentSettings struct {
	ReminderRounds int `json:"reminder_rounds"`
	// MaxSubagents / MaxExploreSubagents 子 agent 并发槽上限（0/缺失 = 默认
	// subagent.DefaultConcurrency = 1000）。主池 = agent_spawn（写型子 agent），
	// 辅池 = subagent_explore（只读）——分池避免 explore 占满饿死 spawn。
	// 2026-09-20 加固：原硬编码 4 太小，槽满会让工具调用同步阻塞 → 主 Agent 整批挂死。
	MaxSubagents        int `json:"max_subagents"`
	MaxExploreSubagents int `json:"max_explore_subagents"`
}

// maxSubagents 主池上限（<=0 = 默认）。
func (s agentSettings) maxSubagents() int {
	if s.MaxSubagents <= 0 {
		return subagent.DefaultConcurrency
	}
	return s.MaxSubagents
}

// maxExploreSubagents 辅池上限（<=0 = 默认）。
func (s agentSettings) maxExploreSubagents() int {
	if s.MaxExploreSubagents <= 0 {
		return subagent.DefaultConcurrency
	}
	return s.MaxExploreSubagents
}

// runtimeSettings settings.json 顶层 runtime 段：运行期行为开关。
// StopBackgroundOnInterrupt: 主「停止」（Composer 停止按钮 → interrupt 命令）是否
// 连后台子 agent 一起停（S3-B）。默认 true（保持兼容——用户显式点主停止时预期
// 一并停下，否则子 agent 流式输出仍在、且前端 streaming 门控会卡输入）；
// false = 主停止只停主会话，后台任务继续运行（任务按 interrupt_task 精确停止）。
// 指针语义：缺字段 = 保持默认 true。
type runtimeSettings struct {
	StopBackgroundOnInterrupt *bool `json:"stop_background_on_interrupt"`
}

// loadRuntimeSettings 读 runtime 段（settings.json；缺失/坏配置 → 零值 = 全默认）。
func loadRuntimeSettings() runtimeSettings {
	raw, err := os.ReadFile(homeCfgPath("settings.json"))
	if err != nil {
		return runtimeSettings{}
	}
	var outer struct {
		Runtime *runtimeSettings `json:"runtime"`
	}
	if json.Unmarshal(raw, &outer) != nil || outer.Runtime == nil {
		return runtimeSettings{}
	}
	return *outer.Runtime
}

// stopBackgroundOnInterrupt 生效值（默认 false：主停止只停主会话，
// 后台异步任务不受影响——需停单个任务走 interrupt_task 精确中断）。
func stopBackgroundOnInterrupt() bool {
	rs := loadRuntimeSettings()
	if rs.StopBackgroundOnInterrupt == nil {
		return false
	}
	return *rs.StopBackgroundOnInterrupt
}

func loadAgentSettings() agentSettings {
	out := agentSettings{ReminderRounds: 30}
	raw, err := os.ReadFile(homeCfgPath("settings.json"))
	if err != nil {
		return out
	}
	var root struct {
		Agent *agentSettings `json:"agent"`
	}
	if json.Unmarshal(raw, &root) == nil && root.Agent != nil && root.Agent.ReminderRounds >= 0 {
		out = *root.Agent
	}
	return out
}

// → true（放宽，用户决策 2026-08-16）。Seatbelt 后端在装配时不可用（缺 sandbox-exec/
// 构造失败）再降级 NoSandbox。
func loadSandboxSettings() sandboxSettings {
	ss := sandboxSettings{Mode: "seatbelt"}
	raw, err := os.ReadFile(homeCfgPath("settings.json"))
	if err != nil {
		return ss
	}
	var outer struct {
		Sandbox json.RawMessage `json:"sandbox"`
	}
	if json.Unmarshal(raw, &outer) != nil || outer.Sandbox == nil {
		return ss
	}
	var s sandboxSettings
	if json.Unmarshal(outer.Sandbox, &s) != nil {
		return ss // 坏 sandbox 配置回默认 seatbelt
	}
	if s.Mode == "none" || s.Mode == "seatbelt" {
		ss.Mode = s.Mode
	}
	ss.SensitivePaths = s.SensitivePaths
	if s.WriteHome != nil {
		ss.WriteHome = s.WriteHome
	}
	return ss
}

// loadComputerApprovedApps 读 Computer Use 已授权 App 集合（显示名）。
// 来源：settings.json permissions.computer_approved_apps（唯一入口——旧 allowed_apps
// open 白名单已随「权限」设置页移除而废弃，不再兜底）。字段缺失/坏值/空 → 空集合
// （全部拒绝，安全默认——用户显式授权后才放行）。
// 返回值转 map 便于 Executor 门禁 O(1) 判定。
func loadComputerApprovedApps() map[string]bool {
	raw, err := os.ReadFile(homeCfgPath("settings.json"))
	if err != nil {
		return map[string]bool{}
	}
	var outer struct {
		Permissions map[string]json.RawMessage `json:"permissions"`
	}
	if json.Unmarshal(raw, &outer) != nil {
		return map[string]bool{}
	}
	var src []string
	if permRaw, ok := outer.Permissions["computer_approved_apps"]; ok {
		_ = json.Unmarshal(permRaw, &src) // 坏值 → src 空（安全默认）
	}
	out := make(map[string]bool, len(src))
	for _, a := range src {
		if a != "" {
			out[a] = true
		}
	}
	return out
}

// cronSettings settings.json 顶层 cron 段（桌面端定时任务功能）。
// Enabled: 是否开启定时任务工具 + 触发调度（缺省 true，用户决策 2026-08-19）。
// AutoClean: 是否自动清理 N 天前的派生会话文件（缺省 true；只删派生会话文件，
// 任务定义 cron.json 与运行账本 cron_runs.jsonl 保留）。
type cronSettings struct {
	Enabled   bool `json:"enabled"`
	AutoClean bool `json:"auto_clean"`
}

// loadCronSettings 读定时任务设置：未配置/坏配置 → 默认全开
// （Enabled=true, AutoClean=true，用户决策 2026-08-19）。
func loadCronSettings() cronSettings {
	cs := cronSettings{Enabled: true, AutoClean: true}
	raw, err := os.ReadFile(homeCfgPath("settings.json"))
	if err != nil {
		return cs
	}
	var outer struct {
		Cron json.RawMessage `json:"cron"`
	}
	if json.Unmarshal(raw, &outer) != nil || outer.Cron == nil {
		return cs
	}
	var c cronSettings
	if json.Unmarshal(outer.Cron, &c) != nil {
		return cs // 坏配置回默认全开
	}
	return c
}

// loadUILang 读界面语言（settings.json 顶层 lang；zh/en）。缺省/坏配置 → "zh"
// （产品默认界面语言）。供 mesh_from 来源提示等后端 i18n 文案使用。
func loadUILang() string {
	raw, err := os.ReadFile(homeCfgPath("settings.json"))
	if err != nil {
		return "zh"
	}
	var outer struct {
		Lang string `json:"lang"`
	}
	if json.Unmarshal(raw, &outer) != nil {
		return "zh"
	}
	if outer.Lang == "en" {
		return "en"
	}
	return "zh"
}

// meshSettings settings.json 顶层 mesh 段（Session Mesh 会话间通信能力开关）。
// 类型归属：remote.Settings 定义在 session/remote/config.go（与 Gateway 同包，
// 防 desktop↔session 循环依赖）；本文件只负责「读 JSON → 产出 remote.Settings」。
type meshSettings struct {
	SessionEnabled bool            `json:"session_enabled"` // C1：本机多 Session 互聊工具
	RemoteEnabled  bool            `json:"remote_enabled"`  // C2：远程多节点互通（依赖 SessionEnabled）
	Remote         remote.Settings `json:"remote"`          // 远程网关配置（listen/peers/token）
}

// loadMeshSettings 读 Session Mesh 设置：未配置/坏配置 → 全关
// （安全默认，与 cron 的默认全开相反——「开启后注册工具」需求）。
// 依赖约束：remote_enabled 依赖 session_enabled（远程消息最终落在本机会话）。
func loadMeshSettings() meshSettings {
	ms := meshSettings{} // 默认全关
	raw, err := os.ReadFile(homeCfgPath("settings.json"))
	if err != nil {
		return ms
	}
	var outer struct {
		Mesh json.RawMessage `json:"mesh"`
	}
	if json.Unmarshal(raw, &outer) != nil || outer.Mesh == nil {
		return ms
	}
	var m meshSettings
	if json.Unmarshal(outer.Mesh, &m) != nil {
		return ms // 坏配置回默认全关
	}
	if !m.SessionEnabled {
		m.RemoteEnabled = false // 依赖约束
	}
	// 默认值补齐：listen_addr 缺省 127.0.0.1:17890（与前端表单默认一致）。
	// 缺省为空会导致 Gateway.startServer 判定"纯客户端不监听"——远程开而本机不可连。
	if m.Remote.ListenAddr == "" {
		m.Remote.ListenAddr = defaultMeshListenAddr
	}
	return m
}

// defaultMeshListenAddr 本机 mesh 监听缺省地址（仅回环，防意外暴露；与前端默认一致）。
const defaultMeshListenAddr = "127.0.0.1:17890"

// meshNodeID 本机 Session Mesh 节点标识：settings mesh.remote.node_id 非 auto 用之；
// "auto"（缺省）→ 读 ~/.go-code/mesh_node_id，不存在则生成 UUID v4 持久化。
// 节点 id 全局唯一，用于 node://<node-id>/<session-id> 远程寻址去歧义。
// meshNodeIDCache 进程内缓存：写盘失败时也保持进程内稳定（N10 修复），
// 避免每次调用生成新 UUID 导致 node:// 寻址漂移。
var meshNodeIDCache string

func meshNodeID() string {
	if meshNodeIDCache != "" {
		return meshNodeIDCache
	}
	ms := loadMeshSettings()
	if ms.Remote.NodeID != "" && ms.Remote.NodeID != "auto" {
		meshNodeIDCache = ms.Remote.NodeID
		return meshNodeIDCache
	}
	path := homeCfgPath("mesh_node_id")
	if b, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			meshNodeIDCache = s
			return s
		}
	}
	id := newUUID()
	meshNodeIDCache = id
	_ = os.WriteFile(path, []byte(id), 0o600) // 节点标识非敏感但防误读，0600；写失败则进程内仍稳定
	return id
}

// newUUID 生成 UUID v4（crypto/rand；失败回退时间戳+随机十六进制）。
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		b[6] = (b[6] & 0x0f) | 0x40 // version 4
		b[8] = (b[8] & 0x3f) | 0x80 // variant 10
		return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	}
	return fmt.Sprintf("node-%d-%x", time.Now().UnixNano(), time.Now().UnixNano()&0xffff)
}

// defaultMaxTokens 单次输出上限兜底（未配置/未知模型）：保持现状 8192。
const defaultMaxTokens = 8192

// OpenAI 兼容接入协议（Provider 面板可选；三协议均已实现：chat-completions / responses / anthropic）。
const (
	protocolChatCompletions = "chat_completions"
	protocolResponses       = "responses"
	protocolAnthropic       = "anthropic"
)

type providerConfig struct {
	mu        sync.Mutex
	active    string                                       // "deepseek" | "openai" | "opencode" | "kimi" | "zhipu" | 自定义
	baseURL   map[string]string                            // provider → base url
	apiKey    map[string]string                            // provider → key（内存；env / set_provider）
	models    map[string]map[string]int64                  // provider → model → context window（tokens）
	maxTokens map[string]map[string]int64                  // provider → model → 单次输出上限（tokens）
	protocol  map[string]string                            // provider → OpenAI 接入协议
	compat    map[string]provider.OpenAICompatCapabilities // provider → chat-completions 能力
	// 新增（2026-08 provider 优化）：
	inputTypes map[string]map[string][]string // provider → model → ["text","image"]（前端 input_types；bridge 现开始消费）
	registry   *provider.Registry             // 模型元数据注册表（内置真实常量 + settings 覆盖；依赖注入）
	// usage 口径标注（2026-09-21）：provider → input_tokens 是否已含缓存（settings
	// usage_input_includes_cache）。存在 = 用户显式标注（覆盖协议默认与响应形状嗅探）；
	// 不存在 = 自动（anthropic 协议按官方语义相加，OpenAI 形状网关靠响应自报字段判定）。
	usageIncl map[string]bool
	// cacheTTL provider → Anthropic 缓存断点 TTL 是否为 1h（settings cache_ttl_1h）。
	// 存在 = 用户显式标注（覆盖端点默认：官方 1h / 兼容 5m）；不存在 = 自动。
	cacheTTL map[string]bool
	// 新增（2026-08 OAuth 订阅登录）：
	authType   map[string]string          // provider → "api_key"（默认）| "oauth"（settings auth_type）
	oauthStore *auth.Store                // OAuth 凭证存储（~/.go-code/oauth.json）
	oauthFlow  map[string]auth.Flow       // provider → flow（懒构造缓存）
	oauthTrans map[string]*auth.Transport // provider → OAuth 注入 transport（缓存）
}

func homeCfgPath(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return name
	}
	return filepath.Join(home, ".go-code", name)
}

// builtinFiveDefaultTables 内置 5 家（deepseek/openai/opencode/kimi/zhipu）的默认模型表，
// 从 providers.json（注册表单一事实源）派生，消除手工维护漂移：
//   - models：预设模型清单 → context_window（首次打开/未配置时的展示模型）
//   - maxTokens：模型真实 max_tokens
//   - inputTypes：模型真实输入模态（含 image → ["text","image"]；纯文本 → 不列出，
//     由注册表默认兜底）
func builtinFiveDefaultTables() (models, maxTokens map[string]map[string]int64, inputTypes map[string]map[string][]string) {
	models = map[string]map[string]int64{}
	maxTokens = map[string]map[string]int64{}
	inputTypes = map[string]map[string][]string{}
	for _, id := range []string{"deepseek", "openai", "opencode", "kimi", "zhipu"} {
		preset := provider.FindProviderPreset(id)
		if preset == nil {
			continue
		}
		m := map[string]int64{}
		mt := map[string]int64{}
		it := map[string][]string{}
		for _, mid := range preset.Models {
			info, ok := provider.RegistryLookupBuiltin(id, mid)
			if !ok {
				continue
			}
			if info.ContextWindow > 0 {
				m[mid] = info.ContextWindow
			}
			if info.MaxTokens > 0 {
				mt[mid] = info.MaxTokens
			}
			inputs := info.Inputs
			if len(inputs) == 0 {
				inputs = []string{"text"}
			}
			hasImage := false
			for _, in := range inputs {
				if in == "image" {
					hasImage = true
					break
				}
			}
			if hasImage {
				it[mid] = inputs
			}
		}
		if len(m) > 0 {
			models[id] = m
		}
		if len(mt) > 0 {
			maxTokens[id] = mt
		}
		if len(it) > 0 {
			inputTypes[id] = it
		}
	}
	return models, maxTokens, inputTypes
}

// loadProviderConfig：默认值 + env key + settings.json（base_url/active/models/max_tokens）
func loadProviderConfig() *providerConfig {
	// 内置 5 家的默认模型表从 providers.json（单一事实源）派生：
	//   models     = 预设模型清单 → 真实窗口
	//   maxTokens  = 模型真实 max_tokens
	//   inputTypes = 模型真实输入模态（含 image → ["text","image"]）
	// 不再手工维护（历史：providers.go 硬编码表与 JSON 漂移，如 opencode glm-5.1 曾误写 32768）。
	models, maxTokens, inputTypes := builtinFiveDefaultTables()
	pc := &providerConfig{
		active: "deepseek",
		baseURL: map[string]string{
			"deepseek": "https://api.deepseek.com/v1",
			"openai":   "https://api.openai.com/v1",
			"opencode": "https://opencode.ai/zen/go/v1",
			// Kimi（月之暗面/Moonshot）：境内端点；境外可改 https://api.moonshot.ai/v1
			"kimi": "https://api.moonshot.cn/v1",
			// 智谱 Zhipu（ZAI/BigModel）：版本根为 /api/paas/v4（非 /v1），
			// 故 compat 的 BasePath 置 /v4，保证 /chat/completions 落在正确路径。
			"zhipu": "https://open.bigmodel.cn/api/paas/v4",
		},
		apiKey:    map[string]string{},
		models:    models,
		maxTokens: maxTokens,
		protocol: map[string]string{
			"deepseek": protocolChatCompletions,
			"openai":   protocolChatCompletions,
			"opencode": protocolChatCompletions,
			"kimi":     protocolChatCompletions,
			"zhipu":    protocolChatCompletions,
		},
		compat: map[string]provider.OpenAICompatCapabilities{
			"deepseek": provider.FullOpenAICompatCapabilities(),
			"openai":   provider.FullOpenAICompatCapabilities(),
			"opencode": provider.FullOpenAICompatCapabilities(),
			"kimi":     provider.FullOpenAICompatCapabilities(),
			// 智谱 /v4：保守能力（tools + stream_usage，省略 reasoning_effort/
			// max_completion_tokens——GLM 用 thinking/max_tokens），BasePath=/v4 修正版本根。
			"zhipu": provider.ZhipuOpenAICompatCapabilities(),
		},
		registry:   provider.NewRegistry(), // 内置真实模型常量（providers.json 单一事实源）
		inputTypes: inputTypes,
		// OAuth 订阅登录（2026-08）：凭证存 ~/.go-code/oauth.json；flow/transport 懒构造
		authType:   map[string]string{},
		usageIncl:  map[string]bool{}, // usage 口径标注（settings usage_input_includes_cache；空 = 全自动）
		cacheTTL:   map[string]bool{}, // 缓存 TTL 标注（settings cache_ttl_1h；空 = 端点默认）
		oauthStore: auth.NewStore(""), // 默认 ~/.go-code/oauth.json（0600）
		oauthFlow:  map[string]auth.Flow{},
		oauthTrans: map[string]*auth.Transport{},
	}
	// env key（Electron main 注入）；DeepSeek 兜底 .env（resolveToken）
	if v := resolveToken(); v != "" {
		pc.apiKey["deepseek"] = v
	}
	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		pc.apiKey["openai"] = strings.TrimSpace(v)
	}
	if v := os.Getenv("OPENCODE_API_KEY"); v != "" {
		pc.apiKey["opencode"] = strings.TrimSpace(v)
	}
	if v := os.Getenv("KIMI_API_KEY"); v != "" {
		pc.apiKey["kimi"] = strings.TrimSpace(v)
	} else if v := os.Getenv("MOONSHOT_API_KEY"); v != "" {
		pc.apiKey["kimi"] = strings.TrimSpace(v)
	}
	if v := os.Getenv("ZHIPUAI_API_KEY"); v != "" {
		pc.apiKey["zhipu"] = strings.TrimSpace(v)
	}
	pc.loadSettings()
	pc.syncRegistry() // 把 settings 窗口/上限/价表/input_types 覆盖注入注册表（用户配置 > 内置常量）
	return pc
}

// loadSettings 解析 settings.json 的 provider 段（内置三卡 + 用户自定义统一按 key 遍历）。
// active 是保留键；其余每个 key 是一个 provider（base_url/api_key/protocol/models/max_tokens），
// 键名经 validProviderName 校验（跳过非法键）。内置三卡默认值在 loadProviderConfig 预置，
// 这里只覆盖显式配置；自定义 provider 在此全量加载。
// api_key 明文持久化在 settings.json（桌面端用户决策 2026-08-16：Provider 设置直接写
// settings.json，不依赖 .env/keys.json 注入）——loadSettings 是 bridge 读取 key 的主通道。
func (pc *providerConfig) loadSettings() {
	raw, err := os.ReadFile(homeCfgPath("settings.json"))
	if err != nil {
		return
	}
	var outer struct {
		Provider map[string]json.RawMessage `json:"provider"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		return
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if raw, ok := outer.Provider["active"]; ok {
		var active string
		if json.Unmarshal(raw, &active) == nil && validProviderName(active) {
			pc.active = active
		}
	}
	for name, rawMsg := range outer.Provider {
		if name == "active" || !validProviderName(name) {
			continue
		}
		var p struct {
			BaseURL    string                            `json:"base_url"`
			ApiKey     string                            `json:"api_key"`
			Protocol   string                            `json:"protocol"`
			AuthType   string                            `json:"auth_type"`
			Models     map[string]int64                  `json:"models"`
			MaxTokens  map[string]int64                  `json:"max_tokens"`
			InputTypes map[string][]string               `json:"input_types"`
			Prices     map[string]provider.ModelPrice    `json:"prices"`
			Compat     provider.OpenAICompatCapabilities `json:"openai_compat"`
			Deleted    bool                              `json:"deleted"`
			// UsageInputIncludesCache usage 口径显式标注（可选）：true = 该端点 input_tokens
			// 已含缓存（OpenAI 形状网关），false = 未含（按 Anthropic 语义相加）。
			// 缺省（nil）= 自动。见 provider.Registry.UsageInputIncludesCache。
			UsageInputIncludesCache *bool `json:"usage_input_includes_cache"`
			// CacheTTL1h 缓存断点 TTL 标注（可选）：true = 1h extended TTL，false = 5m。
			// 缺省（nil）= 自动（官方端点 1h、兼容端点 5m）。见 provider.Registry.CacheTTL1h。
			CacheTTL1h *bool `json:"cache_ttl_1h"`
		}
		if json.Unmarshal(rawMsg, &p) != nil {
			continue // 坏 provider 块跳过
		}
		// 用户已删除（内置 5 家删除标记）：从内存预置表移除（重启不复活）。
		// 自定义 provider 是物理删除（settings 无此 key），不会走到这里。
		if p.Deleted {
			delete(pc.baseURL, name)
			delete(pc.apiKey, name)
			delete(pc.models, name)
			delete(pc.maxTokens, name)
			delete(pc.inputTypes, name)
			delete(pc.protocol, name)
			delete(pc.compat, name)
			delete(pc.authType, name)
			delete(pc.usageIncl, name)
			delete(pc.cacheTTL, name)
			if pc.active == name {
				pc.active = "deepseek"
			}
			continue
		}
		if p.BaseURL != "" {
			pc.baseURL[name] = p.BaseURL
		}
		if p.ApiKey != "" {
			pc.apiKey[name] = p.ApiKey // 明文 key（settings.json 主通道）；空值清除内存旧值（避免旧 key 残留）
		} else {
			// settings 里无 key（用户删除/从未配置）：清内存旧值，回退 env 兜底。
			// 防止「面板删 key 后内存仍持旧 key → 新会话 401」。
			delete(pc.apiKey, name)
		}
		if p.Protocol == protocolResponses || p.Protocol == protocolAnthropic {
			pc.protocol[name] = p.Protocol
		} else if p.Protocol != "" {
			pc.protocol[name] = protocolChatCompletions
		}
		// auth_type（OAuth 订阅登录 2026-08）：合法值 "api_key" | "oauth"；非法回退 api_key。
		// 注意：oauth 模式仍需 preset.OAuth 支持，否则按 api_key 处理（loadSettings 无 preset 上下文，
		// 由 authFor/newProvider 在构造时二次校验）。
		if p.AuthType == "oauth" {
			pc.authType[name] = "oauth"
		} else if p.AuthType != "" {
			pc.authType[name] = "api_key"
		}
		// usage 口径标注：settings 显式配置生效；未配置则清除（回自动判定）——
		// 删除字段后必须能回到「自动」，否则旧标注会永久粘住（与 auth_type 的
		// 「空值不动」不同，这里 nil = 显式清除语义）。
		if p.UsageInputIncludesCache != nil {
			pc.usageIncl[name] = *p.UsageInputIncludesCache
		} else {
			delete(pc.usageIncl, name)
		}
		// 缓存 TTL 标注：同 usage 口径 —— nil = 显式清除（回端点默认），不能粘住旧值。
		if p.CacheTTL1h != nil {
			pc.cacheTTL[name] = *p.CacheTTL1h
		} else {
			delete(pc.cacheTTL, name)
		}
		if p.Models != nil {
			pc.models[name] = p.Models
		}
		if p.MaxTokens != nil {
			pc.maxTokens[name] = p.MaxTokens
		}
		// input_types：settings 显式配置覆盖内存表（模态门控唯一事实源；未配置保持
		// 内置默认/注册表真实模态）。前端拉取模型后 providerSave 落盘 → bridge 重启/热重载
		// 也能拿到用户勾选的模态，不再只依赖 set_provider payload。
		if p.InputTypes != nil {
			pc.inputTypes[name] = p.InputTypes
		}
		// 仅当 settings 显式含 openai_compat 时覆盖默认，保证旧配置不变。
		var compatRaw struct {
			Compat json.RawMessage `json:"openai_compat"`
		}
		if json.Unmarshal(rawMsg, &compatRaw) == nil && compatRaw.Compat != nil {
			compat := p.Compat
			if compat.BasePath == "" {
				compat.BasePath = "/v1"
			}
			pc.compat[name] = compat
		}
		// 每模型价表覆盖（Provider 设置面板配置；USD/1M tokens）：
		// 整表替换 userPriceTable（内置 priceTable 永不污染）——prices 里没有的模型 =
		// 删除覆盖回退内置价。读侧 priceFor/overrideRegistryLocked 用户价优先。
		if p.Prices != nil {
			setUserPrice(name, p.Prices)
		}
	}
}

// validProviderName provider 命名校验（内置三卡 deepseek/openai/opencode 与用户自定义共用）：
// 非空、非保留键 "active"，仅字母/数字/短横/下划线且首字符为字母（与前端添加表单同规则）。
// set_provider / settings.json 解析 / provider 增删共用。
func validProviderName(name string) bool {
	if name == "" || name == "active" {
		return false
	}
	for i, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
		if !ok {
			return false
		}
		if i == 0 && (r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func (pc *providerConfig) setActive(name string) error {
	if !validProviderName(name) {
		return fmt.Errorf("无效 provider: %s", name)
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.active = name
	return nil
}

func (pc *providerConfig) setBaseURL(name, url string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if validProviderName(name) {
		pc.baseURL[name] = url
	}
}

func (pc *providerConfig) setKey(name, key string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if validProviderName(name) {
		if key == "" {
			// 显式清空：内存旧 key 不得残留（旧 key 轮换后若不清除，
			// keyLocked 会继续返回旧值 → 401；回退 env/.env 兜底）。
			delete(pc.apiKey, name)
		} else {
			pc.apiKey[name] = key
		}
	}
}

func (pc *providerConfig) setCompat(name string, compat provider.OpenAICompatCapabilities) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if validProviderName(name) {
		pc.compat[name] = compat
	}
}

func parseCompatCapabilities(raw map[string]any) provider.OpenAICompatCapabilities {
	b, _ := json.Marshal(raw)
	var c provider.OpenAICompatCapabilities
	_ = json.Unmarshal(b, &c)
	return c
}

func (pc *providerConfig) providerName() string {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.active
}

// isOpenCodeGateway 判定当前活跃 provider 是否走 OpenCode Go 网关
// （baseURL 域名含 opencode.ai）：该网关把 chat/completions 转发给上游思考模型
// （deepseek/glm/qwen/minimax…），全部要求 assistant 的 reasoning_content 原样回传，
// 缺失即 400 "The reasoning_content in the thinking mode must be passed back to the API"。
// 因此 buildLoop 对 opencode 网关强制 RetainReasoning=true（覆盖按模型名自动判断）。
func (pc *providerConfig) isOpenCodeGateway() bool {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	u := strings.ToLower(pc.baseURL[pc.active])
	return strings.Contains(u, "opencode.ai")
}

// DeepSeek 自家采样默认（宿主决策 2026-08）：构造会话 loop 时显式传入，让 DeepSeek
// 未显式指定采样参数时按 top_p=0.95 / temperature=1.0 发请求；OpenAI 保持厂商默认（不传）。
// 注意：DeepSeek 思考模式下这两个参数会被忽略（文档口径：设置不报错但不生效）。
const (
	deepseekDefaultTemperature = 1.0
	deepseekDefaultTopP        = 0.95
)

// samplingDefaults 当前活跃 provider 的采样默认（temperature/topP；nil = 厂商默认）。
func (pc *providerConfig) samplingDefaults() (temperature, topP *float64) {
	if pc.providerName() != "deepseek" {
		return nil, nil
	}
	t, p := deepseekDefaultTemperature, deepseekDefaultTopP
	return &t, &p
}

// keyLocked 取某 provider 的 key：运行中 set_provider 写入的内存值优先；
// 其次内置三卡的既定 env（boot 时 Electron 注入 / .env）；最后自定义 provider 的
// 通用 env 兜底 GOCODE_KEY_<UPPER(name)>（Electron loadKeysEnv 对任意已存 key 生成）。
func (pc *providerConfig) keyLocked(name string) string {
	// G2 修复：加锁读 pc.apiKey（newProvider 可能在 runLoop goroutine 并发读，
	// 与 set_provider 主线程写存在竞争窗口；-race 未触发只因实际串行）。
	pc.mu.Lock()
	k := pc.apiKey[name]
	pc.mu.Unlock()
	if k != "" {
		return k
	}
	switch name {
	case "openai":
		return strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	case "opencode":
		return strings.TrimSpace(os.Getenv("OPENCODE_API_KEY"))
	case "kimi":
		if v := strings.TrimSpace(os.Getenv("KIMI_API_KEY")); v != "" {
			return v
		}
		return strings.TrimSpace(os.Getenv("MOONSHOT_API_KEY"))
	case "zhipu":
		return strings.TrimSpace(os.Getenv("ZHIPUAI_API_KEY"))
	}
	if v := strings.TrimSpace(os.Getenv("DEEPSEEK_API_TOKEN")); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("GOCODE_KEY_" + strings.ToUpper(name)))
}

// setProtocol 设置某 provider 的接入协议（set_provider 热切换；payload 兜底 settings.json 落盘时序）。
// 仅接受合法值：responses/anthropic 显式记录；其余归 chat_completions。非法值由 newProvider 拒绝构造。
func (pc *providerConfig) setProtocol(name, protocol string) {
	if !validProviderName(name) || protocol == "" {
		return
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	switch protocol {
	case protocolResponses, protocolAnthropic:
		pc.protocol[name] = protocol
	default:
		pc.protocol[name] = protocolChatCompletions
	}
}

// setAuthType 设置某 provider 的鉴权方式（oauth_login / set_provider 热切换）。
// 合法值 "api_key" | "oauth"；其余忽略。
func (pc *providerConfig) setAuthType(name, authType string) {
	if !validProviderName(name) {
		return
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	switch authType {
	case "oauth":
		pc.authType[name] = "oauth"
	case "api_key":
		pc.authType[name] = "api_key"
	}
}

// authTypeOf 取某 provider 的鉴权方式（缺省 api_key）。
func (pc *providerConfig) authTypeOf(name string) string {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.authType[name] == "oauth" {
		return "oauth"
	}
	return "api_key"
}

// oauthFlowOf 取某 provider 的 OAuth flow（懒构造；无 OAuth 支持返回 nil）。
func (pc *providerConfig) oauthFlowOf(name string) auth.Flow {
	pc.mu.Lock()
	if f, ok := pc.oauthFlow[name]; ok {
		pc.mu.Unlock()
		return f
	}
	pc.mu.Unlock()

	// 锁外查预设（只读，无锁安全）
	preset := provider.FindProviderPreset(name)
	if preset == nil || preset.OAuth == nil {
		pc.mu.Lock()
		pc.oauthFlow[name] = nil // 负缓存
		pc.mu.Unlock()
		return nil
	}
	f, err := oauth.NewFlow(preset.OAuth.Flow)
	if err != nil {
		pc.mu.Lock()
		pc.oauthFlow[name] = nil
		pc.mu.Unlock()
		return nil
	}
	pc.mu.Lock()
	pc.oauthFlow[name] = f
	pc.mu.Unlock()
	return f
}

// oauthTransportOf 取某 provider 的 OAuth 注入 transport（懒构造；无 OAuth 支持返回 nil）。
// transport 把 access token 注入 HTTP 头（Bearer / Copilot 专用头），三协议子包零改动。
func (pc *providerConfig) oauthTransportOf(name string) *auth.Transport {
	pc.mu.Lock()
	if tr, ok := pc.oauthTrans[name]; ok {
		pc.mu.Unlock()
		return tr
	}
	pc.mu.Unlock()

	flow := pc.oauthFlowOf(name)
	if flow == nil {
		return nil
	}
	tr := auth.NewTransport(pc.oauthStore, flow, name, nil)
	pc.mu.Lock()
	pc.oauthTrans[name] = tr
	pc.mu.Unlock()
	return tr
}

// oauthLoggedIn 某 provider 是否已 OAuth 登录（有有效凭证）。
func (pc *providerConfig) oauthLoggedIn(name string) bool {
	cred, err := pc.oauthStore.Read(name)
	return err == nil && cred != nil && cred.Valid()
}

// providerLLMHTTPClient 统一 LLM HTTP client：ResponseHeaderTimeout = **次级兜底**
// （策略本体是 agents 层的首字预算 30s，见 agents.Config.FirstChunkTimeout —— 那个
// 计时覆盖「请求发出 → 首个 provider 事件」，且首个事件到达即解除）。
// 本层 60s 只在「等响应头」这一段生效（拿不到响应头时兜底；比首字预算宽，正常情况下
// 不会先触发，留着防 provider 侧把事件吞掉的情形）。响应头到达后流式进行中不再计时
// （长 reasoning/生成不受限）。三协议（chat-completions / responses / anthropic）全部
// 经此 client 发请求。注意：不做 client.Timeout（那是整请求总时限，会误杀健康长流式）。
const llmFirstPacketTimeout = 60 * time.Second

func llmHTTPClient(base http.RoundTripper) *http.Client {
	if base == nil {
		base = http.DefaultTransport
	}
	// 只对 *http.Transport 克隆并设 ResponseHeaderTimeout；自定义 RoundTripper
	//（OAuth/mock/日志包装）原样保留 —— 其自身超时语义由实现决定。
	if tr, ok := base.(*http.Transport); ok {
		clone := tr.Clone()
		if clone.ResponseHeaderTimeout <= 0 || clone.ResponseHeaderTimeout > llmFirstPacketTimeout {
			clone.ResponseHeaderTimeout = llmFirstPacketTimeout
		}
		return &http.Client{Transport: clone}
	}
	return &http.Client{Transport: base}
}

// newProvider 构造某 provider（三协议分发）：chat-completions / responses / anthropic。
// 依赖注入 registry（模型元数据真实常量 + settings 覆盖）。
// OAuth 模式：access token 经 auth.Transport 注入 HTTP 层（三协议子包零改动）；
// Copilot 等动态端点由 transport 的 ToAuth.BaseURL 覆盖 baseURL。
func (pc *providerConfig) newProvider(name string) provider.Provider {
	pc.mu.Lock()
	protocol := pc.protocol[name]
	baseURL := pc.baseURL[name]
	pc.mu.Unlock()

	deps := provider.Dependencies{Registry: pc.registry}

	// OAuth 订阅登录（2026-08）：auth_type=oauth 且该 provider 支持 OAuth → transport 注入
	oauthMode := pc.authTypeOf(name) == "oauth" && pc.oauthFlowOf(name) != nil
	apiKey := pc.keyLocked(name)
	if oauthMode {
		if tr := pc.oauthTransportOf(name); tr != nil {
			deps.HTTPClient = llmHTTPClient(tr)
		}
		// OAuth 模式：key 由 transport 注入（Bearer/Copilot 头），构造时传空避免 SDK
		// 侧重复 Authorization；OpenRouter（KeyInstead）除外——其登录产物就是 api_key。
		preset := provider.FindProviderPreset(name)
		if preset == nil || preset.OAuth == nil || !preset.OAuth.KeyInstead {
			apiKey = ""
		}
		// 动态 baseURL（Copilot proxy-ep）：从已存凭证解析，覆盖 settings 配置
		if cred, err := pc.oauthStore.Read(name); err == nil && cred != nil && cred.Valid() {
			if flow := pc.oauthFlowOf(name); flow != nil {
				if ma, aerr := flow.ToAuth(cred); aerr == nil && ma.BaseURL != "" {
					baseURL = ma.BaseURL
				}
			}
		}
	}

	// 非 OAuth：默认走 llmHTTPClient（含 ResponseHeaderTimeout 首包超时）。
	// OAuth 分支上面已设 deps.HTTPClient（同样带首包超时）。
	if deps.HTTPClient == nil {
		deps.HTTPClient = llmHTTPClient(nil)
	}

	proto, err := provider.ParseProtocol(protocol)
	if err != nil {
		// 非法协议（settings.json 手改/旧配置残留）：拒绝构造，返回占位 provider
		// 让上层 LLM 请求报错提示，而不是静默当 chat-completions（问题十六）。
		return provider.UnsupportedProtocolProvider(err)
	}
	switch proto {
	case provider.ProtocolAnthropic:
		// Anthropic Messages（T5 已实现）：依赖注入 registry（bridge 直接调子包，避免根包循环依赖）
		return anthropic.NewAnthropicProvider(deps, baseURL, apiKey)
	case provider.ProtocolResponses:
		// OpenAI Responses（T6 已实现）：依赖注入 registry
		return responses.NewResponsesProvider(deps, baseURL, apiKey)
	default:
		// chat-completions：依赖注入 registry（模型元数据真实常量 + settings 覆盖）
		return provider.NewOpenAIProviderWithDeps(deps, baseURL, apiKey, pc.compat[name])
	}
}

// activeProvider 构造当前活跃 provider（主 loop / 子 agent loop 共用）
func (pc *providerConfig) activeProvider() provider.Provider {
	pc.mu.Lock()
	name := pc.active
	pc.mu.Unlock()
	return pc.newProvider(name)
}

func (pc *providerConfig) defaultModel() string {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.active == "deepseek" {
		return "deepseek-v4-flash-vision-exp"
	}
	// 其余 provider：取配置模型列表排序首个（与前端 defaultModelFor 一致）
	if keys := sortedModelKeys(pc.models[pc.active]); len(keys) > 0 {
		return keys[0]
	}
	switch pc.active {
	case "openai":
		return "gpt-4o"
	case "kimi":
		return "kimi-k2.6"
	case "zhipu":
		return "glm-4.6"
	}
	return "deepseek-v4-flash-vision-exp"
}

// sortedModelKeys 模型 map 的 key 升序（defaultModel 取首个用）。
func sortedModelKeys(models map[string]int64) []string {
	if len(models) == 0 {
		return nil
	}
	keys := make([]string, 0, len(models))
	for k := range models {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// windowFor 某模型上下文窗口（按活跃 Provider 取表）。兼容保留：新代码请用
// windowForIn 显式指定 Provider（问题六：跨 Provider 会话不得随 active 漂移）。
func (pc *providerConfig) windowFor(name string) int64 {
	return pc.windowForIn(pc.active, name)
}

// windowForIn 某模型在某 Provider 下的上下文窗口：
// 注册表唯一事实源（settings 显式配置经 syncRegistry/setModels Override 注入）> 默认窗口。
// 注册表 nil（测试直接构造 providerConfig）时回退旁路表（models map）。
func (pc *providerConfig) windowForIn(prov, name string) int64 {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.registry != nil {
		if w := pc.registry.ContextWindowFor(prov, name); w > 0 {
			return w
		}
	} else if w, ok := pc.models[prov][name]; ok && w > 0 {
		return w
	}
	return defaultContextWindow
}

// maxTokensFor 某模型单次输出上限：设置里显式配置优先；否则已知模型官方默认；
// 兜底 defaultMaxTokens。构造 loop 时 WithMaxTokens 取值来源（随模型切换变化）。
// 兼容保留：新代码请用 maxTokensForIn 显式指定 Provider。
func (pc *providerConfig) maxTokensFor(name string) int64 {
	return pc.maxTokensForIn(pc.active, name)
}

// maxTokensForIn 某模型在某 Provider 下的单次输出上限：
// 注册表唯一事实源（settings 显式配置经 syncRegistry/setMaxTokens Override 注入）> 默认 8192。
// 注册表 nil（测试直接构造 providerConfig）时回退旁路表（maxTokens map）。
func (pc *providerConfig) maxTokensForIn(prov, name string) int64 {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.registry != nil {
		if v := pc.registry.MaxTokensDefaultFor(prov, name); v > 0 {
			return v
		}
	} else if v, ok := pc.maxTokens[prov][name]; ok && v > 0 {
		return v
	}
	return defaultMaxTokens
}

// setUsageIncl 设置某 provider 的 usage 口径标注（nil = 清除 → 回自动判定）。
// settings.json 的持久化由前端 saveProvider 负责（与 auth_type 同范式）；本方法只做
// 内存 + 注册表热应用（下一轮请求即生效，不依赖落盘时序）。
func (pc *providerConfig) setUsageIncl(name string, inclusive *bool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if !validProviderName(name) {
		return
	}
	if inclusive == nil {
		delete(pc.usageIncl, name)
	} else {
		pc.usageIncl[name] = *inclusive
	}
	pc.overrideRegistryLocked(name)
}

// setCacheTTL1h 设置某 provider 的缓存 TTL 标注（nil = 清除 → 回端点默认）。
// 与 setUsageIncl 同范式：只做内存 + 注册表热应用，settings 持久化由前端 saveProvider 负责。
func (pc *providerConfig) setCacheTTL1h(name string, oneHour *bool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if !validProviderName(name) {
		return
	}
	if oneHour == nil {
		delete(pc.cacheTTL, name)
	} else {
		pc.cacheTTL[name] = *oneHour
	}
	pc.overrideRegistryLocked(name)
}

// cacheTTL1hOf 某 provider 的缓存 TTL 标注（第二返回值 = 是否已标注）。
func (pc *providerConfig) cacheTTL1hOf(name string) (bool, bool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	v, ok := pc.cacheTTL[name]
	return v, ok
}

// usageInclOf 某 provider 的 usage 口径标注（第二返回值 = 是否已标注）。
func (pc *providerConfig) usageInclOf(name string) (bool, bool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	v, ok := pc.usageIncl[name]
	return v, ok
}

// setMaxTokens 设置某 provider 的模型 max_tokens map（set_provider 命令热切换，避免
// 依赖 settings.json 落盘时序）。同时同步注册表（唯一事实源：windowForIn/maxTokensForIn
// 只查注册表，见 syncRegistry 同源逻辑）。
func (pc *providerConfig) setMaxTokens(name string, tokens map[string]int64) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if validProviderName(name) {
		pc.maxTokens[name] = tokens
		pc.overrideRegistryLocked(name)
	}
}

// setModels 设置某 provider 的模型窗口 map（set_provider 命令热切换，避免依赖
// settings.json 落盘时序——窗口改动即时生效）。同时同步注册表（唯一事实源）。
func (pc *providerConfig) setModels(name string, models map[string]int64) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if validProviderName(name) {
		pc.models[name] = models
		pc.overrideRegistryLocked(name)
	}
}

// setPrices 设置某 provider 的每模型价表覆盖（set_provider 命令热切换解析 prices payload；
// USD/1M tokens）。价表数据不入 providerConfig 旁路表——整表替换 userPriceTable
// （与内置 priceTable 分离：删除覆盖 = 表内无该模型），再 overrideRegistryLocked 注入
// 注册表 info.Cost，使事件层（FillUsageCost 走 registry.PriceFor）与宿主层
// （costUsd 走 priceFor：用户价优先）一致。调用方须先 loadSettings 落盘
// （set_provider 已保证落盘在前，payload 兜底时序）。
func (pc *providerConfig) setPrices(name string, prices map[string]provider.ModelPrice) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if !validProviderName(name) {
		return
	}
	setUserPrice(name, prices)
	pc.overrideRegistryLocked(name)
}

// setInputTypes 设置某 provider 的 input_types（set_provider 热切换解析 input_types payload；
// 前端 Provider 面板逐模型勾选图片 → ["text","image"]）。同时同步注册表（唯一事实源）。
func (pc *providerConfig) setInputTypes(name string, types map[string][]string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if validProviderName(name) {
		pc.inputTypes[name] = types
		pc.overrideRegistryLocked(name)
	}
}

// syncRegistry 把 settings 的窗口/上限/价表/input_types 覆盖注入注册表（用户配置 > 内置常量）。
// 语义：以注册表内置真实数据为基底，只覆盖用户显式设置的字段（merge，不抹掉内置能力）。
// 调用点：loadProviderConfig（启动）+ set_provider / reload_settings（热更新）。
func (pc *providerConfig) syncRegistry() {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.registry == nil {
		pc.registry = provider.NewRegistry() // 零值构造兜底（测试直接 new providerConfig）
	}
	for name := range pc.baseURL {
		pc.overrideRegistryLocked(name)
	}
}

// overrideRegistryLocked 把某 provider 的旁路表（models/maxTokens/inputTypes）覆盖注入注册表。
// 必须在 pc.mu 持锁下调用（setModels/setMaxTokens/setInputTypes/syncRegistry 共用）。
// 注册表是窗口/上限/模态的唯一事实源——windowForIn/maxTokensForIn 只查注册表，
// 旁路表仅保留「模型存在性」用途（defaultModel/hasModel/providerForModel）。
// 遍历模型集合 = models ∪ maxTokens ∪ inputTypes（用户只配模态/上限但 models 表缺该
// 模型时也覆盖注册表，避免模态/上限丢失）。
func (pc *providerConfig) overrideRegistryLocked(name string) {
	if pc.registry == nil {
		pc.registry = provider.NewRegistry()
	}
	// usage 口径标注：显式配置 → 注入注册表（provider 级，先于模型级查询）；未配置 → 清除，
	// 回「协议默认 + 响应形状」判定。nil 与 false 语义不同，必须逐次同步而不是「只写不清」。
	if v, ok := pc.usageIncl[name]; ok {
		b := v
		pc.registry.SetProviderUsageInputIncludesCache(name, &b)
	} else {
		pc.registry.SetProviderUsageInputIncludesCache(name, nil)
	}
	// 缓存 TTL 标注：同语义（未配置 → 清除，回端点默认：官方 1h / 兼容 5m）。
	if v, ok := pc.cacheTTL[name]; ok {
		b := v
		pc.registry.SetProviderCacheTTL1h(name, &b)
	} else {
		pc.registry.SetProviderCacheTTL1h(name, nil)
	}
	// 收集模型集合（并集；map 无序 → 排序保证确定性）
	seen := map[string]bool{}
	for m := range pc.models[name] {
		seen[m] = true
	}
	for m := range pc.maxTokens[name] {
		seen[m] = true
	}
	for m := range pc.inputTypes[name] {
		seen[m] = true
	}
	models := make([]string, 0, len(seen))
	for m := range seen {
		models = append(models, m)
	}
	sort.Strings(models)
	for _, model := range models {
		// 基底：内置真实数据（窗口/上限/价/推理/模态/思考能力），用户设置只覆盖对应字段。
		// 用 RegistryLookupBuiltin（跳过 override）而非 Lookup —— Lookup 优先返回上次
		// override 残留（删价格条目后基底价错误地仍是旧用户价，无法回退内置）。
		info, builtinOK := provider.RegistryLookupBuiltin(name, model)
		if !builtinOK {
			info, _ = pc.registry.Lookup(name, model) // 非内置模型（自定义 provider 手加）：兜底当前值
			// 网关别名模型（commandcode 等把上游模型以 "deepseek/..." 形式暴露）没有内置
			// 元数据：零值 Reasoning=false 会盖掉 ShouldRetainReasoning/RequiresReasoningContent
			// 的模型名兜底 —— 既不回传推理、也不补 reasoning_content 占位，上游一旦无法
			// 自补即 400（2026-09-10 command-code-2 + deepseek/deepseek-v4.1-flash 实证：
			// 工具轮缺字段 6/6 400、空格占位 200）。按名称族补全。
			if !info.Reasoning {
				info.Reasoning = provider.AutoRetainReasoning(model)
			}
			if info.Reasoning && !info.RequiresReasoningContent {
				info.RequiresReasoningContent = provider.RequiresReasoningContentByName(model)
			}
		}
		info.Provider = name
		info.ID = model
		if window, ok := pc.models[name][model]; ok && window > 0 {
			info.ContextWindow = window
		}
		if mt, ok := pc.maxTokens[name]; ok {
			if v, ok := mt[model]; ok && v > 0 {
				info.MaxTokens = v
			}
		}
		// 价表：用户显式覆盖（priceFor 命中 userPriceTable/内置宿主表）才写 info.Cost；
		// 未覆盖时**保留基底内置价**（RegistryLookupBuiltin 已带 providers.json 真实价，
		// 不在此抹零）。修复：此前对自定义/未覆盖模型以零值 ModelInfo 整体 Override，
		// 导致注册表 Lookup 恒优先命中 override 零值 → FillUsageCost/costUsd 全 0。
		if price, ok := priceFor(name, model); ok {
			info.Cost = provider.ModelPrice{Input: price.Input, CacheRead: price.CacheRead, CacheWrite: price.CacheWrite, Output: price.Output}
		}
		// input_types（前端透传，bridge 现开始消费）：显式配置 → 覆盖 Inputs/SupportsImage；
		// 未配置 → 保持内置真实模态
		if it, ok := pc.inputTypes[name]; ok {
			if types, ok := it[model]; ok && len(types) > 0 {
				info.Inputs = types
				info.SupportsImage = containsStr(types, "image")
			}
		}
		pc.registry.Override(info)
	}
}

// modelCapabilities 返回模型能力（注册表真实数据；未知模型走默认）—— list_models 响应附带。
// 含 cost（$/M tokens 价表：内置 providers.json 价 + 用户覆盖）—— 前端价格编辑器
// 空覆盖时回显内置价（此前不返回 → UI 只能显示占位空/0，误以为"内置价丢失"）。
// 网关别名（command-code 的 "xiaomi/mimo-*"、"deepseek/…"）走 LookupAlias 兜底：
// 否则「拉取模型列表」会把上游 1M 窗口 / 128K 输出的模型填成默认窗口 / 8192 上限
// （实测：command-code 的 xiaomi 系模型拉下来全是 128000/8192），拉完还得手改。
func (pc *providerConfig) modelCapabilities(prov string, models []string) map[string]any {
	out := map[string]any{}
	reg := pc.registry
	if reg == nil {
		reg = provider.NewRegistry()
	}
	for _, m := range models {
		info, ok := reg.Lookup(prov, m)
		if !ok {
			info, ok = reg.LookupAlias(prov, m)
		}
		if !ok {
			out[m] = map[string]any{
				"inputs": []string{"text"}, "context_window": defaultContextWindow, "max_tokens": defaultMaxTokens,
			}
			continue
		}
		caps := map[string]any{
			"inputs":         info.Inputs,
			"context_window": info.ContextWindow,
			"max_tokens":     info.MaxTokens,
			"reasoning":      info.Reasoning,
		}
		if info.Cost.Input > 0 || info.Cost.Output > 0 || info.Cost.CacheRead > 0 || info.Cost.CacheWrite > 0 {
			caps["cost"] = map[string]any{
				"input": info.Cost.Input, "cache_read": info.Cost.CacheRead,
				"cache_write": info.Cost.CacheWrite, "output": info.Cost.Output,
			}
		}
		out[m] = caps
	}
	return out
}

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// baseURLOf 某 provider 的 base_url（价表刷新按端点匹配实时价源用）。
func (pc *providerConfig) baseURLOf(name string) string {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.baseURL[name]
}

// configuredModels 已配置的模型清单（provider → models 表的 key，排序）—— 价表刷新只处理
// 用户真正在用的模型，不把整个上游模型表灌进用户价表。
func (pc *providerConfig) configuredModels(name string) []string {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	out := make([]string, 0, len(pc.models[name]))
	for m := range pc.models[name] {
		out = append(out, m)
	}
	sort.Strings(out) // map 遍历无序 → 排序保证确定性
	return out
}

func (pc *providerConfig) hasModel(name string) bool {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	_, ok := pc.models[pc.active][name]
	return ok
}

// hasModelIn 某指定 Provider 下是否有该模型（switch_model 携带显式 provider 时用）。
func (pc *providerConfig) hasModelIn(prov, name string) bool {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	_, ok := pc.models[prov][name]
	return ok
}

// providerForModel 在所有已配置 Provider 的模型表里查找该模型所属的 Provider；找不到返回空串。
// 用于让「对话窗口切模型」不受「活跃 Provider」限制：模型来自哪个 Provider 就以它为准（并自动切活跃）。
// 有多个 Provider 配置同名模型时返回第一个（排序稳定）。
func (pc *providerConfig) providerForModel(name string) string {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	keys := make([]string, 0, len(pc.models))
	for prov := range pc.models {
		keys = append(keys, prov)
	}
	sort.Strings(keys)
	for _, prov := range keys {
		if _, ok := pc.models[prov][name]; ok {
			return prov
		}
	}
	return ""
}

// listModels 拉取某 provider 的模型列表（用其 key + baseURL）
func (pc *providerConfig) listModels(name string) ([]string, error) {
	pc.mu.Lock()
	base := pc.baseURL[name]
	compat := pc.compat[name]
	pc.mu.Unlock()
	// keyLocked 自带锁（G2 修复）；不能在持 pc.mu 时调用（死锁：Mutex 不可重入）
	key := pc.keyLocked(name)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// OAuth 模式：用 OAuth 鉴权拉模型（token 经 transport 注入；OpenRouter KeyInstead 走 api_key）
	if pc.authTypeOf(name) == "oauth" && pc.oauthFlowOf(name) != nil {
		if cred, err := pc.oauthStore.Read(name); err == nil && cred != nil && cred.Valid() {
			// Copilot 特殊：模型清单来自 /models 的 Copilot 专用格式（policy 过滤 + 专用头）
			if name == "github-copilot" {
				return oauth.FetchCopilotModels(ctx, cred, "")
			}
			if tr := pc.oauthTransportOf(name); tr != nil {
				client := &http.Client{Transport: tr}
				// 动态 baseURL（Copilot proxy-ep 之外；一般无需）
				url := base
				if flow := pc.oauthFlowOf(name); flow != nil {
					if ma, aerr := flow.ToAuth(cred); aerr == nil && ma.BaseURL != "" {
						url = ma.BaseURL
					}
				}
				return provider.ListModelsWithClient(ctx, client, url, compat)
			}
		}
		return nil, fmt.Errorf("provider %s has not completed OAuth login (sign in from Settings)", name)
	}
	if key == "" {
		return nil, fmt.Errorf("provider %s has no API key configured (set it in Settings)", name)
	}
	return provider.ListModelsWithCapabilities(ctx, base, key, compat)
}

// testConnection 测试一个待配置（未保存）的 provider 连接：用给定 baseURL + key 发
// GET /models，校验端点连通 + Key 有效；10s 超时防挂死。瞬态——不写入 providerConfig。
func (pc *providerConfig) testConnection(baseURL, apiKey string) ([]string, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("no API key configured (enter it in Settings first)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return provider.ListModelsWithCapabilities(ctx, baseURL, apiKey, provider.MinimalOpenAICompatCapabilities())
}
