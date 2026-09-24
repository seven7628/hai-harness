package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/seven7628/hai-harness/tools"
	"time"
)

// 由于 cron Store 是进程级（跨会话、跨工作区），工具通过构造闭包持有 Store 指针，
// 而非像 todo 那样经 context 注入（todo 是会话级，cron 是宿主级）。创建时绑定
// 当前会话的 workspace/session_id 作为触发目标的工作区与信息性来源。
// 全部声明只读（只动会话内/宿主状态、无外部不可逆副作用）→ plan 模式可见。

// ToolBound 一次注册的绑定信息（CronCreate 触发目标）。
type ToolBound struct {
	Workspace string // 触发时新建 Session 所在工作区
	SessionID string // 创建时所在会话（信息性）
}

// NewTools 构造 4 个 cron 工具的切片（CronCreate/CronList/CronUpdate/CronDelete）。
func NewTools(store *Store, bound ToolBound) []tools.Tool {
	b := &bound
	return []tools.Tool{
		&cronCreateTool{BaseTool: tools.BaseTool{
			Name_: "CronCreate",
			Description_: "Create (or schedule) a cron/scheduled job. Use for recurring schedules (e.g. 'every 5 minutes', " +
				"'weekdays at 9am') and one-shot reminders ('remind me at <time>'). Standard 5-field cron in local time: " +
				"minute hour day-of-month month day-of-week. For one-shot reminders pin minute/hour/day-of-month/month and " +
				"set recurring=false. Prefer an off-:00/:30 minute unless the user names an exact time. Returns the job id. " +
				"Recurring jobs auto-expire after 7 days (the user should be told).",
			Params_: tools.Obj(map[string]any{
				"cron":      tools.Str("Standard 5-field cron expression, e.g. \"*/5 * * * *\" or \"30 14 19 8 *\""),
				"prompt":    tools.Str("The prompt to run when the job fires (a new session will execute it)."),
				"recurring": tools.Map{"type": "boolean", "description": "true (default) = repeat on every match until deleted/expired; false = fire once then auto-delete.", "default": true},
			}, "cron", "prompt"),
			ReadOnly_: true,
		}, store: store, bound: b},

		&cronListTool{BaseTool: tools.BaseTool{
			Name_:        "CronList",
			Description_: "List all cron jobs scheduled via CronCreate (ids, cron expressions, prompts, recurring flag, next/last run, run count, status).",
			Params_:      tools.Obj(map[string]any{}),
			ReadOnly_:    true,
		}, store: store},

		&cronUpdateTool{BaseTool: tools.BaseTool{
			Name_: "CronUpdate",
			Description_: "Edit an existing cron job: change its prompt and/or cron schedule (and recurring flag). " +
				"Only the provided fields change; the job id, run history and run count are preserved. If cron changes, " +
				"the next run is recomputed from now. Set paused=true to pause (stop firing, keep the job and history); " +
				"paused=false to resume.",
			Params_: tools.Obj(map[string]any{
				"id":        tools.Str("Job id returned by CronCreate."),
				"prompt":    tools.Str("New prompt (optional)."),
				"cron":      tools.Str("New 5-field cron expression (optional)."),
				"recurring": tools.Map{"type": "boolean", "description": "New recurring flag (optional)."},
				"paused":    tools.Map{"type": "boolean", "description": "Pause (true) or resume (false) the job (optional)."},
			}, "id"),
			ReadOnly_: true,
		}, store: store},

		&cronDeleteTool{BaseTool: tools.BaseTool{
			Name_:        "CronDelete",
			Description_: "Cancel a cron job previously scheduled with CronCreate. Removes it from the store; its run history is preserved.",
			Params_:      tools.Obj(map[string]any{"id": tools.Str("Job id returned by CronCreate.")}, "id"),
			ReadOnly_:    true,
		}, store: store},
	}
}

type cronCreateTool struct {
	tools.BaseTool
	store *Store
	bound *ToolBound
}

func (t *cronCreateTool) Call(_ context.Context, _, arguments string) (string, error) {
	var a struct {
		Cron      string `json:"cron"`
		Prompt    string `json:"prompt"`
		Recurring *bool  `json:"recurring"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	recurring := true
	if a.Recurring != nil {
		recurring = *a.Recurring
	}
	job, err := t.store.Add(a.Cron, a.Prompt, recurring, t.bound.Workspace, t.bound.SessionID, time.Now())
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("scheduled job %s: %q next run %s (recurring=%v)", job.ID, job.Prompt, job.NextRun.Format("2006-01-02 15:04"), job.Recurring), nil
}

func (t *cronCreateTool) ValidParams(_ context.Context, _, arguments string) error {
	var a struct {
		Cron   string `json:"cron"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return err
	}
	if a.Cron == "" || a.Prompt == "" {
		return fmt.Errorf("cron and prompt are required")
	}
	if _, err := Parse(a.Cron); err != nil {
		return err
	}
	return nil
}

type cronListTool struct {
	tools.BaseTool
	store *Store
}

func (t *cronListTool) ValidParams(_ context.Context, _, _ string) error { return nil }

func (t *cronListTool) Call(_ context.Context, _, _ string) (string, error) {
	jobs := t.store.List()
	b, err := json.Marshal(jobs)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type cronUpdateTool struct {
	tools.BaseTool
	store *Store
}

func (t *cronUpdateTool) ValidParams(_ context.Context, _, arguments string) error {
	var a struct {
		ID   string  `json:"id"`
		Cron *string `json:"cron"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return err
	}
	if a.ID == "" {
		return fmt.Errorf("id is required")
	}
	if a.Cron != nil {
		if _, err := Parse(*a.Cron); err != nil {
			return err
		}
	}
	return nil
}

func (t *cronUpdateTool) Call(_ context.Context, _, arguments string) (string, error) {
	var a struct {
		ID        string  `json:"id"`
		Prompt    *string `json:"prompt"`
		Cron      *string `json:"cron"`
		Recurring *bool   `json:"recurring"`
		Paused    *bool   `json:"paused"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	if a.ID == "" {
		return "", fmt.Errorf("id is required")
	}
	job, err := t.store.Update(a.ID, a.Prompt, a.Cron, a.Recurring, a.Paused, time.Now())
	if err != nil {
		return "", err
	}
	state := "running"
	if job.Paused {
		state = "paused"
	}
	return fmt.Sprintf("updated job %s: prompt=%q cron=%q next run=%s state=%s", job.ID, job.Prompt, job.Cron, job.NextRun.Format("2006-01-02 15:04"), state), nil
}

type cronDeleteTool struct {
	tools.BaseTool
	store *Store
}

func (t *cronDeleteTool) ValidParams(_ context.Context, _, arguments string) error {
	var a struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return err
	}
	if a.ID == "" {
		return fmt.Errorf("id is required")
	}
	return nil
}

func (t *cronDeleteTool) Call(_ context.Context, _, arguments string) (string, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	if err := t.store.Delete(a.ID); err != nil {
		return "", err
	}
	return fmt.Sprintf("cron job %s deleted", a.ID), nil
}

// 冗余开关：CronList 只读、CronUpdate/CronDelete 也标只读（仅动宿主状态，
// 无外部不可逆副作用），保证 plan 模式可用。ReadOnly 由 BaseTool.ReadOnly_ 提供。
