package tools

import (
	"context"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/workdir"
	"time"
)

// ContextDescriber 允许工具按**调用方上下文**给出描述。
//
// 用途：bash 工具的描述里要输出**该调用方当前的工作目录**
// —— 主 agent 与子 agent 各有自己的 cwd 分区 ⇒ 静态 Description() 给不出正确的值。
//
// 引擎侧：ToolParams(ctx) 优先用 DescribeFor(ctx)，没有则退回 Description()。
// （schema 每请求重建 ⇒ 描述里的 cwd 每次都是当前值。）
type ContextDescriber interface {
	DescribeFor(ctx context.Context) string
}

// WorkdirAware 工具可暴露其**会话级 cwd 状态**。
//
// 用途：装配层要能证明「同一引擎里的 bash / 文件工具 / run_python 用的是**同一个**
// workdir.State」—— 否则它们的「当前目录」可能各走各的（静默错位，且行为层测不出来：
// 每个工具单独看都"对"）。
//
// ⚠️ 这是**装配断言**用的接口，不是业务接口：业务上工具之间不靠它通信
// （通信走 ctx 上的分区键，见 workdir.KeyFrom）。
type WorkdirAware interface {
	WorkdirState() *workdir.State
}

type Tool interface {
	Name() string
	Description() string
	Parameters() any
	CanParallel() bool

	ValidParams(ctx context.Context, name, arguments string) error
	BeforeCall(ctx context.Context, toolCall core.ToolCall) BeforeToolCallResponse
	Call(ctx context.Context, name, arguments string) (string, error)
	AfterCall(ctx context.Context, toolCall core.ToolCall)
}

type ToolEngine interface {
	// Sequence 顺序执行工具调用，返回与输入对齐的结果。
	Sequence(ctx context.Context, calls []core.ToolCall, eventHandler events.EventHandler) ([]core.ToolResult, error)
	// Parallel 并行执行工具调用（受并发上限约束），返回与输入对齐的结果。
	Parallel(ctx context.Context, calls []core.ToolCall, eventHandler events.EventHandler) ([]core.ToolResult, error)
	// RunBatch 批执行（agents.runTools 走此）：支持运行时 promote 摘离为后台任务。
	// batch 为批控制器（Session.PromoteTask 经此摘离运行中工具；nil = 不支持 promote，
	// 语义同 Sequence/Parallel）；promoteAfter > 0 = 自动摘离阈值（默认关）。
	RunBatch(ctx context.Context, calls []core.ToolCall, eventHandler events.EventHandler,
		decisions map[string]bool, batch *BatchController, promoteAfter time.Duration) ([]core.ToolResult, error)

	// ToolParams 返回所有已注册工具的厂商无关 schema。
	ToolParams(ctx context.Context) ([]core.ToolSchema, error)

	// RegisterTool 工具注册/发现。
	RegisterTool(ctx context.Context, tool Tool)
	// GetTool 按名称获取工具。
	GetTool(ctx context.Context, toolName string) (Tool, error)
}
