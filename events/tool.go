package events

import (
	"context"
	"errors"
	"github.com/seven7628/hai-harness/core"
	"time"
)

// Approver 工具执行前的人工确认接口（HITL）。
// 由运行层（RunOptions.Approver）注入工具上下文，engine 在工具轮执行前
// 统一收集需要审批的调用并逐个请求确认 —— 决策锚 = ToolCall.Id
// （调用级唯一；RequestId 是请求级，同轮多调用共享，不能做决策锚）。
//
// 注册先行语义：BeginApproval 先注册决策通道再发事件 —— 事件消费者收到
// ToolApprovalRequested 即可注入决策（Session.Approve），无「事件已发但
// 决策未注册」的竞态窗口。
type Approver interface {
	// BeginApproval 注册单个调用的决策通道并返回等待函数
	// （阻塞直到用户决策 / 超时（=拒绝）/ ctx 取消）。
	BeginApproval(call core.ToolCall) (wait func(ctx context.Context) (bool, error))
}

// ContextualApprover is an optional extension of Approver for implementations that
// need the run context while registering a request. The engine uses it when
// available; older Approver implementations keep the original API semantics.
type ContextualApprover interface {
	Approver
	BeginApprovalWithContext(ctx context.Context, call core.ToolCall) (wait func(context.Context) (bool, error), err error)
}

// 引擎 preApprove 用：实现方（如模式包装层）可覆盖「工具自声明 RequiresApproval」的现状路径——
// 使模式策略能强制审批不自声明的工具（如 manual 模式对 write/edit），或豁免自声明的工具
// （如 AllPass 对 bash）。未实现 = 回退工具自声明（现状）。
type ApprovalPolicy interface {
	// NeedsApproval 返回 true = 调用需经审批通道（引擎发起 ToolApprovalRequested 并经 BeginApproval 决策）；
	// false = 不发起审批（直接执行）。
	NeedsApproval(call core.ToolCall) bool
}

// ErrApprovalTimeout 审批等待窗口用尽（用户既没批准也没拒绝）。session 的审批等待器在
// 超时时返回它，engine 据此与「用户拒绝」区分措辞 —— 否则模型会把「人还没来得及看」
// 当成「被否决」（与 ask_user 超时同一条原则：超时后必须让 LLM 在上下文里知道「用户没填」）。
var ErrApprovalTimeout = errors.New("approval timed out with no decision from the user")

// ToolApprovalRequested 工具审批请求事件（UI 展示「谁要执行什么」，
// 用户经 Session.Approve(id, decision) 注入决策）。
type ToolApprovalRequested struct {
	RunId     string    `json:"run_id"`               // 所属运行（事件自包含）
	RequestId string    `json:"request_id,omitempty"` // 关联请求（审计用，非决策锚）
	Id        string    `json:"id"`                   // 调用 id（决策锚：Session.Approve 按此匹配）
	Index     int64     `json:"index"`
	Name      string    `json:"name"`
	Arguments string    `json:"arguments"`
	Timestamp time.Time `json:"timestamp"`

	EventType EventType `json:"event_type"`
}

func (a *ToolApprovalRequested) Type() EventType { return a.EventType }

// ToolStart 工具执行开始。
type ToolStart struct {
	RequestId string    `json:"request_id,omitempty"` // 发起该调用的请求（事件自包含）
	Index     int64     `json:"index"`
	Id        string    `json:"id"`
	Name      string    `json:"name"`
	RunId     string    `json:"run_id,omitempty"` // 所属运行（事件自包含：主/子 agent 并发工具归属）
	Timestamp time.Time `json:"timestamp"`        // 开始时间（与 ToolResponse.Timestamp 差即工具耗时）

	// Arguments 实际执行的参数 JSON（UI 展示「传了什么」；PreToolUse 改写后，
	// 与 ToolResponse.Arguments 同语义）。历史 ToolCall 保留原参（审计）。
	Arguments string `json:"arguments,omitempty"`

	// TaskId 本次调用对应的工具任务 id（tooltask-<seq>）：前端据此把工具行关联到后台任务
	//（promote 时经 task_id 定位；占位结果 {task_id,status:"running"} 回写历史同源）。
	TaskId string `json:"task_id,omitempty"`

	EventType EventType `json:"event_type"`
}

func (l *ToolStart) Type() EventType {
	return l.EventType
}

// TaskPromoted 运行中的工具/同步子 agent 已被摘离为后台任务（promote 成功时发出）。
// 前端据此把该工具行/子 agent 卡切换为"后台任务"态，并展示"移到后台"已生效。
// SessionId 由宿主注入（SDK 事件自包含 run_id，SessionId 属宿主路由键——bridge emit 时补写）。
type TaskPromoted struct {
	SessionId string    `json:"session_id"`
	TaskId    string    `json:"task_id"`
	Kind      string    `json:"kind"` // "tool" | "subagent"（同步子 agent 摘离也走引擎层 = "tool"）
	Name      string    `json:"name"` // 工具名
	Timestamp time.Time `json:"timestamp"`

	EventType EventType `json:"event_type"`
}

func (t *TaskPromoted) Type() EventType {
	return t.EventType
}

// ToolResponse 工具执行结果（含错误）。
type ToolResponse struct {
	RequestId string `json:"request_id,omitempty"`
	Index     int64  `json:"index"`
	Id        string `json:"id"`
	Name      string `json:"name"`
	RunId     string `json:"run_id,omitempty"` // 所属运行（事件自包含：主/子 agent 并发工具归属）
	// Arguments 实际执行的参数 JSON（PreToolUse 改写后；历史 ToolCall 保留原参——
	// 审计「实际执行了什么」经此字段，见 agents/hooks.go）
	Arguments  string    `json:"arguments,omitempty"`
	Result     string    `json:"result"`
	IsError    bool      `json:"is_error"`
	ErrorCode  string    `json:"error_code,omitempty"`
	Changed    string    `json:"changed,omitempty"`
	Retryable  bool      `json:"retryable,omitempty"`
	NextAction string    `json:"next_action,omitempty"`
	Recovery   string    `json:"recovery,omitempty"`
	Timestamp  time.Time `json:"timestamp"` // 结束时间

	// Diff 本次执行对文件的变更（**实现了 ToolDiffProvider 的工具**产出：write/edit 类，
	// 以及声明了 outputs 的 run_python；事件层携带供宿主渲染 +/- 变更，不进入 LLM 上下文）。
	// 信息源头原则：工具持有旧/新内容，见 core.FileDiff。
	Diff *core.FileDiff `json:"diff,omitempty"`

	// Images 本次结果携带的图片内容块（read_file 读图 / 截图 / MCP image 内容；
	// ToolImageProvider 产出）。双通道语义与 Diff 不同：**会进 LLM 上下文**
	//（AgentLoop 构造 tool 消息时并入 Blocks），此处随事件发给宿主只为
	// UI 直显（data URL 当 <img src>）与恢复重放——宿主可忽略不渲染。
	Images []core.Content `json:"images,omitempty"`

	// Usage 本次执行的用量（子 agent 类工具回传子运行用量，含成本）。
	// 后台任务（promoted 工具 / 异步子 agent）另经 TaskEnd.Usage 与
	// TaskResultDelivered.Usage 回传——同源 Usage，宿主可按可用通道取其一。
	Usage *core.Usage `json:"usage,omitempty"`

	// Exec 命令类工具（bash 等）的执行结局（退出码/超时/取消）。存在意义：把「失败/
	// 超时/取消」从结果文本升级为结构化信号（UI 红色标记 / 指标聚合），Result 仍保留完整输出。
	Exec *core.ExecStatus `json:"exec,omitempty"`

	EventType EventType `json:"event_type"`
}

func (l *ToolResponse) Type() EventType {
	return l.EventType
}
