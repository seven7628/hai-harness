// Package subagent 子 agent 工具：把独立运行的任务（研究/审查等）封装为特殊工具。
//
// 语义：
//   - 子 agent 有独立的上下文、工具集与压缩配置（Spec.Loop，其 SystemPrompt
//     可由上层按任务生成）；父子上下文天然隔离
//   - 子运行事件转发到父事件流（AgentStart/End 带 ParentRunId/Depth，可重建调用树）
//   - 子运行用量经 ToolResult.Usage 回传，父 AgentLoop 累加（成本传导 → Session 预算）
//   - 深度限制：Spec.MaxDepth（0 = 不限制）；abort 沿 context 级联
package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/tools"
	"strings"
	"sync"
	"time"
)

// Spec 子 agent 工具规范。
type Spec struct {
	Name        string            // 注册工具名（模型可见，如 "subagent_research"）
	Description string            // 工具描述
	Loop        *agents.AgentLoop // 子 agent 运行时（独立 SystemPrompt / 工具集 / 压缩配置）
	MaxDepth    int               // 允许的最大运行深度（子运行深度 = 父深度+1；0 = 不限制）

	// Mode 子运行的模式（Run 级注入；nil = 继承父的审批策略但无模式过滤）。
	// 典型用法：父 normal 派"只读规划子 agent"——Spec.Mode = agents.PlanMode()
	// （A/C 维度在子运行生效；B 维度经 WrapApprover 与父审批同源）。
	Mode *agents.Mode

	// PlanSafe 声明"plan 模式下可见且安全"：仅当子 loop 的工具集保证只读时
	// 才可置 true（plan 主 agent 据此委派规划，见 agents.PlanSafeTool）。
	PlanSafe bool

	// DisableHookInherit 关闭 hooks 继承：默认子运行透传父的
	// PreToolUse/PostToolBatch/PreCompact（经 ToolContext.Hooks）。
	DisableHookInherit bool

	// Background true = 后台运行：工具立即返回 taskId，主 agent 继续干活，
	// 结果自动回传主会话（需 NewBackgroundTool 注入 Session 级 ctx 与 Registry；
	// 直接用 NewTool 时此字段无效——同步形态零破坏）。
	Background bool

	// ForkTurns 上下文继承形态（Codex 对齐）："none"（默认）= 独立上下文
	//（现状同步语义）；"all" = 继承父全历史——v1 无父 Messages 数据通道
	//（ToolContext 只携带 ParentRunId/Depth/Handler/Approver/Hooks），
	// 暂不区分（语义恒为 none），列为后续。
	ForkTurns string

	// JournalPath 后台任务的输出 journal 路径解析器（宿主注入闭包，绑定
	// workspace/session 目录约定；nil = 不落盘）。解析在 Registry 任务构造期执行
	//（此处才拿到生成 taskId）；journal 的打开与写入全部发生在后台 goroutine 内，
	// 任一环节失败一律降级为「不落盘、任务照跑」——spawn 同步路径零开销零失败面
	//（主 Agent 防线，docs/SUBAGENT_OUTPUT_JOURNAL_TASKOUTPUT.md §3.3）。
	JournalPath func(taskID string) (string, error)

	// Slots 本工具使用的并发槽池（nil = Registry 主池）。宿主用它把只读/轻量
	// 子 agent 分到独立池（典型：subagent_explore 用 Registry.AuxSlots()），
	// 避免它占满主池饿死 agent_spawn。
	Slots *SlotPool
}

// Tool 实现 tools.Tool 的子 agent 工具。
type Tool struct {
	spec Spec

	// bgCtx 后台子运行的上下文提供者（Session 级 ctx：Session.Context() 闭包，
	// 工具注册时 Session 可能尚未创建，调用时才取值）；nil = 禁止后台运行。
	// 关键：不能复用 Call 的 ctx —— 工具 Call 返回后父 ctx 即取消，子任务会立刻死。
	bgCtx func() context.Context

	// registry 任务注册表（Session 持单例注入）；nil = 禁止后台运行。
	registry *Registry

	// defReg 文件化 subagent 定义注册表（可选）：agent_spawn 未携带 task 时，
	// 按 name 从 *.md 加载人设（合并 spawn_agent 能力，2026-08-30）。
	// loopForDef 按定义构造人设 loop（bridge 注入）；defReg 非 nil 时必填。
	defReg     *DefinitionRegistry
	loopForDef func(def *AgentDefinition) *agents.AgentLoop

	mu        sync.Mutex
	lastUsage core.Usage // 最近一次子运行用量（ToolUsageProvider，同步形态）
}

// NewTool 创建子 agent 工具（同步形态，注册进 ToolEngine 后生效）。
func NewTool(spec Spec) *Tool {
	return &Tool{spec: spec}
}

// NewBackgroundTool 创建后台子 agent 工具：spec.Background 生效——
// bgCtx 提供 Session 级 ctx（典型：func() context.Context { return s.Context() }，
// 工具注册先于 Session 创建，闭包延迟取值）；registry 为 Session 持有的单例。
func NewBackgroundTool(spec Spec, bgCtx func() context.Context, reg *Registry) *Tool {
	return &Tool{spec: spec, bgCtx: bgCtx, registry: reg}
}

// PlanSafe 实现 agents.PlanSafeTool：plan 模式可见性声明（Spec.PlanSafe）。
func (t *Tool) PlanSafe() bool { return t.spec.PlanSafe }

func (t *Tool) Name() string { return t.spec.Name }

func (t *Tool) Description() string { return t.spec.Description }

// Parameters name 必填且带 maxLength 约束：短标签标识子 agent 在干什么
// （模型/列表/结果回填对账用），schema 层直接声明上限，减少被 ValidParams 拒后的重试。
// task 可选（文件化模式省略）：省略时按 name 从 [Available subagents] 清单加载定义人设。
func (t *Tool) Parameters() any {
	return tools.Obj(map[string]any{
		"name": tools.Map{"type": "string", "maxLength": maxSubAgentNameLen, "description": fmt.Sprintf("REQUIRED. Short label for this subagent's job (e.g. \"code review\", \"migrate billing state machine\"). Keep it brief — max %d characters. Used to identify what this agent is doing in the creation result, TaskList, and the result pushback. In file-based mode (task omitted), this is the subagent definition name from the [Available subagents] list.", maxSubAgentNameLen)},
		"task": tools.Str("OPTIONAL when `name` matches a file-based subagent from the [Available subagents] list (then it is used as the concrete instruction for this run on top of the loaded persona); REQUIRED otherwise (dynamic mode: the subagent's complete, self-contained Prompt+Task — role / goal and scope / constraints / output format, not a one-liner)"),
	}, "name")
}

// maxSubAgentNameLen agent_spawn 的 name 短标签最大长度（简短标识，防长串占上下文）。
const maxSubAgentNameLen = 40

func (t *Tool) CanParallel() bool { return false }

// ToolTimeout 实现 tools.ToolTimeoutProvider：返回 <0 = 显式不受引擎默认超时限制。
// 子运行有自己的生命周期（长时任务可运行数分钟），由 abort/ctx 级联控制终止，
// 父引擎默认 30s 会误杀长时子 agent。
func (t *Tool) ToolTimeout() time.Duration { return -1 }

// MaxDepthLimit 本工具的深度上限（Spec.MaxDepth；0 = 不限制）。
// 供宿主做装配断言：子 agent 的递归层数是安全边界（每层任务占一个并发槽，
// 无限深会演成「等待者自己占着槽」的自锁），不该被悄悄改回 0。
func (t *Tool) MaxDepthLimit() int { return t.spec.MaxDepth }

// SlotPool 本工具使用的并发槽池（Spec.Slots；nil = Registry 主池）。
// 同上：装配断言用（explore 类只读工具必须走独立池，否则与 agent_spawn 互抢）。
func (t *Tool) SlotPool() *SlotPool { return t.spec.Slots }

type subAgentArgs struct {
	Name string `json:"name"`
	Task string `json:"task"`
}

func (t *Tool) ValidParams(_ context.Context, _, arguments string) error {
	var a subAgentArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return fmt.Errorf("%s: %w", t.spec.Name, err)
	}
	if strings.TrimSpace(a.Name) == "" {
		return fmt.Errorf("%s: name is required (short label of what this subagent is doing)", t.spec.Name)
	}
	if len(a.Name) > maxSubAgentNameLen {
		return fmt.Errorf("%s: name too long (max %d chars)", t.spec.Name, maxSubAgentNameLen)
	}
	// task 与定义二选一：task 空时，name 必须命中 *.md 定义（文件化模式），否则报错。
	if strings.TrimSpace(a.Task) == "" {
		if t.defReg == nil || t.loopForDef == nil {
			return fmt.Errorf("%s: task is required (or configure definition registry for file-based subagents)", t.spec.Name)
		}
		if _, err := t.defReg.Load(strings.TrimSpace(a.Name)); err != nil {
			return fmt.Errorf("%s: task is required (no subagent definition %q found: %v)", t.spec.Name, a.Name, err)
		}
	}
	return nil
}

func (t *Tool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}

func (t *Tool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a subAgentArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}

	// 深度限制：子运行深度 = 父深度 + 1（根 agent 直接调用时父深度为 0）
	tc := events.ToolContextFrom(ctx)
	childDepth := 0
	if tc != nil {
		childDepth = tc.Depth + 1
	}
	if t.spec.MaxDepth > 0 && childDepth > t.spec.MaxDepth {
		return "", fmt.Errorf("%s: max depth %d exceeded", t.spec.Name, t.spec.MaxDepth)
	}

	// 文件化模式（2026-08-30 合并 spawn_agent）：name 命中 *.md 定义 → 人设来自定义正文，
	// task（可选）作为本次运行的增量指令；未命中 → 纯动态模式（task 必填，完整 Prompt+Task）。
	if defReg := t.defReg; defReg != nil && t.loopForDef != nil {
		if def, err := defReg.Load(strings.TrimSpace(a.Name)); err == nil {
			// name 命中定义 → 文件化模式（人设 = 定义正文；task 增量指令）
			return t.callWithDefinition(ctx, tc, childDepth, def, a.Task)
		}
		// 未命中：继续动态模式（task 必须有——否则下面报错）
	}
	// 动态模式兜底：task 为空（defReg 未配置或 name 未命中）→ 报错，不把空任务传给子 agent。
	if strings.TrimSpace(a.Task) == "" {
		return "", fmt.Errorf("%s: task is required (name %q does not match any subagent definition and no task was provided)", t.spec.Name, a.Name)
	}

	// 后台分支：工具立即返回 taskId，子运行在独立 goroutine（Session 级 ctx），
	// 结果自动回传。槽获取是同步的（Session 级 ctx，豁免批超时）——池满会阻塞本次
	// Call；容量默认 DefaultConcurrency + 主/辅分池（slots.go）把「池满」压到真失控场景。
	if t.spec.Background && t.registry != nil {
		return t.runBackground(ctx, tc, childDepth, a.Task, a.Name)
	}
	return t.runSync(ctx, tc, childDepth, a.Task, a.Name)
}

// callWithDefinition 文件化 subagent 派发：按已加载定义构造人设 loop → 后台运行。
// 与动态模式同内核（runBackground），仅 loop 初始化不同（人设来自 *.md 正文）。
// task 为本次运行增量指令；空 → 默认指令。正文为空（无法作为人设）一律报错。
func (t *Tool) callWithDefinition(ctx context.Context, tc *events.ToolContext, childDepth int, def *AgentDefinition, task string) (string, error) {
	if strings.TrimSpace(def.Instructions) == "" {
		return "", fmt.Errorf("%s: subagent definition %q has empty instructions (cannot be used as persona); check {name}.md or provide a task instead", t.spec.Name, def.Name)
	}
	if t.spec.Background && t.registry != nil {
		// 临时 Tool：loop = 按定义构造的人设 loop；其余（bgCtx/registry）沿用。
		// task 缺省 → 默认指令（人设来自定义正文，task 仅作本次运行指引）。
		if strings.TrimSpace(task) == "" {
			task = "Proceed with your defined role and complete the task per your instructions."
		}
		tool := &Tool{
			spec: Spec{Name: t.spec.Name, Loop: t.loopForDef(def), Background: true,
				JournalPath: t.spec.JournalPath, Slots: t.spec.Slots},
			bgCtx:    t.bgCtx,
			registry: t.registry,
		}
		return tool.runBackground(ctx, tc, childDepth, task, def.Name)
	}
	return "", fmt.Errorf("%s: file-based subagent only supports background execution", t.spec.Name)
}

// buildOptions 组装子运行选项（同步/后台共用）：事件关联/审批继承/hooks 继承/模式。
// 子运行不继承父的待办：Todo 不传，AgentLoop 默认创建独立 store
// （子 agent 独立 TODO 管理，随子运行销毁；父会话的待办不受子影响）。
func (t *Tool) buildOptions(tc *events.ToolContext, childDepth int, name string) *agents.RunOptions {
	opts := &agents.RunOptions{Depth: childDepth, Name: name}
	if tc != nil {
		opts.ParentRunId = tc.RunId // 事件关联：子运行归属发起当前调用的运行
		// 审批继承：子运行内的工具同样需要人工确认；Spec.Mode 非 nil 时按
		// 模式包装（与 Session 组装同一入口 agents.WrapApprover）
		opts.Approver = agents.WrapApprover(tc.Approver, t.spec.Mode)
		// hooks 继承：父的 PreToolUse/PostToolBatch/PreCompact 透传子运行
		//（Spec.DisableHookInherit 可关——子 agent 有独立安全边界时）
		if tc.Hooks != nil && !t.spec.DisableHookInherit {
			opts.PreToolUse = tc.Hooks.PreToolUse
			opts.PostToolBatch = tc.Hooks.PostToolBatch
			opts.PreCompact = tc.Hooks.PreCompact
		}
	}
	if t.spec.Mode != nil {
		opts.Mode = t.spec.Mode // 子运行模式（A/C 维度：工具过滤 + 行为提示）
	}
	return opts
}

// runSync 同步路径（现状语义）：工具调用内阻塞等待子运行完成。
func (t *Tool) runSync(ctx context.Context, tc *events.ToolContext, childDepth int, task, name string) (string, error) {
	opts := t.buildOptions(tc, childDepth, name)
	var handler events.EventHandler
	if tc != nil {
		handler = tc.Handler // 事件转发：子事件流入父事件流
	}
	sub := t.spec.Loop.RunStream(ctx, []core.Message{
		core.NewUserMessage(core.Content{Type: "text", Content: task}),
	}, handler, opts)

	if sub.Err != nil {
		return "", sub.Err
	}

	t.mu.Lock()
	t.lastUsage = sub.Usage
	t.mu.Unlock()
	return sub.FinalResponse, nil
}

// runBackground 后台路径：取槽（池满则阻塞，见 slots.go）→ 起 goroutine → 立即返回 taskId。
// 子运行 ctx = Session 级（bgCtx()），父工具 Call 返回后不受影响；
// 消息接收 = Task.inbox → Poll drain（工具轮投递的延迟到子下个 stop 轮消费）。
func (t *Tool) runBackground(ctx context.Context, tc *events.ToolContext, childDepth int, task, name string) (string, error) {
	if t.bgCtx == nil {
		return "", fmt.Errorf("%s: background tool requires a session context provider", t.spec.Name)
	}
	opts := t.buildOptions(tc, childDepth, name)
	var handler events.EventHandler
	parentRun := ""
	if tc != nil {
		handler = tc.Handler
		parentRun = tc.RunId
	}

	// 闭包捕获 parentRun/handler/opts 均为外层确定值；task 经 Start 参数传入
	//（闭包引用返回值会撞「Start 返回前 goroutine 先跑」竞态）。
	// onStarted 在任务登记后、goroutine 启动前同步回调 —— TaskStarted（占位）必然先于
	// 子运行的 AgentStart 到达前端，任务卡片可用 task_id 确定性关联（停止按钮/结果回填）。
	onStarted := func(taskID string) {
		if handler != nil {
			handler(ctx, &events.TaskStarted{
				RunId:     parentRun,
				TaskId:    taskID,
				ToolName:  t.spec.Name,
				Name:      name,
				Async:     true,
				Status:    string(TaskRunning),
				Timestamp: time.Now(),
				EventType: events.TaskStartedType,
			})
		}
	}
	// journal 路径由 Registry 在任务构造期经解析器产出（StartOptions.JournalPath）；
	// 打开/写入全部在下方 run 闭包（后台 goroutine）内，spawn 同步路径零开销。
	taskRef, err := t.registry.StartToolWithOptions(t.bgCtx(), StartOptions{
		ToolName:    t.spec.Name,
		Name:        name,
		OnStarted:   onStarted,
		Slots:       t.spec.Slots, // nil = Registry 主池；explore 类工具用独立辅池
		JournalPath: t.spec.JournalPath,
	}, func(bg context.Context, tk *Task) (string, core.Usage, error) {
		// 任务 id 注入运行选项：AgentStart/AgentEnd 事件携带 task_id（前端关联锚；不依赖事件顺序）
		opts.TaskId = tk.ID
		// 消息接收：Task.inbox → Poll drain（stop 轮消费；工具轮投递延迟到下个 stop 轮）
		opts.Poll = func(context.Context) []core.Message {
			var msgs []core.Message
			for {
				select {
				case m := <-tk.inbox:
					msgs = append(msgs, core.NewUserMessage(core.Content{Type: "text", Content: m}))
				default:
					return msgs
				}
			}
		}
		// 非失败中断：OnRunning 捕获 ac.Abort → tk.abort（agent_interrupt 调用）
		opts.OnRunning = func(ac *agents.AgentContext) { tk.setAbort(ac.Abort) }

		// journal：打开失败降级 jr=nil（任务照跑）；start 记录；defer 就近收口。
		// 全部 Append 忽略错误 —— 进度落盘是尽力而为的旁路，绝不影响子运行本身
		//（主 Agent 防线：journal 环节零失败面传导到 spawn/子运行，docs §3.3）。
		var jr *TaskJournal
		if p := tk.OutputFile(); p != "" {
			if j, jerr := OpenTaskJournal(p); jerr == nil {
				jr = j
				defer j.Close()
				excerpt, _ := truncateRunes(task, 200)
				_ = j.Append(JournalRecord{
					Type: "start", Ts: time.Now(), TaskId: tk.ID,
					Name: name, ParentRunID: parentRun, Depth: childDepth,
					Text: excerpt,
				})
			}
		}

		// 用量增量入账 + turn 进度记录：包一层 handler（宿主转发不受影响）。
		// 子 agent 每轮 LLMEnd 实时累进 tk.Usage 并写一条 turn —— 压缩轮不经此
		// handler 发 LLMEnd（压缩器私有 provider 流，docs §1.4-1），计数即决策轮数。
		round := 0
		streamHandler := func(hctx context.Context, e events.Event) {
			if le, ok := e.(*events.LLMEnd); ok && le.Usage != nil {
				tk.addUsage(*le.Usage)
				if jr != nil {
					round++
					text, trunc := truncateRunes(le.Content, turnTextCap)
					_ = jr.Append(JournalRecord{
						Type: "turn", Ts: time.Now(), TaskId: tk.ID,
						Round: round, Text: text, Truncated: trunc,
						ToolNames: toolCallNames(le.ToolCalls),
						Usage:     le.Usage,
					})
				}
			}
			if handler != nil {
				handler(hctx, e)
			}
		}
		sub := t.spec.Loop.RunStream(bg, []core.Message{
			core.NewUserMessage(core.Content{Type: "text", Content: task}),
		}, streamHandler, opts)

		// 终态判定（TaskEnd 事件与 journal result 共用 classifySubEnd，防漂移）：
		// 中断请求优先于错误（abort 非失败）；被 AbandonAll 弃置的任务以 abandoned
		// 入账（journal 与权威终态一致；E7 竞窗内展示以 Registry 快照为准）。
		usage := sub.Usage
		tk.mu.Lock()
		status := classifySubEnd(sub.Err, tk.abortRequested)
		if tk.Status == TaskAbandoned {
			status = TaskAbandoned
		}
		tk.mu.Unlock()

		// result 记录：唯一 fsync 行（交付物兜底——推送失败时可经 TaskOutput 补取）
		if jr != nil {
			errText := ""
			if sub.Err != nil {
				errText = sub.Err.Error()
			}
			u := usage
			_ = jr.Append(JournalRecord{
				Type: "result", Ts: time.Now(), TaskId: tk.ID,
				Status: string(status), Text: sub.FinalResponse, Error: errText, Usage: &u,
			})
		}

		// TaskEnd 事件（Usage 单通道：成本只经 TaskEnd + Registry.TotalUsage 合成，
		// spawn/wait 工具不实现 ToolUsageProvider——双路径会双计）。Registry 在 run
		// 返回后写权威终态；这里按同一中断请求与错误提前发出一致的事件状态。
		if handler != nil {
			end := &events.TaskEnd{
				RunId:     sub.ParentRunId,
				TaskId:    tk.ID,
				Status:    string(status),
				Usage:     &usage,
				Timestamp: time.Now(),
				EventType: events.TaskEndType,
			}
			if sub.Err != nil {
				end.Error = sub.Err.Error()
			} else {
				end.Result = sub.FinalResponse
			}
			handler(bg, end)
		}
		if sub.Err != nil {
			return "", sub.Usage, sub.Err
		}
		return sub.FinalResponse, sub.Usage, nil
	})
	if err != nil {
		return "", err
	}
	// TaskStarted 已由 onStarted 回调在任务登记后、goroutine 启动前发出（先于任何子运行事件）。
	// S2-B：结果附带 hint 行为提示——模型只有 `{"status":"running"}` 时最自然的
	// 动作是"继续按自己的套路分析"（它没有"已委派"的概念）；hint 明确告知：
	// ① 任务目标（从任务描述提取首句，主 Agent 据此判断哪些范围会被覆盖、避免重复劳动）；
	// ② 后台运行、结果自动送达；③ 可用 TaskOutput 拉取进度（勿高频轮询）；④ 勿重复委派范围。
	hint := backgroundHint(name, task)
	return fmt.Sprintf(`{"task_id":%q,"status":"running","type":"subagent","name":%q,"output_file":%q,"hint":%q}`,
		taskRef.ID, t.spec.Name, taskRef.OutputFile(), hint), nil
}

// backgroundHint 构造后台任务提示（主 Agent 可见）。
// 设计原则（2026-08-30 二轮）：**模型拿到 hint 必须零思考知道该干嘛**——
// 过长/多句的 hint 会让模型犹豫（实测：有的模型收到长 hint 后 sleep）。
//   - 首句直接给行动指令："Continue your current work"（消除"要不要等"的歧义）；
//   - Goal 极简（extractGoal 首句 + 60 字符封顶）；
//   - 保留 TaskOutput 查询与勿重复提示（次句）。
func backgroundHint(name, task string) string {
	goal := extractGoal(task)
	if goal == "" {
		goal = "background analysis task"
	}
	return fmt.Sprintf(
		"Background task %q is running (Goal: %s). Continue your current work; its result will be pushed automatically when done — do not wait for it, poll sparingly via TaskOutput(%s) if needed, and do not duplicate its scope.",
		name, goal, "task_id")
}

// extractGoal 从任务描述提取目标（主 Agent hint 用，需极简——模型拿到 hint 要零思考）。
// 语言无关的确定性规则（不依赖任何语言的标点/句读）：
//  1. 取首行（换行前）——换行是跨语言通用的段落边界；
//  2. 首行内按句号断到第一句（中英句号都认，避免半截语义）；
//  3. 剥离常见角色/指令前缀（中英 best-effort，长前缀优先）；
//  4. rune 截断到 ~60 字符（hint 内嵌，过长会稀释"继续干活"的指令）。
//
// 返回 "" 表示无法提取（调用方回退通用文案）。
func extractGoal(task string) string {
	task = strings.TrimSpace(task)
	if task == "" {
		return ""
	}
	first := task
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i] // 首行
	}
	// 句号断句（rune 安全）：只留第一句
	runes := []rune(first)
	for i, r := range runes {
		if r == '。' || r == '.' || r == '！' || r == '!' {
			first = string(runes[:i+1])
			break
		}
	}
	first = strings.TrimSpace(first)
	// 长前缀优先（"你是一个" 先于 "你是" 匹配，避免剥出 "一个…"）
	for _, p := range []string{"你是一个", "你是", "You are", "Please", "请"} {
		if strings.HasPrefix(first, p) {
			first = strings.TrimSpace(strings.TrimPrefix(first, p))
			break
		}
	}
	if r, trunc := truncateRunes(first, 60); trunc {
		return r + "…"
	}
	return first
}

// classifySubEnd 子运行终态判定（TaskEnd 事件与 journal result 记录共用，防两处漂移）：
// 中断请求优先于错误（abort 非失败语义），无错即完成。abandoned 由调用方按 Registry 态覆盖。
func classifySubEnd(err error, abortRequested bool) TaskStatus {
	switch {
	case abortRequested:
		return TaskInterrupted
	case err != nil:
		return TaskFailed
	default:
		return TaskCompleted
	}
}

// Usage 返回最近一次子运行的用量（tools.ToolUsageProvider：引擎填进结果与事件）。
func (t *Tool) Usage() core.Usage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastUsage
}

func (t *Tool) AfterCall(context.Context, core.ToolCall) {}
