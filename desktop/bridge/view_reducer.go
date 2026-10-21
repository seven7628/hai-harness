// view_reducer.go —— bridge 内 per-session ViewReducer 与稳定 ViewCheckpoint 投影。
//
// 目标架构（docs/SESSION_RECOVERY_FIX_2026-08-26.md §3.1.3）：
//   - 每个 bridgeSession 持有一个 ViewReducer 和一个 ordered dispatcher；
//   - SDK typed event 串行进入 Apply，chunk 只更新内存聚合 buffer，语义事件更新稳定视图；
//   - Snapshot() 产出版本化 Go/JSON checkpoint（不序列化前端 zustand 对象）；
//   - chunk 只允许相邻同 key 合并；任何非 chunk 事件先 flush；
//   - 同一事件重送按稳定键去重；未知事件保留为受约束的 diagnostic，不静默改变稳定 view；
//   - 审批、提问、沙箱放行、流式/计时器、运行中压缩等瞬时交互态不进入 checkpoint。
//
// 本文件是独立实现：不修改 bridgeSession.emit、commit barrier 或恢复入口（后续阶段接入）。
package main

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/session"
)

// checkpointSchemaVersion 与 storage envelope 的 ViewCheckpointSchemaVersion 保持一致；
// storage 层校验 view_checkpoint.schema_version == 1。
const checkpointSchemaVersion = session.ViewCheckpointSchemaVersion

// ---------------------------------------------------------------------------
// Checkpoint 类型（bridge-owned，版本化 Go/JSON schema）
// ---------------------------------------------------------------------------

// Checkpoint 是 ViewReducer.Snapshot() 的稳定可见投影。// 字段命名与 docs/SESSION_RECOVERY_FIX_2026-08-26.md §3.1.1 示例一致；
// 额外字段（lifetime_usage / lifetime_cost_usd / ctx_tokens / context_window /
// diffs / diagnostics）属于兼容扩展，storage 层只要求
// schema_version + blocks + run_nodes + turns。
type Checkpoint struct {
	SchemaVersion int `json:"schema_version"`

	Blocks   []Block   `json:"blocks"`
	RunNodes []RunNode `json:"run_nodes"`
	Turns    []Turn    `json:"turns"`

	// usage / cost_usd 是当前 clear generation 的展示累计（clear 后归零）；
	// lifetime_usage / lifetime_cost_usd 是跨 generation 的对账值（clear 不清）。
	Usage           Usage   `json:"usage"`
	CostUSD         float64 `json:"cost_usd"`
	LifetimeUsage   Usage   `json:"lifetime_usage"`
	LifetimeCostUSD float64 `json:"lifetime_cost_usd"`

	Todos []TodoItem `json:"todos"`

	CtxTokens     int64 `json:"ctx_tokens,omitempty"`     // 主 agent 当前上下文占用（llm_end Input 结算 / compress_end 估算）
	ContextWindow int64 `json:"context_window,omitempty"` // harness 上报窗口（agent_start）
	// CtxTokensEstimated 标记 CtxTokens 的口径：true = 值来自 compress_end 的字符/token
	// 估算（不是 provider 报的真实 usage）。两个写入点口径不同，展示层必须能区分，
	// 否则「估算」会被当成真实占用（面板校准的锚点也会跟着失真）。
	CtxTokensEstimated bool `json:"ctx_tokens_estimated,omitempty"`

	Diffs []Diff `json:"diffs,omitempty"` // 工具 diff 汇总（按事件顺序）

	LastRun   *LastRun   `json:"last_run,omitempty"`
	LastError *LastError `json:"last_error,omitempty"`

	Degraded    bool         `json:"degraded"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"` // 未知/降级事件的受约束记录（有界）
}

// Block 是对话/任务时间线的单块稳定投影。所有可选字段按 kind 解释；
// 不保存 streaming 句柄、审批/提问/放行等瞬时交互态（streaming 恒 false）。
type Block struct {
	Kind string `json:"kind"` // user|assistant|tool|agent|async_task|task_result|task_delivery|compression|system|error|divider
	ID   uint64 `json:"id"`

	// divider（上下文隔离分隔线）：进程关闭/重启（shutdown）或用户中断（abort）时
	// 插入的「横线」标记。UI 统一渲染为分隔线，内部按 DividerReason 区分语义。
	DividerReason string `json:"divider_reason,omitempty"` // divider: shutdown|abort

	// user / assistant / system / error
	Text     string    `json:"text,omitempty"`
	Thinking string    `json:"thinking,omitempty"`
	Contents []Content `json:"contents,omitempty"` // user 附件（图片等）
	// Images 工具结果携带的图片内容块（read_file 读图 / 截图 / MCP image）：
	// 随 tool_run_end 事件到达，checkpoint 落盘 → 恢复时 UI 仍能展示「模型看过的图」
	//（data URL 直显；不参与 LLM 上下文——那是 AgentLoop 侧的 ToolResult.Blocks）。
	Images    []Content `json:"images,omitempty"`
	Model     string    `json:"model,omitempty"`
	RunID     string    `json:"run_id,omitempty"`
	RequestID string    `json:"request_id,omitempty"`
	Ts        int64     `json:"ts,omitempty"` // 事件时间戳（unix ms；缺失回退接收时刻）
	Tone      string    `json:"tone,omitempty"`

	// error 块的重试进度（llm_error 事件，2026-09-18）：前端据此把「第 N/M 次失败 →
	// 重试第 N+1/M 次」渲染成实时状态 —— 退避倒计时（Ts + RetryDelayMs 之前）与
	// 「本次尝试已进行 Xs」（之后）。落进 JSON 而非只留内存：刷新/重放后倒计时仍在。
	Retrying       bool  `json:"retrying,omitempty"`
	RetryAttempt   int   `json:"retry_attempt,omitempty"`
	RetryMax       int   `json:"retry_max_attempts,omitempty"`
	RetryDelayMs   int64 `json:"retry_delay_ms,omitempty"`
	RetryStartedAt int64 `json:"retry_started_at,omitempty"` // 本轮重试首个失败时刻（累计「已等待时长」锚点）
	// RetryCostUSD 本轮重试**已花费**金额（USD，2026-09-23 决策「失败尝试也要记账」）：
	// 失败尝试的已产生用量（上游按已生成 token 计费）折算而来，跨多次重试累加。
	// 落 JSON：刷新/重放后成本不丢（与 RetryStartedAt 同一处累计）。
	RetryCostUSD float64 `json:"retry_cost_usd,omitempty"`

	// assistant 时序结算值
	ThinkMs    int64 `json:"think_ms,omitempty"`
	TtftMs     int64 `json:"ttft_ms,omitempty"`
	DurMs      int64 `json:"dur_ms,omitempty"`
	AgentDurMs int64 `json:"agent_dur_ms,omitempty"`

	// assistant 本轮输出 token（llm_end 结算；Output 含 Reasoning）。渲染层据此展示
	// 输出速度（tokens/s）——必须落库：否则刷新/重放后该行数字消失（此前仅存内存）。
	OutputTokens    int64 `json:"output_tokens,omitempty"`
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
	// ThinkingSummarized：本轮 reasoning 是摘要形态（Anthropic display=summarized）——
	// 输出速度的分母必须用整轮耗时（思考 token 已计入 Output，但生成时间不在首字之后）。
	// 落库：刷新/重放后 genMsOf 仍能按同一口径重算（否则 tokens/s 刷新后跳变）。
	ThinkingSummarized bool `json:"thinking_summarized,omitempty"`

	// tool
	ToolID     string         `json:"tool_id,omitempty"`
	Name       string         `json:"name,omitempty"`
	Args       string         `json:"args,omitempty"`
	Result     string         `json:"result,omitempty"`
	IsError    bool           `json:"is_error,omitempty"`
	ErrorInfo  *ToolErrorInfo `json:"error_info,omitempty"`
	Status     string         `json:"status,omitempty"` // tool: running|done; task: running|completed|failed|interrupted|abandoned
	Diff       *Diff          `json:"diff,omitempty"`
	TaskID     string         `json:"task_id,omitempty"`
	Promoted   bool           `json:"promoted,omitempty"`
	TaskStatus string         `json:"task_status,omitempty"`
	// Todos 待办清单快照，按 kind 解释（同名字段、两种宿主）：
	//   · tool 块 → 该次 todo_* 调用完成时刻的**会话**清单（历史计划卡定格）
	//   · agent 块 → 该子 agent **自己的**清单（与顶层 Checkpoint.Todos 分开存：
	//     子 agent 有独立 TODO，两者不得串味）
	Todos []TodoItem `json:"todos,omitempty"`
	// NestedCalls 编排型工具（codemode）脚本内的嵌套调用摘要（与 AgentItem 同名字段同义）。
	// 只出现在编排型工具那一块上；宿主折叠态只显示最后 8 条（desktop/app 的 pickNestedCalls）。
	NestedCalls []NestedCall `json:"nested_calls,omitempty"`
	// HostOps 编排型工具（codemode）脚本内发起的保留操作计数（见 AgentItem.HostOps 的注释）。
	HostOps   *core.HostOps `json:"host_ops,omitempty"`
	StartedAt int64         `json:"started_at,omitempty"`

	// Settled 是内部结算标记（不序列化）：assistant 块收到 llm_end / tool 块收到
	// tool_end 后置 true。归一化悬空态时据此区分「已结算但无正文」（纯工具调用轮）
	// 与「真悬空」（llm_start 无 llm_end 的中断残留）——前者不得标成已中断。
	Settled bool `json:"-"`

	// agent
	Label string `json:"label,omitempty"`
	// ParentRunID 该 agent 卡片的父 run（agent_start.ParentRunId）：左栏子智能体树的
	// 父子锚点。runNodes 里也有（但有界收敛/裁剪后不一定在），块级字段是更稳的来源。
	ParentRunID string      `json:"parent_run_id,omitempty"`
	SpawnedAt   int64       `json:"spawned_at,omitempty"`
	EndedAt     int64       `json:"ended_at,omitempty"`
	Usage       Usage       `json:"usage,omitempty"`
	Items       []AgentItem `json:"items,omitempty"`
	// ItemsTrimmed 表示该 agent 卡片的历史 items 被裁剪掉的条数
	//（UI 可提示「更早的记录未保留」）。见 maxAgentItemsPerBlockDefault。
	ItemsTrimmed    int    `json:"items_trimmed,omitempty"`
	TaskResult      string `json:"task_result,omitempty"`
	TaskError       string `json:"task_error,omitempty"`
	DeliveredToMain bool   `json:"delivered_to_main,omitempty"`

	// compression
	Before    int      `json:"before,omitempty"`
	After     int      `json:"after,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	Summary   string   `json:"summary,omitempty"`
	CtxTokens int64    `json:"ctx_tokens,omitempty"`
	Error     string   `json:"error,omitempty"`
	Aborted   bool     `json:"aborted,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
	Analysis  string   `json:"analysis,omitempty"` // 压缩器 <analysis> 块留档（O1/G7，仅观测）

	// artifacts（本轮产出汇总，AgentEnd 携带）：宿主渲染「本次产出」卡片。
	// 独立块而非挂 assistant —— assistant 块在部分运行里不存在（纯工具轮后 abort），
	// 挂靠会丢展示；与 divider 同属「运行结束追加标记块」的既有先例。
	Artifacts          []ArtifactRef `json:"artifacts,omitempty"`
	ArtifactsTruncated bool          `json:"artifacts_truncated,omitempty"`
}

// NestedCall 是 core.NestedCallRecord 的稳定投影（checkpoint schema，独立于 core 的演进）。
// 字段与 core.NestedCallRecord 一一对应：args/result 已由引擎按 8 KiB 截断、error 按 500 字符
// 截断（本层不再截断，宿主渲染时另按 200/500 字符做预览）。
// Duration 单位纳秒（time.Duration 的原生单位）—— 宿主换算成毫秒展示。
type NestedCall struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Args     string `json:"args,omitempty"`
	Result   string `json:"result,omitempty"`
	IsError  bool   `json:"is_error,omitempty"`
	Error    string `json:"error,omitempty"`
	Duration int64  `json:"duration,omitempty"`
}

// nestedCalls 把 core.NestedCallRecord 投影成 checkpoint 形态（无记录 = nil，不落空数组）。
func nestedCalls(in []core.NestedCallRecord) []NestedCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]NestedCall, 0, len(in))
	for _, r := range in {
		out = append(out, NestedCall{
			ID: r.Id, Name: r.Name, Args: r.Args, Result: r.Result,
			IsError: r.IsError, Error: r.Error, Duration: int64(r.Duration),
		})
	}
	return out
}

// ArtifactRef 是 core.Artifact 的稳定投影（checkpoint schema，独立于 core 的演进）。
// 字段与 core.Artifact 一一对应；只保留宿主展示所需（不含工作区外不可达信息）。
type ArtifactRef struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Kind    string `json:"kind"` // created | modified
	Added   int    `json:"added,omitempty"`
	Removed int    `json:"removed,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Lines   int64  `json:"lines,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

// AgentItem 是子 agent 卡片内的活动记录项。
type AgentItem struct {
	Kind       string `json:"kind"` // text|thinking|tool|compression
	Text       string `json:"text,omitempty"`
	IsError    bool   `json:"is_error,omitempty"` // text item: LLM 失败提示
	ThinkStart int64  `json:"think_start,omitempty"`

	ToolID    string         `json:"tool_id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Args      string         `json:"args,omitempty"`
	Status    string         `json:"status,omitempty"` // tool item: running|done|error
	Result    string         `json:"result,omitempty"`
	ErrorInfo *ToolErrorInfo `json:"error_info,omitempty"`
	DurMs     int64          `json:"dur_ms,omitempty"`
	StartedAt int64          `json:"started_at,omitempty"`
	TaskID    string         `json:"task_id,omitempty"`
	Diff      *Diff          `json:"diff,omitempty"`
	Images    []Content      `json:"images,omitempty"` // tool item: 结果图片（read_file 读图 / 截图）
	// NestedCalls 编排型工具（codemode）脚本内发生的嵌套调用摘要（core.NestedCallRecord 的
	// 稳定投影）。子调用**没有自己的工具行/工具项**，明细只挂在外层调用这一处 ——
	// 宿主在行内联成清单渲染（设计文档 §17 第二条）。
	NestedCalls []NestedCall `json:"nested_calls,omitempty"`
	// HostOps 编排型工具（codemode）脚本内发起的**保留操作**计数（searchTools/describeTool）。
	// 与 NestedCalls 的区别：保留操作不是工具调用，纯检索脚本的 NestedCalls 恒为空 ——
	// 宿主只能靠这里的计数把工具行标成「Codemode Search」（见 core.HostOps）。
	HostOps *core.HostOps `json:"host_ops,omitempty"`
	// Todos todo_* 工具项那一刻**该子 agent 自己**的清单快照（与块级 Todos 同源，
	// 后者是「最近一次」的滚动值；这里定格本次调用，历史计划卡才显示当时状态）。
	Todos []TodoItem `json:"todos,omitempty"`

	// compression item
	Before    int    `json:"before,omitempty"`
	After     int    `json:"after,omitempty"`
	CtxTokens int64  `json:"ctx_tokens,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Model     string `json:"model,omitempty"`
	Summary   string `json:"summary,omitempty"`
	// Usage 本项用量：compression item = 该次压缩调用；tool item = 该次调用
	// （仅子 agent 类工具非零）。含成本（CostUSD，USD）。
	Usage    *Usage `json:"usage,omitempty"`
	Error    string `json:"error,omitempty"`
	Aborted  bool   `json:"aborted,omitempty"`
	Active   bool   `json:"active,omitempty"`
	Analysis string `json:"analysis,omitempty"` // 压缩器 <analysis> 块留档（仅观测）
}

// RunNode 是 run/tool 层级节点（链路树）。
type RunNode struct {
	ID        string `json:"id"`
	ParentID  string `json:"parent_id,omitempty"`
	Label     string `json:"label"`
	Status    string `json:"status"` // running|done|error|interrupted
	Depth     int    `json:"depth"`
	Kind      string `json:"kind"` // run|tool
	Name      string `json:"name,omitempty"`
	Args      string `json:"args,omitempty"`
	Result    string `json:"result,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	StartedAt int64  `json:"started_at,omitempty"`
	DurMs     int64  `json:"dur_ms,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	Promoted  bool   `json:"promoted,omitempty"`
}

// Turn 是单次 LLM 调用（llm_end 一条）的指标明细。
type Turn struct {
	Model        string  `json:"model"`
	Input        int64   `json:"input"`
	Output       int64   `json:"output"`
	CacheRead    int64   `json:"cache_read"`
	CacheWrite   int64   `json:"cache_write"` // 本轮写入缓存量（Anthropic cache_creation；OpenAI 系恒 0）
	Reasoning    int64   `json:"reasoning"`
	DurMs        int64   `json:"dur_ms,omitempty"`
	ThinkMs      int64   `json:"think_ms,omitempty"`
	TtftMs       int64   `json:"ttft_ms,omitempty"`
	CostUSD      float64 `json:"cost_usd"`
	RunID        string  `json:"run_id,omitempty"`
	RequestID    string  `json:"request_id,omitempty"`
	FinishReason string  `json:"finish_reason,omitempty"`
}

// Usage 是 checkpoint 内的 token 展示累计（与 core.Usage 的 JSON 大小写名区分）。
// CostUSD 是**卡片级**成本（USD）：仅任务卡（agent/async_task/promoted tool）填充——
// 会话级总成本另由 ViewState.cost_usd 承载（addUsage 不搬 CostUSD，杜绝双计）。
type Usage struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	CacheRead int64 `json:"cache_read"`
	// CacheWrite 累计写入缓存量（Anthropic cache_creation_input_tokens；OpenAI 系端点无此
	// 计数恒 0）。与 CacheRead 并列上抛：命中率之外，「这轮是在读缓存还是在重写缓存」只有
	// 写入量能回答 —— 缓存前缀一旦失效，CacheRead 归零而 CacheWrite 顶到整段上下文。
	CacheWrite int64 `json:"cache_write"`
	// CacheWrite1h ⊆ CacheWrite：1h extended TTL 写入量（Anthropic 专用；其余端点 0）。
	// 状态栏缓存 tooltip 据此区分 5m/1h 两种缓存规模（2026-09-23 补齐维度）。
	CacheWrite1h int64 `json:"cache_write_1h"`
	Reasoning    int64 `json:"reasoning"`
	// CostUSD 卡片级成本（USD，= core.Cost.Total µUSD / 1e6）：任务结束（TaskEnd /
	// task_result_delivered）时按任务全量权威写入；子 agent 卡片在运行中按每轮
	// llm_end 增量累加（结束时的权威值含压缩轮，会覆盖增量）。
	CostUSD float64 `json:"cost_usd,omitempty"`
}

// TodoItem 与会话 todo.Item 对齐（checkpoint 持展示副本；SDKState.todo 是权威）。
type TodoItem struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Done      bool     `json:"done"`
	BlockedBy []string `json:"blocked_by,omitempty"`
}

// Diff 是工具变更的稳定投影（core.FileDiff 对齐）。
type Diff struct {
	Path      string `json:"path"`
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
	Unified   string `json:"unified,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ToolErrorInfo 是工具错误提示的稳定投影。
type ToolErrorInfo struct {
	Code       string `json:"code,omitempty"`
	Changed    string `json:"changed,omitempty"`
	Retryable  bool   `json:"retryable,omitempty"`
	NextAction string `json:"next_action,omitempty"`
	Recovery   string `json:"recovery,omitempty"`
}

// Content 是用户消息内容块（text/image 附件）。
type Content struct {
	Type     string `json:"type"`
	Content  string `json:"content"`
	MimeType string `json:"mime_type,omitempty"`
}

// LastRun 是最后一次 agent run 的结算摘要。
type LastRun struct {
	RunID        string `json:"run_id,omitempty"`
	Status       string `json:"status"` // done|error|interrupted
	FinishReason string `json:"finish_reason,omitempty"`
	TaskID       string `json:"task_id,omitempty"`
	EndedAt      int64  `json:"ended_at,omitempty"`
}

// LastError 是最后一次运行/LLM 错误摘要。
type LastError struct {
	Message string `json:"message"`
	Kind    string `json:"kind,omitempty"`
	At      int64  `json:"at,omitempty"`
}

// Diagnostic 是未知/降级事件的受约束记录（有界、不参与稳定视图）。
type Diagnostic struct {
	EventType string `json:"event_type"`
	Count     int    `json:"count"`
	FirstSeq  uint64 `json:"first_seq,omitempty"`
	LastSeq   uint64 `json:"last_seq,omitempty"`
}

// EventMeta 是 dispatcher 在 Apply 时附加的宿主注解：
//   - Provider：cost 价表维度（与 metrics.go costUsd 同表）；
//   - CostUSD：宿主已按价表计算好的单次成本（>0 时优先）；
//   - Todos：todo_* 工具 / compress_end 时刻的待办快照（bridge emit 注入）。
type EventMeta struct {
	Provider string
	CostUSD  float64
	Todos    []TodoItem
}

// ---------------------------------------------------------------------------
// Block kind / status 常量
// ---------------------------------------------------------------------------

const (
	blockKindUser         = "user"
	blockKindAssistant    = "assistant"
	blockKindTool         = "tool"
	blockKindAgent        = "agent"
	blockKindAsyncTask    = "async_task"
	blockKindTaskResult   = "task_result"
	blockKindTaskDelivery = "task_delivery"
	blockKindCompression  = "compression"
	blockKindSystem       = "system"
	blockKindError        = "error"
	blockKindDivider      = "divider"
	blockKindArtifacts    = "artifacts"

	agentItemText        = "text"
	agentItemThinking    = "thinking"
	agentItemTool        = "tool"
	agentItemCompression = "compression"
	// agentItemUser 子运行上下文里的一条 **user 消息**（用户/模型 agent_send、孙任务结果回投）。
	// 出自 events.AgentUserMessage（在消息被投入上下文的那一刻发出）—— 子会话的转录因此
	// 与主会话同构：user / assistant / tool / compression 四种块都有。
	agentItemUser = "user"

	runNodeKindRun  = "run"
	runNodeKindTool = "tool"

	statusRunning     = "running"
	statusDone        = "done"
	statusError       = "error"
	statusInterrupted = "interrupted"
	statusAbandoned   = "abandoned"
	statusCompleted   = "completed"
	statusFailed      = "failed"

	// maxDiagnostics 未知事件的诊断上限（超出按 FirstSeq 淘汰最旧）。
	maxDiagnostics = 64

	// maxAgentItemsPerBlockDefault 是**已完成** agent block 保留的 item 上限。
	//
	// 背景（实测 2026-10-18，120 轮子 agent 语料）：线上长会话 record 达 128.6MB，
	// 其中 98% 是 blocks[kind=agent].items（子智能体的 tool/thinking/text 历史流水）。
	// 这些条目只用于「展开子智能体卡片看历史」；reducer 只在 block **进行中**
	// 才按 tool_id 查找/追加（hasAgentToolItem），agent_end 之后不再写入 ——
	// 所以终态后裁剪是安全的，且不影响 SDK 续跑（续跑走 sdk_state）。
	//
	// 膝点实测（视图 / commit 耗时 / 提升）：
	//   不裁剪 128.6MB / 6025ms / 1.0x
	//   200     62.0MB / 2714ms / 2.2x
	//   100     31.7MB / 1082ms / 5.6x
	//    50     16.6MB /  568ms / 10.6x   ← 选它
	//    20      7.5MB /  285ms / 21.2x
	//
	// **这不是截断，是首屏**：更早的记录通过 agent_history 命令从子任务
	// journal 分页取回（见 agent_history.go，实测 23MB journal 首页读取 13ms）。
	// UI 展开卡片时按 offset/limit 拉取即可看到全量；
	// Block.ItemsTrimmed > 0 表示「还有更早的，可加载」。
	maxAgentItemsPerBlockDefault = 50
)

// ---------------------------------------------------------------------------
// ViewReducer
// ---------------------------------------------------------------------------

// ViewReducer 是单个 session 的稳定视图 reducer。
//
// 并发约束：同一 reducer 的 Apply/Snapshot/Restore/Clear/NormalizeForRestore
// 必须由 per-session dispatcher 串行调用；内部仍用互斥锁防御误用，保证
// Snapshot 不会与 Apply 交错产生半写状态。
type ViewReducer struct {
	mu sync.Mutex

	seq             uint64 // 已应用的 reducer 顺序号（dispatcher 单调分配）
	clearGeneration uint64
	degraded        bool

	// marshalCache 缓存最近一次 Marshal 的结果（seq/generation 相同则复用）。
	// 动机：Marshal 持锁序列化整份 checkpoint（线上 84MB，数十 ms），
	// 而导航/保存路径可能在没有任何新事件时反复调用它 —— 每次都是同一份字节。
	// 失效：任何 Apply（seq 递增）、Restore、NormalizeForRestore。
	marshalCache    []byte
	marshalCacheSeq uint64
	marshalCacheGen uint64
	marshalCacheOK  bool

	// blockBytes 按 block id 缓存序列化字节（配合 dirtyBlocks 增量重算）。
	blockBytes map[uint64][]byte
	// dirtyBlocks 本轮内容变化的 block id（由写入方精确标记）。
	dirtyBlocks map[uint64]struct{}
	// cachedSpans 与 marshalCache 同代：每个 block 的字节（与 view 中片段逐字节相同）
	cachedSpans [][]byte

	blocks        []Block
	runNodes      []RunNode
	turns         []Turn
	usage         Usage
	lifetime      Usage
	costUSD       float64
	lifetimeCost  float64
	todos         []TodoItem
	lastRun       *LastRun
	lastError     *LastError
	ctxTokens     int64
	ctxTokensEst  bool // ctxTokens 口径：true = compress_end 字符估算（非真实 usage）
	contextWindow int64
	diffs         []Diff
	diag          map[string]*diagEntry // event_type → 计数（有界，序列化时展开）

	// 稳定去重集合（内存态；Restore 后清空——恢复之后到达的都是新事件）
	llmEndSeen        map[string]bool
	llmErrorSeen      map[string]bool
	toolStartSeen     map[string]bool
	toolEndSeen       map[string]bool
	approvalSeen      map[string]bool
	taskStartedSeen   map[string]bool
	taskEndSeen       map[string]bool
	taskResultSeen    map[string]bool
	agentStartSeen    map[string]bool
	agentEndSeen      map[string]bool
	compressEndSeen   map[string]bool
	commandResultSeen map[string]bool
	userInputsSeen    map[string]bool

	// chunk 聚合（仅相邻同 key 合并；非 chunk 事件先 flush）
	chunk *chunkAgg

	// 请求 → assistant 块映射（流式路由 + llm_end 结算）
	reqState map[string]*reqState
	// runID（"" = 主 agent）→ 子 agent 卡片块 id
	agentBlockID map[string]uint64
	// toolID → 主 agent 工具块 id（子 agent 工具在卡片 items 内按 toolId 查）
	toolBlockID map[string]uint64

	// 计时瞬态（仅结算使用，不持久化）
	thinkStart       map[string]time.Time // runID → 首个 reasoning chunk 时间
	turnThinkMs      map[string]int64     // runID → 已结算思考耗时
	turnStart        map[string]time.Time // runID → 当前 LLM 轮 llm_start 时刻（末轮无 llm_end 时兜底算「已跑多久」）
	toolStartTs      map[string]time.Time // toolID → 工具开始时间
	compStart        map[string]time.Time // runID（""=主）→ 压缩开始时刻
	agentCompressing map[string]bool      // runID → 子 agent 压缩进行中
	mainCompressing  bool                 // 主 agent 压缩进行中（瞬态）

	// blockIdx 是 blocks 的 id→下标索引（性能）：chunk flush / llm_end / tool_end
	// 高频按 id 定位块，避免长会话线性扫描。批量替换（filter/clear/restore）标脏，
	// find 时惰性重建；append 新块记录下标；miss 仍有线性兜底保证正确性。
	blockIdx      map[uint64]int
	blockIdxDirty bool

	blockSeq uint64
}

// diagEntry 是诊断计数内部表示。
type diagEntry struct {
	count    int
	firstSeq uint64
	lastSeq  uint64
}

// chunkAgg 是相邻同 key chunk 的聚合缓冲。
type chunkAgg struct {
	kind      string // "reasoning" | "content"
	runID     string
	requestID string
	content   string
	firstTs   time.Time
	blockID   uint64
}

// reqState 是单次 LLM 请求的结算状态。
type reqState struct {
	runID        string
	model        string
	start        time.Time
	blockID      uint64
	ttftMs       int64
	thinkSettled bool
	textSettled  bool
}

// NewViewReducer 创建空 reducer（seq 后续由 Apply 从 0 递增；Restore 可设基线）。
func NewViewReducer() *ViewReducer {
	return newReducer(0, 0)
}

func newReducer(seq, clearGeneration uint64) *ViewReducer {
	return &ViewReducer{
		seq:               seq,
		clearGeneration:   clearGeneration,
		blocks:            make([]Block, 0),
		runNodes:          make([]RunNode, 0),
		turns:             make([]Turn, 0),
		diag:              map[string]*diagEntry{},
		llmEndSeen:        map[string]bool{},
		llmErrorSeen:      map[string]bool{},
		toolStartSeen:     map[string]bool{},
		toolEndSeen:       map[string]bool{},
		approvalSeen:      map[string]bool{},
		taskStartedSeen:   map[string]bool{},
		taskEndSeen:       map[string]bool{},
		taskResultSeen:    map[string]bool{},
		agentStartSeen:    map[string]bool{},
		agentEndSeen:      map[string]bool{},
		compressEndSeen:   map[string]bool{},
		commandResultSeen: map[string]bool{},
		userInputsSeen:    map[string]bool{},
		reqState:          map[string]*reqState{},
		agentBlockID:      map[string]uint64{},
		toolBlockID:       map[string]uint64{},
		thinkStart:        map[string]time.Time{},
		turnThinkMs:       map[string]int64{},
		turnStart:         map[string]time.Time{},
		toolStartTs:       map[string]time.Time{},
		compStart:         map[string]time.Time{},
		agentCompressing:  map[string]bool{},
		blockIdx:          map[uint64]int{},
	}
}

// Apply 串行应用一个 typed SDK 事件；分配并递增 reducer_seq。
// meta 由 dispatcher 注入宿主注解（provider/todos/cost）。
func (r *ViewReducer) Apply(ev events.Event, meta EventMeta) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	r.applyLocked(ev, meta)
	// 防御性失效：缓存判据已含 seq，但显式清空以免依赖「seq 必递增」的隐含前提。
	r.invalidateMarshalCache()
	// 逐块字节缓存：只有「本轮可能被改动的块」需要失效。
	//
	// 不能整体清空（那样每次 Apply 都要重新序列化 86MB，优化归零），
	// 也不能不清（终态块若被原地改就会返回旧字节）。
	//
	// 判据：applyLocked 完成后，凡是**内容可能变化**的块都失效：
	//   · 非终态块（running/interrupting）—— 下一轮反正会重新序列化，无需处理；
	//   · 本轮被改动过的块 —— 通过与 before 比较指针/长度无法可靠判断，
	//     因此采用「保守但正确」的做法：**失效所有非终态块 + 新增块**，
	//     终态块保持缓存。
	//
	// 终态块「内容不再变化」是 reducer 的写模型保证（所有写入路径都只改活动块，
	// 见 view_reducer 各处 toolBlockID / running 块查找）。任何破例都必须
	// 在这里补失效 —— TestIncrementalMarshalMatchesFull 覆盖了主要事件类型。
	return nil
}

// ApplyPlain 是 Apply 的便捷形式（无宿主注解），供测试与无条件路径使用。
func (r *ViewReducer) ApplyPlain(ev events.Event) error {
	return r.Apply(ev, EventMeta{})
}

// LastSeq 返回已应用的最大 reducer 顺序号（commit barrier 用）。
func (r *ViewReducer) LastSeq() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq
}

// ClearGeneration 返回当前视图代际。
func (r *ViewReducer) ClearGeneration() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clearGeneration
}

// MarkDegraded 标记 checkpoint 为降级恢复（由恢复/提交路径在检测到不完整时调用）。
func (r *ViewReducer) MarkDegraded(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.degraded = true
	r.noteDiagLocked("degraded:"+reason, 1)
}

// IsDegraded 返回当前降级标记。
func (r *ViewReducer) IsDegraded() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.degraded
}

// Clear 处理 clear 语义：视图代际 +1，重置当前 generation 的 blocks/run/turn/
// 展示 usage/cost/diffs/错误摘要，保留 todos（会话待办不随上下文清空）与
// lifetime 对账值。
func (r *ViewReducer) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearLocked()
}

// Snapshot 返回当前稳定投影的深拷贝。返回前 flush 未决 chunk，并把进行中的
// 子 agent 压缩项归一为 aborted —— checkpoint 不含瞬时 active 态。
// LiveRootRunID 返回「当前仍在运行」的顶层 run id（无则空）。
// 判据与前端 liveRootRunId（store/trace.ts）同源：runNodes 里 kind=run 且无 parent 且 running。
// 用途：trace 投影区分「正在跑」（无 agent_end 属正常）与「中断残留」（该标 interrupted）。
func (r *ViewReducer) LiveRootRunID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.runNodes {
		if n.Kind == runNodeKindRun && n.ParentID == "" && n.Status == statusRunning {
			return n.ID
		}
	}
	return ""
}

// CtxTokens 当前主 agent 上下文占用（最近一次 llm_end 的 Input 结算 / compress_end 修正）。
// 单值读取走轻量访问器：ctx 占用面板每次 hover 都要拿它做校准，不值当走 Snapshot 全量克隆。
func (r *ViewReducer) CtxTokens() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ctxTokens
}

// CtxAnchor 当前上下文锚点 = (占用, 是否估算口径)。estimated=true 表示这个值来自
// compress_end 的字符/token 估算（压缩后尚无新一轮真实 usage），展示层据此标注「≈」，
// 面板也据此决定分区口径说明 —— 真实 usage 与估算混在同一个字段会让口径无从分辨。
func (r *ViewReducer) CtxAnchor() (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ctxTokens, r.ctxTokensEst
}

func (r *ViewReducer) Snapshot() *Checkpoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

// snapshotLocked 是 Snapshot 的持锁实现（Marshal 已持锁时调用它，避免重入死锁）。
func (r *ViewReducer) snapshotLocked() *Checkpoint {
	r.flushChunkLocked()

	out := &Checkpoint{
		SchemaVersion:      checkpointSchemaVersion,
		Blocks:             cloneBlocks(r.blocks),
		RunNodes:           cloneRunNodes(r.runNodes),
		Turns:              cloneTurns(r.turns),
		Usage:              r.usage,
		CostUSD:            r.costUSD,
		LifetimeUsage:      r.lifetime,
		LifetimeCostUSD:    r.lifetimeCost,
		Todos:              cloneTodos(r.todos),
		CtxTokens:          r.ctxTokens,
		CtxTokensEstimated: r.ctxTokensEst,
		ContextWindow:      r.contextWindow,
		Diffs:              cloneDiffs(r.diffs),
		Degraded:           r.degraded,
	}
	if r.lastRun != nil {
		lr := *r.lastRun
		out.LastRun = &lr
	}
	if r.lastError != nil {
		le := *r.lastError
		out.LastError = &le
	}
	out.Diagnostics = r.diagSliceLocked()
	normalizeSnapshotBlocks(out.Blocks)
	normalizeDanglingState(out)
	return out
}

// normalizeDanglingState 把 checkpoint 中的「悬空态」收敛为终态（幂等，只影响投影）。
//
// 成对事件缺失处理矩阵（对应测试 TestReducerDangling*）：
//
//	llm_start 无 llm_end      → 完全空壳 assistant 块（无正文/思考/未结算）→ 从投影移除
//	                            （不展示任何东西，也不标「已中断」——热更新/恢复时进行中的
//	                            轮次不应误导成用户中断；有思考内容的块保留，思考有价值）
//	tool_start 无 tool_end    → tool 块 running → done（promoted → task_status=abandoned）
//	agent_start 无 agent_end  → 实时提交保留 running（合法中间态）；重启 NormalizeForRestore 收敛
//	task_started 无 task_end  → 同上：实时保留 running，重启收敛 abandoned
//	compress_start 无 end     → 主：不产生块；子：卡片项 active → aborted
//	end 无 start（异常序）     → llm_end 兜底补全块 / tool_end 兜底建块 / task_result 独立块
//
// 注意 1：仅作用于 Snapshot 输出（深拷贝），不改内存 reducer——实时流中迟到的
// end 事件仍可正常更新内存（toolBlockID 仍指向原块）。
// 注意 2：不收敛 agent/async_task 卡片的 running——agent_start 后到 agent_end 之间
// 可能有中间提交（tool_end/llm_end 边界），运行中的任务是合法状态；悬空收敛只
// 发生在 NormalizeForRestore（重启恢复场景）。
func normalizeDanglingState(cp *Checkpoint) {
	kept := cp.Blocks[:0]
	for i := range cp.Blocks {
		b := &cp.Blocks[i]
		switch b.Kind {
		case blockKindAssistant:
			// llm_start 无 llm_end：块为空（无正文/思考）且未结算。Model 在 llm_start
			// 就带、不代表有输出；已结算（Settled，纯工具调用轮）的空块保留。
			// 完全空壳（无正文无思考）直接移除——恢复/热更新时进行中的轮次不展示
			// 任何东西（不标「（已中断）」），客户端发送不受影响。
			if b.Text == "" && b.Thinking == "" && !b.Settled {
				continue // 丢弃空壳块
			}
		case blockKindTool:
			if b.Status == statusRunning {
				b.Status = statusDone
				// 有 task 归属（promoted 后台任务）→ 重启语义 abandoned
				if b.TaskID != "" {
					b.TaskStatus = statusAbandoned
				}
			}
		}
		kept = append(kept, *b)
	}
	cp.Blocks = kept
}

// Marshal 返回 checkpoint 的规范 JSON（可直接作为 session.ViewCheckpoint）。
// Marshal 序列化当前 checkpoint，带 (seq, clearGeneration) 缓存。
//
// 为什么缓存安全：Marshal 的输入完全由（seq, clearGeneration）决定 ——
// 每个 Apply 都递增 seq，任何改变视图的路径都经过 Apply；
// Restore / NormalizeForRestore 绕过 Apply，它们显式失效缓存。
//
// 返回的字节**只读**：多处以 json.RawMessage 直接塞进响应。
// 不做防御性拷贝 —— 拷贝 84MB 会抵消全部收益，契约写在文档里。
func (r *ViewReducer) Marshal() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.marshalLocked()
}

// marshalLocked 是 Marshal 的持锁实现（供已持锁的调用方复用）。
func (r *ViewReducer) marshalLocked() ([]byte, error) {
	if r.marshalCacheOK && r.marshalCacheSeq == r.seq && r.marshalCacheGen == r.clearGeneration {
		return r.marshalCache, nil
	}
	out := new(bytes.Buffer)
	if r.marshalCacheOK {
		out.Grow(len(r.marshalCache) + 1024)
	} else {
		out.Grow(64 * 1024)
	}
	if err := r.encodeSnapshotLocked(out); err != nil {
		return nil, err
	}
	// Encoder.Encode 追加换行；历史行为是 json.Marshal（无换行）→ 去掉，
	// 保证与改造前逐字节一致。
	b := out.Bytes()
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	r.marshalCache = b
	r.marshalCacheSeq = r.seq
	r.marshalCacheGen = r.clearGeneration
	r.marshalCacheOK = true
	return b, nil
}

// encodeSnapshotLocked 把当前快照写进 out，blocks 走 per-block 字节缓存。
//
// 为什么不能直接 json.Encode(整个 Checkpoint)：那会对每个 block 重新做
// 反射序列化 + 对 json.RawMessage 做转义扫描，86MB 实测 283ms，而每个
// checkpoint 边界通常只有 1~2 个 block 变化。
//
// 这里把 Checkpoint 拆成「blocks 之外的部分」+「逐 block 字节」：
//
//	· 前者很小（turns/diffs/标量，约 4MB），每次照常序列化；
//	· 后者按 block 内容指纹命中缓存，未变化的直接复用上次的字节。
//
// 输出必须与 json.Marshal 的**键序完全一致**：所以不能简单地
// "序列化去掉 blocks 的结构再拼接"，而是用一个 RawMessage 占位符合成
// 完整结构，再把占位符替换成拼好的 blocks 数组 —— 与 marshalRecordEnvelope
// 同一手法（那边已证明可靠）。
func (r *ViewReducer) encodeSnapshotLocked(out *bytes.Buffer) error {
	_, _, err := r.encodeLocked(out)
	return err
}

// marshalLockedSpans 与 marshalLocked 相同，但额外返回每个 block 的字节切片。
func (r *ViewReducer) marshalLockedSpans() ([]byte, [][]byte, error) {
	if r.marshalCacheOK && r.marshalCacheSeq == r.seq && r.marshalCacheGen == r.clearGeneration {
		return r.marshalCache, r.cachedSpans, nil
	}
	var out bytes.Buffer
	out.Grow(len(r.marshalCache) + 1024)
	view, spans, err := r.encodeLocked(&out)
	if err != nil {
		return nil, nil, err
	}
	r.marshalCache = view
	r.cachedSpans = spans
	r.marshalCacheSeq = r.seq
	r.marshalCacheGen = r.clearGeneration
	r.marshalCacheOK = true
	return view, spans, nil
}

// encodeLocked 是真正的编码实现，返回 view 字节与 block 切片。
func (r *ViewReducer) encodeLocked(out *bytes.Buffer) ([]byte, [][]byte, error) {
	cp := r.snapshotLocked()
	blocks := cp.Blocks
	cp.Blocks = nil

	// 1. 序列化不含 blocks 的骨架，得到 `...,"blocks":null,...`
	head, err := json.Marshal(cp)
	if err != nil {
		return nil, nil, err
	}
	const placeholder = `"blocks":null`
	idx := bytes.Index(head, []byte(placeholder))
	if idx < 0 {
		// 结构异常 → 回退整份序列化（正确性优先）
		enc := json.NewEncoder(out)
		cp.Blocks = blocks
		if err := enc.Encode(cp); err != nil {
			return nil, nil, err
		}
		return out.Bytes(), nil, nil
	}

	// 2. 逐 block 取字节：只有「被写入方标脏」的块才重新序列化。
	//
	// 这是本优化正确性的来源：脏标记由写入方（applyLocked 的 r.blocks[idx]=b
	// 与新建块）**精确给出**，而不是在 Marshal 时猜「内容变没变」——
	// 猜的两种做法（手写字段清单、终态块复用）都已被回归测试证伪。
	dirty := r.takeDirtyBlocks()
	if r.blockBytes == nil {
		r.blockBytes = make(map[uint64][]byte, len(blocks))
	}
	// 缓存远大于当前块数（长会话经历大量块更替）→ 重建，防无界增长。
	if len(r.blockBytes) > 2*len(blocks)+256 {
		r.blockBytes = make(map[uint64][]byte, len(blocks))
	}

	parts := make([][]byte, len(blocks))
	total := 0
	for i := range blocks {
		id := blocks[i].ID
		_, isDirty := dirty[id]
		if !isDirty {
			if b, ok := r.blockBytes[id]; ok {
				parts[i] = b
				total += len(b) + 1
				continue
			}
		}
		b, err := json.Marshal(&blocks[i])
		if err != nil {
			return nil, nil, err
		}
		parts[i] = b
		r.blockBytes[id] = b
		total += len(b) + 1
	}

	buf := make([]byte, 0, total+2)
	buf = append(buf, '[')
	for i, p := range parts {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, p...)
	}
	buf = append(buf, ']')

	// 3. 拼回骨架，保持键序与 json.Marshal 逐字节一致
	out.Write(head[:idx])
	out.WriteString(`"blocks":`)
	out.Write(buf)
	out.Write(head[idx+len(placeholder):])
	view := out.Bytes()
	// spans 指向 view 内部：需要在 view 中定位每个 part 的位置。
	// parts 是各自独立的字节，拼接后散落在 view 中 —— 重新定位会退化成扫描。
	// 因此改为「把 parts 直接返回」，由调用方按顺序使用（它们的内容与 view 中的
	// 对应片段逐字节相同，可直接写入 sidecar）。
	return view, parts, nil
}

// blockFingerprint 计算 block 的内容指纹（用于字节缓存）。
//
// 用 FNV-1a over 序列化后的字节？不行 —— 那要先序列化才有指纹，本末倒置。
// 这里直接对 block 的**结构字段**做稳定哈希：json.Marshal 是确定性的，
// 等价于「同一内容 ⇒ 同一指纹」。成本是反射扫描一次（不产出字节），
// 远低于序列化本身。
//
// 注意：指纹必须覆盖所有会影响输出的字段，否则会返回陈旧字节。
// 用 json.Marshal 到 hasher 是最稳的做法（不分配输出 buffer）。
// 指纹的精度取舍：
//
// 全字段反射哈希要 151ms（3033 块 × 每块每字段递归，实测），与优化目标矛盾。
// 但**终态 block 是不可变的**（reducer 在 running 期才追加 items，agent_end 后
// 不再写），所以只需一个「能区分内容是否变过」的廉价指纹：
//
//   · ID / Kind / Status / Ts / EndedAt 等标量 → 必看；
//   · Items 的**长度与首尾元素**→ 足以发现追加/替换（reducer 只追加，不改中间项）；
//   · 其余大字段（Text/Result/Summary）→ 看长度，加首尾 8 字节。
//
// 这个指纹**只用于字节缓存命中判定**，不参与持久化校验：命中错了会返回旧字节，
// 所以宁可保守 —— 任何长度变化或首尾字节变化都会使指纹改变。
// 关键安全性：block 一旦进入终态就不再被写，因此「内容变了但指纹没变」需要
// 长度与首尾同时不变，而 reducer 没有任何「原地改中间内容」的路径。

// markBlockDirty 记录「该 id 的块内容已变」。
//
// 所有对 r.blocks 的写入都必须经过它（或 appendNewBlock）。这样 Marshal 时
// 只需重新序列化脏块 —— 这是「既正确又跳过序列化」的唯一可靠做法：
// 脏标记由**写入方**给出，而不是在 Marshal 时猜内容变没变。
func (r *ViewReducer) markBlockDirty(id uint64) {
	if r.dirtyBlocks == nil {
		r.dirtyBlocks = make(map[uint64]struct{}, 8)
	}
	r.dirtyBlocks[id] = struct{}{}
}

// takeDirtyBlocks 取走脏集合（Marshal 消费后清空）。
func (r *ViewReducer) takeDirtyBlocks() map[uint64]struct{} {
	d := r.dirtyBlocks
	r.dirtyBlocks = nil
	return d
}

// BlockSpans 是「已切分的 block 字节」清单，供存储层做 sidecar 增量落盘。
//
// 为什么由 reducer 给出：存储层要按 block 增量就得把 86MB 的 blocks 数组切成
// 每块的字节；让它自己解析要一趟扫描 86MB（实测 106ms）。而 reducer 在
// encodeLocked 里**本来就有**每个 block 的字节，顺手记录零成本。
//
// View 是完整 view 字节；Spans[i] 与 View 中对应片段逐字节相同（独立 backing）。
type BlockSpans struct {
	View  []byte
	Spans [][]byte
}

// MarshalWithSpans 返回 view 字节 + 每个 block 的字节切片。
func (r *ViewReducer) MarshalWithSpans() (*BlockSpans, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	view, parts, err := r.marshalLockedSpans()
	if err != nil {
		return nil, err
	}
	return &BlockSpans{View: view, Spans: parts}, nil
}

// invalidateMarshalCache 让下一次 Marshal 重新序列化。
// 由任何「绕过 Apply 直接改内部状态」的入口调用。
func (r *ViewReducer) invalidateMarshalCache() {
	r.marshalCacheOK = false
	r.marshalCache = nil
}

// backfillOutputTokens 用 turns 回填 assistant 块的 OutputTokens/ReasoningTokens。
//
// 背景：这两个字段是 2026-09-10 才加进 Block 的（此前只有 turns 里有 output/reasoning）。
// 旧版本落盘的 checkpoint 里块上没有它们 → 渲染层 `out == null` 不渲染 →
// 历史轮次的 Tokens/s 整行缺失（刷新/重启后尤其明显，用户 2026-09-10 报障）。
//
// 回填依据：Turns 一直记录每轮 output/reasoning，且与块共享 RequestId —— 二者一一对应。
// 幂等：仅填 0 值，不覆盖已有数据；匹配不到就跳过（宁缺勿错）。
func backfillOutputTokens(blocks []Block, turns []Turn) {
	if len(blocks) == 0 || len(turns) == 0 {
		return
	}
	byReq := make(map[string]*Turn, len(turns))
	for i := range turns {
		if turns[i].RequestID != "" {
			byReq[turns[i].RequestID] = &turns[i]
		}
	}
	for i := range blocks {
		b := &blocks[i]
		if b.Kind != blockKindAssistant || b.OutputTokens > 0 {
			continue
		}
		t := byReq[b.RequestID]
		if t == nil {
			continue
		}
		b.OutputTokens = t.Output
		b.ReasoningTokens = t.Reasoning
	}
}

// Restore 用已校验 checkpoint 重建 reducer 状态（不扫描 raw events）。
// 内存去重集合与计时瞬态全部清空：恢复后到达的事件按新事件处理。
func (r *ViewReducer) Restore(cp *Checkpoint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cp == nil {
		return nil
	}
	r.blocks = cloneBlocks(cp.Blocks)
	r.blockIdxDirty = true // 索引随新 blocks 惰性重建
	r.runNodes = cloneRunNodes(cp.RunNodes)
	r.turns = cloneTurns(cp.Turns)
	backfillOutputTokens(r.blocks, r.turns)
	r.usage = cp.Usage
	r.lifetime = cp.LifetimeUsage
	r.costUSD = cp.CostUSD
	r.lifetimeCost = cp.LifetimeCostUSD
	r.todos = cloneTodos(cp.Todos)
	r.ctxTokens = cp.CtxTokens
	r.ctxTokensEst = cp.CtxTokensEstimated
	r.contextWindow = cp.ContextWindow
	r.diffs = cloneDiffs(cp.Diffs)
	r.degraded = cp.Degraded
	if cp.LastRun != nil {
		lr := *cp.LastRun
		r.lastRun = &lr
	} else {
		r.lastRun = nil
	}
	if cp.LastError != nil {
		le := *cp.LastError
		r.lastError = &le
	} else {
		r.lastError = nil
	}
	r.diag = map[string]*diagEntry{}
	for _, d := range cp.Diagnostics {
		r.diag[d.EventType] = &diagEntry{count: d.Count, firstSeq: d.FirstSeq, lastSeq: d.LastSeq}
	}
	// Restore 直接替换内部状态（不经过 Apply）→ 必须显式失效序列化缓存。
	r.invalidateMarshalCache()

	// 重建块 id 计数器（不依赖事件流）
	var maxID uint64
	for _, b := range r.blocks {
		if b.ID > maxID {
			maxID = b.ID
		}
	}
	r.blockSeq = maxID

	// 去重集合与瞬态全部复位
	r.llmEndSeen = map[string]bool{}
	r.llmErrorSeen = map[string]bool{}
	r.toolStartSeen = map[string]bool{}
	r.toolEndSeen = map[string]bool{}
	r.approvalSeen = map[string]bool{}
	r.taskStartedSeen = map[string]bool{}
	r.taskEndSeen = map[string]bool{}
	r.taskResultSeen = map[string]bool{}
	r.agentStartSeen = map[string]bool{}
	r.agentEndSeen = map[string]bool{}
	r.compressEndSeen = map[string]bool{}
	r.commandResultSeen = map[string]bool{}
	r.userInputsSeen = map[string]bool{}
	r.reqState = map[string]*reqState{}
	r.agentBlockID = map[string]uint64{}
	r.toolBlockID = map[string]uint64{}
	r.thinkStart = map[string]time.Time{}
	r.turnThinkMs = map[string]int64{}
	r.turnStart = map[string]time.Time{}
	r.toolStartTs = map[string]time.Time{}
	r.compStart = map[string]time.Time{}
	r.agentCompressing = map[string]bool{}
	r.mainCompressing = false
	r.chunk = nil
	return nil
}

// NormalizeForRestore 把 checkpoint 中残留的 running 态收敛为重启后的终态
// （对齐 renderer finalizeReplayView 语义）：主/子 run 节点 running→done；
// 子 agent 卡片与 async_task 占位 running/interrupting→abandoned；
// 工具块 running→done（有 task 归属时 taskStatus=abandoned）。
// 由恢复入口在 hydrate 后调用（reducer 正常运行期间不调用）。
func (r *ViewReducer) NormalizeForRestore() {
	r.mu.Lock()
	defer r.mu.Unlock()
	// 该方法直接改写 blocks/runNodes（收敛悬空态），绕过 Apply → 失效缓存。
	r.invalidateMarshalCache()
	hadRunningMain := false // 顶层 run（runNodes 根节点）仍在跑 → 进程关闭/重启被杀 → 插 shutdown divider
	blocks := make([]Block, 0, len(r.blocks))
	var wasRunning []bool // 与 blocks 同序：恢复**前**是否处于运行态
	for _, b := range r.blocks {
		wasRunning = append(wasRunning, b.Status == statusRunning || b.Status == "interrupting")
		switch b.Kind {
		case blockKindTool:
			if b.Status == statusRunning {
				b.Status = statusDone
				if b.TaskID != "" {
					b.TaskStatus = statusAbandoned
				}
			} else if b.Promoted && b.TaskID != "" && (b.TaskStatus == statusRunning || b.TaskStatus == "interrupting") {
				// 已结算块（status=done）但后台任务态仍 running/interrupting：后台任务
				// 随进程关闭/重启而亡（无从再交付终态）→ abandoned。此前漏收敛该形态，
				// 僵尸「执行中」卡经 checkpoint 代代固化（2026-08-30 实证 8 条）。
				b.TaskStatus = statusAbandoned
			}
		case blockKindAgent, blockKindAsyncTask:
			if b.Status == statusRunning || b.Status == "interrupting" {
				b.Status = statusAbandoned
				if b.EndedAt == 0 {
					b.EndedAt = time.Now().UnixMilli()
				}
			}
		}
		// 恢复时对**终态** agent 卡片补做一次裁剪。
		//
		// applyAgentEndLocked 只在 agent_end 当刻裁剪；早于该特性上线的历史
		// record（以及恢复时状态为 abandoned 的卡片）不会触发，于是 85% 体积
		// 的老 items 会一直留在每次 checkpoint 里被反复重写。恢复是唯一能
		// 一次性收敛它们的地方：此处裁剪不丢信息 —— 完整历史在子任务 journal
		// 里，UI 通过 agent_history 分页取回。
		//
		// 只裁终态（done/failed/aborted/abandoned）：running 卡片的 items 之后
		// 还会按 tool_id 追加，裁掉会让后续 hasAgentToolItem 查不到而重复插入。
		// 判据必须是「恢复前」的状态：上面刚把 running 一律改成 abandoned，
		// 若用改后状态判断，运行中的卡片也会被裁 —— 而它们复跑时还要按
		// tool_id 追加 items。用 wasRunning[i] 而非 b.Status。
		if (b.Kind == blockKindAgent || b.Kind == blockKindAsyncTask) && !wasRunning[len(blocks)] {
			if n := len(b.Items); n > agentItemsLimit {
				b.Items = append([]AgentItem(nil), b.Items[n-agentItemsLimit:]...)
				b.ItemsTrimmed += n - agentItemsLimit
			}
		}
		blocks = append(blocks, b)
	}
	// 顶层 run（ParentID==""）running → 主 run 被杀（进程关闭/重启）：
	// 对话末尾插一条 shutdown divider（横线）作上下文隔离；UI 与 abort 同款横线，
	// 内部 reason 区分（shutdown=进程关闭/重启，abort=用户主动中断）。
	for _, n := range r.runNodes {
		if n.ParentID == "" && n.Status == statusRunning {
			hadRunningMain = true
			break
		}
	}
	if hadRunningMain {
		blocks = append(blocks, Block{Kind: blockKindDivider, ID: r.nextBlockID(), DividerReason: "shutdown", Ts: time.Now().UnixMilli()})
	}
	r.blocks = blocks
	r.blockIdxDirty = true
	runNodes := make([]RunNode, 0, len(r.runNodes))
	for _, n := range r.runNodes {
		if n.Status == statusRunning {
			n.Status = statusDone
		}
		runNodes = append(runNodes, n)
	}
	r.runNodes = runNodes
	// 重置瞬态（恢复后无在途压缩/计时）
	r.mainCompressing = false
	r.agentCompressing = map[string]bool{}
	r.chunk = nil
	r.reqState = map[string]*reqState{}
}

// ---------------------------------------------------------------------------
// 内部：事件应用
// ---------------------------------------------------------------------------

func (r *ViewReducer) applyLocked(ev events.Event, meta EventMeta) {
	switch e := ev.(type) {
	case *events.ContentChunk:
		r.applyChunkLocked("content", e.RunId, e.RequestId, e.Content, e.Timestamp)
	case *events.ReasoningChunk:
		r.applyChunkLocked("reasoning", e.RunId, e.RequestId, e.Content, e.Timestamp)
	case *events.LLMStart:
		r.applyLLMStartLocked(e)
	case *events.LLMEnd:
		r.applyLLMEndLocked(e, meta)
	case *events.LLMError:
		r.applyLLMErrorLocked(e, meta)
	case *events.ToolApprovalRequested:
		r.flushChunkLocked()
		key := "approval:" + e.Id
		if r.approvalSeen[key] {
			return
		}
		r.approvalSeen[key] = true
	case *events.ToolStart:
		r.applyToolStartLocked(e)
	case *events.ToolResponse:
		r.applyToolResponseLocked(e, meta)
	case *events.TaskPromoted:
		r.flushChunkLocked()
		r.applyTaskPromotedLocked(e)
	case *events.AgentStart:
		r.applyAgentStartLocked(e)
	case *events.AgentEnd:
		r.applyAgentEndLocked(e)
	case *events.AgentUserMessage:
		r.applyAgentUserMessageLocked(e)
	case *events.GoalAlignmentReminder:
		r.flushChunkLocked()
		text := "已注入目标一致性提醒"
		if e.Round > 0 {
			text = "已注入目标一致性提醒（第 " + itoa(e.Round) + " 轮）"
		}
		r.blocks = append(r.blocks, Block{Kind: blockKindSystem, ID: r.nextBlockID(), Text: text, Tone: "info", Ts: nowMillis()})
	case *events.TaskStarted:
		r.applyTaskStartedLocked(e)
	case *events.TaskMessage:
		r.flushChunkLocked() // 已知但非视图事件：仅顺序边界
	case *events.RefsLoaded:
		r.flushChunkLocked() // 已知但非视图事件（@引用展开瞬时提示）：不进入 checkpoint 稳定视图
	case *events.TaskEnd:
		r.applyTaskEndLocked(e)
	case *events.TaskResultDelivered:
		r.applyTaskResultDeliveredLocked(e)
	case *events.CompressStart:
		r.applyCompressStartLocked(e)
	case *events.CompressEnd:
		r.applyCompressEndLocked(e, meta)
	case *events.CommandResult:
		r.applyCommandResultLocked(e)
	case *events.UserInputsConsumed:
		r.applyUserInputsConsumedLocked(e)
	case *events.SessionRunError:
		r.applySessionRunErrorLocked(e)
	case *events.ToolHookWarning:
		r.flushChunkLocked() // 已知事件：警告仅审计，不进稳定视图
	case *events.SessionPersistError, *events.SessionBudgetExceeded, *events.SessionClosed:
		r.flushChunkLocked() // 已知事件：持久化/预算/关闭为宿主级提示，不进入 checkpoint
	case *events.RunSilentEnd:
		// 第三批 C1/A1b（判据于 C9 放宽为「收尾轮无文本」）：静默收尾是**可观测性**事件
		// ——第 2 类「静默失败」的可见化，与 ToolHookWarning 同类：仅审计/遥测消费
		//（baseline.py 按事件名包含 silent 计数，SawAssistantText 拆两形态），
		// 不进入稳定视图；不登记为未知事件，避免 noteDiagLocked 产生诊断噪音。
		r.flushChunkLocked()
	default:
		// 未知事件：不改变稳定视图，保留为受约束的 diagnostic 记录。
		r.flushChunkLocked()
		r.noteDiagLocked(string(ev.Type()), 1)
	}
}

// applyChunkLocked 只允许相邻同 key chunk 合并；key 不同或非 chunk 已由调用方 flush。
func (r *ViewReducer) applyChunkLocked(kind, runID, requestID, content string, ts time.Time) {
	if r.chunk != nil && r.chunk.kind == kind && r.chunk.runID == runID && r.chunk.requestID == requestID {
		r.chunk.content += content
		return
	}
	r.flushChunkLocked()
	r.chunk = &chunkAgg{kind: kind, runID: runID, requestID: requestID, content: content, firstTs: ts}
}

// flushChunkLocked 把未决 chunk 合并进目标 assistant 块 / 子 agent 卡片项。
func (r *ViewReducer) flushChunkLocked() {
	c := r.chunk
	if c == nil {
		return
	}
	r.chunk = nil
	if c.content == "" {
		return
	}
	ts := c.firstTs
	if ts.IsZero() {
		ts = time.Now()
	}
	runKey := c.runID

	// 首个任意 chunk → TTFT；首个 reasoning → 思考起点；首个 content → 思考结算。
	switch c.kind {
	case "reasoning":
		if _, ok := r.thinkStart[runKey]; !ok {
			r.thinkStart[runKey] = ts
		}
	case "content":
		if started, ok := r.thinkStart[runKey]; ok {
			r.turnThinkMs[runKey] = msBetween(started, ts)
			delete(r.thinkStart, runKey)
		}
	}

	if r.isSubRunLocked(c.runID) {
		r.appendAgentStreamLocked(c.runID, c.kind, c.content, ts)
		return
	}
	// 主 agent：路由到 request 对应的 assistant 块；缺失回退最后一条 assistant。
	bid := r.blockIDForRequestLocked(c.requestID, c.runID)
	if bid == 0 {
		bid = r.lastAssistantBlockLocked()
	}
	idx := r.findBlockLocked(bid)
	if idx >= 0 && r.blocks[idx].Kind == blockKindAssistant {
		b := r.blocks[idx]
		if c.kind == "reasoning" {
			b.Thinking += c.content
		} else {
			b.Text += c.content
		}
		r.blocks[idx] = b
		r.markBlockDirty(b.ID)
	} else {
		// 无块可归（纯流式早于 llm_start 的异常序）：兜底新建。
		bid = r.nextBlockID()
		nb := Block{Kind: blockKindAssistant, ID: bid, Text: "", Thinking: "", Model: "", RunID: c.runID, RequestID: c.requestID, Ts: millis(ts)}
		if c.kind == "reasoning" {
			nb.Thinking = c.content
		} else {
			nb.Text = c.content
		}
		r.blocks = append(r.blocks, nb)
		r.noteBlockAppendLocked(bid)
		if c.requestID != "" {
			if rs, ok := r.reqState[c.requestID]; ok {
				rs.blockID = bid
			}
		}
	}
	// TTFT 结算挂在请求态（首个任意 chunk 到达时刻）。
	if c.requestID != "" {
		if rs, ok := r.reqState[c.requestID]; ok && rs.ttftMs == 0 && !rs.start.IsZero() {
			rs.ttftMs = msBetween(rs.start, ts)
			if idx := r.findBlockLocked(rs.blockID); idx >= 0 && r.blocks[idx].Kind == blockKindAssistant {
				b := r.blocks[idx]
				b.TtftMs = rs.ttftMs
				r.blocks[idx] = b
				r.markBlockDirty(b.ID)
			}
		}
	} else if bid != 0 {
		// 无 request 的旧流：TTFT 挂到最后一条 assistant。
		if rs, ok := r.reqState[""]; ok && rs.ttftMs == 0 && !rs.start.IsZero() && rs.blockID == bid {
			rs.ttftMs = msBetween(rs.start, ts)
		}
	}
}

// blockIDForRequestLocked 返回 request 对应的 assistant 块（无则 0）。
func (r *ViewReducer) blockIDForRequestLocked(requestID, runID string) uint64 {
	if requestID != "" {
		if rs, ok := r.reqState[requestID]; ok {
			return rs.blockID
		}
	}
	return 0
}

func (r *ViewReducer) lastAssistantBlockLocked() uint64 {
	for i := len(r.blocks) - 1; i >= 0; i-- {
		if r.blocks[i].Kind == blockKindAssistant {
			return r.blocks[i].ID
		}
	}
	return 0
}

// lastAssistantBlockForRunLocked 返回指定 run 的最后一条「有正文」assistant 块（无则 0）。
// 与 lastAssistantBlockLocked 的区别：
//  1. 按 RunID 精确匹配——多轮对话中后续 run 已追加新块时，全局最后一条会指向新 run 的块，
//     导致旧 run 的结算（如 AgentDurMs）落到错误块上。
//  2. 跳过空块——每轮 llm_start 都会新建块（工具轮无正文），字面最后一条可能是空块；
//     AgentDurMs 是整轮运行时长，应挂在用户可见的最终正文块上（与前端 findLastIndex
//     的 `Boolean(b.text)` 条件一致）。
func (r *ViewReducer) lastAssistantBlockForRunLocked(runID string) uint64 {
	for i := len(r.blocks) - 1; i >= 0; i-- {
		b := r.blocks[i]
		if b.Kind == blockKindAssistant && b.RunID == runID && b.Text != "" {
			return b.ID
		}
	}
	// 无正文块（纯工具轮/异常）：回退该 run 最后一条块（保证 AgentDurMs 不丢）
	for i := len(r.blocks) - 1; i >= 0; i-- {
		b := r.blocks[i]
		if b.Kind == blockKindAssistant && b.RunID == runID {
			return b.ID
		}
	}
	return 0
}

func (r *ViewReducer) rebuildBlockIdxLocked() {
	m := make(map[uint64]int, len(r.blocks))
	for i, b := range r.blocks {
		m[b.ID] = i
	}
	r.blockIdx = m
	r.blockIdxDirty = false
}

// findBlockLocked 定位块下标：索引优先（O(1)），dirty 时惰性重建，
// miss 时线性兜底（防御任何遗漏的索引维护点——正确性优先）。
func (r *ViewReducer) findBlockLocked(id uint64) int {
	if r.blockIdxDirty || r.blockIdx == nil {
		r.rebuildBlockIdxLocked()
	}
	if i, ok := r.blockIdx[id]; ok {
		return i
	}
	for i := range r.blocks {
		if r.blocks[i].ID == id {
			r.blockIdx[id] = i
			return i
		}
	}
	return -1
}

// noteBlockAppendLocked 记录新 append 块的下标（dirty 时跳过——重建包含全部）。
func (r *ViewReducer) noteBlockAppendLocked(id uint64) {
	if r.blockIdxDirty || r.blockIdx == nil {
		return
	}
	r.blockIdx[id] = len(r.blocks) - 1
}

// appendAgentStreamLocked 把流式文本/思考追加到子 agent 卡片活动记录
// （同类型连续项合并，跨轮/跨工具各自成项）。
func (r *ViewReducer) appendAgentStreamLocked(runID, kind, content string, ts time.Time) {
	// chunk kind（reasoning/content）→ 卡片项 kind（thinking/text）
	switch kind {
	case "reasoning":
		kind = agentItemThinking
	case "content":
		kind = agentItemText
	default:
		return
	}
	bid := r.agentBlockID[runID]
	if bid == 0 {
		return
	}
	idx := r.findBlockLocked(bid)
	if idx < 0 || r.blocks[idx].Kind != blockKindAgent {
		return
	}
	b := r.blocks[idx]
	items := b.Items
	last := len(items) - 1
	if last >= 0 && items[last].Kind == kind && items[last].Text != "" && !items[last].IsError {
		items[last].Text += content
	} else {
		item := AgentItem{Kind: kind, Text: content}
		if kind == "thinking" {
			item.ThinkStart = millis(ts)
		}
		items = append(items, item)
	}
	b.Items = items
	r.blocks[idx] = b
	r.markBlockDirty(b.ID)
}

// applyLLMStartLocked 打开一次 LLM 请求的结算态。
func (r *ViewReducer) applyLLMStartLocked(e *events.LLMStart) {
	r.flushChunkLocked()
	if e.RequestId != "" {
		if _, ok := r.reqState[e.RequestId]; ok {
			return // 重送/重试期重复 start：不新建块
		}
	}
	var bid uint64
	if r.isSubRunLocked(e.RunId) {
		bid = r.agentBlockID[e.RunId] // 子 agent：结算态锚到卡片块
	} else {
		bid = r.nextBlockID()
		ts := e.Timestamp
		if ts.IsZero() {
			ts = time.Now()
		}
		r.blocks = append(r.blocks, Block{
			Kind: blockKindAssistant, ID: bid, Text: "", Thinking: "",
			Model: e.Model, RunID: e.RunId, RequestID: e.RequestId, Ts: millis(ts),
		})
		r.noteBlockAppendLocked(bid)
	}
	key := e.RequestId
	if key == "" {
		key = "$" + e.RunId // 无 request 的旧流：按 run 共享一个结算态
	}
	ts := e.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	r.reqState[key] = &reqState{runID: e.RunId, model: e.Model, start: ts, blockID: bid}
	// 当前 LLM 轮起点（runID 级）：末轮若没有 llm_end（中断/失败/截断），AgentEnd 用它
	// 兜底出「这一轮已经跑了多久」——否则该块永远拿不到 DurMs，刷新后指标行缺一段。
	r.turnStart[e.RunId] = ts
}

// applyLLMEndLocked 结算单次 LLM 调用：turn 指标、usage/cost、assistant 块终态。
func (r *ViewReducer) applyLLMEndLocked(e *events.LLMEnd, meta EventMeta) {
	r.flushChunkLocked()
	key := llmEndKey(e, r.seq)
	if r.llmEndSeen[key] {
		return // 重复投递幂等
	}
	r.llmEndSeen[key] = true

	now := e.Timestamp
	if now.IsZero() {
		now = time.Now()
	}
	runKey := e.RunId
	thinkMs := r.turnThinkMs[runKey]
	if started, ok := r.thinkStart[runKey]; ok {
		thinkMs = msBetween(started, now)
		delete(r.thinkStart, runKey)
	}
	delete(r.turnThinkMs, runKey)
	delete(r.turnStart, runKey) // 本轮已结算：AgentEnd 不再兜底（否则会重复计一次）

	// 请求态（durMs / TTFT）
	var rs *reqState
	if e.RequestId != "" {
		rs = r.reqState[e.RequestId]
	} else if s, ok := r.reqState["$"+e.RunId]; ok {
		rs = s
	}
	var durMs, ttftMs int64
	if rs != nil && !rs.start.IsZero() {
		durMs = msBetween(rs.start, now)
		ttftMs = rs.ttftMs
	}
	bid := uint64(0)
	if rs != nil {
		bid = rs.blockID
	}

	// usage / cost
	var u core.Usage
	if e.Usage != nil {
		u = *e.Usage
	}
	agg := usageFromCore(u)
	addUsage(&r.usage, agg)
	addUsage(&r.lifetime, agg)
	cost := meta.CostUSD
	if cost <= 0 {
		cost = costUsd(meta.Provider, e.Model, u)
	}
	r.costUSD += cost
	r.lifetimeCost += cost

	turn := Turn{
		Model: e.Model, Input: agg.Input, Output: agg.Output,
		CacheRead: agg.CacheRead, CacheWrite: agg.CacheWrite, Reasoning: agg.Reasoning,
		DurMs: durMs, ThinkMs: thinkMs, TtftMs: ttftMs, CostUSD: cost,
		RunID: e.RunId, RequestID: e.RequestId,
		FinishReason: string(e.FinishReason),
	}
	r.turns = append(r.turns, turn)

	// 主 agent：最终正文/思考（chunk 缺失时由 llm_end 补全，已有 chunk 不重复拼接）
	if !r.isSubRunLocked(e.RunId) {
		if bid == 0 {
			bid = r.lastAssistantBlockLocked()
		}
		if idx := r.findBlockLocked(bid); idx >= 0 && r.blocks[idx].Kind == blockKindAssistant {
			b := r.blocks[idx]
			b.Model = e.Model
			b.Settled = true // 已结算标记：纯工具调用轮（无正文）不得被归一化成已中断
			if b.Text == "" && e.Content != "" {
				b.Text = e.Content
			}
			if b.Thinking == "" && e.Reasoning != "" {
				b.Thinking = e.Reasoning
			}
			if thinkMs > 0 {
				b.ThinkMs = thinkMs
			}
			if ttftMs > 0 {
				b.TtftMs = ttftMs
			}
			if durMs > 0 {
				b.DurMs = durMs
			}
			// 输出速度（tokens/s）的分子：Output 含 Reasoning，与渲染层口径一致。
			// 分母不落库——由渲染层按 Thinking 是否流式决定（见 store llm_end 分支），
			// 规则是纯函数，重放时同样能算对。
			b.OutputTokens = u.Output
			b.ReasoningTokens = u.Reasoning
			b.ThinkingSummarized = u.ReasoningSummarized
			r.blocks[idx] = b
			r.markBlockDirty(b.ID)
		}
		// 主 agent 成功轮清除错误块（与 renderer 一致）
		if e.FinishReason != core.FinishReasonError {
			r.blocks = filterBlocks(r.blocks, func(b Block) bool { return b.Kind != blockKindError })
			r.blockIdxDirty = true
		}
		// 上下文占用 = 本轮实际输入（0 视为缺数保留旧值）；provider 真实报数 → 口径置回真实
		if u.Input > 0 {
			r.ctxTokens = u.Input
			r.ctxTokensEst = false
		}
	} else if bid != 0 {
		if idx := r.findBlockLocked(bid); idx >= 0 && r.blocks[idx].Kind == blockKindAgent {
			b := r.blocks[idx]
			b.Usage.Input += agg.Input
			b.Usage.Output += agg.Output
			b.Usage.CacheRead += agg.CacheRead
			b.Usage.CacheWrite += agg.CacheWrite
			b.Usage.Reasoning += agg.Reasoning
			b.Usage.CostUSD += cost // 卡片成本（运行中近似；TaskEnd 到达时按任务全量覆盖）
			if thinkMs > 0 && b.ThinkMs == 0 {
				b.ThinkMs = thinkMs
			}
			r.blocks[idx] = b
			r.markBlockDirty(b.ID)
		}
	}

	if e.RequestId != "" {
		delete(r.reqState, e.RequestId)
	}
}

// applyLLMErrorLocked 记录错误：主 agent 走 error 块，子 agent 挂卡片错误项。
func (r *ViewReducer) applyLLMErrorLocked(e *events.LLMError, meta EventMeta) {
	r.flushChunkLocked()
	key := "llm_error:" + e.RunId + "\x00" + itoa(e.Attempt)
	if r.llmErrorSeen[key] {
		return
	}
	r.llmErrorSeen[key] = true

	// 失败 attempt 的 reqState 清理：每次 attempt 的 llm_start 都按**自己的** request_id
	// 开一条 reqState（上游 chatcmpl id 每次 attempt 都不同），但只有成功那次有 llm_end
	// 去 delete。applyLLMEndLocked 删的是 llm_end 自带的那个 request_id，失败 attempt 的
	// 条目没人回收 —— 每次失败重试在 map 里留一条（会话存活期内累积），且条目带着过期的
	// blockID / start，迟到事件按 request_id 命中它会读到错误的结算锚。
	//
	// 为什么按 run 整扫而不是按 request_id 精确删：events.LLMError 不携带失败 attempt 的
	// request_id，Runtime 侧 roundResult 也没有，要从 provider 流里把它拽出来是侵入式改动
	// （要改 provider 事件 → translate → streamWithRetry 三处）。而 runID 是 reqState 自带的
	// 字段，且同一 run 内 LLM 调用严格串行（streamWithRetry 是阻塞调用，单 goroutine 循环），
	// llm_error 到达时该 run 名下唯一活着的条目就是这次失败 attempt 的 —— 整扫安全，
	// 也不会误删并发中的其它 run（父 run 正在计时的轮、兄弟子 agent 各占各的 key）。
	for k, rs := range r.reqState {
		if rs.runID == e.RunId {
			delete(r.reqState, k)
		}
	}

	at := nowMillis()
	r.lastError = &LastError{Message: e.Message, Kind: e.ErrorKind, At: at}

	text := llmRetryText(e)
	// 失败尝试的已产生成本（2026-09-23）：上游按已生成 token 计费，这笔钱此前整个丢弃。
	// 口径与 llm_end 一致（同一价表/同一 costUsd 兜底），只是记在「重试浪费」上：
	// 会话展示成本 + 子 agent 卡片 + 错误块（重试块显示 +$x）。
	cost := 0.0
	if e.Usage != nil && e.Usage.Cost.Total > 0 {
		cost = costUsd(meta.Provider, e.Model, *e.Usage)
	}
	if cost > 0 {
		r.costUSD += cost
		r.lifetimeCost += cost
	}
	if r.isSubRunLocked(e.RunId) {
		bid := r.agentBlockID[e.RunId]
		if idx := r.findBlockLocked(bid); idx >= 0 && r.blocks[idx].Kind == blockKindAgent {
			b := r.blocks[idx]
			b.Items = upsertAgentErrorItem(b.Items, text)
			b.Usage.CostUSD += cost // 子 agent 卡片成本含重试浪费（终态 TaskEnd 权威值会覆盖）
			if e.ErrorKind == "context_exceeded" || !e.WillRetry {
				b.Status = statusFailed
				b.TaskError = e.Message
			}
			r.blocks[idx] = b
			r.markBlockDirty(b.ID)
		}
		return
	}
	r.upsertErrorBlockLocked(text, e, cost)
}

// llmRetryText 组「重试进度」文案（2026-09-18 改）：
//
// 旧文案「正在重试 1…」把 Attempt 的语义读反了 —— Attempt 是**已经失败的那一次**，
// 不是「正在跑第几次」。下一次尝试正挂着（实测一次 187s 无首字）时，界面就一直停在
// 「正在重试 1」，用户看到的是「卡住 / 重试机制没了」。新文案把「第几次失败」与
// 「接下来重试第几次 / 共几次」都摆出来；实时数字（退避倒计时、本次尝试已进行多久）
// 由前端按 RetryDelayMs + 错误时刻计算，事件里没有的东西不写死进文案。
func llmRetryText(e *events.LLMError) string {
	// 上下文超限：终止性错误，重试语义不适用，保持专用文案。
	if e.ErrorKind == "context_exceeded" {
		return "（LLM 上下文超限终止）" + e.Message
	}
	failed := "第 " + itoa(e.Attempt) + " 次尝试失败"
	if e.MaxAttempts > 0 {
		failed = "第 " + itoa(e.Attempt) + "/" + itoa(e.MaxAttempts) + " 次尝试失败"
	}
	// 取消类（2026-09-24）：必须先于 !WillRetry 判定，且**绝不能**落到「重试已耗尽」——
	// 取消发生在第 1 次尝试上（实测日志：48 条 llm_error 里 6 条取消全是 attempt=1），
	// 一次都没重试却说「重试已耗尽」，用户读到的是「重试机制没了」。两者都不重试，但归因不同：
	//   - aborted（用户点停止 / agent_interrupt）→ 是用户自己的动作，说「已中断」；
	//   - canceled（会话关闭 / 后台任务终止/超时）→ 用户没按停止，只陈述取消事实，不归因给用户。
	if e.ErrorKind == "aborted" {
		return "（LLM 请求已中断：" + failed + " 时用户中断，未重试）" + e.Message
	}
	if e.ErrorKind == "canceled" {
		return "（LLM 请求已取消：" + failed + " 时被取消，未重试）" + e.Message
	}
	if !e.WillRetry {
		// 上游模型服务瞬时不可用 + 重试耗尽：必须给出归因——否则用户会以为自己的请求
		// 或本地环境有问题。
		if e.ErrorKind == "upstream_unavailable" {
			return "（上游模型服务连续不可用，" + failed + "、重试已耗尽后终止；这是供应商侧瞬时故障，不是你的请求造成的，稍后重试即可）" + e.Message
		}
		return "（LLM 请求失败：" + failed + "、重试已耗尽）" + e.Message
	}
	next := "重试第 " + itoa(e.Attempt+1) + " 次"
	if e.MaxAttempts > 0 {
		next = "重试第 " + itoa(e.Attempt+1) + "/" + itoa(e.MaxAttempts) + " 次"
	}
	return "（LLM 请求失败：" + failed + " → " + next + "）" + e.Message
}

// applyToolStartLocked 创建/更新工具块与链路节点（toolId 幂等）。
func (r *ViewReducer) applyToolStartLocked(e *events.ToolStart) {
	r.flushChunkLocked()
	key := "tool_start:" + e.Id
	if r.toolStartSeen[key] {
		return
	}
	r.toolStartSeen[key] = true

	// 嵌套子调用（parent_call_id 非空）**不生成独立工具块/链路节点**：它不是模型发起的
	// 工具调用，明细只挂在外层编排工具（codemode）那一块的 NestedCalls 上（设计文档 §17
	// 第一条）。宿主渲染器同一规则见 desktop/app/src/store/useAppStore.ts 的 tool_start ——
	// 两边必须一致，否则刷新/恢复后这些行会「凭空出现」。
	if e.ParentCallId != "" {
		return
	}

	ts := e.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	r.toolStartTs[e.Id] = ts

	if r.isSubRunLocked(e.RunId) {
		bid := r.agentBlockID[e.RunId]
		if idx := r.findBlockLocked(bid); idx >= 0 && r.blocks[idx].Kind == blockKindAgent {
			b := r.blocks[idx]
			if !hasAgentToolItem(b.Items, e.Id) {
				item := AgentItem{
					Kind: agentItemTool, ToolID: e.Id, Name: e.Name, Args: e.Arguments,
					Status: statusRunning, StartedAt: millis(ts), TaskID: e.TaskId,
				}
				b.Items = append(b.Items, item)
				r.blocks[idx] = b
				r.markBlockDirty(b.ID)
			}
		}
		return
	}

	bid := r.toolBlockID[e.Id]
	if bid != 0 {
		if idx := r.findBlockLocked(bid); idx >= 0 && r.blocks[idx].Kind == blockKindTool {
			b := r.blocks[idx]
			b.Name = e.Name
			b.Args = e.Arguments
			if e.TaskId != "" {
				b.TaskID = e.TaskId
			}
			r.blocks[idx] = b
			r.markBlockDirty(b.ID)
		}
		return
	}
	bid = r.nextBlockID()
	r.toolBlockID[e.Id] = bid
	r.blocks = append(r.blocks, Block{
		Kind: blockKindTool, ID: bid, ToolID: e.Id, Name: e.Name, Args: e.Arguments,
		Status: statusRunning, StartedAt: millis(ts), TaskID: e.TaskId,
	})
	r.noteBlockAppendLocked(bid)
	if e.RunId != "" {
		r.runNodes = append(r.runNodes, RunNode{
			ID: "tool-" + e.Id, ParentID: e.RunId, Label: labelForTool(e.Name, e.Arguments),
			Status: statusRunning, Depth: 0, Kind: runNodeKindTool, Name: e.Name,
			Args: e.Arguments, StartedAt: millis(ts), TaskID: e.TaskId,
		})
	}
}

// applyToolResponseLocked 结算工具调用：结果/错误/diff/时长/终态。
func (r *ViewReducer) applyToolResponseLocked(e *events.ToolResponse, meta EventMeta) {
	r.flushChunkLocked()
	key := "tool_end:" + e.Id
	if r.toolEndSeen[key] {
		return
	}
	r.toolEndSeen[key] = true

	// 嵌套子调用（parent_call_id 非空）不结算任何工具块/工具项/链路节点 —— 与 applyToolStartLocked
	// 同一条规则（设计文档 §17 第一条；宿主侧同规则见 desktop/app 的 useAppStore）。
	// 但**会话级事实照常入档**：diff 是真实发生的文件变更、todos 是脚本改过的计划快照 ——
	// 因为「不显示成一行」而丢掉它们，会让 Git 面板/待办列表在脚本跑完后是错的。
	nested := e.ParentCallId != ""

	ts := e.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	var durMs int64
	if started, ok := r.toolStartTs[e.Id]; ok {
		durMs = msBetween(started, ts)
		delete(r.toolStartTs, e.Id)
	}
	info := toolErrorInfo(e)

	if nested {
		// 子调用没有自己的工具块/工具项/链路节点：只留会话级事实（见上），
		// 行内清单挂在外层编排工具（codemode）那一块上。
		if e.Diff != nil {
			r.diffs = append(r.diffs, *fileDiff(e.Diff))
		}
		// 待办归属仍按 run 判定：脚本由子 agent 发起 → 归那张子卡片（不污染主会话清单）。
		if meta.Todos != nil {
			if r.isSubRunLocked(e.RunId) {
				r.applySubTodosLocked(e.RunId, meta.Todos)
			} else {
				r.todos = cloneTodos(meta.Todos)
			}
		}
		delete(r.approvalSeen, "approval:"+e.Id)
		return
	}

	if r.isSubRunLocked(e.RunId) {
		bid := r.agentBlockID[e.RunId]
		if idx := r.findBlockLocked(bid); idx >= 0 && r.blocks[idx].Kind == blockKindAgent {
			b := r.blocks[idx]
			items := make([]AgentItem, 0, len(b.Items))
			updated := false
			for _, it := range b.Items {
				if it.Kind == agentItemTool && it.ToolID == e.Id {
					status := statusDone
					if e.IsError {
						status = statusError
					}
					it.Status = status
					it.Result = e.Result
					it.ErrorInfo = info
					it.DurMs = durMs
					it.Diff = fileDiff(e.Diff)
					it.Images = contentBlocks(e.Images)
					// 编排型工具（codemode）：脚本内的嵌套调用清单（子 agent 转录里的脚本行同样要能展开）
					it.NestedCalls = nestedCalls(e.NestedCalls)
					it.HostOps = e.HostOps
					// todo_* 子 agent 调用：定格**该子 agent 自己**的清单快照（历史计划卡
					// 显示当时状态；缺失时渲染层回退卡片级 Todos）
					if meta.Todos != nil {
						it.Todos = cloneTodos(meta.Todos)
					}
					// 子 agent 卡片内的工具项：本次调用用量（仅子 agent 类工具非零）
					if e.Usage != nil && !e.Usage.IsZero() {
						u := taskUsageFromCore(*e.Usage)
						it.Usage = &u
					}
					updated = true
				}
				items = append(items, it)
			}
			if updated {
				b.Items = items
				r.blocks[idx] = b
				r.markBlockDirty(b.ID)
			}
		}
		// **不再写 r.todos**：那是主会话的待办清单，子 agent 的清单归它自己的卡片。
		if meta.Todos != nil {
			r.applySubTodosLocked(e.RunId, meta.Todos)
		}
		return
	}

	bid := r.toolBlockID[e.Id]
	idx := r.findBlockLocked(bid)
	if idx < 0 {
		// 异常序：只有 end 无 start —— 兜底补块（保持终态可见）
		bid = r.nextBlockID()
		r.toolBlockID[e.Id] = bid
		r.blocks = append(r.blocks, Block{Kind: blockKindTool, ID: bid, ToolID: e.Id})
		r.noteBlockAppendLocked(bid)
		idx = r.findBlockLocked(bid)
	}
	if idx >= 0 && r.blocks[idx].Kind == blockKindTool {
		b := r.blocks[idx]
		b.Name = e.Name
		b.Args = e.Arguments
		b.Result = e.Result
		b.IsError = e.IsError
		b.ErrorInfo = info
		b.DurMs = durMs
		b.Status = statusDone
		b.Diff = fileDiff(e.Diff)
		b.Images = contentBlocks(e.Images)
		// 编排型工具（codemode）的嵌套调用清单：与结果同一时刻定格（重放/恢复后清单不丢）
		b.NestedCalls = nestedCalls(e.NestedCalls)
		b.HostOps = e.HostOps
		// 本次调用的用量（子 agent 类工具经 ToolUsageProvider 回传，含成本；普通工具全零）——
		// 一次调用的用量是**一次性**值（非累计），直接覆盖；卡片/工具行据此展示 tokens + 成本。
		if e.Usage != nil && !e.Usage.IsZero() {
			b.Usage = taskUsageFromCore(*e.Usage)
		}
		if b.Promoted && b.TaskID != "" {
			b.TaskStatus = statusCompleted
		}
		if meta.Todos != nil {
			b.Todos = cloneTodos(meta.Todos)
			r.todos = cloneTodos(meta.Todos)
		}
		r.blocks[idx] = b
		r.markBlockDirty(b.ID)
	}
	if e.Diff != nil {
		r.diffs = append(r.diffs, *fileDiff(e.Diff))
	}
	if e.RunId != "" {
		for i := range r.runNodes {
			if r.runNodes[i].ID == "tool-"+e.Id {
				n := r.runNodes[i]
				n.Status = statusDone
				if e.IsError {
					n.Status = statusError
				}
				n.Result = e.Result
				n.IsError = e.IsError
				n.DurMs = durMs
				r.runNodes[i] = n
				break
			}
		}
	}
	delete(r.approvalSeen, "approval:"+e.Id)
}

// applyTaskPromotedLocked 标记工具行/节点为后台任务态。
func (r *ViewReducer) applyTaskPromotedLocked(e *events.TaskPromoted) {
	if e.TaskId == "" {
		return
	}
	for i := range r.blocks {
		b := r.blocks[i]
		if b.Kind == blockKindTool && b.TaskID == e.TaskId {
			b.Promoted = true
			b.TaskStatus = statusRunning
			r.blocks[i] = b
		}
	}
	for i := range r.runNodes {
		if r.runNodes[i].Kind == runNodeKindTool && r.runNodes[i].TaskID == e.TaskId {
			r.runNodes[i].Promoted = true
		}
	}
}

// applyAgentStartLocked 建立主/子 run 节点与子 agent 卡片。
// applyAgentUserMessageLocked 把一条投向子运行上下文的 user 消息记进它自己的转录。
//
// 归属锚是 **TaskId**（与 AgentStart.TaskId 同一个键）：同步子智能体（in-loop）没有
// 后台任务 id，也就没有 inbox，因此不会有本事件 —— 那种情况下没有"对它单独说话"这条路。
//
// 找不到卡片（start 还没到 / 卡片已收敛）时**静默丢弃**：这条消息在子运行的上下文里
// 仍然有效（它已经进了 LLM 请求），只是没有卡片可挂。为它凭空建一张卡片会让左栏树
// 多出一个没有 run 的孤儿节点。
func (r *ViewReducer) applyAgentUserMessageLocked(e *events.AgentUserMessage) {
	r.flushChunkLocked()
	if e.TaskId == "" {
		return
	}
	for i := range r.blocks {
		b := &r.blocks[i]
		if b.Kind != blockKindAgent || b.TaskID != e.TaskId {
			continue
		}
		text := strings.TrimSpace(e.Text)
		if text == "" {
			return
		}
		ts := e.Timestamp
		if ts.IsZero() {
			ts = time.Now()
		}
		b.Items = append(b.Items, AgentItem{
			Kind:      agentItemUser,
			Text:      text,
			StartedAt: millis(ts),
			// Source 不进 AgentItem 的展示字段（主会话的 user 块也没有"谁说的"），
			// 仅用于日志/诊断时区分投递来源。
		})
		// 与被裁到上限的其它 item 一样：超出部分只在 journal 里（见 agentItemsLimit）。
		if n := len(b.Items); n > agentItemsLimit {
			b.Items = append([]AgentItem(nil), b.Items[n-agentItemsLimit:]...)
			b.ItemsTrimmed += n - agentItemsLimit
		}
		return
	}
}

func (r *ViewReducer) applyAgentStartLocked(e *events.AgentStart) {
	r.flushChunkLocked()
	key := "agent_start:" + e.RunId
	if r.agentStartSeen[key] {
		return // 同一 run 重复 start：幂等
	}
	r.agentStartSeen[key] = true

	ts := e.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	label := e.Name
	if label == "" {
		label = e.Content
	}
	node := RunNode{
		ID: e.RunId, ParentID: e.ParentRunId, Label: label, Status: statusRunning,
		Depth: e.Depth, Kind: runNodeKindRun, StartedAt: millis(ts),
	}
	if e.ParentRunId == "" {
		// 新顶层 run = 新 trace：丢弃旧 trace 与错误块
		r.runNodes = []RunNode{node}
		r.blocks = filterBlocks(r.blocks, func(b Block) bool { return b.Kind != blockKindError })
		r.blockIdxDirty = true
		if e.ContextWindow > 0 {
			r.contextWindow = e.ContextWindow
		}
		return
	}
	// 子 agent：追加节点 + 卡片（替换同 taskId 的 async_task 占位）
	r.runNodes = append(r.runNodes, node)
	bid := r.agentBlockID[e.RunId]
	if bid != 0 {
		if idx := r.findBlockLocked(bid); idx >= 0 && r.blocks[idx].Kind == blockKindAgent {
			b := r.blocks[idx]
			b.Label = label
			b.SpawnedAt = millis(ts)
			if e.ParentRunId != "" {
				b.ParentRunID = e.ParentRunId
			}
			if e.TaskId != "" {
				b.TaskID = e.TaskId
			}
			r.blocks[idx] = b
			r.markBlockDirty(b.ID)
		}
		return
	}
	bid = r.nextBlockID()
	r.agentBlockID[e.RunId] = bid
	// 移除 async_task 占位（同 taskId 或同 label 的 running 占位）
	blocks := make([]Block, 0, len(r.blocks))
	taskID := e.TaskId
	if taskID == "" {
		for i := len(r.blocks) - 1; i >= 0; i-- {
			b := r.blocks[i]
			if b.Kind == blockKindAsyncTask && (b.Status == statusRunning || b.Status == "interrupting") && b.Label == label {
				taskID = b.TaskID
				break
			}
		}
	}
	for _, b := range r.blocks {
		if taskID != "" && b.Kind == blockKindAsyncTask && b.TaskID == taskID {
			continue
		}
		blocks = append(blocks, b)
	}
	r.blocks = append(blocks, Block{
		Kind: blockKindAgent, ID: bid, RunID: e.RunId, ParentRunID: e.ParentRunId, Label: label,
		Status: statusRunning, SpawnedAt: millis(ts), TaskID: taskID,
		Items: make([]AgentItem, 0),
	})
	r.blockIdxDirty = true // blocks 由 filter 产物重建（含去占位）
	r.noteBlockAppendLocked(bid)
}

// applyAgentEndLocked 结算 run 节点、子 agent 卡片与主 run 中断/截断提示。
func (r *ViewReducer) applyAgentEndLocked(e *events.AgentEnd) {
	r.flushChunkLocked()
	key := "agent_end:" + e.RunId
	if r.agentEndSeen[key] {
		return
	}
	r.agentEndSeen[key] = true

	now := e.Timestamp
	if now.IsZero() {
		now = time.Now()
	}
	runStatus := finishToRunStatus(e.FinishReason)
	taskStatus := finishToTaskStatus(e.FinishReason)
	aborted := e.FinishReason == core.FinishReasonAbort
	truncated := e.FinishReason == core.FinishReasonMaxIterations

	r.lastRun = &LastRun{
		RunID: e.RunId, Status: runStatus, FinishReason: string(e.FinishReason),
		TaskID: e.TaskId, EndedAt: millis(now),
	}
	if runStatus == statusError && e.Error != "" {
		r.lastError = &LastError{Message: e.Error, Kind: e.ErrorKind, At: millis(now)}
	}

	// run 节点结算
	for i := range r.runNodes {
		n := r.runNodes[i]
		if n.ID != e.RunId {
			continue
		}
		n.Status = runStatus
		if e.DurationMS > 0 {
			n.DurMs = e.DurationMS
		} else if n.StartedAt > 0 {
			n.DurMs = millis(now) - n.StartedAt
		}
		r.runNodes[i] = n
		break
	}

	isSub := r.isSubRunLocked(e.RunId)
	if isSub {
		bid := r.agentBlockID[e.RunId]
		if idx := r.findBlockLocked(bid); idx >= 0 && r.blocks[idx].Kind == blockKindAgent {
			b := r.blocks[idx]
			b.Status = taskStatus
			if b.TaskID == "" && e.TaskId != "" {
				b.TaskID = e.TaskId
			}
			if e.DurationMS > 0 {
				b.DurMs = e.DurationMS
			} else if !b.SpawnedAtZero() {
				b.DurMs = millis(now) - b.SpawnedAt
			}
			if taskStatus == statusFailed && e.Error != "" {
				b.TaskError = e.Error
			}
			if aborted {
				b.Items = abortAgentCompressionItems(b.Items)
			}
			// 该 block 已到终态（agent_end 之后不再追加 items）→ 裁剪展示用历史。
			// 这是 checkpoint 体积的大头（实测占 98%）。见 maxAgentItemsPerBlockDefault。
			if n := len(b.Items); n > agentItemsLimit {
				b.Items = append([]AgentItem(nil), b.Items[n-agentItemsLimit:]...)
				b.ItemsTrimmed += n - agentItemsLimit
			}
			// 子运行权威用量（AgentEnd.Usage = 整次运行跨轮累计，含压缩轮与成本）——
			// 覆盖按轮 llm_end 累加的近似值。异步子 agent 之后还会收到 TaskEnd（同源同值，
			// 幂等覆盖）；**同步子 agent 没有 TaskEnd**（不是任务），这里是它唯一的权威来源。
			if e.Usage != nil && !e.Usage.IsZero() {
				b.Usage = taskUsageFromCore(*e.Usage)
			}
			// 子 agent 的产出归入其卡片（不产生独立块——避免主对话被每个子 agent 的
			// 产出卡片刷屏；卡片展开即见）。宿主渲染为卡片尾部的 ArtifactCard。
			if len(e.Artifacts) > 0 {
				b.Artifacts = artifactRefs(e.Artifacts)
				b.ArtifactsTruncated = e.ArtifactsTruncated
			}
			r.blocks[idx] = b
			r.markBlockDirty(b.ID)
		}
		// 注意：不删除 agentBlockID —— 卡片保留在 blocks（终态也可渲染），
		// 迟到的 chunk/llm_end/compress_end 等事件仍需按 runId 路由到卡片
		// （与前端 isSubAgentRun 按 blocks 判定一致）。
		delete(r.agentCompressing, e.RunId)
		delete(r.compStart, e.RunId)
		return
	}

	// 主 run：Agent 总耗时结算（AgentStart→AgentEnd）挂在最终 assistant 块上，
	// 供前端「本轮 TTFT · LLM 总耗时 · Agent 总耗时」展示。LLMEnd 只结算
	// DurMs（单次 LLM 调用耗时），AgentEnd.DurationMS 才是整轮运行总时长。
	// 注意：必须按 RunID 匹配最后一条 assistant 块——多轮/后续 run 已追加新块时，
	// lastAssistantBlockLocked（全局最后一条）会结算到后续 run 的块，本轮块漏结算
	//（实测：run A 结束后 run B 已开始，A 的块 agent_dur_ms 恒为 None）。
	if e.DurationMS > 0 {
		if bid := r.lastAssistantBlockForRunLocked(e.RunId); bid != 0 {
			if idx := r.findBlockLocked(bid); idx >= 0 && r.blocks[idx].Kind == blockKindAssistant {
				b := r.blocks[idx]
				b.AgentDurMs = e.DurationMS
				r.blocks[idx] = b
				r.markBlockDirty(b.ID)
			}
		}
	}

	// 末轮兜底结算（2026-09-29）：中断 / run 级失败 / 达轮数上限截断都没有 llm_end，
	// 该轮正文块因此永远拿不到 DurMs/TtftMs —— 刷新后底部指标行缺一段（与前端实时
	// 分段渲染的缺口一致）。用 llm_start→AgentEnd 的**已流逝时间**补 DurMs；TtftMs 不补
	// （首字超时那类一个 chunk 都没来过，如实留空）。已结算的轮次不受影响（DurMs>0）。
	if ts, ok := r.turnStart[e.RunId]; ok && !ts.IsZero() {
		elapsed := msBetween(ts, now)
		if bid := r.lastAssistantBlockForRunLocked(e.RunId); bid != 0 {
			if idx := r.findBlockLocked(bid); idx >= 0 && r.blocks[idx].Kind == blockKindAssistant {
				b := r.blocks[idx]
				if b.DurMs == 0 && elapsed > 0 {
					b.DurMs = elapsed
				}
				if b.ThinkMs == 0 {
					if st, ok2 := r.thinkStart[e.RunId]; ok2 {
						b.ThinkMs = msBetween(st, now)
					}
				}
				r.blocks[idx] = b
				r.markBlockDirty(b.ID)
			}
		}
	}
	delete(r.turnStart, e.RunId)
	delete(r.thinkStart, e.RunId) // 末轮思考计时在此落地（llm_end 不会再来），清瞬态

	// 主 run：中断/截断提示 + 工具块收尾
	blocks := r.blocks
	if aborted {
		blocks = filterBlocks(blocks, func(b Block) bool { return b.Kind != blockKindError })
		for i := range blocks {
			if blocks[i].Kind == blockKindTool && blocks[i].Status == statusRunning {
				blocks[i].Status = statusDone
			}
		}
		// 产出块插在 divider **之前**：产出属于本轮成果，横线分隔的是下一轮上下文。
		blocks = appendArtifactsBlockLocked(r, blocks, e, now)
		// 用户中断：不补「（已中断）」文本，插一条 divider（横线）作上下文隔离；
		// 内部 reason=abort 与 shutdown 区分（UI 同款横线样式）。
		blocks = append(blocks, Block{Kind: blockKindDivider, ID: r.nextBlockID(), DividerReason: "abort", Ts: millis(now)})
	} else {
		blocks = appendArtifactsBlockLocked(r, blocks, e, now)
		if truncated {
			blocks = append(blocks, Block{Kind: blockKindAssistant, ID: r.nextBlockID(), Text: "（已达本轮轮数上限，任务未完成；输入「继续」可续跑）", Ts: millis(now)})
		}
	}
	r.blocks = blocks
	r.blockIdxDirty = true // aborted 分支经 filter 重建
	r.mainCompressing = false
	delete(r.compStart, "")
}

// appendArtifactsBlockLocked 把 AgentEnd.Artifacts 投影为一个独立块（本轮产出汇总）。
// 无产出 → 原样返回（不产生空块，保持叙述区整洁）。
// 去重：同一 run 重复 AgentEnd（重连补发/重放）不追加第二个块——见 agentEndSeen 前置去重，
// 此处仅防御性检查（同 runId 已有产出块则跳过）。
func appendArtifactsBlockLocked(r *ViewReducer, blocks []Block, e *events.AgentEnd, now time.Time) []Block {
	if len(e.Artifacts) == 0 {
		return blocks
	}
	for i := range blocks {
		if blocks[i].Kind == blockKindArtifacts && blocks[i].RunID == e.RunId {
			return blocks // 同 run 已有产出块（幂等）
		}
	}
	refs := make([]ArtifactRef, 0, len(e.Artifacts))
	for _, a := range e.Artifacts {
		refs = append(refs, ArtifactRef{
			Name: a.Name, Path: a.Path, Kind: a.Kind,
			Added: a.Added, Removed: a.Removed,
			Size: a.Size, Lines: a.Lines, SHA256: a.SHA256,
		})
	}
	return append(blocks, Block{
		Kind: blockKindArtifacts, ID: r.nextBlockID(), RunID: e.RunId,
		Artifacts: refs, ArtifactsTruncated: e.ArtifactsTruncated, Ts: millis(now),
	})
}

// artifactRefs 把 core.Artifact 投影为 checkpoint 的 ArtifactRef 列表。
func artifactRefs(in []core.Artifact) []ArtifactRef {
	out := make([]ArtifactRef, 0, len(in))
	for _, a := range in {
		out = append(out, ArtifactRef{
			Name: a.Name, Path: a.Path, Kind: a.Kind,
			Added: a.Added, Removed: a.Removed,
			Size: a.Size, Lines: a.Lines, SHA256: a.SHA256,
		})
	}
	return out
}

// taskLifecycleKey 任务生命周期的去重键（taskId + 事件时刻）。
//
// 为什么不能只按 taskId（会话级恢复引入，见 docs/…TASKOUTPUT.md §13）：agent_resume
// **原地续跑**同一 task_id，同一任务会经历多个 start→end 生命周期。按 taskId 去重会让
// 第二轮的 TaskStarted / TaskEnd / TaskResultDelivered 全部被丢弃 —— Go 侧 reducer
// 不退终态、前端拿不到续跑结果，表现就是「任务结束了但没有结果通知」。
// 加入事件时刻（纳秒精度）→ 同轮事件重放仍幂等，新一轮正常通过。
func taskLifecycleKey(prefix, taskID string, ts time.Time) string {
	if ts.IsZero() {
		return prefix + ":" + taskID // 无时刻信息：退回按 task 去重（保守，不重复建块）
	}
	return prefix + ":" + taskID + ":" + strconv.FormatInt(ts.UnixNano(), 10)
}

// applyTaskStartedLocked 建立/更新 async_task 占位（同一生命周期内幂等）。
func (r *ViewReducer) applyTaskStartedLocked(e *events.TaskStarted) {
	r.flushChunkLocked()
	key := taskLifecycleKey("task_started", e.TaskId, e.Timestamp)
	if r.taskStartedSeen[key] {
		return
	}
	r.taskStartedSeen[key] = true
	if e.TaskId == "" {
		return
	}
	ts := e.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	for i := range r.blocks {
		b := r.blocks[i]
		if (b.Kind == blockKindAsyncTask || b.Kind == blockKindAgent) && b.TaskID == e.TaskId {
			if b.Status == statusRunning || b.Status == "interrupting" {
				b.Status = statusRunning
				r.blocks[i] = b
			}
			return
		}
	}
	label := e.Name
	if label == "" {
		label = e.ToolName
	}
	if label == "" {
		label = "后台任务"
	}
	r.blocks = append(r.blocks, Block{
		Kind: blockKindAsyncTask, ID: r.nextBlockID(), TaskID: e.TaskId, Label: label,
		Name: e.ToolName, Status: statusRunning, StartedAt: millis(ts),
	})
}

// applyTaskEndLocked 收敛任务终态（agent / async_task / promoted tool 卡）。
func (r *ViewReducer) applyTaskEndLocked(e *events.TaskEnd) {
	r.flushChunkLocked()
	key := taskLifecycleKey("task_end", e.TaskId, e.Timestamp)
	if r.taskEndSeen[key] {
		return
	}
	r.taskEndSeen[key] = true
	if e.TaskId == "" {
		return
	}
	ts := e.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	status := e.Status
	if status == "" {
		if e.Error != "" {
			status = statusFailed
		} else {
			status = statusCompleted
		}
	}
	for i := range r.blocks {
		b := r.blocks[i]
		switch b.Kind {
		case blockKindAgent, blockKindAsyncTask:
			if b.TaskID == e.TaskId {
				b.Status = status
				if b.Kind == blockKindAsyncTask {
					b.EndedAt = millis(ts)
				} else if b.DurMs == 0 && b.SpawnedAt > 0 {
					b.DurMs = millis(ts) - b.SpawnedAt
				}
				if e.Usage != nil {
					b.Usage = taskUsageFromCore(*e.Usage) // 权威全量（含压缩轮）+ 成本
				}
				r.blocks[i] = b
			}
		case blockKindTool:
			if b.TaskID == e.TaskId && b.Promoted {
				b.Status = statusDone
				b.TaskStatus = status
				if b.DurMs == 0 && b.StartedAt > 0 {
					b.DurMs = millis(ts) - b.StartedAt
				}
				if e.Usage != nil {
					b.Usage = taskUsageFromCore(*e.Usage)
				}
				r.blocks[i] = b
			}
		}
	}
}

// applyTaskResultDeliveredLocked 回填任务结果（agent/async_task/promoted tool；
// 找不到任务时保留独立结果块；taskId 幂等）。
func (r *ViewReducer) applyTaskResultDeliveredLocked(e *events.TaskResultDelivered) {
	r.flushChunkLocked()
	key := taskLifecycleKey("task_result", e.TaskId, e.Timestamp)
	if r.taskResultSeen[key] {
		return
	}
	r.taskResultSeen[key] = true
	if e.TaskId == "" {
		return
	}
	ts := e.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	status := e.Status
	if status == "" {
		if e.Error != "" {
			status = statusFailed
		} else {
			status = statusCompleted
		}
	}
	idx := -1
	for i := range r.blocks {
		b := r.blocks[i]
		if (b.Kind == blockKindAgent || b.Kind == blockKindAsyncTask || b.Kind == blockKindTool) && b.TaskID == e.TaskId {
			idx = i
			break
		}
	}
	if idx < 0 {
		// 旧日志/异常顺序找不到任务：保留结果块避免丢失（去重后仅一次）
		r.blocks = append(r.blocks,
			Block{Kind: blockKindTaskResult, ID: r.nextBlockID(), TaskID: e.TaskId, Result: e.Result, Error: e.Error, Ts: millis(ts)},
			Block{Kind: blockKindTaskDelivery, ID: r.nextBlockID(), TaskID: e.TaskId, Status: status, Result: e.Result, Error: e.Error, Ts: millis(ts)},
		)
		return
	}
	b := r.blocks[idx]
	switch b.Kind {
	case blockKindTool:
		b.Status = statusDone
		b.TaskStatus = status
		b.IsError = status == statusFailed
		if e.Result != "" {
			b.Result = e.Result
		} else if e.Error != "" {
			b.Error = e.Error
		}
		if b.DurMs == 0 && b.StartedAt > 0 {
			b.DurMs = millis(ts) - b.StartedAt
		}
		if e.Usage != nil {
			b.Usage = taskUsageFromCore(*e.Usage)
		}
	case blockKindAgent, blockKindAsyncTask:
		b.Status = status
		b.TaskResult = e.Result
		b.TaskError = e.Error
		b.DeliveredToMain = true
		if e.Usage != nil {
			b.Usage = taskUsageFromCore(*e.Usage)
		}
	}
	r.blocks[idx] = b
	r.markBlockDirty(b.ID)
}

// applyCompressStartLocked 记录压缩瞬态（主/子）。
func (r *ViewReducer) applyCompressStartLocked(e *events.CompressStart) {
	r.flushChunkLocked()
	started := time.Now()
	r.compStart[e.RunId] = started
	if r.isSubRunLocked(e.RunId) {
		bid := r.agentBlockID[e.RunId]
		if idx := r.findBlockLocked(bid); idx >= 0 && r.blocks[idx].Kind == blockKindAgent {
			b := r.blocks[idx]
			b.Items = append(b.Items, AgentItem{
				Kind: agentItemCompression, Before: e.Before, After: e.Before,
				Reason: e.Reason, Active: true, StartedAt: millis(started),
			})
			r.blocks[idx] = b
			r.markBlockDirty(b.ID)
		}
		r.agentCompressing[e.RunId] = true
		return
	}
	r.mainCompressing = true
}

// applyCompressEndLocked 收敛压缩：主 agent 追加压缩块；子 agent 更新卡片项。
// 压缩事件无时间戳字段，耗时按接收时刻结算（重放不可准确重建，属 best-effort）。
func (r *ViewReducer) applyCompressEndLocked(e *events.CompressEnd, meta EventMeta) {
	r.flushChunkLocked()
	runKey := e.RunId
	key := "compress_end:" + runKey + "\x00" + itoa(e.Before) + "\x00" + itoa(e.After) + "\x00" + e.Model + "\x00" + e.Summary
	if r.compressEndSeen[key] {
		return
	}
	r.compressEndSeen[key] = true

	var durMs int64
	if started, ok := r.compStart[runKey]; ok {
		durMs = msBetween(started, time.Now())
		delete(r.compStart, runKey)
	}
	aborted := e.Error != ""

	// 压缩调用成本计入会话展示累计（与 SDK ac.addUsage 同口径：llm_end 不覆盖压缩轮，
	// 压缩用量只在 CompressEnd.Usage）。子 agent 的压缩**同样计入**——此前计费代码在
	// 子分支 return 之后，会话展示成本漏掉子 agent 的压缩轮（子卡片成本同时累加，
	// 运行中近似；任务结束时被 TaskEnd 的权威值覆盖）。
	compCost := 0.0
	if e.Usage != nil && e.Usage.Cost.Total > 0 {
		compCost = float64(e.Usage.Cost.Total) / 1e6
		r.costUSD += compCost
		r.lifetimeCost += compCost
	}

	if r.isSubRunLocked(runKey) {
		bid := r.agentBlockID[runKey]
		if idx := r.findBlockLocked(bid); idx >= 0 && r.blocks[idx].Kind == blockKindAgent {
			b := r.blocks[idx]
			b.Usage.CostUSD += compCost
			b.Items = upsertAgentCompressionItem(b.Items, AgentItem{
				Kind: agentItemCompression, Before: e.Before, After: e.After,
				CtxTokens: e.CtxTokens, Reason: e.Reason, Model: e.Model,
				Summary: e.Summary, Usage: taskUsagePtr(e.Usage), Error: e.Error,
				Aborted: aborted, Active: false, DurMs: durMs, Analysis: e.Analysis,
			})
			r.blocks[idx] = b
			r.markBlockDirty(b.ID)
		}
		delete(r.agentCompressing, runKey)
		// 子 agent 的压缩边界同样只刷新**它自己**的清单（主会话清单不动）
		if meta.Todos != nil {
			r.applySubTodosLocked(runKey, meta.Todos)
		}
		return
	}
	// 主 agent
	r.mainCompressing = false
	if meta.Todos != nil {
		r.todos = cloneTodos(meta.Todos)
	}
	if e.CtxTokens > 0 {
		// 压缩后的占用是字符/token 估算（agents.agent_loop estimateMessagesTokens）——
		// 标记口径，避免展示层把估算当 provider 真实 usage（下一轮 llm_end 会置回真实）。
		r.ctxTokens = e.CtxTokens
		r.ctxTokensEst = true
	}
	blk := Block{
		Kind: blockKindCompression, ID: r.nextBlockID(),
		Before: e.Before, After: e.After, Reason: e.Reason, RunID: e.RunId,
		CtxTokens: e.CtxTokens, Model: e.Model, Summary: e.Summary,
		Error: e.Error, Aborted: aborted, DurMs: durMs, Warnings: append([]string{}, e.Warnings...),
		Analysis: e.Analysis,
	}
	// 压缩调用用量（含成本）：此前只进会话累计、不落块 —— 重启/恢复后压缩卡的
	// tokens 行静默消失（前端 restore 读 b.usage）。落块后与实时一致。
	if e.Usage != nil && !e.Usage.IsZero() {
		blk.Usage = taskUsageFromCore(*e.Usage)
	}
	r.blocks = append(r.blocks, blk)
}

// applyCommandResultLocked 处理命令结果；clear 触发代际重置。
func (r *ViewReducer) applyCommandResultLocked(e *events.CommandResult) {
	r.flushChunkLocked()
	key := "command_result:" + e.Name + "\x00" + uitoa(uint64(millis(e.Timestamp)))
	if r.commandResultSeen[key] {
		return
	}
	r.commandResultSeen[key] = true
	if e.Name == "clear" {
		r.clearLocked()
		return
	}
	text := e.Result
	if e.Error != "" {
		text = e.Error
	}
	if text == "" {
		return
	}
	if e.Name == "skills" {
		skillName := skillsResultName(text)
		r.blocks = append(r.blocks, Block{
			Kind: blockKindTool, ID: r.nextBlockID(), ToolID: "cmd-" + uitoa(r.blockSeq),
			Name: "SKILL", Args: skillName, Result: text, Status: statusDone,
			IsError: e.Error != "", Ts: millis(e.Timestamp),
		})
		return
	}
	if e.Name == "reload_skills" {
		tone := "success"
		if e.Error != "" {
			tone = "error"
		}
		r.blocks = append(r.blocks, Block{Kind: blockKindSystem, ID: r.nextBlockID(), Text: text, Tone: tone, Ts: millis(e.Timestamp)})
		return
	}
	r.blocks = append(r.blocks, Block{Kind: blockKindAssistant, ID: r.nextBlockID(), Text: text, Ts: millis(e.Timestamp)})
}

// applyUserInputsConsumedLocked 按服务端确认把用户消息（含附件）加入对话。
func (r *ViewReducer) applyUserInputsConsumedLocked(e *events.UserInputsConsumed) {
	r.flushChunkLocked()
	key := userInputsKey(e)
	if r.userInputsSeen[key] {
		return
	}
	r.userInputsSeen[key] = true
	if len(e.UserTexts) == 0 && len(e.UserContents) == 0 {
		return
	}
	ts := e.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	n := len(e.UserContents)
	if n == 0 {
		n = len(e.UserTexts)
	}
	for i := 0; i < n; i++ {
		text := ""
		var contents []Content
		if i < len(e.UserContents) && len(e.UserContents[i]) > 0 {
			for _, c := range e.UserContents[i] {
				ct := Content{Type: c.Type, Content: c.Content, MimeType: c.MimeType}
				if c.Type == core.ContentTypeImage {
					ct.Type = "image"
				} else {
					ct.Type = "text"
				}
				contents = append(contents, ct)
			}
			text = joinText(contents)
			if text == "" {
				text = "(附件)"
			}
		} else if i < len(e.UserTexts) {
			text = e.UserTexts[i]
		} else {
			continue
		}
		r.blocks = append(r.blocks, Block{
			Kind: blockKindUser, ID: r.nextBlockID(), Text: text,
			Contents: contents, Ts: millis(ts),
		})
	}
}

// userInputsKey 用户消费事件的去重键：时间戳 + 内容数量/首条指纹。
// 仅时间戳会在同毫秒内误丢两条不同消费（数据损失）；加指纹后只有完全相同的
// 事件重发才被去重（IPC 重放尾部/重复投递场景）。
func userInputsKey(e *events.UserInputsConsumed) string {
	var first string
	switch {
	case len(e.UserTexts) > 0:
		first = e.UserTexts[0]
	case len(e.UserContents) > 0 && len(e.UserContents[0]) > 0:
		first = e.UserContents[0][0].Content
	}
	if len(first) > 64 {
		first = first[:64] // 指纹截断：只用于区分同毫秒的不同事件，不要求全量
	}
	return "user_inputs:" + uitoa(uint64(millis(e.Timestamp))) +
		"\x00" + itoa(len(e.UserTexts)) + "\x00" + itoa(len(e.UserContents)) + "\x00" + first
}

// applySessionRunErrorLocked 记录运行级最终错误（错误块 + lastError）。
func (r *ViewReducer) applySessionRunErrorLocked(e *events.SessionRunError) {
	r.flushChunkLocked()
	msg := e.Error
	if msg == "" {
		msg = "任务运行失败"
	}
	at := e.Timestamp
	if at.IsZero() {
		at = time.Now()
	}
	r.lastError = &LastError{Message: e.Error, Kind: e.ErrorKind, At: millis(at)}
	r.upsertErrorBlockLocked("（运行失败）"+msg, nil, 0) // nil = 非重试错误（无重试进度/无重试成本）
}

// clearLocked 执行 clear 代际重置（持锁调用）。
func (r *ViewReducer) clearLocked() {
	r.clearGeneration++
	// 展示 usage/cost 归零；lifetime 对账值保留（SDK state 仍累计）
	r.usage = Usage{}
	r.costUSD = 0
	// 清空上下文：不推「（上下文已清空）」文本，插一条 divider（横线）作上下文隔离；
	// 内部 reason=clear 与 shutdown/abort 区分（UI 同款横线，无文本）。
	r.blocks = []Block{{Kind: blockKindDivider, ID: r.nextBlockID(), DividerReason: "clear", Ts: nowMillis()}}
	r.runNodes = []RunNode{}
	r.turns = []Turn{}
	r.diffs = []Diff{}
	r.lastRun = nil
	r.lastError = nil
	r.ctxTokens = 0
	r.ctxTokensEst = false
	r.contextWindow = 0
	r.chunk = nil
	r.reqState = map[string]*reqState{}
	r.thinkStart = map[string]time.Time{}
	r.turnThinkMs = map[string]int64{}
	r.turnStart = map[string]time.Time{}
	r.toolStartTs = map[string]time.Time{}
	r.compStart = map[string]time.Time{}
	r.agentCompressing = map[string]bool{}
	r.mainCompressing = false
	// 卡片/工具索引与新 blocks 同步清空（旧 sub run 的迟到事件路由到空 map 后无副作用）
	r.agentBlockID = map[string]uint64{}
	r.toolBlockID = map[string]uint64{}
	r.blockIdxDirty = true
	// todos 与 lifetime 保留；去重集合不清（同一事件重送依然幂等）
}

// upsertErrorBlockLocked 叙述通道错误块：原位更新（多次重试只留一条）。
func (r *ViewReducer) upsertErrorBlockLocked(text string, e *events.LLMError, costUSD float64) {
	retry := retryStateFrom(e)
	for i := range r.blocks {
		if r.blocks[i].Kind == blockKindError {
			b := r.blocks[i]
			// 本轮重试起点沿用首值（同一轮重试内累计「已等待时长」）；上一次已结束
			// （成功/耗尽/中止后 retrying=false）则本次失败重新起算。
			if retry.retrying && b.Retrying && b.RetryStartedAt > 0 {
				retry.startedAt = b.RetryStartedAt
			}
			b.Text = text
			b.Ts = retry.at
			b.Retrying = retry.retrying
			b.RetryAttempt = retry.attempt
			b.RetryMax = retry.max
			b.RetryDelayMs = retry.delayMs
			b.RetryStartedAt = retry.startedAt
			b.RetryCostUSD += costUSD // 跨多次重试累加（同一轮重试 = 同一条错误块）
			r.blocks[i] = b
			return
		}
	}
	r.blocks = append(r.blocks, Block{
		Kind: blockKindError, ID: r.nextBlockID(), Text: text, Ts: retry.at,
		Retrying: retry.retrying, RetryAttempt: retry.attempt, RetryMax: retry.max,
		RetryDelayMs: retry.delayMs, RetryStartedAt: retry.startedAt,
		RetryCostUSD: costUSD,
	})
}

// retryState 错误块的重试进度（落进 Block JSON，快照/重放后前端仍能算倒计时与已等待时长）。
type retryState struct {
	at        int64 // 锚点时刻：下一次尝试的起点 = at + delayMs
	retrying  bool
	attempt   int
	max       int
	delayMs   int64
	startedAt int64 // 本轮重试首个失败时刻（累计「已等待时长」）；非重试错误不置
}

// retryStateFrom 从 llm_error 事件取重试进度；e 为 nil（运行失败/中止等非重试错误）→ 无进度。
func retryStateFrom(e *events.LLMError) retryState {
	if e == nil {
		return retryState{at: nowMillis()}
	}
	at := nowMillis()
	if !e.Timestamp.IsZero() {
		at = e.Timestamp.UnixMilli() // 事件时刻：重放/补发时倒计时仍与当时一致
	}
	st := retryState{
		at:       at,
		retrying: e.WillRetry,
		attempt:  e.Attempt,
		max:      e.MaxAttempts,
		delayMs:  e.RetryDelayMs,
	}
	if e.WillRetry {
		st.startedAt = at // 首次失败即本轮起点；后续失败由 upsert 沿用旧值
	}
	return st
}

// applySubTodosLocked 把快照写到**该子 agent 自己的卡片**上（不碰 r.todos）。
// 子 agent 有独立待办清单：它调用 todo_* 时，清单属主是它那份 store（见
// agents.AgentContext.todoSnapshot 与 subagent.buildOptions 刻意不继承父 Todo），
// 宿主的会话清单必须保持不动 —— 否则子 agent 的计划会顶掉主会话的（用户 2026-10 报障）。
func (r *ViewReducer) applySubTodosLocked(runID string, todos []TodoItem) {
	bid := r.agentBlockID[runID]
	idx := r.findBlockLocked(bid)
	if idx < 0 || r.blocks[idx].Kind != blockKindAgent {
		return
	}
	b := r.blocks[idx]
	b.Todos = cloneTodos(todos)
	r.blocks[idx] = b
	r.markBlockDirty(b.ID)
}

// isSubRunLocked 判断 run 是否子 agent（有 agent 卡片或有父的 run 节点）。
func (r *ViewReducer) isSubRunLocked(runID string) bool {
	if runID == "" {
		return false
	}
	if r.agentBlockID[runID] != 0 {
		return true
	}
	for _, n := range r.runNodes {
		if n.ID == runID && n.Kind == runNodeKindRun && n.ParentID != "" {
			return true
		}
	}
	for _, b := range r.blocks {
		if b.Kind == blockKindAgent && b.RunID == runID {
			return true
		}
	}
	return false
}

// noteDiagLocked 记录/更新未知事件诊断（有界）。
func (r *ViewReducer) noteDiagLocked(eventType string, count int) {
	if r.diag == nil {
		r.diag = map[string]*diagEntry{}
	}
	d, ok := r.diag[eventType]
	if !ok {
		if len(r.diag) >= maxDiagnostics {
			// 淘汰 firstSeq 最旧的一条
			var oldestKey string
			var oldestSeq uint64
			first := true
			for k, v := range r.diag {
				if first || v.firstSeq < oldestSeq {
					oldestKey, oldestSeq, first = k, v.firstSeq, false
				}
			}
			delete(r.diag, oldestKey)
		}
		d = &diagEntry{}
		r.diag[eventType] = d
	}
	d.count += count
	if d.firstSeq == 0 || r.seq < d.firstSeq {
		d.firstSeq = r.seq
	}
	if r.seq > d.lastSeq {
		d.lastSeq = r.seq
	}
}

func (r *ViewReducer) diagSliceLocked() []Diagnostic {
	out := make([]Diagnostic, 0, len(r.diag))
	for k, d := range r.diag {
		out = append(out, Diagnostic{EventType: k, Count: d.count, FirstSeq: d.firstSeq, LastSeq: d.lastSeq})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FirstSeq < out[j].FirstSeq })
	return out
}

func (r *ViewReducer) nextBlockID() uint64 {
	r.blockSeq++
	return r.blockSeq
}

// ---------------------------------------------------------------------------
// 内部：工具函数
// ---------------------------------------------------------------------------

// normalizeSnapshotBlocks 把快照中的瞬时 active 压缩项收敛为 aborted。
func normalizeSnapshotBlocks(blocks []Block) {
	for i := range blocks {
		b := &blocks[i]
		if b.Kind != blockKindAgent {
			continue
		}
		for j := range b.Items {
			if b.Items[j].Kind == agentItemCompression && b.Items[j].Active {
				b.Items[j].Active = false
				b.Items[j].Aborted = true
			}
		}
	}
}

func finishToRunStatus(f core.FinishReason) string {
	switch f {
	case core.FinishReasonError:
		return statusError
	case core.FinishReasonAbort:
		return statusInterrupted
	default:
		return statusDone
	}
}

func finishToTaskStatus(f core.FinishReason) string {
	switch f {
	case core.FinishReasonError:
		return statusFailed
	case core.FinishReasonAbort:
		return statusInterrupted
	default:
		return statusCompleted
	}
}

func llmEndKey(e *events.LLMEnd, seq uint64) string {
	if e.RequestId != "" {
		return "llm_end:" + e.RunId + "\x00" + e.RequestId
	}
	// 无 request 的旧流：以结束时间戳（+seq 兜底零值时间）区分不同调用。
	return "llm_end:run:" + e.RunId + "\x00" + uitoa(uint64(millis(e.Timestamp))) + "\x00" + uitoa(seq)
}

func usageFromCore(u core.Usage) Usage {
	return Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
		CacheWrite1h: u.CacheWrite1h, Reasoning: u.Reasoning}
}

// taskUsageFromCore 任务卡（agent/async_task/promoted tool）的**权威**用量投影：
// 比 usageFromCore 多一层成本折算（µUSD → USD），用于 TaskEnd / TaskResultDelivered
// 携带的任务全量用量（覆盖运行中按轮累加的近似值——压缩轮不经 llm_end，增量会少算）。
func taskUsageFromCore(u core.Usage) Usage {
	return Usage{
		Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
		CacheWrite1h: u.CacheWrite1h, Reasoning: u.Reasoning, CostUSD: float64(u.Cost.Total) / 1e6,
	}
}

func addUsage(dst *Usage, u Usage) {
	dst.Input += u.Input
	dst.Output += u.Output
	dst.CacheRead += u.CacheRead
	dst.CacheWrite += u.CacheWrite
	dst.CacheWrite1h += u.CacheWrite1h
	dst.Reasoning += u.Reasoning
}

func toolErrorInfo(e *events.ToolResponse) *ToolErrorInfo {
	if !e.IsError {
		return nil
	}
	info := &ToolErrorInfo{Code: e.ErrorCode, Changed: e.Changed, Retryable: e.Retryable, NextAction: e.NextAction, Recovery: e.Recovery}
	if info.Code == "" && info.Changed == "" && !info.Retryable && info.NextAction == "" && info.Recovery == "" {
		return nil
	}
	return info
}

// contentBlocks core.Content 切片 → checkpoint Content 切片（原样保留 data URL 与 mime）。
// 图片块的唯一处理是「不落空」：空切片返回 nil（omitempty 语义，避免 checkpoint 里
// 出现一堆空数组）。
func contentBlocks(in []core.Content) []Content {
	if len(in) == 0 {
		return nil
	}
	out := make([]Content, 0, len(in))
	for _, c := range in {
		out = append(out, Content{Type: c.Type, Content: c.Content, MimeType: c.MimeType})
	}
	return out
}

func fileDiff(d *core.FileDiff) *Diff {
	if d == nil {
		return nil
	}
	return &Diff{Path: d.Path, Added: d.Added, Removed: d.Removed, Unified: d.Unified, Truncated: d.Truncated}
}

func usagePtr(u *core.Usage) *Usage {
	if u == nil {
		return nil
	}
	agg := usageFromCore(*u)
	return &agg
}

// taskUsagePtr 同 taskUsageFromCore 的指针形态（AgentItem.Usage 等可选字段用）。
func taskUsagePtr(u *core.Usage) *Usage {
	if u == nil {
		return nil
	}
	agg := taskUsageFromCore(*u)
	return &agg
}

func upsertAgentErrorItem(items []AgentItem, text string) []AgentItem {
	out := append([]AgentItem{}, items...)
	if n := len(out); n > 0 && out[n-1].Kind == agentItemText && out[n-1].IsError {
		out[n-1].Text = text
		return out
	}
	item := AgentItem{Kind: agentItemText, Text: text, IsError: true}
	return append(out, item)
}

func hasAgentToolItem(items []AgentItem, toolID string) bool {
	for _, it := range items {
		if it.Kind == agentItemTool && it.ToolID == toolID {
			return true
		}
	}
	return false
}

func upsertAgentCompressionItem(items []AgentItem, item AgentItem) []AgentItem {
	out := append([]AgentItem{}, items...)
	// 找 active 压缩项原位收敛；不存在则追加
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Kind == agentItemCompression && out[i].Active {
			it := item
			it.Kind = agentItemCompression
			it.StartedAt = out[i].StartedAt
			out[i] = it
			return out
		}
	}
	return append(out, item)
}

func abortAgentCompressionItems(items []AgentItem) []AgentItem {
	out := append([]AgentItem{}, items...)
	for i := range out {
		if out[i].Kind == agentItemCompression && out[i].Active {
			out[i].Active = false
			out[i].Aborted = true
		}
	}
	return out
}

func filterBlocks(blocks []Block, keep func(Block) bool) []Block {
	out := make([]Block, 0, len(blocks))
	for _, b := range blocks {
		if keep(b) {
			out = append(out, b)
		}
	}
	return out
}

func joinText(contents []Content) string {
	var b strings.Builder
	for _, c := range contents {
		if c.Type == "text" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(c.Content)
		}
	}
	return strings.TrimSpace(b.String())
}

func labelForTool(name, _ string) string {
	if name == "" {
		return "tool"
	}
	return name
}

func skillsResultName(text string) string {
	const prefix = `skill "`
	i := strings.Index(text, prefix)
	if i < 0 {
		return ""
	}
	rest := text[i+len(prefix):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// (b Block) SpawnedAtZero 占位——内联判断替代，见调用处。
func (b Block) SpawnedAtZero() bool { return b.SpawnedAt == 0 }

func millis(t time.Time) int64 {
	if t.IsZero() {
		return time.Now().UnixMilli()
	}
	return t.UnixMilli()
}

func nowMillis() int64 { return time.Now().UnixMilli() }

func msBetween(a, b time.Time) int64 {
	if a.IsZero() || b.IsZero() {
		return 0
	}
	d := b.Sub(a)
	if d < 0 {
		return 0
	}
	return d.Milliseconds()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func uitoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// ---------------------------------------------------------------------------
// clone helpers（深拷贝，Snapshot/Restore 用）
// ---------------------------------------------------------------------------

func cloneBlocks(in []Block) []Block {
	out := make([]Block, len(in))
	for i, b := range in {
		b.Contents = append([]Content{}, b.Contents...)
		b.Todos = append([]TodoItem{}, b.Todos...)
		// NestedCalls 同 Artifacts：切片必须显式复制（Snapshot 的深拷贝语义）
		b.NestedCalls = append([]NestedCall{}, b.NestedCalls...)
		b.Warnings = append([]string{}, b.Warnings...)
		b.Items = cloneAgentItems(b.Items)
		// Artifacts 是切片：值拷贝 out[i]=b 会共享底层数组，必须显式复制
		// （否则 Snapshot 的深拷贝语义被破坏——调用方 append 会写到共享数组）。
		b.Artifacts = append([]ArtifactRef{}, b.Artifacts...)
		if b.ErrorInfo != nil {
			ei := *b.ErrorInfo
			b.ErrorInfo = &ei
		}
		if b.Diff != nil {
			d := *b.Diff
			b.Diff = &d
		}
		out[i] = b
	}
	return out
}

func cloneAgentItems(in []AgentItem) []AgentItem {
	out := make([]AgentItem, len(in))
	for i, it := range in {
		it.NestedCalls = append([]NestedCall{}, it.NestedCalls...)
		if it.ErrorInfo != nil {
			ei := *it.ErrorInfo
			it.ErrorInfo = &ei
		}
		if it.Diff != nil {
			d := *it.Diff
			it.Diff = &d
		}
		if it.Usage != nil {
			u := *it.Usage
			it.Usage = &u
		}
		out[i] = it
	}
	return out
}

func cloneRunNodes(in []RunNode) []RunNode {
	return append([]RunNode{}, in...)
}

func cloneTurns(in []Turn) []Turn {
	return append([]Turn{}, in...)
}

func cloneTodos(in []TodoItem) []TodoItem {
	if in == nil {
		return []TodoItem{}
	}
	out := make([]TodoItem, len(in))
	for i, t := range in {
		t.BlockedBy = append([]string{}, t.BlockedBy...)
		out[i] = t
	}
	return out
}

func cloneDiffs(in []Diff) []Diff {
	return append([]Diff{}, in...)
}

// agentItemsLimit 是当前生效的 agent items 上限（默认 maxAgentItemsPerBlockDefault）。
// 提成变量是为了让 A/B 测试能切换它（见 checkpoint_items_trim_test.go）。
var agentItemsLimit = maxAgentItemsPerBlockDefault

// maxAgentItemsPerBlock 返回当前上限（供测试与诊断读取）。
func maxAgentItemsPerBlock() int { return agentItemsLimit }

// SetMaxAgentItemsForTest 临时修改上限（仅测试用）。
func SetMaxAgentItemsForTest(n int) { agentItemsLimit = n }

// ---------------------------------------------------------------------------
// 打开会话的「尾部块」投影
// ---------------------------------------------------------------------------

// PartialView 是打开会话时的轻量投影：只有最近的 blocks，加上「还有更早」的游标。
//
// ## 为什么需要（实测 2026-10-20）
//
// 打开真实长会话（22269 块）时 `reducer.Marshal()` 要 138ms 生成 67MB JSON，
// 而这 67MB 经 stdout 发给 UI、UI 再解析成 22269 个对象、全量渲染
// （103794 个 DOM 节点，布局 1704ms）。后端已经把「按需分页读取」做到
// 0.06ms/页，但打开路径仍一次吐全量 —— 分页能力完全没被用上。
//
// ## 为什么要嵌在完整 view 里，而不是新协议
//
// view_checkpoint 的**其余字段**（run_nodes/turns/usage/todos/...）UI 都要，
// 且体积很小。只有 blocks 是大头。所以这里仍是完整的一份 checkpoint JSON，
// 只把 blocks 数组**截成尾部若干块**，用两个额外字段说明截断情况：
//
//	blocks_partial: true        表示 blocks 不是全部
//	blocks_cursor: <最早块的 id> UI 用这个 id 向前翻（见 agent_history 先例）
//	blocks_total:  <总块数>     UI 显示「共 N 条」/ 判断是否还有更早
//
// 旧 UI 完全不受影响吗？**不完全**：它会把截断后的 blocks 当成全部。
// 因此调用方必须显式请求（limit>0），默认行为保持全量。
type PartialView struct {
	View        []byte
	Cursor      uint64 // 本次返回的最早块 id（请求更早时作为 BeforeID）
	Total       int    // 全部块数
	HasMore     bool   // 是否还有更早的块
	Returned    int    // 本次返回的块数
	MarshalCost int64  // 编码耗时（ns，诊断用）

	// Agents 是**落在窗口之外**的子智能体 / 后台任务卡片（见 detachAgentsLocked）。
	//
	// 为什么必须单独带回来（用户 2026-10 报障「刷新后左侧子智能体全没了」）：
	// 左栏的子智能体树是从 blocks 投影出来的（lib/agentTree.agentTreeFromView），
	// 而打开会话默认只回最近 200 块 —— 跑半天的会话里 agent 卡片早就在窗口之外，
	// 于是**每次刷新树都是空的**。run_nodes 救不了：新顶层 run 会把 runNodes
	// 重置成 {主 run}（见 applyAgentStartLocked），子 run 的历史只活在卡片里。
	Agents []Block
}

// MarshalTail 只序列化最后 limit 个 block，并在 JSON 里标注截断游标。
//
// 与 Marshal 的关系：骨架（除 blocks 外的全部字段）走同一条编码路径，
// 保证字段顺序与取值完全一致；只有 blocks 数组内容是尾部切片。
//
// limit <= 0 或块数不超过 limit 时等价于 Marshal（不标注 partial）。
func (r *ViewReducer) MarshalTail(limit int) (*PartialView, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 必须先取快照再切尾部：snapshotLocked 会调 flushChunkLocked，
	// 后者可能**追加一个新块**（流式 chunk 落成正式块）。若先按 r.blocks
	// 计数再取快照，total 会比投影里的实际块数少 1，且切出的尾部错位一格
	//（实测 2026-10-20：total=22341 而全量 21890，差 451 是因为调用方
	//  在两次 Marshal 之间还有别的变更并入 —— 但差 1 的切片错位更隐蔽）。
	//
	// 正确做法：以快照为唯一事实来源，total/tail/骨架全部来自同一份 cp.Blocks。
	cp := r.snapshotLocked()
	blocks := cp.Blocks
	total := len(blocks)
	if limit <= 0 || limit >= total {
		// 不截断：走全量编码（带逐块字节缓存）。**不能**在这里自己拼 JSON ——
		// 那会绕过 blockBytes 缓存，让每个会话每次打开都重新序列化全部块（小会话也变慢）。
		view, _, err := r.marshalLockedSpans()
		if err != nil {
			return nil, err
		}
		return &PartialView{View: view, Total: total, Returned: total}, nil
	}

	start := total - limit
	tail := blocks[start:]
	cursor := tail[0].ID

	// turns / diffs 同类裁剪：它们与 blocks 一样是「历史越长越大」的数组，
	// 而 UI 对 turns 只用到「总条数」与「最后两条」（见 Statusbar / Composer）。
	// 真实会话实测：turns 6.57MB、diffs 2.24MB —— 比 blocks 之外的全部字段都大，
	// 不裁的话「首屏 200 块」仍要从 9.17MB 里减，收益被吃掉七成。
	//
	// 裁的是**发给 UI 的这份投影**，不影响 r.turns 本身，也不影响持久化：
	// 提交路径走 marshalLockedSpans（全量），与这里互不影响。
	// turns_total 让 UI 显示真实回合数，避免「加载了多少就显示多少」的错数。
	turnsTotal := len(cp.Turns)
	if len(cp.Turns) > limit {
		cp.Turns = cp.Turns[len(cp.Turns)-limit:]
	}
	if len(cp.Diffs) > limit {
		cp.Diffs = cp.Diffs[len(cp.Diffs)-limit:]
	}
	// 窗口之外的子智能体/任务卡片：独立于 blocks 带回（见 PartialView.Agents 的注释）
	agents := detachAgents(blocks[:start])

	// 骨架：blocks 置 nil 后序列化，再注入尾部块 + 三个标注字段。
	cp.Blocks = nil
	head, err := json.Marshal(cp)
	if err != nil {
		return nil, err
	}
	const placeholder = `"blocks":null`
	idx := bytes.Index(head, []byte(placeholder))
	if idx < 0 {
		// 结构异常 → 退回全量（正确性优先）
		view, _, merr := r.marshalLockedSpans()
		if merr != nil {
			return nil, merr
		}
		return &PartialView{View: view, Total: total, Returned: total}, nil
	}

	if r.blockBytes == nil {
		r.blockBytes = make(map[uint64][]byte, len(blocks))
	}
	parts := make([][]byte, len(tail))
	totalBytes := 0
	for i := range tail {
		id := tail[i].ID
		if b, ok := r.blockBytes[id]; ok {
			parts[i] = b
			totalBytes += len(b) + 1
			continue
		}
		b, err := json.Marshal(&tail[i])
		if err != nil {
			return nil, err
		}
		parts[i] = b
		r.blockBytes[id] = b
		totalBytes += len(b) + 1
	}

	var buf bytes.Buffer
	buf.Grow(len(head) + totalBytes + 128)
	buf.Write(head[:idx])
	buf.WriteString(`"blocks":`)
	buf.WriteByte('[')
	for i, p := range parts {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(p)
	}
	buf.WriteByte(']')
	// 在 blocks 之后、其余字段之前插入标注（顺序无关，UI 按 key 读）
	buf.WriteString(`,"blocks_partial":true,"blocks_cursor":`)
	buf.WriteString(strconv.FormatUint(cursor, 10))
	buf.WriteString(`,"blocks_total":`)
	buf.WriteString(strconv.Itoa(total))
	buf.WriteString(`,"turns_total":`)
	buf.WriteString(strconv.Itoa(turnsTotal))
	buf.Write(head[idx+len(placeholder):])
	view := buf.Bytes()

	// 缓存：下一轮增量 Marshal 仍可复用这些块字节（不必重排）
	for _, b := range tail {
		if _, ok := r.blockBytes[b.ID]; !ok {
			if enc, e := json.Marshal(&b); e == nil {
				r.blockBytes[b.ID] = enc
			}
		}
	}
	return &PartialView{
		View:     view,
		Cursor:   cursor,
		Total:    total,
		HasMore:  start > 0,
		Returned: len(tail),
		Agents:   agents,
	}, nil
}

// detachAgents 从窗口外的块里挑出「树要用的卡片」，并把重内容换成可回落的分页句柄。
//
// 两类卡片，处理方式不同：
//
//   - **有 task_id**（后台子智能体 / 后台工具任务）：items 是完整转录，实测单卡
//     100–185KB。这里只发骨架 —— UI 打开它时按 task_id 从子任务 journal 取首屏
//     （agent_history，offset=0 就取最新一页）。裁掉的条数记进 ItemsTrimmed，
//     否则 UI 会以为「这条卡片本来就没有记录」而不显示加载入口。
//   - **没有 task_id**（同步子智能体，in-loop）：没有 journal 可取，checkpoint 里的
//     items 是唯一副本 → 整份带上，宁可多传也不能让它变成空白页。
//
// 只挑树投影认的那三种：agent / async_task / 被摘离的（promoted）工具任务。
// 其余块留在窗口以外由「加载更早」逐页取回，不进这个数组。
func detachAgents(out []Block) []Block {
	var agents []Block
	for _, b := range out {
		switch {
		case b.Kind == blockKindAgent, b.Kind == blockKindAsyncTask:
		case b.Kind == blockKindTool && b.Promoted && b.TaskID != "":
		default:
			continue
		}
		if b.TaskID == "" || len(b.Items) == 0 {
			agents = append(agents, b)
			continue
		}
		slim := b // 值拷贝：Items 切片头置空不影响 r.blocks 本体
		slim.ItemsTrimmed += len(slim.Items)
		slim.Items = nil
		agents = append(agents, slim)
	}
	return agents
}

// ReadBlockPage 按游标返回一页 block 的 JSON 字节（供 UI「加载更早」）。
//
// 与 MarshalTail 共用同一套块字节缓存，因此 UI 首次打开后再翻页不会
// 重复序列化同一批块。
func (r *ViewReducer) ReadBlockPage(beforeID uint64, limit int) (blocks []json.RawMessage, nextCursor uint64, hasMore bool, total int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 必须基于**归一化快照**而不是 r.blocks：
	// snapshotLocked 会丢弃空壳 assistant 块（llm_start 无 llm_end，无正文/无思考、
	// 未结算 —— 见 normalizeDanglingState）。真实长会话里这类块有 451 个（22341→21890），
	// 若这里用 r.blocks，分页会返回 Marshal 里根本不存在的块，UI 出现「幽灵消息」。
	//
	// 代价：每页复制一次 blocks 切片（浅拷贝切片头 + 结构体数组）。21890 个 Block
	// 结构体的拷贝是 ~10MB memcpy（实测数 ms），相对「返回错误数据」完全可接受。
	// 只有真的翻页才走这里，打开热路径（MarshalTail）不受影响。
	snap := r.snapshotLocked()
	view := snap.Blocks
	total = len(view)
	if limit <= 0 {
		limit = 50
	}
	// 定位 beforeID 之前的最后一页；beforeID==0 表示「最新一页」。
	end := total
	if beforeID > 0 {
		// 用二分：blocks 按 id 严格递增（blockSeq 单调分配），
		// 线性扫描在长会话里是 21890 次比较/页，没有必要。
		end = sort.Search(total, func(i int) bool { return view[i].ID >= beforeID })
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	if end <= 0 {
		return nil, 0, false, total, nil
	}
	page := view[start:end]
	blocks = make([]json.RawMessage, 0, len(page))
	if r.blockBytes == nil {
		r.blockBytes = make(map[uint64][]byte, total)
	}
	for i := range page {
		id := page[i].ID
		b, ok := r.blockBytes[id]
		if !ok {
			b, err = json.Marshal(&page[i])
			if err != nil {
				return nil, 0, false, total, err
			}
			r.blockBytes[id] = b
		}
		blocks = append(blocks, json.RawMessage(b))
	}
	nextCursor = page[0].ID
	hasMore = start > 0
	return blocks, nextCursor, hasMore, total, nil
}

// BlockCount 返回当前 block 总数（UI 显示「共 N 条」/ 判断是否还有更早）。
func (r *ViewReducer) BlockCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.blocks)
}
