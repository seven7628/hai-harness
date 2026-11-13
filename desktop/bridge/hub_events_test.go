package main

// P3a-09：群内流式 delta 的聚合与 final 同 ID 回归。
// 场景2b：按 LLM 轮次（RequestId）分段 —— 一段一条草稿、一段一条气泡。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

// streamProvider 单轮流式 fake：先发 LLMStart（分段键 RequestId），再发正文增量，
// 最后 LLMEnd 带权威正文。真实 provider 都发 LLMStart —— 少发它就开不出段，
// 草稿流会整段消失（所以这里必须与生产同形）。
type streamProvider struct {
	chunks    []string
	reply     string
	requestID string // 空 = "req_stream"
}

func (p *streamProvider) Stream(_ context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	rid := p.requestID
	if rid == "" {
		rid = "req_stream"
	}
	if err := h(provider.LLMStartEvent{RequestId: rid, Model: req.Model, Timestamp: time.Now()}); err != nil {
		return err
	}
	for i, c := range p.chunks {
		if err := h(provider.LLMContentDeltaEvent{RequestId: rid, Delta: c, Index: int64(i)}); err != nil {
			return err
		}
	}
	return h(provider.LLMEndEvent{
		RequestId:    rid,
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

// startSinkRun 绑定一次根 run 并开第一段（只关心单段流式的用例用它起手）。
// 第一段的 message_id 复用 bindRun 传入的预分配 ID（群 wake.MessageID）。
func startSinkRun(sink *hubDeltaSink, runID, requestID string) {
	sink.bindRun(runID, "grp_x", "ag_x", "rnd_x", "msg_x")
	sink.beginSegment(runID, requestID)
}

func TestHubDeltaSinkMergesWithinWindow(t *testing.T) {
	buf := &syncBuf{}
	sink := newHubDeltaSink(newOut(buf))
	sink.interval = 10 * time.Millisecond
	startSinkRun(sink, "run_1", "req_1")
	sink.feed("run_1", "req_1", "你")
	sink.feed("run_1", "req_1", "好")
	sink.feed("run_1", "req_1", "呀")
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
	startSinkRun(sink, "run_1", "req_1")
	sink.feed("run_1", "req_1", "最后一段")
	sink.finish()
	evs := deltaEvents(t, buf)
	if len(evs) != 1 || evs[0]["delta"] != "最后一段" {
		t.Fatalf("finish did not flush: %+v", evs)
	}
	sink.feed("run_1", "req_1", "late")
	if got := len(deltaEvents(t, buf)); got != 1 {
		t.Fatalf("late delta after finish leaked: %d", got)
	}
}

func TestHubDeltaSinkCapsChunkAndDraft(t *testing.T) {
	buf := &syncBuf{}
	sink := newHubDeltaSink(newOut(buf))
	sink.interval = time.Hour
	startSinkRun(sink, "run_1", "req_1")
	sink.feed("run_1", "req_1", strings.Repeat("a", 9000))
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
	startSinkRun(sink2, "run_1", "req_1")
	sink2.feed("run_1", "req_1", strings.Repeat("b", hubDraftMaxBytes+5000))
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
	startSinkRun(sink, "run_1", "req_1")
	const presend = 4095
	sink.sent = presend // 模拟已发 4095 字节
	sink.feed("run_1", "req_1", strings.Repeat("c", 258300))
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

// 按 RequestId 分段：一次 run 里的多次 LLM 调用各开一段、各自 message_id 与 index，
// 段文本以 LLMEnd.Content 为权威，顺序 = 调用顺序。
func TestHubDeltaSinkSegmentsByRequestID(t *testing.T) {
	buf := &syncBuf{}
	sink := newHubDeltaSink(newOut(buf))
	sink.interval = time.Hour
	sink.bindRun("run_1", "grp_x", "ag_x", "rnd_x", "msg_first")
	first := sink.beginSegment("run_1", "req_1")
	if first != "msg_first" {
		t.Fatalf("第一段 message_id = %q，应复用 AgentStart 预分配的 msg_first", first)
	}
	sink.feed("run_1", "req_1", "工具前的正文（草稿）")
	sink.closeSegment("run_1", "第一轮正文")
	second := sink.beginSegment("run_1", "req_2")
	if second == "" || second == first {
		t.Fatalf("第二段 message_id = %q，必须新分配（第一段是 %q）", second, first)
	}
	sink.feed("run_1", "req_2", "工具后的正文")
	sink.closeSegment("run_1", "第二轮正文")
	segs := sink.finish()
	if len(segs) != 2 {
		t.Fatalf("segments = %d, want 2: %+v", len(segs), segs)
	}
	if segs[0].messageID != first || segs[0].text != "第一轮正文" || segs[0].requestID != "req_1" {
		t.Fatalf("seg[0] = %+v", segs[0])
	}
	if segs[1].messageID != second || segs[1].text != "第二轮正文" || segs[1].requestID != "req_2" {
		t.Fatalf("seg[1] = %+v", segs[1])
	}
	if !strings.HasPrefix(segs[0].messageID, "msg_") || !strings.HasPrefix(segs[1].messageID, "msg_") {
		t.Fatalf("段的 message_id 必须 msg_ 前缀：%q / %q", segs[0].messageID, segs[1].messageID)
	}
	evs := deltaEvents(t, buf)
	if len(evs) != 2 {
		t.Fatalf("deltas = %d, want 2: %+v", len(evs), evs)
	}
	for i, ev := range evs {
		wantID, wantText := segs[0].messageID, "工具前的正文（草稿）"
		if i == 1 {
			wantID, wantText = segs[1].messageID, "工具后的正文"
		}
		if ev["message_id"] != wantID || ev["delta"] != wantText || int(ev["index"].(float64)) != 0 {
			t.Fatalf("delta[%d] = %+v，want id=%s text=%q index=0", i, ev, wantID, wantText)
		}
	}
}

// 段的 chunk 只认当前 RequestId：上一轮/重试失败那次的迟到 chunk 不能串进新气泡。
func TestHubDeltaSinkDropsOtherRequestID(t *testing.T) {
	buf := &syncBuf{}
	sink := newHubDeltaSink(newOut(buf))
	sink.interval = time.Hour
	startSinkRun(sink, "run_1", "req_1")
	sink.feed("run_1", "req_0", "上一轮的迟到尾巴")
	sink.feed("run_1", "req_1", "本轮正文")
	sink.closeSegment("run_1", "本轮正文")
	segs := sink.finish()
	if len(segs) != 1 || segs[0].text != "本轮正文" {
		t.Fatalf("segments = %+v", segs)
	}
	evs := deltaEvents(t, buf)
	if len(evs) != 1 || evs[0]["delta"] != "本轮正文" {
		t.Fatalf("迟到 chunk 串进来了: %+v", evs)
	}
}

// LLMError{WillRetry}：失败那次已流出的半截正文不能落账/冻成气泡；重试的新 RequestId
// 开新段、新 message_id。
func TestHubDeltaSinkDiscardSegmentOnRetry(t *testing.T) {
	buf := &syncBuf{}
	sink := newHubDeltaSink(newOut(buf))
	sink.interval = time.Hour
	startSinkRun(sink, "run_1", "req_fail")
	sink.feed("run_1", "req_fail", "半截正文")
	sink.discardSegment("run_1")
	// 失败那次的迟到 chunk 也不能把已丢弃的段复活。
	sink.feed("run_1", "req_fail", "迟到的半截")
	retryID := sink.beginSegment("run_1", "req_ok")
	if retryID == "" {
		t.Fatal("重试没有开新段")
	}
	sink.feed("run_1", "req_ok", "重试成功正文")
	sink.closeSegment("run_1", "重试成功正文")
	segs := sink.finish()
	if len(segs) != 1 || segs[0].text != "重试成功正文" || segs[0].messageID != retryID {
		t.Fatalf("segments = %+v（失败那次的半截正文不能落账）", segs)
	}
	evs := deltaEvents(t, buf)
	if len(evs) != 1 || evs[0]["delta"] != "重试成功正文" {
		t.Fatalf("丢弃段的 delta 泄漏: %+v", evs)
	}
}

// LLMEnd.Content 为空（provider 只发了 delta）时用草稿兜底，否则已流出去的正文
// 永远等不到 final；草稿也为空就是真·空段（纯工具轮）。
func TestHubDeltaSinkEmptyContentFallsBackToDraft(t *testing.T) {
	buf := &syncBuf{}
	sink := newHubDeltaSink(newOut(buf))
	sink.interval = time.Hour
	sink.bindRun("run_1", "grp_x", "ag_x", "rnd_x", "")
	sink.beginSegment("run_1", "req_1")
	sink.feed("run_1", "req_1", "只有草稿")
	sink.closeSegment("run_1", "")
	sink.beginSegment("run_1", "req_2")
	sink.closeSegment("run_1", "") // 工具轮：没有任何正文
	segs := sink.finish()
	if len(segs) != 1 || segs[0].text != "只有草稿" {
		t.Fatalf("segments = %+v，want 1 条草稿兜底段", segs)
	}
}

// 同一个 run 的重复 AgentStart（续跑）只刷新绑定：已发 index、pending、已收口段都不能丢
// （index 归零会让前端把后续 delta 当迟到丢弃，草稿冻死）。
func TestHubDeltaSinkBindRunKeepsSameRunState(t *testing.T) {
	buf := &syncBuf{}
	sink := newHubDeltaSink(newOut(buf))
	sink.interval = time.Hour
	sink.bindRun("run_1", "grp_x", "ag_x", "rnd_x", "msg_a")
	sink.beginSegment("run_1", "req_1")
	sink.feed("run_1", "req_1", "前半")
	sink.flush() // 已发出 index 0
	sink.bindRun("run_1", "grp_x", "ag_x", "rnd_x", "")
	sink.feed("run_1", "req_1", "后半")
	sink.bindRun("run_1", "grp_x", "ag_x", "rnd_x", "") // pending 里正躺着「后半」
	sink.closeSegment("run_1", "前半后半")
	segs := sink.finish()
	evs := deltaEvents(t, buf)
	if len(evs) != 2 {
		t.Fatalf("deltas = %d, want 2（重复 bindRun 丢掉了 pending）: %+v", len(evs), evs)
	}
	if evs[0]["delta"] != "前半" || int(evs[0]["index"].(float64)) != 0 {
		t.Fatalf("delta[0] = %+v", evs[0])
	}
	if evs[1]["delta"] != "后半" || int(evs[1]["index"].(float64)) != 1 {
		t.Fatalf("delta[1] = %+v（重复 bindRun 把 index 归零了）", evs[1])
	}
	if len(segs) != 1 || segs[0].text != "前半后半" || segs[0].messageID != "msg_a" {
		t.Fatalf("segments = %+v", segs)
	}

	// 换 run 才是新消息：上一轮残留（未收口段 + pending）不能串进新气泡。
	buf2 := &syncBuf{}
	sink2 := newHubDeltaSink(newOut(buf2))
	sink2.interval = time.Hour
	sink2.bindRun("run_1", "grp_x", "ag_x", "rnd_x", "msg_a")
	sink2.beginSegment("run_1", "req_1")
	sink2.feed("run_1", "req_1", "上一轮残留")
	sink2.bindRun("run_2", "grp_x", "ag_x", "rnd_x", "msg_b")
	sink2.beginSegment("run_2", "req_2")
	sink2.feed("run_2", "req_2", "新消息")
	sink2.closeSegment("run_2", "新消息")
	segs2 := sink2.finish()
	next := deltaEvents(t, buf2)
	if len(next) != 1 || next[0]["message_id"] != "msg_b" || next[0]["delta"] != "新消息" || int(next[0]["index"].(float64)) != 0 {
		t.Fatalf("新 run 没有清掉上一轮残留: %+v", next)
	}
	if len(segs2) != 1 || segs2[0].text != "新消息" {
		t.Fatalf("segments = %+v（上一轮的开放段不能带进新 run）", segs2)
	}
}

func TestHubDeltaSinkIgnoresSubagentRun(t *testing.T) {
	buf := &syncBuf{}
	sink := newHubDeltaSink(newOut(buf))
	sink.interval = time.Hour
	sink.bindRun("root", "grp_x", "ag_x", "rnd_x", "msg_x")
	sink.beginSegment("root", "req_root")
	if id := sink.beginSegment("child", "req_child"); id != "" {
		t.Fatalf("子 run 开出了段 %q", id)
	}
	sink.feed("child", "req_child", "子 agent 的正文")
	sink.closeSegment("child", "子 agent 的正文")
	sink.closeSegment("root", "")
	segs := sink.finish()
	if evs := deltaEvents(t, buf); len(evs) != 0 {
		t.Fatalf("subagent chunk leaked: %+v", evs)
	}
	if len(segs) != 0 {
		t.Fatalf("子 run 的正文进了段落账: %+v", segs)
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
	startSinkRun(sink, "run_1", "req_1")
	sink.feed("run_1", "req_1", "工具前正文")

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

// finish()（AgentEnd）之后 sink 关闭：同一个 run 再握手也不能续流（这条 run 的段已
// 经落账，迟到 delta 只会被前端丢弃）；新 run 才重新打开。
func TestHubDeltaSinkFinishClosesRun(t *testing.T) {
	buf := &syncBuf{}
	sink := newHubDeltaSink(newOut(buf))
	sink.interval = time.Hour
	sink.bindRun("run_1", "grp_x", "ag_x", "rnd_x", "msg_x")
	sink.beginSegment("run_1", "req_1")
	sink.feed("run_1", "req_1", "第一段")
	sink.closeSegment("run_1", "第一段")
	if segs := sink.finish(); len(segs) != 1 || segs[0].text != "第一段" {
		t.Fatalf("finish 没有交出第一段: %+v", segs)
	}
	if evs := deltaEvents(t, buf); len(evs) != 1 {
		t.Fatalf("finish 没有 flush 第一段: %+v", evs)
	}

	sink.bindRun("run_1", "grp_x", "ag_x", "rnd_x", "")
	if id := sink.beginSegment("run_1", "req_2"); id != "" {
		t.Fatalf("已收尾的 run 又开出了段 %q", id)
	}
	sink.feed("run_1", "req_2", "续跑正文")
	sink.finish()
	if evs := deltaEvents(t, buf); len(evs) != 1 {
		t.Fatalf("续跑后又冒出了 delta（共 %d 条）: %+v", len(evs), evs)
	}

	// 新 run 重新打开：段与 delta 都从 0 起。
	sink.bindRun("run_2", "grp_x", "ag_x", "rnd_x", "")
	sink.beginSegment("run_2", "req_3")
	sink.feed("run_2", "req_3", "新 run")
	sink.closeSegment("run_2", "新 run")
	segs := sink.finish()
	if len(segs) != 1 || segs[0].text != "新 run" {
		t.Fatalf("新 run 的段 = %+v", segs)
	}
	evs := deltaEvents(t, buf)
	if len(evs) != 2 || evs[1]["delta"] != "新 run" || int(evs[1]["index"].(float64)) != 0 {
		t.Fatalf("新 run 的 delta = %+v", evs)
	}
}

// 单段（一次 LLM 调用）时：delta 拼接 == final 正文、delta 与 final 同一个 message_id；
// 多段时每段各自成立（见 TestHubGroupStreamMultiSegmentBubbles）。
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

// ---------- 场景2b：多轮 LLM 的分段流式与多气泡落账 ----------

// segmentRound 一轮模型响应：流式正文 + LLMEnd 的权威正文 + 可选 tool call / 错误。
type segmentRound struct {
	chunks  []string
	content string
	tool    *core.ToolCall
	err     error
}

// segmentProvider 按调用次序回放 segmentRound（每次调用 = 一次 LLM 轮次）。
//
// 与真实 provider 同形：先发 LLMStart（RequestId 是分段键），再 content delta，
// 最后 LLMEnd；round.err 模拟流中断（重试会带新的 RequestId 再来一次）。
type segmentProvider struct {
	mu     sync.Mutex
	rounds []segmentRound
	calls  int
}

func newSegmentProvider(rounds ...segmentRound) *segmentProvider {
	return &segmentProvider{rounds: rounds}
}

func (p *segmentProvider) Stream(_ context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	p.mu.Lock()
	idx := p.calls
	p.calls++
	round := segmentRound{}
	if len(p.rounds) > 0 {
		if idx < len(p.rounds) {
			round = p.rounds[idx]
		} else {
			round = p.rounds[len(p.rounds)-1]
		}
	}
	p.mu.Unlock()
	model := ""
	if req != nil {
		model = req.Model
	}
	rid := fmt.Sprintf("req_seg_%d", idx)
	if err := h(provider.LLMStartEvent{RequestId: rid, Model: model, Timestamp: time.Now()}); err != nil {
		return err
	}
	for i, c := range round.chunks {
		if err := h(provider.LLMContentDeltaEvent{RequestId: rid, Delta: c, Index: int64(i)}); err != nil {
			return err
		}
	}
	if round.err != nil {
		return round.err
	}
	ev := provider.LLMEndEvent{
		RequestId: rid, FinishReason: core.FinishReasonStop, Content: round.content,
		Model: model, Usage: core.Usage{Input: 20, Output: 10},
	}
	if round.tool != nil {
		ev.ToolCalls = []core.ToolCall{*round.tool}
		ev.FinishReason = core.FinishReasonToolCall
	}
	return h(ev)
}

func (p *segmentProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// deltaGroup 同一个 message_id 的全部 delta（顺序 = 到达顺序）。
type deltaGroup struct {
	messageID string
	text      string
	indexes   []int
	events    []map[string]any
}

// groupDeltas 按 message_id 聚合 hub_message_delta（首次出现顺序 = 段顺序）。
func groupDeltas(t *testing.T, buf *syncBuf) []*deltaGroup {
	t.Helper()
	var out []*deltaGroup
	byID := map[string]*deltaGroup{}
	for _, ev := range hubEvents(t, buf, "hub_message_delta") {
		id, _ := ev["message_id"].(string)
		if id == "" {
			t.Fatalf("hub_message_delta 缺 message_id: %v", ev)
		}
		grp := byID[id]
		if grp == nil {
			grp = &deltaGroup{messageID: id}
			byID[id] = grp
			out = append(out, grp)
		}
		grp.text += str(ev, "delta")
		grp.indexes = append(grp.indexes, int(ev["index"].(float64)))
		grp.events = append(grp.events, ev)
	}
	return out
}

// agentTextMessages 某智能体在群日志里的 text 消息（按 seq 升序 = ReadMessages 顺序）。
func agentTextMessages(msgs []agenthub.Message, agentID agenthub.AgentID) []agenthub.Message {
	out := make([]agenthub.Message, 0, len(msgs))
	for _, msg := range msgs {
		if msg.Sender.Kind == agenthub.SenderAgent && msg.Sender.AgentID == agentID && msg.Kind == agenthub.KindText {
			out = append(out, msg)
		}
	}
	return out
}

// setGroupStreamLoop 与 setGroupFakeLoopIters 同形，额外接受 loop 选项：
// 重试用例要把退避压到毫秒级（setGroupFakeLoopIters 建的 loop 默认不重试）。
func setGroupStreamLoop(t *testing.T, m *manager, bs *bridgeSession, g *agenthub.Group, agentID string, fake provider.Provider, maxIter int, extra ...agents.Option) {
	t.Helper()
	if maxIter <= 0 {
		maxIter = 1
	}
	loaded, _, err := m.hubLoadAgent(agentID)
	if err != nil {
		t.Fatal(err)
	}
	members, err := m.hubGroupMemberProfiles(context.Background(), mustHubStore(t, m), g)
	if err != nil {
		t.Fatal(err)
	}
	ws := m.hubRuntime(agentID)
	spec := m.hubSpecFor(loaded, agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID}, ws.path)
	spec.Group = g
	spec.Members = members
	opts := m.hubLoopOptions(ws, bs.id, spec, bs.hubEngine)
	opts = append(opts,
		agents.WithProvider(fake),
		agents.WithModel("group-stream-fake"),
		agents.WithMaxIterations(maxIter),
	)
	opts = append(opts, extra...)
	bs.s.SetLoop(agents.NewAgentLoop(opts...))
	bs.loop = bs.s.Loop()
}

// setDMStreamLoop 同 setDMFakeLoop，但留 maxIter 轮（工具轮 + 收尾正文需要 ≥2）。
func setDMStreamLoop(t *testing.T, m *manager, bs *bridgeSession, agentID string, fake provider.Provider, maxIter int) {
	t.Helper()
	if maxIter <= 0 {
		maxIter = 1
	}
	loaded, _, err := m.hubLoadAgent(agentID)
	if err != nil {
		t.Fatal(err)
	}
	ws := m.hubRuntime(agentID)
	spec := m.hubSpecFor(loaded, agenthub.Scope{Kind: agenthub.ScopeDM}, ws.path)
	opts := m.hubLoopOptions(ws, bs.id, spec, bs.hubEngine)
	opts = append(opts,
		agents.WithProvider(fake),
		agents.WithModel("dm-stream-fake"),
		agents.WithMaxIterations(maxIter),
	)
	bs.s.SetLoop(agents.NewAgentLoop(opts...))
	bs.loop = bs.s.Loop()
}

// 场景2b 主用例：一轮「正文 + 工具调用」再一轮「收尾正文」→ 两个不同 message_id 的
// 草稿、群里两条 text 消息（文本 = 两轮 LLMEnd.Content）、按 seq 升序、只有最后一段
// 参与 @ 路由。
func TestHubGroupStreamMultiSegmentBubbles(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "MultiA")
	b := createHubAgent(t, m, buf, 2, "MultiB")
	g := groupWithMembers(t, m, buf, 3, "多段群", memberRef{a.ID, "owner"}, memberRef{b.ID, "member"})
	bs, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	// 默认 80ms 定时器会把 pending 提前发出去，段的先后就测不准了（同 P3a-09 的理由）。
	bs.hubDelta.interval = time.Hour
	setGroupStreamLoop(t, m, bs, g, string(b.ID), newSegmentProvider(
		segmentRound{
			chunks:  []string{"@MultiA ", "第一轮", "正文"},
			content: "@MultiA 第一轮正文",
			tool:    &core.ToolCall{Id: "call_todo_seg", Index: 0, Name: "todo_add", Arguments: `{"title":"分段测试"}`},
		},
		segmentRound{
			chunks:  []string{"第二轮", "正文"},
			content: "第二轮正文",
		},
	), 2)
	if ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@MultiB 说两段",
	}); !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		return len(agentTextMessages(msgs, b.ID)) >= 2
	})
	// 段是 AgentEnd 批量落账的：等 run 真正收尾后再数，才能排除「第三条稍后才来」。
	p404aWaitGroupIdle(t, bs)
	msgs, err := mustHubStore(t, m).ReadMessages(context.Background(), g.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	bMsgs := agentTextMessages(msgs, b.ID)
	if len(bMsgs) != 2 {
		t.Fatalf("B 的 text 消息 = %d，want 2（两轮各一条，不能有第三条）: %+v", len(bMsgs), bMsgs)
	}
	if bMsgs[0].Text != "@MultiA 第一轮正文" || bMsgs[1].Text != "第二轮正文" {
		t.Fatalf("文本 = %q / %q，want 两轮 LLMEnd.Content 按 seq 升序", bMsgs[0].Text, bMsgs[1].Text)
	}
	for i, msg := range bMsgs {
		if !strings.HasPrefix(msg.ID, "msg_") {
			t.Fatalf("第 %d 段 message_id = %q，必须 msg_ 前缀", i, msg.ID)
		}
		if msg.ID == agenthub.MessageID(msg.Seq) {
			t.Fatalf("第 %d 段 message_id = %q 是 seq 默认 ID，流式段必须用预分配 ID", i, msg.ID)
		}
	}
	if bMsgs[0].ID == bMsgs[1].ID {
		t.Fatalf("两段 message_id 相同（%q），前端会把两条气泡并成一条", bMsgs[0].ID)
	}

	// 每段一条草稿：message_id 各不同、index 各从 0 起、文本 == 该段 final 正文。
	groups := groupDeltas(t, buf)
	if len(groups) != 2 {
		t.Fatalf("草稿段数 = %d，want 2: %+v", len(groups), groups)
	}
	for i, grp := range groups {
		if grp.messageID != bMsgs[i].ID {
			t.Fatalf("第 %d 段草稿 message_id = %q，final = %q", i, grp.messageID, bMsgs[i].ID)
		}
		if grp.text != bMsgs[i].Text {
			t.Fatalf("第 %d 段草稿文本 = %q，final = %q", i, grp.text, bMsgs[i].Text)
		}
		for j, idx := range grp.indexes {
			if idx != j {
				t.Fatalf("第 %d 段 index[%d] = %d，want 每段各自从 0 递增: %v", i, j, idx, grp.indexes)
			}
		}
	}

	// final 必须在对应 delta 之后（前端按 message_id 用 final 替换草稿）。
	for i, msg := range bMsgs {
		id := msg.ID
		deltaAt, finalAt := waitEventOffsets(t, buf,
			func(ev map[string]any) bool {
				return ev["event_type"] == "hub_message_delta" && ev["message_id"] == id
			},
			func(ev map[string]any) bool {
				if ev["event_type"] != "hub_message" {
					return false
				}
				body, _ := ev["message"].(map[string]any)
				return body != nil && body["id"] == id
			},
		)
		if deltaAt > finalAt {
			t.Fatalf("第 %d 段顺序反了：final(%d) 早于 delta(%d)", i, finalAt, deltaAt)
		}
	}

	// 只有最后一段参与路由：第一段正文里的 @MultiA 不能落 mentions、不能产生 round。
	if len(bMsgs[0].Mentions) != 0 || bMsgs[0].MentionAll {
		t.Fatalf("中间段被路由了：mentions=%v all=%v", bMsgs[0].Mentions, bMsgs[0].MentionAll)
	}
	for _, ev := range hubEvents(t, buf, "hub_round") {
		round, _ := ev["round"].(map[string]any)
		if round == nil {
			continue
		}
		if int64(round["trigger_seq"].(float64)) == bMsgs[0].Seq {
			t.Fatalf("中间段 %d 产生了路由 round（SkipRoute 失效）: %v", bMsgs[0].Seq, round)
		}
	}
	if len(hubEvents(t, buf, "hub_message")) < 2 {
		t.Fatal("缺少 hub_message 回执")
	}
}

// 场景2b：轮1 有正文、轮2 空（纯工具收尾）→ 只有一条气泡，不能给空轮开空气泡。
func TestHubGroupStreamSkipsEmptyToolRound(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "EmptyA")
	b := createHubAgent(t, m, buf, 2, "EmptyB")
	g := groupWithMembers(t, m, buf, 3, "空轮群", memberRef{a.ID, "owner"}, memberRef{b.ID, "member"})
	bs, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	bs.hubDelta.interval = time.Hour
	setGroupStreamLoop(t, m, bs, g, string(b.ID), newSegmentProvider(
		segmentRound{
			chunks:  []string{"有正文的", "一轮"},
			content: "有正文的一轮",
			tool:    &core.ToolCall{Id: "call_todo_empty", Index: 0, Name: "todo_add", Arguments: `{"title":"空轮"}`},
		},
		segmentRound{content: ""},
	), 2)
	if ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@EmptyB 说一句",
	}); !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	msgs := waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		return len(agentTextMessages(msgs, b.ID)) >= 1
	})
	p404aWaitGroupIdle(t, bs)
	msgs, err = mustHubStore(t, m).ReadMessages(context.Background(), g.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	bMsgs := agentTextMessages(msgs, b.ID)
	if len(bMsgs) != 1 {
		t.Fatalf("B 的 text 消息 = %d，want 恰 1 条（空轮不落空气泡）: %+v", len(bMsgs), bMsgs)
	}
	if bMsgs[0].Text != "有正文的一轮" {
		t.Fatalf("正文 = %q", bMsgs[0].Text)
	}
	groups := groupDeltas(t, buf)
	if len(groups) != 1 || groups[0].messageID != bMsgs[0].ID {
		t.Fatalf("草稿段 = %+v，want 只有一条且与 final 同 ID %q", groups, bMsgs[0].ID)
	}
}

// 场景2b：group_say 已把完整正文发过，最后一轮又说一遍 → 不重复落账。
// group_say 仍是完整消息、不流式（它落账时 AgentEnd 还没到）。
func TestHubGroupStreamGroupSayDedup(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "SayA")
	b := createHubAgent(t, m, buf, 2, "SayB")
	g := groupWithMembers(t, m, buf, 3, "去重群", memberRef{a.ID, "owner"}, memberRef{b.ID, "member"})
	bs, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	bs.hubDelta.interval = time.Hour
	const report = "这是完整的汇报正文，模型会先 group_say 发一次。"
	setGroupStreamLoop(t, m, bs, g, string(b.ID), newSegmentProvider(
		segmentRound{tool: &core.ToolCall{
			Id: "call_say_seg", Index: 0, Name: "group_say",
			Arguments: fmt.Sprintf(`{"group_id":%q,"text":%q}`, string(g.ID), report),
		}},
		segmentRound{chunks: []string{"这是完整的汇报正文，", "模型会先 group_say 发一次。"}, content: report},
	), 2)
	if ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@SayB 汇报一下",
	}); !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		return len(agentTextMessages(msgs, b.ID)) >= 1
	})
	p404aWaitGroupIdle(t, bs)
	msgs, err := mustHubStore(t, m).ReadMessages(context.Background(), g.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	bMsgs := agentTextMessages(msgs, b.ID)
	if len(bMsgs) != 1 {
		t.Fatalf("B 的 text 消息 = %d，want 1（group_say + 最后一段同文）: %+v", len(bMsgs), bMsgs)
	}
	if bMsgs[0].Text != report {
		t.Fatalf("正文 = %q，want group_say 的完整正文", bMsgs[0].Text)
	}
	if bMsgs[0].RoundID != "" {
		t.Fatalf("group_say 落账的 round_id = %q，want 空（不参与 final 的轮次记账）", bMsgs[0].RoundID)
	}
}

// 场景2b（多段回归）：**两段正文** + group_say 同文 → 最后一段仍要去重。
//
// 去重基线一旦在落账循环里被写回成「上一段」，2 段以上时最后一段永远在和铺垫段
// 比较，group_say 已发过的正文被 final 又写一遍（群日志里 report 出现两次）。
// 铺垫段照常落账（它确实没有发过），但不路由、不带 mentions。
func TestHubGroupStreamGroupSayDedupMultiSegment(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "MultiSayA")
	b := createHubAgent(t, m, buf, 2, "MultiSayB")
	g := groupWithMembers(t, m, buf, 3, "多段去重群", memberRef{a.ID, "owner"}, memberRef{b.ID, "member"})
	bs, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	bs.hubDelta.interval = time.Hour
	const preamble = "铺垫段：我先看一眼群日志，然后汇报。"
	const report = "这是完整的汇报正文，模型会先 group_say 发一次。"
	setGroupStreamLoop(t, m, bs, g, string(b.ID), newSegmentProvider(
		segmentRound{
			chunks:  []string{"铺垫段：我先看一眼群日志，", "然后汇报。"},
			content: preamble,
			tool: &core.ToolCall{
				Id: "call_say_multi", Index: 0, Name: "group_say",
				Arguments: fmt.Sprintf(`{"group_id":%q,"text":%q}`, string(g.ID), report),
			},
		},
		segmentRound{chunks: []string{"这是完整的汇报正文，", "模型会先 group_say 发一次。"}, content: report},
	), 2)
	if ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@MultiSayB 汇报一下",
	}); !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	// 等两段都落账（group_say 在 run 中写，铺垫段在 AgentEnd 收尾时写）。
	waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		return len(agentTextMessages(msgs, b.ID)) >= 2
	})
	p404aWaitGroupIdle(t, bs)
	msgs, err := mustHubStore(t, m).ReadMessages(context.Background(), g.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	bMsgs := agentTextMessages(msgs, b.ID)
	if len(bMsgs) != 2 {
		t.Fatalf("B 的 text 消息 = %d，want 2（铺垫段 + group_say；最后一段同文不再写第三条）: %+v", len(bMsgs), bMsgs)
	}
	// 顺序 = seq：group_say 在 run 中落账（round_id 空），铺垫段在 AgentEnd 收尾时落账。
	if bMsgs[0].Text != report || bMsgs[0].RoundID != "" {
		t.Fatalf("第一条 = %+v，want group_say 的完整正文且 round_id 空", bMsgs[0])
	}
	if bMsgs[1].Text != preamble {
		t.Fatalf("第二条 = %q，want 铺垫段（它确实没发过，必须照常落账）", bMsgs[1].Text)
	}
	// report 在整条群日志里只出现 1 次（group_say 那条）；final 的最后一段被去重。
	n := 0
	for _, msg := range msgs {
		if strings.TrimSpace(msg.Text) == report {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("report 在群日志里出现 %d 次，want 1（group_say 已发过，final 不能再写）", n)
	}
	// 铺垫段是中间段：SkipRoute —— 不解析 @、不填 mentions、不产生 round。
	if len(bMsgs[1].Mentions) != 0 || bMsgs[1].MentionAll {
		t.Fatalf("铺垫段带了路由结果（mentions=%v all=%v），want 空（中间段不路由）", bMsgs[1].Mentions, bMsgs[1].MentionAll)
	}
	// 只有人类那条 @MultiSayB 产生 hub_round：回复两段都不该唤醒任何人。
	if rounds := gwGroupRounds(buf, g.ID); rounds != 1 {
		t.Fatalf("hub_round = %d 条，want 1（只有人的 @；agent 回复的段不得路由）", rounds)
	}
}

// 场景2b：LLMError{WillRetry} 后自动重试 → 该轮只 1 条气泡，文本 = 重试成功的 Content。
func TestHubGroupStreamRetryKeepsOneBubble(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "RetryA")
	b := createHubAgent(t, m, buf, 2, "RetryB")
	g := groupWithMembers(t, m, buf, 3, "重试群", memberRef{a.ID, "owner"}, memberRef{b.ID, "member"})
	bs, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	bs.hubDelta.interval = time.Hour
	setGroupStreamLoop(t, m, bs, g, string(b.ID), newSegmentProvider(
		// 失败那次先流出半截正文再断（重试必须丢掉这半截）。
		segmentRound{chunks: []string{"失败的半截正文"}, err: errors.New("upstream boom")},
		segmentRound{chunks: []string{"重试成功"}, content: "重试成功的正文"},
	), 1, agents.WithMaxRetries(2), agents.WithRetryBackoff(agents.FixedBackoff(time.Millisecond)))
	if ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@RetryB 说一句",
	}); !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		return len(agentTextMessages(msgs, b.ID)) >= 1
	})
	p404aWaitGroupIdle(t, bs)
	msgs, err := mustHubStore(t, m).ReadMessages(context.Background(), g.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	bMsgs := agentTextMessages(msgs, b.ID)
	if len(bMsgs) != 1 {
		t.Fatalf("B 的 text 消息 = %d，want 1（失败段丢弃 + 成功段一条）: %+v", len(bMsgs), bMsgs)
	}
	if bMsgs[0].Text != "重试成功的正文" {
		t.Fatalf("正文 = %q，want 重试成功 Content", bMsgs[0].Text)
	}
	groups := groupDeltas(t, buf)
	if len(groups) != 1 || groups[0].messageID != bMsgs[0].ID || groups[0].text != "重试成功" {
		t.Fatalf("草稿段 = %+v，失败那次的半截正文不能出现在流里", groups)
	}
	if n := len(hubEvents(t, buf, "llm_error")); n != 1 {
		t.Fatalf("llm_error = %d，want 1（重试用例没走到重试路径）", n)
	}
}

// 场景2b DM：两轮正文 → 两条 DM 消息 + 两条 hub_message；unread/last_text 按最后一段。
func TestHubDMStreamMultiSegment(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "DM多段")
	agentID := string(p.ID)
	bs, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	bs.hubDelta.interval = time.Hour
	setDMStreamLoop(t, m, bs, agentID, newSegmentProvider(
		segmentRound{
			chunks:  []string{"DM 第一", "段"},
			content: "DM 第一段",
			tool:    &core.ToolCall{Id: "call_todo_dm", Index: 0, Name: "todo_add", Arguments: `{"title":"dm 分段"}`},
		},
		segmentRound{chunks: []string{"DM 第二", "段"}, content: "DM 第二段"},
	), 2)
	if ok, errMsg, _ := hubCmd(t, m, buf, 2, "hub_send", map[string]any{
		"target": map[string]any{"kind": "dm", "agent_id": agentID},
		"text":   "说两段",
	}); !ok {
		t.Fatalf("hub_send: %s", errMsg)
	}
	msgs := waitHubIndex(t, m, p.ID, 2) // 人 + 第一段
	// 两段分两次 Append：等两条 agent 消息都在。
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msgs, err = st.ReadDirectMessages(context.Background(), p.ID, 0, 500)
		if err == nil && len(agentDMTexts(msgs)) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	agentMsgs := agentDMTexts(msgs)
	if len(agentMsgs) != 2 {
		t.Fatalf("DM 里的 agent 消息 = %d，want 2: %+v", len(agentMsgs), agentMsgs)
	}
	if agentMsgs[0].Text != "DM 第一段" || agentMsgs[1].Text != "DM 第二段" {
		t.Fatalf("DM 文本 = %q / %q，want 按轮次顺序", agentMsgs[0].Text, agentMsgs[1].Text)
	}
	if agentMsgs[0].ID == agentMsgs[1].ID || !strings.HasPrefix(agentMsgs[0].ID, "msg_") || !strings.HasPrefix(agentMsgs[1].ID, "msg_") {
		t.Fatalf("两段 message_id = %q / %q，必须各自 msg_ 前缀且不同", agentMsgs[0].ID, agentMsgs[1].ID)
	}
	// hub_message 事件：一条人发 + 两条 agent（前端按 id 回显）。
	var dmAgentEvents int
	for _, ev := range hubEvents(t, buf, "hub_message") {
		body, _ := ev["message"].(map[string]any)
		if body == nil {
			continue
		}
		sender, _ := body["sender"].(map[string]any)
		if sender != nil && sender["kind"] == string(agenthub.SenderAgent) {
			dmAgentEvents++
		}
	}
	if dmAgentEvents != 2 {
		t.Fatalf("agent hub_message 事件 = %d，want 2", dmAgentEvents)
	}
	// DM 也发流式 delta（group_id=""），供前端后续接入；每段各自一条、index 从 0 起。
	groups := groupDeltas(t, buf)
	if len(groups) != 2 {
		t.Fatalf("DM 草稿段 = %d，want 2: %+v", len(groups), groups)
	}
	for i, grp := range groups {
		if grp.messageID != agentMsgs[i].ID {
			t.Fatalf("DM 第 %d 段草稿 message_id = %q，final = %q", i, grp.messageID, agentMsgs[i].ID)
		}
		if grp.text != agentMsgs[i].Text {
			t.Fatalf("DM 第 %d 段草稿文本 = %q，final = %q", i, grp.text, agentMsgs[i].Text)
		}
		for _, ev := range grp.events {
			if gid, _ := ev["group_id"].(string); gid != "" {
				t.Fatalf("DM delta 的 group_id = %q，want 空串: %v", gid, ev)
			}
			if ev["agent_id"] != agentID {
				t.Fatalf("DM delta 的 agent_id = %v，want %s", ev["agent_id"], agentID)
			}
		}
	}
	// 左栏预览/未读：人发的消息已读（hub_send 推进游标），两段 agent 回复都未读。
	ok, errMsg, data := hubCmd(t, m, buf, 3, "hub_agent_list", map[string]any{})
	if !ok {
		t.Fatalf("hub_agent_list: %s", errMsg)
	}
	dm, _ := data["dm"].(map[string]any)
	item, _ := dm[agentID].(map[string]any)
	if item == nil {
		t.Fatalf("agent_list 缺 DM 预览: %+v", data["dm"])
	}
	if item["unread"] != float64(2) {
		t.Fatalf("unread = %v，want 2（两段回复）", item["unread"])
	}
	if item["last_text"] != "DM 第二段" {
		t.Fatalf("last_text = %v，want 最后一段正文", item["last_text"])
	}
}

// agentDMTexts DM 索引里智能体发的 text 消息（旧→新）。
func agentDMTexts(msgs []agenthub.Message) []agenthub.Message {
	out := make([]agenthub.Message, 0, len(msgs))
	for _, msg := range msgs {
		if msg.Sender.Kind == agenthub.SenderAgent && msg.Kind == agenthub.KindText {
			out = append(out, msg)
		}
	}
	return out
}
