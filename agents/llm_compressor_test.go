package agents

import (
	"context"
	"errors"
	"github.com/seven7628/hai-harness/agents/prompt"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// compressProvider 可控的压缩 provider：捕获请求，按预设返回摘要/用量/错误。
type compressProvider struct {
	req     *provider.StreamRequest
	content string
	usage   core.Usage
	model   string
	err     error
}

func (p *compressProvider) Stream(_ context.Context, req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	p.req = req
	if p.err != nil {
		return p.err
	}
	return handler(provider.LLMEndEvent{
		FinishReason: core.FinishReasonStop,
		Content:      p.content,
		Usage:        p.usage,
		Model:        p.model,
	})
}

// TestLLMCompressorCompact 验证基本压缩：输出 = [SystemPrompt 原样, 摘要消息(summary 类型),
// 最新用户输入原样保留]；Model/Usage 透传；压缩请求 = [压缩指令, 历史渲染（不含最新输入）]。
func TestLLMCompressorCompact(t *testing.T) {
	p := &compressProvider{content: "# 会话摘要\n\n## 任务目标\n- 目标", model: "m1",
		usage: core.Usage{Input: 100, Output: 20, TotalTokens: 120}}
	c := NewLLMCompressor(p, "m1")

	input := []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewUserMessage(core.Content{Type: "text", Content: "你好"}),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "你好"}}, "", nil),
		core.NewUserMessage(core.Content{Type: "text", Content: "帮我查资料"}),
	}
	result, err := c.Compact(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}

	// 压缩请求：system=压缩指令，user=历史渲染（最新用户输入被保留，不进压缩输入）
	if len(p.req.Messages) != 2 || p.req.Messages[0].Role != core.System {
		t.Fatalf("compress request should be [prompt, history]: %+v", p.req.Messages)
	}
	if !strings.Contains(p.req.Messages[1].Content[0].Content, "你好") {
		t.Fatalf("history should be rendered: %q", p.req.Messages[1].Content[0].Content)
	}
	if strings.Contains(p.req.Messages[1].Content[0].Content, "帮我查资料") {
		t.Fatalf("latest user input must not enter compress input: %q", p.req.Messages[1].Content[0].Content)
	}
	if p.req.Config.MaxTokens != defaultSummaryMaxTokens {
		t.Fatalf("want default max tokens, got %d", p.req.Config.MaxTokens)
	}
	// 摘要档位固定 high（2026-09 用户决策）：不随会话档位漂移。
	if p.req.Config.Effort != provider.ReasoningEffortLevelHigh {
		t.Fatalf("summary effort = %q, want high", p.req.Config.Effort)
	}

	// 输出结构：[system 原样, summary 摘要, 任务锚点注记(首条 user 原文逐字保留), 最新用户输入原样]
	if len(result.Messages) != 4 {
		t.Fatalf("want [system, summary, anchor, latest user], got %d messages", len(result.Messages))
	}
	if result.Messages[0].Role != core.System || result.Messages[0].Content[0].Content != "你是助手" {
		t.Fatalf("system prompt should be preserved: %+v", result.Messages[0])
	}
	s := result.Messages[1]
	if s.Role != core.System || s.Content[0].Type != "summary" {
		t.Fatalf("summary message must be system + summary type: %+v", s)
	}
	if !strings.Contains(s.Content[0].Content, "# 会话摘要") {
		t.Fatalf("summary should be structured markdown: %q", s.Content[0].Content)
	}
	if !strings.Contains(result.Messages[2].Content[0].Content, "你好") {
		t.Fatalf("task anchor should preserve the first user message verbatim: %+v", result.Messages[2])
	}
	u := result.Messages[3]
	if u.Role != core.User || u.Content[0].Content != "帮我查资料" {
		t.Fatalf("latest user input should be preserved verbatim: %+v", u)
	}
	if result.Model != "m1" || result.Usage.Input != 100 {
		t.Fatalf("model/usage should pass through: %+v", result)
	}
}

// TestLLMCompressorRolling 验证滚动合并：旧摘要（summary 类型）作为「已有摘要」
// 参与下次压缩，SystemPrompt 保留，最新用户输入原样保留。
func TestLLMCompressorRolling(t *testing.T) {
	p := &compressProvider{content: "# 会话摘要\n## 任务目标\n- 合并后"}
	c := NewLLMCompressor(p, "m1")

	input := []core.Message{
		core.NewSystemMessage("你是助手"),
		{Role: core.System, Content: []core.Content{{Type: "summary", Content: "# 会话摘要\n\n## 任务目标\n- 旧目标"}}},
		core.NewUserMessage(core.Content{Type: "text", Content: "继续"}),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "好的"}}, "", nil),
	}
	result, err := c.Compact(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}

	req := p.req.Messages[1].Content[0].Content
	if !strings.Contains(req, "<existing_summary>") || !strings.Contains(req, "旧目标") || !strings.Contains(req, "好的") {
		t.Fatalf("rolling input should merge old summary + new history: %q", req)
	}
	// 最新用户输入不进 <conversation_history>；但作为任务锚点逐字进 <task_anchor>
	// 是 C1 预期行为（Main Requests 逐字引用的事实来源，防锚点被滚动合并稀释）
	if strings.Contains(req, "[user] 继续") {
		t.Fatalf("latest user input must not enter compress history: %q", req)
	}
	if !strings.Contains(req, "<task_anchor>\n") || !strings.Contains(req, "</task_anchor>") {
		t.Fatalf("task anchor block must be present (C1): %q", req)
	}
	// 输出：[system, summary 合并后, user 继续]
	if len(result.Messages) != 3 {
		t.Fatalf("want [system, summary, latest user], got %d", len(result.Messages))
	}
	if result.Messages[0].Content[0].Content != "你是助手" {
		t.Fatalf("system prompt must survive rolling: %+v", result.Messages[0])
	}
	if result.Messages[2].Role != core.User || result.Messages[2].Content[0].Content != "继续" {
		t.Fatalf("latest user input must survive rolling: %+v", result.Messages[2])
	}
}

func TestLLMCompressorEmptyHistory(t *testing.T) {
	c := NewLLMCompressor(&compressProvider{content: "x"}, "m1")
	if _, err := c.Compact(context.Background(), []core.Message{core.NewSystemMessage("你是助手")}); err == nil {
		t.Fatal("want error when nothing to compress")
	}
	// 仅有最新用户输入（无历史）同样无可压缩：保留输入，不产生摘要
	if _, err := c.Compact(context.Background(), []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
	}); err == nil {
		t.Fatal("want error when only latest user input exists")
	}
}

func TestLLMCompressorEmptySummary(t *testing.T) {
	c := NewLLMCompressor(&compressProvider{content: "   "}, "m1")
	if _, err := c.Compact(context.Background(), []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "好"}}, "", nil),
	}); err == nil {
		t.Fatal("want error for empty summary")
	}
}

func TestLLMCompressorProviderError(t *testing.T) {
	c := NewLLMCompressor(&compressProvider{err: errors.New("boom")}, "m1")
	if _, err := c.Compact(context.Background(), []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "好"}}, "", nil),
	}); err == nil {
		t.Fatal("want provider error propagated")
	}
}

// TestLLMCompressorLatestUserBlock 验证最后一段连续 user 消息块整体原样保留
// （连续 Ask 聚合的批在上下文中天然连续——用户最近一次输入的完整批）。
func TestLLMCompressorLatestUserBlock(t *testing.T) {
	p := &compressProvider{content: "# 会话摘要"}
	c := NewLLMCompressor(p, "m1")

	input := []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewUserMessage(core.Content{Type: "text", Content: "第一问"}),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "答"}}, "", nil),
		core.NewUserMessage(core.Content{Type: "text", Content: "追问1"}),
		core.NewUserMessage(core.Content{Type: "text", Content: "追问2"}),
	}
	result, err := c.Compact(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}

	// 输出保留整块连续 user：追问1 + 追问2；第一问属于历史进压缩输入，
	// 且作为任务锚点注入（[system, summary, anchor, user, user]）
	if len(result.Messages) != 5 {
		t.Fatalf("want [system, summary, anchor, user, user], got %d", len(result.Messages))
	}
	if result.Messages[3].Content[0].Content != "追问1" || result.Messages[4].Content[0].Content != "追问2" {
		t.Fatalf("latest user block should be preserved whole: %+v", result.Messages[3:])
	}
	if !strings.Contains(result.Messages[2].Content[0].Content, "第一问") {
		t.Fatalf("task anchor should preserve the first user message: %+v", result.Messages[2])
	}
	if !strings.Contains(p.req.Messages[1].Content[0].Content, "第一问") {
		t.Fatalf("earlier user message should enter compress input: %q", p.req.Messages[1].Content[0].Content)
	}
}

// TestLLMCompressorShouldCompact 验证触发边界：预估 >= 预算触发。
func TestLLMCompressorShouldCompact(t *testing.T) {
	c := NewLLMCompressor(&compressProvider{}, "m1")
	if c.ShouldCompact(context.Background(), CompressStats{Budget: 100, EstimatedTokens: 99}) {
		t.Fatal("below budget must not compact")
	}
	if !c.ShouldCompact(context.Background(), CompressStats{Budget: 100, EstimatedTokens: 100}) {
		t.Fatal("at budget must compact")
	}
	if c.ShouldCompact(context.Background(), CompressStats{Budget: 0, EstimatedTokens: 999}) {
		t.Fatal("zero budget disables compaction")
	}
}

// TestEstimateTokens 验证预估：首轮全量字符/4 兜底；有真实 usage 后 = 真实值本身
// （不叠加新增消息估算——压缩触发只信真实 usage，杜绝预估误差）。
func TestEstimateTokens(t *testing.T) {
	msg := func(s string) core.Message {
		return core.NewUserMessage(core.Content{Type: "text", Content: s})
	}

	ac := &AgentContext{Messages: []core.Message{msg(strings.Repeat("a", 16))}, est: &estimator{}}
	if got := ac.est.estimate(ac.Messages); got != 4 {
		t.Fatalf("first round: want 16/4=4, got %d", got)
	}

	ac.est.observe(100)
	ac.Messages = append(ac.Messages, msg(strings.Repeat("b", 16)))
	if got := ac.est.estimate(ac.Messages); got != 100 {
		t.Fatalf("usage-only: want 100 (real input, no delta), got %d", got)
	}
}

// TestCompressCostAccounted 验证压缩成本计入运行结算 + CompressEnd 事件携带用量。
func TestCompressCostAccounted(t *testing.T) {
	comp := &costCompressor{}
	p := &mockProvider{rounds: []func(handler func(e provider.StreamEvent) error) error{
		func(handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
		},
	}}
	loop := NewAgentLoop(WithProvider(p), WithCompressor(comp), WithContextWindow(125))

	handler, list := collectEvents()
	ac := loop.Run(context.Background(), []core.Message{
		core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
	}, handler)

	if ac.Usage.Input != 100 || ac.Usage.Output != 50 || ac.Usage.Cost.Total != 30 {
		t.Fatalf("compress usage should be accounted: %+v", ac.Usage)
	}

	var end *events.CompressEnd
	for _, e := range *list {
		if ce, ok := e.(*events.CompressEnd); ok {
			end = ce
		}
	}
	if end == nil || end.Model != "m1" || end.Usage == nil || end.Usage.Cost.Total != 30 {
		t.Fatalf("compress_end should carry model/usage: %+v", end)
	}
}

// TestCompressEndCtxTokens 验证 CompressEnd 携带压缩后上下文大小估算（CtxTokens，
// 前端 ctx 环「压缩后大小」的数据源）：
//   - 压缩成功：CtxTokens > 0 且远小于压前输入（压缩的大小不被忽略）；
//   - 压缩失败：上下文不变，CtxTokens = 当前（未变小）上下文估算。
func TestCompressEndCtxTokens(t *testing.T) {
	input := []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: strings.Repeat("c", 10000)})}
	preInput := int64(float64(messagesChars(input)) / 4) // ≈2500 token
	stop := func(handler func(e provider.StreamEvent) error) error {
		return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
	}

	// 压缩成功：costCompressor 保留 messages[:1]（system）→ 压后上下文极小
	p := &mockProvider{rounds: []func(handler func(e provider.StreamEvent) error) error{stop}}
	loop := NewAgentLoop(WithProvider(p), WithCompressor(&costCompressor{}), WithContextWindow(125))
	handler, list := collectEvents()
	_ = loop.Run(context.Background(), input, handler)

	var end *events.CompressEnd
	for _, e := range *list {
		if ce, ok := e.(*events.CompressEnd); ok {
			end = ce
		}
	}
	if end == nil {
		t.Fatal("compress_end should be emitted")
	}
	if end.CtxTokens <= 0 {
		t.Fatalf("compressed context size must not be ignored (0), got %d", end.CtxTokens)
	}
	if end.CtxTokens >= preInput {
		t.Fatalf("post-compression CtxTokens (%d) must be < pre-compression input (~%d)", end.CtxTokens, preInput)
	}

	// 压缩失败：上下文不变，CtxTokens = 当前（未变小）上下文估算
	p2 := &mockProvider{rounds: []func(handler func(e provider.StreamEvent) error) error{stop}}
	loop2 := NewAgentLoop(WithProvider(p2), WithCompressor(&failCompressor{}), WithContextWindow(125))
	handler2, list2 := collectEvents()
	_ = loop2.Run(context.Background(), input, handler2)

	var end2 *events.CompressEnd
	for _, e := range *list2 {
		if ce, ok := e.(*events.CompressEnd); ok {
			end2 = ce
		}
	}
	if end2 == nil || end2.Error == "" {
		t.Fatalf("failed compress_end should carry error: %+v", end2)
	}
	if end2.CtxTokens <= preInput {
		t.Fatalf("failed event should reflect unchanged (large) context, got %d", end2.CtxTokens)
	}
}

// TestCompressFailureDegrades 验证失败降级：上下文不变，事件带错误。
func TestCompressFailureDegrades(t *testing.T) {
	comp := &failCompressor{}
	p := &mockProvider{rounds: []func(handler func(e provider.StreamEvent) error) error{
		func(handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
		},
	}}
	loop := NewAgentLoop(WithProvider(p), WithCompressor(comp), WithContextWindow(125))

	handler, list := collectEvents()
	input := []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}
	ac := loop.Run(context.Background(), input, handler)

	// system（默认 base 注入）在前，原输入原样保留
	if len(ac.Messages) != 2 || ac.Messages[0].Role != core.System || ac.Messages[1].Content[0].Content != "hi" {
		t.Fatalf("context must be preserved on failure: %+v", ac.Messages)
	}
	var end *events.CompressEnd
	for _, e := range *list {
		if ce, ok := e.(*events.CompressEnd); ok {
			end = ce
		}
	}
	if end == nil || end.Error == "" || end.Before != end.After {
		t.Fatalf("compress_end should carry error with before==after: %+v", end)
	}
}

type costCompressor struct{}

func (costCompressor) ShouldCompact(_ context.Context, stats CompressStats) bool {
	return true
}

func (costCompressor) Compact(_ context.Context, messages []core.Message) (CompressResult, error) {
	return CompressResult{
		Messages: messages[:1],
		Model:    "m1",
		Usage:    core.Usage{Input: 100, Output: 50, TotalTokens: 150, Cost: core.Cost{Total: 30}},
	}, nil
}

type failCompressor struct{}

func (failCompressor) ShouldCompact(_ context.Context, stats CompressStats) bool {
	return true
}

func (failCompressor) Compact(context.Context, []core.Message) (CompressResult, error) {
	return CompressResult{}, errors.New("boom")
}

// blockingStreamProvider 阻塞在流式调用中直到 ctx 取消（模拟网络挂住）。
type blockingStreamProvider struct{}

func (blockingStreamProvider) Stream(ctx context.Context, _ *provider.StreamRequest, _ func(e provider.StreamEvent) error) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestLLMCompressorTimeout 验证压缩调用超时：阻塞 provider 在超时后返回错误，
// 整个 agent 不被挂死（AgentLoop 侧降级为不压缩）。
func TestLLMCompressorTimeout(t *testing.T) {
	c := NewLLMCompressor(blockingStreamProvider{}, "m1", WithCompressTimeout(100*time.Millisecond))
	start := time.Now()
	_, err := c.Compact(context.Background(), []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "好"}}, "", nil),
	})
	if err == nil {
		t.Fatal("want timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("compress must not hang: took %v", elapsed)
	}
}

// TestSummaryPromptSections 验证摘要输出结构（10 个 section，防回归）。
func TestSummaryPromptSections(t *testing.T) {
	for _, s := range []string{
		"Main Requests and Intent", "Key Decisions", "File Changes Summary", "Fixed Issues",
		"User Inputs", "Open Tasks", "Current Task", "Background Tasks",
		"Suggested Next Steps", "File Context",
	} {
		if !strings.Contains(prompt.SummaryPrompt, "## "+s) {
			t.Fatalf("summary prompt must contain section %q", s)
		}
	}
}

// TestLLMCompressorLatestUserOversized 验证超长最新输入并入压缩：
// 宁可压缩输入也不原样保留撑爆窗口，输出回到 [system, summary]。
func TestLLMCompressorLatestUserOversized(t *testing.T) {
	p := &compressProvider{content: "# 会话摘要"}
	c := NewLLMCompressor(p, "m1", WithLatestUserMaxTokens(100))

	input := []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewUserMessage(core.Content{Type: "text", Content: "历史问题"}),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "答"}}, "", nil),
		core.NewUserMessage(core.Content{Type: "text", Content: strings.Repeat("x", 2000)}),
	}
	result, err := c.Compact(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	// 超限并入：输出 [system, summary, anchor]（无独立 user 块）
	if len(result.Messages) != 3 {
		t.Fatalf("oversized input must merge into summary, got %d messages", len(result.Messages))
	}
	// 任务锚点仍逐字保留（即使最新输入并入压缩，任务目标不丢）
	if !strings.Contains(result.Messages[2].Content[0].Content, "历史问题") {
		t.Fatalf("task anchor should survive oversized input merge: %+v", result.Messages[2])
	}
	// 超长输入确实进入了压缩输入（摘要 LLM 可见其要点）
	if !strings.Contains(p.req.Messages[1].Content[0].Content, strings.Repeat("x", 2000)) {
		t.Fatalf("oversized input should enter compress input: %q", p.req.Messages[1].Content[0].Content[:60])
	}
}

// TestEstimateTokensReasoningAndTools 验证预估包含回传的 Reasoning 与工具调用参数
// （两者都占输入 token，漏算会低估导致压缩触发偏晚）。
func TestEstimateTokensReasoningAndTools(t *testing.T) {
	am := core.NewAssistantMessage(
		[]core.Content{{Type: "text", Content: strings.Repeat("a", 8)}}, // content 8
		strings.Repeat("r", 8), // reasoning 8
		[]core.ToolCall{{Name: "read_file", Arguments: strings.Repeat("p", 16)}}, // name 9 + args 16
	)
	// 字符统计：8 + 8 + 9 + 16 = 41
	if got := messageChars(am); got != 41 {
		t.Fatalf("want 41 (content+reasoning+tool calls), got %d", got)
	}
}

// TestExtractSummary 双层输出剥离：提取 <summary>…</summary> 内容,丢弃 <analysis> 思考过程。
func TestExtractSummary(t *testing.T) {
	// 标准双层：analysis 思考 + summary 最终摘要 → 只保留 summary 内容
	doubled := `<analysis>
Some thinking that should be dropped.
</analysis>

<summary>
# Session Summary

## Key Decisions
- Decision A
</summary>`
	got := extractSummary(doubled)
	if strings.Contains(got, "thinking") || !strings.Contains(got, "Session Summary") || !strings.Contains(got, "Decision A") {
		t.Fatalf("double-layer should keep only summary content: %q", got)
	}

	// 无标签：模型未按双层输出 → 全量容错
	plain := "# Session Summary\n\n## Key Decisions\n- D"
	if got := extractSummary(plain); got != plain {
		t.Fatalf("no tags should return full text: %q", got)
	}

	// 只有 <summary> 标签（无 analysis）→ 提取标签内内容
	only := "prefix\n<summary>摘要内容</summary>\nsuffix"
	if got := extractSummary(only); got != "摘要内容" {
		t.Fatalf("summary tag should be extracted: %q", got)
	}

	// <summary> 出现在 <analysis> 前（异常顺序）→ 仍提取第一个 summary 段
	reversed := "<summary>摘要</summary><analysis>思考</analysis>"
	if got := extractSummary(reversed); got != "摘要" {
		t.Fatalf("summary should be extracted even before analysis: %q", got)
	}
}

// TestLLMCompressorDoubleLayer 双层输出经 Compact 后剥离 <analysis>,摘要消息只含 <summary> 内容。
func TestLLMCompressorDoubleLayer(t *testing.T) {
	p := &compressProvider{content: "<analysis>\n思考过程\n</analysis>\n\n<summary>\n# 会话摘要\n\n## 任务目标\n- 目标\n</summary>"}
	c := NewLLMCompressor(p, "m1")

	input := []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewUserMessage(core.Content{Type: "text", Content: "你好"}),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "好"}}, "", nil),
		core.NewUserMessage(core.Content{Type: "text", Content: "任务"}),
	}
	result, err := c.Compact(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}

	// 摘要消息 = [system, summary, latestUser] 中的 summary：只含 <summary> 内容,不含思考
	summary := result.Messages[1].Content[0].Content
	if strings.Contains(summary, "思考过程") || !strings.Contains(summary, "任务目标") {
		t.Fatalf("summary message should be stripped of analysis: %q", summary)
	}
}

// TestLLMCompressorTaskAnchorTruncate 验证任务锚点截断规则（S1-A）：
// 超长首条用户指令截断到 4000 字符并注明，任务目标原文仍可见。
func TestLLMCompressorTaskAnchorTruncate(t *testing.T) {
	p := &compressProvider{content: "# 会话摘要"}
	c := NewLLMCompressor(p, "m1")

	long := strings.Repeat("命", taskAnchorMaxChars+100)
	input := []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewUserMessage(core.Content{Type: "text", Content: "原始任务" + long}),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "答"}}, "", nil),
		core.NewUserMessage(core.Content{Type: "text", Content: "继续"}),
	}
	result, err := c.Compact(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	// 锚点注记消息 = 模板 + 锚点原文；从原文起点取锚点部分校验截断
	body := result.Messages[2].Content[0].Content
	idx := strings.Index(body, "原始任务")
	if idx < 0 {
		t.Fatalf("anchor reminder must quote the original request: %q", body[:80])
	}
	anchor := body[idx:]
	if runes := utf8.RuneCountInString(anchor); runes > taskAnchorMaxChars+300 {
		t.Fatalf("anchor must be truncated: %d runes", runes)
	}
	if !strings.Contains(anchor, "truncated") {
		t.Fatalf("truncated anchor must carry a truncation note: %q", anchor[:80])
	}
	if !strings.Contains(anchor, "原始任务") {
		t.Fatalf("anchor must keep the original task head: %q", anchor[:80])
	}
}

// TestLLMCompressorTaskAnchorInLatest 验证锚点已位于最新输入保留块内时不重复注入。
func TestLLMCompressorTaskAnchorInLatest(t *testing.T) {
	p := &compressProvider{content: "# 会话摘要"}
	c := NewLLMCompressor(p, "m1")
	input := []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewUserMessage(core.Content{Type: "text", Content: "唯一任务"}),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "答"}}, "", nil),
		core.NewUserMessage(core.Content{Type: "text", Content: "最新指令"}),
	}
	// latestUser = [最新指令]，锚点 = 唯一任务（index 1，块外）→ 注入
	result, err := c.Compact(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 4 {
		t.Fatalf("anchor outside latest block must be injected: got %d", len(result.Messages))
	}

	// 锚点 = 唯一任务位于 latestUser 块（本会话仅一条 user 消息 + 历史）→ 跳过
	input2 := []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "答"}}, "", nil),
		core.NewUserMessage(core.Content{Type: "text", Content: "唯一任务"}),
	}
	result2, err := c.Compact(context.Background(), input2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result2.Messages) != 3 {
		t.Fatalf("anchor inside latest block must not be duplicated: got %d", len(result2.Messages))
	}
	if result2.Messages[2].Role != core.User {
		t.Fatalf("last message must be the latest user input: %+v", result2.Messages[2])
	}
}

// TestLLMCompressorNoAnchor 验证无真实用户消息（task_result/空文本）时不注入锚点、不报错。
func TestLLMCompressorNoAnchor(t *testing.T) {
	p := &compressProvider{content: "# 会话摘要"}
	c := NewLLMCompressor(p, "m1")
	input := []core.Message{
		core.NewSystemMessage("你是助手"),
		// 首条 user 角色消息是 task_result（引擎回推信号，不算任务指令）
		core.NewTaskResultMessage("task-1", "research", "结论", nil),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "答"}}, "", nil),
		core.NewUserMessage(core.Content{Type: "text", Content: "继续"}),
	}
	result, err := c.Compact(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range result.Messages {
		if strings.Contains(messageText(m), "This session's original task goal") {
			t.Fatalf("task_result must not become the task anchor: %+v", result.Messages)
		}
	}
	// 无任何 user 消息：无可锚定，且无可压缩（latestUser 空 + history 空）
	if _, err := c.Compact(context.Background(), []core.Message{core.NewSystemMessage("你是助手")}); err == nil {
		t.Fatal("want error when nothing to compress")
	}
}

// TestCompressInputIncludesTaskResult 验证 task_result 消息参与压缩输入（S1-D）：
// 后台任务结果被后续用户消息挤出 latestUser 块后，压缩输入仍渲染其结果正文
// （修复前 messageText 白名单不含 task_result，该消息渲染为空——结果随压缩消失）。
func TestCompressInputIncludesTaskResult(t *testing.T) {
	p := &compressProvider{content: "# 会话摘要"}
	c := NewLLMCompressor(p, "m1")
	input := []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewUserMessage(core.Content{Type: "text", Content: "研究 X"}),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "好"}}, "", nil),
		core.NewTaskResultMessageWithStatus("task-1", "research", "研究结论：X 方案可行", "completed", nil),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "已处理"}}, "", nil),
		core.NewUserMessage(core.Content{Type: "text", Content: "继续"}),
	}
	if _, err := c.Compact(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	req := p.req.Messages[1].Content[0].Content
	if !strings.Contains(req, "研究结论：X 方案可行") || !strings.Contains(req, "task-1") {
		t.Fatalf("task_result must be rendered into compress input: %q", req)
	}
}

// TestLLMCompressorKeepRecent 验证滚动近窗（S1-C）：keepRecent>0 时 latestUser 块之前
// 的尾部 n 条消息原文保留在摘要之后；n=0 行为与现状一致。
func TestLLMCompressorKeepRecent(t *testing.T) {
	p := &compressProvider{content: "# 会话摘要"}
	c := NewLLMCompressor(p, "m1", WithKeepRecentMessages(2))
	input := []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewUserMessage(core.Content{Type: "text", Content: "任务"}),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "第一轮"}}, "", nil),
		core.NewToolMessage("t1", "bash 输出 1"),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "第二轮"}}, "", nil),
		core.NewToolMessage("t2", "bash 输出 2"),
		core.NewUserMessage(core.Content{Type: "text", Content: "继续"}),
	}
	result, err := c.Compact(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	// [system, summary, anchor(任务), recent(第二轮+tool t2), latest(继续)]
	if len(result.Messages) != 6 {
		t.Fatalf("want [system, summary, anchor, recent*2, latest], got %d", len(result.Messages))
	}
	if !strings.Contains(result.Messages[3].Content[0].Content, "第二轮") ||
		!strings.Contains(result.Messages[4].Content[0].Content, "bash 输出 2") {
		t.Fatalf("recent messages must be preserved verbatim: %+v", result.Messages[3:5])
	}
	if strings.Contains(result.Messages[4].Content[0].Content, "bash 输出 1") {
		t.Fatalf("recent window must be capped at n: %+v", result.Messages[3:5])
	}
	if result.Messages[5].Content[0].Content != "继续" {
		t.Fatalf("latest user input must stay last: %+v", result.Messages[5])
	}
	// 近窗消息同时进入压缩输入（摘要 LLM 可见，滚动合并不丢）
	req := p.req.Messages[1].Content[0].Content
	if !strings.Contains(req, "bash 输出 2") || !strings.Contains(req, "第二轮") {
		t.Fatalf("recent messages should also enter compress input: %q", req)
	}
}

// TestCompressMissingSectionsWarnings 验证缺节护栏（S1-G）：摘要缺失关键节 →
// Warnings 记录（不失败）；默认不重试（单次调用）。
func TestCompressMissingSectionsWarnings(t *testing.T) {
	p := &compressProvider{content: "<summary>\n# Session Summary\n\n## Key Decisions\n- D\n</summary>"}
	c := NewLLMCompressor(p, "m1")
	result, err := c.Compact(context.Background(), []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewUserMessage(core.Content{Type: "text", Content: "任务"}),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "好"}}, "", nil),
		core.NewUserMessage(core.Content{Type: "text", Content: "继续"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	// E4：V3 关键节 4 节（Goal/Next Action/Progress/Archive）+ HANDOFF 块 + 旧节 Key Decisions
	if len(result.Warnings) == 0 {
		t.Fatalf("want missing-section warnings, got none")
	}
	foundMissing := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "missing section") {
			foundMissing = true
		}
	}
	if !foundMissing {
		t.Fatalf("warning should name the missing section: %v", result.Warnings)
	}
}

// TestCompressRetryOnMissingSection 验证缺节重试：WithCompressorRetryOnMissingSection
// 开启时缺节 → 第二次调用；第二次仍有缺节 → 采用结果 + warnings（两次为上限）。
func TestCompressRetryOnMissingSection(t *testing.T) {
	calls := 0
	p := &countingCompressProvider{content: func() string {
		calls++
		if calls == 1 {
			return "<summary>\n# Session Summary\n\n## Key Decisions\n- D\n</summary>"
		}
		return "<summary>\n# Session Summary\n\n> HANDOFF\n> - GOAL: g\n> - PROGRESS: p\n> - NEXT: n\n\n## Goal\n- g\n\n## Key Decisions\n- d\n\n## User Inputs\n- u\n\n## Fixed Issues\n- None\n\n## File Changes\n- None (read-only round)\n\n## Next Action\n- Next action: x\n- Needed inputs: a.go\n\n## Progress\n- v\n\n## File Context\n- `a.go`: x\n</summary>"
	}}
	c := NewLLMCompressor(p, "m1", WithCompressorRetryOnMissingSection(true))
	result, err := c.Compact(context.Background(), []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewUserMessage(core.Content{Type: "text", Content: "任务"}),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "好"}}, "", nil),
		// 工具调用携带 a.go：摘要 File Context 提及的路径须能在输入中找到（L2 幻觉检测）
		{Role: core.Assistant, ToolCalls: []core.ToolCall{{Name: "write_file", Arguments: `{"path":"a.go"}`}}},
		core.NewUserMessage(core.Content{Type: "text", Content: "继续"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("retry must call the provider twice, got %d", calls)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("second attempt with all sections must clear warnings, got %v", result.Warnings)
	}
}

// countingCompressProvider 按次返回不同摘要内容（重试用例）。
type countingCompressProvider struct {
	content func() string
}

func (p *countingCompressProvider) Stream(_ context.Context, req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: p.content()})
}

// TestCompressTruncatedDegrades 验证截断检测（S1-G）：输出接近 MaxTokens 且无
// </summary> 结束标签 → 视为截断返回错误（AgentLoop 降级不压缩，上下文不丢）。
func TestCompressTruncatedDegrades(t *testing.T) {
	// 截断启发式用「字符数/4」估算 token 比较 MaxTokens（中文 1 token ≈ 1-2 字符，
	// 裸字节会高估）：需 content 达 MaxTokens*4 字节才触发。
	bound := defaultSummaryMaxTokens * 4
	p := &compressProvider{content: "<summary>\n" + strings.Repeat("x", int(bound))}
	c := NewLLMCompressor(p, "m1")
	_, err := c.Compact(context.Background(), []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewUserMessage(core.Content{Type: "text", Content: "任务"}),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "好"}}, "", nil),
		core.NewUserMessage(core.Content{Type: "text", Content: "继续"}),
	})
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("truncated summary must degrade with error, got %v", err)
	}
}

// TestLLMCompressorKeepRecentCompleteGroup 问题三回归：近窗按条数截断落在工具组中间时
// （tool 回执进了近窗、其 assistant 调用请求被摘要），向前扩展到配对的 assistant 消息，
// 保证 assistant→tool 组完整（无源回执/悬空调用会让部分上游 400）。
func TestLLMCompressorKeepRecentCompleteGroup(t *testing.T) {
	p := &compressProvider{content: "# 会话摘要"}
	c := NewLLMCompressor(p, "m1", WithKeepRecentMessages(2))
	input := []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewUserMessage(core.Content{Type: "text", Content: "任务"}),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "调用A"}}, "", []core.ToolCall{
			{Id: "t1", Name: "bash"}, {Id: "t2", Name: "read_file"},
		}),
		core.NewToolMessage("t1", "输出1"),
		core.NewToolMessage("t2", "输出2"),
		core.NewUserMessage(core.Content{Type: "text", Content: "继续"}),
	}
	result, err := c.Compact(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	// [system, summary, anchor(任务), recent(assistant 调用A + tool t1 + tool t2), latest(继续)]
	if len(result.Messages) != 7 {
		t.Fatalf("want [system, summary, anchor, recent*3(完整组), latest], got %d: %+v", len(result.Messages), result.Messages)
	}
	if !strings.Contains(result.Messages[3].Content[0].Content, "调用A") {
		t.Fatalf("assistant 调用消息必须随其回执进近窗: %+v", result.Messages[3])
	}
	if !strings.Contains(result.Messages[4].Content[0].Content, "输出1") ||
		!strings.Contains(result.Messages[5].Content[0].Content, "输出2") {
		t.Fatalf("两条回执都必须在近窗: %+v", result.Messages[4:6])
	}
}

// TestMessageTextRendersImageMarker 压缩输入渲染必须给图片块留占位标记：
// 图片不可丢（摘要需知道「这里有一张图」），也不能把 base64 倒进压缩输入。
func TestMessageTextRendersImageMarker(t *testing.T) {
	msg := core.NewToolMessageWithImages("t1", "screenshot taken", []core.Content{
		{Type: core.ContentTypeImage, Content: "data:image/png;base64," + strings.Repeat("A", 100000), MimeType: "image/png"},
	})
	got := messageText(msg)
	if strings.Contains(got, "AAAA") {
		t.Fatalf("base64 must not enter compress input: %.200s", got)
	}
	if !strings.Contains(got, imageMarker) {
		t.Fatalf("image marker missing: %q", got)
	}
	if !strings.Contains(got, "screenshot taken") {
		t.Fatalf("text must survive: %q", got)
	}
}

// TestMessageTextMergesAdjacentImages 连续多图合并为一个标记（不刷屏）。
func TestMessageTextMergesAdjacentImages(t *testing.T) {
	var blocks []core.Content
	for i := 0; i < 4; i++ {
		blocks = append(blocks, core.Content{Type: core.ContentTypeImage, Content: "data:image/png;base64,AAAA"})
	}
	got := messageText(core.NewUserMessage(blocks...))
	if n := strings.Count(got, imageMarker); n != 1 {
		t.Fatalf("4 adjacent images should merge to 1 marker, got %d: %q", n, got)
	}
}

// TestMessageTextSeparatesImagesAcrossText 文本隔开的多张图各自留标记（不误合并）。
func TestMessageTextSeparatesImagesAcrossText(t *testing.T) {
	img := core.Content{Type: core.ContentTypeImage, Content: "data:image/png;base64,AAAA"}
	got := messageText(core.NewUserMessage(img, core.Content{Type: "text", Content: " and "}, img))
	if n := strings.Count(got, imageMarker); n != 2 {
		t.Fatalf("images split by text should each mark, got %d: %q", n, got)
	}
}
