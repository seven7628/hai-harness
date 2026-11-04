package agents

import (
	"context"
	"sync"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
)

// 子运行的初始 Prompt 必须走事件流（否则子会话转录的第一条 user 消息永久空缺）。
//
// 用户 2026-11-04 报障：「子智能体会话页面没有主智能体给的 Prompt」。
// 成因不是投影/渲染（那两端早就能落 kind:'user' 项），而是**根本没有事件**：
// 初始 input 只进了 checkpoint/journal，唯一的 AgentUserMessage 发出点在 subagent 的
// Poll（只覆盖 inbox 投递）。修复 = AgentLoop 在 AgentStart 之后补发一条 Source=prompt。
//
// 为什么必须在 AgentStart **之后**：两侧 reducer 都只往已存在的 agent 卡片追加 user 项，
// 早于 AgentStart 会被静默丢弃（本用例按事件顺序钉住这一点）。
//
// 单独成文件：本仓多数 *_test.go 被 .git/info/exclude 排除（见项目记忆
// clean-checkout-test-breakage），写进那些文件不会随提交进库。本文件的 provider 桩自带。

// promptStubProvider 一轮就收尾的假 provider（只用来让 loop 跑完一次）。
type promptStubProvider struct{}

func (promptStubProvider) Stream(_ context.Context, _ *provider.StreamRequest, handler func(provider.StreamEvent) error) error {
	return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "done"})
}

func collectPromptEvents(t *testing.T, parentRunId, prompt string) (order []string, umsgs []*events.AgentUserMessage) {
	t.Helper()
	loop := NewAgentLoop(WithProvider(promptStubProvider{}), WithMaxIterations(2))
	var mu sync.Mutex
	handler := func(_ context.Context, e events.Event) {
		mu.Lock()
		defer mu.Unlock()
		switch ev := e.(type) {
		case *events.AgentStart:
			order = append(order, "agent_start")
		case *events.AgentUserMessage:
			order = append(order, "agent_user_message")
			umsgs = append(umsgs, ev)
		}
	}
	ac := loop.RunStream(context.Background(),
		[]core.Message{core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: prompt})},
		handler, &RunOptions{ParentRunId: parentRunId, TaskId: "task-abc"})
	if ac == nil {
		t.Fatal("RunStream returned nil")
	}
	return order, umsgs
}

// TestSubagentRunEmitsPromptAgentUserMessage 子运行：AgentStart 之后紧跟一条
// Source=prompt 的 AgentUserMessage，正文 = 派的 Prompt，RunId/TaskId 都带上。
func TestSubagentRunEmitsPromptAgentUserMessage(t *testing.T) {
	const prompt = "review the auth module and fix the 401 branch"
	order, umsgs := collectPromptEvents(t, "run-parent", prompt)

	if len(umsgs) != 1 {
		t.Fatalf("want 1 agent_user_message, got %d (order=%v)", len(umsgs), order)
	}
	// 顺序断言：必须是 agent_start 之后（早于它会因为没有卡片而被丢弃）
	if len(order) < 2 || order[0] != "agent_start" || order[1] != "agent_user_message" {
		t.Fatalf("agent_user_message must come right after agent_start, got %v", order)
	}
	u := umsgs[0]
	if u.Source != events.AgentUserSourcePrompt {
		t.Fatalf("source = %q, want %q", u.Source, events.AgentUserSourcePrompt)
	}
	if u.Text != prompt {
		t.Fatalf("text = %q, want the prompt verbatim", u.Text)
	}
	if u.TaskId != "task-abc" {
		t.Fatalf("task_id = %q, want task-abc（后台任务卡片的归属锚）", u.TaskId)
	}
	if u.RunId == "" {
		t.Fatal("run_id 必须带上（同步子智能体没有 task_id，只能靠 run_id 归属）")
	}
	if u.RunId == "run-parent" {
		t.Fatal("run_id 应是本次子运行的 id，不能是父运行 id")
	}
}

// TestMainRunDoesNotEmitPromptAgentUserMessage 主会话运行（ParentRunId 为空）不补发：
// 主会话的用户输入走 UserInputsConsumed，补发会让转录里出现两条一样的 user 块。
func TestMainRunDoesNotEmitPromptAgentUserMessage(t *testing.T) {
	_, umsgs := collectPromptEvents(t, "", "hello")
	if len(umsgs) != 0 {
		t.Fatalf("主 run 不该补发 agent_user_message，got %d", len(umsgs))
	}
}
