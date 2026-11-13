package main

// P3b-04：对话控制器的 bridge 接线 —— 轮次预算、深度、踢皮球、同内容循环、冷却、
// 人类优先（清队列）、停止按钮、忙目标任务泵、hub_conversation 事件。
//
// 全部用确定性 fake provider：链路只在进程内跑，不碰网络。等待一律用轮询
// （5–10ms，超时 5s）+ 阻塞式 fake（channel 门闩），不靠长 sleep 猜时序。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

// convProvider 脚本化 fake：按调用次序回放 reply，记录每次开流时间与请求。
// block 非 nil 时先等门闩（用来把「目标正忙」做成确定性窗口）。
type convProvider struct {
	mu      sync.Mutex
	replies []string
	idx     int
	times   []time.Time
	reqs    []provider.StreamRequest
	started chan struct{}
	release <-chan struct{}
}

func newConvProvider(replies ...string) *convProvider {
	return &convProvider{replies: replies, started: make(chan struct{}, 1)}
}

// gated 让这个 provider 每次开流先阻塞，直到测试 close 掉返回的 release。
func (p *convProvider) gated() chan struct{} {
	ch := make(chan struct{})
	p.release = ch
	return ch
}

func (p *convProvider) Stream(ctx context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	p.mu.Lock()
	p.times = append(p.times, time.Now())
	if req != nil {
		p.reqs = append(p.reqs, *req)
	}
	reply := ""
	if len(p.replies) > 0 {
		if p.idx < len(p.replies) {
			reply = p.replies[p.idx]
		} else {
			reply = p.replies[len(p.replies)-1]
		}
	}
	p.idx++
	release := p.release
	p.mu.Unlock()
	select {
	case p.started <- struct{}{}:
	default:
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	model := ""
	if req != nil {
		model = req.Model
	}
	return h(provider.LLMEndEvent{
		FinishReason: core.FinishReasonStop, Content: reply, Model: model,
		Usage: core.Usage{Input: 12, Output: 8},
	})
}

func (p *convProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.times)
}

func (p *convProvider) lastRequest() *provider.StreamRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.reqs) == 0 {
		return nil
	}
	cp := p.reqs[len(p.reqs)-1]
	return &cp
}

// waitStarted 等这个 provider 至少开流一次（门闩式 fake 的确定性锚点）。
func (p *convProvider) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider 没有被调用：唤醒链路没走通")
	}
}

// convLimits 注入对话限额（必须在第一次人类消息之前），并返回 manager。
func convLimits(m *manager, limits agenthub.ConversationLimits) {
	m.hubConvLimits = &limits
}

// convWait 轮询直到 pred 为真（5ms 间隔，5s 超时）。
func convWait(t *testing.T, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

func convView(m *manager, gid agenthub.GroupID) agenthub.ConversationView {
	return m.hubConversationView(gid)
}

// waitConvTurns 等某群对话用掉 want 轮。
func waitConvTurns(t *testing.T, m *manager, gid agenthub.GroupID, want int) agenthub.ConversationView {
	t.Helper()
	var v agenthub.ConversationView
	convWait(t, "对话轮次到达", func() bool {
		v = convView(m, gid)
		return v.TurnsUsed >= want
	})
	return v
}

func convGroupMessages(t *testing.T, m *manager, gid agenthub.GroupID) []agenthub.Message {
	t.Helper()
	msgs, err := mustHubStore(t, m).ReadMessages(context.Background(), gid, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func convSystemMessages(msgs []agenthub.Message) []agenthub.Message {
	var out []agenthub.Message
	for _, msg := range msgs {
		if msg.Kind == agenthub.KindSystem && msg.Sender.Kind == agenthub.SenderSystem {
			out = append(out, msg)
		}
	}
	return out
}

func convAgentMessages(msgs []agenthub.Message, id agenthub.AgentID) int {
	n := 0
	for _, msg := range msgs {
		if msg.Sender.Kind == agenthub.SenderAgent && msg.Sender.AgentID == id {
			n++
		}
	}
	return n
}

// waitConvSystemMessage 等群里出现一条含 substr 的系统消息。
func waitConvSystemMessage(t *testing.T, m *manager, gid agenthub.GroupID, substr string) []agenthub.Message {
	t.Helper()
	var msgs []agenthub.Message
	convWait(t, "对话系统消息："+substr, func() bool {
		msgs = convGroupMessages(t, m, gid)
		for _, msg := range convSystemMessages(msgs) {
			if strings.Contains(msg.Text, substr) {
				return true
			}
		}
		return false
	})
	return msgs
}

func convConversationEvents(t *testing.T, buf *syncBuf) []map[string]any {
	t.Helper()
	return hubEvents(t, buf, "hub_conversation")
}

func convEventStatus(t *testing.T, ev map[string]any) string {
	t.Helper()
	conv, _ := ev["conversation"].(map[string]any)
	if conv == nil {
		t.Fatalf("hub_conversation 事件缺 conversation: %+v", ev)
	}
	status, _ := conv["status"].(string)
	return status
}

// convWakeHeaders 从一次 LLM 请求里取出所有 hub_group_wake header。
func convWakeHeaders(req *provider.StreamRequest) []*agenthub.GroupWakeHeader {
	if req == nil {
		return nil
	}
	var out []*agenthub.GroupWakeHeader
	for _, msg := range req.Messages {
		for _, c := range msg.Content {
			if c.Type != core.ContentTypeHubGroupWake {
				continue
			}
			h, _, err := agenthub.ParseGroupWake(c.Content)
			if err != nil || h == nil {
				continue
			}
			out = append(out, h)
		}
	}
	return out
}

// convGroupWith 建群 + 建智能体（成员顺序固定，owner = 第一个）。
func convGroupWith(t *testing.T, m *manager, buf *syncBuf, cmdID int, groupName string, policy map[string]any, members ...agenthub.AgentProfile) *agenthub.Group {
	t.Helper()
	raw := make([]struct {
		id   agenthub.AgentID
		role string
	}, 0, len(members))
	for i, a := range members {
		role := "member"
		if i == 0 {
			role = "owner"
		}
		raw = append(raw, struct {
			id   agenthub.AgentID
			role string
		}{a.ID, role})
	}
	return p3bGroup(t, m, buf, cmdID, groupName, policy, raw...)
}

// TestHubConversationEightTurnsThenExhausted：8 轮之后第 9 条 @ 被护栏拦下，
// 群里只多一条系统消息、只多一个 exhausted 事件（不刷屏）。
func TestHubConversationEightTurnsThenExhausted(t *testing.T) {
	m, buf := testManager(t)
	// 隔离预算：深度/踢皮球/冷却放到宽松值，只让 8 轮上限起作用。
	convLimits(m, agenthub.ConversationLimits{TurnBudget: 8, MaxDepth: 99, PairLimit: 99, Cooldown: -1})

	a := createHubAgent(t, m, buf, 1, "轮次A")
	b := createHubAgent(t, m, buf, 2, "轮次B")
	c := createHubAgent(t, m, buf, 3, "轮次C")
	d := createHubAgent(t, m, buf, 4, "轮次D")
	g := convGroupWith(t, m, buf, 10, "8轮群",
		nil, a, b, c, d)
	for _, ag := range []agenthub.AgentProfile{a, b, c, d} {
		p3bWireFake(t, m, g, string(ag.ID), "收到")
	}
	order := []agenthub.AgentProfile{a, b, c, d, a, b, c, d}
	next := []agenthub.AgentProfile{b, c, d, a, b, c, d, a}
	for i := 0; i < 8; i++ {
		sender, target := order[i], next[i]
		res, err := p3bGroupSay(t, m, string(sender.ID), map[string]any{
			"group_id": string(g.ID), "text": "第 " + string(rune('1'+i)) + " 轮派活",
			"mentions": []string{string(target.ID)},
		})
		if err != nil {
			t.Fatalf("第 %d 轮 group_say: %v", i+1, err)
		}
		if got := toStringIDs(res["targets"]); !p3bSameIDs(got, string(target.ID)) {
			t.Fatalf("第 %d 轮 targets = %v", i+1, got)
		}
		waitConvTurns(t, m, g.ID, i+1)
	}
	if v := convView(m, g.ID); v.TurnsUsed != 8 || v.Status != agenthub.ConversationRunning {
		t.Fatalf("8 轮后对话 = %+v", v)
	}

	// 第 9 轮：工具调用本身成功（消息照发），但没有人被唤醒。
	res, err := p3bGroupSay(t, m, string(a.ID), map[string]any{
		"group_id": string(g.ID), "text": "第 9 轮还想继续说",
		"mentions": []string{string(b.ID)},
	})
	if err != nil {
		t.Fatalf("第 9 轮 group_say 不该报错（消息照发）: %v", err)
	}
	if _, ok := res["wake_errors"]; ok {
		t.Fatalf("护栏命中不该冒充投递错误: %+v", res)
	}
	conv, _ := res["conversation"].(map[string]any)
	if conv == nil || conv["status"] != string(agenthub.ConversationExhausted) {
		t.Fatalf("第 9 轮结果缺 exhausted 视图: %+v", res)
	}
	v := convView(m, g.ID)
	if v.Status != agenthub.ConversationExhausted || v.TurnsUsed != 8 || v.QueueLen != 0 {
		t.Fatalf("第 9 轮后对话 = %+v", v)
	}
	if !strings.Contains(v.StopReason, "8") {
		t.Fatalf("停止原因缺预算数字: %q", v.StopReason)
	}

	msgs := waitConvSystemMessage(t, m, g.ID, "上限")
	if got := len(convSystemMessages(msgs)); got != 1 {
		t.Fatalf("系统消息 = %d 条, want 1", got)
	}
	events := convConversationEvents(t, buf)
	exhausted := 0
	for _, ev := range events {
		if convEventStatus(t, ev) == string(agenthub.ConversationExhausted) {
			exhausted++
			if ev["group_id"] != string(g.ID) {
				t.Fatalf("事件缺 group_id: %+v", ev)
			}
		}
	}
	if exhausted != 1 {
		t.Fatalf("exhausted 事件 = %d 个, want 1（事件: %d）", exhausted, len(events))
	}

	// 再 @ 一次：仍然只有一条系统消息、一个事件（不刷屏）。
	if _, err := p3bGroupSay(t, m, string(b.ID), map[string]any{
		"group_id": string(g.ID), "text": "第 10 轮继续刷",
		"mentions": []string{string(c.ID)},
	}); err != nil {
		t.Fatalf("第 10 轮 group_say: %v", err)
	}
	if got := len(convSystemMessages(convGroupMessages(t, m, g.ID))); got != 1 {
		t.Fatalf("重复触发后系统消息 = %d 条, want 1", got)
	}
	events = convConversationEvents(t, buf)
	if len(events) != len(convConversationEvents(t, buf)) || events == nil {
		t.Fatalf("事件读取不稳定：%d", len(events))
	}
	exhausted = 0
	for _, ev := range events {
		if convEventStatus(t, ev) == string(agenthub.ConversationExhausted) {
			exhausted++
		}
	}
	if exhausted != 1 {
		t.Fatalf("重复触发后 exhausted 事件 = %d 个, want 1", exhausted)
	}
}

// TestHubConversationDepthStopsAtFourHops：人类→A(1)→B(2)→C(3)→D(4) 之后，
// D 再 @E 就是第 5 跳 → 不入队、不唤醒 E，写一条深度原因的系统消息。
func TestHubConversationDepthStopsAtFourHops(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1, MaxDepth: 4})

	a := createHubAgent(t, m, buf, 1, "深度A")
	b := createHubAgent(t, m, buf, 2, "深度B")
	c := createHubAgent(t, m, buf, 3, "深度C")
	d := createHubAgent(t, m, buf, 4, "深度D")
	e := createHubAgent(t, m, buf, 5, "深度E")
	g := convGroupWith(t, m, buf, 10, "深度群", nil, a, b, c, d, e)
	fe := newConvProvider("E 不该被叫醒")
	p3bWireFake(t, m, g, string(a.ID), "@深度B 我 A 说完")
	p3bWireFake(t, m, g, string(b.ID), "@深度C 我 B 说完")
	p3bWireFake(t, m, g, string(c.ID), "@深度D 我 C 说完")
	p3bWireFake(t, m, g, string(d.ID), "@深度E 我 D 说完")
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(e.ID), string(g.ID)), g, string(e.ID), fe)

	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@深度A 走四跳看看",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	msgs := waitConvSystemMessage(t, m, g.ID, "深度")
	v := convView(m, g.ID)
	// P6-04：到限只拒绝该链继续 @（waiting_human），不把整个群判死（exhausted）。
	if v.Status != agenthub.ConversationWaitingHuman {
		t.Fatalf("四跳之后对话 = %+v, want waiting_human", v)
	}
	if v.Depth != 4 {
		t.Fatalf("最后一跳深度 = %d, want 4", v.Depth)
	}
	if got := len(convSystemMessages(msgs)); got != 1 {
		t.Fatalf("系统消息 = %d 条, want 1", got)
	}
	// 四跳都真的跑过（否则「E 没被叫醒」可能只是因为链没走起来）。
	for _, ag := range []agenthub.AgentProfile{a, b, c, d} {
		if n := convAgentMessages(msgs, ag.ID); n == 0 {
			t.Fatalf("%s 没有发言，链条没走通", ag.Name)
		}
	}
	if n := fe.callCount(); n != 0 {
		t.Fatalf("深度 5 的目标被唤醒了 %d 次", n)
	}
}

// TestHubConversationPairLimitStopsPingPong：A↔B 同一对互踢到上限就停 —— 即使每句
// 内容都不同（靠指纹拦不住的那种踢皮球）。
func TestHubConversationPairLimitStopsPingPong(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{TurnBudget: 99, MaxDepth: 99, PairLimit: 2, Cooldown: -1})

	a := createHubAgent(t, m, buf, 1, "斗嘴A")
	b := createHubAgent(t, m, buf, 2, "斗嘴B")
	g := convGroupWith(t, m, buf, 10, "斗嘴群", nil, a, b)
	p3bWireFake(t, m, g, string(a.ID), "@斗嘴B A 第一次")
	p3bWireFake(t, m, g, string(b.ID), "@斗嘴A B 第一次")
	// A 每次都说不同的话：这样踢皮球只能靠 PairCount 拦，指纹拦不住。
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(a.ID), string(g.ID)), g, string(a.ID),
		newConvProvider("@斗嘴B A 第一次", "@斗嘴B A 第二次", "@斗嘴B A 第三次"))
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(b.ID), string(g.ID)), g, string(b.ID),
		newConvProvider("@斗嘴A B 第一次", "@斗嘴A B 第二次"))

	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@斗嘴A 你们俩聊聊",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	msgs := waitConvSystemMessage(t, m, g.ID, "互踢")
	v := convView(m, g.ID)
	if v.Status != agenthub.ConversationWaitingHuman {
		t.Fatalf("踢皮球之后对话 = %+v, want waiting_human", v)
	}
	if got := v.PairCount[agenthub.ConversationPairKey(a.ID, b.ID)]; got != 2 {
		t.Fatalf("A→B = %d, want 2（PairCount=%v）", got, v.PairCount)
	}
	// 轮次 = 人类首轮 + A@B + B@A = 3；A 的第三次 @B 被 pair 上限跳过（不算轮次）。
	if v.TurnsUsed != 3 {
		t.Fatalf("turns used = %d, want 3（人类首轮 + 两次往返）", v.TurnsUsed)
	}
	if got := len(convSystemMessages(msgs)); got != 1 {
		t.Fatalf("系统消息 = %d 条, want 1", got)
	}
	// A 说了两次（第二次的 @ 被拦），B 只说了一次。
	if n := convAgentMessages(msgs, a.ID); n != 2 {
		t.Fatalf("A 的群消息 = %d, want 2", n)
	}
	if n := convAgentMessages(msgs, b.ID); n != 1 {
		t.Fatalf("B 的群消息 = %d, want 1", n)
	}
}

// TestHubConversationFingerprintLoopStops：同一对 agent 重复同一段话（标点/空白
// 变体）→ 判定循环，不再唤醒。
func TestHubConversationFingerprintLoopStops(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{MaxDepth: 99, PairLimit: 99, Cooldown: -1})

	a := createHubAgent(t, m, buf, 1, "循环A")
	b := createHubAgent(t, m, buf, 2, "循环B")
	g := convGroupWith(t, m, buf, 10, "循环群", nil, a, b)
	// A 每次都复读同一段话；B 每次都把话头推回 A。
	p3bWireFake(t, m, g, string(a.ID), "@循环B 这件事我来处理，请你确认。")
	p3bWireFake(t, m, g, string(b.ID), "@循环A 请你再确认一遍。")
	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@循环A 你们对一下",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	msgs := waitConvSystemMessage(t, m, g.ID, "循环")
	v := convView(m, g.ID)
	if v.Status != agenthub.ConversationExhausted {
		t.Fatalf("复读之后对话 = %+v", v)
	}
	if !strings.Contains(v.StopReason, "循环") {
		t.Fatalf("停止原因 = %q, want 循环", v.StopReason)
	}
	if got := len(convSystemMessages(msgs)); got != 1 {
		t.Fatalf("系统消息 = %d 条, want 1", got)
	}
	// A 说了两遍（第二遍被拦），B 只说了一遍。
	if n := convAgentMessages(msgs, a.ID); n != 2 {
		t.Fatalf("A 的群消息 = %d, want 2", n)
	}
	if n := convAgentMessages(msgs, b.ID); n != 1 {
		t.Fatalf("B 的群消息 = %d, want 1", n)
	}
}

// TestHubConversationStaleGenerationWakeDropped（P6-04 §1.1/§1.5）：四跳旧链还在跑
// （header depth=4）时人类插话开新对话 —— 旧链路回来 group_say @E **不得进入新对话**：
// 它的 wake 记 skipped(stale_conversation)，E 一次都不会被叫醒，新对话的深度额度
// 一条都不被消耗。
func TestHubConversationStaleGenerationWakeDropped(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1, MaxDepth: 4})

	a := createHubAgent(t, m, buf, 1, "旧链A")
	b := createHubAgent(t, m, buf, 2, "旧链B")
	c := createHubAgent(t, m, buf, 3, "旧链C")
	d := createHubAgent(t, m, buf, 4, "旧链D")
	e := createHubAgent(t, m, buf, 5, "旧链E")
	g := convGroupWith(t, m, buf, 10, "旧链群", nil, a, b, c, d, e)
	p3bWireFake(t, m, g, string(a.ID), "@旧链B 一")
	p3bWireFake(t, m, g, string(b.ID), "@旧链C 二")
	p3bWireFake(t, m, g, string(c.ID), "@旧链D 三")
	fd := newConvProvider("D 看完了不收尾 @") // 不带 @：四跳停在 D
	releaseD := fd.gated()
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(d.ID), string(g.ID)), g, string(d.ID), fd)
	fe := newConvProvider("E 不该被叫醒")
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(e.ID), string(g.ID)), g, string(e.ID), fe)

	// 人类 → A(1) → B(2) → C(3) → D(4)：D 卡在门闩上，header 里攥着 depth=4。
	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@旧链A 走四跳看看",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	fd.waitStarted(t)
	bsD := mustGroupSession(t, m, string(d.ID), string(g.ID))
	convWait(t, "D 的唤醒 header 深度到 4", func() bool { return bsD.hubWakeDepth.Load() == 4 })

	// 人类插话（无 @）：开新对话，depth 归 0。
	if ok, errMsg, _ := hubCmd(t, m, buf, 12, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "先别说了，我看一下",
	}); !ok {
		t.Fatalf("human interject: %s", errMsg)
	}
	if v := convView(m, g.ID); v.Depth != 0 {
		t.Fatalf("人类插话后对话深度 = %d, want 0", v.Depth)
	}
	oldConv := convView(m, g.ID).ID

	// 旧链的 D 还没跑完就回来说话：这条 @ 属于旧 generation → 丢弃。
	if _, err := p3bGroupSay(t, m, string(d.ID), map[string]any{
		"group_id": string(g.ID), "text": "@旧链E 你也看一眼",
		"mentions": []string{string(e.ID)},
	}); err != nil {
		t.Fatalf("旧链路的 group_say: %v", err)
	}
	var stale bool
	for _, dl := range m.hubConversation().Deliveries(g.ID) {
		if dl.Target == e.ID && dl.Status == agenthub.DeliverySkipped && dl.Reason == agenthub.DeliveryReasonStale {
			stale = true
		}
	}
	if !stale {
		t.Fatalf("旧 generation 的 @ 没有被记成 skipped(stale): %+v", m.hubConversation().Deliveries(g.ID))
	}
	close(releaseD)
	time.Sleep(100 * time.Millisecond)
	if n := fe.callCount(); n != 0 {
		t.Fatalf("旧链的 @ 把 E 叫醒了 %d 次（不得进入新对话）", n)
	}
	if v := convView(m, g.ID); v.ID != oldConv || v.Depth != 0 || v.TurnsUsed != 0 || v.Terminal() {
		t.Fatalf("新对话被旧链污染: %+v", v)
	}
	// 新对话照常工作：人类 @E → 第 1 跳。
	if ok, errMsg, _ := hubCmd(t, m, buf, 13, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@旧链E 现在你说",
	}); !ok {
		t.Fatalf("human send 2: %s", errMsg)
	}
	fe.waitStarted(t)
	headers := convWakeHeaders(fe.lastRequest())
	if len(headers) != 1 || headers[0].Depth != 1 {
		t.Fatalf("E 的唤醒 header = %+v, want 1 条 depth=1", headers)
	}
}

// TestHubConversationParallelAcrossTargets（P6-02）：不同目标之间**并行**放行 ——
// A 还在跑时，C @B 的唤醒立刻投出去（旧实现要求「本群一次只跑一个」，已删除）。
// 同一目标忙则留队（不丢、不报错），目标这一轮结束后补投。
func TestHubConversationParallelAcrossTargets(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})

	a := createHubAgent(t, m, buf, 1, "并行A")
	b := createHubAgent(t, m, buf, 2, "并行B")
	c := createHubAgent(t, m, buf, 3, "并行C")
	g := convGroupWith(t, m, buf, 10, "并行群", nil, a, b, c)
	fa := newConvProvider("A 说完了", "A 补投后的话")
	releaseA := fa.gated()
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(a.ID), string(g.ID)), g, string(a.ID), fa)
	fb := newConvProvider("B 说完了")
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(b.ID), string(g.ID)), g, string(b.ID), fb)
	p3bWireFake(t, m, g, string(c.ID), "C 说完了")

	// 人类 @A：A 卡在门闩上（这一轮在飞）。
	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@并行A 你先跑",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	fa.waitStarted(t)

	// C 空闲，直接 group_say @B：**不同目标**，必须立刻并行跑（不再等 A）。
	if _, err := p3bGroupSay(t, m, string(c.ID), map[string]any{
		"group_id": string(g.ID), "text": "@并行B 你跟 A 一起", "mentions": []string{string(b.ID)},
	}); err != nil {
		t.Fatalf("C 的 group_say: %v", err)
	}
	convWait(t, "B 与 A 并行开跑", func() bool { return fb.callCount() >= 1 })
	if v := convView(m, g.ID); v.TurnsUsed != 2 {
		t.Fatalf("turns used = %d, want 2（人类首轮 + B）: %+v", v.TurnsUsed, v)
	}

	// 同一目标再 @ 一次 A：A 还在跑 → 留队（queued/target_busy），不报错、不丢。
	if _, err := p3bGroupSay(t, m, string(c.ID), map[string]any{
		"group_id": string(g.ID), "text": "@并行A 你还没完？", "mentions": []string{string(a.ID)},
	}); err != nil {
		t.Fatalf("C 再 @A: %v", err)
	}
	var queued agenthub.Delivery
	convWait(t, "A 的第二次唤醒排队", func() bool {
		for _, d := range m.hubConversation().Deliveries(g.ID) {
			if d.Target == a.ID && d.Status == agenthub.DeliveryQueued {
				queued = d
				return true
			}
		}
		return false
	})
	if queued.Reason != agenthub.DeliveryReasonBusy {
		t.Fatalf("排队的记录 = %+v, want reason=target_busy", queued)
	}
	if n := fa.callCount(); n != 1 {
		t.Fatalf("A 在忙时被投了第二份快照（调用 %d 次）", n)
	}
	// A 这一轮结束 → 排队的 wake 补投（同一条 wake id，attempts 累加）。
	close(releaseA)
	convWait(t, "A 收尾后补投排队的唤醒", func() bool { return fa.callCount() >= 2 })
}

// TestHubConversationParallelBatchesAllTargets（P6-02）：一次 @ 的全部目标同时开跑
// （并发 = 本轮目标数），后一批不再等前一批 —— 旧「串行」闸门已删除。
func TestHubConversationParallelBatchesAllTargets(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})

	a := createHubAgent(t, m, buf, 1, "批次A")
	b := createHubAgent(t, m, buf, 2, "批次B")
	c := createHubAgent(t, m, buf, 3, "批次C")
	d := createHubAgent(t, m, buf, 4, "批次D")
	g := convGroupWith(t, m, buf, 10, "批次群", nil, a, b, c, d)
	fa := newConvProvider("A 看完了")
	releaseA := fa.gated()
	defer close(releaseA)
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(a.ID), string(g.ID)), g, string(a.ID), fa)
	fb := newConvProvider("B 看完了")
	releaseB := fb.gated()
	defer close(releaseB)
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(b.ID), string(g.ID)), g, string(b.ID), fb)
	p3bWireFake(t, m, g, string(c.ID), "C 说完了")
	fd := newConvProvider("D 最后说")
	releaseD := fd.gated()
	defer close(releaseD)
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(d.ID), string(g.ID)), g, string(d.ID), fd)

	// 人类同时 @A @B：首轮并行。
	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@批次A @批次B 你们一起看",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	fa.waitStarted(t)
	fb.waitStarted(t)

	// C 空闲，直接 group_say @D：A/B 还在跑，但 D 是**另一个目标** → 立刻并行开跑。
	if _, err := p3bGroupSay(t, m, string(c.ID), map[string]any{
		"group_id": string(g.ID), "text": "@批次D 你也一起", "mentions": []string{string(d.ID)},
	}); err != nil {
		t.Fatalf("C 的 group_say: %v", err)
	}
	fd.waitStarted(t)
	// 人类首轮只算 1 轮（两条人类 wake 不各算一轮），D 的自动唤醒再 +1。
	if v := convView(m, g.ID); v.TurnsUsed != 2 {
		t.Fatalf("turns used = %d, want 2（人类首轮 + D）: %+v", v.TurnsUsed, v)
	}
	if v := convView(m, g.ID); v.QueueLen != 0 {
		t.Fatalf("queue = %d, want 0（三条都投出去了）", v.QueueLen)
	}
}

// TestHubConversationStopRejectedAfterShutdown 锁住 N6：关停之后停止按钮不能写群日志。
func TestHubConversationStopRejectedAfterShutdown(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})
	a := createHubAgent(t, m, buf, 1, "关停A")
	g := convGroupWith(t, m, buf, 10, "关停群", nil, a)
	before := len(convSystemMessages(convGroupMessages(t, m, g.ID)))

	m.hubShutdown.Store(true)
	ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_conversation_stop", map[string]any{
		"group_id": string(g.ID),
	})
	if ok {
		t.Fatal("关停之后 hub_conversation_stop 必须拒绝")
	}
	if !strings.Contains(errMsg, "关停") {
		t.Fatalf("错误文案 = %q, want 提到关停", errMsg)
	}
	if got := len(convSystemMessages(convGroupMessages(t, m, g.ID))); got != before {
		t.Fatalf("关停后仍写了系统消息：%d → %d", before, got)
	}
}

// TestHubConversationCooldownGatesNextWake：连续两次智能体唤醒之间至少间隔冷却。
//
// TestHubConversationCooldownGatesNextWake：冷却只作用在**批次之间**（P6-02 之后
// 同一批目标并行放行，不再互相排队）。这里锁两件事：
//
//	1）B 的唤醒落在冷却窗口内 → 留在队列里，Peek 的 ReadyAt = 上一次唤醒 + 冷却；
//	2）真正放行时，两次智能体唤醒的实际间隔 >= 冷却。
//
// 两条都用控制器自己的时间戳，不依赖调度抖动。
func TestHubConversationCooldownGatesNextWake(t *testing.T) {
	const cooldown = 80 * time.Millisecond
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: cooldown, MaxDepth: 99, PairLimit: 99})

	a := createHubAgent(t, m, buf, 1, "冷却A")
	b := createHubAgent(t, m, buf, 2, "冷却B")
	g := convGroupWith(t, m, buf, 10, "冷却群", nil, a, b)
	p3bWireFake(t, m, g, string(a.ID), "@冷却B 你接着说")
	p3bWireFake(t, m, g, string(b.ID), "B 说完了")

	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@冷却A 你先说",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	// turn 1 = 人类首轮唤 A；turn 2 = A 的回复唤 B。
	v2 := waitConvTurns(t, m, g.ID, 2)
	if v2.LastWakeAt == nil {
		t.Fatalf("turn 2 缺 last_wake_at: %+v", v2)
	}
	t2 := *v2.LastWakeAt

	// B 回复 @A：A 的唤醒必须等冷却窗口（冷却只挡「下一次唤醒」，不是同一批）。
	if _, err := p3bGroupSay(t, m, string(b.ID), map[string]any{
		"group_id": string(g.ID), "text": "@冷却A 再来一轮", "mentions": []string{string(a.ID)},
	}); err != nil {
		t.Fatalf("B 的 group_say: %v", err)
	}
	ctrl := m.hubConversation()
	head, ok := ctrl.Peek(g.ID)
	if !ok {
		t.Fatal("A 的第二次唤醒应当还在队列里（冷却中）—— 没有冷却就会被立刻放行")
	}
	if head.Target != a.ID {
		t.Fatalf("队首 = %+v, want → A", head)
	}
	if ready := head.ReadyAt.Sub(t2); ready < cooldown-5*time.Millisecond {
		t.Fatalf("A 的可跑时刻距上一次唤醒 = %v, want >= %v（冷却没生效）", ready, cooldown)
	}

	v3 := waitConvTurns(t, m, g.ID, 3)
	gap := v3.LastWakeAt.Sub(t2)
	if gap < cooldown-5*time.Millisecond {
		t.Fatalf("两次智能体唤醒间隔 = %v, want >= %v（冷却没生效）", gap, cooldown)
	}
	if v := convView(m, g.ID); v.CooldownMS != cooldown.Milliseconds() {
		t.Fatalf("视图冷却 = %dms, want %dms", v.CooldownMS, cooldown.Milliseconds())
	}
	// 每轮推进都广播一个 hub_conversation（UI 靠它画「第 N 轮 · 深度」）。
	turnEvents := 0
	for _, ev := range convConversationEvents(t, buf) {
		conv, _ := ev["conversation"].(map[string]any)
		if conv == nil {
			continue
		}
		if used, _ := conv["turns_used"].(float64); used >= 2 {
			turnEvents++
		}
	}
	if turnEvents == 0 {
		t.Fatal("没有 turns_used>=2 的 hub_conversation 事件（轮次推进没广播）")
	}
}

// TestHubConversationHumanClearsQueue：人类消息清空排队中的唤醒并开新对话；
// 就算冷却定时器/收尾泵随后触发，也绝不会把旧对话的队列捞回来。
func TestHubConversationHumanClearsQueue(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: 50 * time.Millisecond, MaxDepth: 99, PairLimit: 99})

	a := createHubAgent(t, m, buf, 1, "人类A")
	b := createHubAgent(t, m, buf, 2, "人类B")
	c := createHubAgent(t, m, buf, 3, "人类C")
	g := convGroupWith(t, m, buf, 10, "人类优先群", nil, a, b, c)
	p3bWireFake(t, m, g, string(a.ID), "@人类B @人类C 你们俩都说说")
	fb := newConvProvider("B 在跑")
	release := fb.gated()
	defer close(release)
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(b.ID), string(g.ID)), g, string(b.ID), fb)
	fc := newConvProvider("C 不该跑")
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(c.ID), string(g.ID)), g, string(c.ID), fc)
	// C 正忙：它的 wake 只能排队 —— 人类插话必须把这条排队记录一起清掉。
	bsC := mustGroupSession(t, m, string(c.ID), string(g.ID))
	bsC.hubGroupBusy.Store(true)
	defer bsC.hubGroupBusy.Store(false)

	// 人类 @A → A 回复同时 @B/@C：B 立刻跑（门闩卡住），C 忙 → 排队。
	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@人类A 你安排一下",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	fb.waitStarted(t)
	convWait(t, "C 的唤醒排队", func() bool { return convView(m, g.ID).QueueLen >= 1 })
	v := convView(m, g.ID)
	firstConvID := v.ID
	if v.TurnsUsed != 2 { // turn1 = A，turn2 = B
		t.Fatalf("turns used = %d, want 2", v.TurnsUsed)
	}

	// 人类插话（不 @ 人）：当前 run 不腰斩，但队列清空、计数归零、开新对话。
	if ok, errMsg, _ := hubCmd(t, m, buf, 12, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "先停一下，等我看完",
	}); !ok {
		t.Fatalf("human interrupt: %s", errMsg)
	}
	v = convView(m, g.ID)
	if v.QueueLen != 0 || v.TurnsUsed != 0 || v.Status != agenthub.ConversationRunning {
		t.Fatalf("人类消息之后对话 = %+v", v)
	}
	if v.ID == firstConvID {
		t.Fatal("人类消息必须开启新对话（id 没变）")
	}
	// 再来一次泵（等价于冷却定时器/上一次 run 收尾泵迟到触发）：旧队列不能复活。
	m.pumpHubConversation(g.ID)
	if n := fc.callCount(); n != 0 {
		t.Fatalf("人类消息之后 C 仍被唤醒 %d 次", n)
	}
}

// TestHubConversationBusyTargetWakeStaysQueued：对话唤醒撞上「目标忙」时不能丢 ——
// wake 留在队首，目标这一轮跑完（hubGroupBusy 放开）后自动补投。
func TestHubConversationBusyTargetWakeStaysQueued(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})

	a := createHubAgent(t, m, buf, 1, "忙唤醒A")
	b := createHubAgent(t, m, buf, 2, "忙唤醒B")
	g := convGroupWith(t, m, buf, 10, "忙唤醒群", nil, a, b)
	p3bWireFake(t, m, g, string(a.ID), "@忙唤醒B 你接着说")
	fb := newConvProvider("B 第一轮", "B 第二轮")
	release := fb.gated()
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(b.ID), string(g.ID)), g, string(b.ID), fb)

	// ① 人类 @B：B 开始跑并卡住（忙）。
	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@忙唤醒B 你先忙",
	}); !ok {
		t.Fatalf("human send #1: %s", errMsg)
	}
	fb.waitStarted(t)

	// ② 人类 @A：A 回复 @B —— 这一条不能丢，只能排队（B 还在跑）。
	if ok, errMsg, _ := hubCmd(t, m, buf, 12, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@忙唤醒A 你去叫一下 B",
	}); !ok {
		t.Fatalf("human send #2: %s", errMsg)
	}
	convWait(t, "A 的队列里压住给 B 的唤醒", func() bool {
		v := convView(m, g.ID)
		return v.QueueLen == 1 && v.TurnsUsed == 1
	})
	if n := fb.callCount(); n != 1 {
		t.Fatalf("B 在忙时被投了第二份快照（调用 %d 次）", n)
	}

	// ③ B 这一轮结束 → 收尾泵把排队的那条投出去。
	close(release)
	convWait(t, "排队的唤醒被补投", func() bool {
		return convView(m, g.ID).TurnsUsed == 2 && fb.callCount() >= 2
	})
	if v := convView(m, g.ID); v.QueueLen != 0 {
		t.Fatalf("补投之后队列 = %d, want 0", v.QueueLen)
	}
}

// TestHubConversationStopInterruptsActiveRun：停止按钮中断当前群 run、清队列、写一条
// 系统消息；重复点击幂等。
func TestHubConversationStopInterruptsActiveRun(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})

	a := createHubAgent(t, m, buf, 1, "停止A")
	b := createHubAgent(t, m, buf, 2, "停止B")
	g := convGroupWith(t, m, buf, 10, "停止群", nil, a, b)
	fa := newConvProvider("@停止B 我先说")
	release := fa.gated()
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(a.ID), string(g.ID)), g, string(a.ID), fa)

	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@停止A 开始跑",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	fa.waitStarted(t)

	ok, errMsg, data := hubCmd(t, m, buf, 12, "hub_conversation_stop", map[string]any{
		"group_id": string(g.ID),
	})
	if !ok {
		t.Fatalf("hub_conversation_stop: %s", errMsg)
	}
	conv, _ := data["conversation"].(map[string]any)
	if conv == nil || conv["status"] != string(agenthub.ConversationStopped) {
		t.Fatalf("停止回执 = %+v", data)
	}
	if n, _ := data["interrupted"].(float64); n < 1 {
		t.Fatalf("interrupted = %v, want >= 1", data["interrupted"])
	}
	// 中断生效：run 真的结束了（provider 在 ctx.Done 上返回）。running 也要放开 ——
	// AgentEnd 在 finishHubReply 之后才把 running 置 false，只等闩会撞上那个窗口。
	convWait(t, "被中断的 run 收尾", func() bool {
		bs, err := m.hubGroupSession(string(a.ID), string(g.ID))
		return err == nil && !bs.hubGroupBusy.Load() && !bs.running.Load()
	})
	msgs := waitConvSystemMessage(t, m, g.ID, "停止了自动对话")
	if got := len(convSystemMessages(msgs)); got != 1 {
		t.Fatalf("系统消息 = %d 条, want 1", got)
	}
	// 幂等：再点一次不再写消息/事件。
	ok, errMsg, data = hubCmd(t, m, buf, 13, "hub_conversation_stop", map[string]any{
		"group_id": string(g.ID),
	})
	if !ok {
		t.Fatalf("第二次 stop: %s", errMsg)
	}
	if n, _ := data["interrupted"].(float64); n != 0 {
		t.Fatalf("第二次 interrupted = %v, want 0", data["interrupted"])
	}
	if got := len(convSystemMessages(convGroupMessages(t, m, g.ID))); got != 1 {
		t.Fatalf("重复停止后系统消息 = %d 条, want 1", got)
	}
	stoppedEvents := 0
	for _, ev := range convConversationEvents(t, buf) {
		if convEventStatus(t, ev) == string(agenthub.ConversationStopped) {
			stoppedEvents++
		}
	}
	if stoppedEvents != 1 {
		t.Fatalf("stopped 事件 = %d 个, want 1", stoppedEvents)
	}
	close(release)
}

// TestHubConversationBusyTargetTaskPump：目标忙时 assign_task 留在账本 queued；
// 目标这一轮跑完（hubGroupBusy 放开）后任务泵把它派发出去。
func TestHubConversationBusyTargetTaskPump(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})

	from := createDispatchAgent(t, m, buf, 1, "派发A", true)
	to := createDispatchAgent(t, m, buf, 2, "派发B", false)
	raw := []map[string]any{
		{"agent_id": string(from.ID), "role": "owner"},
		{"agent_id": string(to.ID), "role": "member"},
	}
	if ok, errMsg, data := hubCmd(t, m, buf, 10, "hub_group_create", map[string]any{
		"name": "忙任务群", "members": raw, "policy": nil,
	}); !ok {
		t.Fatalf("create group: %s", errMsg)
	} else {
		_ = data
	}
	gid := convOnlyGroupID(t, m)
	g := g2Group(t, m, gid)

	ft := newConvProvider("B 说一句不带 @ 的话")
	release := ft.gated()
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(to.ID), string(gid)), g, string(to.ID), ft)

	// 人类 @B：B 开始跑并卡在门闩上（目标忙）。
	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(gid), "text": "@派发B 你先忙",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	ft.waitStarted(t)

	tool := assignTool(t, m, g, from.ID)
	res, err := callAssign(t, tool, map[string]any{
		"to_agent": string(to.ID), "instruction": "把报告整理成一页纸",
	})
	if err != nil {
		t.Fatalf("assign_task: %v", err)
	}
	task := taskOf(t, res)
	if task.Status != agenthub.TaskQueued {
		t.Fatalf("目标忙时任务状态 = %q, want queued（res=%+v）", task.Status, res)
	}
	if dispatched, _ := res["dispatched"].(bool); dispatched {
		t.Fatalf("目标忙时不该显示已派发: %+v", res)
	}
	q, err := m.hubQueueFor()
	if err != nil {
		t.Fatal(err)
	}
	target := agenthub.TaskTarget{Kind: agenthub.TargetAgent, GroupID: gid, AgentID: to.ID}
	if n := q.QueuedCountFor(target); n != 1 {
		t.Fatalf("队列长度 = %d, want 1", n)
	}

	// B 这一轮结束 → 任务泵（hubGroupBusy 已放开）把它派发出去。
	close(release)
	ledger := agenthub.NewLedger(mustHubStore(t, m))
	convWait(t, "任务被泵出队", func() bool {
		latest, rerr := ledger.Replay(context.Background(), task.ID)
		if rerr != nil || latest == nil {
			return false
		}
		if latest.Status == agenthub.TaskQueued {
			return false
		}
		return q.QueuedCountFor(target) == 0
	})
	latest, err := ledger.Replay(context.Background(), task.ID)
	if err != nil || latest == nil {
		t.Fatalf("replay task: %v", err)
	}
	switch latest.Status {
	case agenthub.TaskDispatched, agenthub.TaskRunning, agenthub.TaskCompleted:
	default:
		t.Fatalf("任务终态 = %q, want 已派发/在跑/完成", latest.Status)
	}
	// B 真的被再唤醒了一次（runner 第二次开流）。
	convWait(t, "B 收到派发的任务", func() bool { return ft.callCount() >= 2 })
}

// convOnlyGroupID 取当前唯一群的 id（测试里只建了一个群）。
func convOnlyGroupID(t *testing.T, m *manager) agenthub.GroupID {
	t.Helper()
	groups, err := mustHubStore(t, m).ListGroups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g != nil && !g.Deleted {
			return g.ID
		}
	}
	t.Fatal("没有群")
	return ""
}

func g2Group(t *testing.T, m *manager, gid agenthub.GroupID) *agenthub.Group {
	t.Helper()
	g, err := mustHubStore(t, m).LoadGroup(context.Background(), gid)
	if err != nil || g == nil {
		t.Fatalf("load group %s: %v", gid, err)
	}
	return g
}

// TestHubConversationEventAndWakeDepth：人类消息 = 深度 0 的首轮（唤醒 header 带
// depth=1/turn=1），并广播一个 running 的 hub_conversation 事件。
func TestHubConversationEventAndWakeDepth(t *testing.T) {
	m, buf := testManager(t)
	// 显式注入有限预算：测试注入路径语义不变（生产默认是 UnlimitedTurnBudget）。
	convLimits(m, agenthub.ConversationLimits{TurnBudget: 8, Cooldown: -1})

	a := createHubAgent(t, m, buf, 1, "事件A")
	g := convGroupWith(t, m, buf, 10, "事件群", nil, a)
	fa := newConvProvider("A 看到消息了")
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(a.ID), string(g.ID)), g, string(a.ID), fa)

	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@事件A 看一下",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	fa.waitStarted(t)
	headers := convWakeHeaders(fa.lastRequest())
	if len(headers) != 1 {
		t.Fatalf("唤醒 header = %d, want 1", len(headers))
	}
	if headers[0].Depth != 1 || headers[0].Turn != 1 {
		t.Fatalf("人类首轮 header depth/turn = %d/%d, want 1/1", headers[0].Depth, headers[0].Turn)
	}
	events := convConversationEvents(t, buf)
	if len(events) == 0 {
		t.Fatal("人类消息应当广播 hub_conversation 事件")
	}
	if got := convEventStatus(t, events[0]); got != string(agenthub.ConversationRunning) {
		t.Fatalf("begin 事件 status = %q, want running", got)
	}
	conv, _ := events[0]["conversation"].(map[string]any)
	// 显式注入的有限预算要原样透出（生产默认是 -1，见 hub_group_get 用例）。
	if conv == nil || conv["turn_budget"].(float64) != 8 ||
		conv["max_depth"].(float64) != float64(agenthub.DefaultConversationMaxDepth) {
		t.Fatalf("事件里的限额 = %+v", conv)
	}
}

// TestHubGroupNextDepthFromOldHeader：P3b-04 之前的唤醒 header 没有 depth/turn，
// 解析为 0 → 下一跳按 1 算（不能因为缺字段就断链）。
func TestHubGroupNextDepthFromOldHeader(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "旧头A")
	g := convGroupWith(t, m, buf, 10, "旧头群", nil, a)
	bs := mustGroupSession(t, m, string(a.ID), string(g.ID))

	// 旧块（无 depth/turn）解析出来是 0/0：下一跳 = 1。
	old := `{"group_id":"` + string(g.ID) + `","agent_id":"` + string(a.ID) + `","round_id":"rnd_old","trigger_seq":3}` + "\n\nsnap"
	h, _, err := agenthub.ParseGroupWake(old)
	if err != nil {
		t.Fatalf("旧 header 解析失败: %v", err)
	}
	bs.setHubGroupWake(h)
	if got := m.hubGroupNextDepth(string(a.ID), g); got != 1 {
		t.Fatalf("旧 header 的下一跳深度 = %d, want 1", got)
	}
	bs.setHubGroupWake(&agenthub.GroupWakeHeader{
		GroupID: g.ID, AgentID: a.ID, RoundID: "rnd_new", TriggerSeq: 4, Depth: 3, Turn: 2,
	})
	if got := m.hubGroupNextDepth(string(a.ID), g); got != 4 {
		t.Fatalf("新 header 的下一跳深度 = %d, want 4", got)
	}
	bs.setHubGroupWake(nil)
	if got := m.hubGroupNextDepth(string(a.ID), g); got != 1 {
		t.Fatalf("无 header 的下一跳深度 = %d, want 1", got)
	}
}

func mustGroupSession(t *testing.T, m *manager, agentID string, gid string) *bridgeSession {
	t.Helper()
	bs, err := m.hubGroupSession(agentID, gid)
	if err != nil {
		t.Fatal(err)
	}
	return bs
}
