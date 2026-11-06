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
	ID        NeedID  `json:"id"`
	Kind      string  `json:"kind"`   // approval | question | handoff | failure | budget
	Status    string  `json:"status"` // pending | resolved | expired
	AgentID   AgentID `json:"agent_id,omitempty"`
	GroupID   GroupID `json:"group_id,omitempty"`
	TaskID    TaskID  `json:"task_id,omitempty"`
	SessionID string  `json:"session_id,omitempty"`
	Workspace string  `json:"workspace,omitempty"`
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
