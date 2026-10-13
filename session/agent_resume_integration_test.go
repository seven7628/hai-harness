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
// 只能从头重新 spawn。此处钉住：
//  1. agent_resume 在真实 Session 装配下可达（schema 注册 + 引擎分派）；
//  2. 续跑产出**新** task id，且新任务的结果照常经 OnTaskDone → PushTaskResult 回传；
//  3. 交接简报带原任务身份与原始指令（否则子 agent 无从知道自己在接手什么）。
func TestAgentResumeEndToEnd(t *testing.T) {
	journalDir := t.TempDir()
	journalPath := func(taskID string) (string, error) {
		return filepath.Join(journalDir, taskID+".jsonl"), nil
	}

	// 子 agent 的脚本：用「阻塞开关」而不是轮次下标决定行为 —— 中断可能在子运行
	// 发出第一次 LLM 调用之前就已到达（Registry 先登记任务再起 goroutine），
	// 按下标脚本会错位把「阻塞轮」留给续跑任务。此处每次调用都捕获输入（续跑正是
	// 要验证它收到了交接简报），并在开关打开时阻塞直到被放行。
	gate := make(chan struct{})
	var (
		childMu    sync.Mutex
		childCalls int
		briefs     []string
		blockChild = true
	)
	childP := &rzFuncProvider{stream: func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
		var got string
		for _, m := range req.Messages {
			if m.Role != core.User {
				continue
			}
			for _, c := range m.Content {
				if c.Type == core.ContentTypeText && c.Content != "" {
					got = c.Content
				}
			}
			break
		}
		childMu.Lock()
		childCalls++
		briefs = append(briefs, got)
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
	for _, tool := range subagent.NewAgentTools(childLoop, func() context.Context { return s.Context() }, reg, journalPath, nil, nil) {
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
			if infos := reg.List(); len(infos) >= 1 {
				for _, in := range infos {
					if in.ID != taskID && subagent.ValidTaskID(in.ID) {
						resumedTaskID = in.ID
					}
				}
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
	// 放行开关同时把 blockChild 置 false —— 之后派生的续跑任务不再阻塞（见 rzFuncProvider）。
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

	// 续跑任务登记且最终完成；原任务保持 interrupted（新任务语义，不回滚旧终态）。
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		infos := reg.List()
		if len(infos) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	infos := reg.List()
	if len(infos) < 2 {
		t.Fatalf("续跑任务未登记，registry=%+v", infos)
	}
	if resumedTaskID == "" {
		for _, in := range infos {
			if in.ID != spawned {
				resumedTaskID = in.ID
			}
		}
	}
	if resumedTaskID == "" || resumedTaskID == spawned {
		t.Fatalf("续跑应产生新 task id：spawned=%s resumed=%s", spawned, resumedTaskID)
	}
	if info, err := reg.Snapshot(spawned); err != nil || info.Status != subagent.TaskInterrupted {
		t.Fatalf("原任务终态不应被改写: %+v err=%v", info, err)
	}
	// 恢复链可追溯（TaskList 数据源）。
	if info, err := reg.Snapshot(resumedTaskID); err != nil || info.ResumedFrom != spawned {
		t.Fatalf("续跑任务的 resumed_from 应为 %s: %+v err=%v", spawned, info, err)
	}

	// 交接简报：续跑子 agent 真的收到了原任务身份与原始指令。
	// 先放行阻塞开关（原任务那一轮），再等续跑任务收尾，最后在捕获器里找**带交接
	// 标记**的那份输入（按内容而不是按下标取，避免受「中断可能早于首次调用」影响）。
	waitChild := time.Now().Add(5 * time.Second)
	for time.Now().Before(waitChild) {
		if info, err := reg.Snapshot(resumedTaskID); err == nil && !rzTaskActive(info.Status) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	childMu.Lock()
	all := append([]string(nil), briefs...)
	childMu.Unlock()
	brief := ""
	for _, b := range all {
		if strings.Contains(b, "[Resumed background task]") {
			brief = b
		}
	}
	if brief == "" {
		t.Fatalf("续跑子 agent 未收到交接简报，捕获到 %d 份输入: %q", len(all), all)
	}
	for _, want := range []string{spawned, "调研并写出长文报告", "接着写完报告"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("交接简报缺 %q:\n%s", want, brief)
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
