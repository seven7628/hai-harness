package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/tools"
)

// LoadSkillTool 是渐进披露的激活入口（框架内置工具）：
// 模型判定任务匹配某个技能后调用 load_skill(name)，完整指令作为工具结果回传上下文。
type LoadSkillTool struct {
	reg *Registry

	// OnLoad 可选钩子：技能成功加载后回调（参数 = 技能名）。宿主注入产品级副作用
	// （如加载 ego-browser 技能时激活 ego lite 窗口）；工具层不关心具体逻辑。
	// nil = 无钩子。实现应快速返回（不阻塞工具结果）。
	OnLoad func(name string)
}

// WithOnLoad 设置技能加载钩子（宿主装配时调用；返回自身便于链式）。
func (t *LoadSkillTool) WithOnLoad(fn func(name string)) *LoadSkillTool {
	t.OnLoad = fn
	return t
}

// NewLoadSkillTool 创建 load_skill 工具（注册进 ToolEngine 后生效）。
func NewLoadSkillTool(reg *Registry) *LoadSkillTool {
	return &LoadSkillTool{reg: reg}
}

func (t *LoadSkillTool) Name() string { return "load_skill" }

func (t *LoadSkillTool) Description() string {
	return "Load the full instructions of a skill into the context. The skill list and names are in the system prompt; " +
		"call this when a task matches a skill's description, with the name parameter being the skill name."
}

func (t *LoadSkillTool) Parameters() any {
	return tools.Obj(map[string]any{
		"name": tools.Str("Skill name (see the skill list in the system prompt)"),
	}, "name")
}

func (t *LoadSkillTool) CanParallel() bool { return false }

type loadSkillArgs struct {
	Name string `json:"name"`
}

func (t *LoadSkillTool) ValidParams(_ context.Context, _, arguments string) error {
	var a loadSkillArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return fmt.Errorf("load_skill: %w", err)
	}
	if a.Name == "" {
		return errors.New("load_skill: name is required")
	}
	return nil
}

func (t *LoadSkillTool) BeforeCall(_ context.Context, _ core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}

func (t *LoadSkillTool) Call(_ context.Context, _, arguments string) (string, error) {
	var a loadSkillArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	s, err := t.reg.Load(a.Name)
	if err != nil {
		return "", err
	}
	if s.Instructions == "" {
		return "", fmt.Errorf("skill %q has no instructions", a.Name)
	}
	// 宿主钩子：技能成功加载后通知（如 ego-browser → 激活窗口）。
	if t.OnLoad != nil {
		t.OnLoad(s.Name)
	}
	// 渲染与命令路径（agents/commands.go 的 skills load）共用：同一段指令字节，
	// 只有「谁决定加载」不同（模型调用工具 vs 用户下命令）。
	return InstructionsBlock(s), nil
}

func (t *LoadSkillTool) AfterCall(_ context.Context, _ core.ToolCall) {}
