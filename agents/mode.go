package agents

import (
	"context"
	"github.com/seven7628/hai-harness/agents/prompt"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/tools"
)

// ApprovalDecision 模式审批决策（Mode.Approve 的输出）。
// Auto/Block 为同步决策：不触发 ToolApprovalRequested 事件、不等待用户
// （engine 在工具轮执行前统一收集，同步决策零等待）；Required 转交底层
// Approver（现状 HITL 流程）。
type ApprovalDecision int

const (
	// ApprovalRequired 人工审批（现状：转交注入的 Approver，超时 = 拒绝）
	ApprovalRequired ApprovalDecision = iota
	// ApprovalAuto 自动放行（all-pass / plan 模式的只读工具）
	ApprovalAuto
	// ApprovalBlock 直接拒绝（调用以拒绝结果回传 LLM）
	ApprovalBlock
)

// Mode 一次 Run 的执行模式 = 三个正交维度的组合：
//
//	A 工具可见性 AllowTool —— schema 过滤 + 执行前防御（同一份规则）
//	B 审批策略   Approve   —— nil = 工具自声明现状；否则按调用决策
//	C 行为契约   SystemHint —— 拼在 system 最尾部（稳定前缀在前，切换不破坏缓存）
//
// Mode 是 Run 级参数（RunOptions.Mode）：AgentLoop 是唯一翻译点，
// ToolEngine/HITL 无感知（只见过滤后的 schema 与包装后的 Approver）。
// Session 持有会话当前模式状态（SetMode，快照注入下一次 Run，不持久化）。
type Mode struct {
	// Name 模式名（"plan" / "normal" / "all-pass" 或产品自定义）
	Name string

	// AllowTool 工具可见性：返回 false 的工具不注入 schema（LLM 不可见），
	// 且执行前防御拒绝（LLM 硬调 → IsError 回传，不走审批 —— plan 语义是禁止）。
	// nil = 全部可见。
	AllowTool func(t tools.Tool) bool

	// Approve 审批决策：每次运行快照时按调用判定。nil = 现状
	// （工具自声明 ApprovalRequired + 注入的 Approver）。
	Approve func(call core.ToolCall) ApprovalDecision

	// SystemHint 行为契约片段：拼在 system 最尾部（Agent.md/技能之后），
	// 参与内容比较替换；"" = 不注入。模式切换只影响此处 + 修改工具桶，
	// 稳定前缀（base/产品层/Agent.md/只读 schema）保持缓存命中。
	SystemHint string
}

// PlanSafeTool 可选接口：工具声明"plan 模式下可见且安全"。
// 语义：委派只读子 agent（subagent.Spec.PlanSafe）本身是只读动作 ——
// 子 agent 工具默认非只读（子可能改文件），仅显式声明 PlanSafe 的
// 才在 plan 模式可见，且其子 loop 必须由产品保证只读。
type PlanSafeTool interface {
	PlanSafe() bool
}

// isReadOnly 只读判定（tools.ReadOnlyTool 可选接口，未实现 = 非只读，保守）。
func isReadOnly(t tools.Tool) bool {
	ro, ok := t.(tools.ReadOnlyTool)
	return ok && ro.ReadOnly()
}

// isPlanSafeSubAgent plan 安全子 agent 判定（subagent.Spec.PlanSafe 声明）。
func isPlanSafeSubAgent(t tools.Tool) bool {
	ps, ok := t.(PlanSafeTool)
	return ok && ps.PlanSafe()
}

// modeApprover 按 Mode.Approve 包装 Approver 的策略层：
// Auto = 同步放行；Block = 同步拒绝；Required = 转交底层 inner（可能为 nil）。
type modeApprover struct {
	inner events.Approver
	mode  *Mode
}

func (m modeApprover) BeginApproval(call core.ToolCall) func(ctx context.Context) (bool, error) {
	switch m.mode.Approve(call) {
	case ApprovalAuto:
		return func(context.Context) (bool, error) { return true, nil }
	case ApprovalBlock:
		return func(context.Context) (bool, error) { return false, nil }
	}
	if m.inner == nil {
		return nil // 未注入底层审批：现状不审批
	}
	return m.inner.BeginApproval(call)
}

func (m modeApprover) BeginApprovalWithContext(ctx context.Context, call core.ToolCall) (func(context.Context) (bool, error), error) {
	switch m.mode.Approve(call) {
	case ApprovalAuto:
		return func(context.Context) (bool, error) { return true, nil }, nil
	case ApprovalBlock:
		return func(context.Context) (bool, error) { return false, nil }, nil
	}
	if m.inner == nil {
		return nil, nil // 未注入底层审批：现状不审批
	}
	if ca, ok := m.inner.(events.ContextualApprover); ok {
		return ca.BeginApprovalWithContext(ctx, call)
	}
	return m.inner.BeginApproval(call), nil
}

// NeedsApproval 实现 events.ApprovalPolicy：模式策略对调用是否需经审批通道的同步判定。
// Auto → 无需审批（同步放行，不发起事件）；Block/Required → 经审批通道
// （Block 同步拒绝 / Required 转人工）。引擎据此把模式要强制的工具（如 manual 对
// write/edit）纳入审批，即使该工具不自声明 RequiresApproval。
func (m modeApprover) NeedsApproval(call core.ToolCall) bool {
	return m.mode.Approve(call) != ApprovalAuto
}

// WrapApprover 按模式包装人工审批（RunOptions.Approver 注入处的边界翻译）：
// mode 为 nil 或 Approve 为 nil 时原样返回 inner（现状语义，零开销）。
// Session 组装与 subagent 组装共用 —— 保证子 agent 指定 Mode 时 B 维度同样生效。
func WrapApprover(inner events.Approver, mode *Mode) events.Approver {
	if mode == nil || mode.Approve == nil {
		return inner
	}
	return modeApprover{inner: inner, mode: mode}
}

// PlanMode 预置：只读规划。只读工具 + plan 安全子 agent 可见（Auto 放行），
// 修改工具不可见（执行防御拒绝），SystemHint 指导输出计划。
// 行为约束靠机制（工具不可见 = 无法调用），Hint 仅提升计划输出质量，可改可关。
func PlanMode() *Mode {
	return &Mode{
		Name: "plan",
		AllowTool: func(t tools.Tool) bool {
			return isReadOnly(t) || isPlanSafeSubAgent(t)
		},
		Approve:    func(core.ToolCall) ApprovalDecision { return ApprovalAuto },
		SystemHint: prompt.PlanModeHint,
	}
}

// NormalMode 预置：全工具 + 现状审批（工具自声明 —— write/edit 自动、bash 人工）。
// 等价于"显式化的现状"：Approve nil = 走注入的 Approver 原逻辑。
func NormalMode() *Mode {
	return &Mode{
		Name: "normal",
	}
}

// AllPassMode 预置：全工具 + 全部自动放行（零审批事件零等待）。
// 注意：只关闭"人工审批"，工具自身的安全拦截（如 bash 高危命令检查）不被绕过。
func AllPassMode() *Mode {
	return &Mode{
		Name:    "all-pass",
		Approve: func(core.ToolCall) ApprovalDecision { return ApprovalAuto },
	}
}
