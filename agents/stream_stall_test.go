package agents

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

// 流内停滞预算（StreamStallTimeout，2026-09-22）回归锚点。
//
// 背景（用户反馈）：使用 GLM Provider 时「llm end 前卡住、超过 5 分钟没有任何输出」。
// 既有策略只在「请求发出 → 首个事件」设界（FirstChunkTimeout 30s），流开始后完全不设界
// （刻意：长 reasoning / 大文档生成可达数分钟，设总时限会误杀健康生成）—— 但「不设总时限」
// 不等于「允许永远没有动静」：上游连接僵死时 provider 的 Recv 会一直阻塞，只能等对端 RST
// 或用户手动停止。本策略把「流中段静默超过预算」变成一次**可重试的上游故障**。
//
// 本文件钉住五条：①静默超预算 → 记失败并按上游故障重试 ②静默不超预算的慢流不被打断
//（每个事件重置计时，不界总时长）③首字之前的静默仍归首字预算（两段不抢归因）
// ④默认就是生产值 ⑤压缩调用（run 内的第二个 LLM 调用点）同样受保护。

// stallProvider 首字正常、其后静默挂死的 provider：发出 LLMStart 后阻塞到 ctx 取消
// （模拟上游连接僵死：TCP 不断开、也不再发任何字节）。hangRounds 之后的轮次正常结束。
type stallProvider struct {
	hangRounds int
	calls      int
}

func (p *stallProvider) Stream(ctx context.Context, _ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	call := p.calls
	p.calls++
	if call < p.hangRounds {
		if err := handler(provider.LLMStartEvent{Model: "glm-probe"}); err != nil {
			return err
		}
		<-ctx.Done() // 静默挂死：不返回任何事件，直到看门狗取消 ctx
		return ctx.Err()
	}
	if err := handler(provider.LLMStartEvent{Model: "glm-probe"}); err != nil {
		return err
	}
	return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
}

// trickleProvider 持续产出但块间隔小于预算、总耗时大于预算的 provider：
// 「只界静默、不界总时长」的行为边界（旧口径下这段耗时会超出任何总时限）。
type trickleProvider struct {
	gap    time.Duration
	chunks int
	calls  int
}

func (p *trickleProvider) Stream(ctx context.Context, _ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	p.calls++
	if err := handler(provider.LLMStartEvent{Model: "trickle"}); err != nil {
		return err
	}
	for i := 0; i < p.chunks; i++ {
		select {
		case <-time.After(p.gap):
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := handler(provider.LLMContentDeltaEvent{Delta: "x"}); err != nil {
			return err
		}
	}
	return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "done"})
}

// silentProviderNoFirstEvent 无首字的 provider（复刻首字超时用例的形态）：用于验证
// 「首字之前的静默」仍归首字预算，不会被停滞预算抢走归因。
type silentProviderNoFirstEvent struct{ calls int }

func (p *silentProviderNoFirstEvent) Stream(ctx context.Context, _ *provider.StreamRequest, _ func(e provider.StreamEvent) error) error {
	p.calls++
	<-ctx.Done()
	return ctx.Err()
}

// TestStreamStallRetriesAsUpstreamError 流中段静默超预算 → 记一次失败并重试（可重试的上游故障）。
func TestStreamStallRetriesAsUpstreamError(t *testing.T) {
	p := &stallProvider{hangRounds: 1}
	loop := NewAgentLoop(
		WithProvider(p),
		WithMaxRetries(3),
		WithFirstChunkTimeout(0), // 关掉首字预算：本用例只考察流中段停滞
		WithStreamStallTimeout(30*time.Millisecond),
		WithRetryBackoff(FixedBackoff(time.Millisecond)),
	)

	ac, list := runOnce(t, loop)
	if ac.Err != nil {
		t.Fatalf("静默挂死一轮后应重试成功: %v", ac.Err)
	}
	if p.calls != 2 {
		t.Fatalf("want 2 attempts（第 1 次流中段静默挂死 + 第 2 次成功）, got %d", p.calls)
	}
	errs := collectLLMErrors(&list)
	if len(errs) != 1 {
		t.Fatalf("want 1 llm_error, got %d", len(errs))
	}
	e := errs[0]
	if !e.WillRetry {
		t.Fatalf("流中段静默是上游瞬时故障，必须标记可重试: %+v", e)
	}
	if e.ErrorKind != provider.ErrorKindUpstreamUnavailable {
		t.Fatalf("want kind=%s, got %q", provider.ErrorKindUpstreamUnavailable, e.ErrorKind)
	}
	if !strings.Contains(e.Message, "流式响应停滞") {
		t.Fatalf("错误文案应点明流式停滞（否则用户只看到 context canceled）: %q", e.Message)
	}
	if e.RetryDelayMs <= 0 {
		t.Fatalf("可重试时退避窗口必须 > 0（前端倒计时依据）: %+v", e)
	}
}

// TestStreamStallExhaustedIsUpstream 重试耗尽 → 终态错误仍是流式停滞（可归因上游）。
func TestStreamStallExhaustedIsUpstream(t *testing.T) {
	p := &stallProvider{hangRounds: 5}
	loop := NewAgentLoop(
		WithProvider(p),
		WithMaxRetries(2),
		WithFirstChunkTimeout(0),
		WithStreamStallTimeout(20*time.Millisecond),
		WithRetryBackoff(FixedBackoff(time.Millisecond)),
	)

	ac, list := runOnce(t, loop)
	if ac.Err == nil {
		t.Fatal("want run failure after retries exhausted")
	}
	var se *provider.StreamStallError
	if !errors.As(ac.Err, &se) {
		t.Fatalf("want StreamStallError, got %T: %v", ac.Err, ac.Err)
	}
	if errors.Is(ac.Err, context.Canceled) {
		t.Fatal("停滞不得满足 errors.Is(context.Canceled)：会被判成用户中断而不重试")
	}
	if provider.IsPermanent(ac.Err) {
		t.Fatal("流式停滞不得标永久（永久 = 重试无意义）")
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

// TestStreamStallDoesNotCutTricklingStream 每个事件重置计时 → 块间隔小于预算的慢流
// 无论跑多久都不被打断（总耗时刻意大于预算）。
func TestStreamStallDoesNotCutTricklingStream(t *testing.T) {
	p := &trickleProvider{gap: 40 * time.Millisecond, chunks: 4} // 总 ≈160ms > 预算 100ms
	loop := NewAgentLoop(
		WithProvider(p),
		WithMaxRetries(3),
		WithFirstChunkTimeout(0),
		WithStreamStallTimeout(100*time.Millisecond),
		WithRetryBackoff(FixedBackoff(time.Millisecond)),
	)

	ac, list := runOnce(t, loop)
	if ac.Err != nil {
		t.Fatalf("块间隔小于预算的慢流不得被打断（不界总时长）: %v", ac.Err)
	}
	if p.calls != 1 {
		t.Fatalf("不应重试（未失败），got %d calls", p.calls)
	}
	if errs := collectLLMErrors(&list); len(errs) != 0 {
		t.Fatalf("不应有 llm_error 事件: %+v", errs)
	}
}

// TestStreamStallLeavesFirstChunkWindowAlone 首字之前的静默仍归首字预算：停滞计时从首个
// 事件才开始，一个事件都没到时报的必须是「首字超时」（用户归因不同：网关排队 vs 连接挂死）。
func TestStreamStallLeavesFirstChunkWindowAlone(t *testing.T) {
	p := &silentProviderNoFirstEvent{}
	loop := NewAgentLoop(
		WithProvider(p),
		WithMaxRetries(1), // 不重试：直接看终态错误
		WithFirstChunkTimeout(20*time.Millisecond),
		WithStreamStallTimeout(2*time.Second), // 远大于首字预算：不该轮到它
		WithRetryBackoff(FixedBackoff(time.Millisecond)),
	)

	ac, _ := runOnce(t, loop)
	var ft *provider.FirstChunkTimeoutError
	if !errors.As(ac.Err, &ft) {
		t.Fatalf("零事件挂死必须归因为首字超时, got %T: %v", ac.Err, ac.Err)
	}
	var se *provider.StreamStallError
	if errors.As(ac.Err, &se) {
		t.Fatal("零事件挂死不得归因为流式停滞（此时还没有「首字之后」）")
	}
}

// TestStreamStallDefaultPolicy 默认策略值就是生产值：桌面端（bridge）**不覆盖**
// StreamStallTimeout，因此这条默认值 = 用户实际跑的流内静默上限。改它等于改用户可见行为
// （重试节奏、UI 文案、docs/EXECUTION.md 口径），必须一起改。
func TestStreamStallDefaultPolicy(t *testing.T) {
	if got := NewAgentLoop().cfg.StreamStallTimeout; got != 3*time.Minute {
		t.Fatalf("默认流内停滞预算 = %v, want 3min", got)
	}
	if got := NewAgentLoop(WithStreamStallTimeout(0)).cfg.StreamStallTimeout; got != 0 {
		t.Fatalf("WithStreamStallTimeout(0) 应关闭看门狗, got %v", got)
	}
	if got := NewLLMCompressor(&stallProvider{}, "m").StallTimeout; got != 3*time.Minute {
		t.Fatalf("压缩调用的默认停滞预算 = %v, want 3min（与主会话同值）", got)
	}
}

// TestCompressorFirstChunkTimeoutRetriesAsUpstream 压缩调用同样受首字预算保护
// （2026-10-11 补齐）：压缩器此前只有「首包 60s」（HTTP 层）与停滞预算（从首事件起算），
// 「响应头已回但没有首个流事件」这段无任何一层管得住 —— 而压缩是 run 内部的第二个 LLM
// 调用点，无首字挂死表现为**整个会话静默**（与主对话同因，解法同源）。
func TestCompressorFirstChunkTimeoutRetriesAsUpstream(t *testing.T) {
	// 无首字 provider（连 LLMStart 都没发就阻塞）：首字看门狗必须在 stall 计时之前命中。
	p := &silentProviderNoFirstEvent{calls: 1}
	c := NewLLMCompressor(p, "glm-probe",
		WithCompressTimeout(0),
		WithCompressMaxRetries(1), // 不重试：直接看终态错误
		WithCompressFirstChunkTimeout(30*time.Millisecond),
	)
	if got := c.FirstChunkTimeout; got != 30*time.Millisecond {
		t.Fatalf("WithCompressFirstChunkTimeout 未生效: %v", got)
	}

	_, err := c.Compact(context.Background(), []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "你好"}),
		core.NewAssistantMessage([]core.Content{{Type: core.ContentTypeText, Content: "你好"}}, "", nil),
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "继续"}),
	})
	var fe *provider.FirstChunkTimeoutError
	if !errors.As(err, &fe) {
		t.Fatalf("want FirstChunkTimeoutError, got %T: %v", err, err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatal("压缩首字超时不得满足 errors.Is(context.Canceled)：重试层会判成用户中断而一次都不重试")
	}
	if provider.ClassifyError(err) != provider.ErrorKindUpstreamUnavailable {
		t.Fatalf("ClassifyError = %q, want %q", provider.ClassifyError(err), provider.ErrorKindUpstreamUnavailable)
	}
}

// TestCompressorFirstChunkTimeoutDefaultPolicy 压缩调用默认就带首字预算，且与主对话同值
// （两个 LLM 调用点策略不漂移；此前压缩器该值为 0 = 无首字预算，属策略缺口）。
func TestCompressorFirstChunkTimeoutDefaultPolicy(t *testing.T) {
	if got := NewLLMCompressor(&stallProvider{}, "m").FirstChunkTimeout; got != 90*time.Second {
		t.Fatalf("压缩调用的默认首字预算 = %v, want 90s（与主会话同值）", got)
	}
	if got := NewAgentLoop().cfg.FirstChunkTimeout; got != 90*time.Second {
		t.Fatalf("主会话默认首字预算 = %v, want 90s", got)
	}
}

// TestCompressorStreamStallIsRetryableUpstream 压缩调用（run 内的第二个 LLM 调用点）同样
// 受停滞看门狗保护：自动压缩流中段挂死 = 整个会话静默无输出（与主对话同因，解法同源）。
func TestCompressorStreamStallIsRetryableUpstream(t *testing.T) {
	p := &stallProvider{hangRounds: 5}
	c := NewLLMCompressor(p, "glm-probe",
		WithCompressTimeout(0),
		WithCompressMaxRetries(1), // 不重试：直接看终态错误
		WithCompressStallTimeout(30*time.Millisecond),
	)

	_, err := c.Compact(context.Background(), []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "你好"}),
		core.NewAssistantMessage([]core.Content{{Type: core.ContentTypeText, Content: "你好"}}, "", nil),
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "继续"}),
	})
	var se *provider.StreamStallError
	if !errors.As(err, &se) {
		t.Fatalf("want StreamStallError, got %T: %v", err, err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatal("压缩停滞不得满足 errors.Is(context.Canceled)：重试层会判成用户中断")
	}
	if provider.ClassifyError(err) != provider.ErrorKindUpstreamUnavailable {
		t.Fatalf("ClassifyError = %q, want %q", provider.ClassifyError(err), provider.ErrorKindUpstreamUnavailable)
	}
}
