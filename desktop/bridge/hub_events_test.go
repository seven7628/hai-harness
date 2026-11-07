package main

// P3a-09：群内流式 delta 的聚合与 final 同 ID 回归。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

type streamProvider struct {
	chunks []string
	reply  string
}

func (p *streamProvider) Stream(_ context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	for i, c := range p.chunks {
		if err := h(provider.LLMContentDeltaEvent{Delta: c, Index: int64(i)}); err != nil {
			return err
		}
	}
	return h(provider.LLMEndEvent{
		FinishReason: core.FinishReasonStop,
		Content:      p.reply,
		Model:        req.Model,
		Usage:        core.Usage{Input: 3, Output: 2},
	})
}

// rawEventOffset 返回 raw 里最后一条满足 match 的事件行的字节偏移（没有则 -1）。
//
// 为什么用原始偏移：要断言的是 stdout 上「谁先写出去」的真实顺序，解析成 map 之后
// 顺序信息就不存在了（readAllLines 也只能给出切片顺序，无法证明行内先后语义）。
func rawEventOffset(raw string, match func(map[string]any) bool) int {
	off, found := 0, -1
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) != "" {
			var ev map[string]any
			if json.Unmarshal([]byte(line), &ev) == nil && match(ev) {
				found = off
			}
		}
		off += len(line) + 1
	}
	return found
}

// waitEventOffsets 等到 final 事件出现，并返回同一份 raw 快照里 delta/final 两条行的偏移。
// 必须同一份快照：两次读之间 buffer 还在增长，跨快照比较偏移没有意义。
func waitEventOffsets(t *testing.T, buf *syncBuf, want, final func(map[string]any) bool) (deltaAt, finalAt int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw := buf.String()
		deltaAt, finalAt = rawEventOffset(raw, want), rawEventOffset(raw, final)
		if finalAt >= 0 && deltaAt >= 0 {
			return deltaAt, finalAt
		}
		if time.Now().After(deadline) {
			t.Fatalf("事件流里缺少行：delta offset=%d, final offset=%d", deltaAt, finalAt)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func deltaEvents(t *testing.T, buf *syncBuf) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ev := range hubEvents(t, buf, "hub_message_delta") {
		out = append(out, ev)
	}
	return out
}

func TestHubDeltaSinkMergesWithinWindow(t *testing.T) {
	buf := &syncBuf{}
	sink := newHubDeltaSink(newOut(buf))
	sink.interval = 10 * time.Millisecond
	sink.start("grp_x", "ag_x", "rnd_x", "msg_x", "run_1")
	sink.feed("run_1", "你")
	sink.feed("run_1", "好")
	sink.feed("run_1", "呀")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(deltaEvents(t, buf)) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	evs := deltaEvents(t, buf)
	if len(evs) != 1 {
		t.Fatalf("deltas = %d, want 1 merged within window", len(evs))
	}
	if evs[0]["delta"] != "你好呀" || int(evs[0]["index"].(float64)) != 0 {
		t.Fatalf("delta event = %+v", evs[0])
	}
}

func TestHubDeltaSinkFinishFlushesAndDropsLate(t *testing.T) {
	buf := &syncBuf{}
	sink := newHubDeltaSink(newOut(buf))
	sink.interval = time.Hour // 只能由 finish 触发
	sink.start("grp_x", "ag_x", "rnd_x", "msg_x", "run_1")
	sink.feed("run_1", "最后一段")
	sink.finish()
	evs := deltaEvents(t, buf)
	if len(evs) != 1 || evs[0]["delta"] != "最后一段" {
		t.Fatalf("finish did not flush: %+v", evs)
	}
	sink.feed("run_1", "late")
	if got := len(deltaEvents(t, buf)); got != 1 {
		t.Fatalf("late delta after finish leaked: %d", got)
	}
}

func TestHubDeltaSinkCapsChunkAndDraft(t *testing.T) {
	buf := &syncBuf{}
	sink := newHubDeltaSink(newOut(buf))
	sink.interval = time.Hour
	sink.start("grp_x", "ag_x", "rnd_x", "msg_x", "run_1")
	sink.feed("run_1", strings.Repeat("a", 9000))
	sink.finish()
	evs := deltaEvents(t, buf)
	if len(evs) != 3 {
		t.Fatalf("deltas = %d, want 3 (4096+4096+808)", len(evs))
	}
	var all strings.Builder
	for i, ev := range evs {
		chunk := ev["delta"].(string)
		if len(chunk) > hubDeltaMaxBytes {
			t.Fatalf("delta %d = %d bytes, cap %d", i, len(chunk), hubDeltaMaxBytes)
		}
		if int(ev["index"].(float64)) != i {
			t.Fatalf("index[%d] = %v", i, ev["index"])
		}
		all.WriteString(chunk)
	}
	if all.Len() != 9000 {
		t.Fatalf("concatenated = %d, want 9000", all.Len())
	}

	buf2 := &syncBuf{}
	sink2 := newHubDeltaSink(newOut(buf2))
	sink2.interval = time.Hour
	sink2.start("grp_x", "ag_x", "rnd_x", "msg_x", "run_1")
	sink2.feed("run_1", strings.Repeat("b", hubDraftMaxBytes+5000))
	sink2.finish()
	sum := 0
	for _, ev := range deltaEvents(t, buf2) {
		sum += len(ev["delta"].(string))
	}
	if sum > hubDraftMaxBytes {
		t.Fatalf("draft emitted %d bytes, cap %d", sum, hubDraftMaxBytes)
	}
}

// 最后一块跨过 256KB 上限时走 flushLocked 的「按剩余额度截断」分支。
// 常规 4KB 对齐的正文永远正好落在上限上（4096 整除 256KB），覆盖不到这条分支；
// 先用 4095 字节把偏移错开，末块就只剩 1 字节额度。
func TestHubDeltaSinkTruncatesTailAtDraftCap(t *testing.T) {
	buf := &syncBuf{}
	sink := newHubDeltaSink(newOut(buf))
	sink.interval = time.Hour
	sink.start("grp_x", "ag_x", "rnd_x", "msg_x", "run_1")
	const presend = 4095
	sink.sent = presend // 模拟已发 4095 字节
	sink.feed("run_1", strings.Repeat("c", 258300))
	sink.finish()
	sum := 0
	for _, ev := range deltaEvents(t, buf) {
		sum += len(ev["delta"].(string))
	}
	if sum > hubDraftMaxBytes {
		t.Fatalf("delta 合计 %d 字节，超过上限 %d", sum, hubDraftMaxBytes)
	}
	if sum+presend != hubDraftMaxBytes || sink.sent != hubDraftMaxBytes {
		t.Fatalf("尾块没有按剩余额度截断：emitted=%d + presend=%d = %d，sent=%d，上限 %d",
			sum, presend, sum+presend, sink.sent, hubDraftMaxBytes)
	}
}

// 同一 messageID 的重复 AgentStart（续跑/重试）不能重置 index/sent/pending：
// 前端按 index 单调丢弃迟到 delta，index 归零后后续 delta 全被扔掉（草稿冻住）。
func TestHubDeltaSinkStartKeepsSameMessageState(t *testing.T) {
	buf := &syncBuf{}
	sink := newHubDeltaSink(newOut(buf))
	sink.interval = time.Hour
	sink.start("grp_x", "ag_x", "rnd_x", "msg_x", "run_1")
	sink.feed("run_1", "前半")
	sink.flush() // 已发出 index 0
	sink.start("grp_x", "ag_x", "rnd_x", "msg_x", "run_1")
	sink.feed("run_1", "后半")
	sink.start("grp_x", "ag_x", "rnd_x", "msg_x", "run_1") // pending 里正躺着「后半」
	sink.finish()
	evs := deltaEvents(t, buf)
	if len(evs) != 2 {
		t.Fatalf("deltas = %d, want 2（重复 start 丢掉了 pending）: %+v", len(evs), evs)
	}
	if evs[0]["delta"] != "前半" || int(evs[0]["index"].(float64)) != 0 {
		t.Fatalf("delta[0] = %+v", evs[0])
	}
	if evs[1]["delta"] != "后半" || int(evs[1]["index"].(float64)) != 1 {
		t.Fatalf("delta[1] = %+v（重复 start 把 index 归零了）", evs[1])
	}

	// 换成新 messageID 仍是「新消息」语义：上一轮残留不能串进新气泡。
	buf2 := &syncBuf{}
	sink2 := newHubDeltaSink(newOut(buf2))
	sink2.interval = time.Hour
	sink2.start("grp_x", "ag_x", "rnd_x", "msg_a", "run_1")
	sink2.feed("run_1", "上一轮残留")
	sink2.start("grp_x", "ag_x", "rnd_x", "msg_b", "run_2")
	sink2.feed("run_2", "新消息")
	sink2.finish()
	next := deltaEvents(t, buf2)
	if len(next) != 1 || next[0]["message_id"] != "msg_b" || next[0]["delta"] != "新消息" || int(next[0]["index"].(float64)) != 0 {
		t.Fatalf("新 messageID 没有清掉上一轮残留: %+v", next)
	}
}

func TestHubDeltaSinkIgnoresSubagentRun(t *testing.T) {
	buf := &syncBuf{}
	sink := newHubDeltaSink(newOut(buf))
	sink.interval = time.Hour
	sink.start("grp_x", "ag_x", "rnd_x", "msg_x", "root")
	sink.feed("child", "子 agent 的正文")
	sink.finish()
	if evs := deltaEvents(t, buf); len(evs) != 0 {
		t.Fatalf("subagent chunk leaked: %+v", evs)
	}
}

// flush() 停在排的定时器：只把 timer 置 nil 而不 Stop()，回调稍后仍会补一次 flush，
// 「80ms 合并」的节奏被打乱（孤儿定时器），工具边界之后的窗口里会冒出多余 delta。
//
// 判定手法：time.Timer.Stop 对「已经停过/已触发」的表返回 false，对还活着的返回 true。
// 所以 flush 之后再 Stop 一次得到 false 才说明它真的停了表；只置 nil 的话这里会返回 true。
func TestHubDeltaSinkFlushStopsTimer(t *testing.T) {
	buf := &syncBuf{}
	sink := newHubDeltaSink(newOut(buf))
	sink.interval = time.Hour // 这个窗口内只有显式 flush 会发事件
	sink.start("grp_x", "ag_x", "rnd_x", "msg_x", "run_1")
	sink.feed("run_1", "工具前正文")

	sink.mu.Lock()
	timer := sink.timer
	sink.mu.Unlock()
	if timer == nil {
		t.Fatal("feed 没有排定时器")
	}

	sink.flush()
	// flush 自己要立即把正文发出去（工具边界不能等窗口）。
	evs := deltaEvents(t, buf)
	if len(evs) != 1 || evs[0]["delta"] != "工具前正文" || int(evs[0]["index"].(float64)) != 0 {
		t.Fatalf("flush 没有立即发出正文: %+v", evs)
	}
	if timer.Stop() {
		t.Fatal("flush 只把 timer 置 nil 没有 Stop()：已排队的回调还会再 flush 一次")
	}
	sink.mu.Lock()
	left := sink.timer
	sink.mu.Unlock()
	if left != nil {
		t.Fatalf("flush 后 timer = %v, want nil", left)
	}
}

// 已 final 的 message 不续流：同一 messageID 在 finish() 之后再 start()（续跑/重试）
// 只刷新绑定，closed 保持 true。否则续跑正文会接着旧的 index 继续发 delta，
// 而这条消息在前端已经定稿，迟到 delta 只会被丢弃（并可能让草稿状态错乱）。
func TestHubDeltaSinkStartSameMessageIDAfterFinishStaysClosed(t *testing.T) {
	buf := &syncBuf{}
	sink := newHubDeltaSink(newOut(buf))
	sink.interval = time.Hour
	sink.start("grp_x", "ag_x", "rnd_x", "msg_x", "run_1")
	sink.feed("run_1", "第一段")
	sink.finish()
	if evs := deltaEvents(t, buf); len(evs) != 1 || evs[0]["delta"] != "第一段" {
		t.Fatalf("finish 没有 flush 第一段: %+v", evs)
	}

	sink.start("grp_x", "ag_x", "rnd_x", "msg_x", "run_2")
	if !sink.closed {
		t.Fatal("同 messageID 的 start 把 closed 复位了：已 final 的消息不能续流")
	}
	sink.feed("run_2", "续跑正文")
	sink.finish()
	evs := deltaEvents(t, buf)
	if len(evs) != 1 {
		t.Fatalf("续跑后又冒出了 delta（共 %d 条）: %+v", len(evs), evs)
	}
	if sink.index != 1 {
		t.Fatalf("index = %d, want 1（续跑正文不应推进 index）", sink.index)
	}
}

func TestHubGroupStreamDeltaFinalSameMessageID(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "StreamOwner")
	b := createHubAgent(t, m, buf, 2, "StreamB")
	g := groupWithMembers(t, m, buf, 3, "流式群",
		struct {
			id   agenthub.AgentID
			role string
		}{a.ID, "owner"},
		struct {
			id   agenthub.AgentID
			role string
		}{b.ID, "member"},
	)
	bs, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	if bs.hubDelta == nil {
		t.Fatal("群会话没有 hubDelta sink")
	}
	// 只让 finish() 能 flush 尾巴：默认 80ms 定时器可能在 AgentEnd 之前就把 pending 发出去，
	// 那样即使 finish() 被挪到 final 之后（顺序 bug），delta 也早已先落账，测试会误判通过
	// （实测变异体 10 次里过 3 次）。窗口拉到 1 小时，尾部的先后就只由 finish() 的位置决定。
	bs.hubDelta.interval = time.Hour
	setGroupFakeLoop(t, m, bs, g, string(b.ID), &streamProvider{
		chunks: []string{"B 正在", "回复"},
		reply:  "B 正在回复",
	})
	ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@StreamB 说一句",
	})
	if !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	msgs := waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		for _, msg := range msgs {
			if msg.Sender.AgentID == b.ID && msg.Text == "B 正在回复" {
				return true
			}
		}
		return false
	})
	var final *agenthub.Message
	for i := range msgs {
		if msgs[i].Sender.AgentID == b.ID && msgs[i].Text == "B 正在回复" {
			final = &msgs[i]
		}
	}
	if final == nil {
		t.Fatal("final message missing")
	}
	if !strings.HasPrefix(final.ID, "msg_") || final.ID == agenthub.MessageID(final.Seq) {
		t.Fatalf("final ID = %q (seq %d): wanted preallocated stream ID", final.ID, final.Seq)
	}
	evs := deltaEvents(t, buf)
	if len(evs) == 0 {
		t.Fatal("no hub_message_delta events")
	}
	var text strings.Builder
	for i, ev := range evs {
		if ev["message_id"] != final.ID {
			t.Fatalf("delta message_id = %v, final = %s", ev["message_id"], final.ID)
		}
		if int(ev["index"].(float64)) != i {
			t.Fatalf("delta index[%d] = %v", i, ev["index"])
		}
		text.WriteString(ev["delta"].(string))
	}
	if text.String() != final.Text {
		t.Fatalf("delta text = %q, final = %q", text.String(), final.Text)
	}

	// 核心顺序：最后一段 delta（final flush）必须先于 final hub_message 写出。
	// 反了就说明 finish() 落在了 finishHubRound 之后，前端会先 final 再收到 delta 并丢弃
	// （草稿在 final 里补齐，但流式尾巴永远看不到）。
	deltaAt, finalAt := waitEventOffsets(t, buf,
		func(ev map[string]any) bool {
			return ev["event_type"] == "hub_message_delta" && ev["message_id"] == final.ID
		},
		func(ev map[string]any) bool {
			if ev["event_type"] != "hub_message" {
				return false
			}
			msg, _ := ev["message"].(map[string]any)
			return msg != nil && msg["id"] == final.ID
		},
	)
	if deltaAt > finalAt {
		t.Fatalf("顺序反了：final hub_message 在 offset %d，最后一段 delta 在 %d（final flush 必须先于 final 落账）", finalAt, deltaAt)
	}
}
