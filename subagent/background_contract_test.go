package subagent

import (
	"context"
	"errors"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// safeEvents 线程安全事件收集器：后台任务（独立 goroutine）并发写、测试读快照。
type safeEvents struct {
	mu   sync.Mutex
	list []events.Event
}

func (s *safeEvents) handler() events.EventHandler {
	return func(_ context.Context, e events.Event) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.list = append(s.list, e)
	}
}

func (s *safeEvents) snapshot() []events.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]events.Event(nil), s.list...)
}

// countingProvider 统计 Stream 调用次数（断言「上下文超限零重试」）。
type countingProvider struct {
	mockProvider
	calls atomic.Int64
}

func (c *countingProvider) Stream(ctx context.Context, req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	c.calls.Add(1)
	return c.mockProvider.Stream(ctx, req, handler)
}

// TestBackgroundOrderContract 后台任务事件顺序契约（task_started 占位先于 agent_start）+
// AgentStart 携带 task_id：任务卡片可确定性关联（停止按钮 / 结果回填 / 去重），不再依赖事件到达顺序。
func TestBackgroundOrderContract(t *testing.T) {
	childLoop := agents.NewAgentLoop(agents.WithProvider(&mockProvider{rounds: []func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
		func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "结论"})
		},
	}}))
	reg := NewRegistry()
	reg.SetSessionCtx(context.Background()) // Wait 用
	defer reg.Close(context.Background())
	tool := NewBackgroundTool(Spec{
		Name: "subagent_explore", Description: "d", Loop: childLoop, Background: true,
	}, func() context.Context { return context.Background() }, reg)

	evs := &safeEvents{}
	out, err := tool.Call(events.WithToolContext(context.Background(), "parent-run", 0, evs.handler(), nil, nil), "", `{"name":"explore","task":"分析 X"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"task_id"`) {
		t.Fatalf("background call should return task id: %s", out)
	}

	// 等待任务完成（TaskEnd 已入事件流）
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hasTaskEnd(evs.snapshot(), "") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	var tsIdx, asIdx = -1, -1
	var ts *events.TaskStarted
	var as *events.AgentStart
	for i, e := range evs.snapshot() {
		if s, ok := e.(*events.TaskStarted); ok {
			if tsIdx < 0 {
				tsIdx = i
				ts = s
			}
		}
		if s, ok := e.(*events.AgentStart); ok && s.ParentRunId != "" {
			if asIdx < 0 {
				asIdx = i
				as = s
			}
		}
	}
	if ts == nil || ts.TaskId == "" {
		t.Fatal("task_started 应携带 task_id")
	}
	if as == nil || as.TaskId == "" {
		t.Fatalf("agent_start 应携带 task_id（与 task_started 同源）: %+v", as)
	}
	if ts.TaskId != as.TaskId {
		t.Fatalf("task_id 不一致: task_started=%q agent_start=%q", ts.TaskId, as.TaskId)
	}
	// 顺序契约：占位先于任何子运行事件（前端停止按钮/卡片关联的前提）
	if tsIdx < 0 || asIdx < 0 || tsIdx > asIdx {
		t.Fatalf("task_started (%d) 必须先于 agent_start (%d)", tsIdx, asIdx)
	}
	if as.ParentRunId != "parent-run" || as.Depth != 1 {
		t.Fatalf("agent_start 应关联父 run: %+v", as)
	}
}

// hasTaskEnd 事件流内是否已有指定状态的 TaskEnd（status 空 = 任意）。
func hasTaskEnd(list []events.Event, status string) bool {
	for _, e := range list {
		if te, ok := e.(*events.TaskEnd); ok && (status == "" || te.Status == status) {
			return true
		}
	}
	return false
}

// TestBackgroundTaskContextExceededFails 后台任务上下文超限语义：
// 零重试 + 任务 failed + TaskEnd.Error 为合成"为什么结束/怎么办"文案 +
// AgentEnd/LLMError 携带 error_kind=context_exceeded（前端专门提示的数据源）。
func TestBackgroundTaskContextExceededFails(t *testing.T) {
	p := &countingProvider{mockProvider: mockProvider{rounds: []func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
		func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			// 裸错误文本（模拟未分类的 provider）：AgentLoop 层 DetectContextExceeded 兜底
			return errors.New("This model's maximum context length is 131072 tokens. However, you requested 150000 tokens.")
		},
	}}}
	childLoop := agents.NewAgentLoop(agents.WithProvider(p), agents.WithMaxRetries(10))
	reg := NewRegistry()
	reg.SetSessionCtx(context.Background())
	defer reg.Close(context.Background())
	tool := NewBackgroundTool(Spec{
		Name: "subagent_explore", Description: "d", Loop: childLoop, Background: true,
	}, func() context.Context { return context.Background() }, reg)

	evs := &safeEvents{}
	_, err := tool.Call(events.WithToolContext(context.Background(), "parent-run", 0, evs.handler(), nil, nil), "", `{"name":"explore","task":"分析超长任务"}`)
	if err != nil {
		t.Fatal(err)
	}

	// 任务终态：failed + 合成错误
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hasTaskEnd(evs.snapshot(), "failed") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !hasTaskEnd(evs.snapshot(), "failed") {
		t.Fatalf("task 应为 failed: %+v", evs.snapshot())
	}

	// 零重试：MaxRetries=10 也必须只调 1 次
	if got := p.calls.Load(); got != 1 {
		t.Fatalf("context exceeded must not retry: provider called %d times, want 1", got)
	}

	// 事件链：LLMError(error_kind=context_exceeded, will_retry=false) →
	// AgentEnd(error_kind) → TaskEnd(Error 合成文案)
	var llmErr *events.LLMError
	var agentEnd *events.AgentEnd
	var taskEnd *events.TaskEnd
	for _, e := range evs.snapshot() {
		switch v := e.(type) {
		case *events.LLMError:
			llmErr = v
		case *events.AgentEnd:
			agentEnd = v
		case *events.TaskEnd:
			taskEnd = v
		}
	}
	if llmErr == nil || llmErr.WillRetry || llmErr.ErrorKind != provider.ErrorKindContextExceeded {
		t.Fatalf("llm_error 应携带 error_kind=context_exceeded 且不重试: %+v", llmErr)
	}
	if agentEnd == nil || agentEnd.ErrorKind != provider.ErrorKindContextExceeded || agentEnd.Error == "" {
		t.Fatalf("agent_end 应携带 context_exceeded 错误: %+v", agentEnd)
	}
	if taskEnd == nil || taskEnd.Error == "" {
		t.Fatalf("task_end 应携带错误: %+v", taskEnd)
	}
	if !strings.Contains(taskEnd.Error, "上下文超限") || !strings.Contains(taskEnd.Error, "建议") {
		t.Fatalf("task_end.Error 应为合成说明（含原因与建议）: %s", taskEnd.Error)
	}

	// Registry 终态：failed + 可 Wait 出合成错误（宿主回传主会话的数据源）
	var taskID string
	for _, ti := range reg.List() {
		taskID = ti.ID
		if ti.Status != TaskFailed {
			t.Fatalf("registry 状态应为 failed: %+v", ti)
		}
	}
	if taskID == "" {
		t.Fatal("registry 应有任务记录")
	}
	_, waitErr := reg.Wait(taskID)
	var ce *provider.ContextExceededError
	if !errors.As(waitErr, &ce) {
		t.Fatalf("Wait 应返回 ContextExceededError, got %v", waitErr)
	}
	if !strings.Contains(waitErr.Error(), "131072") {
		t.Fatalf("合成文案应含解析出的窗口数字: %s", waitErr.Error())
	}
}

// TestStartToolWithOnStartedBeforeRun onStarted 必须在任务 goroutine 启动前同步回调：
// 任务体运行时必然已见 onStarted 信号（无时序窗口）。
func TestStartToolWithOnStartedBeforeRun(t *testing.T) {
	reg := NewRegistry()
	reg.SetSessionCtx(context.Background())
	defer reg.Close(context.Background())
	onStartedCh := make(chan string, 1)
	runSeen := make(chan string, 1)

	_, err := reg.StartToolWithOnStarted(context.Background(), "agent_spawn", "review", func(taskID string) {
		onStartedCh <- taskID
	}, func(_ context.Context, tk *Task) (string, core.Usage, error) {
		select {
		case id := <-onStartedCh:
			runSeen <- id
		case <-time.After(2 * time.Second):
			runSeen <- "MISSING"
		}
		return "done", core.Usage{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-runSeen:
		if id == "MISSING" {
			t.Fatal("run 启动时 onStarted 尚未回调（顺序契约被破坏）")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run 未执行")
	}
}
