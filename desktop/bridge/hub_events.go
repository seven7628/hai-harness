package main

// hub_events.go：Hub 流式草稿的生产侧（P3a-09，方案 §6.5.8；场景2b 按 LLM 轮次分段）。
//
// 为什么在 bridge 合并而不是直接透传 content_chunk：
//   - 群视图只订阅白名单 hub_* 事件，不订阅 SDK 高频 chunk；
//   - 80ms 合并后 stdout 行数、序列化成本和多端差异都可控。
//
// 约束：
//   - delta 不落盘、不进快照、不进其他智能体上下文（只是 stdout 临时态）；
//   - 一段 = 一次 LLM 调用（RequestId）：新段换 message_id、index 从 0 起；
//   - 单条 delta 上限 4KB；单段草稿最多 256KB，超出丢后续 delta，final 仍完整；
//   - 段正文以 LLMEnd.Content 为权威（草稿仅用于流式；Content 为空时用草稿兜底）；
//   - 第一段复用 AgentStart 预分配的 message_id（群 wake.MessageID），final 复用段 ID。

import (
	"strings"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

const (
	hubDeltaInterval = 80 * time.Millisecond
	hubDeltaMaxBytes = 4 * 1024
	hubDraftMaxBytes = 256 * 1024
)

// hubSegment 一段已收口的正文：一次 LLM 调用（RequestId）= 一条最终气泡。
// text 以 LLMEnd.Content 为权威（草稿只用于流式展示），messageID 以 msg_ 开头。
type hubSegment struct {
	messageID string
	requestID string
	text      string
}

// hubNonEmptySegments 过滤空段：LLMEnd.Content 为空且没有草稿的轮次不落账
// （纯工具轮 / 重试失败后没有正文的轮次）。顺序保持不变（= seq 顺序）。
func hubNonEmptySegments(segs []hubSegment) []hubSegment {
	if len(segs) == 0 {
		return nil
	}
	out := make([]hubSegment, 0, len(segs))
	for _, seg := range segs {
		if strings.TrimSpace(seg.text) == "" {
			continue
		}
		out = append(out, seg)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// hubDeltaSink 一次根 run 的流式草稿状态机：AgentStart 绑 run，LLMStart 开段，
// content_chunk 喂段，LLMEnd 收段，AgentEnd 取走所有非空段落账。
//
// 为什么按 RequestId 分段：一次 run 可以有多个 LLM 轮次（工具调用前后各一轮），
// ContentChunk.RequestId 是唯一可靠的分段键（Index 只是块序号）。旧实现把整轮 run
// 绑到同一个 message_id，前一站正文会被后一轮草稿覆盖/丢失，AgentEnd.Content 只
// 剩最后一轮。现在每段各自一条气泡，中间段不路由（见 finishHubRound）。
type hubDeltaSink struct {
	mu       sync.Mutex
	out      *out
	interval time.Duration

	// agentID 是 delta 事件的归属：DM 由 AgentStart 从会话带入（没有群唤醒头），
	// 群会话用 wake.AgentID 覆盖（同值）。
	agentID string

	bound     bool // 已收到本 run 的 AgentStart：没绑定就不流式（不知道段该落哪）
	groupID   string
	roundID   string
	rootRunID string
	firstID   string // AgentStart 预分配给「第一段」的 message_id（空 = 现场分配）

	// 当前段（一次 LLM 调用）
	active    bool
	messageID string
	requestID string
	index     int64
	sent      int
	pending   strings.Builder
	draft     strings.Builder
	timer     *time.Timer

	// 本 run 已收口的段（按 LLM 轮次顺序，AgentEnd 批量落账）
	segments []hubSegment
	closed   bool // AgentEnd 已收尾：本 run 不再接收 delta
}

func newHubDeltaSink(o *out) *hubDeltaSink {
	return &hubDeltaSink{out: o, interval: hubDeltaInterval}
}

// bindRun 绑定一次根 run（AgentStart）：记录 runID 与段归属，第一段复用
// firstMessageID（群会话的 wake.MessageID，保留「AgentStart 分配 → final 同 ID」）。
//
// 同一个 runId 的重复握手（续跑）只刷新绑定，不丢已收口段；换 run 才是新一轮。
func (s *hubDeltaSink) bindRun(runID, groupID, agentID, roundID, firstMessageID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if runID != "" && runID == s.rootRunID {
		s.groupID = groupID
		s.agentID = agentID
		s.roundID = roundID
		return
	}
	s.stopTimerLocked()
	s.bound = true
	s.rootRunID = runID
	s.groupID = groupID
	s.agentID = agentID
	s.roundID = roundID
	s.firstID = firstMessageID
	s.segments = nil
	s.closed = false
	s.resetSegmentLocked()
}

// beginSegment 开始一段新草稿（LLMStart）：新 message_id、index 归零、pending 清空。
// 上一段没收口（没有对应 LLMEnd）的草稿直接丢弃 —— 半截正文不能冻成气泡。
func (s *hubDeltaSink) beginSegment(runID, requestID string) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.bound || !s.isRootLocked(runID) {
		return ""
	}
	s.resetSegmentLocked()
	id := s.firstID
	if id == "" {
		id = agenthub.NewID(agenthub.PrefixMessage)
	}
	s.firstID = ""
	s.active = true
	s.messageID = id
	s.requestID = requestID
	return id
}

// feed 接收当前段的正文增量：只认根 run + 当前段的 requestID。
// 子 agent 的 chunk 按 runID 丢弃；迟到/上一轮（含重试失败那次）的 chunk 按
// requestID 丢弃，避免串进新气泡。
//
// 空 requestID 视作通配：有的网关第一个 SSE 帧不带 id，provider 先把 LLMStart 发成
// RequestId=""，之后才从 chunk.ID 拿到真正的请求 id（见 provider/openai.go），此时
// 正文 chunk 的 id 与段头不同 —— 严格相等会把整轮正文丢掉。同一 run 内 LLM 调用严格
// 串行、段也只开一个，通配不会把两轮混进同一段。
func (s *hubDeltaSink) feed(runID, requestID, delta string) {
	if s == nil || delta == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.active {
		return
	}
	if !s.isRootLocked(runID) || !sameRequest(requestID, s.requestID) {
		return
	}
	if s.sent >= hubDraftMaxBytes {
		return
	}
	s.pending.WriteString(delta)
	if s.draft.Len() < hubDraftMaxBytes {
		s.draft.WriteString(delta)
	}
	if s.timer == nil {
		interval := s.interval
		if interval <= 0 {
			interval = hubDeltaInterval
		}
		s.timer = time.AfterFunc(interval, s.flush)
	}
}

// discardSegment 丢弃当前段（LLMError{WillRetry}）：半截正文不落账、不冻成气泡。
// 重试会带着新的 RequestId 再走一次 beginSegment（新 message_id）。
func (s *hubDeltaSink) discardSegment(runID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.active || !s.isRootLocked(runID) {
		return
	}
	s.resetSegmentLocked()
}

// closeSegment 收口当前段（LLMEnd）：先 flush 尾巴，再记正文。
//
// 文本以 LLMEnd.Content 为权威（不能用草稿拼接：流式 delta 可能有丢块/截断）；
// Content 为空时用草稿兜底（否则已流出去的正文永远等不到 final）。空段不记录。
func (s *hubDeltaSink) closeSegment(runID, content string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.active || !s.isRootLocked(runID) {
		return
	}
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.flushLocked()
	text := content
	if strings.TrimSpace(text) == "" {
		text = s.draft.String()
	}
	text = strings.TrimSpace(text)
	if text != "" && s.messageID != "" {
		s.segments = append(s.segments, hubSegment{
			messageID: s.messageID,
			requestID: s.requestID,
			text:      text,
		})
	}
	s.resetSegmentLocked()
}

// flush 立即发送当前合并段（工具开始/审批前调用，避免正文卡在 80ms 窗口里）。
func (s *hubDeltaSink) flush() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		// 工具边界主动 flush：先停表，否则已经排队的回调会在后续窗口里多 flush 一次，
		// 把「80ms 合并」的节奏打乱（孤儿定时器）。
		s.timer.Stop()
		s.timer = nil
	}
	s.flushLocked()
}

// finish 在本轮 AgentEnd 落账之前调用：停表、flush 尾巴，返回本轮已收口的段。
//
// 没有 LLMEnd 的开放段（中断/取消）不记录（按现状丢弃），由调用方回退
// AgentEnd.Content 的单条行为。
func (s *hubDeltaSink) finish() []hubSegment {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	if s.active {
		s.flushLocked()
	}
	s.closed = true
	s.active = false
	segs := s.segments
	s.segments = nil
	return segs
}

// close 丢弃未发送的草稿（会话关闭/进程退出，不再产生事件）。
func (s *hubDeltaSink) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopTimerLocked()
	s.closed = true
	s.active = false
	s.pending.Reset()
	s.draft.Reset()
	s.segments = nil
}

// isRootLocked 只放行根 run：子 agent 的 LLM 事件也走同一个 emit。
func (s *hubDeltaSink) isRootLocked(runID string) bool {
	return runID == "" || s.rootRunID == "" || runID == s.rootRunID
}

// sameRequest 比对段键；任一侧为空视作通配（理由见 feed 注释）。
func sameRequest(a, b string) bool {
	return a == b || a == "" || b == ""
}

func (s *hubDeltaSink) stopTimerLocked() {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}

func (s *hubDeltaSink) resetSegmentLocked() {
	s.stopTimerLocked()
	s.active = false
	s.messageID = ""
	s.requestID = ""
	s.index = 0
	s.sent = 0
	s.pending.Reset()
	s.draft.Reset()
}

func (s *hubDeltaSink) flushLocked() {
	if !s.active || s.messageID == "" {
		return
	}
	text := s.pending.String()
	s.pending.Reset()
	for text != "" {
		if s.sent >= hubDraftMaxBytes {
			return
		}
		chunk, rest := takeUTF8Bytes(text, hubDeltaMaxBytes)
		// 256KB 硬上限：最后一块按剩余额度截断，后面的 delta 直接丢弃。
		if remain := hubDraftMaxBytes - s.sent; len(chunk) > remain {
			chunk = truncateUTF8(chunk, remain)
			rest = ""
		}
		if chunk == "" {
			return
		}
		if s.out != nil {
			s.out.writeLine(map[string]any{
				"event_type": "hub_message_delta",
				"group_id":   s.groupID,
				"agent_id":   s.agentID,
				"round_id":   s.roundID,
				"message_id": s.messageID,
				"index":      s.index,
				"delta":      chunk,
			})
		}
		s.index++
		s.sent += len(chunk)
		text = rest
	}
}

// takeUTF8Bytes 按 UTF-8 边界切出最多 max 字节；rest 为余下文本。
func takeUTF8Bytes(s string, max int) (head, rest string) {
	if max <= 0 || len(s) <= max {
		return s, ""
	}
	cut := max
	for cut > 0 && (s[cut]&0xC0) == 0x80 { // 落在 continuation byte 上，往前退
		cut--
	}
	return s[:cut], s[cut:]
}
