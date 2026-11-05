package agenthub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// 任务事件类型。状态迁移直接用目标状态字符串，created/notified 是额外的账本事件。
const (
	TaskEventCreated  = "created"
	TaskEventNotified = "notified"
)

// TransitionOptions 是状态迁移同时要落账的字段。
type TransitionOptions struct {
	RunID           string
	Result          *TaskResult
	Error           string
	HumanIntervened *bool
	At              time.Time
}

// FinishInput 是一次 AgentEnd 的结算输入；Ledger 只落账，不负责真正回推。
type FinishInput struct {
	TaskID          TaskID
	RunID           string
	Status          TaskStatus // completed | failed | interrupted
	Result          *TaskResult
	Error           string
	HumanIntervened bool
	At              time.Time
}

// Ledger 是任务账本的读写入口。锁按 task_id 全局共享，
// 保证同一进程内多个 Ledger（hub/bridge）不会重复回推同一个 run。
type Ledger struct {
	store Store
}

// NewLedger 绑定一个 Store。
func NewLedger(store Store) *Ledger { return &Ledger{store: store} }

var ledgerTaskLocks sync.Map // map[TaskID]*sync.Mutex

func ledgerTaskLock(id TaskID) *sync.Mutex {
	v, _ := ledgerTaskLocks.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// Create 创建任务快照与 created 事件；ID 为空时由 Store 分配。
// 事件先写：即使快照写失败，created 事件里的完整快照仍可被 Replay 恢复。
func (l *Ledger) Create(ctx context.Context, t *Task) (*Task, error) {
	if l == nil || l.store == nil {
		return nil, fmt.Errorf("%w: ledger has no store", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("%w: task is nil", ErrInvalid)
	}
	if t.ID == "" {
		id, err := l.store.AllocateID(ctx, PrefixTask)
		if err != nil {
			return nil, err
		}
		t.ID = TaskID(id)
	}
	if t.Status != "" && t.Status != TaskQueued {
		return nil, fmt.Errorf("%w: new task must start queued, got %q", ErrInvalid, t.Status)
	}

	lock := ledgerTaskLock(t.ID)
	lock.Lock()
	defer lock.Unlock()

	now := time.Now().UTC()
	t.Status = TaskQueued
	if t.RootTaskID == "" {
		t.RootTaskID = t.ID
	}
	if len(t.Chain) == 0 {
		if t.RootTaskID == t.ID {
			t.Chain = []TaskID{t.ID}
		} else {
			t.Chain = []TaskID{t.RootTaskID, t.ID}
		}
	}
	if t.Attempt == 0 {
		t.Attempt = 1
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = now
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	if _, err := l.store.LoadTask(ctx, t.ID); err == nil {
		return nil, fmt.Errorf("%w: task %s already exists", ErrConflict, t.ID)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	events, err := l.store.ReadTaskEvents(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	if len(events) > 0 {
		return nil, fmt.Errorf("%w: task %s already has events", ErrConflict, t.ID)
	}

	ev := TaskEvent{
		TaskID: t.ID,
		Type:   TaskEventCreated,
		At:     now,
		Data:   taskEventData(t, "", "", nil, "", nil),
	}
	if _, err := l.store.AppendTaskEvent(ctx, t.ID, ev); err != nil {
		return nil, err
	}
	if err := l.store.SaveTask(ctx, t); err != nil {
		return nil, err
	}
	return t, nil
}

// FindByClientMsgID 按 (target, client_msg_id) 找已存在任务（派发幂等）。
// 空 id 返回 (nil, nil)；找到返回最新的一条（账本 append-only，ID 不复用）。
func (l *Ledger) FindByClientMsgID(ctx context.Context, target TaskTarget, clientMsgID string) (*Task, error) {
	if l == nil || l.store == nil {
		return nil, fmt.Errorf("%w: ledger has no store", ErrInvalid)
	}
	if strings.TrimSpace(clientMsgID) == "" {
		return nil, nil
	}
	all, err := l.store.ListTasks(ctx, TaskFilter{})
	if err != nil {
		return nil, err
	}
	var hit *Task
	for _, t := range all {
		if t == nil || t.ClientMsgID != clientMsgID {
			continue
		}
		if t.Target.Kind != target.Kind || t.Target.Workspace != target.Workspace || t.Target.SessionID != target.SessionID {
			continue
		}
		if hit == nil || t.CreatedAt.After(hit.CreatedAt) {
			hit = t
		}
	}
	return hit, nil
}

// Transition 校验并落盘一次状态迁移；同状态重放不追加事件（除非显式补 RunID/结果）。
func (l *Ledger) Transition(ctx context.Context, id TaskID, to TaskStatus, opts TransitionOptions) (*Task, error) {
	if l == nil || l.store == nil {
		return nil, fmt.Errorf("%w: ledger has no store", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock := ledgerTaskLock(id)
	lock.Lock()
	defer lock.Unlock()

	t, err := l.store.LoadTask(ctx, id)
	if err != nil {
		return nil, err
	}
	at := opts.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if t.Status == to {
		if opts.RunID == "" && opts.Result == nil && opts.Error == "" && opts.HumanIntervened == nil {
			return t, nil
		}
		if opts.RunID != "" && t.Dispatch.RunID == opts.RunID && opts.Result == nil && opts.Error == "" && opts.HumanIntervened == nil {
			return t, nil
		}
		prev := cloneTask(t)
		applyTransitionFields(t, to, opts, at)
		t.UpdatedAt = at
		if err := l.saveAndAppend(ctx, prev, t, string(to), opts.RunID, opts.Result, opts.Error, opts.HumanIntervened, at); err != nil {
			return nil, err
		}
		return t, nil
	}

	prev := cloneTask(t)
	if err := t.Transition(to, at); err != nil {
		return nil, err
	}
	applyTransitionFields(t, to, opts, at)
	if err := l.saveAndAppend(ctx, prev, t, string(to), opts.RunID, opts.Result, opts.Error, opts.HumanIntervened, at); err != nil {
		return nil, err
	}
	return t, nil
}

// Finish 把 AgentEnd 结算进账本；不负责回推，回推后再调 MarkNotified。
// 同一状态重复 Finish 幂等；若携带新的 Result/Error，则更新快照。
func (l *Ledger) Finish(ctx context.Context, in FinishInput) (*Task, error) {
	if in.Status != TaskCompleted && in.Status != TaskFailed && in.Status != TaskInterrupted && in.Status != TaskBlockedBudget {
		return nil, fmt.Errorf("%w: finish status %q must be completed/failed/interrupted/blocked_budget", ErrInvalid, in.Status)
	}
	if in.RunID == "" {
		return nil, fmt.Errorf("%w: finish run_id is required", ErrInvalid)
	}
	if l == nil || l.store == nil {
		return nil, fmt.Errorf("%w: ledger has no store", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock := ledgerTaskLock(in.TaskID)
	lock.Lock()
	defer lock.Unlock()

	t, err := l.store.LoadTask(ctx, in.TaskID)
	if err != nil {
		return nil, err
	}
	now := in.At
	if now.IsZero() {
		now = time.Now().UTC()
	}
	prev := cloneTask(t)
	if t.Status == in.Status {
		changed := false
		if in.Result != nil {
			t.Result = in.Result
			changed = true
		}
		if in.Error != "" && t.Error != in.Error {
			t.Error = in.Error
			changed = true
		}
		if t.HumanIntervened != in.HumanIntervened {
			t.HumanIntervened = in.HumanIntervened
			changed = true
		}
		if t.Dispatch.AgentEndAt == nil {
			t.Dispatch.AgentEndAt = &now
			changed = true
		}
		if t.Dispatch.RunID != "" && t.Dispatch.RunID != in.RunID {
			t.SupersededRuns = appendUniqueString(t.SupersededRuns, t.Dispatch.RunID)
			t.Dispatch.RunID = in.RunID
			changed = true
		}
		if !changed {
			return t, nil
		}
		t.UpdatedAt = now
		if err := l.saveAndAppend(ctx, prev, t, string(in.Status), in.RunID, in.Result, in.Error, &in.HumanIntervened, now); err != nil {
			return nil, err
		}
		return t, nil
	}
	if err := t.Transition(in.Status, now); err != nil {
		return nil, err
	}
	t.Result = in.Result
	t.Error = in.Error
	t.HumanIntervened = in.HumanIntervened
	if t.Dispatch.AgentEndAt == nil {
		t.Dispatch.AgentEndAt = &now
	}
	if t.Dispatch.RunID != "" && t.Dispatch.RunID != in.RunID {
		t.SupersededRuns = appendUniqueString(t.SupersededRuns, t.Dispatch.RunID)
	}
	t.Dispatch.RunID = in.RunID
	if err := l.saveAndAppend(ctx, prev, t, string(in.Status), in.RunID, in.Result, in.Error, &in.HumanIntervened, now); err != nil {
		return nil, err
	}
	return t, nil
}

// MarkNotified 原子记录 (task_id, run_id)；返回 true 表示第一次标记、可以回推。
// 只有已结算（completed/failed/interrupted/delivered）的任务才能占幂等键；
// 先 notify 后 finish 会留下永远无法回推的键，必须拒绝。
func (l *Ledger) MarkNotified(ctx context.Context, id TaskID, runID string) (bool, error) {
	if runID == "" {
		return false, fmt.Errorf("%w: notified run_id is empty", ErrInvalid)
	}
	if l == nil || l.store == nil {
		return false, fmt.Errorf("%w: ledger has no store", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	lock := ledgerTaskLock(id)
	lock.Lock()
	defer lock.Unlock()

	t, err := l.store.LoadTask(ctx, id)
	if err != nil {
		return false, err
	}
	switch t.Status {
	case TaskCompleted, TaskFailed, TaskInterrupted, TaskDelivered:
	default:
		return false, fmt.Errorf("%w: task %s status %q is not settled", ErrConflict, id, t.Status)
	}
	for _, r := range t.NotifiedRuns {
		if r == runID {
			return false, nil
		}
	}
	prev := cloneTask(t)
	now := time.Now().UTC()
	t.NotifiedRuns = append(t.NotifiedRuns, runID)
	if t.Status == TaskCompleted || t.Status == TaskFailed || t.Status == TaskInterrupted {
		if err := t.Transition(TaskDelivered, now); err != nil {
			return false, err
		}
	}
	t.UpdatedAt = now
	if err := l.saveAndAppend(ctx, prev, t, TaskEventNotified, runID, nil, "", nil, now); err != nil {
		return false, err
	}
	return true, nil
}

// Replay 从 events.jsonl 重放任务快照。每个事件都带完整 Task 快照；
// 若快照文件比事件更新（上次 append 失败），取 UpdatedAt 较新的那份，避免静默分叉。
func (l *Ledger) Replay(ctx context.Context, id TaskID) (*Task, error) {
	if l == nil || l.store == nil {
		return nil, fmt.Errorf("%w: ledger has no store", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock := ledgerTaskLock(id)
	lock.Lock()
	defer lock.Unlock()

	snap, snapErr := l.store.LoadTask(ctx, id)
	if snapErr != nil && !errors.Is(snapErr, ErrNotFound) {
		return nil, snapErr
	}
	events, err := l.store.ReadTaskEvents(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		if snapErr != nil {
			return nil, snapErr
		}
		return snap, nil
	}

	var t *Task
	if snapErr == nil {
		t = cloneTask(snap)
	}
	hasCreated := false
	for _, ev := range events {
		var p taskEventPayload
		if len(ev.Data) > 0 {
			raw, err := json.Marshal(ev.Data)
			if err != nil {
				return nil, fmt.Errorf("%w: task event %s: %v", ErrCorrupt, id, err)
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, fmt.Errorf("%w: task event %s: %v", ErrCorrupt, id, err)
			}
		}
		if ev.Type == TaskEventCreated {
			if p.Task == nil {
				return nil, fmt.Errorf("%w: created event %s has no snapshot", ErrCorrupt, id)
			}
			hasCreated = true
			t = p.Task
			t.ID = id
		} else if p.Task != nil {
			t = p.Task
		}
		if t == nil {
			return nil, fmt.Errorf("%w: task event %s has no base snapshot", ErrCorrupt, id)
		}
		if ev.Type != TaskEventCreated {
			if p.Status != "" {
				t.Status = p.Status
			}
			if p.RunID != "" {
				t.Dispatch.RunID = p.RunID
			}
			if p.Result != nil {
				t.Result = p.Result
			}
			if p.Error != "" {
				t.Error = p.Error
			}
			if p.HumanIntervened != nil {
				t.HumanIntervened = *p.HumanIntervened
			}
			if ev.Type == TaskEventNotified {
				t.NotifiedRuns = appendUniqueString(t.NotifiedRuns, p.RunID)
			}
		}
		if !ev.At.IsZero() {
			t.UpdatedAt = ev.At
		}
	}
	if t == nil {
		return nil, ErrNotFound
	}
	if !hasCreated && snapErr != nil {
		return nil, fmt.Errorf("%w: task %s events have no created snapshot", ErrCorrupt, id)
	}
	if snapErr == nil && snap.UpdatedAt.After(t.UpdatedAt) {
		return snap, nil
	}
	return t, nil
}

type taskEventPayload struct {
	Task            *Task       `json:"task,omitempty"`
	Status          TaskStatus  `json:"status,omitempty"`
	RunID           string      `json:"run_id,omitempty"`
	Result          *TaskResult `json:"result,omitempty"`
	Error           string      `json:"error,omitempty"`
	HumanIntervened *bool       `json:"human_intervened,omitempty"`
}

func taskEventData(t *Task, status TaskStatus, runID string, result *TaskResult, errMsg string, human *bool) map[string]any {
	data := map[string]any{}
	if t != nil {
		data["task"] = t
	}
	if status != "" {
		data["status"] = string(status)
	}
	if runID != "" {
		data["run_id"] = runID
	}
	if result != nil {
		data["result"] = result
	}
	if errMsg != "" {
		data["error"] = errMsg
	}
	if human != nil {
		data["human_intervened"] = *human
	}
	return data
}

func applyTransitionFields(t *Task, to TaskStatus, opts TransitionOptions, at time.Time) {
	if opts.RunID != "" {
		t.Dispatch.RunID = opts.RunID
	}
	switch to {
	case TaskDispatched:
		if t.Dispatch.DispatchedAt == nil {
			t.Dispatch.DispatchedAt = &at
		}
	case TaskRunning:
		if t.Dispatch.AcceptedAt == nil {
			t.Dispatch.AcceptedAt = &at
		}
		if t.Dispatch.AgentStartAt == nil {
			t.Dispatch.AgentStartAt = &at
		}
	case TaskCompleted, TaskFailed, TaskInterrupted:
		if t.Dispatch.AgentEndAt == nil {
			t.Dispatch.AgentEndAt = &at
		}
	}
	if opts.Result != nil {
		t.Result = opts.Result
	}
	if opts.Error != "" {
		t.Error = opts.Error
	}
	if opts.HumanIntervened != nil {
		t.HumanIntervened = *opts.HumanIntervened
	}
}

// saveAndAppend 先原子写快照，再追加事件；append 失败时回滚快照，避免半状态。
func (l *Ledger) saveAndAppend(ctx context.Context, prev, t *Task, evType string, runID string, result *TaskResult, errMsg string, human *bool, at time.Time) error {
	if err := l.store.SaveTask(ctx, t); err != nil {
		return err
	}
	ev := TaskEvent{
		TaskID: t.ID,
		Type:   evType,
		At:     at,
		Data:   taskEventData(t, t.Status, runID, result, errMsg, human),
	}
	if _, err := l.store.AppendTaskEvent(ctx, t.ID, ev); err != nil {
		// best effort：事件是审计源，不能让快照永久领先。
		if prev != nil {
			_ = l.store.SaveTask(ctx, prev)
		}
		return err
	}
	return nil
}

func appendUniqueString(in []string, s string) []string {
	if s == "" {
		return in
	}
	for _, v := range in {
		if v == s {
			return in
		}
	}
	return append(in, s)
}
