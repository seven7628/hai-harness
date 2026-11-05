package agenthub

import "time"

// GroupID 是群组的稳定标识。
type GroupID string

// MemberRole 是群内角色。角色只管群权限，不强制工具只读（D18）。
type MemberRole string

const (
	RoleOwner    MemberRole = "owner"
	RoleMember   MemberRole = "member"
	RoleObserver MemberRole = "observer"
)

// Member 是群成员。JoinedAt 用于成员变更审计。
type Member struct {
	AgentID  AgentID    `json:"agent_id"`
	Role     MemberRole `json:"role"`
	JoinedAt time.Time  `json:"joined_at"`
}

// GroupPolicy 是群级策略。Mode 与 tools.preset 是两个独立概念：
// 前者管“要不要审批/能不能产生副作用”，后者管“模型看不看得见工具”。
type GroupPolicy struct {
	Mode             string  `json:"mode"` // auto | ask | readonly
	MaxParallelRuns  int     `json:"max_parallel_runs"`
	MaxHandoffDepth  int     `json:"max_handoff_depth"`
	MaxTurnsPerHuman int     `json:"max_turns_per_human"`      // 默认 8
	MaxMentions      int     `json:"max_mentions_per_message"` // 默认 3
	DailyBudgetUSD   float64 `json:"daily_budget_usd"`
}

// 群策略取值。默认 GroupModeAsk（D4）。
const (
	GroupModeAuto     = "auto"
	GroupModeAsk      = "ask"
	GroupModeReadonly = "readonly"
)

// EffectiveMode 把零值（""）归一到默认 ask，避免未初始化的 Group 落盘成非法模式。
// 其他数值默认（并发 3、轮次 8、mention 3）由 P0-03 的默认值/校验统一处理。
func (p GroupPolicy) EffectiveMode() string {
	switch p.Mode {
	case GroupModeAuto, GroupModeAsk, GroupModeReadonly:
		return p.Mode
	default:
		return GroupModeAsk
	}
}

// Group 是群组元数据。群头像不落盘：由成员 accent/avatar 派生。
type Group struct {
	ID           GroupID     `json:"id"`
	Name         string      `json:"name"`
	OwnerAgentID AgentID     `json:"owner_agent_id,omitempty"`
	Members      []Member    `json:"members"`
	Policy       GroupPolicy `json:"policy"`
	Pinned       bool        `json:"pinned"`
	Deleted      bool        `json:"deleted,omitempty"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
}

// Scope 是智能体会话的作用域：DM 或某个群。
// 它是 session_id（hub.<agent>.dm / hub.<agent>.g.<gid>）的来源，不是群组本身。
type Scope struct {
	Kind    string  `json:"kind"` // dm | group
	GroupID GroupID `json:"group_id,omitempty"`
}

const (
	ScopeDM    = "dm"
	ScopeGroup = "group"
)

// SessionID 返回该作用域下的稳定 session_id（§6.1.4）。
// 非法/空 group 作用域返回空串，由调用方拒绝，而不是拼出一个看似合法的 id。
func (s Scope) SessionID(agentID AgentID) string {
	switch s.Kind {
	case ScopeDM:
		return "hub." + string(agentID) + ".dm"
	case ScopeGroup:
		if s.GroupID == "" {
			return ""
		}
		return "hub." + string(agentID) + ".g." + string(s.GroupID)
	default:
		return ""
	}
}
