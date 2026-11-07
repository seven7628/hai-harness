package main

// hub_events.go：群内流式草稿的生产侧（P3a-09，方案 §6.5.8）。
//
// 为什么在 bridge 合并而不是直接透传 content_chunk：
//   - 群视图只订阅白名单 hub_* 事件，不订阅 SDK 高频 chunk；
//   - 80ms 合并后 stdout 行数、序列化成本和多端差异都可控。
//
// 约束：
//   - delta 不落盘、不进快照、不进其他智能体上下文（只是 stdout 临时态）；
//   - 同一 message_id 的 index 从 0 单调递增；
//   - 单条 delta 上限 4KB；单条草稿最多 256KB，超出丢后续 delta，final 仍完整；
//   - final hub_message 复用 AgentStart 预分配的同一个 message_id。

import (
	"strings"
	"sync"
	"time"
)

const (
	hubDeltaInterval = 80 * time.Millisecond
	hubDeltaMaxBytes = 4 * 1024
	hubDraftMaxBytes = 256 * 1024
)

type hubDeltaSink struct {
	mu       sync.Mutex
	out      *out
	interval time.Duration

	groupID   string
	agentID   string
	roundID   string
	messageID string
	rootRunID string

	index   int64
	sent    int
	pending strings.Builder
	closed  bool
	timer   *time.Timer
}

func newHubDeltaSink(o *out) *hubDeltaSink {
	return &hubDeltaSink{out: o, interval: hubDeltaInterval}
}

// start 绑定本轮流式消息。
//
// 同一 messageID 的重复 AgentStart（续跑/重试）只刷新绑定，不重置 index/sent/pending：
// index 归零会让前端把后续 delta 当迟到丢弃（草稿冻死），pending.Reset 会吞掉窗口内正文。
// 只有 messageID 变化（新消息）才丢弃上一轮残留，避免串到新消息。
func (s *hubDeltaSink) start(groupID, agentID, roundID, messageID, runID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.groupID = groupID
	s.agentID = agentID
	s.roundID = roundID
	s.rootRunID = runID
	if messageID != "" && messageID == s.messageID {
		// 同一条消息：保持 index/sent/pending（以及 closed，已 final 的消息不再续流）。
		return
	}
	s.messageID = messageID
	s.index = 0
	s.sent = 0
	s.pending.Reset()
	s.closed = false
}

// feed 接收根 run 的正文增量；子 agent 的 chunk 按 runID 丢弃。
func (s *hubDeltaSink) feed(runID, delta string) {
	if s == nil || delta == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.messageID == "" {
		return
	}
	if runID != "" && s.rootRunID != "" && runID != s.rootRunID {
		return
	}
	if s.sent >= hubDraftMaxBytes {
		return
	}
	s.pending.WriteString(delta)
	if s.timer == nil {
		interval := s.interval
		if interval <= 0 {
			interval = hubDeltaInterval
		}
		s.timer = time.AfterFunc(interval, s.flush)
	}
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

// finish 在 final hub_message 之前调用：停表并同步 flush 最后一段。
func (s *hubDeltaSink) finish() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.closed = true
	s.flushLocked()
}

// close 丢弃未发送的草稿（会话关闭/进程退出，不再产生事件）。
func (s *hubDeltaSink) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.closed = true
	s.pending.Reset()
}

func (s *hubDeltaSink) flushLocked() {
	if s.closed && s.pending.Len() == 0 {
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
