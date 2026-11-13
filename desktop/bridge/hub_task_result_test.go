package main

// P3b-05：智能体任务（assign_task，target=agent）的结算（§6.5.7 / §6.7.1–§6.7.3 / §6.7.9）。
//
// 覆盖：账本终态 + 群内 task_result 结果卡（幂等）+ 只唤醒派发者（结果卡里出现
// @Name 也不唤醒别人）+ 迟到 AgentEnd 不覆盖 canceled/declined + task_ack 拒绝。
// 全部确定性：fake provider，不碰网络；等待用轮询（5–10ms，5s 超时）。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// p3bGroupSpec 取群会话的装配 spec（finishHubReply 只需要 Scope/Agent）。
func p3bGroupSpec(t *testing.T, m *manager, g *agenthub.Group, agentID agenthub.AgentID) *hubSessionSpec {
	t.Helper()
	p, _, err := m.hubLoadAgent(string(agentID))
	if err != nil || p == nil {
		t.Fatalf("load agent %s: %v", agentID, err)
	}
	ws := m.hubRuntime(string(agentID))
	if ws == nil {
		t.Fatalf("agent %s 没有 hub runtime", agentID)
	}
	spec := m.hubSpecFor(p, agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID}, ws.path)
	spec.Group = g
	return spec
}

// p3bAgentTask 建一条「A 派给 B」的账本任务（默认 dispatched）。
func p3bAgentTask(t *testing.T, m *manager, g *agenthub.Group, origin, target agenthub.AgentID,
	title string, status agenthub.TaskStatus) *agenthub.Task {
	t.Helper()
	st := mustHubStore(t, m)
	ledger := agenthub.NewLedger(st)
	ctx := context.Background()
	task, err := ledger.Create(ctx, &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginGroup, GroupID: g.ID, AgentID: origin, AgentName: string(origin)},
		Target: agenthub.TaskTarget{
			Kind: agenthub.TargetAgent, AgentID: target, GroupID: g.ID,
			SessionID: hubSessionID(string(target), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID}),
		},
		Title: title, Instruction: "把价格表整理成一页", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(ctx, task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if status == agenthub.TaskRunning {
		if _, err := ledger.Transition(ctx, task.ID, agenthub.TaskRunning, agenthub.TransitionOptions{At: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.LoadTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func p3bMessages(t *testing.T, m *manager, gid agenthub.GroupID) []agenthub.Message {
	t.Helper()
	msgs, err := mustHubStore(t, m).ReadMessages(context.Background(), gid, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func p3bTaskResultCards(msgs []agenthub.Message, id agenthub.TaskID) []agenthub.Message {
	out := make([]agenthub.Message, 0, 1)
	for _, m := range msgs {
		if m.Kind == agenthub.KindTaskResult && (id == "" || m.TaskID == id) {
			out = append(out, m)
		}
	}
	return out
}

func p3bKindCount(msgs []agenthub.Message, kind agenthub.MessageKind) int {
	n := 0
	for _, m := range msgs {
		if m.Kind == kind {
			n++
		}
	}
	return n
}

// p3bEventCount 统计某个事件里 task_id 命中的条数（push 次数的确定性锚点：
// task_result_delivered 在 PushTaskResultUsage 内同步发出，不依赖 run 何时消费）。
func p3bEventCount(t *testing.T, buf *syncBuf, eventType string, id agenthub.TaskID) int {
	t.Helper()
	n := 0
	for _, ev := range readAllLines(buf) {
		if ev["event_type"] != eventType {
			continue
		}
		if ev["task_id"] != string(id) {
			continue
		}
		n++
	}
	return n
}

// waitResultNotifyDrained 等异步通知簿把已排队的回推全部处理完。
// scheduleTaskResultNotification 同步 +1，所以调用后 inflight>0 是确定的，
// 等到 0 就说明这次回推已经结束（测试不必猜延迟）。
func waitResultNotifyDrained(t *testing.T, m *manager) {
	t.Helper()
	convWait(t, "任务结果回推排空", func() bool { return m.hubNotify.inflight.Load() == 0 })
}

// p3bTaskResultTexts 取发起者上下文里该任务的 task_result 正文（History 为准；
// 运行还没吸收时退回 Messages）。
func p3bTaskResultTexts(bs *bridgeSession, id agenthub.TaskID) []string {
	if bs == nil || bs.s == nil {
		return nil
	}
	prefix := core.TaskResultMessagePrefix(string(id))
	collect := func(list []core.Message) []string {
		out := make([]string, 0, 1)
		for _, msg := range list {
			for _, c := range msg.Content {
				if c.Type == core.ContentTypeTaskResult && strings.HasPrefix(c.Content, prefix) {
					out = append(out, c.Content)
				}
			}
		}
		return out
	}
	if out := collect(bs.s.History()); len(out) > 0 {
		return out
	}
	return collect(bs.s.Messages())
}

func p3bLoadTask(t *testing.T, m *manager, id agenthub.TaskID) *agenthub.Task {
	t.Helper()
	got, err := mustHubStore(t, m).LoadTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func p3bRefFloat(t *testing.T, refs map[string]any, key string) float64 {
	t.Helper()
	raw, ok := refs[key]
	if !ok {
		t.Fatalf("结果卡 refs 缺 %s: %+v", key, refs)
	}
	f, ok := raw.(float64)
	if !ok {
		t.Fatalf("refs[%s] = %T(%v)，want number", key, raw, raw)
	}
	return f
}

func p3bRefString(t *testing.T, refs map[string]any, key string) string {
	t.Helper()
	raw, ok := refs[key]
	if !ok {
		t.Fatalf("结果卡 refs 缺 %s: %+v", key, refs)
	}
	s, ok := raw.(string)
	if !ok {
		t.Fatalf("refs[%s] = %T(%v)，want string", key, raw, raw)
	}
	return s
}

// TestFinishAgentTaskCompleted：completed → delivered、恰好一张结果卡、只唤醒派发者。
func TestFinishAgentTaskCompleted(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "结算A", true)
	b := createDispatchAgent(t, m, buf, 2, "结算B", false)
	c := createDispatchAgent(t, m, buf, 3, "旁观C", false)
	g := assignGroup(t, m, buf, 4, "结算群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member", c.ID: "observer",
	})
	task := p3bAgentTask(t, m, g, a.ID, b.ID, "价格表简报", agenthub.TaskDispatched)
	if task.Target.GroupID != g.ID || task.Origin.Kind != agenthub.OriginGroup {
		t.Fatalf("任务归属不对: %+v", task)
	}

	// 发起者 A 的群会话用 fake：回推唤醒后只跑一次，不碰网络。
	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))
	fa := newConvProvider("收到结果，我去汇报")
	setGroupFakeLoop(t, m, bsA, g, string(a.ID), fa)
	// 旁观者 C 同样装配（断言它没有被唤醒）。
	bsC := mustGroupSession(t, m, string(c.ID), string(g.ID))
	fc := newConvProvider("我不该被叫醒")
	setGroupFakeLoop(t, m, bsC, g, string(c.ID), fc)

	var usage core.Usage
	usage = usage.Add(core.Usage{Input: 60, Cost: core.Cost{Total: 1800, Priced: true, Source: core.CostSourceBuiltin}})
	usage = usage.Add(core.Usage{Input: 40, Output: 20, Cost: core.Cost{Total: 1200, Priced: true, Source: core.CostSourceBuiltin}})
	summary := "价格表出来了 @旁观C 你也看看（结果卡不能把 C 叫醒）"
	bsB := &bridgeSession{ctx: context.Background(), hubTaskIDs: []agenthub.TaskID{task.ID}}
	m.finishHubTask(bsB, &events.AgentEnd{
		RunId: "run-agent-done", Content: summary, FinishReason: core.FinishReasonStop,
		Usage: &usage, DurationMS: 4321,
	})
	waitResultNotifyDrained(t, m) // 回推异步（P3b-08 B1）

	if len(bsB.hubTaskIDs) != 0 {
		t.Fatal("hubTaskIDs 结算后必须清空")
	}
	got := p3bLoadTask(t, m, task.ID)
	if got.Status != agenthub.TaskDelivered {
		t.Fatalf("status = %s, want delivered（completed 已回推）", got.Status)
	}
	if got.Result == nil || got.Result.DurationMS != 4321 || got.Result.Summary != summary {
		t.Fatalf("result = %+v", got.Result)
	}
	if got.Result.Usage.Input != 100 || got.Result.Usage.Output != 20 || got.Result.Usage.CostUSD != 0.003 {
		t.Fatalf("usage = %+v", got.Result.Usage)
	}

	// 结果卡：恰好一张，字段齐、无 mentions、无路由回执。
	msgs := p3bMessages(t, m, g.ID)
	cards := p3bTaskResultCards(msgs, task.ID)
	if len(cards) != 1 {
		t.Fatalf("task_result 卡 = %d, want 1（msgs=%+v）", len(cards), msgs)
	}
	card := cards[0]
	if card.Sender.Kind != agenthub.SenderAgent || card.Sender.AgentID != b.ID {
		t.Fatalf("card sender = %+v, want agent B", card.Sender)
	}
	if !strings.Contains(card.Text, "价格表出来了") {
		t.Fatalf("card text = %q", card.Text)
	}
	if len(card.Mentions) != 0 || card.MentionAll || card.RoundID != "" || card.ReplyToSeq != 0 {
		t.Fatalf("结果卡不能带 mentions/路由: %+v", card)
	}
	if card.Visibility.EffectiveVisibility() != agenthub.VisibilityGroup || card.CreatedAt.IsZero() {
		t.Fatalf("card visibility/created = %+v", card)
	}
	if card.Refs["client_msg_id"] != "task_result:"+string(task.ID) {
		t.Fatalf("client_msg_id = %v", card.Refs["client_msg_id"])
	}
	if p3bRefString(t, card.Refs, "task_id") != string(task.ID) ||
		p3bRefString(t, card.Refs, "status") != string(agenthub.TaskCompleted) ||
		p3bRefString(t, card.Refs, "agent_id") != string(b.ID) ||
		p3bRefString(t, card.Refs, "title") != "价格表简报" {
		t.Fatalf("refs = %+v", card.Refs)
	}
	if p3bRefFloat(t, card.Refs, "duration_ms") != 4321 {
		t.Fatalf("refs duration_ms = %v", card.Refs["duration_ms"])
	}
	// B5：派发者快照必须在卡里，UI 不能拿 sender（=执行者）猜发起人。
	if p3bRefString(t, card.Refs, "origin_kind") != task.Origin.Kind ||
		p3bRefString(t, card.Refs, "origin_agent_id") != string(task.Origin.AgentID) ||
		p3bRefString(t, card.Refs, "origin_agent_name") != task.Origin.AgentName {
		t.Fatalf("refs origin = %+v, want %+v", card.Refs, task.Origin)
	}
	usageRef, ok := card.Refs["usage"].(map[string]any)
	if !ok {
		t.Fatalf("refs usage = %T", card.Refs["usage"])
	}
	if usageRef["input"] != float64(100) || usageRef["output"] != float64(20) ||
		usageRef["cost_priced"] != true || usageRef["cost_usd"] != 0.003 {
		t.Fatalf("refs usage = %+v", usageRef)
	}
	if _, has := card.Refs["artifacts"]; !has {
		t.Fatalf("refs 缺 artifacts: %+v", card.Refs)
	}
	if _, has := card.Refs["error"]; !has {
		t.Fatalf("refs 缺 error: %+v", card.Refs)
	}

	// 持久层事件：一张 hub_message 卡（前端实时更新）。
	hubMessages := 0
	for _, ev := range readAllLines(buf) {
		if ev["event_type"] != "hub_message" {
			continue
		}
		msg, ok := ev["message"].(map[string]any)
		if !ok || msg["kind"] != string(agenthub.KindTaskResult) || msg["task_id"] != string(task.ID) {
			continue
		}
		hubMessages++
	}
	if hubMessages != 1 {
		t.Fatalf("hub_message(task_result) = %d, want 1", hubMessages)
	}

	// 唤醒层：只有发起者 A 收到一次 task_result。
	if !bsA.s.HasTaskResult(string(task.ID)) {
		t.Fatal("发起者没有收到 task_result")
	}
	convWait(t, "发起者被唤醒并开跑", func() bool { return fa.callCount() >= 1 })
	if n := p3bEventCount(t, buf, string(events.TaskResultDeliveredType), task.ID); n != 1 {
		t.Fatalf("task_result_delivered = %d, want 1", n)
	}
	convWait(t, "发起者上下文里有结果正文", func() bool {
		texts := p3bTaskResultTexts(bsA, task.ID)
		return len(texts) == 1
	})
	if texts := p3bTaskResultTexts(bsA, task.ID); !strings.Contains(texts[0], "价格表出来了") {
		t.Fatalf("结果正文 = %q", texts[0])
	}

	// 其它成员：0 次 provider 调用、没有 task_result（结果卡里的 @ 不路由）。
	if n := fc.callCount(); n != 0 {
		t.Fatalf("旁观者被叫醒 %d 次（结果卡不能路由 @）", n)
	}
	if bsC.s.HasTaskResult(string(task.ID)) {
		t.Fatal("旁观者收到 task_result：只应唤醒派发者")
	}
}

// TestFinishAgentTaskViaGroupRun：真实链路（assign_task → 群会话 run → emit 结算）。
// 锁住 emit 的结算接线：hub agent 会话的根 AgentEnd 也必须结算 agent 目标任务。
func TestFinishAgentTaskViaGroupRun(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "端到端A", true)
	b := createDispatchAgent(t, m, buf, 2, "端到端B", false)
	g := assignGroup(t, m, buf, 3, "端到端群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member",
	})
	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))
	fa := newConvProvider("收到端到端结果")
	setGroupFakeLoop(t, m, bsA, g, string(a.ID), fa)
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	fb := newConvProvider("端到端做完了：价格表在 out/pricing.md")
	setGroupFakeLoop(t, m, bsB, g, string(b.ID), fb)

	res, err := callAssign(t, assignTool(t, m, g, a.ID), map[string]any{
		"to_agent": string(b.ID), "instruction": "整理价格表",
	})
	if err != nil {
		t.Fatalf("assign_task: %v", err)
	}
	task := taskOf(t, res)
	ledger := agenthub.NewLedger(mustHubStore(t, m))
	convWait(t, "群内任务被 AgentEnd 结算", func() bool {
		latest, rerr := ledger.Replay(context.Background(), task.ID)
		return rerr == nil && latest != nil && latest.Status == agenthub.TaskDelivered
	})
	convWait(t, "群里有结果卡", func() bool {
		return len(p3bTaskResultCards(p3bMessages(t, m, g.ID), task.ID)) == 1
	})
	card := p3bTaskResultCards(p3bMessages(t, m, g.ID), task.ID)[0]
	if !strings.Contains(card.Text, "out/pricing.md") || card.Sender.AgentID != b.ID {
		t.Fatalf("结果卡 = %+v", card)
	}
	convWait(t, "发起者收到结果", func() bool { return bsA.s.HasTaskResult(string(task.ID)) })
	// B 的最终正文不进群（结果卡就是产出）：B 的普通气泡必须是 0 条。
	textFromB := 0
	for _, msg := range p3bMessages(t, m, g.ID) {
		if msg.Kind == agenthub.KindText && msg.Sender.AgentID == b.ID {
			textFromB++
		}
	}
	if textFromB != 0 {
		t.Fatalf("B 的最终正文写了 %d 条群气泡, want 0", textFromB)
	}
	if fb.callCount() < 1 || fa.callCount() < 1 {
		t.Fatalf("provider calls: B=%d A=%d（B 必须跑过，A 必须被唤醒）", fb.callCount(), fa.callCount())
	}
}

// TestAgentTaskCancelDuringSettlementKeepsResult：根 AgentEnd 已到、结算未完成时，
// 取消不能把「已经在回推」的智能体任务抢成 canceled（与项目会话同一把 settle latch）。
func TestAgentTaskCancelDuringSettlementKeepsResult(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "结算窗A", true)
	b := createDispatchAgent(t, m, buf, 2, "结算窗B", false)
	g := assignGroup(t, m, buf, 3, "结算窗群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member",
	})
	task := p3bAgentTask(t, m, g, a.ID, b.ID, "结算窗口里的任务", agenthub.TaskDispatched)
	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))
	setGroupFakeLoop(t, m, bsA, g, string(a.ID), newConvProvider("收到"))
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	bsB.bindHubTasks([]agenthub.TaskID{task.ID})
	bsB.running.Store(true)

	// 占住结算锁：emit 会先清 running（暴露结算窗口），再在 finishHubTask 处等待。
	m.hubFinishMu.Lock()
	emitDone := make(chan struct{})
	go func() {
		bsB.emit(&events.AgentEnd{
			EventType: events.AgentEndType, RunId: "run-settle-agent", Content: "结算窗口里跑完了",
			FinishReason: core.FinishReasonStop,
		})
		close(emitDone)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for bsB.running.Load() {
		if time.Now().After(deadline) {
			m.hubFinishMu.Unlock()
			t.Fatal("emit 未在 3s 内清掉 running")
		}
		time.Sleep(2 * time.Millisecond)
	}
	canceled, note, err := m.cancelHubTask(context.Background(), task.ID)
	if err != nil {
		m.hubFinishMu.Unlock()
		t.Fatalf("cancel: %v", err)
	}
	if canceled.Status == agenthub.TaskCanceled {
		m.hubFinishMu.Unlock()
		t.Fatal("结算窗口里的任务被抢成 canceled（结果丢失）")
	}
	if !strings.Contains(note, "正在结算") {
		m.hubFinishMu.Unlock()
		t.Fatalf("cancel note = %q, want 「正在结算」", note)
	}
	m.hubFinishMu.Unlock()
	select {
	case <-emitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("emit 没有在结算后返回")
	}
	waitResultNotifyDrained(t, m)
	if got := p3bLoadTask(t, m, task.ID); got.Status != agenthub.TaskDelivered ||
		got.Result == nil || !strings.Contains(got.Result.Summary, "结算窗口") {
		t.Fatalf("结算结果 = %s %+v", got.Status, got.Result)
	}
	if n := len(p3bTaskResultCards(p3bMessages(t, m, g.ID), task.ID)); n != 1 {
		t.Fatalf("结果卡 = %d, want 1", n)
	}
	if !bsA.s.HasTaskResult(string(task.ID)) {
		t.Fatal("结算窗口任务没有回推派发者")
	}
}

// TestFinishAgentTaskFailed：失败终态 + 重复 AgentEnd 幂等 + 摘要 @ 不唤醒别人。
func TestFinishAgentTaskFailed(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "失败A", true)
	b := createDispatchAgent(t, m, buf, 2, "失败B", false)
	c := createDispatchAgent(t, m, buf, 3, "失败C", false)
	g := assignGroup(t, m, buf, 4, "失败群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member", c.ID: "observer",
	})
	task := p3bAgentTask(t, m, g, a.ID, b.ID, "会失败的任务", agenthub.TaskDispatched)

	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))
	fa := newConvProvider("失败也要汇报")
	setGroupFakeLoop(t, m, bsA, g, string(a.ID), fa)
	bsC := mustGroupSession(t, m, string(c.ID), string(g.ID))
	fc := newConvProvider("我不该被叫醒")
	setGroupFakeLoop(t, m, bsC, g, string(c.ID), fc)

	bsB := &bridgeSession{ctx: context.Background(), hubTaskIDs: []agenthub.TaskID{task.ID}}
	ae := &events.AgentEnd{
		RunId: "run-agent-fail", Content: "做到一半 @失败C 你来接手",
		FinishReason: core.FinishReasonError, Error: "上游 500",
	}
	m.finishHubTask(bsB, ae)
	waitResultNotifyDrained(t, m)
	got := p3bLoadTask(t, m, task.ID)
	if got.Status != agenthub.TaskDelivered {
		t.Fatalf("status = %s, want delivered（failed 已回推）", got.Status)
	}
	if got.Result == nil || got.Result.Error != "上游 500" || got.Error != "上游 500" {
		t.Fatalf("failed result = %+v error=%q", got.Result, got.Error)
	}
	cards := p3bTaskResultCards(p3bMessages(t, m, g.ID), task.ID)
	if len(cards) != 1 || p3bRefString(t, cards[0].Refs, "status") != string(agenthub.TaskFailed) ||
		p3bRefString(t, cards[0].Refs, "error") != "上游 500" {
		t.Fatalf("failed cards = %+v", cards)
	}
	if !bsA.s.HasTaskResult(string(task.ID)) {
		t.Fatal("失败结果没有回推给发起者")
	}
	convWait(t, "发起者收到失败结果", func() bool { return fa.callCount() >= 1 })
	convWait(t, "失败结果进入发起者上下文", func() bool {
		texts := p3bTaskResultTexts(bsA, task.ID)
		return len(texts) == 1 && strings.Contains(texts[0], "上游 500")
	})

	// 重复 AgentEnd（同 run）：不再写卡、不再回推。
	m.finishHubTask(&bridgeSession{ctx: context.Background(), hubTaskIDs: []agenthub.TaskID{task.ID}}, ae)
	if n := len(p3bTaskResultCards(p3bMessages(t, m, g.ID), task.ID)); n != 1 {
		t.Fatalf("重复 AgentEnd 写了 %d 张卡, want 1", n)
	}
	if n := p3bEventCount(t, buf, string(events.TaskResultDeliveredType), task.ID); n != 1 {
		t.Fatalf("重复 AgentEnd 回推 %d 次, want 1", n)
	}
	// 回推成功但 MarkNotified 失败后的重放（账本未记录已通知）：通知入口必须靠
	// 卡片的 client_msg_id 与接收端 HasTaskResult 保持「一张卡、一次回推」。
	m.notifyAgentTaskResult(p3bLoadTask(t, m, task.ID))
	waitResultNotifyDrained(t, m)
	if n := len(p3bTaskResultCards(p3bMessages(t, m, g.ID), task.ID)); n != 1 {
		t.Fatalf("重放通知写了 %d 张卡, want 1（client_msg_id 幂等失效）", n)
	}
	if n := p3bEventCount(t, buf, string(events.TaskResultDeliveredType), task.ID); n != 1 {
		t.Fatalf("重放通知回推 %d 次, want 1（接收端幂等失效）", n)
	}
	if n := fc.callCount(); n != 0 {
		t.Fatalf("失败摘要里的 @ 把旁观者叫醒 %d 次", n)
	}
	if bsC.s.HasTaskResult(string(task.ID)) {
		t.Fatal("旁观者收到 task_result：只应唤醒派发者")
	}
}

// TestFinishAgentTaskNoFinalBubble：任务批次不写普通群气泡（结果卡就是产出）。
func TestFinishAgentTaskNoFinalBubble(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "气泡A", true)
	b := createDispatchAgent(t, m, buf, 2, "气泡B", false)
	g := assignGroup(t, m, buf, 3, "气泡群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member",
	})
	task := p3bAgentTask(t, m, g, a.ID, b.ID, "不要两个气泡", agenthub.TaskDispatched)

	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))
	fa := newConvProvider("收到")
	setGroupFakeLoop(t, m, bsA, g, string(a.ID), fa)

	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	spec := p3bGroupSpec(t, m, g, b.ID)
	// 模拟「这一轮既绑了群唤醒、又绑了 hub_task」（防御性混合批）：最终正文不能
	// 再写一条普通气泡，否则同一条回复会出现两次。
	bsB.setHubGroupWake(&agenthub.GroupWakeHeader{
		GroupID: g.ID, AgentID: b.ID, RoundID: "rnd_bubble", TriggerSeq: 0,
	})
	bsB.hubGroupBusy.Store(true)
	bsB.hubGroupOwner.Store(1)
	bsB.bindHubTasks([]agenthub.TaskID{task.ID})
	m.finishHubReply(bsB, spec, &events.AgentEnd{
		RunId: "run-agent-bubble", Content: "最终正文不该变成群气泡", FinishReason: core.FinishReasonStop,
	}, nil)
	if bsB.hubGroupBusy.Load() || bsB.hubGroupOwner.Load() != 0 || bsB.hubGroupRunSeq.Load() != 0 {
		t.Fatalf("群轮闩没清干净: busy=%v owner=%d run=%d",
			bsB.hubGroupBusy.Load(), bsB.hubGroupOwner.Load(), bsB.hubGroupRunSeq.Load())
	}
	if n := p3bKindCount(p3bMessages(t, m, g.ID), agenthub.KindText); n != 0 {
		t.Fatalf("任务批次写了 %d 条普通气泡, want 0", n)
	}

	m.finishHubTask(bsB, &events.AgentEnd{
		RunId: "run-agent-bubble", Content: "最终正文不该变成群气泡", FinishReason: core.FinishReasonStop,
	})
	msgs := p3bMessages(t, m, g.ID)
	if n := p3bKindCount(msgs, agenthub.KindText); n != 0 {
		t.Fatalf("结算后普通气泡 = %d, want 0（结果卡就是产出）", n)
	}
	if n := len(p3bTaskResultCards(msgs, task.ID)); n != 1 {
		t.Fatalf("结果卡 = %d, want 1", n)
	}
	convWait(t, "发起者收到结果", func() bool { return fa.callCount() >= 1 })
}

// TestAgentTaskCanceledNotifiesOrigin：取消（未开始/空闲兜底）通知派发者一次；
// 迟到的 AgentEnd 不能覆盖 canceled。
func TestAgentTaskCanceledNotifiesOrigin(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "取消A", true)
	b := createDispatchAgent(t, m, buf, 2, "取消B", false)
	g := assignGroup(t, m, buf, 3, "取消群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member",
	})
	task := p3bAgentTask(t, m, g, a.ID, b.ID, "会被取消", agenthub.TaskDispatched)

	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))
	fa := newConvProvider("收到取消通知")
	setGroupFakeLoop(t, m, bsA, g, string(a.ID), fa)

	// 目标会话没装配 → 空闲兜底直接 canceled（不是中断）。
	canceled, note, err := m.cancelHubTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitResultNotifyDrained(t, m)
	if canceled.Status != agenthub.TaskCanceled || !strings.Contains(note, "已取消") {
		t.Fatalf("canceled = %s note=%q", canceled.Status, note)
	}
	if strings.TrimSpace(canceled.Error) == "" {
		t.Fatal("取消任务必须留下原因")
	}
	cards := p3bTaskResultCards(p3bMessages(t, m, g.ID), task.ID)
	if len(cards) != 1 || p3bRefString(t, cards[0].Refs, "status") != string(agenthub.TaskCanceled) {
		t.Fatalf("cancel cards = %+v", cards)
	}
	if !strings.Contains(cards[0].Text, "取消") {
		t.Fatalf("取消卡正文 = %q（应说明取消原因）", cards[0].Text)
	}
	if !bsA.s.HasTaskResult(string(task.ID)) {
		t.Fatal("取消没有通知派发者")
	}
	if n := p3bEventCount(t, buf, string(events.TaskResultDeliveredType), task.ID); n != 1 {
		t.Fatalf("取消回推 %d 次, want 1", n)
	}
	convWait(t, "派发者收到取消通知", func() bool {
		texts := p3bTaskResultTexts(bsA, task.ID)
		return len(texts) == 1 && strings.Contains(texts[0], "取消")
	})

	// 再取消一次：终态幂等，不再写卡/回推。
	if again, note2, err := m.cancelHubTask(context.Background(), task.ID); err != nil || again.Status != agenthub.TaskCanceled ||
		!strings.Contains(note2, "无需取消") {
		t.Fatalf("repeat cancel = %v / %q / %v", again, note2, err)
	}
	if n := len(p3bTaskResultCards(p3bMessages(t, m, g.ID), task.ID)); n != 1 {
		t.Fatalf("重复取消写了 %d 张卡, want 1", n)
	}
	if n := p3bEventCount(t, buf, string(events.TaskResultDeliveredType), task.ID); n != 1 {
		t.Fatalf("重复取消回推 %d 次, want 1", n)
	}

	// 迟到的 AgentEnd：不覆盖 canceled、不写第二张卡、不二次回推。
	buf.Take()
	m.finishHubTask(&bridgeSession{ctx: context.Background(), hubTaskIDs: []agenthub.TaskID{task.ID}},
		&events.AgentEnd{RunId: "run-agent-late", Content: "其实我跑完了", FinishReason: core.FinishReasonStop})
	if got := p3bLoadTask(t, m, task.ID); got.Status != agenthub.TaskCanceled || got.Result != nil {
		t.Fatalf("迟到 AgentEnd 覆盖了取消: %s %+v", got.Status, got.Result)
	}
	if n := len(p3bTaskResultCards(p3bMessages(t, m, g.ID), task.ID)); n != 1 {
		t.Fatalf("迟到 AgentEnd 写了 %d 张卡, want 1", n)
	}
	if n := p3bEventCount(t, buf, string(events.TaskResultDeliveredType), task.ID); n != 0 {
		t.Fatalf("迟到 AgentEnd 二次回推 %d 次, want 0", n)
	}
	if n := p3bEventCount(t, buf, "hub_alert", task.ID); n != 0 {
		t.Fatalf("迟到 AgentEnd 制造了 %d 条任务告警（应安静返回）", n)
	}
}

// TestAgentTaskDeclinedAck：task_ack 拒绝（一次、通知派发者、续跑/错误分支）。
func TestAgentTaskDeclinedAck(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "拒绝A", true)
	b := createDispatchAgent(t, m, buf, 2, "拒绝B", false)
	c := createDispatchAgent(t, m, buf, 3, "拒绝C", false)
	g := assignGroup(t, m, buf, 4, "拒绝群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member", c.ID: "observer",
	})
	origin := p3bAgentTask(t, m, g, a.ID, b.ID, "要拒绝的任务", agenthub.TaskDispatched)
	acceptTask := p3bAgentTask(t, m, g, a.ID, b.ID, "要接受的任务", agenthub.TaskDispatched)
	runningTask := p3bAgentTask(t, m, g, a.ID, b.ID, "已经在跑的任务", agenthub.TaskRunning)
	foreignTask := p3bAgentTask(t, m, g, a.ID, c.ID, "派给 C 的任务", agenthub.TaskDispatched)

	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))
	fa := newConvProvider("收到拒绝通知")
	setGroupFakeLoop(t, m, bsA, g, string(a.ID), fa)
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	bsC := mustGroupSession(t, m, string(c.ID), string(g.ID))

	ack := func(bs *bridgeSession, args map[string]any) (string, error) {
		t.Helper()
		tool, err := bs.hubEngine.GetTool(context.Background(), "task_ack")
		if err != nil {
			t.Fatalf("task_ack 未注册: %v", err)
		}
		raw, _ := jsonMarshal(t, args)
		return tool.Call(context.Background(), "task_ack", raw)
	}
	if _, err := bsA.hubEngine.GetTool(context.Background(), "task_ack"); err != nil {
		t.Fatalf("群会话缺 task_ack 工具: %v", err)
	}

	// 不是任务运行：没有绑定 → 拒绝。
	if _, err := ack(bsC, map[string]any{"group_id": string(g.ID), "accept": false}); err == nil {
		t.Fatal("非任务运行的 task_ack 必须失败")
	}
	// 只有目标智能体能 ack：把派给 C 的任务绑到 B 上，B 也不能拒绝。
	bsB.bindHubTasks([]agenthub.TaskID{foreignTask.ID})
	if _, err := ack(bsB, map[string]any{"group_id": string(g.ID), "accept": false}); err == nil ||
		!errors.Is(err, agenthub.ErrInvalid) {
		t.Fatalf("非目标任务 ack err = %v, want ErrInvalid", err)
	}
	// 已经开始跑的任务不能拒绝。
	bsB.bindHubTasks([]agenthub.TaskID{runningTask.ID})
	if _, err := ack(bsB, map[string]any{"group_id": string(g.ID), "accept": false}); err == nil ||
		!strings.Contains(err.Error(), "已开始") {
		t.Fatalf("running 任务 decline err = %v, want 「已开始」", err)
	}
	if got := p3bLoadTask(t, m, runningTask.ID); got.Status != agenthub.TaskRunning {
		t.Fatalf("running 任务被拒绝改写: %s", got.Status)
	}
	// 别的群 id 不能被当成当前群。
	bsB.bindHubTasks([]agenthub.TaskID{origin.ID})
	if _, err := ack(bsB, map[string]any{"group_id": "grp_other", "accept": false}); err == nil {
		t.Fatal("group_id 不匹配必须失败")
	}
	// accept=true 是纯确认：状态不变、不写卡。
	if _, err := ack(bsB, map[string]any{"group_id": string(g.ID), "accept": true}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if got := p3bLoadTask(t, m, origin.ID); got.Status != agenthub.TaskDispatched {
		t.Fatalf("accept=true 改了状态: %s", got.Status)
	}
	if n := len(p3bTaskResultCards(p3bMessages(t, m, g.ID), origin.ID)); n != 0 {
		t.Fatalf("accept=true 写了 %d 张卡, want 0", n)
	}

	// 拒绝：dispatched → declined + 结果卡 + 只回推派发者，各一次。
	bsB.bindHubTasks([]agenthub.TaskID{origin.ID, acceptTask.ID})
	if _, err := ack(bsB, map[string]any{
		"group_id": string(g.ID), "accept": false, "reason": "这块我不熟，建议换人",
	}); err != nil {
		t.Fatalf("decline: %v", err)
	}
	waitResultNotifyDrained(t, m)
	got := p3bLoadTask(t, m, origin.ID)
	if got.Status != agenthub.TaskDeclined || !strings.Contains(got.Error, "这块我不熟") {
		t.Fatalf("declined = %s error=%q", got.Status, got.Error)
	}
	if got := p3bLoadTask(t, m, acceptTask.ID); got.Status != agenthub.TaskDispatched {
		t.Fatalf("非绑定的第二个任务被误改: %s", got.Status)
	}
	cards := p3bTaskResultCards(p3bMessages(t, m, g.ID), origin.ID)
	if len(cards) != 1 || p3bRefString(t, cards[0].Refs, "status") != string(agenthub.TaskDeclined) ||
		p3bRefString(t, cards[0].Refs, "error") != got.Error {
		t.Fatalf("declined cards = %+v", cards)
	}
	if !strings.Contains(cards[0].Text, "这块我不熟") {
		t.Fatalf("拒绝卡正文 = %q", cards[0].Text)
	}
	if !bsA.s.HasTaskResult(string(origin.ID)) {
		t.Fatal("拒绝没有通知派发者")
	}
	if n := p3bEventCount(t, buf, string(events.TaskResultDeliveredType), origin.ID); n != 1 {
		t.Fatalf("拒绝回推 %d 次, want 1", n)
	}
	convWait(t, "派发者收到拒绝", func() bool {
		texts := p3bTaskResultTexts(bsA, origin.ID)
		return len(texts) == 1 && strings.Contains(texts[0], "拒绝")
	})

	// 再拒绝一次：仍只有一张卡、一次回推。
	if _, err := ack(bsB, map[string]any{"group_id": string(g.ID), "accept": false, "reason": "再说一次"}); err != nil {
		t.Fatalf("repeat decline: %v", err)
	}
	if n := len(p3bTaskResultCards(p3bMessages(t, m, g.ID), origin.ID)); n != 1 {
		t.Fatalf("重复拒绝写了 %d 张卡, want 1", n)
	}
	if n := p3bEventCount(t, buf, string(events.TaskResultDeliveredType), origin.ID); n != 1 {
		t.Fatalf("重复拒绝回推 %d 次, want 1", n)
	}

	// 迟到的 AgentEnd 不能覆盖 declined。
	buf.Take()
	m.finishHubTask(&bridgeSession{ctx: context.Background(), hubTaskIDs: []agenthub.TaskID{origin.ID}},
		&events.AgentEnd{RunId: "run-agent-declined", Content: "我反悔了做完了", FinishReason: core.FinishReasonStop})
	if got := p3bLoadTask(t, m, origin.ID); got.Status != agenthub.TaskDeclined || got.Result != nil {
		t.Fatalf("迟到 AgentEnd 覆盖了 declined: %s %+v", got.Status, got.Result)
	}
	if n := len(p3bTaskResultCards(p3bMessages(t, m, g.ID), origin.ID)); n != 1 {
		t.Fatalf("迟到 AgentEnd 写了 %d 张卡, want 1", n)
	}
	if n := p3bEventCount(t, buf, string(events.TaskResultDeliveredType), origin.ID); n != 0 {
		t.Fatalf("迟到 AgentEnd 二次回推 %d 次, want 0", n)
	}
	if n := p3bEventCount(t, buf, "hub_alert", origin.ID); n != 0 {
		t.Fatalf("迟到 AgentEnd 制造了 %d 条告警（应安静返回）", n)
	}
}

func jsonMarshal(t *testing.T, v any) (string, error) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), nil
}

// p3bQueuedAgentTask 建一条**还没派发**的 agent 目标任务（走 Queue.SubmitAgent，
// 与 assign_task 同一建账路径，只是不写消息/不泵）。
func p3bQueuedAgentTask(t *testing.T, m *manager, g *agenthub.Group, origin, target agenthub.AgentID,
	title, clientMsgID string) *agenthub.Task {
	t.Helper()
	q, err := m.hubQueueFor()
	if err != nil {
		t.Fatal(err)
	}
	task, err := q.SubmitAgent(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginGroup, GroupID: g.ID, AgentID: origin, AgentName: string(origin)},
		Target: agenthub.TaskTarget{
			Kind: agenthub.TargetAgent, AgentID: target, GroupID: g.ID,
			SessionID: hubSessionID(string(target), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID}),
		},
		Title: title, Instruction: "把价格表整理成一页", Callback: agenthub.CallbackAgentEnd,
		ClientMsgID: clientMsgID,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != agenthub.TaskQueued {
		t.Fatalf("新任务 status = %s, want queued", task.Status)
	}
	return task
}

func p3bTaskTarget(g *agenthub.Group, target agenthub.AgentID) agenthub.TaskTarget {
	return agenthub.TaskTarget{
		Kind: agenthub.TargetAgent, AgentID: target, GroupID: g.ID,
		SessionID: hubSessionID(string(target), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID}),
	}
}

// p3bHasTaskUpdate 输出里是否有一条该任务、该状态的 hub_task_update。
func p3bHasTaskUpdate(buf *syncBuf, id agenthub.TaskID, status agenthub.TaskStatus) bool {
	for _, ev := range readAllLines(buf) {
		if ev["event_type"] == "hub_task_update" && ev["task_id"] == string(id) && ev["status"] == string(status) {
			return true
		}
	}
	return false
}

// TestAgentTaskPumpTerminalNotifiesOrigin（P3b-05 复审 B-1）：泵把任务落成 canceled
// 也必须发 hub_task_update + 结果卡 + 回推派发者，且重复泵不会双写。
func TestAgentTaskPumpTerminalNotifiesOrigin(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "泵终态A", true)
	b := createDispatchAgent(t, m, buf, 2, "泵终态B", false)
	g := assignGroup(t, m, buf, 3, "泵终态群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member",
	})
	task := p3bQueuedAgentTask(t, m, g, a.ID, b.ID, "会被泵成 canceled", "")

	// 目标被移出群：AgentDispatch 必然永久失败（ErrInvalid），泵要把任务落 canceled。
	if ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_group_member_remove", map[string]any{
		"group_id": string(g.ID), "agent_id": string(b.ID),
	}); !ok {
		t.Fatalf("member_remove: %s", errMsg)
	}
	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))
	fa := newConvProvider("收到泵终态通知")
	setGroupFakeLoop(t, m, bsA, g, string(a.ID), fa)

	buf.Take()
	got, perr := m.pumpAgentTask(context.Background(), p3bTaskTarget(g, b.ID))
	waitResultNotifyDrained(t, m)
	if perr == nil {
		t.Fatal("成员被移出后泵必须把「不可派发」如实返回")
	}
	if got == nil || got.Status != agenthub.TaskCanceled {
		t.Fatalf("pump 返回 = %+v, want canceled 快照", got)
	}
	latest := p3bLoadTask(t, m, task.ID)
	if latest.Status != agenthub.TaskCanceled || !strings.Contains(latest.Error, "目标不可派发") {
		t.Fatalf("账本 = %s error=%q", latest.Status, latest.Error)
	}
	// 任务板：必须有终态快照（以前被丢掉）。
	if !p3bHasTaskUpdate(buf, task.ID, agenthub.TaskCanceled) {
		t.Fatalf("缺 hub_task_update(canceled)：%s", buf.String())
	}
	// 结果卡 + 回推：都只应有 1 次。
	cards := p3bTaskResultCards(p3bMessages(t, m, g.ID), task.ID)
	if len(cards) != 1 || p3bRefString(t, cards[0].Refs, "status") != string(agenthub.TaskCanceled) ||
		p3bRefString(t, cards[0].Refs, "error") != latest.Error {
		t.Fatalf("泵终态结果卡 = %+v", cards)
	}
	if !bsA.s.HasTaskResult(string(task.ID)) {
		t.Fatal("泵出来的 canceled 没有回推派发者")
	}
	convWait(t, "派发者收到泵终态", func() bool {
		texts := p3bTaskResultTexts(bsA, task.ID)
		return len(texts) == 1 && strings.Contains(texts[0], "取消")
	})
	if n := p3bEventCount(t, buf, string(events.TaskResultDeliveredType), task.ID); n != 1 {
		t.Fatalf("泵终态回推 %d 次, want 1", n)
	}

	// 重复泵：队列已空，不能再写第二张卡 / 二次回推。
	if again, aerr := m.pumpAgentTask(context.Background(), p3bTaskTarget(g, b.ID)); aerr != nil || again != nil {
		t.Fatalf("重复泵 = %+v, %v; want nil, nil", again, aerr)
	}
	if n := len(p3bTaskResultCards(p3bMessages(t, m, g.ID), task.ID)); n != 1 {
		t.Fatalf("重复泵写了 %d 张卡, want 1", n)
	}
	if n := p3bEventCount(t, buf, string(events.TaskResultDeliveredType), task.ID); n != 1 {
		t.Fatalf("重复泵回推 %d 次, want 1", n)
	}
}

// TestAgentTaskPumpDuplicateNotifiesOrigin（P3b-05 复审 B-1）：目标侧 client_msg_id
// 去重命中 → Pump 落 canceled（无 error），结果卡/回推同样不能丢。
func TestAgentTaskPumpDuplicateNotifiesOrigin(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "去重A", true)
	b := createDispatchAgent(t, m, buf, 2, "去重B", false)
	g := assignGroup(t, m, buf, 3, "去重群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member",
	})
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))
	fa := newConvProvider("收到去重通知")
	setGroupFakeLoop(t, m, bsA, g, string(a.ID), fa)

	// 目标侧幂等表命中：AgentDispatch 返回 duplicate（本次没投递），泵应落 canceled。
	const cmid = "p3b05-dup-cmid"
	m.mu.Lock()
	if m.hubMsgDedup == nil {
		m.hubMsgDedup = map[string]string{}
	}
	m.hubMsgDedup["agent\x00"+bsB.id+"\x00"+cmid] = bsB.id
	m.mu.Unlock()

	task := p3bQueuedAgentTask(t, m, g, a.ID, b.ID, "去重命中的任务", cmid)
	buf.Take()
	got, perr := m.pumpAgentTask(context.Background(), p3bTaskTarget(g, b.ID))
	waitResultNotifyDrained(t, m)
	if perr != nil {
		t.Fatalf("去重命中不是派发错误: %v", perr)
	}
	if got == nil || got.Status != agenthub.TaskCanceled || !strings.Contains(got.Error, "去重") {
		t.Fatalf("pump 返回 = %+v, want canceled(去重)", got)
	}
	if !p3bHasTaskUpdate(buf, task.ID, agenthub.TaskCanceled) {
		t.Fatalf("缺 hub_task_update(canceled)：%s", buf.String())
	}
	if n := len(p3bTaskResultCards(p3bMessages(t, m, g.ID), task.ID)); n != 1 {
		t.Fatalf("去重取消写了 %d 张卡, want 1", n)
	}
	if !bsA.s.HasTaskResult(string(task.ID)) {
		t.Fatal("去重取消没有回推派发者")
	}
	convWait(t, "派发者收到去重取消", func() bool {
		texts := p3bTaskResultTexts(bsA, task.ID)
		return len(texts) == 1 && strings.Contains(texts[0], "取消")
	})
	if again, aerr := m.pumpAgentTask(context.Background(), p3bTaskTarget(g, b.ID)); aerr != nil || again != nil {
		t.Fatalf("重复泵 = %+v, %v; want nil, nil", again, aerr)
	}
	if n := len(p3bTaskResultCards(p3bMessages(t, m, g.ID), task.ID)); n != 1 {
		t.Fatalf("重复泵写了 %d 张卡, want 1", n)
	}
	if n := p3bEventCount(t, buf, string(events.TaskResultDeliveredType), task.ID); n != 1 {
		t.Fatalf("重复泵回推 %d 次, want 1", n)
	}
}

// TestAssignTaskPumpCanceledNote（P3b-05 复审 B-1）：同步泵把任务落 canceled 时，
// 工具响应必须说「未派发、已终止」，不能再说「已入队」。
func TestAssignTaskPumpCanceledNote(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "指派取消A", true)
	b := createDispatchAgent(t, m, buf, 2, "指派取消B", false)
	g := assignGroup(t, m, buf, 3, "指派取消群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member",
	})
	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))
	fa := newConvProvider("收到指派取消")
	setGroupFakeLoop(t, m, bsA, g, string(a.ID), fa)

	// 任务已建、群消息写入之前把目标移出群：路由用的是 assignHubTask 已加载的群
	// 快照（所以意图消息仍写得进去），而同步泵会重新读群 —— AgentDispatch 必然
	// 永久失败（ErrInvalid: 不是群成员），任务落 canceled 而不是留队。
	prev := hubAssignMessageHook
	hubAssignMessageHook = func(agenthub.TaskID) error {
		ctx := context.Background()
		st, err := m.hubStore()
		if err != nil {
			return err
		}
		cur, err := st.LoadGroup(ctx, g.ID)
		if err != nil {
			return err
		}
		next := *cur
		next.Members = make([]agenthub.Member, 0, len(cur.Members))
		for _, mem := range cur.Members {
			if mem.AgentID != b.ID {
				next.Members = append(next.Members, mem)
			}
		}
		next.UpdatedAt = time.Now().UTC()
		if err := next.Normalize(); err != nil {
			return err
		}
		return st.SaveGroup(ctx, &next)
	}
	t.Cleanup(func() { hubAssignMessageHook = prev })

	res, err := callAssign(t, assignTool(t, m, g, a.ID), map[string]any{
		"to_agent": string(b.ID), "instruction": "把价格表整理成一页",
	})
	if err != nil {
		t.Fatalf("assign_task: %v", err)
	}
	task := taskOf(t, res)
	if task.Status != agenthub.TaskCanceled {
		t.Fatalf("status = %s, want canceled（目标已被移出群）", task.Status)
	}
	if dispatched, _ := res["dispatched"].(bool); dispatched {
		t.Fatalf("canceled 任务不能报 dispatched=true: %+v", res)
	}
	note, _ := res["note"].(string)
	if strings.Contains(note, "已入队") || strings.Contains(note, "queued") {
		t.Fatalf("note = %q，不能再说「已入队」（任务已终止）", note)
	}
	if !strings.Contains(note, "未能派发") || !strings.Contains(note, "已终止") ||
		!strings.Contains(note, string(agenthub.TaskCanceled)) {
		t.Fatalf("note = %q，应说明未派发/已终止/状态", note)
	}
	if task.Error == "" || !strings.Contains(note, task.Error) {
		t.Fatalf("note = %q 缺 task.Error=%q", note, task.Error)
	}
	cards := p3bTaskResultCards(p3bMessages(t, m, g.ID), task.ID)
	if len(cards) != 1 || p3bRefString(t, cards[0].Refs, "status") != string(agenthub.TaskCanceled) {
		t.Fatalf("同步泵取消没有写结果卡: %+v", cards)
	}
	if !bsA.s.HasTaskResult(string(task.ID)) {
		t.Fatal("同步泵取消没有回推派发者")
	}
}

// TestAssignTaskQueuedNote（P3b-05 复审 B-1 反向）：目标忙（queued）仍然给
// 「已入队」说明，不能因为终态分支把正常排队也说成终止。
func TestAssignTaskQueuedNote(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "排队A", true)
	b := createDispatchAgent(t, m, buf, 2, "排队B", false)
	g := assignGroup(t, m, buf, 3, "排队群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member",
	})
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	bsB.hubGroupBusy.Store(true) // 目标忙：AgentDispatch 不投，任务留 queued
	t.Cleanup(func() { bsB.hubGroupBusy.Store(false) })

	res, err := callAssign(t, assignTool(t, m, g, a.ID), map[string]any{
		"to_agent": string(b.ID), "instruction": "把价格表整理成一页",
	})
	if err != nil {
		t.Fatalf("assign_task: %v", err)
	}
	task := taskOf(t, res)
	if task.Status != agenthub.TaskQueued {
		t.Fatalf("status = %s, want queued", task.Status)
	}
	note, _ := res["note"].(string)
	if !strings.Contains(note, "已入队") {
		t.Fatalf("queued note = %q，应说明已入队", note)
	}
	if strings.Contains(note, "未能派发") {
		t.Fatalf("queued note = %q，不能说已终止", note)
	}
}
