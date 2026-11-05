package agenthub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// Ledger 是任务账本的读写入口：快照 + events.jsonl，进程内串行化。
type Ledger struct {
	store Store
	mu    sync.Mutex
}

// NewLedger 绑定一个 Store。
func NewLedger(store Store) *Ledger { return &Ledger{store: store} }

// Create 创建任务快照与 created 事件；ID 为空时由 Store 分配。
func (l *Ledger) Create(ctx context.Context, t *Task) (*Task, error) {
	if l == nil || l.store == nil {
		return nil, fmt.Errorf("%w: ledger has no store", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()

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
	now := time.Now().UTC()
	if t.Status == "" {
		t.Status = TaskQueued
	}
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
	} else if !errorsIsNotFound(err) {
		return nil, err
	}
	if err := l.store.SaveTask(ctx, t); err != nil {
		return nil, err
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
	return t, nil
}

// Transition 校验并落盘一次状态迁移；同状态重放是 no-op（不追加事件）。
func (l *Ledger) Transition(ctx context.Context, id TaskID, to TaskStatus, opts TransitionOptions) (*Task, error) {
	if l == nil || l.store == nil {
		return nil, fmt.Errorf("%w: ledger has no store", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	t, err := l.store.LoadTask(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.Status == to {
		return t, nil
	}
	at := opts.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if err := t.Transition(to, at); err != nil {
		return nil, err
	}
	applyTransitionFields(t, to, opts, at)
	if err := l.store.SaveTask(ctx, t); err != nil {
		return nil, err
	}
	ev := TaskEvent{
		TaskID: t.ID,
		Type:   string(to),
		At:     at,
		Data:   taskEventData(nil, to, opts.RunID, opts.Result, opts.Error, opts.HumanIntervened),
	}
	if _, err := l.store.AppendTaskEvent(ctx, t.ID, ev); err != nil {
		return nil, err
	}
	return t, nil
}

// Finish 把 AgentEnd 结算进账本；不负责回推，回推后再调 MarkNotified。
// 同一状态重复 Finish 幂等，不追加事件。
func (l *Ledger) Finish(ctx context.Context, in FinishInput) (*Task, error) {
	if in.Status != TaskCompleted && in.Status != TaskFailed && in.Status != TaskInterrupted {
		return nil, fmt.Errorf("%w: finish status %q must be completed/failed/interrupted", ErrInvalid, in.Status)
	}
	if l == nil || l.store == nil {
		return nil, fmt.Errorf("%w: ledger has no store", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	t, err := l.store.LoadTask(ctx, in.TaskID)
	if err != nil {
		return nil, err
	}
	if t.Status == in.Status {
		return t, nil
	}
	if err := t.Transition(in.Status, in.At); err != nil {
		return nil, err
	}
	now := in.At
	if now.IsZero() {
		now = time.Now().UTC()
	}
	t.Result = in.Result
	t.Error = in.Error
	t.HumanIntervened = in.HumanIntervened
	if t.Dispatch.AgentEndAt == nil {
		t.Dispatch.AgentEndAt = &now
	}
	if in.RunID != "" {
		t.Dispatch.RunID = in.RunID
	}
	if err := l.store.SaveTask(ctx, t); err != nil {
		return nil, err
	}
	human := in.HumanIntervened
	ev := TaskEvent{
		TaskID: t.ID,
		Type:   string(in.Status),
		At:     now,
		Data:   taskEventData(nil, in.Status, in.RunID, in.Result, in.Error, &human),
	}
	if _, err := l.store.AppendTaskEvent(ctx, t.ID, ev); err != nil {
		return nil, err
	}
	return t, nil
}

// MarkNotified 原子记录 (task_id, run_id)；返回 true 表示第一次标记、可以回推。
// 已标记过的 run 返回 false，调用方不得重复 PushTaskResult。
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
	l.mu.Lock()
	defer l.mu.Unlock()

	t, err := l.store.LoadTask(ctx, id)
	if err != nil {
		return false, err
	}
	for _, r := range t.NotifiedRuns {
		if r == runID {
			return false, nil
		}
	}
	now := time.Now().UTC()
	t.NotifiedRuns = append(t.NotifiedRuns, runID)
	if t.Status == TaskCompleted || t.Status == TaskFailed || t.Status == TaskInterrupted {
		if err := t.Transition(TaskDelivered, now); err != nil {
			return false, err
		}
	}
	t.UpdatedAt = now
	if err := l.store.SaveTask(ctx, t); err != nil {
		return false, err
	}
	ev := TaskEvent{
		TaskID: t.ID,
		Type:   TaskEventNotified,
		At:     now,
		Data:   taskEventData(nil, t.Status, runID, nil, "", nil),
	}
	if _, err := l.store.AppendTaskEvent(ctx, t.ID, ev); err != nil {
		return false, err
	}
	return true, nil
}

// Replay 从 events.jsonl 重放任务快照；created 事件带初始快照，后续事件是增量。
// 快照缺失/损坏时，只要 events 完整仍可恢复；没有任何事件时回落到 LoadTask。
func (l *Ledger) Replay(ctx context.Context, id TaskID) (*Task, error) {
	if l == nil || l.store == nil {
		return nil, fmt.Errorf("%w: ledger has no store", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	events, err := l.store.ReadTaskEvents(ctx, id)
	if err != nil {
		return nil, err
	}
	var t *Task
	if len(events) == 0 {
		return l.store.LoadTask(ctx, id)
	}
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
		if p.Task != nil {
			t = p.Task
		}
		if t == nil {
			t = &Task{ID: id}
		}
		if ev.Type == TaskEventCreated {
			if p.Task == nil {
				return nil, fmt.Errorf("%w: created event %s has no snapshot", ErrCorrupt, id)
			}
			t.ID = id
		} else {
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

func errorsIsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}
