package subagent

// 端到端：消息投进子运行以后，宿主**必须看得见**（events.AgentUserMessage）。
//
// 用户 2026-10 报障：「主 Session 发送给子智能体的消息，我在子智能体中也没有看到」。
// 链路本身有三段，任何一段断了症状都一样（消息进了上下文，但转录里什么都没有）：
//  1. Registry.Send → Task.inbox（投递）
//  2. 子 loop 下一轮 Poll drain 消费 inbox（进上下文）
//  3. **Poll 里发 AgentUserMessage** → 宿主的 emit 漏斗（可见）
//
// 已有测试只钉住第 3 段的末端（事件 → 卡片 items），前面两段靠"读代码看着对"。
// 这条测试用真实 AgentLoop + 真实 Registry 跑一遍：子任务在跑，主侧 Send 一条，
// 断言父 loop 的 handler 真的收到了那条事件（task_id / 正文都对）。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/tools"
)

func TestSubAgentInboxMessageEmitsAgentUserMessage(t *testing.T) {
	reg := NewRegistry()
	reg.SetSessionCtx(context.Background())
	defer reg.Close(context.Background())

	bgCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 子 loop：每轮都空转，直到上下文里出现我们发的那句话才收尾。
	var sawMessage sync.Mutex
	seen := false
	childP := &mockProvider{rounds: []func(_ *provider.StreamRequest, h func(e provider.StreamEvent) error) error{
		func(_ *provider.StreamRequest, h func(e provider.StreamEvent) error) error {
			return h(provider.LLMEndEvent{FinishReason: core.FinishReasonToolCall,
				ToolCalls: []core.ToolCall{{Id: "s1", Index: 0, Name: "spin", Arguments: `{}`}}})
		},
		func(req *provider.StreamRequest, h func(e provider.StreamEvent) error) error {
			found := false
			for _, m := range req.Messages {
				for _, c := range m.Content {
					if strings.Contains(c.Content, "把返回 401 的分支也改掉") {
						found = true
					}
				}
			}
			sawMessage.Lock()
			seen = found
			sawMessage.Unlock()
			if !found {
				// 消息还没到：再空转一轮（Poll 在每轮 LLM 调用前 drain，所以下一轮必然到）
				return h(provider.LLMEndEvent{FinishReason: core.FinishReasonToolCall,
					ToolCalls: []core.ToolCall{{Id: "s2", Index: 0, Name: "spin", Arguments: `{}`}}})
			}
			return h(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "收到补充要求"})
		},
	}}
	childEngine := tools.NewToolEngine()
	childEngine.RegisterTool(context.Background(), &spinTool{d: 15 * time.Millisecond})
	childLoop := agents.NewAgentLoop(
		agents.WithProvider(childP),
		agents.WithToolEngine(childEngine),
		agents.WithMaxIterations(40),
	)

	spawn := NewBackgroundTool(Spec{
		Name: "agent_spawn", Description: "d", Loop: childLoop, Background: true, MaxDepth: maxSpawnDepth,
	}, func() context.Context { return bgCtx }, reg)

	var mu sync.Mutex
	var userMsgs []*events.AgentUserMessage
	handler := func(_ context.Context, e events.Event) {
		if um, ok := e.(*events.AgentUserMessage); ok {
			mu.Lock()
			userMsgs = append(userMsgs, um)
			mu.Unlock()
		}
	}

	// 主 loop：只派一个子任务，然后收尾（子任务自己在后台继续跑）
	mainEngine := tools.NewToolEngine()
	mainEngine.RegisterTool(context.Background(), spawn)
	mainP := &mockProvider{rounds: []func(_ *provider.StreamRequest, h func(e provider.StreamEvent) error) error{
		func(_ *provider.StreamRequest, h func(e provider.StreamEvent) error) error {
			return h(provider.LLMEndEvent{FinishReason: core.FinishReasonToolCall,
				ToolCalls: []core.ToolCall{{Id: "m1", Index: 0, Name: "agent_spawn", Arguments: `{"name":"child","task":"审查 auth"}`}}})
		},
		func(_ *provider.StreamRequest, h func(e provider.StreamEvent) error) error {
			return h(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "已派发"})
		},
	}}
	mainLoop := agents.NewAgentLoop(agents.WithProvider(mainP), agents.WithToolEngine(mainEngine), agents.WithMaxIterations(2))
	mainLoop.Run(bgCtx, []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "派个子代理"})}, handler)

	// 等子任务真的跑起来（Send 只在运行中可投）
	deadline := time.Now().Add(10 * time.Second)
	var taskID string
	for time.Now().Before(deadline) {
		for _, ti := range reg.List() {
			if ti.Name == "child" && ti.Status == TaskRunning {
				taskID = ti.ID
			}
		}
		if taskID != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if taskID == "" {
		t.Fatal("子任务没有进入运行态（10s）")
	}
	if err := reg.Send(taskID, "把返回 401 的分支也改掉"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// 等子任务消费完消息并收尾
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		done := true
		for _, ti := range reg.List() {
			if ti.ID == taskID && ti.Status == TaskRunning {
				done = false
			}
		}
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	sawMessage.Lock()
	gotInContext := seen
	sawMessage.Unlock()
	if !gotInContext {
		t.Fatal("消息没有进入子运行的上下文（Poll 没有 drain 到 inbox）")
	}
	mu.Lock()
	got := append([]*events.AgentUserMessage(nil), userMsgs...)
	mu.Unlock()
	if len(got) == 0 {
		t.Fatal("Poll 消费 inbox 时必须发 AgentUserMessage（否则宿主看不到人对子智能体说的话）")
	}
	if got[0].TaskId != taskID {
		t.Fatalf("事件归属错的 task_id（%q want %q）", got[0].TaskId, taskID)
	}
	if !strings.Contains(got[0].Text, "把返回 401 的分支也改掉") {
		t.Fatalf("事件正文不对: %q", got[0].Text)
	}
}
