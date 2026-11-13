package agents

import (
	"context"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/skills"
	"github.com/seven7628/hai-harness/tools"
	"strings"
	"testing"
)

// reqCaptureProvider 捕获请求消息并正常结束一轮。
type reqCaptureProvider struct {
	messages []core.Message
}

func (p *reqCaptureProvider) Stream(_ context.Context, req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	p.messages = req.Messages
	return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
}

// TestSkillManifestInjection 验证渐进披露的发现阶段：技能清单（仅元数据）
// 拼接进系统提示词。
func TestSkillManifestInjection(t *testing.T) {
	reg, err := skills.NewRegistry("../skills/testdata/skills")
	if err != nil {
		t.Fatal(err)
	}
	p := &reqCaptureProvider{}
	loop := NewAgentLoop(WithProvider(p), WithSystemPrompt("你是助手"), WithSkills(reg))

	_ = loop.Run(context.Background(), []core.Message{
		core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
	}, nil)

	if len(p.messages) == 0 || p.messages[0].Role != core.System {
		t.Fatalf("system prompt should be first: %+v", p.messages)
	}
	sp := p.messages[0].Content[0].Content
	if !strings.Contains(sp, "你是助手") || !strings.Contains(sp, "code-review") || !strings.Contains(sp, "load_skill") {
		t.Fatalf("manifest should be appended to system prompt: %q", sp)
	}
	// 发现阶段不携带指令正文（断言只存在于正文的内容）
	if strings.Contains(sp, "阅读变更文件") {
		t.Fatalf("instructions must not leak into manifest: %q", sp)
	}
}

// TestSkillManifestOmitted 验证未配置技能时系统提示词保持原样。
func TestSkillManifestOmitted(t *testing.T) {
	p := &reqCaptureProvider{}
	loop := NewAgentLoop(WithProvider(p), WithSystemPrompt("你是助手"))

	_ = loop.Run(context.Background(), []core.Message{
		core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
	}, nil)

	sp := p.messages[0].Content[0].Content
	if strings.Contains(sp, "Available skills") || strings.Contains(sp, "load_skill") {
		t.Fatalf("manifest must be omitted without skills: %q", sp)
	}
}

// TestLoadSkillActivation 验证激活链路端到端：模型调用 load_skill 工具 →
// 完整指令作为工具结果回传上下文。
func TestLoadSkillActivation(t *testing.T) {
	reg, err := skills.NewRegistry("../skills/testdata/skills")
	if err != nil {
		t.Fatal(err)
	}
	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), skills.NewLoadSkillTool(reg))

	p := &mockProvider{
		rounds: []func(handler func(e provider.StreamEvent) error) error{
			func(handler func(e provider.StreamEvent) error) error {
				return handler(provider.LLMEndEvent{
					FinishReason: core.FinishReasonToolCall,
					ToolCalls: []core.ToolCall{
						{Id: "c1", Index: 0, Name: "load_skill", Arguments: `{"name":"code-review"}`},
					},
				})
			},
			func(handler func(e provider.StreamEvent) error) error {
				return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "done"})
			},
		},
	}
	loop := NewAgentLoop(WithProvider(p), WithToolEngine(eng), WithSkills(reg))

	ac := loop.Run(context.Background(), []core.Message{
		core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
	}, nil)
	if ac.Err != nil {
		t.Fatal(ac.Err)
	}

	found := false
	for _, m := range ac.Messages {
		if m.Role == core.Tool && strings.Contains(m.Content[0].Content, "代码审查") {
			found = true
		}
	}
	if !found {
		t.Fatalf("skill instructions should be injected via tool result: %+v", ac.Messages)
	}
}
