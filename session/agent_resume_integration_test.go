package session

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/subagent"
	"github.com/seven7628/hai-harness/tools"
)

// rzFuncProvider 以回调实现 provider.Provider（测试要按「当前状态」而非轮次下标决定行为）。
type rzFuncProvider struct {
	stream func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error
}

func (p *rzFuncProvider) Stream(_ context.Context, req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	return p.stream(req, handler)
}

// TestAgentResumeEndToEnd 端到端：主会话派生后台子 agent → 中断它 → 主 Agent 调
// agent_resume 续跑 → 续跑结果自动回传主会话（模型下一轮可见）。
//
// 这条链路是本功能的存在理由：被中断的任务原本无从续起（agent_send 拒绝终态任务），
// 只能从头重新 spawn。此处钉住**会话级恢复语义**（2026-10 修订）：
//  1. agent_resume 在真实 Session 装配下可达（schema 注册 + 引擎分派）；
//  2. 续跑**原地复用同一 task id**（不新建任务对象）——任务卡按同一 id 从终态翻回
//     running，恢复链不再靠 ResumedFrom 拼接；
//  3. 续跑子 agent 收到的是**上一轮落盘的真实上下文**（含原任务原文），不是有损简报；
//  4. 续跑结果照常经 OnTaskDone → PushTaskResult 回传主会话。
func TestAgentResumeEndToEnd(t *testing.T) {
	journalDir := t.TempDir()
	journalPath := func(taskID string) (string, error) {
		return filepath.Join(journalDir, taskID+".jsonl"), nil
	}

	// 子 agent 的脚本：用「阻塞开关」而不是轮次下标决定行为 —— 中断可能在子运行
	// 发出第一次 LLM 调用之前就已到达（Registry 先登记任务再起 goroutine），
	// 按下标脚本会错位把「阻塞轮」留给续跑任务。此处每次调用都捕获**全部用户输入**
	//（续跑正是要验证它收到了落盘上下文），并在开关打开时阻塞直到被放行。
	gate := make(chan struct{})
	var (
		childMu    sync.Mutex
		childCalls int
		inputs     []string
		blockChild = true
	)
	childP := &rzFuncProvider{stream: func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
		var joined strings.Builder
		for _, m := range req.Messages {
			if m.Role != core.User {
				continue
			}
			for _, c := range m.Content {
				if c.Type == core.ContentTypeText && c.Content != "" {
					joined.WriteString(c.Content)
					joined.WriteString("\n")
				}
			}
		}
		childMu.Lock()
		childCalls++
		inputs = append(inputs, joined.String())
		block := blockChild
		childMu.Unlock()
		if block {
			<-gate // 原任务：等中断后放行
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "未完成"})
		}
		return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "续跑后的最终结论"})
	}}
	childLoop := agents.NewAgentLoop(agents.WithProvider(childP))

	reg := subagent.NewRegistry()
	var s *Session
	eng := tools.NewToolEngine()
	for _, tool := range subagent.NewAgentTools(childLoop, func() context.Context { return s.Context() }, reg, journalPath, nil, nil,
		subagent.WithAgentStateStore(subagent.NewMemoryStateStore())) { // 会话级恢复：注入状态持久化端口
		eng.RegisterTool(context.Background(), tool)
	}

	// 主 Agent 脚本：r1 派子任务 → r2 stop（等中断）→ r3 调 agent_resume → r4 收尾。
	if _, err := eng.GetTool(context.Background(), "agent_resume"); err != nil {
		t.Fatalf("agent_resume 未注册进引擎: %v", err)
	}
	var taskID string
	var resumedTaskID string
	mainP := &rzScriptProvider{rounds: []func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
		func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonToolCall,
				ToolCalls:    []core.ToolCall{{Id: "c1", Index: 0, Name: "agent_spawn", Arguments: `{"name":"long research","task":"调研并写出长文报告"}`}},
			})
		},
		func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "已派出，等待"})
		},
		func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			// 打断是异步的：这里等任务真的进终态再续（否则会被状态闸门拒绝）。
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if infos := reg.List(); len(infos) == 1 && !rzTaskActive(infos[0].Status) {
					taskID = infos[0].ID
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if taskID == "" {
				return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "任务未达终态"})
			}
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonToolCall,
				ToolCalls:    []core.ToolCall{{Id: "c2", Index: 0, Name: "agent_resume", Arguments: `{"task_id":"` + taskID + `","message":"接着写完报告"}`}},
			})
		},
		func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			// 原地续跑：注册表里始终只有同一个任务（同一 id），等待它再次收尾。
			if infos := reg.List(); len(infos) >= 1 {
				resumedTaskID = infos[0].ID
			}
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "done"})
		},
	}}
	mainLoop := agents.NewAgentLoop(agents.WithProvider(mainP), agents.WithToolEngine(eng))

	var (
		delivMu   sync.Mutex
		delivered []string
	)
	s = rzMustSession(t, "resume-e2e", mainLoop, nil, WithRegistry(reg), WithEventHandler(func(_ context.Context, e events.Event) {
		if ev, ok := e.(*events.TaskResultDelivered); ok {
			delivMu.Lock()
			delivered = append(delivered, ev.TaskId)
			delivMu.Unlock()
		}
	}))
	reg.SetOnTaskDone(func(taskID, name, result string, status subagent.TaskStatus, usage core.Usage, err error) {
		if perr := s.PushTaskResultUsage(taskID, name, result, string(status), usage, err); perr != nil {
			t.Errorf("PushTaskResult: %v", perr)
		}
	})

	if err := s.Ask(context.Background(), core.NewUserMessage(core.Content{Type: "text", Content: "调研并写报告"})); err != nil {
		t.Fatal(err)
	}
	// 第一轮：派出子任务（停在 gate 上，running）。
	if err := s.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	var spawned string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if infos := reg.List(); len(infos) == 1 && subagent.ValidTaskID(infos[0].ID) {
			spawned = infos[0].ID
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if spawned == "" {
		t.Fatalf("子任务未登记: %+v", reg.List())
	}
	// 中断它（模拟「任务没交付就结束了」）；放行阻塞开关让子运行收尾到 interrupted。
	// 放行开关同时把 blockChild 置 false —— 之后续跑不再阻塞（见 rzFuncProvider）。
	if err := reg.Interrupt(spawned); err != nil {
		t.Fatal(err)
	}
	childMu.Lock()
	blockChild = false
	childMu.Unlock()
	close(gate)
	for time.Now().Before(deadline) {
		if info, err := reg.Snapshot(spawned); err == nil && info.Status == subagent.TaskInterrupted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if info, _ := reg.Snapshot(spawned); info.Status != subagent.TaskInterrupted {
		t.Fatalf("原任务应为 interrupted, got %s", info.Status)
	}

	// 第二轮：主 loop 调 agent_resume 续跑。
	if err := s.Ask(context.Background(), core.NewUserMessage(core.Content{Type: "text", Content: "继续"})); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	// 原地续跑：任务总数仍为 1（同一 task id 被重新驱动），最终回到 completed。
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := reg.Snapshot(spawned); err == nil && info.Status == subagent.TaskCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	infos := reg.List()
	if len(infos) != 1 {
		t.Fatalf("续跑应原地复用同一任务（注册表应恒为 1 条），实际 %d 条: %+v", len(infos), infos)
	}
	if resumedTaskID != spawned {
		t.Fatalf("续跑必须是同一 task id：spawned=%s resumed=%s", spawned, resumedTaskID)
	}
	info, err := reg.Snapshot(spawned)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != subagent.TaskCompleted {
		t.Fatalf("续跑后任务应回到 completed，实际 %s", info.Status)
	}
	// 恢复链可追溯：原地续跑的 ResumedFrom 记为自身（同一身份）。
	if info.ResumedFrom != spawned {
		t.Fatalf("原地续跑的 resumed_from 应为自身 %s: %+v", spawned, info)
	}

	// 上下文连续性（本功能的核心价值）：续跑子 agent 收到的是**落盘的真实上下文**，
	// 含原任务原文 —— 不是有损简报，也不需要重做已完成的工作。
	waitChild := time.Now().Add(5 * time.Second)
	for time.Now().Before(waitChild) {
		if info, err := reg.Snapshot(spawned); err == nil && !rzTaskActive(info.Status) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	childMu.Lock()
	all := append([]string(nil), inputs...)
	childMu.Unlock()
	if len(all) == 0 {
		t.Fatal("续跑子 agent 未收到任何输入")
	}
	// 最后一次调用即续跑：输入应同时含**原任务原文**（从落盘上下文恢复）与本次增量指令。
	resumedInput := all[len(all)-1]
	for _, want := range []string{"调研并写出长文报告", "接着写完报告"} {
		if !strings.Contains(resumedInput, want) {
			t.Fatalf("续跑输入应同时含原任务原文与增量指令 %q:\n%s", want, resumedInput)
		}
	}

	// 续跑结果自动回传主会话（模型下一轮能看到）——与原派发同一条通道。
	if err := s.Ask(context.Background(), core.NewUserMessage(core.Content{Type: "text", Content: "还有吗"})); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		delivMu.Lock()
		n := len(delivered)
		delivMu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	delivMu.Lock()
	defer delivMu.Unlock()
	found := false
	for _, id := range delivered {
		if id == resumedTaskID {
			found = true
		}
	}
	if !found {
		t.Fatalf("续跑结果应经 TaskResultDelivered 回传主会话：delivered=%v resumed=%s", delivered, resumedTaskID)
	}
}

// ---------- 本文件自带的测试桩（rz 前缀） ----------
//
// 为什么不复用同包既有的 mockProvider / mustSession：它们定义在**未入库**的测试文件里
// （本仓 .git/info/exclude 隐藏了 262 个 _test.go），干净检出下不存在 —— 引用会让
// session 包在干净检出里编译失败。本文件自带最小集合，**只提交它也能编译通过**。

// rzScriptProvider 按预设脚本逐轮发射 LLM 事件（同 mockProvider 语义，自带一份）。
type rzScriptProvider struct {
	rounds []func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error
	calls  int
}

func (m *rzScriptProvider) Stream(_ context.Context, req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	round := m.rounds[m.calls]
	if m.calls < len(m.rounds)-1 {
		m.calls++
	}
	return round(req, handler)
}

// rzMustSession 建会话并断言成功（同 mustSession 语义，自带一份）。
func rzMustSession(t *testing.T, id string, loop *agents.AgentLoop, store Store, opts ...Option) *Session {
	t.Helper()
	s, err := NewSession(id, loop, store, opts...)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return s
}

// rzTaskActive 状态是否仍在跑（running / interrupting）——测试内联判定，等价于
// subagent 侧的权威语义，避免为断言在包里新开导出的 API。
func rzTaskActive(st subagent.TaskStatus) bool {
	return st == subagent.TaskRunning || st == subagent.TaskInterrupting
}
