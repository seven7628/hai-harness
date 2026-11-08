package main

// P3b-08 最终复审阻塞项（B1/B2/B3）与批准的小 P2 的回归测试。
//
//   B1：结算/恢复窗口里**不能**同步跨会话回推。这里锁住「发起者会话正忙（持
//       dispatchMu）」时 finishHubTask 仍必须立刻返回 —— 以前它会在 dispatchMu
//       上等，A↔B 互派就是死锁。
//   B2：自动对话的「取 wake → 投递 → 记账」与停止/人类插话共用一把群闸门。
//       投递窗口里的停止必须等投递结束，而不是先把对话标 stopped、再把 wake
//       投出去（那一轮会绕过预算/深度记账）。
//   B3：根 AgentStart 必须把 dispatched 任务推进到 running（带真实 run_id，
//       任务板/卡片才看得到「运行中」）。
//   P2：恢复的 agent 目标也要写卡+唤醒派发者；task_ack 群校验不再 fail-open；
//       mark_read 夹紧到最后一条；人类派任务开新对话；batch_conflict 额外任务
//       也有结果卡。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// TestHubTaskFinishDoesNotWaitForOriginDispatchLock 是 B1 的直接回归：
// 发起者会话持着 dispatchMu（真实互派时它正在自己的 emit 里等我们），
// 结算必须仍然完成落账并返回；回推由后台通知簿在锁外做。
func TestHubTaskFinishDoesNotWaitForOriginDispatchLock(t *testing.T) {
	m, buf := testManager(t)
	hub := createHubAgent(t, m, buf, 1, "B1Hub")
	origin, err := m.hubDMSession(string(hub.ID))
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: hub.ID},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: "/w", SessionID: "s1"},
		Title:  "B1", Instruction: "B1", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{}); err != nil {
		t.Fatal(err)
	}

	origin.dispatchMu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.finishHubTask(&bridgeSession{ctx: context.Background(), hubTaskIDs: []agenthub.TaskID{task.ID}},
			&events.AgentEnd{RunId: "r-b1", Content: "ok", FinishReason: core.FinishReasonStop})
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		origin.dispatchMu.Unlock()
		t.Fatal("finishHubTask 在发起者 dispatchMu 上等待跨会话回推（B1 死锁回归）")
	}
	// 回推还堵在发起者锁上：结算本身已经落账（completed），但未 delivered。
	got, err := st.LoadTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agenthub.TaskCompleted {
		t.Fatalf("结算后 status = %s, want completed（回推尚未发出）", got.Status)
	}
	origin.dispatchMu.Unlock()
	waitResultNotifyDrained(t, m)
	got, err = st.LoadTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agenthub.TaskDelivered {
		t.Fatalf("放锁后 status = %s, want delivered", got.Status)
	}
	if n := p3bEventCount(t, buf, string(events.TaskResultDeliveredType), task.ID); n != 1 {
		t.Fatalf("task_result_delivered = %d, want 1", n)
	}
}

// TestHubConversationStopWaitsForInFlightDispatch 锁住 B2 的群闸门：
// 泵已经取到 wake（还没投递）时点停止，停止必须排在投递之后生效 —— 不能先把
// 对话停掉、再把 wake 投出去（那样这一轮就不记账了）。
func TestHubConversationStopWaitsForInFlightDispatch(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})
	a := createDispatchAgent(t, m, buf, 1, "GateA", true)
	b := createDispatchAgent(t, m, buf, 2, "GateB", false)
	c := createDispatchAgent(t, m, buf, 3, "GateC", false)
	g := assignGroup(t, m, buf, 4, "闸门群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member", c.ID: "member",
	})
	gate := make(chan struct{})
	fb := newP3bEvalProvider(p3bEvalRound{content: "B 说完了", gate: gate})
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	setGroupFakeLoopIters(t, m, bsB, g, string(b.ID), fb, 1)
	defer close(gate)

	// 直接让控制器入队（生产里 group_say 命中空闲目标会立刻泵一次；这里要的是
	// 「队列里有一条 wake、泵还没跑」的窗口）。
	if _, err := m.hubConversation().Offer(agenthub.ConversationOffer{
		GroupID: g.ID, Sender: c.ID, Targets: []agenthub.AgentID{b.ID}, Text: "@GateB 你来说",
	}); err != nil {
		t.Fatalf("入队: %v", err)
	}
	if v := convView(m, g.ID); v.QueueLen != 1 {
		t.Fatalf("队列 = %+v, want 1 条 wake", v)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	hubConvDispatchHook = func(phase string, gid agenthub.GroupID) {
		if phase == "due" && gid == g.ID {
			close(entered)
			<-release
		}
	}
	defer func() { hubConvDispatchHook = nil }()

	go m.pumpHubConversation(g.ID)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("泵没有进入投递窗口")
	}
	stopDone := make(chan bool, 1)
	go func() {
		stopDone <- m.dispatchHub(command{
			Id: 11, Type: "hub_conversation_stop", Payload: map[string]any{"group_id": string(g.ID)},
		})
	}()
	select {
	case <-stopDone:
		t.Fatal("停止在投递窗口里直接生效（B2：泵与停止没有按群串行）")
	case <-time.After(150 * time.Millisecond):
	}
	if v := convView(m, g.ID); v.Status != agenthub.ConversationRunning {
		t.Fatalf("停止在投递前生效：%+v", v)
	}
	close(release)
	select {
	case ok := <-stopDone:
		if !ok {
			t.Fatal("停止命令没有处理")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("放行投递后停止仍未完成")
	}
	convWait(t, "B 的 run 收尾", func() bool { return !bsB.running.Load() && !bsB.hubGroupBusy.Load() })
	if v := convView(m, g.ID); v.Status != agenthub.ConversationStopped || v.QueueLen != 0 {
		t.Fatalf("停止后对话 = %+v, want stopped/queue=0", v)
	}
	for _, ev := range readLines(buf) {
		if ev["event_type"] == "hub_alert" && ev["kind"] == "group_conversation_turn_lost" {
			t.Fatalf("闸门内出现了 turn_lost 告警（BeginTurn 记账失败）：%v", ev)
		}
	}
}

// TestHubTaskStartMarksRunning 锁住 B3：根 AgentStart 带 RunID → dispatched→running。
func TestHubTaskStartMarksRunning(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "RunA", true)
	b := createDispatchAgent(t, m, buf, 2, "RunB", false)
	g := assignGroup(t, m, buf, 3, "运行中群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member",
	})
	gate := make(chan struct{})
	fb := newP3bEvalProvider(p3bEvalRound{content: "干完了", gate: gate})
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	setGroupFakeLoopIters(t, m, bsB, g, string(b.ID), fb, 1)
	defer close(gate)

	if ok, errMsg, _ := hubCmd(t, m, buf, 10, "hub_group_assign", map[string]any{
		"group_id": string(g.ID), "to_agent": string(b.ID), "instruction": "跑一下",
	}); !ok {
		t.Fatalf("hub_group_assign: %s", errMsg)
	}
	convWait(t, "B 的 run 起来（provider 被调用）", func() bool { return fb.callCount() >= 1 })
	task := p3bEvalAgentTasks(t, m)
	if len(task) != 1 {
		t.Fatalf("agent 任务 = %d, want 1", len(task))
	}
	got := p3bEvalLoadTask(t, m, task[0].ID)
	if got.Status != agenthub.TaskRunning {
		t.Fatalf("AgentStart 后 status = %s, want running", got.Status)
	}
	if got.Dispatch.RunID == "" {
		t.Fatal("running 任务缺 run_id")
	}
	if n := p3bEvalEventCount(buf, "hub_task_update", func(ev map[string]any) bool {
		return ev["task_id"] == string(got.ID) && ev["status"] == string(agenthub.TaskRunning)
	}); n != 1 {
		t.Fatalf("running 快照事件 = %d, want 1", n)
	}
}

// TestHubRecoveryAgentTargetWritesCardAndWakes 覆盖恢复的 TargetAgent 分支（P2）：
// 崩溃前已完成的 agent 任务，恢复时既要补结果卡，也要唤醒派发者。
func TestHubRecoveryAgentTargetWritesCardAndWakes(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "RecA", true)
	b := createDispatchAgent(t, m, buf, 2, "RecB", false)
	g := assignGroup(t, m, buf, 3, "恢复群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member",
	})
	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginGroup, GroupID: g.ID, AgentID: a.ID},
		Target: agenthub.TaskTarget{
			Kind: agenthub.TargetAgent, AgentID: b.ID, GroupID: g.ID,
			SessionID: hubSessionID(string(b.ID), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID}),
		},
		Title: "恢复任务", Instruction: "恢复任务", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{RunID: "run-rec"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Finish(context.Background(), agenthub.FinishInput{
		TaskID: task.ID, RunID: "run-rec", Status: agenthub.TaskCompleted,
		Result: &agenthub.TaskResult{Summary: "重启前就做完了"},
	}); err != nil {
		t.Fatal(err)
	}
	buf.Take()
	recoverAndDrain(t, m)

	msgs := p3bMessages(t, m, g.ID)
	if cards := p3bTaskResultCards(msgs, task.ID); len(cards) != 1 {
		t.Fatalf("恢复后结果卡 = %d 张, want 1（msgs=%+v）", len(cards), msgs)
	}
	convWait(t, "派发者收到 task_result", func() bool { return bsA.s.HasTaskResult(string(task.ID)) })
	if got := p3bEvalLoadTask(t, m, task.ID); got.Status != agenthub.TaskDelivered {
		t.Fatalf("恢复后 status = %s, want delivered", got.Status)
	}
	// 二次恢复不得重复写卡/重复回推。
	buf.Take()
	recoverAndDrain(t, m)
	if n := len(p3bTaskResultCards(p3bMessages(t, m, g.ID), task.ID)); n != 1 {
		t.Fatalf("二次恢复写了 %d 张卡, want 1", n)
	}
	if n := p3bEventCount(t, buf, string(events.TaskResultDeliveredType), task.ID); n != 0 {
		t.Fatalf("二次恢复回推了 %d 次, want 0", n)
	}
}

// TestHubTaskAckRejectsMissingGroup 锁住 task_ack 的严格群校验（P2）：
// 目标任务的 group_id 缺失或与调用方群不一致都必须拒绝（旧的 fail-open）。
func TestHubTaskAckRejectsMissingGroup(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "AckA", true)
	b := createDispatchAgent(t, m, buf, 2, "AckB", false)
	g := assignGroup(t, m, buf, 3, "ack 群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member",
	})
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenthub.NewLedger(st)
	mk := func(groupID agenthub.GroupID) *agenthub.Task {
		task, err := ledger.Create(context.Background(), &agenthub.Task{
			Origin: agenthub.TaskOrigin{Kind: agenthub.OriginGroup, GroupID: g.ID, AgentID: a.ID},
			Target: agenthub.TaskTarget{Kind: agenthub.TargetAgent, AgentID: b.ID, GroupID: groupID, SessionID: bsB.id},
			Title:  "ack", Instruction: "ack", Callback: agenthub.CallbackAgentEnd,
		})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}

	// ledger.Create 会拒绝 agent 目标缺 group_id，所以先建合法任务再把 group_id 抠掉，
	// 模拟旧账本/异常装配留下来的脏快照。
	missing := mk(g.ID)
	blank, err := st.LoadTask(context.Background(), missing.ID)
	if err != nil {
		t.Fatal(err)
	}
	blank.Target.GroupID = ""
	if err := st.SaveTask(context.Background(), blank); err != nil {
		t.Fatal(err)
	}
	bsB.bindHubTasks([]agenthub.TaskID{missing.ID})
	if _, err := m.hubTaskAckTarget(bsB.id, b.ID, g.ID); err == nil {
		t.Fatal("group_id 缺失时 task_ack 仍然放行（fail-open 回归）")
	}
	other := mk(agenthub.GroupID("grp_other"))
	bsB.bindHubTasks([]agenthub.TaskID{other.ID})
	if _, err := m.hubTaskAckTarget(bsB.id, b.ID, g.ID); err == nil {
		t.Fatal("群不匹配时 task_ack 仍然放行")
	}
	good := mk(g.ID)
	bsB.bindHubTasks([]agenthub.TaskID{good.ID})
	if got, err := m.hubTaskAckTarget(bsB.id, b.ID, g.ID); err != nil || got == nil || got.ID != good.ID {
		t.Fatalf("正确的任务被拒: %+v, %v", got, err)
	}
}

// TestHubMarkReadClampsToLastSeq 锁住 P2：超过日志末尾的 seq 按末尾算，
// 不能让一条旧快照把「刚落盘的新消息」一起标成已读。
func TestHubMarkReadClampsToLastSeq(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "ReadA", true)
	g := assignGroup(t, m, buf, 2, "已读群", map[agenthub.AgentID]string{a.ID: "owner"})
	for _, text := range []string{"第一条", "第二条"} {
		if ok, errMsg, _ := hubCmd(t, m, buf, 3, "hub_group_send", map[string]any{
			"group_id": string(g.ID), "text": text,
		}); !ok {
			t.Fatalf("hub_group_send: %s", errMsg)
		}
	}
	msgs := p3bMessages(t, m, g.ID)
	last := msgs[len(msgs)-1].Seq
	ok, errMsg, data := hubCmd(t, m, buf, 4, "hub_mark_read", map[string]any{
		"group_id": string(g.ID), "seq": last + 1000,
	})
	if !ok {
		t.Fatalf("hub_mark_read: %s", errMsg)
	}
	if got, _ := data["seq"].(float64); int64(got) != last {
		t.Fatalf("mark_read seq = %v, want 夹紧到 %d", data["seq"], last)
	}
}

// TestHubGroupAssignStartsNewConversation：人类派任务也是一条人类消息，
// 必须清掉自动接龙的队列并换代（P2）。
func TestHubGroupAssignStartsNewConversation(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})
	a := createDispatchAgent(t, m, buf, 1, "AsgA", true)
	b := createDispatchAgent(t, m, buf, 2, "AsgB", false)
	c := createDispatchAgent(t, m, buf, 3, "AsgC", false)
	g := assignGroup(t, m, buf, 4, "派活群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member", c.ID: "member",
	})
	fb := newP3bEvalProvider(p3bEvalRound{content: "收到"})
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	setGroupFakeLoopIters(t, m, bsB, g, string(b.ID), fb, 1)

	// 先让对话里有一条排队 wake（直接入队，避免 group_say 的空闲即时泵），再人类派活。
	if _, err := m.hubConversation().Offer(agenthub.ConversationOffer{
		GroupID: g.ID, Sender: c.ID, Targets: []agenthub.AgentID{b.ID}, Text: "@AsgB 先别动",
	}); err != nil {
		t.Fatalf("入队: %v", err)
	}
	before := convView(m, g.ID)
	if before.QueueLen != 1 {
		t.Fatalf("派活前队列 = %+v, want 1", before)
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 10, "hub_group_assign", map[string]any{
		"group_id": string(g.ID), "to_agent": string(b.ID), "instruction": "做点正事",
	}); !ok {
		t.Fatalf("hub_group_assign: %s", errMsg)
	}
	after := convView(m, g.ID)
	if after.QueueLen != 0 {
		t.Fatalf("人类派活后仍保留 %d 条自动接龙（应清队列）", after.QueueLen)
	}
	if after.ID == before.ID {
		t.Fatal("人类派活没有换代（旧对话的预算/深度会继续生效）")
	}
	if after.TurnsUsed != 1 {
		t.Fatalf("TurnsUsed = %d, want 1（人类首轮）", after.TurnsUsed)
	}
	convWait(t, "派活任务投递", func() bool { return fb.callCount() >= 1 })
}

// TestHubTaskFinishBatchConflictExtrasGetCard：batch_conflict 的额外任务
// 以前只发告警，派发者永远等不到结果（P2）。
func TestHubTaskFinishBatchConflictExtrasGetCard(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "BcA", true)
	b := createDispatchAgent(t, m, buf, 2, "BcB", false)
	c := createDispatchAgent(t, m, buf, 3, "BcC", false)
	g := assignGroup(t, m, buf, 4, "冲突群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member", c.ID: "member",
	})
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenthub.NewLedger(st)
	ids := make([]agenthub.TaskID, 0, 2)
	for _, target := range []agenthub.AgentID{b.ID, c.ID} {
		task, err := ledger.Create(context.Background(), &agenthub.Task{
			Origin: agenthub.TaskOrigin{Kind: agenthub.OriginGroup, GroupID: g.ID, AgentID: a.ID},
			Target: agenthub.TaskTarget{
				Kind: agenthub.TargetAgent, AgentID: target, GroupID: g.ID,
				SessionID: hubSessionID(string(target), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID}),
			},
			Title: "冲突", Instruction: "x", Callback: agenthub.CallbackAgentEnd,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, task.ID)
	}
	m.finishHubTask(&bridgeSession{ctx: context.Background(), hubTaskIDs: ids},
		&events.AgentEnd{RunId: "r-batch", Content: "ok", FinishReason: core.FinishReasonStop})
	waitResultNotifyDrained(t, m)

	msgs := p3bMessages(t, m, g.ID)
	for _, id := range ids {
		if cards := p3bTaskResultCards(msgs, id); len(cards) != 1 {
			t.Fatalf("任务 %s 的结果卡 = %d 张, want 1", id, len(cards))
		}
	}
	second := p3bEvalLoadTask(t, m, ids[1])
	if second.Status != agenthub.TaskDelivered || second.Error == "" ||
		!strings.Contains(second.Error, "batch_conflict") {
		t.Fatalf("额外任务 = %+v, want failed + batch_conflict 后 delivered", second)
	}
}
