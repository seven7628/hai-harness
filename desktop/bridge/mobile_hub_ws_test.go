// mobile_hub_ws_test.go —— Hub 协议的事件扇出回归（P4-03a）。
//
// 覆盖：WS 鉴权、hub 作用域只收 hub_*、会话作用域不收 hub_*、两类订阅并存不重复、
// 以及 out.writeLine 单点接线（Hub 事件的生产侧不经 bs.emit）。
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func mobileTestPair(t *testing.T, s *mobileServer) string {
	t.Helper()
	code, _ := s.pair.newCode(time.Now())
	token, _, err := s.pair.pair(code, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func mobileWSURL(ts *httptest.Server, query string) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/events?" + query
}

func dialMobileWS(t *testing.T, rawURL string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, rawURL, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", rawURL, err)
	}
	t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "") })
	return c
}

func readMobileWSFrame(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("读 WS 帧失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("WS 帧不是 JSON: %v: %s", err, b)
	}
	return m
}

func mobileTestLine(t *testing.T, v map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Hub 事件流和普通会话流一样必须配对；未带 token 直接 401（不升级 WS）。
func TestMobileHubWSEventStreamRequiresToken(t *testing.T) {
	_, ts := testMobileServer(t)
	for _, query := range []string{"", "?scope=hub"} {
		r, err := http.Get(ts.URL + "/api/events" + query)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		if r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("/api/events%s 无 token 应为 401，得到 %d", query, r.StatusCode)
		}
	}
}

func TestMobileHubWSRejectsUnknownScope(t *testing.T) {
	s, ts := testMobileServer(t)
	token := mobileTestPair(t, s)
	r, err := http.Get(ts.URL + "/api/events?scope=wat&token=" + url.QueryEscape(token))
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知 scope 应为 400，得到 %d", r.StatusCode)
	}
}

func TestMobileHubWSHubScopeReceivesOnlyHubEvents(t *testing.T) {
	s, ts := testMobileServer(t)
	token := mobileTestPair(t, s)
	c := dialMobileWS(t, mobileWSURL(ts, "scope=hub&token="+url.QueryEscape(token)))

	hello := readMobileWSFrame(t, c)
	if hello["type"] != "hello" || hello["scope"] != "hub" {
		t.Fatalf("hub hello 不符：%+v", hello)
	}

	// 先投会话事件再投 hub 事件：若串台，第一帧会是 content_chunk 而不是 hub_message。
	s.hub.broadcast("/w", "s1", mobileTestLine(t, map[string]any{
		"event_type": "content_chunk", "workspace": "/w", "session_id": "s1", "content": "chat",
	}))
	s.hub.broadcastHub(mobileTestLine(t, map[string]any{"event_type": "hub_message", "message_id": "m1"}))
	s.hub.broadcastHub(mobileTestLine(t, map[string]any{"event_type": "hub_task_update", "task_id": "t1"}))
	s.hub.broadcastHub(mobileTestLine(t, map[string]any{"event_type": "hub_message_delta", "message_id": "m1", "delta": "x"}))

	for _, want := range []string{"hub_message", "hub_task_update", "hub_message_delta"} {
		got := readMobileWSFrame(t, c)
		if got["event_type"] != want {
			t.Fatalf("hub 订阅者应收到 %s，得到 %+v", want, got)
		}
	}
}

func TestMobileHubWSChatScopeDoesNotReceiveHubEvents(t *testing.T) {
	s, ts := testMobileServer(t)
	token := mobileTestPair(t, s)
	c := dialMobileWS(t, mobileWSURL(ts,
		"workspace=%2Fw&session_id=s1&token="+url.QueryEscape(token)))
	if got := readMobileWSFrame(t, c); got["type"] != "hello" {
		t.Fatalf("chat hello 不符：%+v", got)
	}

	// Hub 事件不能漏进会话流；夹在两条会话事件之间，若混入第一帧就不再是 sentinel-1。
	s.hub.broadcastHub(mobileTestLine(t, map[string]any{"event_type": "hub_message", "message_id": "m1"}))
	s.hub.broadcast("/w", "s1", mobileTestLine(t, map[string]any{
		"event_type": "content_chunk", "content": "sentinel-1",
	}))
	got := readMobileWSFrame(t, c)
	if got["event_type"] != "content_chunk" || got["content"] != "sentinel-1" {
		t.Fatalf("会话订阅者收到了非本会话事件：%+v", got)
	}

	// 其他会话的事件同样不能串进来。
	s.hub.broadcast("/w", "s2", mobileTestLine(t, map[string]any{
		"event_type": "content_chunk", "content": "other-session",
	}))
	s.hub.broadcastHub(mobileTestLine(t, map[string]any{"event_type": "hub_task_update", "task_id": "t1"}))
	s.hub.broadcast("/w", "s1", mobileTestLine(t, map[string]any{
		"event_type": "content_chunk", "content": "sentinel-2",
	}))
	got = readMobileWSFrame(t, c)
	if got["event_type"] != "content_chunk" || got["content"] != "sentinel-2" {
		t.Fatalf("会话订阅者串台或收到 hub 事件：%+v", got)
	}
}

// 两个作用域同时在线：hub 事件只投一次给 hub，会话事件只投一次给 chat。
func TestMobileHubWSBothScopesNoDuplicateDelivery(t *testing.T) {
	s, ts := testMobileServer(t)
	token := mobileTestPair(t, s)
	hubC := dialMobileWS(t, mobileWSURL(ts, "scope=hub&token="+url.QueryEscape(token)))
	chatC := dialMobileWS(t, mobileWSURL(ts,
		"workspace=%2Fw&session_id=s1&token="+url.QueryEscape(token)))
	if got := readMobileWSFrame(t, hubC); got["scope"] != "hub" {
		t.Fatalf("hub hello 不符：%+v", got)
	}
	if got := readMobileWSFrame(t, chatC); got["type"] != "hello" {
		t.Fatalf("chat hello 不符：%+v", got)
	}

	s.hub.broadcastHub(mobileTestLine(t, map[string]any{"event_type": "hub_message", "n": 1}))
	if got := readMobileWSFrame(t, hubC); got["event_type"] != "hub_message" || got["n"] != float64(1) {
		t.Fatalf("hub 订阅者第一帧不符：%+v", got)
	}
	s.hub.broadcast("/w", "s1", mobileTestLine(t, map[string]any{
		"event_type": "content_chunk", "content": "chat",
	}))
	if got := readMobileWSFrame(t, chatC); got["event_type"] != "content_chunk" {
		t.Fatalf("chat 订阅者第一帧不符（hub 事件泄漏或重复）：%+v", got)
	}

	// 第二条 hub 事件：若第一条被重复投递，这里会先读到 n=1。
	s.hub.broadcastHub(mobileTestLine(t, map[string]any{"event_type": "hub_message", "n": 2}))
	if got := readMobileWSFrame(t, hubC); got["n"] != float64(2) {
		t.Fatalf("hub 订阅者发生重复投递/乱序：%+v", got)
	}
}

// Hub 事件由 m.out.writeLine 写出（不是 bs.emit）；输出总线钩子是唯一接线点，
// 且只转发 hub_*：普通会话行不会进入 hub 订阅，hub 行也不会投给会话订阅。
func TestMobileOutLineHookFansOutHubEventsOnly(t *testing.T) {
	buf := &syncBuf{}
	o := newOut(buf)
	s := &mobileServer{mgr: &manager{out: o}, hub: newMobileHub()}
	s.attachEvents()

	hubSub := s.hub.subscribeHub()
	chatSub := s.hub.subscribe("/w", "s1")
	defer s.hub.unsubscribe(hubSub)
	defer s.hub.unsubscribe(chatSub)

	o.writeLine(map[string]any{"event_type": "hub_message", "message_id": "m1"})
	select {
	case line := <-hubSub.ch:
		if eventTypeOf(line) != "hub_message" {
			t.Fatalf("hub 订阅者事件类型不符：%s", line)
		}
	default:
		t.Fatal("hub 事件未通过 out.writeLine 钩子投递")
	}
	select {
	case line := <-chatSub.ch:
		t.Fatalf("hub 事件泄漏到会话订阅者：%s", line)
	default:
	}

	o.writeLine(map[string]any{
		"event_type": "content_chunk", "workspace": "/w", "session_id": "s1", "content": "x",
	})
	select {
	case line := <-hubSub.ch:
		t.Fatalf("会话事件泄漏到 hub 订阅者：%s", line)
	default:
	}
	if !strings.Contains(buf.String(), `"content":"x"`) {
		t.Fatal("普通事件仍应正常写 stdout")
	}
}

// 慢订阅者：delta 可丢并标记 resync；控制事件必须挤进缓冲、不得静默消失。
func TestMobileHubDeltaOverflowKeepsControlEvent(t *testing.T) {
	h := newMobileHub()
	sub := h.subscribeHub()
	defer h.unsubscribe(sub)

	for i := 0; i < mobileSubBuf; i++ {
		h.broadcastHub([]byte(`{"event_type":"hub_message_delta","delta":"x"}`))
	}
	if sub.dropped.Load() {
		t.Fatal("缓冲刚好满时不应标记 dropped")
	}
	h.broadcastHub([]byte(`{"event_type":"hub_message_delta","delta":"overflow"}`))
	if !sub.dropped.Load() {
		t.Fatal("溢出 delta 应标记 dropped（写循环据此发 resync）")
	}

	h.broadcastHub([]byte(`{"event_type":"hub_task_update","task_id":"t1"}`))
	var last []byte
	for {
		select {
		case line := <-sub.ch:
			last = line
		default:
			goto drained
		}
	}
drained:
	if eventTypeOf(last) != "hub_task_update" {
		t.Fatalf("控制事件被慢订阅者挤掉了，最后一条是 %s", last)
	}
}
