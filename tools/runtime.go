package tools

import (
	"context"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"time"
)

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
