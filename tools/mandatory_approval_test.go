package tools

// mandatory_approval_test.go —— 「宿主级强制审批」不被模式策略覆盖的回归（P2c）。
//
// 背景：Auto/Full Access 的 events.ApprovalPolicy 会把未知工具判成 Auto。插件写工具
// 是第三方代码 + 不可逆副作用，必须在策略层**之前**钉死；否则用户选了「自动执行」，
// 插件 write 工具就会静默落盘。只读工具不实现 MandatoryApproval，不弹卡。

import (
	"context"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

type mandatoryTestTool struct{ stubTool }

func (t *mandatoryTestTool) MandatoryApproval(context.Context, core.ToolCall) bool { return true }

// autoDenyPolicy 复刻 Auto 模式：NeedsApproval=false，但一旦被请求就拒绝并计数。
type autoDenyPolicy struct {
	began int
}

func (a *autoDenyPolicy) NeedsApproval(core.ToolCall) bool { return false }

func (a *autoDenyPolicy) BeginApproval(core.ToolCall) func(context.Context) (bool, error) {
	a.began++
	return func(context.Context) (bool, error) { return false, nil }
}

func TestMandatoryApprovalOverridesAutoPolicy(t *testing.T) {
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &mandatoryTestTool{stubTool{
		BaseTool: BaseTool{Name_: "plugin_write", Description_: "d", Params_: map[string]any{}},
		result:   "SHOULD-NOT-RUN",
	}})
	ap := &autoDenyPolicy{}
	ctx := events.WithToolContext(context.Background(), "r1", 0, nil, ap, nil)

	res, err := e.Sequence(ctx, []core.ToolCall{{Id: "c1", Name: "plugin_write", Arguments: "{}"}}, nil)
	if err != nil {
		t.Fatalf("Sequence: %v", err)
	}
	if ap.began != 1 {
		t.Fatalf("Auto 策略把强制审批覆盖掉了：BeginApproval 调用 %d 次，want 1", ap.began)
	}
	if len(res) != 1 || !res[0].IsError || res[0].Result == "SHOULD-NOT-RUN" {
		t.Fatalf("被拒的强制审批工具却执行了：%+v", res)
	}
}

func TestPlainToolStillHonorsAutoPolicy(t *testing.T) {
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &stubTool{
		BaseTool: BaseTool{Name_: "plain", Description_: "d", Params_: map[string]any{}},
		result:   "RAN",
	})
	ap := &autoDenyPolicy{}
	ctx := events.WithToolContext(context.Background(), "r2", 0, nil, ap, nil)

	res, err := e.Sequence(ctx, []core.ToolCall{{Id: "c1", Name: "plain", Arguments: "{}"}}, nil)
	if err != nil {
		t.Fatalf("Sequence: %v", err)
	}
	if ap.began != 0 {
		t.Fatalf("普通工具在 Auto 模式下不应发起审批：BeginApproval=%d", ap.began)
	}
	if len(res) != 1 || res[0].IsError || res[0].Result != "RAN" {
		t.Fatalf("普通工具被误拦：%+v", res)
	}
}
