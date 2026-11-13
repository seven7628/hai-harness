package main

// mobile_p4_e2e_test.go —— P4-03d：手机端端到端验收（真实 HTTP/WS + 真实 Hub 状态机）。
//
// 与既有 Mobile 测试的分工：
//   - mobile_server_test.go 用 fake dispatch 验协议（echo 替身，不碰状态机）；
//   - 本文件只替换模型（确定性 fake provider）：dispatch 走 mgr.dispatch、Hub 存储是
//     真实 FileStore（HOME 已隔离）、群会话与任务账本都是真的；命令一律走 /api/cmd
//     的白名单入口，事件一律走 scope=hub 的 WS 流。
//
// 覆盖（方案 §7.4）：
//  1) 配对：真的 POST /api/pair 拿 token（不是直接调 pair 包）；
//  2) hub_group_list / hub_group_history 读到真状态；
//  3) 人 @A（结构化 mentions）→ 真路由 + 真唤醒 → hub_message / hub_message_delta /
//     智能体 final；delta 拼起来必须等于 final 正文（手机流式草稿的自愈依据）；
//  4) hub_group_assign 派给 B → 真任务账本 → hub_task_update(running→completed) →
//     群里恰好一张 task_result 卡；hub_task_list / hub_need_list / hub_mark_read
//     读写同一份存储；
//  5) 断线重连：hub 流不重放历史（只有 hello），手机按 hello 重拉群历史快照后，
//     已收的实时帧与快照按 id/seq 合并不得出现第二条，终态任务卡仍在；
//  6) hub_conversation_stop：停止后不启动队列里的 B、不再写智能体 final
//     （复用 TestEval8HumanStopStopsChain 的语义），且 WS 线上顺序一致。
//
// 同步原语：事件一律用有界超时 await（WS 帧 / 存储轮询），不用 sleep 当同步。
// 「不该出现」用有界观察窗口（absence window）表达，同样是超时而非 sleep。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
	"nhooyr.io/websocket"
)

// p4EventWait 单帧事件的有界等待。真实链路（fake provider + 进程内队列）通常 <100ms，
// 留足 -race/CI 抖动余量；超时即失败（绝不无限期挂住）。
const p4EventWait = 10 * time.Second

// p4AbsenceWindow 「不该出现」的观察窗口：短于任何生产定时器（应用心跳 15s），
// 又足够让已排队的帧到达测试端。
const p4AbsenceWindow = 300 * time.Millisecond

// ---------- 流式 fake provider ----------

// p4DeltaProvider 会先发正文增量再发结束帧：手机端因此能真的收到 hub_message_delta
// （p3bEvalProvider 只发 LLMEndEvent，覆盖不到草稿流）。
type p4DeltaProvider struct {
	mu      sync.Mutex
	deltas  []string
	content string
	calls   int
}

func newP4DeltaProvider(content string, deltas ...string) *p4DeltaProvider {
	return &p4DeltaProvider{deltas: deltas, content: content}
}

func (p *p4DeltaProvider) Stream(ctx context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	p.mu.Lock()
	p.calls++
	rid := fmt.Sprintf("req_p4_%d", p.calls)
	p.mu.Unlock()
	model := ""
	if req != nil {
		model = req.Model
	}
	if err := h(provider.LLMStartEvent{RequestId: rid, Model: model, Timestamp: time.Now()}); err != nil {
		return err
	}
	for _, d := range p.deltas {
		if err := h(provider.LLMContentDeltaEvent{RequestId: rid, Delta: d}); err != nil {
			return err
		}
	}
	return h(provider.LLMEndEvent{
		RequestId:    rid,
		FinishReason: core.FinishReasonStop, Content: p.content, Model: model,
		Usage: core.Usage{Input: 20, Output: 10},
	})
}

func (p *p4DeltaProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// ---------- WS 事件收集器 ----------

// p4WSSocket 是手机 Hub 事件流的读侧：单个 goroutine 持续读帧进缓冲 channel，
// 测试按谓词 await（有界超时）。history 保留读到的全部帧，用来做「线上顺序」断言。
type p4WSSocket struct {
	t       *testing.T
	conn    *websocket.Conn
	frames  chan map[string]any
	done    chan struct{}
	history []map[string]any
	// scanned 是「已被某个断言检查过的 history 下标」。观察窗口（expectNone）
	// 会把帧读进 history，后续 await 先补扫这一段，避免「帧已经到过」却等超时。
	scanned int
}

func p4DialHub(t *testing.T, h *p4Mobile) *p4WSSocket {
	t.Helper()
	raw := "ws" + strings.TrimPrefix(h.base, "http") + "/api/events?scope=hub&token=" + url.QueryEscape(h.token)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 不用 dialMobileWS：它的 cleanup 走 Close 的关闭握手，而手机端 Hub 流不回 close
	// 帧（服务端不 Read），每次收尾白等 5s。这里自己拨号 + CloseNow。
	c, _, err := websocket.Dial(ctx, raw, nil)
	if err != nil {
		t.Fatalf("dial hub ws: %v", err)
	}
	w := &p4WSSocket{t: t, conn: c, frames: make(chan map[string]any, 2048), done: make(chan struct{})}
	h.socks = append(h.socks, w)
	go w.read()
	return w
}

func (w *p4WSSocket) read() {
	defer close(w.done)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, b, err := w.conn.Read(ctx)
		cancel()
		if err != nil {
			return
		}
		var f map[string]any
		if json.Unmarshal(b, &f) != nil {
			continue
		}
		select {
		case w.frames <- f:
		default:
			// 测试端读得太慢：事件流已积压 2048 帧，说明场景本身失控，明确失败。
			w.t.Errorf("hub WS 帧缓冲溢出：%v", p4EventTypes(w.history))
			return
		}
	}
}

// close 直接断链（CloseNow）：Close 会等对端 close 帧，而服务端写循环不读客户端帧。
func (w *p4WSSocket) close() {
	if w == nil || w.conn == nil {
		return
	}
	_ = w.conn.CloseNow()
}

// await 读到满足 pred 的帧（其余帧记入 history 后继续读）。
func (w *p4WSSocket) await(what string, pred func(map[string]any) bool) map[string]any {
	w.t.Helper()
	for w.scanned < len(w.history) {
		f := w.history[w.scanned]
		w.scanned++
		if pred(f) {
			return f
		}
	}
	timer := time.NewTimer(p4EventWait)
	defer timer.Stop()
	for {
		select {
		case f := <-w.frames:
			w.history = append(w.history, f)
			w.scanned = len(w.history)
			if pred(f) {
				return f
			}
		case <-w.done:
			w.t.Fatalf("hub WS 已关闭，等待「%s」失败；已收事件：%v", what, p4EventTypes(w.history))
		case <-timer.C:
			w.t.Fatalf("等待「%s」超时（%s）；已收事件：%v", what, p4EventWait, p4EventTypes(w.history))
		}
	}
}

// expectNone 在观察窗口内断言没有任何帧满足 pred。
func (w *p4WSSocket) expectNone(what string, pred func(map[string]any) bool) {
	w.t.Helper()
	timer := time.NewTimer(p4AbsenceWindow)
	defer timer.Stop()
	for {
		select {
		case f := <-w.frames:
			// 只记进 history，不推进 scanned：这一段没被子句检查过，后续 await 还要扫。
			w.history = append(w.history, f)
			if pred(f) {
				w.t.Fatalf("不该出现「%s」：%+v", what, f)
			}
		case <-w.done:
			w.t.Fatalf("hub WS 在观察「%s」期间意外关闭", what)
		case <-timer.C:
			return
		}
	}
}

func (w *p4WSSocket) framesOf(eventType string, match func(map[string]any) bool) []map[string]any {
	out := make([]map[string]any, 0, 2)
	for _, f := range w.history {
		if f["event_type"] != eventType {
			continue
		}
		if match != nil && !match(f) {
			continue
		}
		out = append(out, f)
	}
	return out
}

func p4EventTypes(frames []map[string]any) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		if t := p4StrVal(f["event_type"]); t != "" {
			out = append(out, t)
		} else {
			out = append(out, "frame:"+p4StrVal(f["type"]))
		}
	}
	return out
}

// ---------- 手机服务（真 dispatch） ----------

type p4Mobile struct {
	m     *manager
	buf   *syncBuf
	s     *mobileServer
	token string
	base  string
	socks []*p4WSSocket
}

// p4StartMobile 起一个**真实监听**的手机服务：mgr.dispatch（不是 echo 替身）、
// 真实 Hub 存储（testManager 已把 HOME 指向 TempDir）、随机端口。
// 用 start() 而不是 httptest：后者 Close 会等 WS 处理循环退出（每个连接 ~5s），
// 端到端用例里白白拖慢整套测试。
func p4StartMobile(t *testing.T) *p4Mobile {
	t.Helper()
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})
	s := newMobileServer(m, t.TempDir())
	if s.dispatch != nil {
		t.Fatal("端到端用例必须走真实 dispatch，不能注入替身")
	}
	if err := s.start("127.0.0.1:0"); err != nil {
		t.Fatalf("启动手机服务: %v", err)
	}
	h := &p4Mobile{m: m, buf: buf, s: s, base: "http://" + s.addr}
	t.Cleanup(func() {
		// 先断客户端，再投几帧让服务端写循环失败退出，最后关监听（避免 goroutine
		// 挂到下一个用例、也避免写已删除的 TempDir）。
		for _, sock := range h.socks {
			sock.close()
		}
		for i := 0; i < len(h.socks)+2; i++ {
			s.hub.broadcastHub([]byte(`{"event_type":"hub_message_delta","delta":"flush"}`))
		}
		s.close()
		m.respTap = nil
	})
	h.token = h.pair(t)
	return h
}

// pair 走真实配对端点：源码里桌面生成配对码，手机 POST /api/pair 换 token。
func (h *p4Mobile) pair(t *testing.T) string {
	t.Helper()
	code, _ := h.s.pair.newCode(time.Now())
	r := postJSON(t, h.base+"/api/pair", map[string]any{"code": code, "name": "iPhone P4 E2E"}, "")
	defer r.Body.Close()
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/pair = %d: %s", r.StatusCode, body)
	}
	var pr struct {
		Token string `json:"token"`
		Addr  string `json:"addr"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		t.Fatalf("配对响应不是 JSON: %v: %s", err, body)
	}
	if pr.Token == "" {
		t.Fatalf("配对未返回 token: %s", body)
	}
	// 回执里的 addr 是手机拼 URL 用的真实监听地址。
	if pr.Addr != h.s.addr {
		t.Fatalf("配对回执 addr = %q, want %q", pr.Addr, h.s.addr)
	}
	return pr.Token
}

// cmd 发一条手机命令，要求 HTTP 200 且 ok=true，返回 data。
// 走 /api/cmd 意味着同时验证了白名单：未开放的命令在这里会 403。
func (h *p4Mobile) cmd(t *testing.T, name string, payload map[string]any) map[string]any {
	t.Helper()
	r := postJSON(t, h.base+"/api/cmd", map[string]any{"cmd": name, "payload": payload}, h.token)
	defer r.Body.Close()
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("%s = %d: %s", name, r.StatusCode, body)
	}
	var resp struct {
		OK    bool           `json:"ok"`
		Error string         `json:"error"`
		Data  map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("%s 响应不是 JSON: %v: %s", name, err, body)
	}
	if !resp.OK {
		t.Fatalf("%s 失败: %s", name, resp.Error)
	}
	return resp.Data
}

// ---------- JSON 小工具（HTTP 帧是 map[string]any） ----------

func p4Map(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	v, _ := m[key].(map[string]any)
	return v
}

func p4StrVal(v any) string {
	s, _ := v.(string)
	return s
}

func p4Num(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

func p4StrList(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		out = append(out, p4StrVal(x))
	}
	return out
}

// p4Message 取 hub_message 帧里的 message 对象。
func p4Message(f map[string]any) map[string]any {
	return p4Map(f, "message")
}

// p4SenderKind / p4SenderAgent 取消息发送者（WS 帧里的 sender 是嵌套对象）。
func p4SenderKind(msg map[string]any) string {
	return p4StrVal(p4Map(msg, "sender")["kind"])
}

func p4SenderAgent(msg map[string]any) string {
	sender := p4Map(msg, "sender")
	if sender == nil {
		return ""
	}
	return p4StrVal(sender["agent_id"])
}

func p4Refs(msg map[string]any) map[string]any {
	return p4Map(msg, "refs")
}

// p4JSONMessages 把 /api/cmd 返回的 messages（[]any of map）转成 map 切片。
func p4JSONMessages(t *testing.T, v any) []map[string]any {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("messages 不是数组: %T(%v)", v, v)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, x := range raw {
		m, ok := x.(map[string]any)
		if !ok {
			t.Fatalf("messages 元素不是对象: %T(%v)", x, x)
		}
		out = append(out, m)
	}
	return out
}

// p4MergeMessages 复刻 hub_model.mergeMessages 的去重语义（id / seq 双键）。
// 返回合并结果与「快照里没有的新消息」条数：后者必须为 0 —— 说明实时帧与
// 重拉快照是同一批消息，重连不会凭空多出第二条。
func p4MergeMessages(snapshot, live []map[string]any) (merged []map[string]any, fresh int) {
	byID := map[string]bool{}
	bySeq := map[int64]bool{}
	add := func(msg map[string]any) bool {
		id := p4StrVal(msg["id"])
		seq := p4Num(msg["seq"])
		if id != "" && byID[id] {
			return false
		}
		if seq > 0 && bySeq[seq] {
			return false
		}
		if id != "" {
			byID[id] = true
		}
		if seq > 0 {
			bySeq[seq] = true
		}
		merged = append(merged, msg)
		return true
	}
	for _, msg := range snapshot {
		add(msg)
	}
	for _, msg := range live {
		if add(msg) {
			fresh++
		}
	}
	return merged, fresh
}

// p4LiveMessages 从 WS 帧里取出某个群的 hub_message 消息体（按线上顺序）。
func p4LiveMessages(frames []map[string]any, gid agenthub.GroupID) []map[string]any {
	out := make([]map[string]any, 0, 4)
	for _, f := range frames {
		if f["event_type"] != "hub_message" {
			continue
		}
		msg := p4Message(f)
		if msg == nil || p4StrVal(msg["group_id"]) != string(gid) {
			continue
		}
		out = append(out, msg)
	}
	return out
}

// p4WaitStore 轮询存储直到 pred 为真（Hub 结算/泵是异步的，事件到达即同步点）。
func p4WaitStore(t *testing.T, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(p4EventWait)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待「%s」超时（%s）", what, p4EventWait)
}

// ---------- 用例 1：真实链路（列表/发消息/派任务/任务板/已读/重连快照） ----------

func TestMobileP4E2EHubRealDispatch(t *testing.T) {
	h := p4StartMobile(t)
	m, buf := h.m, h.buf

	a := createDispatchAgent(t, m, buf, 1, "E2EA", true)
	b := createDispatchAgent(t, m, buf, 2, "E2EB", false)
	g := assignGroup(t, m, buf, 3, "P4 端到端群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member",
	})

	sock := p4DialHub(t, h)
	hello := sock.await("hello", func(f map[string]any) bool { return f["type"] == "hello" })
	if hello["scope"] != "hub" {
		t.Fatalf("hello 不是 hub 作用域：%+v", hello)
	}

	// —— 1) 列表 / 历史：手机读的是真实 Hub 存储 ——
	list := h.cmd(t, "hub_group_list", nil)
	if n := p4Num(list["count"]); n != 1 {
		t.Fatalf("hub_group_list count = %d, want 1（%+v）", n, list)
	}
	groups, _ := list["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("hub_group_list groups = %v", list["groups"])
	}
	first, _ := groups[0].(map[string]any)
	if p4StrVal(first["id"]) != string(g.ID) || p4StrVal(first["name"]) != g.Name {
		t.Fatalf("hub_group_list 首个群 = %+v, want %s/%s", first, g.ID, g.Name)
	}

	hist := h.cmd(t, "hub_group_history", map[string]any{"group_id": string(g.ID), "limit": 50})
	if msgs := p4JSONMessages(t, hist["messages"]); len(msgs) != 0 {
		t.Fatalf("新群历史应为空，得到 %d 条", len(msgs))
	}

	// —— 2) 人 @A（结构化 mentions）→ 真路由 / 唤醒 / 流式草稿 / final ——
	const aReply = "收到，我来对齐口径。"
	fa := newP4DeltaProvider(aReply, "收到，", "我来对齐口径。")
	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))
	setGroupFakeLoopIters(t, m, bsA, g, string(a.ID), fa, 1)

	const humanText = "@E2EA 先对齐一下口径"
	send := h.cmd(t, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": humanText,
		"mentions":      []any{string(a.ID)},
		"client_msg_id": "e2e-human-1",
	})
	sendSeq := p4Num(send["seq"])
	sendID := p4StrVal(send["message_id"])
	if sendSeq == 0 || sendID == "" {
		t.Fatalf("hub_group_send 回执缺 seq/message_id：%+v", send)
	}
	round := p4Map(send, "round")
	if round == nil {
		t.Fatalf("hub_group_send 回执缺 round（有 @ 必须有回执）：%+v", send)
	}
	if targets := p4StrList(round["targets"]); len(targets) != 1 || targets[0] != string(a.ID) {
		t.Fatalf("round.targets = %v, want [%s]", targets, a.ID)
	}

	humanEv := sock.await("hub_message(人)", func(f map[string]any) bool {
		return f["event_type"] == "hub_message" && p4StrVal(p4Message(f)["id"]) == sendID
	})
	if got := p4Num(humanEv["seq"]); got != sendSeq {
		t.Fatalf("人消息事件 seq = %d, want %d", got, sendSeq)
	}
	// client_msg_id 是手机乐观气泡的幂等键：事件里必须原样带回（重试去重靠它）。
	if got := p4StrVal(humanEv["client_msg_id"]); got != "e2e-human-1" {
		t.Fatalf("事件 client_msg_id = %q, want e2e-human-1", got)
	}
	humanMsg := p4Message(humanEv)
	if p4SenderKind(humanMsg) != string(agenthub.SenderHuman) || p4StrVal(humanMsg["text"]) != humanText {
		t.Fatalf("人消息事件 = %+v", humanMsg)
	}
	if mentions := p4StrList(humanMsg["mentions"]); len(mentions) != 1 || mentions[0] != string(a.ID) {
		t.Fatalf("结构化 mentions = %v, want [%s]（正文 @ 之外必须落结构化目标）", mentions, a.ID)
	}
	if got := p4StrVal(p4Refs(humanMsg)["client_msg_id"]); got != "e2e-human-1" {
		t.Fatalf("消息 refs.client_msg_id = %q", got)
	}

	// 手机重试契约的服务端一半：同一个 client_msg_id 再发一次 = 幂等重放
	// （回执 deduplicated，不写第二条、不再发 hub_message）。客户端复用 cid 就靠它收敛。
	retrySend := h.cmd(t, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": humanText,
		"mentions":      []any{string(a.ID)},
		"client_msg_id": "e2e-human-1",
	})
	if retrySend["deduplicated"] != true {
		t.Fatalf("同 client_msg_id 重发应命中幂等（deduplicated=true）：%+v", retrySend)
	}
	if got := p4Num(retrySend["seq"]); got != sendSeq {
		t.Fatalf("幂等重放 seq = %d, want %d（首次回执）", got, sendSeq)
	}
	if got := p4StrVal(retrySend["message_id"]); got != sendID {
		t.Fatalf("幂等重放 message_id = %q, want %q", got, sendID)
	}
	sock.expectNone("重试写出的第二条人消息", func(f map[string]any) bool {
		return f["event_type"] == "hub_message" && p4StrVal(f["client_msg_id"]) == "e2e-human-1" &&
			p4StrVal(p4Message(f)["id"]) != sendID
	})

	roundEv := sock.await("hub_round", func(f map[string]any) bool {
		r := p4Map(f, "round")
		return f["event_type"] == "hub_round" && p4StrVal(r["id"]) == p4StrVal(round["id"])
	})
	if targets := p4StrList(p4Map(roundEv, "round")["targets"]); len(targets) != 1 || targets[0] != string(a.ID) {
		t.Fatalf("hub_round targets = %v", targets)
	}

	finalEv := sock.await("hub_message(A final)", func(f map[string]any) bool {
		if f["event_type"] != "hub_message" {
			return false
		}
		msg := p4Message(f)
		return p4SenderKind(msg) == string(agenthub.SenderAgent) &&
			p4SenderAgent(msg) == string(a.ID) && p4Num(msg["seq"]) > 0
	})
	finalMsg := p4Message(finalEv)
	if got := p4StrVal(finalMsg["text"]); got != aReply {
		t.Fatalf("A 的 final 正文 = %q, want %q", got, aReply)
	}
	if n := fa.callCount(); n != 1 {
		t.Fatalf("A 的 provider 调用 = %d, want 1", n)
	}

	// 流式草稿：delta 拼起来 == final 正文，message_id 与 final 同一条，index 从 0 连续。
	finalID := p4StrVal(finalMsg["id"])
	deltas := sock.framesOf("hub_message_delta", func(f map[string]any) bool {
		return p4StrVal(f["group_id"]) == string(g.ID)
	})
	if len(deltas) == 0 {
		t.Fatal("手机没收到任何 hub_message_delta（流式草稿链路断了）")
	}
	var streamed strings.Builder
	for i, d := range deltas {
		if p4Num(d["index"]) != int64(i) {
			t.Fatalf("delta[%d].index = %v, want %d", i, d["index"], i)
		}
		if p4StrVal(d["message_id"]) != finalID {
			t.Fatalf("delta[%d].message_id = %q, want final %q", i, p4StrVal(d["message_id"]), finalID)
		}
		if p4StrVal(d["agent_id"]) != string(a.ID) {
			t.Fatalf("delta[%d].agent_id = %q", i, p4StrVal(d["agent_id"]))
		}
		streamed.WriteString(p4StrVal(d["delta"]))
	}
	if got := streamed.String(); got != aReply {
		t.Fatalf("delta 拼起来 = %q, want final 正文 %q", got, aReply)
	}

	// —— 3) 人派任务给 B → 账本 / 任务卡 / 任务板 ——
	const bResult = "结论：可以上线。"
	fb := newP3bEvalProvider(p3bEvalRound{content: bResult})
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	setGroupFakeLoopIters(t, m, bsB, g, string(b.ID), fb, 1)

	const instruction = "把结论整理成一页"
	assign := h.cmd(t, "hub_group_assign", map[string]any{
		"group_id": string(g.ID), "agent_id": string(b.ID),
		"instruction": instruction, "title": "结论整理",
		"client_msg_id": "e2e-assign-1",
	})
	taskID := p4StrVal(assign["task_id"])
	if taskID == "" {
		t.Fatalf("hub_group_assign 回执缺 task_id：%+v", assign)
	}
	if got := p4StrVal(assign["to_agent"]); got != string(b.ID) {
		t.Fatalf("hub_group_assign to_agent = %q, want %q", got, b.ID)
	}
	if assign["dispatched"] != true {
		t.Fatalf("目标空闲时任务应已派发（dispatched=true）：%+v", assign)
	}
	if n := p4Num(assign["seq"]); n == 0 {
		t.Fatalf("派任务消息缺 seq：%+v", assign)
	}

	assignEv := sock.await("hub_message(指派)", func(f map[string]any) bool {
		return f["event_type"] == "hub_message" && p4StrVal(p4Message(f)["task_id"]) == taskID
	})
	assignMsg := p4Message(assignEv)
	if got := p4StrVal(assignMsg["intent"]); got != string(agenthub.IntentAssign) {
		t.Fatalf("指派消息 intent = %q", got)
	}
	if p4SenderKind(assignMsg) != string(agenthub.SenderHuman) {
		t.Fatalf("指派消息发送者 = %v, want human（人派活不能署智能体的名）", assignMsg["sender"])
	}
	if mentions := p4StrList(assignMsg["mentions"]); len(mentions) != 1 || mentions[0] != string(b.ID) {
		t.Fatalf("指派消息结构化 mentions = %v, want [%s]", mentions, b.ID)
	}
	if got := p4StrVal(assignMsg["text"]); got != instruction {
		t.Fatalf("指派消息正文 = %q, want %q", got, instruction)
	}

	// 任务状态机：dispatched → running → completed，线上必须看得到 running。
	doneEv := sock.await("hub_task_update(completed)", func(f map[string]any) bool {
		return f["event_type"] == "hub_task_update" &&
			p4StrVal(f["task_id"]) == taskID && p4StrVal(f["status"]) == string(agenthub.TaskCompleted)
	})
	if got := p4StrVal(doneEv["target_agent_id"]); got != string(b.ID) {
		t.Fatalf("hub_task_update.target_agent_id = %q, want %q", got, b.ID)
	}
	statuses := make([]string, 0, 4)
	for _, f := range sock.framesOf("hub_task_update", func(f map[string]any) bool {
		return p4StrVal(f["task_id"]) == taskID
	}) {
		statuses = append(statuses, p4StrVal(f["status"]))
	}
	idxOf := func(want string) int {
		for i, s := range statuses {
			if s == want {
				return i
			}
		}
		return -1
	}
	runIdx, doneIdx := idxOf(string(agenthub.TaskRunning)), idxOf(string(agenthub.TaskCompleted))
	if runIdx < 0 || doneIdx < 0 || runIdx > doneIdx {
		t.Fatalf("任务状态事件顺序 = %v, want 含 running 且在 completed 之前", statuses)
	}
	if n := countStr(statuses, string(agenthub.TaskCompleted)); n != 1 {
		t.Fatalf("completed 状态事件 = %d 条, want 1（%v）", n, statuses)
	}

	// 结算落盘 + 群里恰好一张终态结果卡。
	var task *agenthub.Task
	p4WaitStore(t, "任务结算 completed + 结果卡", func() bool {
		task = p3bEvalLoadTask(t, m, agenthub.TaskID(taskID))
		return task != nil && task.Status == agenthub.TaskCompleted &&
			len(p3bTaskResultCards(p3bMessages(t, m, g.ID), task.ID)) == 1
	})
	if task.Target.Kind != agenthub.TargetAgent || task.Target.AgentID != b.ID || task.Target.GroupID != g.ID {
		t.Fatalf("任务目标 = %+v", task.Target)
	}
	if task.Origin.Kind != agenthub.OriginHuman {
		t.Fatalf("任务来源 = %s, want human（手机 UI 是人派的活）", task.Origin.Kind)
	}
	if n := fb.callCount(); n != 1 {
		t.Fatalf("B 的 provider 调用 = %d, want 1", n)
	}
	cards := p3bTaskResultCards(p3bMessages(t, m, g.ID), task.ID)
	if got := p3bRefString(t, cards[0].Refs, "status"); got != string(agenthub.TaskCompleted) {
		t.Fatalf("结果卡 refs.status = %q, want completed", got)
	}
	if cards[0].Sender.Kind != agenthub.SenderAgent || cards[0].Sender.AgentID != b.ID {
		t.Fatalf("结果卡 sender = %+v, want 执行者 B", cards[0].Sender)
	}
	if len(cards[0].Mentions) != 0 {
		t.Fatalf("结果卡不该带 mentions（会误唤醒）：%v", cards[0].Mentions)
	}

	cardEv := sock.await("hub_message(task_result)", func(f map[string]any) bool {
		return f["event_type"] == "hub_message" &&
			p4StrVal(p4Message(f)["kind"]) == string(agenthub.KindTaskResult)
	})
	cardMsg := p4Message(cardEv)
	if got := p4StrVal(cardMsg["id"]); got != string(cards[0].ID) {
		t.Fatalf("线上结果卡 id = %q, want %q", got, cards[0].ID)
	}
	if got := p4StrVal(p4Refs(cardMsg)["status"]); got != string(agenthub.TaskCompleted) {
		t.Fatalf("线上结果卡 refs.status = %q", got)
	}
	// 结果卡只发一次（重复 AgentEnd / 泵重放都靠确定性 client_msg_id 幂等）。
	sock.expectNone("第二条 task_result 卡", func(f map[string]any) bool {
		return f["event_type"] == "hub_message" &&
			p4StrVal(p4Message(f)["kind"]) == string(agenthub.KindTaskResult)
	})
	if n := len(sock.framesOf("hub_message", func(f map[string]any) bool {
		return p4StrVal(p4Message(f)["kind"]) == string(agenthub.KindTaskResult)
	})); n != 1 {
		t.Fatalf("task_result 卡事件 = %d 条, want 1", n)
	}

	// —— 4) 任务板 / Needs / 已读游标：手机读写的都是同一份真实存储 ——
	tasks := h.cmd(t, "hub_task_list", map[string]any{"group_id": string(g.ID)})
	if n := p4Num(tasks["count"]); n != 1 {
		t.Fatalf("hub_task_list count = %d, want 1", n)
	}
	taskRows, _ := tasks["tasks"].([]any)
	row, _ := taskRows[0].(map[string]any)
	if p4StrVal(row["id"]) != taskID || p4StrVal(row["status"]) != string(agenthub.TaskCompleted) {
		t.Fatalf("hub_task_list 行 = %+v", row)
	}

	needs := h.cmd(t, "hub_need_list", map[string]any{"group_id": string(g.ID)})
	if n := p4Num(needs["count"]); n != 0 {
		t.Fatalf("hub_need_list 初始 count = %d, want 0", n)
	}
	st := mustHubStore(t, m)
	need := &agenthub.Need{
		ID: "need_p4_e2e_1", Kind: "approval", Status: "pending",
		AgentID: b.ID, GroupID: g.ID, Anchor: "call_e2e_1",
		Title: "要跑一条命令", Body: "npm test", CreatedAt: time.Now().UTC(),
	}
	if err := st.SaveNeed(context.Background(), need); err != nil {
		t.Fatalf("保存 pending need: %v", err)
	}
	needs = h.cmd(t, "hub_need_list", map[string]any{"group_id": string(g.ID)})
	if n := p4Num(needs["count"]); n != 1 {
		t.Fatalf("hub_need_list 之后 count = %d, want 1（%+v）", n, needs)
	}
	needRows, _ := needs["needs"].([]any)
	needRow, _ := needRows[0].(map[string]any)
	if p4StrVal(needRow["id"]) != "need_p4_e2e_1" || p4StrVal(needRow["kind"]) != "approval" ||
		p4StrVal(needRow["status"]) != "pending" {
		t.Fatalf("hub_need_list 行 = %+v", needRow)
	}

	// 结果卡是智能体消息 → 卡片之后至少 1 条未读；手机 mark_read 推进到末尾后归零。
	before := h.cmd(t, "hub_group_list", nil)
	prog := p4Map(p4Map(before, "progress"), string(g.ID))
	if prog == nil {
		t.Fatalf("hub_group_list 缺 progress[%s]：%+v", g.ID, before)
	}
	if unread := p4Num(prog["unread"]); unread < 1 {
		t.Fatalf("结果卡之后 unread = %d, want >= 1", unread)
	}
	msgs, err := st.ReadMessages(context.Background(), g.ID, 0, 1)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("读群末尾消息: %v", err)
	}
	lastSeq := msgs[len(msgs)-1].Seq
	read := h.cmd(t, "hub_mark_read", map[string]any{"group_id": string(g.ID), "seq": lastSeq})
	if got := p4Num(read["seq"]); got != lastSeq {
		t.Fatalf("hub_mark_read seq = %d, want %d", got, lastSeq)
	}
	if got, err := st.GroupReadSeq(context.Background(), g.ID); err != nil || got != lastSeq {
		t.Fatalf("存储里的群已读游标 = %d (err=%v), want %d", got, err, lastSeq)
	}
	after := h.cmd(t, "hub_group_list", nil)
	if unread := p4Num(p4Map(p4Map(after, "progress"), string(g.ID))["unread"]); unread != 0 {
		t.Fatalf("mark_read 之后 unread = %d, want 0", unread)
	}

	// —— 5) 断线重连：不重放 + 手机重拉快照 → 不重复、终态卡仍在 ——
	sock.close()
	const offlineText = "断线期间的一句话"
	offline := h.cmd(t, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": offlineText, "client_msg_id": "e2e-offline-1",
	})
	offlineID := p4StrVal(offline["message_id"])
	if offlineID == "" {
		t.Fatalf("断线期间的发送回执缺 message_id：%+v", offline)
	}

	sock2 := p4DialHub(t, h)
	hello2 := sock2.await("hello(重连)", func(f map[string]any) bool { return f["type"] == "hello" })
	if hello2["scope"] != "hub" {
		t.Fatalf("重连 hello = %+v", hello2)
	}
	// hub 流没有 replay 语义：重连只回 hello，历史一律由快照重拉（app.js hubResync）。
	sock2.expectNone("重连后重放历史 hub_message", func(f map[string]any) bool {
		return f["event_type"] == "hub_message"
	})

	// 手机 resync 路径：hello 之后重拉群历史，再与已收实时帧合并。
	resyncHist := h.cmd(t, "hub_group_history", map[string]any{"group_id": string(g.ID), "limit": 200})
	snapshot := p4JSONMessages(t, resyncHist["messages"])
	live := append(p4LiveMessages(sock.history, g.ID), p4LiveMessages(sock2.history, g.ID)...)
	// 非空证明：断线前的实时帧确实参与了后面的合并（否则「无重复」是空断言）。
	assignID := p4StrVal(assignMsg["id"])
	wantLive := []struct{ name, id string }{
		{"人 @A 的触发消息", sendID},
		{"A 的 final", finalID},
		{"任务指派消息", assignID},
		{"任务结果卡", string(cards[0].ID)},
	}
	liveIDs := map[string]bool{}
	for _, msg := range live {
		liveIDs[p4StrVal(msg["id"])] = true
	}
	for _, want := range wantLive {
		if want.id == "" || !liveIDs[want.id] {
			t.Fatalf("实时帧里缺%s（id=%q）：重连合并断言会变成空断言", want.name, want.id)
		}
	}
	merged, fresh := p4MergeMessages(snapshot, live)
	if fresh != 0 {
		t.Fatalf("快照合并多出 %d 条实时帧里没有定义的消息（重连会造重复）", fresh)
	}
	if len(merged) != len(snapshot) {
		t.Fatalf("合并后 %d 条 != 快照 %d 条：同一条消息出现了两遍", len(merged), len(snapshot))
	}
	seenSeq := map[int64]int{}
	for _, msg := range merged {
		seenSeq[p4Num(msg["seq"])]++
	}
	for seq, n := range seenSeq {
		if n != 1 {
			t.Fatalf("seq=%d 在合并结果里出现 %d 次", seq, n)
		}
	}
	// 断线期间那条只在快照里（证明 resync 真的补上了漏掉的事件）。
	var offlineMsg map[string]any
	for _, msg := range snapshot {
		if p4StrVal(msg["id"]) == offlineID {
			offlineMsg = msg
		}
	}
	if offlineMsg == nil || p4StrVal(offlineMsg["text"]) != offlineText {
		t.Fatalf("重拉快照没补上断线期间的消息 %q", offlineText)
	}
	// 终态任务卡仍在，且恰好一张。
	mergedCards := make([]map[string]any, 0, 1)
	for _, msg := range merged {
		if p4StrVal(msg["kind"]) == string(agenthub.KindTaskResult) {
			mergedCards = append(mergedCards, msg)
		}
	}
	if len(mergedCards) != 1 {
		t.Fatalf("合并后任务卡 = %d 张, want 1", len(mergedCards))
	}
	if p4StrVal(mergedCards[0]["id"]) != string(cards[0].ID) {
		t.Fatalf("合并后的任务卡 id = %q, want %q", p4StrVal(mergedCards[0]["id"]), cards[0].ID)
	}
	if got := p4StrVal(p4Refs(mergedCards[0])["status"]); got != string(agenthub.TaskCompleted) {
		t.Fatalf("合并后任务卡 status = %q, want completed", got)
	}
	mergedIDs := map[string]int{}
	for _, msg := range merged {
		mergedIDs[p4StrVal(msg["id"])]++
	}
	for _, want := range wantLive {
		if n := mergedIDs[want.id]; n != 1 {
			t.Fatalf("合并后%s（%s）出现 %d 次, want 1", want.name, want.id, n)
		}
	}
}

// ---------- 用例 2：停止自动对话（手机入口 + 线上顺序） ----------

func TestMobileP4E2EHubStop(t *testing.T) {
	h := p4StartMobile(t)
	m, buf := h.m, h.buf

	a := createDispatchAgent(t, m, buf, 1, "P4StopA", true)
	b := createDispatchAgent(t, m, buf, 2, "P4StopB", false)
	g := assignGroup(t, m, buf, 3, "P4 停止群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member",
	})

	sock := p4DialHub(t, h)
	if hello := sock.await("hello", func(f map[string]any) bool { return f["type"] == "hello" }); hello["scope"] != "hub" {
		t.Fatalf("hello = %+v", hello)
	}

	gate := make(chan struct{})
	var gateOnce sync.Once
	release := func() { gateOnce.Do(func() { close(gate) }) }
	defer release() // 失败路径也别把 goroutine 留到 TempDir 清理之后

	fa := newP3bEvalProvider(
		// A 的 run 里用生产 group_say @B：消息落账 + B 的唤醒入队（A 还在跑，泵不放行）。
		p3bEvalRound{tool: &core.ToolCall{
			Id: "call_say", Index: 0, Name: "group_say",
			Arguments: fmt.Sprintf(`{"group_id":%q,"text":"@P4StopB 你来写结论。"}`, string(g.ID)),
		}},
		// 第二轮卡住：停止要打在 run 中途，B 的唤醒还留在队列里。
		p3bEvalRound{content: "我先起了个头。", gate: gate},
	)
	fb := newP3bEvalProvider(p3bEvalRound{content: "B 不该跑起来"})
	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))
	setGroupFakeLoopIters(t, m, bsA, g, string(a.ID), fa, 2)
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	setGroupFakeLoopIters(t, m, bsB, g, string(b.ID), fb, 1)

	// P6-02：不同目标现在是**并行**放行的，A @B 会立刻投出去。要让这条唤醒停在
	// 队列里等停止（本用例的意图），把 B 标成「上一轮还没结束」即可。
	bsB.hubGroupBusy.Store(true)
	defer bsB.hubGroupBusy.Store(false)
	h.cmd(t, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@P4StopA 你先起个头",
		"mentions": []any{string(a.ID)},
	})
	convWait(t, "A @B 的唤醒进入队列", func() bool {
		v := convView(m, g.ID)
		return v.QueueLen == 1 && v.TurnsUsed == 1
	})
	if n := fb.callCount(); n != 0 {
		t.Fatalf("B 在停止前就被启动了（%d 次 provider 调用）", n)
	}

	stop := h.cmd(t, "hub_conversation_stop", map[string]any{"group_id": string(g.ID)})
	conv := p4Map(stop, "conversation")
	if p4StrVal(conv["status"]) != string(agenthub.ConversationStopped) {
		t.Fatalf("停止回执 = %+v", stop)
	}
	if n := p4Num(stop["interrupted"]); n < 1 {
		t.Fatalf("interrupted = %v, want >= 1", stop["interrupted"])
	}
	// 放开 gate：被中断的 run 收尾之后，队列不能把 B 补投出去。
	release()
	convWait(t, "A 的 run 收尾", func() bool {
		return !bsA.hubGroupBusy.Load() && !bsA.running.Load()
	})
	if n := fb.callCount(); n != 0 {
		t.Fatalf("停止之后 B 还是跑了（%d 次 provider 调用）", n)
	}
	if v := convView(m, g.ID); v.Status != agenthub.ConversationStopped || v.QueueLen != 0 || v.TurnsUsed != 1 {
		t.Fatalf("停止后对话视图 = %+v, want stopped/queue=0/turns=1", v)
	}

	// 存储：恰好一条停止系统消息；它之后没有任何智能体正文。
	// 「run 恰好在停止窗口里收尾」的抑制路径由 TestHubConversationFastRunAfterStopWritesNoFinal
	// 用 hubFinishRoundHook 钉住；这里负责的是手机入口 + 队列/中断这条真实链路。
	msgs := p3bMessages(t, m, g.ID)
	systems := convSystemMessages(msgs)
	if len(systems) != 1 || !strings.Contains(systems[0].Text, "停止了自动对话") {
		t.Fatalf("停止系统消息 = %+v, want 恰好一条", systems)
	}
	stopSeq := systems[0].Seq
	for _, msg := range msgs {
		if msg.Seq <= stopSeq || msg.Sender.Kind != agenthub.SenderAgent {
			continue
		}
		t.Fatalf("停止之后还有智能体发言 seq=%d text=%q", msg.Seq, msg.Text)
	}

	// 线上顺序：stop 的 hub_conversation 之后不允许再出现该群的智能体 hub_message。
	sock.await("hub_conversation(stopped)", func(f map[string]any) bool {
		if f["event_type"] != "hub_conversation" || p4StrVal(f["group_id"]) != string(g.ID) {
			return false
		}
		return p4StrVal(p4Map(f, "conversation")["status"]) == string(agenthub.ConversationStopped)
	})
	// await 命中即返回，命中的帧永远是 history 的最后一项。
	stopAt := len(sock.history) - 1
	// 观察窗口：迟到的 final / 补唤醒也得在这里暴露（顺便把已排队帧读进 history）。
	sock.expectNone("停止后的智能体 hub_message", func(f map[string]any) bool {
		return f["event_type"] == "hub_message" && p4StrVal(f["group_id"]) == string(g.ID) &&
			p4SenderKind(p4Message(f)) == string(agenthub.SenderAgent)
	})
	for i := stopAt + 1; i < len(sock.history); i++ {
		f := sock.history[i]
		if f["event_type"] != "hub_message" || p4StrVal(f["group_id"]) != string(g.ID) {
			continue
		}
		if p4SenderKind(p4Message(f)) == string(agenthub.SenderAgent) {
			t.Fatalf("停止事件之后线上又出现智能体正文：%+v", p4Message(f))
		}
	}

	bSession := hubSessionID(string(b.ID), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID})
	if n := p3bEvalEventCount(buf, "agent_start", func(ev map[string]any) bool {
		return ev["session_id"] == bSession
	}); n != 0 {
		t.Fatalf("B 有 %d 个 agent_start, want 0", n)
	}
	if n := p3bEvalEventCount(buf, "hub_conversation", func(ev map[string]any) bool {
		cv, _ := ev["conversation"].(map[string]any)
		return cv != nil && cv["status"] == string(agenthub.ConversationStopped) &&
			ev["group_id"] == string(g.ID)
	}); n != 1 {
		t.Fatalf("stopped 的 hub_conversation 事件 = %d 条, want 1（幂等）", n)
	}
	if n := len(p3bTaskResultCards(msgs, "")); n != 0 {
		t.Fatalf("停止链路上冒出 %d 张任务卡, want 0", n)
	}
}

func countStr(items []string, want string) int {
	n := 0
	for _, s := range items {
		if s == want {
			n++
		}
	}
	return n
}
