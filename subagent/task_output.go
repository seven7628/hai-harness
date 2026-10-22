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
	// taskOutputModeLines 按行读子任务会话文件（from_line/to_line）：详情要看时由 model
	// 自己决定读哪几行，而不是让工具每次把整份大 JSON 倒进上下文。
	taskOutputModeLines = "lines"

	taskOutputMaxRecords = 50 // 单次返回条数上限（防一次调用拖回超大上下文）

	// 默认视图的文本长度：Output 是「委派的结论」，给足；每轮摘要只留「在做什么」。
	taskOutputTextCap      = 4000 // output 字段
	taskOutputRoundTextCap = 600  // recent[].text
	taskOutputDoingCap     = 160  // doing 一行

	// 按行查询：默认一次 40 行、每行 400 rune（钳制见 resolveLineChars）。
	taskOutputLinesDefaultSpan = 40
	taskOutputLineCharsDefault = 400
	taskOutputLineCharsMin     = 80
	taskOutputLineCharsMax     = 4000

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
	return "Read a background task's progress and produced output (pull-based monitoring). " +
		"For subagent tasks (task-XXXXXXXX from agent_spawn / subagent_explore) it returns: status, how many rounds have run, one-line \"doing\" summary of the latest round, the produced OUTPUT text (final answer, or the latest round's text while running), the FILES it wrote or edited, and the last few rounds (text excerpt + tool names + files each). " +
		"Use mode=lines with from_line/to_line to page through the task's session file line by line when you need detail it does not summarize: that file holds the per-round progress records AND the full message history (tool arguments and tool results in full) — read only what you need. " +
		"Token/cost accounting is intentionally NOT part of this tool's output. " +
		"For promoted tool tasks (tooltask-XXXXXXXX, listed by TaskList) it returns a state snapshot only: there is no journal and no pullable intermediate output, and their final result is delivered to the main session automatically — do not poll them. " +
		"The final result of a subagent task is ALSO pushed back automatically when the task completes; this tool is for checking progress while it runs (and for verifying the delivered result). " +
		"If you need to wait for a subagent task, pass wait_seconds to block instead of polling repeatedly: it returns as soon as the task finishes OR its session file gains a new record (i.e. a new round of progress), whichever comes first. " +
		"(task_id is the identifier returned by agent_spawn)"
}

func (t *taskOutputTool) Parameters() any {
	return tools.Obj(map[string]any{
		"task_id": tools.Str("Background task identifier returned by agent_spawn / subagent_explore"),
		"mode": tools.Map{
			"type":        "string",
			"enum":        []string{taskOutputModeLatest, taskOutputModeAll, taskOutputModeStatus, taskOutputModeLines},
			"default":     taskOutputModeLatest,
			"description": `Read mode: "latest" (default: status + doing + output + files + the most recent round), "all" (same, plus up to max_records recent rounds), "status" (registry state only, no file read), "lines" (read the task's session file line by line: pass from_line/to_line)`,
		},
		"max_records": tools.Map{
			"type":        "integer",
			"minimum":     1,
			"maximum":     taskOutputMaxRecords,
			"description": "Max recent rounds for latest/all modes; default 1 for latest, 50 for all; values >50 are clamped to 50",
		},
		"from_line": tools.Map{
			"type":        "integer",
			"minimum":     1,
			"description": "mode=lines only: first line (1-based) of the task's session file to read; omit to start at the latest round's line",
		},
		"to_line": tools.Map{
			"type":        "integer",
			"minimum":     1,
			"description": "mode=lines only: last line to read (inclusive); default from_line+39, at most 200 lines per call",
		},
		"max_line_chars": tools.Map{
			"type":        "integer",
			"minimum":     taskOutputLineCharsMin,
			"maximum":     taskOutputLineCharsMax,
			"description": "mode=lines only: truncate each line to this many characters (default 400); raise it when you need one long record in full",
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
	// 按行查询（mode=lines）：0 = 未传（起点默认「最近一轮那一行」，终点默认 +39 行）
	FromLine     int  `json:"from_line,omitempty"`
	ToLine       int  `json:"to_line,omitempty"`
	MaxLineChars *int `json:"max_line_chars,omitempty"`
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
	case "", taskOutputModeLatest, taskOutputModeAll, taskOutputModeStatus, taskOutputModeLines:
	default:
		return fmt.Errorf("TaskOutput: mode must be one of %q/%q/%q/%q",
			taskOutputModeLatest, taskOutputModeAll, taskOutputModeStatus, taskOutputModeLines)
	}
	if a.FromLine < 0 || a.ToLine < 0 {
		return fmt.Errorf("TaskOutput: from_line/to_line must be >= 1")
	}
	if a.ToLine > 0 && a.FromLine > 0 && a.ToLine < a.FromLine {
		return fmt.Errorf("TaskOutput: to_line must be >= from_line")
	}
	if a.MaxLineChars != nil && (*a.MaxLineChars < taskOutputLineCharsMin || *a.MaxLineChars > taskOutputLineCharsMax) {
		return fmt.Errorf("TaskOutput: max_line_chars must be within [%d,%d]", taskOutputLineCharsMin, taskOutputLineCharsMax)
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

	mode := a.Mode
	if mode == "" {
		mode = taskOutputModeLatest
	}

	// 按行查询（mode=lines）：纯读、不等待 —— 它服务的是「我要看这份文件的第 N 行」，
	// 与「等它进展」是两件事，混在一起只会让等待把一次读文件拖成 120s。
	if mode == taskOutputModeLines {
		return t.linesView(info, a)
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

	if mode == taskOutputModeStatus {
		b, err := json.Marshal(taskOutputStatusView{
			TaskID: info.ID, Name: info.Name, Status: string(info.Status),
			WaitedMs: waited, Wake: wake, Error: info.Error,
			Hint: hintFor(info.Status, 1),
		})
		if err != nil {
			return "", fmt.Errorf("TaskOutput: %w", err)
		}
		return string(b), nil
	}

	// —— 默认视图：进度 + 产出 + 文件。一次顺序扫描拿到全部所需（见 ReadProgress）。 ——
	pg, perr := ReadProgress(info.OutputFile, resolveTailCount(mode, a.MaxRecords))
	view := progressView(info, pg, waited, wake)
	if perr != nil {
		// 读侧故障降级为空结果 + 提示（不把 IO 错误抛进模型循环）
		view.ReadError = perr.Error()
	}

	b, err := json.Marshal(view)
	if err != nil {
		return "", fmt.Errorf("TaskOutput: %w", err)
	}
	return string(b), nil
}

// progressView 把「一次扫描的结果」投影成默认视图（纯函数：不读盘、不碰 Registry）。
//
// 拆出来的理由与 ReadProgress 相同：视图口径是这个工具唯一值得被回归钉住的东西
// （进度/产出/文件/行号索引、以及**不含** token），把它与「快照 + 等待」的编排分开，
// 才能拿真实 journal 直接验一遍（见 task_output_progress_test.go 的真实文件用例）。
func progressView(info TaskInfo, pg JournalProgress, waited int, wake string) taskOutputView {
	view := taskOutputView{
		TaskID: info.ID, Name: info.Name, Status: string(info.Status),
		Rounds: pg.Rounds, WaitedMs: waited, Wake: wake, Error: info.Error,
		SessionFile: info.OutputFile,
	}
	view.ElapsedMs = elapsedMs(pg)
	view.Files = pg.AllFiles
	if pg.Result != nil && len(pg.Result.Files) > 0 {
		view.Files = pg.Result.Files // 终态行是权威累计（含续跑上一轮的产出）
	}
	view.Output, view.OutputTruncated = pickOutput(pg, taskOutputTextCap)
	view.Doing = describeProgress(pg)
	for i, rec := range pg.Turns {
		line := 0
		if i < len(pg.TurnLines) {
			line = pg.TurnLines[i]
		}
		text, _ := truncateRunes(rec.Text, taskOutputRoundTextCap)
		view.Recent = append(view.Recent, taskOutputRound{
			Round: rec.Round, Line: line, At: clockOf(rec.Ts),
			Text: text, Tools: rec.ToolNames, Files: rec.Files,
		})
	}
	if info.OutputFile != "" {
		view.Lines = &taskOutputLines{
			Total: pg.Lines, HistoryFirst: pg.HistoryFirst, HistoryLast: pg.HistoryLast, StateLast: pg.StateLast,
		}
	}
	view.Hint = hintFor(info.Status, pg.Records)
	if wake == "progress" && (info.Status == TaskRunning || info.Status == TaskInterrupting) {
		view.Hint = "已因 journal 新记录返回（任务仍在运行）：拿到的是最新一轮进展；继续其他工作即可，" +
			"完成时结果会自动送达主会话，需要再等就再调用一次 wait_seconds"
	}
	if view.Lines != nil && view.Lines.Total > 0 {
		view.Hint += linesHint(view.Lines)
	}
	return view
}

// linesView 按行查询子任务会话文件（mode=lines）。
//
// 子任务的文件里同时躺着三类行：进度（start/turn/result）、全量消息（history，含工具
// 参数与结果全文）、上下文快照（state）。默认视图只给「进度 + 产出 + 文件」；要细节时
// 由 model 自己按行取 —— 它知道自己在找什么（某轮的工具参数？最终正文的完整版？），
// 不需要工具替它猜，更不需要每次都把整份大 JSON 倒进上下文。
func (t *taskOutputTool) linesView(info TaskInfo, a taskOutputArgs) (string, error) {
	if info.OutputFile == "" {
		return "", fmt.Errorf("TaskOutput: task %s has no session file (journal disabled) — use mode=status", info.ID)
	}
	from, to := a.FromLine, a.ToLine
	if from == 0 {
		// 未给起点 = 从最近一轮开始看（那是「现在在干嘛」的落点）
		pg, err := ReadProgress(info.OutputFile, 1)
		if err == nil && len(pg.TurnLines) > 0 {
			from = pg.TurnLines[len(pg.TurnLines)-1]
		} else {
			from = 1
		}
	}
	if to == 0 {
		to = from + taskOutputLinesDefaultSpan - 1
	}
	rg, err := ReadLineRange(info.OutputFile, from, to, resolveLineChars(a.MaxLineChars))
	if err != nil {
		return "", fmt.Errorf("TaskOutput: %w", err)
	}
	view := taskOutputLinesView{
		TaskID: info.ID, Status: string(info.Status), SessionFile: info.OutputFile,
		From: rg.From, To: rg.To, Total: rg.Total, MaxLineChars: rg.MaxChars, Lines: rg.Lines,
	}
	if rg.Total > 0 && rg.To < rg.Total {
		view.Hint = fmt.Sprintf("文件共 %d 行：继续用 from_line=%d 往后再看（单次最多 %d 行）",
			rg.Total, rg.To+1, LineRangeCap)
	}
	if rg.Total == 0 {
		view.Hint = "文件还是空的（任务刚启动或未落盘）"
	}
	b, err := json.Marshal(view)
	if err != nil {
		return "", fmt.Errorf("TaskOutput: %w", err)
	}
	return string(b), nil
}

// —— 默认视图的类型（全部字段都是「主 Agent 决策要用的」，没有 token/成本）——

// taskOutputRound 一轮进度：谁（工具）+ 说了什么（正文摘要）+ 产出了哪些文件 + 行号。
type taskOutputRound struct {
	Round int      `json:"round"`
	Line  int      `json:"line,omitempty"` // 物理行号：看全文就 mode=lines&from_line=line
	At    string   `json:"at,omitempty"`   // 该轮完成时刻（本地时钟，给人看）
	Text  string   `json:"text,omitempty"` // 本轮正文（截断，完整版按行查）
	Tools []string `json:"tools,omitempty"`
	Files []string `json:"files,omitempty"`
}

// taskOutputView 子任务进度的默认视图。
//
// 刻意**不含** usage/tokens/成本：委派方要判的是「它在干嘛、产出在哪、结论是什么」，
// token 消耗是账单口径（前端卡片/用量报表去看），塞进工具结果只是噪音。
type taskOutputView struct {
	TaskID          string            `json:"task_id"`
	Name            string            `json:"name,omitempty"`
	Status          string            `json:"status"`
	Rounds          int               `json:"rounds"` // 已完成的决策轮数
	ElapsedMs       int64             `json:"elapsed_ms,omitempty"`
	WaitedMs        int               `json:"waited_ms,omitempty"` // 让模型确知「这次确实等了」
	Wake            string            `json:"wake,omitempty"`      // done|progress|timeout|cancel
	Error           string            `json:"error,omitempty"`
	ReadError       string            `json:"read_error,omitempty"`
	Doing           string            `json:"doing,omitempty"`  // 一句话「最近一轮在做什么」
	Output          string            `json:"output,omitempty"` // 最新正文（运行中）/ 最终结果（终态）
	OutputTruncated bool              `json:"output_truncated,omitempty"`
	Files           []string          `json:"files,omitempty"`  // 本次委派写出/修改的文件（累计）
	Recent          []taskOutputRound `json:"recent,omitempty"` // 末尾轮次（max_records）
	SessionFile     string            `json:"session_file,omitempty"`
	Lines           *taskOutputLines  `json:"lines,omitempty"`
	Hint            string            `json:"hint,omitempty"`
}

// taskOutputLines 会话文件的行号索引：告诉 model「全文在哪、哪几行是进度、哪几行是全量消息」。
type taskOutputLines struct {
	Total        int `json:"total"`                   // 物理行数
	HistoryFirst int `json:"history_first,omitempty"` // 第一条全量消息行（history）
	HistoryLast  int `json:"history_last,omitempty"`  // 最后一条
	StateLast    int `json:"state_last,omitempty"`    // 最新上下文快照行（state）
}

// taskOutputStatusView status 模式：只回权威状态，不读文件（不受文件大小影响）。
type taskOutputStatusView struct {
	TaskID   string `json:"task_id"`
	Name     string `json:"name,omitempty"`
	Status   string `json:"status"`
	WaitedMs int    `json:"waited_ms,omitempty"`
	Wake     string `json:"wake,omitempty"`
	Error    string `json:"error,omitempty"`
	Hint     string `json:"hint,omitempty"`
}

// taskOutputLinesView 按行查询的返回（mode=lines）。
type taskOutputLinesView struct {
	TaskID       string        `json:"task_id"`
	Status       string        `json:"status"`
	SessionFile  string        `json:"session_file"`
	From         int           `json:"from"`
	To           int           `json:"to"`
	Total        int           `json:"total_lines"`
	MaxLineChars int           `json:"max_line_chars"`
	Lines        []JournalLine `json:"lines"`
	Hint         string        `json:"hint,omitempty"`
}

// elapsedMs 已运行时长：start 行 → result 行（终态）或现在（运行中）。
func elapsedMs(pg JournalProgress) int64 {
	if pg.Start == nil || pg.Start.Ts.IsZero() {
		return 0
	}
	end := time.Now()
	if pg.Result != nil && !pg.Result.Ts.IsZero() {
		end = pg.Result.Ts
	}
	return end.Sub(pg.Start.Ts).Milliseconds()
}

// pickOutput 默认视图的「内容」：终态用 result 行（最终答复）；运行中用**最近一段非空正文**
// —— 不是「最后一轮」：工具型子 agent 常连着几十轮只有工具调用，最后一轮正文为空，
// 而它之前明明说过话（实测某任务 84 轮里只有 6 轮有正文）。
// 这就是「不要只告诉我跑到第几轮，把产出/结论给我」的那个字段。
func pickOutput(pg JournalProgress, cap int) (string, bool) {
	if pg.Result != nil && strings.TrimSpace(pg.Result.Text) != "" {
		return truncateRunes(pg.Result.Text, cap)
	}
	if strings.TrimSpace(pg.LastText) != "" {
		return truncateRunes(pg.LastText, cap)
	}
	for i := len(pg.Turns) - 1; i >= 0; i-- {
		if strings.TrimSpace(pg.Turns[i].Text) != "" {
			return truncateRunes(pg.Turns[i].Text, cap)
		}
	}
	return "", false
}

// describeProgress 一句话描述最近一轮在做什么（工具 + 正文摘要）。
func describeProgress(pg JournalProgress) string {
	if len(pg.Turns) == 0 {
		if pg.Start != nil && pg.Start.Name != "" {
			return "已派发，尚未完成第一轮：" + pg.Start.Name
		}
		return ""
	}
	last := pg.Turns[len(pg.Turns)-1]
	parts := []string{}
	if last.Round > 0 {
		parts = append(parts, fmt.Sprintf("第 %d 轮", last.Round))
	}
	if len(last.ToolNames) > 0 {
		parts = append(parts, strings.Join(last.ToolNames, ", "))
	}
	if text := strings.TrimSpace(last.Text); text != "" {
		excerpt, _ := truncateRunes(text, taskOutputDoingCap)
		parts = append(parts, strings.ReplaceAll(excerpt, "\n", " "))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ")
}

// linesHint 把「怎么按行看全文」直接写进提示：model 不必猜文件布局。
func linesHint(ln *taskOutputLines) string {
	h := fmt.Sprintf("。想看细节（完整正文/工具参数/工具结果）用 mode=lines 按行查这份文件（共 %d 行）", ln.Total)
	if ln.HistoryFirst > 0 {
		h += fmt.Sprintf("；history 行（全量消息）在 %d–%d", ln.HistoryFirst, ln.HistoryLast)
	}
	if ln.StateLast > 0 {
		h += fmt.Sprintf("；state 行（最新上下文快照）在第 %d 行", ln.StateLast)
	}
	return h
}

// clockOf 事件时刻 → 本地时钟串（给人看，跨机器时区不同但语义清楚）。
func clockOf(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.Local().Format("15:04:05")
}

// resolveLineChars 每行内容截断长度：默认 400 rune，钳制到 [80,4000]。
func resolveLineChars(n *int) int {
	if n == nil {
		return taskOutputLineCharsDefault
	}
	v := *n
	if v < taskOutputLineCharsMin {
		v = taskOutputLineCharsMin
	}
	if v > taskOutputLineCharsMax {
		v = taskOutputLineCharsMax
	}
	return v
}
