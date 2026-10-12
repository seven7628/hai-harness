package agents

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
)

// 首字预算（FirstChunkTimeout）回归锚点（2026-09-18）。
//
// 背景（实测）：一次尝试在「无任何首字」状态下静默挂了 187s 才被对端 RST
// （21:39:24 attempt=1 失败 → 21:42:32 attempt=2 失败），期间重试循环停在
// 「重试 1」上，用户看到的是「卡住」。策略落点：**只在「请求发出 → 首个 provider
// 事件」这段设界**，首字一到立即解除；流内不限总时长（长 reasoning/大文档生成不受切）。
//
// 本文件钉住四条：①无首字 → 超时且可重试（不能退化成「用户中断」）②首字已到 →
// 无论流多慢都不超时 ③耗尽 → 终态归因上游 ④用户中断不被误判成首字超时。

// silentProvider 无首字的 provider：前 hangRounds 轮阻塞到 ctx 取消（模拟上游网关
// 排队/挂死），之后各轮立刻首字并正常结束。started 非空时在首次进入 Stream 时关闭
// （响应测试用：确认「已在等首字」再触发中断）。
type silentProvider struct {
	hangRounds int
	calls      int
	started    chan struct{}
}

func (p *silentProvider) Stream(ctx context.Context, _ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	call := p.calls
	p.calls++
	if call == 0 && p.started != nil {
		close(p.started)
	}
	if call < p.hangRounds {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := handler(provider.LLMStartEvent{Model: "ok"}); err != nil {
		return err
	}
	return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
}

// slowStreamProvider 首字立刻到、但流式慢（流内耗时 = delay，可远超首字预算）。
type slowStreamProvider struct {
	delay time.Duration
	calls int
}

func (p *slowStreamProvider) Stream(_ context.Context, _ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	p.calls++
	if err := handler(provider.LLMStartEvent{Model: "slow"}); err != nil {
		return err
	}
	// 流开始后的长耗时：首字预算不应再作用于这段（reasoning/大文档生成的等效物）
	time.Sleep(p.delay)
	if err := handler(provider.LLMContentDeltaEvent{Delta: "done"}); err != nil {
		return err
	}
	return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "done"})
}

func runOnce(t *testing.T, loop *AgentLoop) (*AgentContext, []events.Event) {
	t.Helper()
	handler, list := collectEvents()
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler)
	return ac, *list
}

// TestFirstChunkTimeoutRetriesAsUpstreamError 无首字 → 首字超时 → 记一次失败并重试成功。
func TestFirstChunkTimeoutRetriesAsUpstreamError(t *testing.T) {
	p := &silentProvider{hangRounds: 1}
	loop := NewAgentLoop(
		WithProvider(p),
		WithMaxRetries(3),
		WithFirstChunkTimeout(30*time.Millisecond),
		WithRetryBackoff(FixedBackoff(time.Millisecond)),
	)

	ac, list := runOnce(t, loop)
	if ac.Err != nil {
		t.Fatalf("挂死一轮后应重试成功: %v", ac.Err)
	}
	if p.calls != 2 {
		t.Fatalf("want 2 attempts（第 1 次静默挂死 + 第 2 次成功）, got %d", p.calls)
	}

	errs := collectLLMErrors(&list)
	if len(errs) != 1 {
		t.Fatalf("want 1 llm_error, got %d", len(errs))
	}
	e := errs[0]
	if !e.WillRetry {
		t.Fatalf("首字超时是上游瞬时故障，必须标记可重试: %+v", e)
	}
	if e.Attempt != 1 || e.MaxAttempts != 3 {
		t.Fatalf("attempt/max_attempts 不对（UI 显示「第 1/3 次失败 → 重试第 2/3 次」的依据）: %+v", e)
	}
	if e.ErrorKind != provider.ErrorKindUpstreamUnavailable {
		t.Fatalf("want kind=%s（上游侧归因）, got %q", provider.ErrorKindUpstreamUnavailable, e.ErrorKind)
	}
	if !strings.Contains(e.Message, "首字超时") {
		t.Fatalf("错误文案应点明首字超时（否则用户只看到 context canceled）: %q", e.Message)
	}
	if e.RetryDelayMs <= 0 {
		t.Fatalf("可重试时退避窗口必须 > 0（前端倒计时依据）: %+v", e)
	}
}

// TestFirstChunkTimeoutExhaustedIsUpstream 重试耗尽 → 终态错误仍是首字超时（可归因上游）。
func TestFirstChunkTimeoutExhaustedIsUpstream(t *testing.T) {
	p := &silentProvider{hangRounds: 5}
	loop := NewAgentLoop(
		WithProvider(p),
		WithMaxRetries(2),
		WithFirstChunkTimeout(20*time.Millisecond),
		WithRetryBackoff(FixedBackoff(time.Millisecond)),
	)

	ac, list := runOnce(t, loop)
	if ac.Err == nil {
		t.Fatal("want run failure after retries exhausted")
	}
	var ft *provider.FirstChunkTimeoutError
	if !errors.As(ac.Err, &ft) {
		t.Fatalf("want FirstChunkTimeoutError, got %T: %v", ac.Err, ac.Err)
	}
	if provider.IsPermanent(ac.Err) {
		t.Fatal("首字超时不得标永久（永久 = 重试无意义）")
	}
	if got := provider.ClassifyError(ac.Err); got != provider.ErrorKindUpstreamUnavailable {
		t.Fatalf("ClassifyError = %q, want %q", got, provider.ErrorKindUpstreamUnavailable)
	}
	if p.calls != 2 {
		t.Fatalf("want 2 attempts（MaxRetries=2 含首次）, got %d", p.calls)
	}
	errs := collectLLMErrors(&list)
	if len(errs) != 2 {
		t.Fatalf("want 2 llm_error events, got %d", len(errs))
	}
	if !errs[0].WillRetry || errs[1].WillRetry {
		t.Fatalf("最后一次不得标记可重试: %+v", errs)
	}
}

// TestFirstChunkTimeoutReleasedAfterFirstEvent 首字已到 → 流内耗时远超预算也不超时。
// 这是「与整轮总时限的区别」的行为边界：长 reasoning / 大文档生成不被切。
func TestFirstChunkTimeoutReleasedAfterFirstEvent(t *testing.T) {
	p := &slowStreamProvider{delay: 120 * time.Millisecond} // 6× 首字预算
	loop := NewAgentLoop(
		WithProvider(p),
		WithMaxRetries(3),
		WithFirstChunkTimeout(20*time.Millisecond),
		WithRetryBackoff(FixedBackoff(time.Millisecond)),
	)

	ac, list := runOnce(t, loop)
	if ac.Err != nil {
		t.Fatalf("流开始后不限总时长：慢流不应被打断: %v", ac.Err)
	}
	if p.calls != 1 {
		t.Fatalf("不应重试（未失败），got %d calls", p.calls)
	}
	if errs := collectLLMErrors(&list); len(errs) != 0 {
		t.Fatalf("不应有 llm_error 事件: %+v", errs)
	}
}

// TestFirstChunkTimeoutNotRetriedAfterAbort 首字等待期间用户中断 → 不重试、不误判成首字超时。
// 语义：中断不是失败（abort 优先于任何超时判定，且不得因看门狗而多打一次尝试）。
func TestFirstChunkTimeoutNotRetriedAfterAbort(t *testing.T) {
	p := &silentProvider{hangRounds: 1, started: make(chan struct{})}
	loop := NewAgentLoop(
		WithProvider(p),
		WithMaxRetries(3),
		WithFirstChunkTimeout(30*time.Millisecond),
		WithRetryBackoff(FixedBackoff(time.Millisecond)),
	)

	handler, list := collectEvents()
	done := make(chan struct{})
	var ac *AgentContext
	go func() {
		ac = loop.RunStream(context.Background(), []core.Message{
			core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
		}, handler, &RunOptions{OnRunning: func(r *AgentContext) { ac = r }})
		close(done)
	}()

	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never started")
	}
	ac.Abort() // 等价 Session.Interrupt：等待首字期间点「停止」

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("abort during first-chunk wait did not terminate the run")
	}
	// 中断不是失败：AgentEnd.FinishReason=abort，ac.Err 保持 nil（既有语义）
	if !ac.IsAborted() {
		t.Fatal("abort 未生效")
	}
	if ac.Err != nil {
		t.Fatalf("中断不应记成运行失败: %v", ac.Err)
	}
	if p.calls != 1 {
		t.Fatalf("中断后不得再发起尝试（看门狗不得把它变成重试）, got %d calls", p.calls)
	}
	// 取消来源必须细分（2026-09-24）：用户点停止 → aborted，不是 canceled。
	// 两者共用 canceled 时 UI 只能输出「重试已耗尽」（其实第 1 次就终止，一次都没重试）。
	errs := collectLLMErrors(list)
	if len(errs) != 1 {
		t.Fatalf("中断应恰好发一条 llm_error, got %d: %+v", len(errs), errs)
	}
	if errs[0].ErrorKind != provider.ErrorKindAborted {
		t.Fatalf("用户中断的 error_kind = %q, want %q（前端据此输出「已中断」而非「重试已耗尽」）",
			errs[0].ErrorKind, provider.ErrorKindAborted)
	}
	if errs[0].WillRetry || errs[0].Attempt != 1 {
		t.Fatalf("中断不应重试: attempt=%d will_retry=%v", errs[0].Attempt, errs[0].WillRetry)
	}
	for _, e := range errs {
		if e.ErrorKind == provider.ErrorKindUpstreamUnavailable || strings.Contains(e.Message, "首字超时") {
			t.Fatalf("中断不得被计成首字超时/上游故障: %+v", e)
		}
	}
}

// TestLLMErrorKindCanceledWithoutAbort 系统侧取消（会话关闭 / 后台任务终止）→ canceled。
// 与用户中断同为 context.Canceled 且同样不重试，但用户没按停止，文案必须区分：
// 归因给用户（「已中断」）是错的，说「重试已耗尽」更是错的（一次都没重试）。
func TestLLMErrorKindCanceledWithoutAbort(t *testing.T) {
	p := &silentProvider{hangRounds: 1, started: make(chan struct{})}
	loop := NewAgentLoop(
		WithProvider(p),
		WithMaxRetries(3),
		WithFirstChunkTimeout(0), // 关掉看门狗：本轮只由外部 ctx 取消终止
		WithRetryBackoff(FixedBackoff(time.Millisecond)),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler, list := collectEvents()
	done := make(chan struct{})
	go func() {
		loop.RunStream(ctx, []core.Message{
			core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
		}, handler, nil)
		close(done)
	}()

	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never started")
	}
	cancel() // 等价 Session.Close → sessionCancel：直接取消 ctx，不置 abort 标志

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消未终止运行")
	}
	errs := collectLLMErrors(list)
	if len(errs) != 1 {
		t.Fatalf("应恰好发一条 llm_error, got %d: %+v", len(errs), errs)
	}
	if errs[0].ErrorKind != provider.ErrorKindCanceled {
		t.Fatalf("系统取消的 error_kind = %q, want %q（不能归因成用户中断）",
			errs[0].ErrorKind, provider.ErrorKindCanceled)
	}
	if p.calls != 1 {
		t.Fatalf("ctx 已死时不得再发起尝试, got %d calls", p.calls)
	}
}

// TestFirstChunkTimeoutDefaultPolicy 默认策略值就是生产缺省：30s → 90s（2026-10-11 用户
// 反馈后调宽，同 suite 上游注释）。桌面端（bridge）经 agentSettings.first_chunk_timeout
// **可覆盖**该默认值，未配置时才落在这里 —— 所以这个默认值 = 未配置用户实际跑的首字预算。
// 改它等于改用户可见行为（重试节奏、UI 文案、docs/EXECUTION.md §3.5 的口径），必须一起改。
func TestFirstChunkTimeoutDefaultPolicy(t *testing.T) {
	if got := NewAgentLoop().cfg.FirstChunkTimeout; got != 90*time.Second {
		t.Fatalf("默认首字预算 = %v, want 90s", got)
	}
	// 逃生门：显式 0 = 关掉看门狗（不想要兜底的调用方）
	if got := NewAgentLoop(WithFirstChunkTimeout(0)).cfg.FirstChunkTimeout; got != 0 {
		t.Fatalf("WithFirstChunkTimeout(0) 应关闭看门狗, got %v", got)
	}
}

// TestFirstChunkTimeoutSetAfterConstruct 运行期热改生效（bridge 的
// set_first_chunk_timeout / reload_settings 路径依赖它）：装配后不重建 loop 即改预算，
// 下一次 attempt 读到新值；getter 与内部加锁读一致（防 streamOnce 读到旧值）。
func TestFirstChunkTimeoutSetAfterConstruct(t *testing.T) {
	loop := NewAgentLoop(WithProvider(&silentProvider{hangRounds: 1}))
	if got := loop.FirstChunkTimeout(); got != 90*time.Second {
		t.Fatalf("装配后 getter = %v, want 90s", got)
	}
	loop.SetFirstChunkTimeout(180 * time.Second)
	if got := loop.FirstChunkTimeout(); got != 180*time.Second {
		t.Fatalf("SetFirstChunkTimeout(180s) 后 getter = %v", got)
	}
	if got := loop.currentFirstChunkTimeout(); got != 180*time.Second {
		t.Fatalf("currentFirstChunkTimeout = %v, want 180s（streamOnce 读的是这个）", got)
	}
	// 负数按 0（关看门狗）—— 不出现「负预算立即超时」的荒谬语义
	loop.SetFirstChunkTimeout(-time.Second)
	if got := loop.currentFirstChunkTimeout(); got != 0 {
		t.Fatalf("负预算应归 0, got %v", got)
	}
}
