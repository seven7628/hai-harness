package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/tools"
)

// agent_resume 恢复运行：把已结束的后台子 agent 从它自己的 journal 重建上下文，
// 作为**一条新任务**继续跑。
//
// 为什么是「新任务」而不是「原地续跑」（2026-10 决策，勿轻易改）：
//   - 原任务已经终态收尾（done 通道关闭、终态已入账与推送、TaskEnd 已发），原地复活
//     要重开 done、回滚已推送的终态 —— 每条都在推翻既有契约，且与「崩溃恢复一律
//     AbandonAll」的设计（registry.go 头部注释）正面冲突；
//   - 子运行的 goroutine 与 ctx 已销毁，AgentContext 不落地（不可序列化），本来就
//     无法「接着跑」；真相只在 journal 里。
// 于是恢复 = 读 journal → 组装交接简报 → 用**同一 loop**（同人设/同工具集）再跑一次。
// 新任务继承原任务的 name/工具名/派发链（parentTaskID），结果照常自动回传；
// ResumedFrom 记录恢复来源，TaskList 一眼看出这是续跑而不是新活。
//
// 与 agent_send 的分工：agent_send 只能投给**仍在运行**的任务（终态任务回
// ErrTaskFinished）——恢复已结束的任务正是本工具补的缺口。
//
// 跨进程恢复（这是本工具最实的价值）：Registry 是**内存态**（进程重启后任务对象不存在，
// NewSession 走 AbandonAll），但 journal 是磁盘态且路径只是 taskID 的函数。因此本工具在
// 注册表查不到任务时**回落到 journal 路径解析器**——只要 journal 文件还在，就能把
// 上一次进程里跑了一半的调研/写作续起来（真实场景见记忆库 subagent-longform-doc-budget：
// 三个调研子 agent 都在「正要动笔」时断掉，成果只在 journal 里）。

// agentResumeTool 恢复工具：内嵌派发工具（同一 loop / journal 解析器 / 槽池），
// 只在入口多一道「原任务已结束且 journal 可读」的闸门与简报组装。
type agentResumeTool struct {
	spawn *Tool // agent_spawn 的后台执行器（复用 runBackgroundFrom：同一条派发链路）
	reg   *Registry
}

// resumeArgs agent_resume 参数。
type resumeArgs struct {
	TaskID  string `json:"task_id"`
	Name    string `json:"name,omitempty"`    // 覆盖续跑任务的短标签（缺省沿用原任务的）
	Message string `json:"message,omitempty"` // 本次续跑的增量指令（缺省「接着把原任务做完」）
}

func (t *agentResumeTool) Name() string { return "agent_resume" }

func (t *agentResumeTool) Description() string {
	return "Resume a FINISHED background subagent task (task-XXXXXXXX) as a new task: its journal (original instruction + per-round progress + final result) is rebuilt into a handoff brief, and the subagent runs again from that brief. " +
		"Use it when a task ended without delivering what you needed — interrupted, failed, timed out, or abandoned after a restart — and you want the same subagent to continue instead of starting over from scratch. " +
		"Why a new task: the original run goroutine and context are gone (nothing is serializable mid-run), so only its journal survives — that is what gets replayed. The resumed task keeps the original name/tool/delivery chain and its result is pushed back automatically like any spawned task; the response carries the NEW task_id, and the old one stays in its terminal state. " +
		"Works even after a process restart: tasks live on disk as journals, so a task from a previous run can still be resumed as long as its journal file remains. " +
		"This does NOT fit a completed task that already delivered the right result — re-running costs tokens and may duplicate side effects, so check the journal with TaskOutput first: if the work is actually done, do not resume. It cannot resume a task that is still running (use agent_send to steer it, or agent_interrupt to stop it). Promoted tool tasks (tooltask-*) have no journal and cannot be resumed."
}

func (t *agentResumeTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task_id": map[string]any{
				"type":        "string",
				"description": "Id of a FINISHED subagent task to resume (task-XXXXXXXX, from agent_spawn / subagent_explore; see TaskList). Must be in a terminal state — completed / interrupted / failed / abandoned — or no longer in this session's task list at all (e.g. a task from a previous process run, as long as its journal file still exists).",
			},
			"name": map[string]any{
				"type":        "string",
				"maxLength":   maxSubAgentNameLen,
				"description": fmt.Sprintf("Optional. Short label for the resumed task (max %d chars). Defaults to the original task's name.", maxSubAgentNameLen),
			},
			"message": map[string]any{
				"type":        "string",
				"description": "Optional. Additional instruction for this run, appended to the handoff brief (e.g. \"finish writing the file, do not re-run the research\", or \"the build failed — fix the errors\"). Omit to simply continue the original task.",
			},
		},
		"required":             []string{"task_id"},
		"additionalProperties": false,
	}
}

func (t *agentResumeTool) CanParallel() bool { return true }

func (t *agentResumeTool) ValidParams(_ context.Context, _, arguments string) error {
	var a resumeArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return fmt.Errorf("agent_resume: %w", err)
	}
	if !ValidTaskID(strings.TrimSpace(a.TaskID)) {
		return fmt.Errorf("agent_resume: valid task_id required (task-XXXXXXXX from agent_spawn/subagent_explore; promoted tool tasks cannot be resumed)")
	}
	if len([]rune(a.Name)) > maxSubAgentNameLen {
		return fmt.Errorf("agent_resume: name too long (max %d chars)", maxSubAgentNameLen)
	}
	return nil
}

func (t *agentResumeTool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}

func (t *agentResumeTool) AfterCall(context.Context, core.ToolCall) {}

// resumeSource 恢复来源：原任务的元信息（优先取注册表权威快照，注册表没有时回落到
// journal 自身携带的身份字段——跨进程恢复走后者）。
type resumeSource struct {
	TaskID     string
	Name       string
	ToolName   string
	Status     TaskStatus // 注册表权威状态；跨进程回落时为空（以 journal 的 result 记录为准）
	OutputFile string
	recs       []JournalRecord
}

func (t *agentResumeTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a resumeArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	taskID := strings.TrimSpace(a.TaskID)

	src, err := t.resolveSource(taskID)
	if err != nil {
		return "", fmt.Errorf("agent_resume: %w", err)
	}
	brief, err := src.handoffBrief(a.Message)
	if err != nil {
		return "", fmt.Errorf("agent_resume: %w", err)
	}

	// 深度/发起方上下文：与 agent_spawn 同源（ToolContext 透传），续跑任务照常受深度限制。
	tc := events.ToolContextFrom(ctx)
	childDepth := 0
	if tc != nil {
		childDepth = tc.Depth + 1
	}
	if t.spawn.spec.MaxDepth > 0 && childDepth > t.spawn.spec.MaxDepth {
		return "", fmt.Errorf("agent_resume: max depth %d exceeded", t.spawn.spec.MaxDepth)
	}

	name := strings.TrimSpace(a.Name)
	if name == "" {
		name = src.Name
	}
	if name == "" {
		name = src.ToolName
	}
	if name == "" {
		name = "resumed task"
	}
	return t.spawn.runBackgroundFrom(ctx, tc, childDepth, brief, name, taskID)
}

// resolveSource 定位恢复来源。两条路径，按优先级：
//
//  1. 注册表命中（同一进程内的常见情形）：权限最高的是进程内权威状态 —— 运行中直接拒绝
//     （那是 agent_send / agent_interrupt 的活），然后读它的 journal 路径。
//  2. 注册表未命中（跨进程 / 会话重建后）：把 taskID 交给 journal 路径解析器。路径只是
//     taskID 的函数，所以上一次进程派生的任务只要 journal 还在就能恢复 —— 这正是
//     「进程重启后子 agent 成果还在磁盘上」这一真实场景的入口。
//
// 两条路径都要求 journal 里**有 start 记录**（原始指令全文），否则无物可重建。
func (t *agentResumeTool) resolveSource(taskID string) (resumeSource, error) {
	info, err := t.reg.Snapshot(taskID)
	if err == nil {
		switch info.Status {
		case TaskRunning, TaskInterrupting:
			return resumeSource{}, fmt.Errorf("task %s is still %s — not resumable while running; use agent_send to steer it or agent_interrupt to stop it first, then resume", taskID, info.Status)
		}
		if info.OutputFile == "" {
			return resumeSource{}, fmt.Errorf("task %s has no journal (output_file empty), so there is nothing to rebuild from — spawn a fresh task instead", taskID)
		}
		recs, rerr := ReadAll(info.OutputFile)
		if rerr != nil {
			return resumeSource{}, rerr
		}
		return resumeSource{
			TaskID: taskID, Name: info.Name, ToolName: info.ToolName,
			Status: info.Status, OutputFile: info.OutputFile, recs: recs,
		}, nil
	}
	if !errors.Is(err, ErrTaskNotFound) {
		return resumeSource{}, err
	}

	// 跨进程回落：解析 journal 路径（未注入解析器 / 解析失败 → 明确说清为什么不能恢复）。
	if t.spawn.spec.JournalPath == nil {
		return resumeSource{}, fmt.Errorf("task %s is not in this session's task list and this session has no journal path resolver, so it cannot be located — use TaskList for the ids that are resumable here", taskID)
	}
	path, perr := t.spawn.spec.JournalPath(taskID)
	if perr != nil || path == "" {
		return resumeSource{}, fmt.Errorf("task %s is not in this session's task list and its journal path could not be resolved — use TaskList for the ids that are resumable here", taskID)
	}
	recs, rerr := ReadAll(path)
	if rerr != nil {
		return resumeSource{}, rerr
	}
	if len(recs) == 0 {
		return resumeSource{}, fmt.Errorf("task %s is not in this session's task list and has no journal on disk (%s) — nothing to rebuild from; spawn a fresh task instead", taskID, path)
	}
	src := resumeSource{TaskID: taskID, OutputFile: path, recs: recs}
	// 身份从 journal 自身取（start 记录带 Name；result 记录带终态）。
	for _, r := range recs {
		if r.Type == "start" && r.Name != "" {
			src.Name = r.Name
			break
		}
	}
	for _, r := range recs {
		if r.Type == "result" && r.Status != "" {
			src.Status = TaskStatus(r.Status)
		}
	}
	return src, nil
}

// handoffBrief 组装交接简报（续跑任务的唯一输入）。
//
// 简报必须**自包含**：新任务有独立上下文，看不到原任务的任何对话。构成：
//  1. 原任务身份 + 终态 —— 模型知道自己在接手什么、为什么被交到手；
//  2. 原始指令全文（start 记录的 Task）—— 恢复的核心，交出的意图不能丢；
//  3. 已完成轮次摘要（turn 记录）—— 避免重做已有研究；
//  4. 终态错误 / 最后输出（result 记录）—— 失败原因 / 停在哪里；
//  5. 接手指令 + 两条防线（别重做已验过的；核对落盘实物而不是相信摘要）。
func (s resumeSource) handoffBrief(extra string) (string, error) {
	var (
		origTask  string
		turns     []JournalRecord
		result    string
		resultErr string
		endStatus string
	)
	for _, r := range s.recs {
		switch r.Type {
		case "start":
			if origTask == "" {
				origTask = r.Task
			}
		case "turn":
			turns = append(turns, r)
		case "result":
			result, resultErr, endStatus = r.Text, r.Error, r.Status
		}
	}
	if origTask == "" {
		return "", fmt.Errorf("task %s: its journal has no start record, so the original instruction cannot be recovered — spawn a fresh task instead", s.TaskID)
	}

	status := endStatusOr(s.Status, endStatus)
	var b strings.Builder
	b.WriteString("[Resumed background task]\n")
	fmt.Fprintf(&b, "You are taking over task %s (name: %q), which ended as %s. Your context is fresh — everything you need from the previous run is below.\n\n", s.TaskID, s.Name, status)

	b.WriteString("## Original instruction\n")
	b.WriteString(origTask)
	b.WriteString("\n\n")

	if len(turns) > 0 {
		fmt.Fprintf(&b, "## Progress already made (%d round(s))\n", len(turns))
		b.WriteString("Do not redo research or verification already recorded here; re-read only what it says is missing.\n\n")
		for _, r := range turns {
			text := strings.TrimSpace(r.Text)
			if text == "" {
				text = "(no text this round)"
			}
			fmt.Fprintf(&b, "- Round %d", r.Round)
			if len(r.ToolNames) > 0 {
				fmt.Fprintf(&b, " [tools: %s]", strings.Join(r.ToolNames, ", "))
			}
			if r.Truncated {
				b.WriteString(" (truncated)")
			}
			fmt.Fprintf(&b, ": %s\n", text)
		}
		b.WriteString("\n")
	}

	switch {
	case resultErr != "":
		b.WriteString("## Why it stopped\n")
		fmt.Fprintf(&b, "The previous run failed with: %s\n\n", resultErr)
	case strings.TrimSpace(result) != "":
		b.WriteString("## Last output before it stopped\n")
		b.WriteString(strings.TrimSpace(result))
		b.WriteString("\n\n")
	default:
		b.WriteString("## Why it stopped\n")
		fmt.Fprintf(&b, "No final output was recorded (status: %s). Treat prior work as unverified and confirm what actually exists on disk before continuing.\n\n", status)
	}

	b.WriteString("## Your job now\n")
	if strings.TrimSpace(extra) != "" {
		b.WriteString(strings.TrimSpace(extra))
	} else {
		b.WriteString("Continue the original instruction from where the previous run left off and finish it.")
	}
	b.WriteString("\n- Do not repeat work already verified above; check deliverable artifacts on the filesystem (ls / read) instead of trusting the summary.\n")
	b.WriteString("- State explicitly anything you could not complete.\n")
	return b.String(), nil
}

// endStatusOr 简报里「为什么停」的状态文案：优先 journal result 记录的 status（审计副本），
// 缺失则用 Registry 权威状态（跨进程恢复时两者可能都空 → 如实说 unknown）。
func endStatusOr(authoritative TaskStatus, fromJournal string) string {
	if fromJournal != "" {
		return fromJournal
	}
	if authoritative != "" {
		return string(authoritative)
	}
	return "unknown (the task is no longer tracked in this session)"
}
