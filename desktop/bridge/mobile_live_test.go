package main

import "testing"

// 丢帧后补发快照：积压里的 chunk 必须丢弃（快照已含），控制事件必须保留。
func TestDrainControlKeepsOnlyControlEvents(t *testing.T) {
	h := newMobileHub()
	sub := h.subscribe("", "")
	defer h.unsubscribe(sub)

	sub.ch <- []byte(`{"event_type":"content_chunk","content":"And "}`)
	sub.ch <- []byte(`{"event_type":"tool_approval_requested","id":"a1"}`)
	sub.ch <- []byte(`{"event_type":"reasoning_chunk","content":"想"}`)
	sub.ch <- []byte(`{"event_type":"agent_end"}`)

	kept, closed := drainControl(sub)
	if closed {
		t.Fatal("通道不该被判定为关闭")
	}
	if len(kept) != 2 {
		t.Fatalf("应保留 2 条控制事件，实际 %d 条：%s", len(kept), kept)
	}
	if string(kept[0]) != `{"event_type":"tool_approval_requested","id":"a1"}` {
		t.Fatalf("控制事件顺序/内容被破坏：%s", kept[0])
	}
	if len(sub.ch) != 0 {
		t.Fatalf("积压应已排空，剩余 %d 条", len(sub.ch))
	}
}

// 流式快照语义：chunk 累加、llm_end 覆盖、reset 清空（手机端 live 屏的自愈来源）。
func TestBridgeSessionLiveSnapshot(t *testing.T) {
	bs := &bridgeSession{}
	bs.appendLiveText("And ")
	bs.appendLiveText("add K.8")
	bs.appendLiveThink("先看 spec")
	if text, think := bs.liveSnapshot(); text != "And add K.8" || think != "先看 spec" {
		t.Fatalf("累加结果不对：%q / %q", text, think)
	}

	bs.setLiveText("And add K.8 with the tool landing record.")
	if text, _ := bs.liveSnapshot(); text != "And add K.8 with the tool landing record." {
		t.Fatalf("llm_end 应覆盖累计增量，实际 %q", text)
	}

	bs.resetLive()
	if text, think := bs.liveSnapshot(); text != "" || think != "" {
		t.Fatalf("reset 后应为空，实际 %q / %q", text, think)
	}
}
