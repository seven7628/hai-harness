package subagent

import (
	"context"
	"fmt"
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

// 端到端（真实 AgentLoop 三层嵌套）：主 loop → agent_spawn（子 loop）→ 子 loop 内
// agent_spawn（孙 loop）。断言孙任务结果回到**子 loop 的上下文**（Poll drain 消费
// Task.inbox），且只有子任务的结果经 OnTaskDone 进主会话。
//
// 这条链路由三处接线共同成立，缺一即回归：
//  1. agents.runTools 把 RunOptions.TaskId 注入 ToolContext.TaskId（发起方是谁）；
//  2. subagent.Tool.runBackground 把它作为 StartOptions.ParentTaskID 传下去；
//  3. Registry 收尾时投父任务 inbox（而非 OnTaskDone 推主会话）。

// spinTool 空转工具：让子 loop 在孙任务结果到达前继续轮次（每轮 LLM Start 前 Poll
// drain 一次），避免"结果还没到就收尾"造成的时序偶发。
type spinTool struct{ d time.Duration }

func (s *spinTool) Name() string        { return "spin" }
func (s *spinTool) Description() string { return "空转等待（测试用）" }
func (s *spinTool) Parameters() any     { return tools.Obj(map[string]any{}) }
func (s *spinTool) CanParallel() bool   { return true }
func (s *spinTool) ValidParams(context.Context, string, string) error {
	return nil
}
func (s *spinTool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}
func (s *spinTool) AfterCall(context.Context, core.ToolCall) {}
func (s *spinTool) Call(ctx context.Context, _, _ string) (string, error) {
	select {
	case <-time.After(s.d):
	case <-ctx.Done():
	}
	return "waited", nil
}

// childProvider 子 loop 的 provider：第 1 轮派孙任务；其后每轮检查上下文里是否已出现
// 孙任务结果正文——出现才 stop，否则调 spin 再来一轮。
type childProvider struct {
	mu       sync.Mutex
	spawned  bool
	seen     bool
	requests int
}

func (p *childProvider) Stream(_ context.Context, req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	p.mu.Lock()
	p.requests++
	round := p.requests
	p.mu.Unlock()

	for _, m := range req.Messages {
		for _, c := range m.Content {
			if strings.Contains(c.Content, "孙任务结论") {
				p.mu.Lock()
				p.seen = true
				p.mu.Unlock()
			}
		}
	}
	p.mu.Lock()
	seen := p.seen
	p.mu.Unlock()
	if seen {
		return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "子 agent 汇总孙任务结果后收尾"})
	}
	if round == 1 {
		return handler(provider.LLMEndEvent{
			FinishReason: core.FinishReasonToolCall,
			ToolCalls: []core.ToolCall{
				{Id: "c1", Index: 0, Name: "agent_spawn", Arguments: `{"name":"grandchild","task":"分析孙问题"}`},
			},
		})
	}
	return handler(provider.LLMEndEvent{
		FinishReason: core.FinishReasonToolCall,
		ToolCalls:    []core.ToolCall{{Id: fmt.Sprintf("s%d", round), Index: 0, Name: "spin", Arguments: `{}`}},
	})
}

func (p *childProvider) sawGrandResult() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seen
}

// TestNestedSpawnEndToEndDelivery 主 → 子 → 孙真实嵌套：孙任务结果落在子 loop 上下文，
// 主会话只收到子任务结果（不回归主链路，也不把孙结果错投主会话）。
func TestNestedSpawnEndToEndDelivery(t *testing.T) {
	reg := NewRegistry()
	reg.SetSessionCtx(context.Background())
	defer reg.Close(context.Background())

	var mu sync.Mutex
	var pushed []string
	reg.SetOnTaskDone(func(taskID, _ string, _ string, _ TaskStatus, _ core.Usage, _ error) {
		mu.Lock()
		pushed = append(pushed, taskID)
		mu.Unlock()
	})
	pushedIDs := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), pushed...)
	}

	bgCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 孙 loop：慢一点（30ms）跑完，确保子 loop 至少经历一次"结果未到"的轮次。
	grandP := &mockProvider{rounds: []func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
		func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			time.Sleep(30 * time.Millisecond)
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "孙任务结论"})
		},
	}}
	grandLoop := agents.NewAgentLoop(agents.WithProvider(grandP))

	// agent_spawn（子/孙共用同一工具实例：子 loop 与主 loop 共享工具集是真实形态）
	spawnTool := NewBackgroundTool(Spec{
		Name: "agent_spawn", Description: "d", Loop: grandLoop, Background: true, MaxDepth: maxSpawnDepth,
	}, func() context.Context { return bgCtx }, reg)

	// 子 loop：工具集 = agent_spawn + spin
	cp := &childProvider{}
	childEngine := tools.NewToolEngine()
	childEngine.RegisterTool(context.Background(), spawnTool)
	childEngine.RegisterTool(context.Background(), &spinTool{d: 20 * time.Millisecond})
	childLoop := agents.NewAgentLoop(
		agents.WithProvider(cp),
		agents.WithToolEngine(childEngine),
		agents.WithMaxIterations(30),
	)

	// agent_spawn（主 loop 用）：派子任务
	mainSpawn := NewBackgroundTool(Spec{
		Name: "agent_spawn", Description: "d", Loop: childLoop, Background: true, MaxDepth: maxSpawnDepth,
	}, func() context.Context { return bgCtx }, reg)
	mainEngine := tools.NewToolEngine()
	mainEngine.RegisterTool(context.Background(), mainSpawn)

	mainP := &mockProvider{rounds: []func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
		func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonToolCall,
				ToolCalls: []core.ToolCall{
					{Id: "m1", Index: 0, Name: "agent_spawn", Arguments: `{"name":"child","task":"你负责协调子任务"}`},
				},
			})
		},
		func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			// 主 loop 第二轮：子任务多半还在跑（结果未必回），不阻塞——本测试只关心投递通道
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "主 agent 收尾"})
		},
	}}
	mainLoop := agents.NewAgentLoop(agents.WithProvider(mainP), agents.WithToolEngine(mainEngine), agents.WithMaxIterations(2))
	mainLoop.Run(bgCtx, []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "派子代理协调"})}, nil)

	// 等子任务（= childLoop 运行）收尾
	deadline := time.Now().Add(10 * time.Second)
	var childInfo TaskInfo
	for time.Now().Before(deadline) {
		for _, ti := range reg.List() {
			if ti.Name == "child" && ti.Status != TaskRunning {
				childInfo = ti
				break
			}
		}
		if childInfo.ID != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childInfo.ID == "" {
		t.Fatal("子任务未在 10s 内收尾")
	}

	// 1) 孙任务结果进了**子 loop 的上下文**（Poll drain 消费 inbox）
	if !cp.sawGrandResult() {
		t.Fatalf("子 loop 未收到孙任务结果（孙结果应投本 loop inbox，经 Poll drain 入上下文）")
	}
	// 2) 主会话只收到子任务结果；孙任务结果不经 OnTaskDone
	if len(pushedIDs()) != 1 || pushedIDs()[0] != childInfo.ID {
		t.Fatalf("主会话应只收到子任务 %s 的结果，实际: %v", childInfo.ID, pushedIDs())
	}
	// 3) 孙任务在 Registry 里终态正常（TaskList/TaskOutput 仍可查）
	var grand TaskInfo
	for _, ti := range reg.List() {
		if ti.Name == "grandchild" {
			grand = ti
		}
	}
	if grand.ID == "" || grand.Status != TaskCompleted {
		t.Fatalf("孙任务终态异常: %+v", grand)
	}
}

// TestNestedSpawnEventsStillForwardToUI 子→孙派发只改**结果投递目标**，不改事件流：
// 孙任务的 TaskStarted/AgentStart/TaskEnd 仍经父 handler 上浮（前端任务卡片照常收敛）。
func TestNestedSpawnEventsStillForwardToUI(t *testing.T) {
	reg := NewRegistry()
	reg.SetSessionCtx(context.Background())
	defer reg.Close(context.Background())
	reg.SetOnTaskDone(func(string, string, string, TaskStatus, core.Usage, error) {})

	bgCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	grandLoop := agents.NewAgentLoop(agents.WithProvider(&mockProvider{rounds: []func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
		stopRound,
	}}))
	spawn := NewBackgroundTool(Spec{
		Name: "agent_spawn", Description: "d", Loop: grandLoop, Background: true, MaxDepth: maxSpawnDepth,
	}, func() context.Context { return bgCtx }, reg)

	var evs safeEvents
	parent, err := reg.Start(bgCtx, "child agent", func(bg context.Context, tk *Task) (string, core.Usage, error) {
		tctx := events.WithToolContextForRunTask(bg, "run-child", "run-root", tk.ID, 1, evs.handler(), nil, nil)
		if _, cerr := spawn.Call(tctx, "", `{"name":"grandchild","task":"分析 X"}`); cerr != nil {
			return "", core.Usage{}, cerr
		}
		select {
		case <-tk.inbox:
			return "子 agent 收尾", core.Usage{}, nil
		case <-time.After(5 * time.Second):
			return "", core.Usage{}, context.DeadlineExceeded
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Wait(parent.ID); err != nil {
		t.Fatalf("父任务失败: %v", err)
	}

	var started, ended bool
	var childStart *events.AgentStart
	for _, e := range evs.snapshot() {
		switch e := e.(type) {
		case *events.TaskStarted:
			if e.Name == "grandchild" {
				started = true
			}
		case *events.TaskEnd:
			if e.TaskId != "" {
				ended = true
			}
		case *events.AgentStart:
			// 孙运行的 AgentStart（depth 2 / 有 task_id）：卡片是它建的，归属锚必须同一条
			// 事件带下来 —— 否则前端只能从 task_started 占位继承，占位一被裁就丢归属
			//（2026-11：孙任务卡长在主会话转录里）。
			if e.Depth == 2 && e.TaskId != "" {
				childStart = e
			}
		}
	}
	if childStart == nil {
		t.Fatal("孙运行的 AgentStart 事件应上浮到父事件流")
	}
	if childStart.ParentTaskId != parent.ID {
		t.Errorf("孙运行 AgentStart.ParentTaskId = %q，期望发起方任务 %q（卡片归属锚随运行事件走）",
			childStart.ParentTaskId, parent.ID)
	}
	if !started {
		t.Error("孙任务 TaskStarted 事件应上浮到父事件流（前端任务卡片占位）")
	}
	if !ended {
		t.Error("孙任务 TaskEnd 事件应上浮到父事件流（前端任务卡片终态）")
	}
}

// shortLivedChildProvider 子 loop 的 provider：第 1 轮派孙任务，之后每轮都试图收尾
// （复现真实子 agent 的行为——干完自己的活就 stop，不会主动等孙任务）。
// 若上下文中已出现孙任务结果，则如实汇总后收尾。
type shortLivedChildProvider struct {
	mu       sync.Mutex
	requests int
	seen     bool
}

func (p *shortLivedChildProvider) Stream(ctx context.Context, req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	// 模拟 LLM 往返延迟（真实一轮数百毫秒~数秒）：Stop 门控给的那一轮就是靠这段时间
	// 让孙任务结果回到 inbox。mock provider 零延迟会让门控看起来无效（假阴性）。
	select {
	case <-time.After(60 * time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}
	p.mu.Lock()
	p.requests++
	round := p.requests
	p.mu.Unlock()

	for _, m := range req.Messages {
		for _, c := range m.Content {
			if strings.Contains(c.Content, "孙任务结论") {
				p.mu.Lock()
				p.seen = true
				p.mu.Unlock()
			}
		}
	}
	p.mu.Lock()
	seen := p.seen
	p.mu.Unlock()
	if seen {
		return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "子 agent 汇总孙任务结论后收尾"})
	}
	if round == 1 {
		return handler(provider.LLMEndEvent{
			FinishReason: core.FinishReasonToolCall,
			ToolCalls: []core.ToolCall{
				{Id: "c1", Index: 0, Name: "agent_spawn", Arguments: `{"name":"grandchild","task":"分析孙问题"}`},
			},
		})
	}
	// 收尾意愿（没有孙任务结果也收尾）——修复前 loop 就在这里结束，孙结果回落主会话
	return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "子 agent 干完自己的活，先收尾"})
}

// TestNestedSpawnParentKeepsAliveForGrandchild 子 agent 派完孙任务就打算收尾 —— 这是
// 最常见的真实形态（子干完自己的活，孙任务往往才是重活）。子 loop 必须在自然 stop 时
// 被告知"还有在途孙任务"并多跑一轮，孙结果才真的进得了它的上下文；否则结果回落主会话，
// 等于修复没生效（主 agent 又收到一条自己没派过的结果）。
func TestNestedSpawnParentKeepsAliveForGrandchild(t *testing.T) {
	reg := NewRegistry()
	reg.SetSessionCtx(context.Background())
	defer reg.Close(context.Background())

	var mu sync.Mutex
	var pushedNames []string
	reg.SetOnTaskDone(func(_ string, name, _ string, _ TaskStatus, _ core.Usage, _ error) {
		mu.Lock()
		pushedNames = append(pushedNames, name)
		mu.Unlock()
	})

	bgCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 孙 loop：慢一点，确保子 loop 第一轮"想收尾"时它还没回来
	grandP := &mockProvider{rounds: []func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
		func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			time.Sleep(80 * time.Millisecond)
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "孙任务结论"})
		},
	}}
	grandLoop := agents.NewAgentLoop(agents.WithProvider(grandP))

	spawnTool := NewBackgroundTool(Spec{
		Name: "agent_spawn", Description: "d", Loop: grandLoop, Background: true, MaxDepth: maxSpawnDepth,
	}, func() context.Context { return bgCtx }, reg)

	cp := &shortLivedChildProvider{}
	childEngine := tools.NewToolEngine()
	childEngine.RegisterTool(context.Background(), spawnTool)
	childLoop := agents.NewAgentLoop(
		agents.WithProvider(cp),
		agents.WithToolEngine(childEngine),
		agents.WithMaxIterations(10),
	)

	mainSpawn := NewBackgroundTool(Spec{
		Name: "agent_spawn", Description: "d", Loop: childLoop, Background: true, MaxDepth: maxSpawnDepth,
	}, func() context.Context { return bgCtx }, reg)
	mainEngine := tools.NewToolEngine()
	mainEngine.RegisterTool(context.Background(), mainSpawn)
	mainLoop := agents.NewAgentLoop(
		agents.WithProvider(&mockProvider{rounds: []func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
			func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
				return handler(provider.LLMEndEvent{
					FinishReason: core.FinishReasonToolCall,
					ToolCalls: []core.ToolCall{
						{Id: "m1", Index: 0, Name: "agent_spawn", Arguments: `{"name":"child","task":"你负责协调子任务"}`},
					},
				})
			},
			stopRound,
		}}),
		agents.WithToolEngine(mainEngine), agents.WithMaxIterations(2))
	mainLoop.Run(bgCtx, []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "派子代理协调"})}, nil)

	// 等子任务收尾
	deadline := time.Now().Add(10 * time.Second)
	var child TaskInfo
	for time.Now().Before(deadline) {
		for _, ti := range reg.List() {
			if ti.Name == "child" && ti.Status != TaskRunning {
				child = ti
			}
		}
		if child.ID != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if child.ID == "" {
		t.Fatal("子任务未在 10s 内收尾")
	}
	res, err := reg.Snapshot(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = res

	// 1) 子 agent 的最终结果里含孙任务结论（它真的等到了，而不是自己先收尾）
	snap, err := reg.Snapshot(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = snap
	final := childFinalText(t, reg, child.ID)
	if !strings.Contains(final, "孙任务结论") {
		t.Fatalf("子 agent 未收到孙任务结果（应在收尾前被告知在途任务并多跑一轮）; 子任务最终结果=%q", final)
	}
	// 2) 主会话没有收到野生孙任务结果
	mu.Lock()
	defer mu.Unlock()
	for _, n := range pushedNames {
		if n == "grandchild" {
			t.Fatalf("孙任务结果不应回落主会话（子 agent 应在收尾前等到它），实际推送: %v", pushedNames)
		}
	}
}

// childFinalText 取子任务的最终结果文本（Registry.Snapshot 不带 Result，故经 Wait 语义外的
// 只读途径：List + Task 结果由 registry 内部保存，这里用 TaskOutput 无关的最小实现——
// 通过 reg.Snapshot 拿状态，正文用 journal/结果字段不可得时由调用方断言 status。
// 为避免依赖内部结构，这里改为直接读 Task 对象。
func childFinalText(t *testing.T, reg *Registry, id string) string {
	t.Helper()
	task, err := reg.get(id)
	if err != nil {
		t.Fatalf("取子任务失败: %v", err)
	}
	task.mu.Lock()
	defer task.mu.Unlock()
	return task.Result
}
