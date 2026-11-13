package agenthub

import "time"

// Activity 是群内运行态的活动投影（工具行/压缩/状态），独立于主日志。
// AfterSeq 表示它跟随在哪条群消息之后；UI 按这个位置插入折叠行。
type Activity struct {
	GroupID  GroupID   `json:"group_id"`
	AgentID  AgentID   `json:"agent_id,omitempty"`
	RunID    string    `json:"run_id,omitempty"`
	AfterSeq int64     `json:"after_seq"`
	Kind     string    `json:"kind"` // tool | compress | status
	Name     string    `json:"name,omitempty"`
	Status   string    `json:"status,omitempty"`
	Summary  string    `json:"summary,omitempty"`
	Ms       int64     `json:"ms,omitempty"`
	At       time.Time `json:"at"`
}

// TaskEvent 是任务账本的追加事件，用于恢复与审计；Task 本身是快照。
type TaskEvent struct {
	Seq    int64          `json:"seq"` // 每任务单调递增；追加时由 Store 分配
	TaskID TaskID         `json:"task_id"`
	Type   string         `json:"type"` // created | dispatched | running | completed | ...
	At     time.Time      `json:"at"`
	Data   map[string]any `json:"data,omitempty"`
}

// TaskFilter 是任务列表的过滤条件；零值 = 不过滤。
type TaskFilter struct {
	GroupID    GroupID
	AgentID    AgentID
	Status     TaskStatus
	RootTaskID TaskID
}

// DefaultNeedTTL 是未处理审批/提问的惰性过期窗口（§5.5）。
const DefaultNeedTTL = 30 * time.Minute

// Need 是 Needs you 的一条待处理项：审批/提问/交接/失败/预算。
type Need struct {
	ID     NeedID `json:"id"`
	Kind   string `json:"kind"`   // approval | question | handoff | failure | budget
	Status string `json:"status"` // pending | resolved | expired
	// AgentID 是「谁在等」：Hub 会话即该智能体；Project 会话取绑定 hub_task 的
	// origin.agent_id（场景2），这样 hub_need_list{agent_id} 也能查到项目侧审批。
	AgentID   AgentID `json:"agent_id,omitempty"`
	GroupID   GroupID `json:"group_id,omitempty"`
	TaskID    TaskID  `json:"task_id,omitempty"`
	SessionID string  `json:"session_id,omitempty"`
	Workspace string  `json:"workspace,omitempty"`
	// 归属（场景2）：这条审批/提问是从哪个 Hub 智能体/群发起的、落在哪个目标
	// 会话上、属于哪个 run。Project 会话本身没有 Hub 身份，由 recordHubNeed 从
	// 绑定的 hub_task 反推；Hub 单聊/群内联按这些字段展示。全部 omitempty，
	// 旧 JSON 缺键 → 零值，展示层按空处理。
	OriginAgentID   AgentID `json:"origin_agent_id,omitempty"`
	OriginAgentName string  `json:"origin_agent_name,omitempty"` // 冗余显示名（TaskOrigin.AgentName）
	OriginKind      string  `json:"origin_kind,omitempty"`       // dm | group | human
	TargetKind      string  `json:"target_kind,omitempty"`       // workspace | agent
	TargetWorkspace string  `json:"target_workspace,omitempty"`
	TargetSessionID string  `json:"target_session_id,omitempty"`
	// RunID 是触发这条 need 的 run（审批/提问事件自带 RunId）。
	RunID string `json:"run_id,omitempty"`
	// RunName 预留给子智能体归属：子智能体与主会话共用 approvalWaiter/QuestionWaiter，
	// 事件里只有 RunId 没有名字，暂不填（后续按 run 注册表接）。
	RunName string `json:"run_name,omitempty"`
	// Depth 是审批事件的嵌套深度（codemode 子调用；不是子智能体深度）。提问恒 0。
	Depth int `json:"depth,omitempty"`
	// Anchor 是决策锚：approval 的调用 id / question 的批次 id（hub_need_resolve 用）。
	Anchor     string     `json:"anchor,omitempty"`
	Title      string     `json:"title"`
	Body       string     `json:"body,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// NeedFilter 是 Needs you 列表的过滤条件；零值 = 不过滤。
type NeedFilter struct {
	AgentID AgentID
	GroupID GroupID
	Status  string
}

// Cursor 是每 (group, agent) 的两个游标；不要混用。
type Cursor struct {
	GroupID         GroupID   `json:"group_id"`
	AgentID         AgentID   `json:"agent_id"`
	HumanReadSeq    int64     `json:"human_read_seq"`
	AgentContextSeq int64     `json:"agent_context_seq"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Artifact 是登记后的产物记录（带版本/可见性），不是 TaskArtifact 摘要。
type Artifact struct {
	ID           ArtifactID `json:"id"`
	Name         string     `json:"name"`
	Path         string     `json:"path"`
	SourcePath   string     `json:"source_path,omitempty"` // 预览用真实路径（不对外展示任意路径）
	Kind         string     `json:"kind"`                  // created | modified
	Size         int64      `json:"size,omitempty"`
	Lines        int64      `json:"lines,omitempty"`
	SHA256       string     `json:"sha256,omitempty"`
	OwnerAgentID AgentID    `json:"owner_agent_id,omitempty"`
	GroupID      GroupID    `json:"group_id,omitempty"`
	Visibility   Visibility `json:"visibility"`
	Version      int        `json:"version"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// CurrentSchemaVersion 是 hub.json 的当前 schema。
const CurrentSchemaVersion = 1
