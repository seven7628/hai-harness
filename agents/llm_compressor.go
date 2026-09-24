package agents

import (
	"context"
	"errors"
	"fmt"
	"github.com/seven7628/hai-harness/agents/prompt"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
	"strings"
	"time"
)

// summaryContentType 摘要消息的内容块类型：机器按此识别旧摘要（滚动合并），
// provider 翻译时按文本发送，模型视角无感。
const summaryContentType = core.ContentTypeSummary

const defaultSummaryMaxTokens = 8192

// defaultLatestUserMaxTokens 最新用户输入块的保留上限（估算 token）；
// 超限并入压缩输入（过长原样保留会撑爆上下文窗口）。
const defaultLatestUserMaxTokens = 8192

// taskAnchorMaxChars 任务锚点（首条用户任务指令原文）的保留上限：超限截断，
// 并在末尾注明（防超长指令撑爆压缩后上下文；要点由摘要 Main Requests 覆盖）。
const taskAnchorMaxChars = 4000

// SummaryPrompt 压缩指令（英文，10 section 结构化输出 + <analysis>/<summary> 双层）。
// 常量本体已迁移到 agents/prompt/compact.go（提示词唯一定义地）；此处引用
// prompt.SummaryPrompt。语义与消费逻辑见 compact.go 注释与 Compact()。

// LLMCompressor 基于 LLM 的结构化压缩器：把对话历史总结为结构化 Markdown 摘要。
//
// 语义：
//   - 输出 = [保留的 SystemPrompt, 摘要消息]（摘要消息 Content.Type="summary"）
//   - 旧摘要（Type="summary" 消息）作为「已有摘要」参与下次压缩（滚动合并，
//     压缩成本不随轮次增长）
//   - 压缩调用走 provider 但不产生 LLM 事件流（审计点 = CompressEnd 事件），
//     用量经 CompressResult 计入运行结算
//   - 失败由 AgentLoop 降级（保持原上下文）
type LLMCompressor struct {
	provider     provider.Provider
	model        string
	providerName string // 价表/模型元数据 Lookup 用（StreamRequest.Provider）；空 = 注册表全局兜底
	// sessionID 会话标识（StreamRequest.SessionID）：压缩属同一会话的辅助请求，
	// OpenCode Go 网关要求辅助请求同样带 x-opencode-session（按会话路由 + 缓存前缀
	// 复用）。空 = 不下发（由 transport 兜底 id 接管）。bridge 装配时注入。
	sessionID string
	MaxTokens int64         // 摘要输出上限（默认 8192；建议不超过窗口 5%，防压后二次触发）
	Timeout   time.Duration // 压缩调用可选总时限兜底；0 = 不限（2026-09 起宿主传 0：
	// 首包超时 60s 由 HTTP 层统一覆盖，压缩流开始后不限时长，与主会话一致）

	// StallTimeout 压缩调用的流内停滞预算（2026-09-22）：首个事件之后相邻两个事件之间的
	// 最大静默时长，超时按可重试的 provider.StreamStallError 终止本次调用。
	// 与 Timeout（整次调用总时限）互补，**不界总时长** —— 自动压缩发生在 run 内部，
	// 压缩流中段挂死会让整个会话静默无输出（与主对话同因同解）。0 = 不限制；
	// 默认 3min（与 agents.StreamStallTimeout 同值）。
	StallTimeout time.Duration

	// MaxRetries 压缩 LLM 调用的最大尝试次数（含首次，默认 1 = 不重试）。
	// 与主 Agent streamWithRetry 的 MaxRetries 语义一致（2026-08-30：
	// 压缩曾裸调用无重试，瞬时错误如 JSON 解析失败直接放弃整轮压缩；
	// 现在对齐主 Agent 的 10 次重试策略，由宿主注入）。
	MaxRetries int

	// Backoff 压缩重试退避（nil = DefaultBackoff 指数退避）。2026-09 起宿主
	// 注入 FixedBackoff(1s)（与主 Agent 对齐：快速重试，减少用户等待）。
	Backoff Backoff

	// LatestUserMaxTokens 最新用户输入块的保留上限（估算 token）：
	// 超限并入压缩输入（过长原样保留会撑爆上下文窗口）；0 = 不限制。
	LatestUserMaxTokens int64

	// KeepRecentMessages 压缩后保留的尾部消息条数（滚动近窗，S1-C）：
	// 最近 n 条消息（latestUser 块之前）以原文保留在摘要之后——压缩后模型仍能看到
	// 最近 1-2 轮的工具结果（最后一次 bash 输出/最后一个 diff），"接着干"的连续性
	// 显著提升；下次压缩时近窗消息自然并入滚动合并输入（成本不涨）。
	// 0 = 关（默认）：保持纯摘要行为。
	KeepRecentMessages int

	// RetryOnMissingSection 摘要缺节时重试一次（S1-G，默认 false）：压缩质量护栏
	// 检测关键节（Main Requests and Intent / Current Task / File Context）缺失，
	// 开启后重试一次（成本可控），仍缺节则带 warnings 返回；关闭时缺节仅 warnings。
	RetryOnMissingSection bool

	// SummaryVersion 压缩提示词版本（SW1）：1 = SummaryPrompt（V1，零值默认，
	// 向后兼容）；2 = SummaryPromptV2（2026-08-24 压缩优化方案）；3 = SummaryPromptV3
	// （2026-08-30 决策层强化，见 agents/prompt/compact_v3.go）。重放 A/B 用。
	SummaryVersion int

	// ContextWindow 模型上下文窗口（C5 近窗裁剪，WithContextWindow 注入）：
	// >0 时近窗原文段超 max(10% 窗口, 8k) token 从最老端裁；0 = 不裁。
	ContextWindow int64

	// maxTokensSet 标记 MaxTokens 是否被显式设置（WithSummaryMaxTokens）。
	// 未显式设置时，WithSummaryContextWindow 按窗口 5% 联动 MaxTokens（1M 窗口 →
	// 50K 输出上限）：否则默认 8192 对 560K 输入的手动 summary 必然 finish_reason=length
	// 截断失败。显式设置优先（调用方明确控制）。
	maxTokensSet bool
}

type LLMCompressorOption func(*LLMCompressor)

// WithSummaryMaxTokens 设置摘要输出上限（默认 8192）。显式设置后
// WithSummaryContextWindow 不再联动覆盖。
func WithSummaryMaxTokens(n int64) LLMCompressorOption {
	return func(c *LLMCompressor) {
		c.MaxTokens = n
		c.maxTokensSet = true
	}
}

// WithCompressTimeout 设置单次压缩调用超时（默认 90s）。
func WithCompressTimeout(d time.Duration) LLMCompressorOption {
	return func(c *LLMCompressor) { c.Timeout = d }
}

// WithCompressStallTimeout 设置压缩调用的流内停滞预算（0 = 不限制）。
// 语义与取值理由见 LLMCompressor.StallTimeout。
func WithCompressStallTimeout(d time.Duration) LLMCompressorOption {
	return func(c *LLMCompressor) { c.StallTimeout = d }
}

// WithCompressProviderName 绑定压缩调用的 provider 名（价表/模型元数据 Lookup 用；
// StreamRequest.Provider）。bridge 装配时注入；空 = 注册表按模型名全局兜底。
func WithCompressProviderName(name string) LLMCompressorOption {
	return func(c *LLMCompressor) { c.providerName = name }
}

// WithCompressSessionID 绑定压缩调用的会话标识（StreamRequest.SessionID）：
// 压缩是同一会话的辅助 LLM 请求，OpenCode Go 网关要求辅助请求同样带
// x-opencode-session（缺失即 400；带同一 id 才能复用会话的路由与缓存前缀）。
func WithCompressSessionID(sid string) LLMCompressorOption {
	return func(c *LLMCompressor) { c.sessionID = sid }
}

// WithCompressMaxRetries 设置压缩 LLM 调用的最大尝试次数（含首次，默认 1 = 不重试）。
// 与主 Agent MaxRetries 语义一致：失败自动重试，退避等待，永久错误不重试。
func WithCompressMaxRetries(n int) LLMCompressorOption {
	return func(c *LLMCompressor) { c.MaxRetries = n }
}

// WithCompressBackoff 设置压缩重试退避（nil = DefaultBackoff 指数退避）。
// 2026-09 宿主注入 FixedBackoff(1s)：与主 Agent 对齐，快速重试减少用户等待。
func WithCompressBackoff(f Backoff) LLMCompressorOption {
	return func(c *LLMCompressor) { c.Backoff = f }
}

// WithLatestUserMaxTokens 设置最新用户输入块的保留上限（估算 token，默认 8192）；
// 超限并入压缩输入，摘要覆盖其要点。
func WithLatestUserMaxTokens(n int64) LLMCompressorOption {
	return func(c *LLMCompressor) { c.LatestUserMaxTokens = n }
}

// WithKeepRecentMessages 设置压缩后保留的尾部消息条数（滚动近窗，默认 0 = 关）。
// 开启后近窗消息以原文保留在摘要之后（见 KeepRecentMessages 注释）；与 latestUser
// 块去重（只取 latestUser 之前的消息），下次压缩并入滚动合并输入。
func WithKeepRecentMessages(n int) LLMCompressorOption {
	return func(c *LLMCompressor) { c.KeepRecentMessages = n }
}

// WithCompressorRetryOnMissingSection 摘要缺节时重试一次（S1-G 护栏，默认关）。
func WithCompressorRetryOnMissingSection(b bool) LLMCompressorOption {
	return func(c *LLMCompressor) { c.RetryOnMissingSection = b }
}

// WithSummaryVersion 设置压缩提示词版本（SW1）：1 = SummaryPrompt（V1，默认），
// 2 = SummaryPromptV2（2026-08-24 压缩优化方案，见 agents/prompt/compact.go），
// 3 = SummaryPromptV3（2026-08-30 决策层强化，见 agents/prompt/compact_v3.go）。
func WithSummaryVersion(v int) LLMCompressorOption {
	return func(c *LLMCompressor) { c.SummaryVersion = v }
}

// WithContextWindow 设置模型上下文窗口（C5 近窗裁剪用）：>0 时近窗原文段超过
// max(10% 窗口, 8k) token 即从最老端裁剪；0 = 不裁（保持旧行为）。由宿主按
// 会话实际窗口注入（桌面 Bridge 用 windowForIn）。
func WithSummaryContextWindow(tokens int64) LLMCompressorOption {
	return func(c *LLMCompressor) {
		c.ContextWindow = tokens
		// 注意：ContextWindow 是上下文大小（近窗裁剪用），MaxTokens 是输出上限——
		// 两者独立，不应联动。输出上限由调用方显式 WithSummaryMaxTokens 设置
		//（bridge 用模型真实 max_tokens）；此处仅作未显式设置时的保守兜底
		//（默认 8192 保持原语义，不按窗口放大）。
	}
}

// recentTokenLimit 近窗 token 上限：max(10% 窗口, 8k)；未配置窗口返回 0 = 不裁。
func (c *LLMCompressor) recentTokenLimit() int64 {
	if c.ContextWindow <= 0 {
		return 0
	}
	limit := c.ContextWindow / 10
	if limit < 8000 {
		limit = 8000
	}
	return limit
}

// trimRecentWindow 近窗超限裁剪（C5）：从最老端逐条丢弃直到不超限；新最老端若是
// tool 回执（配对的 assistant 调用已被裁掉），继续丢弃直到非 tool——与
// splitMessages 的交换组外扩同思路，保证不产生无源回执（部分上游直接 400）。
func trimRecentWindow(recent []core.Message, limit int64) []core.Message {
	if len(recent) == 0 || limit <= 0 || estimateMessagesTokens(recent) <= limit {
		return recent
	}
	i := 0
	for i < len(recent) && estimateMessagesTokens(recent[i:]) > limit {
		i++
	}
	for i < len(recent) && recent[i].Role == core.Tool {
		i++
	}
	return recent[i:]
}

// NewLLMCompressor 创建基于 LLM 的结构化压缩器。
func NewLLMCompressor(p provider.Provider, model string, opts ...LLMCompressorOption) *LLMCompressor {
	c := &LLMCompressor{
		provider:            p,
		model:               model,
		MaxTokens:           defaultSummaryMaxTokens,
		Timeout:             90 * time.Second,
		StallTimeout:        3 * time.Minute, // 与 agents.StreamStallTimeout 同值（流内静默上限）
		LatestUserMaxTokens: defaultLatestUserMaxTokens,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *LLMCompressor) ShouldCompact(_ context.Context, stats CompressStats) bool {
	// 触发线：预估 token >= 预算（默认 80% 窗口，由 AgentLoop 计算）
	return stats.Budget > 0 && stats.EstimatedTokens >= stats.Budget
}

// summaryPrompt 按 SummaryVersion 返回压缩提示词（SW1）：默认 V1，>=2 用 V2，>=3 用 V3。
func (c *LLMCompressor) summaryPrompt() string {
	if c.SummaryVersion >= 3 {
		return prompt.SummaryPromptV3
	}
	if c.SummaryVersion >= 2 {
		return prompt.SummaryPromptV2
	}
	return prompt.SummaryPrompt
}

func (c *LLMCompressor) Compact(ctx context.Context, messages []core.Message) (CompressResult, error) {
	sp := splitMessages(messages, c.KeepRecentMessages)
	system, latestUser, prevSummary, history := sp.system, sp.latestUser, sp.prevSummary, sp.history
	// 最新用户输入超限：并入压缩输入（过长原样保留会撑爆窗口，宁可压缩不可超窗）
	if c.LatestUserMaxTokens > 0 && estimateMessagesTokens(latestUser) > c.LatestUserMaxTokens {
		history = append(history, latestUser...)
		latestUser = nil
	}
	if len(history) == 0 {
		return CompressResult{}, errors.New("llm compressor: nothing to compress")
	}

	input := renderCompressInput(prevSummary, sp.anchor, history)
	// B3 动态护栏触发条件：历史含任务结果块 / agent_spawn 调用 / 注入段 [TaskStates]
	taskTrace := hasTaskTrace(history, input)
	req := &provider.StreamRequest{
		Model:     c.model,
		Provider:  c.providerName,
		SessionID: c.sessionID,
		Config: &provider.RequestConfig{
			MaxTokens: c.MaxTokens,
			// 摘要档位固定 high（2026-09 用户决策）：摘要是「忠实压缩已有上下文」的
			// 结构化任务，不随会话档位漂移 —— 此前不带 Effort，落到厂商默认（多数
			// 模型 = high，但显式声明更明确，且避免注册表默认变化时摘要跟着变）。
			// 无档位可表达的模型由 provider 侧消化（toggle-only 只发开关）。
			Effort: provider.ReasoningEffortLevelHigh,
		},
		Messages: []core.Message{
			core.NewSystemMessage(c.summaryPrompt()),
			core.NewUserMessage(core.Content{Type: "text", Content: input}),
		},
	}

	// 压缩调用独立超时：网络挂住不能阻塞整个 agent（主对话有超时+重试，压缩裸奔会挂死）
	cctx := ctx
	cancel := func() {}
	if c.Timeout > 0 {
		cctx, cancel = context.WithTimeout(ctx, c.Timeout)
	}
	defer cancel()

	// 压缩调用重试（2026-08-30 对齐主 Agent streamWithRetry 策略）：
	// 外层 = MaxRetries（含首次）——网络/解析等瞬时错误自动重试（retryable 过滤
	// 永久错误），每次失败退避等待；内层 = RetryOnMissingSection 缺节重试（两次上限）。
	// 截断（finish_reason=length 主判据，无 </summary> 且接近 MaxTokens 为兜底）视为
	// 失败降级——比"全部原文进上下文"更安全（上下文不丢，下轮再试）。
	maxAttempts := c.MaxRetries
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	qualityAttempts := 1
	if c.RetryOnMissingSection {
		qualityAttempts = 2
	}
	var md, analysis string
	var usage core.Usage
	var failedUsage core.Usage // 失败尝试的已产生用量累计（最终成功/失败都随结果回传记账）
	var model string
	var warnings []string
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		lastErr = nil
		// 内层：缺节质量重试（同一调用输入，带上一轮 warnings 反馈）
		for i := 0; i < qualityAttempts; i++ {
			m, a, u, mdl, err := c.compressOnce(cctx, req)
			if err != nil {
				lastErr = err
				failedUsage = failedUsage.Add(u) // 失败尝试的用量也要进账（上游已计费）
				break                            // 调用错误 → 交给外层重试判断
			}
			md, analysis, usage, model = m, a, u, mdl // 赋值外层变量（循环内 := 会遮蔽）
			warnings = summaryQualityWarnings(md, input, taskTrace)
			if len(warnings) == 0 || i == qualityAttempts-1 {
				break // 无问题或已达质量重试上限：采用本次结果（warnings 透传）
			}
			// 带反馈重试（L2/E5）：首轮问题清单拼进重试输入，模型按反馈逐项修正
			req.Messages[1] = core.NewUserMessage(core.Content{Type: "text", Content: input +
				"\n\nPrevious attempt issues (fix all of them in your retry):\n- " +
				strings.Join(warnings, "\n- ")})
		}
		if lastErr == nil {
			break // 调用成功（含质量重试完成）→ 采用结果
		}
		// 调用失败：永久错误不重试；已达上限放弃
		if attempt == maxAttempts || !retryable(lastErr) {
			// 失败收尾：把失败尝试的累计用量随结果回传（调用方 addUsage 进账 + 事件上抛），
			// 不再像此前那样整个丢弃。
			return CompressResult{FailedUsage: failedUsage}, lastErr
		}
		// 退避等待（对齐主 Agent；Backoff nil = DefaultBackoff），ctx 感知取消
		delay := c.Backoff
		if delay == nil {
			delay = DefaultBackoff
		}
		select {
		case <-time.After(delay(attempt, lastErr)):
		case <-ctx.Done():
			return CompressResult{}, ctx.Err()
		}
	}

	out := append([]core.Message(nil), system...)
	out = append(out, core.Message{
		Role:    core.System,
		Content: []core.Content{{Type: summaryContentType, Content: strings.TrimSpace(md)}},
	})
	// 任务锚点（S1-A）：首条用户任务指令由引擎逐字保留，注入摘要之后、近窗/最新输入
	// 之前——任务目标不随滚动合并退化（不依赖摘要 LLM 自觉转述）。锚点消息本身已位于
	// latestUser 保留块内时跳过（无需重复注入）。
	if sp.anchor != "" && !sp.anchorInLatest {
		out = append(out, core.NewSystemMessage(fmt.Sprintf(prompt.TaskAnchorReminder, sp.anchor)))
	}
	// 滚动近窗（S1-C）：最近 n 条消息原文保留（与 latestUser 块去重），
	// 下次压缩并入滚动合并输入，成本不涨。
	// C5 近窗 token 上限：条数不限大小时，单个巨大工具输出即可让近窗自身达数万
	// token——超限从最老端裁（保持交换组完整，不留悬空 tool 回执）。
	if len(sp.recent) > 0 {
		sp.recent = trimRecentWindow(sp.recent, c.recentTokenLimit())
		out = append(out, sp.recent...)
	}
	// 最新用户输入块原样保留在最后：当前任务指令不参与压缩，
	// 压缩吞掉用户输入会让模型只看到摘要转述（指令可能失真）
	out = append(out, latestUser...)
	return CompressResult{
		Messages: out, Summary: strings.TrimSpace(md), Model: model,
		Usage: usage, FailedUsage: failedUsage, Warnings: warnings, Analysis: analysis,
	}, nil
}

// compressOnce 单次压缩调用：返回摘要正文（已剥离 <analysis>）、剥离出的 analysis
// 块（O1 留档，仅观测不进上下文）、用量与模型。截断检测（C4/E3）：以
// FinishReason=="length" 为主判据；"无 </summary> 且接近 MaxTokens" 的字节启发式
// 降为兜底（中文摘要在 ~2.5k token 处会被字节比较误判）。命中即返回错误——调用方
// 降级为不压缩（上下文不丢，下轮再试），避免"截断的摘要全量进上下文"。
func (c *LLMCompressor) compressOnce(ctx context.Context, req *provider.StreamRequest) (string, string, core.Usage, string, error) {
	var raw, model string
	var finishReason core.FinishReason
	var usage core.Usage
	// 流内停滞看门狗（2026-09-22）：压缩调用此前只有「整次调用总时限」（生产传 0 = 不限）
	// 与 HTTP 首包超时 60s，流中段挂死同样会一直阻塞 —— 自动压缩发生在 run 内部，压缩
	// 静默挂死 = 整个会话静默无输出（与主对话同因，解法同源：只界相邻事件的静默，
	// 不界总时长）。语义/取值见 LLMCompressor.StallTimeout。
	streamCtx, cancelCause := context.WithCancelCause(ctx)
	defer cancelCause(context.Canceled)
	stall := newStreamStallWatch(c.StallTimeout, cancelCause)
	defer stall.stop()
	err := c.provider.Stream(streamCtx, req, func(e provider.StreamEvent) error {
		stall.touch() // 事件即活动：重置停滞看门狗
		if ev, ok := e.(provider.LLMEndEvent); ok {
			raw = ev.Content
			usage = ev.Usage
			model = ev.Model
			finishReason = ev.FinishReason
		}
		return nil
	})
	if err != nil {
		// 失败尝试的已产生用量（上游按已生成 token 计费）随错误带回：压缩输入是整段
		// 上下文，失败一次可能白烧大量 input token —— 调用方据此记账（2026-09-23）。
		partial, _ := provider.PartialUsage(err)
		// 停滞看门狗的取消在 provider 侧只表现为「context canceled」→ 按 cause 翻译成
		// 可重试的上游故障（不能以 context.Canceled 上抛：retryable() 会判成用户中断）。
		if stallErr, ok := context.Cause(streamCtx).(*provider.StreamStallError); ok {
			stallErr.SetCause(err)
			return "", "", partial, "", stallErr
		}
		return "", "", partial, "", err
	}
	if strings.TrimSpace(raw) == "" {
		return "", "", core.Usage{}, "", errors.New("llm compressor: empty summary")
	}
	// 截断检测（C4）：主判据 FinishReason；字节启发式兜底（无 </summary> 且超预算 90%）
	hasClose := strings.Contains(raw, "</summary>")
	if finishReason == core.FinishReasonLength ||
		(!hasClose && c.MaxTokens > 0 && int64(len(raw)/4) >= c.MaxTokens) {
		// 字节启发式用「字符数/4」估算 token 比较 MaxTokens（而非裸字节）：
		// 中文摘要 1 token ≈ 1-2 字符，裸字节会高估 2-4 倍 → MaxTokens 增大后误判截断。
		return "", "", core.Usage{}, "", errors.New("llm compressor: summary truncated (finish_reason=length or no </summary> at max_tokens bound)")
	}
	// O1：先捕获 <analysis> 块（留档进事件日志/会话 jsonl），再剥离双层输出——
	// analysis 绝不进 Messages（S6/D.2 语义不变），只作压缩器决策过程的观测材料。
	analysis := extractAnalysis(raw)
	md := extractSummary(raw)
	return strings.TrimSpace(md), analysis, usage, model, nil
}

// extractAnalysis 提取 <analysis>…</analysis> 块内容（O1 留档用）；无则空串。
func extractAnalysis(md string) string {
	start := strings.Index(md, "<analysis>")
	end := strings.Index(md, "</analysis>")
	if start >= 0 && end > start {
		return strings.TrimSpace(md[start+len("<analysis>") : end])
	}
	return ""
}

// summaryCriticalSections 压缩质量护栏（S1-G + E4）的关键节：缺节即记 warnings。
// V3.2（第一性原理重构）：关键节 = Goal（目标）+ Next Action（下一步）+
// Key Decisions / User Inputs / Fixed Issues（一致性锚点——防前后矛盾、防重复修复、
// 防目标漂移）。Progress / Todo / File Context / Background Tasks 按需
// （无快照/无任务/只读轮次时省略合法），不在关键节内。
var summaryCriticalSections = []string{
	"## Goal",
	"## Key Decisions",
	"## User Inputs",
	"## Fixed Issues",
	"## Next Action",
}

// missingSummarySections 检测摘要缺失的关键节（S1-G 护栏）。
func missingSummarySections(summary string) []string {
	var missing []string
	for _, s := range summaryCriticalSections {
		if !strings.Contains(summary, s) {
			missing = append(missing, fmt.Sprintf("summary missing section %q", s))
		}
	}
	return missing
}

// hasTaskTrace 判断压缩输入是否携带任务痕迹（B3 动态护栏触发条件）：
// 历史消息含 task_result 内容块 / 渲染输入含 agent_spawn 工具调用 / 注入了 [TaskStates] 段。
func hasTaskTrace(history []core.Message, renderedInput string) bool {
	if strings.Contains(renderedInput, "[tool_call] agent_spawn") || strings.Contains(renderedInput, "[TaskStates]") {
		return true
	}
	for _, m := range history {
		for _, c := range m.Content {
			if c.Type == core.ContentTypeTaskResult {
				return true
			}
		}
	}
	return false
}

// summaryQualityWarnings 压缩摘要质量护栏总入口（E4；全部 warnings 级，不阻断）：
//   - L1 结构：V3.2 关键节缺失（## Goal / ## Key Decisions / ## User Inputs /
//     ## Fixed Issues / ## Next Action）；
//   - L1.1 V2 特有节名混入（## Continuation State / ## Current Task / ## Task List /
//     ## Open Tasks / ## Suggested Next Steps）→ 新旧结构重复/互斥告警；
//   - L1.2 HANDOFF 块存在性（V3 顶部交接单）；
//   - B3 动态：输入有任务痕迹却缺 ## Background Tasks；
//   - L2 内容：Goal 引用输入渲染头（确定性污染，可重试）、幻觉路径；
//   - L2.1 事实唯一性：文件路径在 File Context 与 Next Action 重复出现即告警；
//   - G4 软护栏：摘要超预算。
//
// 非空 warnings 驱动带反馈重试（RetryOnMissingSection）并透传 CompressEnd.Warnings。
// 注：V3.2 恢复 Key Decisions/User Inputs/Fixed Issues/File Changes/File Context
// 为独立节（第一性原理：决策/用户输入/修复记录是「一致性锚点」，缺失则前后矛盾），
// 这些节名不再视为 legacy；仅 V2 特有的结构（Continuation State 等）仍视为 legacy。
func summaryQualityWarnings(summary, renderedInput string, taskTrace bool) []string {
	var w []string
	w = append(w, missingSummarySections(summary)...)

	// L1.1 V2 特有节名混入 = 新旧结构重复/互斥，必须告警（可重试）
	legacySections := []string{
		"## Continuation State", "## Main Requests and Intent", "## Current Task",
		"## Suggested Next Steps", "## Task List", "## Open Tasks",
	}
	for _, ls := range legacySections {
		if strings.Contains(summary, ls) {
			w = append(w, fmt.Sprintf("legacy section %q present — V3.2 summary uses ## Goal/## Key Decisions/## User Inputs/## Fixed Issues/## File Changes/## Next Action/## Progress/## Todo/## Background Tasks/## File Context; old sections duplicate or contradict the new ones", ls))
		}
	}

	// L1.2 HANDOFF 块存在性（V3 顶部交接单，续跑的首读对象）
	if !strings.Contains(summary, "> HANDOFF") {
		w = append(w, `summary missing the top "> HANDOFF" block — the continuation contract (GOAL/PROGRESS/NEXT/TASKS) must be first`)
	}

	if taskTrace && !strings.Contains(summary, "## Background Tasks") {
		w = append(w, `task traces present in input but summary missing section "## Background Tasks"`)
	}

	// L2 污染检测（确定性）：Goal 引用输入渲染头 = 逐字引用被污染
	goal := sectionBody(summary, "## Goal")
	for _, header := range []string{"New conversation history:", "Existing session summary:"} {
		if strings.Contains(goal, header) {
			w = append(w, fmt.Sprintf("Goal quotes input header %q — headers are structure, never the user's words", header))
			break
		}
	}

	// L2.1 事实唯一性：文件路径只允许完整列出在 File Context（唯一完整清单）；
	// Next Action 引用短名不算重复（V3.2：Next 指向下一步所需，File Context 持索引）。
	// 检测：Next Action 里出现的完整路径（含扩展名+行号/多段路径）若也在 File Context
	// 完整列出 → 重复。短名（单段、无行号）视为引用，不告警。
	fc := sectionBody(summary, "## File Context")
	fcPaths := extractClaimedPaths(fc)
	na := sectionBody(summary, "## Next Action")
	for _, p := range extractClaimedPaths(na) {
		if !strings.Contains(p, "/") && !strings.Contains(p, ":") {
			continue // 短名引用（如 a.go），不视为重复
		}
		for _, fp := range fcPaths {
			if p == fp || strings.HasSuffix(p, fp) || strings.HasSuffix(fp, p) {
				w = append(w, fmt.Sprintf("file path %q appears in both File Context and Next Action — each fact must appear exactly once (File Context is the ONLY full file list)", p))
				break
			}
		}
	}

	// G4：预算（>24k 字符或按 chars/4 近似 >6k tok）
	if len(summary) > 24000 || len(summary)/4 > 6000 {
		w = append(w, fmt.Sprintf("summary exceeds budget (~%d chars; target under ~3000 tokens)", len(summary)))
	}

	// L2 幻觉文件检测：File Context 提及的路径必须能在渲染输入中找到
	for _, p := range fcPaths {
		if !strings.Contains(renderedInput, p) {
			w = append(w, fmt.Sprintf("path %q not found in input ([FileLedger]/tool args/history) — likely a hallucinated path", p))
		}
	}
	return w
}

// sectionBody 取指定 "## Header" 节的正文（至下一个 "## " 或文末）；无该节返回空串。
func sectionBody(s, header string) string {
	i := strings.Index(s, header)
	if i < 0 {
		return ""
	}
	body := s[i+len(header):]
	if j := strings.Index(body, "\n## "); j >= 0 {
		body = body[:j]
	}
	return strings.TrimSpace(body)
}

// countBullets 统计以 "- " 开头的行数（列表条目近似）。
func countBullets(body string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "- ") {
			n++
		}
	}
	return n
}

// extractClaimedPaths 从摘要节正文提取"声称的路径"（L2 幻觉检测用）：
// 反引号包裹的片段 + 行首条目的首个 token。仅保留"像路径"的 token
// （含路径分隔符或带文件扩展名），符号/普通词不算——warnings 级宁漏勿误报。
func extractClaimedPaths(text string) []string {
	var paths []string
	seen := map[string]bool{}
	add := func(tok string) {
		tok = strings.Trim(tok, "` \t.,;:)('\"：")
		if tok == "" || seen[tok] || !isPathLike(tok) {
			return
		}
		seen[tok] = true
		paths = append(paths, tok)
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		// 反引号片段优先
		for {
			i := strings.Index(line, "`")
			if i < 0 {
				break
			}
			rest := line[i+1:]
			j := strings.Index(rest, "`")
			if j < 0 {
				break
			}
			add(rest[:j])
			line = rest[j+1:]
		}
		line = strings.TrimPrefix(line, "- ")
		// 剥离 "- read: x" 式操作前缀
		if i := strings.Index(line, ":"); i > 0 && i < 16 {
			line = line[i+1:]
		}
		for _, seg := range strings.FieldsFunc(line, func(r rune) bool { return r == ';' || r == ',' || r == '(' || r == ')' }) {
			seg = strings.TrimSpace(seg)
			if idx := strings.Index(seg, " "); idx > 0 {
				seg = seg[:idx] // 首个 token
			}
			add(seg)
		}
	}
	return paths
}

// isPathLike 粗判 token 是否形似文件路径：含 "/" 或 "\"，或以 .扩展名 结尾。
func isPathLike(tok string) bool {
	if strings.ContainsAny(tok, "/\\") {
		return true
	}
	dot := strings.LastIndex(tok, ".")
	return dot > 0 && dot < len(tok)-1 && len(tok)-dot-1 <= 6
}

// bracketBlock 提取渲染输入中方括号标记块（[TodoSnapshot] 等）的正文：
// 自标记行下一行起，至下一个行首 "[" 段或文末。
func bracketBlock(input, marker string) string {
	i := strings.Index(input, marker)
	if i < 0 {
		return ""
	}
	rest := input[i+len(marker):]
	j := strings.Index(rest, "\n")
	if j < 0 {
		return ""
	}
	rest = rest[j+1:]
	end := len(rest)
	if k := strings.Index(rest, "\n["); k >= 0 {
		end = k
	}
	return strings.TrimSpace(rest[:end])
}

// normalizeWS 折叠空白字符（Task List diff 用）。
func normalizeWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// splitMessages 分离压缩输入：保留的 SystemPrompt / 最新用户输入块 / 已有摘要 /
// 待压缩历史 / 任务锚点 / 滚动近窗。
// 首条 system 视为框架注入的 SystemPrompt 原样保留；最后一段连续 user 消息
// （用户最近一次输入的完整批——连发多条天然连续）原样保留；其余消息（含历史中的
// 其他 system）全部进压缩输入。
func splitMessages(messages []core.Message, keepRecent int) splitResult {
	userEnd := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == core.User {
			userEnd = i
			break
		}
	}
	userStart := userEnd
	if userEnd >= 0 {
		for userStart > 0 && messages[userStart-1].Role == core.User {
			userStart-- // 扩展为整段连续 user 块（用户连发批全保留）
		}
	}

	var sp splitResult
	for i, m := range messages {
		if i >= userStart && i <= userEnd {
			sp.latestUser = append(sp.latestUser, m)
			continue
		}
		if isSummaryMessage(m) {
			sp.prevSummary += messageText(m)
			continue
		}
		if m.Role == core.System && i == 0 {
			sp.system = append(sp.system, m)
			continue
		}
		sp.history = append(sp.history, m)
	}

	// 任务锚点（S1-A）：首条真实用户输入（text 块；task_result/command 消息不算任务
	// 指令），取全文并截断——任务目标由引擎逐字保留，不依赖摘要转述。
	sp.anchor, sp.anchorIdx, sp.anchorInLatest = extractTaskAnchor(messages, userStart, userEnd)

	// 滚动近窗（S1-C）：latestUser 块之前的尾部 n 条消息原样保留（跳过首条 system
	// 与摘要消息——前者已作 system 保留，后者已进 prevSummary，重复无益）。
	// 完整交换组（问题三）：按条数截断可能落在工具组中间（tool 回执进了近窗、
	// 其 assistant 调用请求被摘要），产生无源回执/悬空调用（部分上游直接 400）。
	// 最老边是 tool 回执时向前扩展到配对的 assistant 消息（允许超出 n 一条）。
	if keepRecent > 0 {
		oldest := -1 // 已选窗口的最老消息下标
		for i := userStart - 1; i >= 0 && len(sp.recent) < keepRecent; i-- {
			m := messages[i]
			if i == 0 && m.Role == core.System {
				continue
			}
			if isSummaryMessage(m) {
				continue // 摘要不进近窗（已进 prevSummary，滚动合并输入）
			}
			sp.recent = append([]core.Message{m}, sp.recent...) // 保持时间序
			oldest = i
		}
		for oldest > 0 && messages[oldest].Role == core.Tool {
			prev := messages[oldest-1]
			if prev.Role == core.System || prev.Role == core.User || isSummaryMessage(prev) {
				break // 结构异常（正常写回 assistant 相邻），保守不外扩
			}
			sp.recent = append([]core.Message{prev}, sp.recent...)
			oldest--
		}
	}
	return sp
}

// splitResult splitMessages 的分离结果。
type splitResult struct {
	system         []core.Message
	latestUser     []core.Message
	prevSummary    string
	history        []core.Message
	anchor         string         // 任务锚点（首条真实 user 输入原文，截断后；空 = 无锚点）
	anchorIdx      int            // 锚点消息下标（-1 = 无）
	anchorInLatest bool           // 锚点消息本身位于 latestUser 块内（原样保留区，无需重复注入）
	recent         []core.Message // 滚动近窗（keepRecent > 0 时非空）
}

// extractTaskAnchor 提取任务锚点：system 之后的第一个「真实用户输入」消息
// （text 内容块；task_result/command 消息是引擎注入信号，不算任务指令）。
// 返回锚点文本（截断后）、消息下标（无 = -1）、锚点是否在 latestUser 保留块内。
func extractTaskAnchor(messages []core.Message, userStart, userEnd int) (string, int, bool) {
	for i, m := range messages {
		if m.Role != core.User || !hasRealText(m) {
			continue
		}
		anchor := strings.TrimSpace(messageText(m))
		if anchor == "" {
			continue
		}
		if runes := []rune(anchor); len(runes) > taskAnchorMaxChars {
			anchor = string(runes[:taskAnchorMaxChars]) +
				"\n\n[truncated: the original request was longer than 4000 characters; its full gist is in Main Requests and Intent in the summary above]"
		}
		return anchor, i, i >= userStart && i <= userEnd
	}
	return "", -1, false
}

// hasRealText 消息是否含非空 text 内容块（task_result/command 等引擎注入块不算）。
func hasRealText(m core.Message) bool {
	for _, c := range m.Content {
		if c.Type == "text" && strings.TrimSpace(c.Content) != "" {
			return true
		}
	}
	return false
}

// isSummaryMessage 按内容块类型识别摘要消息（滚动合并的锚点）。
func isSummaryMessage(m core.Message) bool {
	return len(m.Content) > 0 && m.Content[0].Type == summaryContentType
}

// extractSummary 从模型输出中提取 <summary>…</summary> 之间的内容。
// 未找到标签则原样返回（容错：模型未按双层格式输出）。
func extractSummary(md string) string {
	start := strings.Index(md, "<summary>")
	end := strings.Index(md, "</summary>")
	if start >= 0 && end > start {
		return strings.TrimSpace(md[start+len("<summary>") : end])
	}
	return md
}

// renderCompressInput 组装压缩输入（C1 标签化）：已有摘要（滚动合并）+ 任务锚点 +
// 对话历史，各块以标签包裹——标签与渲染头是结构不是内容（SummaryPromptV2 Input
// format 条款配套；防摘要逐字引用被 "New conversation history:" 之类的头污染，
// 实测污染率 30%）。锚点为空时省略 <task_anchor> 块。
func renderCompressInput(prevSummary, anchor string, history []core.Message) string {
	var b strings.Builder
	if prevSummary != "" {
		b.WriteString("<existing_summary>\n")
		b.WriteString(prevSummary)
		b.WriteString("\n</existing_summary>\n\n")
	}
	if anchor != "" {
		b.WriteString("<task_anchor>\nThe user's original request, preserved verbatim by the engine (authoritative — quote this in Main Requests and Intent):\n")
		b.WriteString(anchor)
		b.WriteString("\n</task_anchor>\n\n")
	}
	b.WriteString("<conversation_history>\n")
	for _, m := range history {
		fmt.Fprintf(&b, "\n[%s] %s\n", m.Role, messageText(m))
	}
	b.WriteString("</conversation_history>")
	return b.String()
}

// toolResultMaxChars 压缩输入中单条工具回执/任务结果的渲染上限（C6）：超出保留
// 头 2/3 + 尾 1/3，中段以截断标记替代——大工具输出曾把压缩输入撑到 ~190k tok；
// 截断只发生在压缩输入渲染路径（messageText 的 tool/task_result 分支），
// SystemPrompt 对比、锚点提取、旧摘要累计均不受影响；[tool_call] 参数行不截断
// （FileLedger 与摘要 File Context 的路径事实来源）。
const toolResultMaxChars = 3000

// truncateToolResult 超限结果的头尾保留渲染（rune 计长，中文安全）。
func truncateToolResult(s string) string {
	r := []rune(s)
	if len(r) <= toolResultMaxChars {
		return s
	}
	head := r[:toolResultMaxChars*2/3]
	tailStart := len(r) - toolResultMaxChars/3
	omitted := tailStart - len(head)
	return string(head) + fmt.Sprintf("\n[truncated ~%d chars]\n", omitted) + string(r[tailStart:])
}

// messageText 消息文本渲染（content 文本拼接 + 工具调用；reasoning 不参与总结）。
// task_result 块（后台任务完成消息）参与渲染：异步任务结果在压缩后不得消失
// （此前白名单不含 task_result，结果消息随压缩输入"渲染为空"——S1-D）。
// C6：tool 角色回执与 task_result 文本按 toolResultMaxChars 截断后渲染。
// 图片块渲染为占位标记（imageMarker，不把 base64 倒进压缩输入，但摘要器必须知道
// 「这里有一张图」——否则视觉任务压缩后摘要失真：模型以为读过的截图不存在，
// 可能重复读取或漏掉结论）。
func messageText(m core.Message) string {
	var b strings.Builder
	sawImage := false
	for _, c := range m.Content {
		switch c.Type {
		case core.ContentTypeImage:
			// 连续多图合并为一个标记（同 provider.ReplaceImagesWithPlaceholder 语义，
			// 避免一屏截图在压缩输入里刷成多行同样的占位）。
			if !sawImage {
				b.WriteString(imageMarker)
			}
			sawImage = true
		case "text", summaryContentType, core.ContentTypeTaskResult:
			if c.Type == core.ContentTypeTaskResult || (c.Type == "text" && m.Role == core.Tool) {
				b.WriteString(truncateToolResult(c.Content))
			} else {
				b.WriteString(c.Content)
			}
			sawImage = false
		}
	}
	for _, tc := range m.ToolCalls {
		fmt.Fprintf(&b, "\n[tool_call] %s(%s)", tc.Name, tc.Arguments)
	}
	return b.String()
}

// imageMarker 图片块在压缩输入渲染中的占位标记：摘要器据此知道「这里有一张图」，
// 视觉任务压缩后摘要不丢事实（此前图片块被整段跳过 → 摘要以为没读过图）。
const imageMarker = "[image]"
