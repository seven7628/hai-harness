package agenthub

import "time"

// AgentID 是智能体的稳定标识。展示名可改，ID 不可改。
type AgentID string

// Avatar 是智能体头像。图片头像存 agents/<id>/avatar.png，不把二进制塞进 profile.json。
type Avatar struct {
	Kind  string `json:"kind"`           // emoji | image
	Value string `json:"value"`          // emoji
	BG    string `json:"bg,omitempty"`   // emoji 背景色（#rrggbb）
	Path  string `json:"path,omitempty"` // image: agents/<id>/avatar.png
}

// MemoryPrefs 是智能体自己的记忆偏好。记忆正文在 agents/<id>/memory，不进 profile。
type MemoryPrefs struct {
	Enabled       bool `json:"enabled"`
	MaxIndexLines int  `json:"max_index_lines,omitempty"` // 默认 200
}

// MCPPolicy 是 MCP 的 server/tool 级开关。零值 = 全开。
//
// 注意：MCP 工具名是 mcp__<server>__<tool>；DisableTools 用 "server.tool" 或 glob，
// 避免把协议前缀泄漏到用户配置里。
type MCPPolicy struct {
	Disable        bool     `json:"disable,omitempty"`         // true = 全部 MCP 不注册
	DisableServers []string `json:"disable_servers,omitempty"` // server 名
	DisableTools   []string `json:"disable_tools,omitempty"`   // "server.tool" / glob
	AllowTools     []string `json:"allow_tools,omitempty"`     // 非空 = 只允许这些
}

// ToolPolicy 是智能体的工具面开关。
//
// 零值 = 全开：安全靠 cwd/沙箱/审批/预算，不靠默认隐藏工具。新增工具默认可见，
// 不会因为忘记加字段而被静默关闭（D18）。
type ToolPolicy struct {
	Preset         string    `json:"preset,omitempty"`  // all | readonly | custom；空 = all
	Disable        []string  `json:"disable,omitempty"` // 精确名或前缀：browser_*、mcp__github__*
	Allow          []string  `json:"allow,omitempty"`   // custom 时白名单；空 = 全部
	BashRoots      []string  `json:"bash_roots,omitempty"`
	DisableNetwork bool      `json:"disable_network,omitempty"`
	MCP            MCPPolicy `json:"mcp"`
}

// 工具预设取值。空字符串等价于 ToolPresetAll。
const (
	ToolPresetAll      = "all"
	ToolPresetReadonly = "readonly"
	ToolPresetCustom   = "custom"
)

// RunPolicy 是智能体“能对世界做什么”的权限与预算。
type RunPolicy struct {
	CanCreateWorkspace bool     `json:"can_create_workspace"`
	CanDispatch        bool     `json:"can_dispatch"`
	CanHandoff         bool     `json:"can_handoff"`
	CanShareArtifact   bool     `json:"can_share_artifact"`
	WorkspaceRoots     []string `json:"workspace_roots,omitempty"`
	MaxTaskCostUSD     float64  `json:"max_task_cost_usd"`
	DailyBudgetUSD     float64  `json:"daily_budget_usd"`
	MaxAutoTurns       int      `json:"max_auto_turns"`
}

// AgentProfile 是智能体的全部可配置状态。运行态（session/run）不在里面。
//
// 注意：Provider/Model 是显式装配的，不依赖全局 active provider。
type AgentProfile struct {
	ID                AgentID     `json:"id"`
	Name              string      `json:"name"`
	Avatar            Avatar      `json:"avatar"`
	Accent            string      `json:"accent"` // #rrggbb；名字/chip/状态环的唯一识别色
	Role              string      `json:"role"`
	Description       string      `json:"description"`
	Instructions      string      `json:"instructions"`
	Persona           string      `json:"persona"`
	Provider          string      `json:"provider"`
	Model             string      `json:"model"`
	Effort            string      `json:"effort"`
	CompressThreshold float64     `json:"compress_threshold"`
	Memory            MemoryPrefs `json:"memory"`
	Tools             ToolPolicy  `json:"tools"`
	Policy            RunPolicy   `json:"policy"`
	Version           int         `json:"version"`
	Deleted           bool        `json:"deleted,omitempty"` // 软删；DM/群记录与记忆保留
	CreatedAt         time.Time   `json:"created_at"`
	UpdatedAt         time.Time   `json:"updated_at"`
}
