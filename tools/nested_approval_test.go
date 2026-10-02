package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// approvalTool 声明自己需要审批的工具（复刻 tools.ApprovalRequired 范式）。
type approvalTool struct {
	stubTool
}

func (a *approvalTool) RequiresApproval(context.Context, core.ToolCall) bool { return true }

// TestExecuteOneRespectsApproval —— 安全回归：嵌套调用**必须**走审批。
//
// execute() 只**消费** decisions 表，不发起审批；审批发生在 preApprove，
// 而 preApprove 只被 Sequence / Parallel / runBatch 调用。
// 若 ExecuteOne 直接传 decisions=nil，则声明了 ApprovalRequired 的工具
// 会在**完全没有审批**的情况下执行 —— 这是一条真实的越权路径。
// 对齐 pi：嵌套调用走完整工具管线，权限门对嵌套逐次生效
// （docs/extensions.md:148 nestedCalls 走 tool_call / tool_result handlers）。
func TestExecuteOneRespectsApproval(t *testing.T) {
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &approvalTool{
		stubTool{BaseTool: BaseTool{Name_: "danger", Description_: "danger", Params_: map[string]any{}},
			result: "SHOULD-NOT-RUN",
		},
	})

	// 注入一个「拒绝一切」的审批器。
	ctx := events.WithToolContext(context.Background(), "run1", 0, nil, denyApprover{}, nil)

	r := e.ExecuteOne(ctx, core.ToolCall{Id: "n1", Name: "danger", Arguments: "{}"}, ExecuteOpts{Depth: 1})

	if !r.IsError {
		t.Fatalf("nested approval-requiring tool executed without approval: result=%q", r.Result)
	}
	if r.Result == "SHOULD-NOT-RUN" {
		t.Fatal("approval-denied tool actually ran — approval was bypassed")
	}
	if !strings.Contains(r.Result, "rejected") {
		t.Fatalf("Result = %q, want a rejection message", r.Result)
	}
}

// TestExecuteOneApprovalGrantedRuns 审批通过时应当正常执行（别把审批变成永久拒绝）。
func TestExecuteOneApprovalGrantedRuns(t *testing.T) {
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &approvalTool{
		stubTool{BaseTool: BaseTool{Name_: "danger2", Description_: "danger2", Params_: map[string]any{}},
			result: "RAN-OK",
		},
	})
	ctx := events.WithToolContext(context.Background(), "run1", 0, nil, allowApprover{}, nil)

	r := e.ExecuteOne(ctx, core.ToolCall{Id: "n1", Name: "danger2", Arguments: "{}"}, ExecuteOpts{Depth: 1})
	if r.IsError {
		t.Fatalf("approved call failed: %q", r.Result)
	}
	if r.Result != "RAN-OK" {
		t.Fatalf("Result = %q, want RAN-OK", r.Result)
	}
}

// TestExecuteOneApprovalTimeoutIsNotRejection 超时未决必须与「用户否决」措辞不同
// （沿用引擎既有语义：模型需要知道「还在等人」而非「被拒了」）。
func TestExecuteOneApprovalTimeoutIsNotRejection(t *testing.T) {
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &approvalTool{
		stubTool{BaseTool: BaseTool{Name_: "danger3", Description_: "d", Params_: map[string]any{}},
			result: "RAN",
		},
	})
	// ContextualApprover 立即返回 ErrApprovalTimeout。
	ctx := events.WithToolContext(context.Background(), "run1", 0, nil,
		timeoutApprover{}, nil)

	r := e.ExecuteOne(ctx, core.ToolCall{Id: "n1", Name: "danger3", Arguments: "{}"}, ExecuteOpts{Depth: 1})
	if !r.IsError {
		t.Fatal("timed-out approval should not run the tool")
	}
	if strings.Contains(r.Result, "user rejected") {
		t.Fatalf("timeout misreported as user rejection: %q", r.Result)
	}
	if !strings.Contains(r.Result, "timed out") {
		t.Fatalf("Result = %q, want a timeout message", r.Result)
	}
}

// denyApprover 拒绝一切审批。
type denyApprover struct{}

func (denyApprover) BeginApproval(core.ToolCall) func(context.Context) (bool, error) {
	return func(context.Context) (bool, error) { return false, nil }
}

// allowApprover 批准一切审批。
type allowApprover struct{}

func (allowApprover) BeginApproval(core.ToolCall) func(context.Context) (bool, error) {
	return func(context.Context) (bool, error) { return true, nil }
}

// timeoutApprover 等待窗口用尽（既没批准也没拒绝）。
type timeoutApprover struct{}

func (timeoutApprover) BeginApproval(core.ToolCall) func(context.Context) (bool, error) {
	return func(context.Context) (bool, error) { return false, events.ErrApprovalTimeout }
}

var _ = time.Second
