package tools

import (
	"context"
	"github.com/seven7628/hai-harness/core"
	"sync"
	"time"
)

// BeforeToolCallResponse BeforeCall 的返回：Block=true 时拦截本次执行（如用户确认场景）。
type BeforeToolCallResponse struct {
	Block     bool   `json:"block"`
	Reasoning string `json:"reasoning"`
}

// ToolUsageProvider 可选接口：工具执行后可报告本次用量（子 agent 类工具的成本传导，
// 引擎把它填进 ToolResult 与 ToolResponse 事件，由 AgentLoop 累加到父运行结算）。
type ToolUsageProvider interface {
	Usage() core.Usage
}

// ReadOnlyTool 可选接口：工具声明自己"只读"——不修改用户工作区/外部系统
// （无不可逆外部副作用）。会话状态不算外部副作用（todo_*/plan_submit 可标只读，
// 规划模式仍要记 TODO、提交计划）。
// 不实现 = 默认非只读（plan 模式不可见，保守安全）。
// 消费方：Mode 的工具可见性过滤（PlanMode.AllowTool）与 schema 只读桶分桶
// （只读在前 = 模式切换时前缀缓存命中）。
type ReadOnlyTool interface {
	ReadOnly() bool
}

// ApprovalRequired 可选接口：工具声明哪些调用需要人工确认（HITL）。
// engine 在工具轮执行前统一收集需要审批的调用并逐个请求确认
// （Approver 经 events.ToolContext 注入；决策锚 = ToolCall.Id，
// 拒绝/超时的调用 Block 结果回传 LLM —— 模型可见后自行调整）。
type ApprovalRequired interface {
	RequiresApproval(ctx context.Context, call core.ToolCall) bool
}

// ToolTimeoutProvider 可选接口：工具声明自己的单次执行时限。
// 方法名为 ToolTimeout 而非 Timeout —— 避免与工具常见字段（BashTool.Timeout
// 等）同名（Go 不允许字段与方法同名），这是设计层面的约定。
// 语义（引擎 execute 统一 WithTimeout 包裹 Call，防工具挂死整个 agent）：
//   - 返回 > 0 覆盖引擎默认值（信任工具作者，如 shell 的命令时限）；
//   - 返回 0（或未实现）使用引擎默认值（WithToolTimeout，默认 30s）；
//   - 返回 < 0 显式不受限 —— 仅限生命周期自管理的工具（如子 agent，
//     由 abort/ctx 级联控制，父引擎默认超时会误杀长时子运行）。
type ToolTimeoutProvider interface {
	ToolTimeout() time.Duration
}

// PerCallTimeout 可选接口：工具按本次调用参数返回期望时限（0 = 用工具级默认，如 bash
// 允许模型经 timeout 参数按任务难易自设）。与 ToolTimeoutProvider（工具级固定值）叠加：
// 引擎先取工具级声明，PerCallTimeout 返回非 0 时覆盖之。该时限落在可摘离的
// liftableTimeout 上——任务 promote 为后台时被 Lift 解除（后台任务不限时）。
type PerCallTimeout interface {
	TimeoutForArgs(arguments string) time.Duration
}

// ToolDiffProvider 可选接口：工具执行后可报告本次调用对文件的变更（diff）。
// engine 在 Call 成功后检测接口，把 Diff 填进 ToolResponse.Diff（事件层，宿主 UI
// 渲染 +/- 变更，不进入 LLM 上下文）。
// 信息源头原则：write/edit 类工具执行时唯一同时持有旧/新内容，是能精确产出 diff
// 的点；宿主无法从事件推导（ToolResponse 不带旧内容）。同 ToolUsageProvider 先例
// （可选接口 + Call 内暂存 + engine 成功后读取），并行调用同工具的竞态顾虑一致。
type ToolDiffProvider interface {
	Diff() *core.FileDiff
}

// ImageSink 单次工具调用的图片收集槽（**每次调用独立**，见 WithImageSink）。
//
// 为什么不用「工具上的暂存字段 + Images() 接口」：引擎在 Call 返回后读取图片，
// 而工具实例在注册表里是共享的 —— 并行批里另一个 goroutine 可能已经进入同一个
// 工具的 Call（清空暂存）并在引擎读取之前写入自己的图，导致**张冠李戴**：
// 模型看到的是另一次调用的截图。并行 read_file 读多图、并行 MCP 截图都会命中
// 这个窗口（Go 1.14+ 异步抢占，同步代码段之间也可能被换出）。
// 把收集槽放进 ctx（每次调用新建）→ 按构造即无竞态，且保留 CanParallel 语义。
type ImageSink struct {
	mu   sync.Mutex
	imgs []core.Content
}

// Add 收集图片块（工具在 Call 内调用；并发安全）。
func (s *ImageSink) Add(imgs ...core.Content) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range imgs {
		if c.Type == core.ContentTypeImage && c.Content != "" {
			s.imgs = append(s.imgs, c)
		}
	}
}

// Images 已收集的图片（engine 在 Call 返回后读取）。
func (s *ImageSink) Images() []core.Content {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.imgs
}

type imageSinkKey struct{}

// WithImageSink 为一次工具调用注入图片收集槽（engine 在 Call 前调用）。
func WithImageSink(ctx context.Context, s *ImageSink) context.Context {
	return context.WithValue(ctx, imageSinkKey{}, s)
}

// ImageSinkFrom 取出本次调用的图片收集槽；未注入（工具被直接调用/单测）返回 nil。
// 工具侧用法：
//
//	if sink := tools.ImageSinkFrom(ctx); sink != nil {
//	    sink.Add(core.Content{Type: core.ContentTypeImage, Content: dataURL, MimeType: mime})
//	}
//
// nil 安全（Add/Images 都是 nil-receiver 安全）——工具不必判空即可调用。
func ImageSinkFrom(ctx context.Context) *ImageSink {
	s, _ := ctx.Value(imageSinkKey{}).(*ImageSink)
	return s
}
