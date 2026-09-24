package events

import (
	"github.com/seven7628/hai-harness/core"
	"time"
)

// AgentStart 一次 agent 运行开始；Content 为首条用户输入摘要。
type AgentStart struct {
	Index   int64  `json:"index"` // 占位（恒 0；运行区分靠 RunId，无运行级序号）
	Content string `json:"content"`
	RunId   string `json:"run_id"` // 本次运行唯一标识（事件流关联/断点恢复锚点）
	Model   string `json:"model"`  // 运行模型
	// Timestamp 运行开始时间（与 AgentEnd.Timestamp 差即运行总时长）
	Timestamp time.Time `json:"timestamp"`

	// Name 运行的短标签（如子 agent 名，agent_spawn 的 name 注入）；空 = 无标签。
	// 前端用它作为子 agent 卡片/右侧树的展示名（回退到 Content）。
	Name string `json:"name,omitempty"`

	// TaskId 后台任务 id（异步 SubAgent / Explore 场景由 runBackground 注入）：前端据此
	// 把运行与任务卡片确定性关联（停止按钮 / 结果回填 / 去重），不依赖 task_started 事件
	// 的到达顺序。空 = 非后台任务 / 旧日志。
	TaskId string `json:"task_id,omitempty"`

	// ParentRunId 发起方运行的 RunId（子 agent 场景）；空 = 根运行。
	// Depth 运行深度（根 = 0）：父子事件流可沿此重建调用树。
	ParentRunId string `json:"parent_run_id,omitempty"`
	Depth       int    `json:"depth,omitempty"`

	// ContextWindow 该运行 loop 记录的上下文窗口（tokens）。前端占比分母的单一事实源：
	// 1M 标记热切换后客户端本地表可能滞后（问题六），以此为准。
	ContextWindow int64 `json:"context_window,omitempty"`

	EventType EventType `json:"event_type"`
}

func (a *AgentStart) Type() EventType {
	return a.EventType
}

// AgentEnd 一次 agent 运行结束：聚合本轮全部 LLM 轮次与工具执行的结果。
// 与 LLMEnd 的区别：LLMEnd 描述单次模型调用（一次 Run 可有多个），
// AgentEnd 是整次运行的结算 —— 最终回复、全部工具调用、跨轮累计用量与成本。
type AgentEnd struct {
	Index int64 `json:"index"` // 本轮迭代轮次数（第几轮 LLM 调用）
	// 注意：Index 语义与 AgentStart 不同（AgentStart 恒 0 占位，AgentEnd = 轮次）。
	// 运行与轮次的区分：RunId（运行级）+ Index（轮次级）。
	Content      string            `json:"content"`         // 最终回复（最后一轮 LLM 输出）
	Reasoning    string            `json:"reasoning"`       // 最终思考内容
	ToolCalls    []core.ToolCall   `json:"tool_calls"`      // 本轮所有工具调用（跨轮累计）
	FinishReason core.FinishReason `json:"finish_reason"`   // 结束原因：stop / tool_call / error
	Usage        *core.Usage       `json:"usage"`           // 本轮总用量（含 Cost）
	Error        string            `json:"error,omitempty"` // 失败原因（FinishReason=error 时）
	// ErrorKind 错误类型标签（provider.ClassifyError：rate_limit / permanent / canceled / generic），
	// 供上层区分「瞬时错误重试耗尽（可稍后重试）」vs「永久错误（配置/参数问题）」
	ErrorKind string `json:"error_kind,omitempty"`

	RunId     string    `json:"run_id"` // 与 AgentStart 一致，关联整条事件流
	Model     string    `json:"model"`
	Timestamp time.Time `json:"timestamp"` // 运行结束时间（与 AgentStart.Timestamp 差即运行时长）
	// DurationMS 是服务端计算的 AgentStart → AgentEnd 总耗时，避免前端时钟偏差。
	DurationMS int64 `json:"duration_ms,omitempty"`

	// TaskId 后台任务 id（与 AgentStart 同源；前端终态收敛兜底锚点）。
	TaskId string `json:"task_id,omitempty"`

	// ParentRunId / Depth 与 AgentStart 一致（子 agent 场景）。
	ParentRunId string `json:"parent_run_id,omitempty"`
	Depth       int    `json:"depth,omitempty"`

	// Artifacts 本轮运行产出的文件清单（write_file / edit_file 落盘；同路径合并，
	// 已按「created 优先 → 产出顺序」排列，超上限则截断并置 ArtifactsTruncated）。
	//
	// 语义边界：
	//   - 只含**成功**写入（IsError=false 的调用）；失败的写入不产生 Artifact；
	//   - 只含 write_file / edit_file；bash 落盘的文件不采集（无结构化路径，
	//     字符串解析会误报）；
	//   - abort / error 结束的运行**照常携带**——中断前写入的文件是真实产出；
	//   - 子 agent 运行的 AgentEnd 同样携带（宿主归属该子 agent 卡片）。
	// 空 = 本轮无产出（纯对话轮 / 旧日志），omitempty 保持既有事件体积。
	//
	// 为什么挂在 AgentEnd 而非独立事件：AgentEnd 已是提交边界（checkpoint.go
	// commitBoundary）与重放白名单（parseLegacyEvent）成员，加字段是**零新增注册**；
	// 独立事件需在 7 处登记且漏一处即静默失效。采集在发出前完成，不阻塞结算。
	Artifacts []core.Artifact `json:"artifacts,omitempty"`

	// ArtifactsTruncated 产出数超过上限（maxArtifacts）被截断，宿主提示残余数量。
	ArtifactsTruncated bool `json:"artifacts_truncated,omitempty"`

	EventType EventType `json:"event_type"`
}

func (a *AgentEnd) Type() EventType {
	return a.EventType
}

// 只用于客户端提示，不进入会话历史。
type GoalAlignmentReminder struct {
	RunId     string    `json:"run_id"`
	Round     int       `json:"round"`
	EventType EventType `json:"event_type"`
}

func (e *GoalAlignmentReminder) Type() EventType { return e.EventType }

// RunSilentEnd 一次「静默结束」run 的**自然结束**：模型跑完若干轮、可能还调过工具，
// 但最后一轮既没产出文本、也没发起工具调用 —— 即**没有交付最终答复** —— run 就结束了，
// 用户与宿主看不到任何最终回复。实测 85/2429 = 3.5% 的 run 以该形态收尾（其中全程无输出
// 31 次 / 1.3%）；B4 收尾门控不覆盖它（从未建过 todo ⇒ 不触发），此前在数据上完全不可见。
//
// 两个形态（靠 SawAssistantText 拆分）：
//   - false = 整次运行从未产出任何 assistant 文本（A1 形态，31/2429 ≈ 1.3%）；
//   - true  = 中途产出过文本、收尾轮却静默（C2 形态，「说了话、干了活、最后没交付」）。
//
// 判据在 C9（2026-09-18 主会话裁定）从「全程无文本」放宽为「收尾轮无文本」：当天 C2 委派
// （task-4mm4rucv）静默死亡，轨迹里 round 67 明确产出过文本、跑满 72 轮、改动落盘完整，
// 却没有送达任何报告 —— 按旧判据这次死亡根本不会被记录，而它恰恰是最需要被记录的形态。
//
// 语义边界（重要）：
//   - **这不是失败**：IsError / AgentEnd 语义都不改，本事件只把「静默结束」变成可见事实；
//   - 只在自然结束时发出：abort / ctx 取消（收尾属打断）与出错路径都不发；
//   - 子 agent 运行同样会发；归属靠同 RunId 的 agent_start / agent_end（本事件只带
//     RunId，不重复携带 ParentRunId/Depth —— 需要时按 RunId join 即可）。
//
// 为什么独立事件而非挂 AgentEnd：本项要的是**计数**（3.5% 量级的独立读数），
// 而 AgentEnd 的字段语义是「本次运行的结算」；且 AgentEnd 已在重放/归档链路里被多处消费，
// 加字段会牵动它们的假设。独立事件只需在此登记（未知类型在重放侧安全跳过）。
type RunSilentEnd struct {
	RunId string `json:"run_id"`
	// Rounds 本次 Run 执行的 LLM 决策轮数（与 AgentEnd.Index 同源；含收尾那个空轮）。
	Rounds int `json:"rounds"`
	// ToolCalls 本次 Run 的工具调用总数；HadToolCalls 为其布尔投影（便于直接聚合
	// 「调过工具却零输出」的子集，不必再解析数组长度）。
	ToolCalls    int  `json:"tool_calls"`
	HadToolCalls bool `json:"had_tool_calls"`
	// SawAssistantText 本 Run 是否**曾**产出过 assistant 文本（只升不降的粘滞位）。
	// 消费者按它拆分上面两个形态：false = 全程一句话没说（A1）、
	// true = 说过话但最后没交付（C2）。审计脚本按 `saw_assistant_text` 键取值。
	SawAssistantText bool      `json:"saw_assistant_text"`
	Timestamp        time.Time `json:"timestamp"`
	EventType        EventType `json:"event_type"`
}

func (e *RunSilentEnd) Type() EventType { return e.EventType }

// TaskStarted 后台任务启动（异步多 agent）：工具 Call 立即返回 taskId 时发出。
type TaskStarted struct {
	RunId       string    `json:"run_id,omitempty"` // 发起运行的 RunId
	TaskId      string    `json:"task_id"`
	ParentRunId string    `json:"parent_run_id,omitempty"`
	ToolName    string    `json:"tool_name,omitempty"`
	Name        string    `json:"name,omitempty"`
	Async       bool      `json:"async"`
	Status      string    `json:"status,omitempty"`
	Timestamp   time.Time `json:"timestamp"`

	EventType EventType `json:"event_type"`
}

func (t *TaskStarted) Type() EventType { return t.EventType }

// TaskMessage 后台任务收到消息（agent_send 投递成功时发出）。
type TaskMessage struct {
	RunId     string    `json:"run_id,omitempty"`
	TaskId    string    `json:"task_id"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`

	EventType EventType `json:"event_type"`
}

func (t *TaskMessage) Type() EventType { return t.EventType }

// TaskEnd 后台任务结束：Result 为最终回复文本、Error 为失败原因（nil 时为空）、
// Usage 为子运行总用量（成本传导的唯一通道——agent_spawn 工具不实现
// ToolUsageProvider，Session.Usage 经 Registry.TotalUsage 合成）。
type TaskEnd struct {
	RunId     string      `json:"run_id,omitempty"`
	TaskId    string      `json:"task_id"`
	Status    string      `json:"status"`
	Result    string      `json:"result,omitempty"`
	Error     string      `json:"error,omitempty"`
	Usage     *core.Usage `json:"usage,omitempty"`
	Timestamp time.Time   `json:"timestamp"`

	EventType EventType `json:"event_type"`
}

func (t *TaskEnd) Type() EventType { return t.EventType }

// TaskResultDelivered 后台任务结果已推入主会话上下文（Session.PushTaskResult 发出）。
// 宿主渲染为可折叠 Markdown 块（类似思考过程）：默认折叠、可展开看全文。
// Result 是**不透明字符串**（纯文本 / HTML / 脚本等 LLM 输出全量，不截断）。
// Usage 是该任务全量用量（含成本 µUSD；nil = 无用量信息，如不花模型钱的普通后台工具
// 任务）——与 TaskEnd.Usage 同源（push 路径原本只有文本，宿主此前拿不到任务成本）。
type TaskResultDelivered struct {
	SessionId string      `json:"session_id"`
	TaskId    string      `json:"task_id"`
	Name      string      `json:"name,omitempty"`   // 任务短标签（这个 agent 在干什么；空=工具任务）
	Status    string      `json:"status"`           // 已注入主会话的任务终态
	Result    string      `json:"result,omitempty"` // Markdown 正文（全量）
	Error     string      `json:"error,omitempty"`
	Usage     *core.Usage `json:"usage,omitempty"`
	Timestamp time.Time   `json:"timestamp"`
	EventType EventType   `json:"event_type"`
}

func (t *TaskResultDelivered) Type() EventType { return t.EventType }
