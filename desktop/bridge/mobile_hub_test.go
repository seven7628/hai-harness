package main

import (
	"fmt"
	"testing"
)

func TestEventTypeOfInjectedLine(t *testing.T) {
	// emit 会在对象首部注入 session_id/workspace，所以 event_type 不是第一个键
	line := []byte(`{"session_id":"s1","workspace":"/w","event_type":"content_chunk","content":"hi"}`)
	if got := eventTypeOf(line); got != "content_chunk" {
		t.Fatalf("eventTypeOf = %q", got)
	}
	if !isChunkEvent(line) {
		t.Fatal("content_chunk 应判为可丢的 chunk")
	}
	if isChunkEvent([]byte(`{"event_type":"agent_end"}`)) {
		t.Fatal("agent_end 不是 chunk")
	}
	if got := eventTypeOf([]byte(`not json`)); got != "" {
		t.Fatalf("非法行应返回空，得到 %q", got)
	}
}

func TestMobileHubFiltersBySession(t *testing.T) {
	h := newMobileHub()
	a := h.subscribe("/w", "s1")
	b := h.subscribe("/w", "s2")
	h.broadcast("/w", "s1", []byte(`{"event_type":"agent_end","session_id":"s1"}`))
	select {
	case <-a.ch:
	default:
		t.Fatal("匹配的订阅者应收到事件")
	}
	select {
	case line := <-b.ch:
		t.Fatalf("不匹配的会话不应收到：%s", line)
	default:
	}
}

func TestMobileHubNeverDropsControlEvents(t *testing.T) {
	h := newMobileHub()
	s := h.subscribe("/w", "s1")
	// 灌满缓冲：全是可丢的 chunk
	for i := 0; i < mobileSubBuf; i++ {
		h.broadcast("/w", "s1", []byte(fmt.Sprintf(`{"event_type":"content_chunk","content":"%d"}`, i)))
	}
	if s.dropped.Load() {
		t.Fatal("缓冲刚好满时不应标记丢弃")
	}
	// 再来一个 chunk → 丢 + 标记
	h.broadcast("/w", "s1", []byte(`{"event_type":"content_chunk","content":"x"}`))
	if !s.dropped.Load() {
		t.Fatal("溢出时应标记 dropped（写循环据此补 resync）")
	}
	// 控制事件必须挤进去
	h.broadcast("/w", "s1", []byte(`{"event_type":"agent_end"}`))
	var last []byte
	for {
		select {
		case line := <-s.ch:
			last = line
			continue
		default:
		}
		break
	}
	if eventTypeOf(last) != "agent_end" {
		t.Fatalf("控制事件被挤掉了，最后一条是 %s", last)
	}
}

func TestMobileHubUnsubscribeClosesChannel(t *testing.T) {
	h := newMobileHub()
	s := h.subscribe("", "")
	h.unsubscribe(s)
	if _, ok := <-s.ch; ok {
		t.Fatal("退订后通道应关闭")
	}
	// 重复退订不 panic
	h.unsubscribe(s)
}

// 空过滤不能等于「订阅全部」：漏传 session_id 的客户端会收下整个工作区的流，
// 手机上表现为「切换会话后仍在显示别的会话的内容」。
func TestMobileHubRejectsEmptyFilter(t *testing.T) {
	h := newMobileHub()
	loose := h.subscribe("", "") // 漏传 workspace / session_id
	exact := h.subscribe("/w", "s1")

	h.broadcast("/w", "s1", []byte(`{"event_type":"content_chunk","content":"x"}`))

	if len(loose.ch) != 0 {
		t.Fatalf("空过滤订阅者不该收到会话事件，实际收到 %d 条", len(loose.ch))
	}
	if len(exact.ch) != 1 {
		t.Fatalf("匹配的订阅者应收到 1 条，实际 %d 条", len(exact.ch))
	}
}
