package agenthub

import "time"

// TaskID 是任务账本的稳定标识。重试会新建 Task，parent 指向旧任务。
type TaskID string

// TaskStatus 是任务账本状态。delivered 是结果已回推后的终态。
type TaskStatus string

const (
	TaskQueued        TaskStatus = "queued"
	TaskDispatched    TaskStatus = "dispatched"
	TaskRunning       TaskStatus = "running"
	TaskCompleted     TaskStatus = "completed"
	TaskFailed        TaskStatus = "failed"
	TaskInterrupted   TaskStatus = "interrupted"
	TaskDelivered     TaskStatus = "delivered"
	TaskCanceled      TaskStatus = "canceled"
	TaskBlockedBudget TaskStatus = "blocked_budget"
	TaskDeclined      TaskStatus = "declined"
)

// TaskTarget 是任务落到哪里：项目工作区会话，或群内另一个智能体。
type TaskTarget struct {
	Kind      string  `json:"kind"` // workspace | agent
	Workspace string  `json:"workspace,omitempty"`
	SessionID string  `json:"session_id,omitempty"`
	AgentID   AgentID `json:"agent_id,omitempty"`
	GroupID   GroupID `json:"group_id,omitempty"`
}

const (
	TargetWorkspace = "workspace"
	TargetAgent     = "agent"
)

// TaskOrigin 是任务由谁发起。Kind 是 dm | group | human，不是消息的 SenderKind。
type TaskOrigin struct {
	Kind       string  `json:"kind"`
	GroupID    GroupID `json:"group_id,omitempty"`
	AgentID    AgentID `json:"agent_id,omitempty"`
	AgentName  string  `json:"agent_name,omitempty"` // 冗余显示名，避免跨端查
	MessageSeq int64   `json:"message_seq,omitempty"`
}

const (
	OriginDM    = "dm"
	OriginGroup = "group"
	OriginHuman = "human"
)

// Dispatch 记录一次派发的时间线，用于恢复与成本归因。
type Dispatch struct {
	DispatchedAt time.Time `json:"dispatched_at"`
	AcceptedAt   time.Time `json:"accepted_at"`
	RunID        string    `json:"run_id,omitempty"`
	AgentStartAt time.Time `json:"agent_start_at,omitempty"`
	AgentEndAt   time.Time `json:"agent_end_at,omitempty"`
}

// Usage 是任务卡展示用的用量快照。
//
// 刻意不复用 core.Usage：core 的字段没有 JSON 小写标签，直接放进账本会污染协议。
// bridge 在结算时把 core.Usage 映射成这个 DTO。
type Usage struct {
	Input        int64   `json:"input"`
	Output       int64   `json:"output"`
	CacheRead    int64   `json:"cache_read"`
	CacheWrite   int64   `json:"cache_write"`
	CacheWrite1h int64   `json:"cache_write_1h"`
	Reasoning    int64   `json:"reasoning"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

// Artifact 是任务产物清单项。全文按 artifact id/路径读取，账本只存摘要。
type Artifact struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Kind    string `json:"kind"` // created | modified
	Added   int    `json:"added,omitempty"`
	Removed int    `json:"removed,omitempty"`
	Size    int64  `json:"size,omitempty"`
}

// TaskResult 是任务终态的权威结果。
type TaskResult struct {
	FinishReason string     `json:"finish_reason,omitempty"`
	Summary      string     `json:"summary,omitempty"`
	Artifacts    []Artifact `json:"artifacts,omitempty"`
	Usage        Usage      `json:"usage"`
	Error        string     `json:"error,omitempty"`
}

// Task 是 Hub 的任务账本条目。一条链的根是 RootTaskID，护栏按链聚合。
type Task struct {
	ID              TaskID      `json:"id"`
	RootTaskID      TaskID      `json:"root_task_id"`
	ParentTaskID    TaskID      `json:"parent_task_id,omitempty"`
	Chain           []TaskID    `json:"chain"`
	Origin          TaskOrigin  `json:"origin"`
	Target          TaskTarget  `json:"target"`
	Title           string      `json:"title"`
	Instruction     string      `json:"instruction"`
	Callback        string      `json:"callback"`
	Status          TaskStatus  `json:"status"`
	Attempt         int         `json:"attempt"`
	Dispatch        Dispatch    `json:"dispatch"`
	Result          *TaskResult `json:"result,omitempty"`
	Error           string      `json:"error,omitempty"`
	NotifiedRuns    []string    `json:"notified_run_ids,omitempty"`
	HumanIntervened bool        `json:"human_intervened"`
	CreatedAt       time.Time   `json:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
}

const (
	CallbackAgentEnd = "agent_end"
	CallbackNone     = "none"
)
