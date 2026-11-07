package agenthub

import (
	"fmt"
	"strings"
	"time"
)

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
// 时间戳用指针：零值 time.Time 不会被 omitempty 省略，会序列化成 0001-01-01。
// nil = 还没发生/未知。
type Dispatch struct {
	DispatchedAt *time.Time `json:"dispatched_at,omitempty"`
	AcceptedAt   *time.Time `json:"accepted_at,omitempty"`
	RunID        string     `json:"run_id,omitempty"`
	AgentStartAt *time.Time `json:"agent_start_at,omitempty"`
	AgentEndAt   *time.Time `json:"agent_end_at,omitempty"`
}

// Usage 是任务卡展示用的用量快照。
//
// 刻意不复用 core.Usage：core 的字段没有 JSON 小写标签，直接放进账本会污染协议。
// bridge 在结算时把 core.Usage 映射成这个 DTO。
type Usage struct {
	Input        int64 `json:"input"`
	Output       int64 `json:"output"`
	CacheRead    int64 `json:"cache_read"`
	CacheWrite   int64 `json:"cache_write"`
	CacheWrite1h int64 `json:"cache_write_1h"`
	Reasoning    int64 `json:"reasoning"`
	// CostPriced=false 表示“不知道花了多少”，不是免费（core.Cost.Priced 的语义）。
	// 预算逻辑只能累加 CostPriced=true 的 CostUSD，否则会把无价模型当成 0 成本。
	CostPriced bool    `json:"cost_priced"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
}

// TaskArtifact 是任务产物清单项。全文按 artifact id/路径读取，账本只存摘要。
// 它与 Store 里的 Artifact 记录（带 id/版本/可见性）是两回事：这里是任务结果快照。
type TaskArtifact struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Kind    string `json:"kind"` // created | modified
	Added   int    `json:"added,omitempty"`
	Removed int    `json:"removed,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Lines   int64  `json:"lines,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

// TaskResult 是任务终态的权威结果。
type TaskResult struct {
	FinishReason string         `json:"finish_reason,omitempty"`
	Summary      string         `json:"summary,omitempty"`
	Artifacts    []TaskArtifact `json:"artifacts,omitempty"`
	Usage        Usage          `json:"usage"`
	Error        string         `json:"error,omitempty"`
}

// Task 是 Hub 的任务账本条目。一条链的根是 RootTaskID，护栏按链聚合。
type Task struct {
	ID           TaskID     `json:"id"`
	RootTaskID   TaskID     `json:"root_task_id"`
	ParentTaskID TaskID     `json:"parent_task_id,omitempty"`
	Chain        []TaskID   `json:"chain"`
	Origin       TaskOrigin `json:"origin"`
	Target       TaskTarget `json:"target"`
	Title        string     `json:"title"`
	Instruction  string     `json:"instruction"`
	// ArtifactIDs 是派发时显式引用的本群产物（assign_task 的可选入参）。
	// 与 Result.Artifacts（执行产出的快照）是两回事：这里是「给对方看的输入」。
	ArtifactIDs []string `json:"artifact_ids,omitempty"`
	Callback    string   `json:"callback"`
	// ClientMsgID 是派发方给的幂等键（session_send 原样传入）；空 = 无幂等要求。
	// 与目标侧去重表共用同一个值，跨进程由账本按 (target, client_msg_id) 查找兜底。
	ClientMsgID     string      `json:"client_msg_id,omitempty"`
	Status          TaskStatus  `json:"status"`
	Attempt         int         `json:"attempt"`
	Dispatch        Dispatch    `json:"dispatch"`
	Result          *TaskResult `json:"result,omitempty"`
	Error           string      `json:"error,omitempty"`
	NotifiedRuns    []string    `json:"notified_run_ids,omitempty"`
	SupersededRuns  []string    `json:"superseded_run_ids,omitempty"`
	HumanIntervened bool        `json:"human_intervened"`
	CreatedAt       time.Time   `json:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
}

const (
	CallbackAgentEnd = "agent_end"
	CallbackNone     = "none"
)

// P0-05：任务状态机。终态 = 不再进入工作态；completed/failed/interrupted
// 仍允许一条特殊迁移到 delivered（结果已回推）。
func (s TaskStatus) Valid() bool {
	switch s {
	case TaskQueued, TaskDispatched, TaskRunning, TaskCompleted, TaskFailed,
		TaskInterrupted, TaskDelivered, TaskCanceled, TaskBlockedBudget, TaskDeclined:
		return true
	default:
		return false
	}
}

// ChainSteps 返回逻辑链长：重试 attempt 会把新 task id 追加进 Chain，
// 但这些是同一跳的重投，不应消耗链深额度（P2-02 N2）。
func (t *Task) ChainSteps() int {
	if t == nil {
		return 0
	}
	attempt := t.Attempt
	if attempt <= 0 {
		attempt = 1
	}
	steps := len(t.Chain) - (attempt - 1)
	if steps < 1 {
		steps = 1
	}
	return steps
}

// Terminal 表示不会再进入工作态。completed/failed/interrupted 仍是结果终态，
// 但允许 delivered 这一步；其余终态没有出边。
func (s TaskStatus) Terminal() bool {
	switch s {
	case TaskCompleted, TaskFailed, TaskInterrupted, TaskDelivered,
		TaskCanceled, TaskBlockedBudget, TaskDeclined:
		return true
	default:
		return false
	}
}

// CanTransition 是状态机的唯一迁移表；同状态视为幂等重放。
func (s TaskStatus) CanTransition(to TaskStatus) bool {
	if !s.Valid() || !to.Valid() {
		return false
	}
	if s == to {
		return true
	}
	switch s {
	case TaskQueued:
		return to == TaskDispatched || to == TaskCanceled || to == TaskBlockedBudget || to == TaskDeclined
	case TaskDispatched:
		// completed/failed 也允许：某些 AgentEnd 可能早于 AgentStart 观测。
		// declined 对应 Ask 已投递后目标拒绝。
		return to == TaskRunning || to == TaskCompleted || to == TaskFailed ||
			to == TaskInterrupted || to == TaskCanceled || to == TaskBlockedBudget || to == TaskDeclined
	case TaskRunning:
		// canceled 保留给显式 task_cancel；人工 interrupt 仍应走 interrupted。
		// blocked_budget = 结算时发现单任务成本超限（P2-07）。
		return to == TaskCompleted || to == TaskFailed || to == TaskInterrupted || to == TaskCanceled || to == TaskBlockedBudget
	case TaskCompleted, TaskFailed, TaskInterrupted:
		return to == TaskDelivered
	default:
		return false // delivered/canceled/blocked_budget/declined 无出边
	}
}

// Transition 校验并应用状态迁移；非法迁移返回 ErrInvalid，终态不可回退。
func (t *Task) Transition(to TaskStatus, at time.Time) error {
	if t == nil {
		return fmt.Errorf("%w: task is nil", ErrInvalid)
	}
	if !t.Status.CanTransition(to) {
		return fmt.Errorf("%w: task %s cannot transition %q -> %q", ErrInvalid, t.ID, t.Status, to)
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	t.Status = to
	t.UpdatedAt = at
	return nil
}

// Validate 校验账本条目的一致性；ID 采用前缀校验，完整形状由创建路径保证。
func (t *Task) Validate() error {
	if t == nil {
		return fmt.Errorf("%w: task is nil", ErrInvalid)
	}
	if t.ID == "" || !strings.HasPrefix(string(t.ID), PrefixTask+"_") {
		return fmt.Errorf("%w: task.id must start with %q", ErrInvalid, PrefixTask+"_")
	}
	if t.RootTaskID == "" || !strings.HasPrefix(string(t.RootTaskID), PrefixTask+"_") {
		return fmt.Errorf("%w: task.root_task_id must start with %q", ErrInvalid, PrefixTask+"_")
	}
	if t.ParentTaskID != "" && !strings.HasPrefix(string(t.ParentTaskID), PrefixTask+"_") {
		return fmt.Errorf("%w: task.parent_task_id must start with %q", ErrInvalid, PrefixTask+"_")
	}
	if len(t.Chain) == 0 {
		return fmt.Errorf("%w: task.chain is required", ErrInvalid)
	}
	hasID := false
	hasRoot := false
	for _, id := range t.Chain {
		if id == t.ID {
			hasID = true
		}
		if id == t.RootTaskID {
			hasRoot = true
		}
	}
	if !hasID || !hasRoot {
		return fmt.Errorf("%w: task.chain must contain id and root_task_id", ErrInvalid)
	}
	if t.Chain[0] != t.RootTaskID {
		return fmt.Errorf("%w: task.chain[0] must be root_task_id", ErrInvalid)
	}
	if t.ParentTaskID != "" {
		parentInChain := false
		for _, id := range t.Chain {
			if id == t.ParentTaskID {
				parentInChain = true
			}
		}
		if !parentInChain {
			return fmt.Errorf("%w: task.parent_task_id must be in chain", ErrInvalid)
		}
	}
	if !t.Status.Valid() {
		return fmt.Errorf("%w: task.status %q is invalid", ErrInvalid, t.Status)
	}
	if t.Origin.Kind != OriginDM && t.Origin.Kind != OriginGroup && t.Origin.Kind != OriginHuman {
		return fmt.Errorf("%w: task.origin.kind %q is invalid", ErrInvalid, t.Origin.Kind)
	}
	if t.Target.Kind != TargetWorkspace && t.Target.Kind != TargetAgent {
		return fmt.Errorf("%w: task.target.kind %q is invalid", ErrInvalid, t.Target.Kind)
	}
	if t.Target.Kind == TargetAgent {
		// 群内智能体目标任务必须有明确的目标与群：缺了就落到「无人接收」的账本条目。
		// session_id 不在这里强制（历史/测试快照可能省略），由 Queue.SubmitAgent 校验。
		if t.Target.AgentID == "" || t.Target.GroupID == "" {
			return fmt.Errorf("%w: agent target requires agent_id and group_id", ErrInvalid)
		}
	}
	if t.Title == "" {
		return fmt.Errorf("%w: task.title is required", ErrInvalid)
	}
	if t.Attempt < 0 {
		return fmt.Errorf("%w: task.attempt must be >= 0", ErrInvalid)
	}
	return nil
}
