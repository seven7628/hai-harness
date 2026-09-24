package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/seven7628/hai-harness/tools"
)

// NewPlanSubmitTool 创建 plan 模式提交工具（注册进引擎后，仅 plan 模式可见——
// 只读声明 + PlanMode 白名单）。
//
// 协议：plan 模式运行中，模型把完整计划作为 plan 参数调用本工具 → 工具把计划
// 原样返回为结果文本（进上下文，模型可见"已提交"）→ 宿主（产品 UI）监听
// ToolRunEnd 事件拿到计划内容 → 用户确认 → 宿主调 Session.SetMode(NormalMode())
// 进入执行模式（下一次 Run 生效）。
//
// 语义边界：本工具无状态（不存储、不鉴权）——"确认"是宿主的动作，SDK 只提供
// 显式协议点，避免 LLM 直接 stop 导致宿主误判（兜底：宿主可从 AgentEnd{stop}
// 检测未提交的结束）。
func NewPlanSubmitTool() tools.Tool {
	return &planSubmitTool{
		BaseTool: tools.BaseTool{
			Name_: "plan_submit",
			Description_: "Submit the final plan (plan mode only). The plan parameter must contain the complete plan content; " +
				"after submitting, wait for user confirmation and do not perform any modifications before it is confirmed.",
			Params_: tools.Obj(map[string]any{
				"plan": tools.Str("Complete plan (steps, affected files, expected results)"),
			}, "plan"),
			ReadOnly_: true, // 协议动作：提交内容由宿主消费，无外部副作用
		},
	}
}

type planSubmitTool struct {
	tools.BaseTool
}

func (t *planSubmitTool) ValidParams(_ context.Context, _, arguments string) error {
	var a struct {
		Plan string `json:"plan"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return err
	}
	if a.Plan == "" {
		return errors.New("plan_submit: plan is required")
	}
	return nil
}

func (t *planSubmitTool) Call(_ context.Context, _, arguments string) (string, error) {
	var a struct {
		Plan string `json:"plan"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	return "plan submitted: " + a.Plan, nil
}
