package agenthub

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// GroupID 是群组的稳定标识。
type GroupID string

// MemberRole 是群内角色。角色只管群权限（谁能 @all / 转让群主），不强制工具只读（D18）：
// 「只读」是群策略 GroupPolicy.Mode（readonly）的事，不是角色的事。
//
// P6-12：observer 角色已删除 —— 「只读旁听」与群 readonly 模式是两套机制，留着角色会让
// 同一件事有两个开关，而且 observer 的每个特判（不能发言/不能交接/不能派活）都要单独维护。
// 旧 JSON 里的 observer 在**加载时**迁移成 member（MigrateLegacyMemberRoles），
// 成员关系与历史都不动；新写入带 observer 一律拒绝（SaveGroup）。
type MemberRole string

const (
	RoleOwner  MemberRole = "owner"
	RoleMember MemberRole = "member"
)

// legacyRoleObserver 是已删除的角色值：只在迁移/拒绝两处出现，不再是一等角色。
const legacyRoleObserver = "observer"

// Member 是群成员。JoinedAt 用于成员变更审计。
type Member struct {
	AgentID  AgentID    `json:"agent_id"`
	Role     MemberRole `json:"role"`
	JoinedAt time.Time  `json:"joined_at"`
}

// GroupPolicy 是群级策略。Mode 与 tools.preset 是两个独立概念：
// 前者管“要不要审批/能不能产生副作用”，后者管“模型看不看得见工具”。
//
// MaxTurnsPerHuman 已废弃（deprecated，见字段注释）：字段与默认值只为兼容旧
// JSON/旧群文件而保留，生产不再用它拦截「人类发言后智能体连续发言」的条数
// （用户口径：群策略限制取消）。自动接龙由深度/同对/冷却/同内容指纹等护栏约束。
type GroupPolicy struct {
	Mode string `json:"mode"` // auto | ask | readonly
	// 并发不是策略旋钮（P6-02/D1）：一个群一个调度器，并发上限 = 本轮目标数
	// （上限 = 活跃群成员数）。旧 JSON 里的 max_parallel_runs 读入即忽略。
	MaxHandoffDepth  int     `json:"max_handoff_depth"`
	MaxTurnsPerHuman int     `json:"max_turns_per_human"`      // 已废弃，默认 8（仅兼容旧数据）
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
	// HumanReadSeq 是**人**在这个群里读到的位置（每个群一个，多端共享；§6.5.2 两个游标）。
	// 只影响左栏未读点/时间线「未读」分隔线，不影响任何智能体的上下文
	//（那是每 (group, agent) 的 Cursor.AgentContextSeq）。旧群记录没有该字段 = 0 = 全未读。
	HumanReadSeq int64     `json:"human_read_seq,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
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

// P0-03：群组默认值、角色与成员校验。
const (
	DefaultGroupMaxHandoffDepth  = 3
	DefaultGroupMaxTurnsPerHuman = 8
	// MaxGroupPolicyTurnsPerHuman 是 ReadMessages 单页窗口的上界；超过它会把
	// 限流静默退化成 500，故策略层直接拒绝而不是悄悄截断。
	MaxGroupPolicyTurnsPerHuman = 500
	DefaultGroupMaxMentions     = 3
)

// Normalize 应用群组默认值并校验；只有全部通过才原地更新，失败时对象保持原样。
func (g *Group) Normalize() error {
	if g == nil {
		return fmt.Errorf("%w: group is nil", ErrInvalid)
	}
	cp := *g
	cp.Members = append([]Member(nil), g.Members...)
	cp.ApplyDefaults()
	if err := cp.Validate(); err != nil {
		return err
	}
	*g = cp
	return nil
}

// ApplyDefaults 填零值并规范化成员；不吞掉非法 role/mode，留给 Validate。
func (g *Group) ApplyDefaults() {
	g.Name = strings.TrimSpace(g.Name)
	g.OwnerAgentID = AgentID(strings.TrimSpace(string(g.OwnerAgentID)))
	g.Policy.ApplyDefaults()

	for i := range g.Members {
		g.Members[i].AgentID = AgentID(strings.TrimSpace(string(g.Members[i].AgentID)))
		g.Members[i].Role = normalizeMemberRole(g.Members[i].Role)
		if g.Members[i].Role == "" {
			g.Members[i].Role = RoleMember
		}
		if g.Members[i].JoinedAt.IsZero() {
			g.Members[i].JoinedAt = time.Now().UTC()
		}
	}

	if g.OwnerAgentID == "" {
		var owner AgentID
		count := 0
		for _, m := range g.Members {
			if m.Role == RoleOwner {
				owner = m.AgentID
				count++
			}
		}
		if count == 1 {
			g.OwnerAgentID = owner
		}
	}
}

// normalizeMemberRole 归一化角色：去空白/小写，并把已删除的 observer 迁移成 member。
// 迁移放在这里（而不是只放 LoadGroup）是为了让「内存里构造的旧数据」也走同一条路
// —— 只要调用方 Normalize/ApplyDefaults，就不会带着一个不存在的角色继续跑。
func normalizeMemberRole(raw MemberRole) MemberRole {
	role := MemberRole(strings.ToLower(strings.TrimSpace(string(raw))))
	if role == legacyRoleObserver {
		return RoleMember
	}
	return role
}

// MigrateLegacyMemberRoles 把旧数据里的 observer 就地迁移成 member（P6-12）。
// 只改角色，不动成员顺序/JoinedAt/owner/history —— 迁移不能删人，也不能丢历史。
// 返回被迁移的成员数（0 = 无需迁移，调用方不必回写）。
func (g *Group) MigrateLegacyMemberRoles() int {
	if g == nil {
		return 0
	}
	migrated := 0
	for i := range g.Members {
		if RemovedMemberRole(g.Members[i].Role) {
			g.Members[i].Role = RoleMember
			migrated++
		}
	}
	return migrated
}

// RemovedMemberRole 判断角色是否是已删除的历史角色（P6-12：observer）。
//
// 给上层写入口用（create/member_add 在 Normalize **之前**判）：Normalize 会把旧
// observer 迁成 member，所以「先归一化再校验」会让一次显式写 observer 静默变成
// member —— 老客户端会以为建了个旁听者。写入口必须自己拒绝。
func RemovedMemberRole(role MemberRole) bool {
	return MemberRole(strings.ToLower(strings.TrimSpace(string(role)))) == legacyRoleObserver
}

// rejectRemovedMemberRoles 拒绝**新写入**已删除的角色（P6-12：observer）。
//
// 与迁移的分工：读路径（LoadGroup/ListGroups/ApplyDefaults）把旧 observer 迁成 member，
// 保证存量数据不会因为角色被删而丢成员；写路径必须拒绝，否则新代码还能造出 observer，
// 迁移就永远做不完（而且「只读旁听」会被重新引入成第二个只读开关）。
func rejectRemovedMemberRoles(g *Group) error {
	if g == nil {
		return nil
	}
	for _, m := range g.Members {
		if RemovedMemberRole(m.Role) {
			return fmt.Errorf("%w: group member %s has removed role %q（角色只剩 owner/member；旧数据在加载时迁移成 member）",
				ErrInvalid, m.AgentID, legacyRoleObserver)
		}
	}
	return nil
}

// ApplyDefaults 填 GroupPolicy 的零值（§5.9）。
func (p *GroupPolicy) ApplyDefaults() {
	if p.Mode == "" {
		p.Mode = GroupModeAsk
	}
	if p.MaxHandoffDepth == 0 {
		p.MaxHandoffDepth = DefaultGroupMaxHandoffDepth
	}
	if p.MaxTurnsPerHuman == 0 {
		p.MaxTurnsPerHuman = DefaultGroupMaxTurnsPerHuman
	}
	if p.MaxMentions == 0 {
		p.MaxMentions = DefaultGroupMaxMentions
	}
}

// RoleOf 返回成员角色；非成员/软删群 fail-closed。
func (g *Group) RoleOf(id AgentID) (MemberRole, bool) {
	if g == nil || g.Deleted {
		return "", false
	}
	for _, m := range g.Members {
		if m.AgentID == id {
			return m.Role, true
		}
	}
	return "", false
}

// Validate 校验群名、成员、角色与策略。
func (g *Group) Validate() error {
	if g == nil {
		return fmt.Errorf("%w: group is nil", ErrInvalid)
	}
	if g.ID != "" {
		if err := ValidateGroupID(g.ID); err != nil {
			return err
		}
	}
	if err := validateGroupName(g.Name); err != nil {
		return err
	}
	if err := g.Policy.Validate(); err != nil {
		return err
	}
	if len(g.Members) == 0 {
		return fmt.Errorf("%w: group must have at least one member", ErrInvalid)
	}
	if len(g.Members) > MaxGroupMembers {
		return fmt.Errorf("%w: group has %d members, limit is %d", ErrInvalid, len(g.Members), MaxGroupMembers)
	}
	if g.OwnerAgentID == "" {
		return fmt.Errorf("%w: group.owner_agent_id is required", ErrInvalid)
	}
	if err := ValidateAgentID(g.OwnerAgentID); err != nil {
		return fmt.Errorf("%w: group.owner_agent_id: %v", ErrInvalid, err)
	}

	seen := make(map[AgentID]struct{}, len(g.Members))
	owners := 0
	for _, m := range g.Members {
		if m.AgentID == "" {
			return fmt.Errorf("%w: group member agent_id is required", ErrInvalid)
		}
		if err := ValidateAgentID(m.AgentID); err != nil {
			return fmt.Errorf("%w: group member %s: %v", ErrInvalid, m.AgentID, err)
		}
		if _, dup := seen[m.AgentID]; dup {
			return fmt.Errorf("%w: duplicate group member %s", ErrInvalid, m.AgentID)
		}
		seen[m.AgentID] = struct{}{}
		switch m.Role {
		case RoleOwner, RoleMember:
		default:
			return fmt.Errorf("%w: group member %s has invalid role %q", ErrInvalid, m.AgentID, m.Role)
		}
		if m.Role == RoleOwner {
			owners++
			if m.AgentID != g.OwnerAgentID {
				return fmt.Errorf("%w: owner member %s does not match owner_agent_id %s", ErrInvalid, m.AgentID, g.OwnerAgentID)
			}
		} else if m.AgentID == g.OwnerAgentID {
			return fmt.Errorf("%w: owner_agent_id %s must have role owner", ErrInvalid, g.OwnerAgentID)
		}
	}
	if owners != 1 {
		return fmt.Errorf("%w: group must have exactly one owner, got %d", ErrInvalid, owners)
	}
	if _, ok := seen[g.OwnerAgentID]; !ok {
		return fmt.Errorf("%w: owner %s is not a group member", ErrInvalid, g.OwnerAgentID)
	}
	return nil
}

// Validate 校验群策略；零值按默认处理。
func (p GroupPolicy) Validate() error {
	mode := p.Mode
	if mode == "" {
		mode = GroupModeAsk
	}
	switch mode {
	case GroupModeAuto, GroupModeAsk, GroupModeReadonly:
	default:
		return fmt.Errorf("%w: group.policy.mode must be auto, ask or readonly", ErrInvalid)
	}
	if p.MaxHandoffDepth < 0 {
		return fmt.Errorf("%w: group.policy.max_handoff_depth must be >= 0", ErrInvalid)
	}
	if p.MaxTurnsPerHuman < 0 || p.MaxTurnsPerHuman > MaxGroupPolicyTurnsPerHuman {
		return fmt.Errorf("%w: group.policy.max_turns_per_human must be 0..%d", ErrInvalid, MaxGroupPolicyTurnsPerHuman)
	}
	if p.MaxMentions < 0 {
		return fmt.Errorf("%w: group.policy.max_mentions_per_message must be >= 0", ErrInvalid)
	}
	if p.DailyBudgetUSD < 0 || math.IsNaN(p.DailyBudgetUSD) || math.IsInf(p.DailyBudgetUSD, 0) {
		return fmt.Errorf("%w: group.policy.daily_budget_usd must be >= 0", ErrInvalid)
	}
	return nil
}

// ValidateGroupName 校验群名形状；唯一性见 ValidateGroupNameUnique。
func ValidateGroupName(name string) error {
	return validateGroupName(name)
}

// ValidateGroupNameUnique 在已有群中做大小写不敏感的重名检查。
func ValidateGroupNameUnique(name string, existing []*Group, except GroupID) error {
	if err := validateGroupName(name); err != nil {
		return err
	}
	want := strings.TrimSpace(name)
	for _, g := range existing {
		if g == nil || g.ID == except || g.Deleted {
			continue
		}
		if strings.ToLower(strings.TrimSpace(g.Name)) == strings.ToLower(want) {
			return fmt.Errorf("%w: group name %q already used by %s", ErrInvalid, want, g.ID)
		}
	}
	return nil
}

// ValidateGroupMemberNames 检查同一群内成员的显示名不重复（不区分大小写）。
// profiles 必须包含所有成员；缺失/已删除的智能体视为非法成员。
func ValidateGroupMemberNames(g *Group, profiles map[AgentID]*AgentProfile) error {
	if g == nil {
		return fmt.Errorf("%w: group is nil", ErrInvalid)
	}
	seen := make(map[string]AgentID, len(g.Members))
	for _, m := range g.Members {
		p, ok := profiles[m.AgentID]
		if !ok || p == nil {
			return fmt.Errorf("%w: group member %s has no profile", ErrInvalid, m.AgentID)
		}
		if p.Deleted {
			return fmt.Errorf("%w: group member %s is deleted", ErrInvalid, m.AgentID)
		}
		if err := validateProfileName(p.Name); err != nil {
			return err
		}
		key := strings.ToLower(strings.TrimSpace(p.Name))
		if other, dup := seen[key]; dup {
			return fmt.Errorf("%w: group member names %q and %s collide", ErrInvalid, p.Name, other)
		}
		seen[key] = m.AgentID
	}
	return nil
}

func validateGroupName(name string) error {
	name = strings.TrimSpace(name)
	n := utf8.RuneCountInString(name)
	if n < 1 || n > 32 {
		return fmt.Errorf("%w: group.name must be 1-32 characters", ErrInvalid)
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("%w: group.name must not contain control/format characters", ErrInvalid)
		}
	}
	return nil
}
