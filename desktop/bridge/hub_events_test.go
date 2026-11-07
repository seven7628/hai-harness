package main

// P3a-09：群内流式 delta 的聚合与 final 同 ID 回归。

import (
	"context"
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
}
