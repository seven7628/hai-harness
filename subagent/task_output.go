package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/tools"
	"os"
	"strings"
	"time"
)

// TaskOutput 后台任务进度拉取工具：子 agent 任务读 journal 最新记录 + Registry 权威状态，
// 让主 Agent 在自己合适的时机感知子 Agent 进度（pull-based monitoring）。
// wait_seconds > 0 时在任务未收尾前阻塞等待（机制上替代紧循环轮询）。
// 接受的 id 与 TaskList 列出的一致：agent_spawn / subagent_explore 派生的 task-* 走
// journal + Registry；promoted 工具任务（tooltask-*）没有 journal、结果自动送达主会话，
// 只返回宿主枚举闭包提供的在途快照（Call 内短路，不走 Snapshot/Done）。
// 设计依据 docs/SUBAGENT_OUTPUT_JOURNAL_TASKOUTPUT.md §3.4。

const (
	taskOutputModeLatest = "latest"
	taskOutputModeAll    = "all"
	taskOutputModeStatus = "status"

	taskOutputMaxRecords = 50 // 单次返回条数上限（防一次调用拖回超大上下文）

	// taskOutputMaxWaitSeconds wait_seconds 上限（>上限钳制而非报错：宽容，超时语义本身由
	// 调用方可控；120s 足以覆盖绝大多数后台任务，避免单次工具调用长时间占住主循环）。
	taskOutputMaxWaitSeconds = 120

	// taskOutputProgressPoll 等待期「进度唤醒」的检测间隔：只 stat journal 文件大小
	//（微秒级系统调用），200ms 足以让「有新记录就返回」体感即时，又不空转 CPU。
	taskOutputProgressPoll = 200 * time.Millisecond
)

type taskOutputTool struct{ reg *Registry }

func (t *taskOutputTool) Name() string { return "TaskOutput" }

func (t *taskOutputTool) Description() string {
	return "Read a background task's output/state to sense its progress (pull-based monitoring). " +
		"For subagent tasks (task-XXXXXXXX from agent_spawn / subagent_explore) it returns the authoritative status plus the latest journal records (per-round progress text and tool activity; the final record is the full result). " +
		"For promoted tool tasks (tooltask-XXXXXXXX, listed by TaskList) it returns a state snapshot only: there is no journal and no pullable intermediate output, and their final result is delivered to the main session automatically — do not poll them. " +
		"The final result of a subagent task is ALSO pushed back to the main session automatically when the task completes; this tool is for checking progress while it runs (and for verifying the delivered result). " +
		"If you need to wait for a subagent task, pass wait_seconds to block instead of polling repeatedly: it returns as soon as the task finishes OR its journal gains a new record (i.e. a new round of progress), whichever comes first — the result is also pushed back automatically when the task finishes. " +
		"(task_id is the identifier returned by agent_spawn)"
}

func (t *taskOutputTool) Parameters() any {
	return tools.Obj(map[string]any{
		"task_id": tools.Str("Background task identifier returned by agent_spawn / subagent_explore"),
		"mode": tools.Map{
			"type":        "string",
			"enum":        []string{taskOutputModeLatest, taskOutputModeAll, taskOutputModeStatus},
			"default":     taskOutputModeLatest,
			"description": `Read mode: "latest" (default: most recent record), "all" (up to max_records newest records), "status" (registry state only, no file read)`,
		},
		"max_records": tools.Map{
			"type":        "integer",
			"minimum":     1,
			"maximum":     taskOutputMaxRecords,
			"description": "Max records for latest/all modes; default 1 for latest, 50 for all; values >50 are clamped to 50",
		},
		"wait_seconds": tools.Map{
			"type":        "integer",
			"minimum":     0,
			"maximum":     taskOutputMaxWaitSeconds,
			"default":     0,
			"description": "0 = return a snapshot immediately (default); >0 = if the task is still running, block for at most this many seconds and return as soon as the task finishes or its journal gains a new record (progress), else when the wait times out. Use this instead of polling. Only applies to subagent tasks (task-*); promoted tool tasks (tooltask-*) are snapshot-only.",
		},
	}, "task_id")
}

func (t *taskOutputTool) CanParallel() bool { return true }

type taskOutputArgs struct {
	TaskID      string `json:"task_id"`
	Mode        string `json:"mode,omitempty"`
	MaxRecords  *int   `json:"max_records,omitempty"`  // 指针区分「未传」与「传 0」（0 非法，未传走默认）
	WaitSeconds *int   `json:"wait_seconds,omitempty"` // 指针区分「未传」与「传 0」（0 = 立即快照，二者同义）
}

func (t *taskOutputTool) ValidParams(_ context.Context, _, arguments string) error {
	var a taskOutputArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return fmt.Errorf("TaskOutput: %w", err)
	}
	if !ValidTaskID(a.TaskID) && !strings.HasPrefix(a.TaskID, "tooltask-") {
		return fmt.Errorf("TaskOutput: valid task_id required — subagent tasks are task-XXXXXXXX (from agent_spawn/subagent_explore), promoted tool tasks are tooltask-XXXXXXXX (see TaskList)")
	}
	switch a.Mode {
	case "", taskOutputModeLatest, taskOutputModeAll, taskOutputModeStatus:
	default:
		return fmt.Errorf("TaskOutput: mode must be one of %q/%q/%q", taskOutputModeLatest, taskOutputModeAll, taskOutputModeStatus)
	}
	if a.MaxRecords != nil && *a.MaxRecords < 1 {
		return fmt.Errorf("TaskOutput: max_records must be >= 1")
	}
	if a.WaitSeconds != nil && *a.WaitSeconds < 0 {
		return fmt.Errorf("TaskOutput: wait_seconds must be >= 0 (0 = snapshot now)")
	}
	return nil
}

// resolveWaitSeconds wait_seconds 归一化：未传/非法（<0）→ 0；> 上限 → 钳到上限（不报错，宽容）。
func resolveWaitSeconds(wait *int) int {
	if wait == nil {
		return 0
	}
	n := *wait
	if n < 0 {
		return 0
	}
	if n > taskOutputMaxWaitSeconds {
		return taskOutputMaxWaitSeconds
	}
	return n
}

// resolveTailCount latest 默认 1、all 默认 50；显式 max_records 钳制到 [1,50]。
func resolveTailCount(mode string, max *int) int {
	def := 1
	if mode == taskOutputModeAll {
		def = taskOutputMaxRecords
	}
	if max == nil {
		return def
	}
	n := *max
	if n < 1 {
		n = 1
	}
	if n > taskOutputMaxRecords {
		n = taskOutputMaxRecords
	}
	return n
}

// hintFor 按权威状态给出行为提示（空 records 时优先提示「刚启动/未落盘」——E4 正常态）。
func hintFor(status TaskStatus, records int) string {
	if records == 0 {
		return "暂无输出记录：任务可能刚启动（首条 start 记录尚未写出）、未启用落盘或已无输出"
	}
	switch status {
	case TaskRunning, TaskInterrupting:
		return "仍在运行：可先用 wait_seconds 阻塞等待，或继续做其他独立步骤——结果完成时会自动送达主会话，无需轮询"
	case TaskCompleted:
		return "已完成：完整结果已自动送达主会话，此处仅供核对"
	case TaskInterrupted:
		return "任务已被中断（非失败）：如需调整方向可用 agent_send 继续对话或重新派生任务"
	case TaskAbandoned:
		return "任务已被弃置（会话重启等），不会有后续输出"
	default: // failed 等
		return "任务失败：详见 error 字段与 result 记录"
	}
}

// journalSize journal 当前字节数（不存在/不可读 → 0）：等待期的进度判据只 stat 大小，
// 不读内容 —— journal 每条记录一物理行且 append-only，文件变大即等于「有新记录」。
func journalSize(path string) int64 {
	if path == "" {
		return 0
	}
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

func (t *taskOutputTool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}

func (t *taskOutputTool) AfterCall(context.Context, core.ToolCall) {}

func (t *taskOutputTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a taskOutputArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", fmt.Errorf("TaskOutput: %w", err)
	}

	// promoted 工具任务（tooltask-*）先短路：它们不在 r.tasks（无 Task 实例/无 journal/
	// 无 done 通道），只有宿主枚举闭包可查 —— 走 Snapshot 必然 not found。
	// 结果完成时自动送达主会话，这里只回最小事实快照；wait_seconds 对它不适用（无可等待的完成通道）。
	if strings.HasPrefix(a.TaskID, "tooltask-") {
		info, ok := t.reg.ToolTaskByID(a.TaskID)
		if !ok {
			return "", fmt.Errorf("TaskOutput: promoted tool task %q not found — use TaskList to list current task ids", a.TaskID)
		}
		status := "running"
		if info.Interrupting {
			status = "interrupting"
		}
		b, err := json.Marshal(map[string]any{
			"task_id": info.ID, "kind": "tool", "tool_name": info.Name, "status": status,
			"note": "已摘离为后台的工具任务：没有 journal，也没有可拉取的中间输出；" +
				"它的最终结果会在完成时自动送达主会话，不要轮询它",
		})
		if err != nil {
			return "", fmt.Errorf("TaskOutput: %w", err)
		}
		return string(b), nil
	}

	// 权威状态来自 Registry 快照（journal 内嵌 status 只是审计副本，展示以此为准）
	info, err := t.reg.Snapshot(a.TaskID)
	if err != nil {
		return "", fmt.Errorf("TaskOutput: %v — use TaskList to recover valid task_ids", err)
	}

	// wait_seconds > 0 且任务未收尾 → 阻塞至「任务结束 / journal 出现新记录（进度）/
	// 等待超时 / 引擎取消」四者之一。意义：把「等待 + 轮询」变成一次工具调用内的机制
	// （实测轮询 221 次/周、最长连续 50 次，工具描述里的反轮询措辞已被数据证伪）。
	//
	// 进度唤醒（2026-09-20）：wait_seconds 是**上限**而不是固定时长 —— 子 agent 每落一条
	// journal 记录就立刻返回最新进展，调用方不必等它整轮收尾，也不必自己写轮询。
	waited := 0
	wake := ""
	if waitSec := resolveWaitSeconds(a.WaitSeconds); waitSec > 0 && (info.Status == TaskRunning || info.Status == TaskInterrupting) {
		if done := t.reg.Done(a.TaskID); done != nil {
			start := time.Now()
			timer := time.NewTimer(time.Duration(waitSec) * time.Second)
			defer timer.Stop()
			ticker := time.NewTicker(taskOutputProgressPoll)
			defer ticker.Stop()
			startSize := journalSize(info.OutputFile)
		waitLoop:
			for {
				select {
				case <-done:
					wake = "done"
					break waitLoop
				case <-timer.C:
					wake = "timeout"
					break waitLoop
				case <-ctx.Done(): // 引擎取消/超时：不吞，按已等待返回
					wake = "cancel"
					break waitLoop
				case <-ticker.C:
					// journal 是 append-only（一条记录一物理行）→ 文件变大即「有新进展」
					if journalSize(info.OutputFile) > startSize {
						wake = "progress"
						break waitLoop
					}
				}
			}
			waited = int(time.Since(start).Milliseconds())
			// 等待结束后必须重新取权威状态（任务可能刚好结束）
			if info2, err2 := t.reg.Snapshot(a.TaskID); err2 == nil {
				info = info2
			}
		}
	}

	payload := map[string]any{
		"task_id":     info.ID,
		"status":      string(info.Status),
		"output_file": info.OutputFile,
		"waited_ms":   waited, // 让模型确知「这次确实等了」而不是被立刻返回迷惑
	}
	if wake != "" {
		// 唤醒原因：done=任务收尾 / progress=journal 新记录 / timeout=等满上限 / cancel=引擎取消
		payload["wake"] = wake
	}
	if info.Error != "" {
		payload["error"] = info.Error
	}

	mode := a.Mode
	if mode == "" {
		mode = taskOutputModeLatest
	}
	payload["mode"] = mode

	if mode == taskOutputModeStatus {
		b, err := json.Marshal(payload)
		if err != nil {
			return "", fmt.Errorf("TaskOutput: %w", err)
		}
		return string(b), nil
	}

	// 只读文件尾部（ReadTail）：等待期这个工具可能被反复调用，全量扫盘 + 逐行反序列化
	// 的成本随 journal 增大线性上升，而调用方要的只是末尾几条。
	tail, terr := ReadTail(info.OutputFile, resolveTailCount(mode, a.MaxRecords))
	recs := tail.Records
	if terr != nil {
		// 读侧故障降级为空结果 + 提示（不把 IO 错误抛给模型循环）
		recs = []JournalRecord{}
		payload["read_error"] = terr.Error()
	}
	if recs == nil {
		recs = []JournalRecord{}
	}
	payload["total_lines"] = tail.Total
	if !tail.TotalExact {
		// 文件超过反向读上限（journalTailMaxBytes）：total_lines 只是下界，如实标注
		payload["total_lines_exact"] = false
	}
	payload["records"] = recs
	hint := hintFor(info.Status, len(recs))
	if wake == "progress" && (info.Status == TaskRunning || info.Status == TaskInterrupting) {
		hint = "已因 journal 新记录返回（任务仍在运行）：拿到的是最新一轮进展；继续其他工作即可，" +
			"完成时结果会自动送达主会话，需要再等就再调用一次 wait_seconds"
	}
	payload["hint"] = hint

	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("TaskOutput: %w", err)
	}
	return string(b), nil
}
