package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/todo"
	"github.com/seven7628/hai-harness/tools"
)

// TestStopFinishesInOneRound 回归锁（2026-10）：模型以 stop + 无工具调用收尾时，**当轮**
// 结束——恰一次 LLM 请求、恰一次 AgentEnd，即便后台任务台账非空、会话 todo 还有未完成项。
//
// 为什么需要这条：收尾检查点原有两道「引擎替模型决定再跑一轮」的门控——在途任务提醒
// （PendingTasksNudge）与完成度检查（TodoGate）。会话日志实测（925 个会话快照）前者
// 触发 461 次、其中 344 次清单里 0 个在途任务（台账 reg.List() 含终态任务且只增不删），
// 后者 145 次里 123 次的下一轮只调了 todo_update。两道门控已删除，本测试是防回归锁：
// 任一门控复活 → provider 请求数与 LLMEnd/AgentEnd 计数立刻变。
func TestStopFinishesInOneRound(t *testing.T) {
	p := &captureRoundsProvider{rounds: []func(func(provider.StreamEvent) error) error{
		reminderStopRound("都做完了。"),
	}}
	loop := NewAgentLoop(
		WithProvider(p),
		WithModel("mock-model"),
		WithToolEngine(tools.NewToolEngine()),
	)
	handler, list := collectEvents()

	// 两道门控的触发条件同时成立：todo 有未完成项 + 台账非空（含 1 条在途、1 条终态）。
	st := todo.New()
	if _, err := st.Add("收尾：跑 go test", nil); err != nil {
		t.Fatal(err)
	}
	ledger := "[TaskStates]\n- task-abc (research): completed\n- task-def (impl): running (awaiting result)"

	ac := loop.RunStream(context.Background(),
		[]core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})},
		handler,
		&RunOptions{Todo: st, TaskState: func() string { return ledger }})
	if ac.Err != nil {
		t.Fatalf("run failed: %v", ac.Err)
	}

	p.mu.Lock()
	calls := p.calls
	p.mu.Unlock()
	if calls != 1 {
		t.Fatalf("stop 收尾必须恰一次 LLM 请求，got %d", calls)
	}

	counts := map[events.EventType]int{}
	for _, e := range *list {
		counts[e.Type()]++
	}
	// 一轮决策 = 一次 LLMEnd（mock 只发 LLMEnd，不发 LLMStart）。
	want := map[events.EventType]int{
		events.AgentStartType: 1,
		events.AgentEndType:   1,
		events.LLMEndType:     1,
	}
	for k, v := range want {
		if counts[k] != v {
			t.Fatalf("event %s: want %d, got %d (all: %v)", k, v, counts[k], counts)
		}
	}

	// 收尾语义不变：AgentEnd 带最终回复、finish_reason=stop、不算失败。
	var end *events.AgentEnd
	for _, e := range *list {
		if v, ok := e.(*events.AgentEnd); ok {
			end = v
		}
	}
	if end == nil {
		t.Fatal("no agent_end event")
	}
	if end.Content != "都做完了。" || end.FinishReason != core.FinishReasonStop || end.Error != "" {
		t.Fatalf("agent_end: content=%q finish=%q error=%q", end.Content, end.FinishReason, end.Error)
	}

	// 历史里不得残留引擎注入的门控提醒（提醒落盘过 → 后续每轮都被旧台账污染）。
	for _, m := range ac.Messages {
		for _, c := range m.Content {
			if strings.Contains(c.Content, "Background tasks are still running") ||
				strings.Contains(c.Content, "still has unfinished items") {
				t.Fatalf("收尾门控提醒不应再注入，got: %q", c.Content)
			}
		}
	}
}
