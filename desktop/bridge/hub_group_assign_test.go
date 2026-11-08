package main

// P3b-07a：人类从群 UI 派任务（hub_group_assign）。
//
// 与 P3b-03 的智能体工具 assign_task 同一条业务路径（assignHubTask），差异：
//   - 任务 Origin=human（没有发起智能体）；
//   - 指派消息 Sender=human、mentions=[B]、intent=assign、task_id；
//   - 不受「同一对 2 个在途」与派发权限约束；群/目标/指令校验照旧。
//
// 全部用确定性 fake provider：派发链路只在进程内跑，不碰网络；等待用轮询/门闩。

import (
	"context"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// humanAssign 发一条 hub_group_assign 命令。
func humanAssign(t *testing.T, m *manager, buf *syncBuf, id int, payload map[string]any) (bool, string, map[string]any) {
	t.Helper()
	return hubCmd(t, m, buf, id, "hub_group_assign", payload)
}

// humanTaskUpdate 取某条 hub_task_update 事件（找不到 = nil）。
func humanTaskUpdate(buf *syncBuf, id agenthub.TaskID, status agenthub.TaskStatus) map[string]any {
	for _, ev := range readAllLines(buf) {
		if ev["event_type"] != "hub_task_update" || ev["task_id"] != string(id) {
			continue
		}
		if status == "" || ev["status"] == string(status) {
			return ev
		}
	}
	return nil
}

// humanTaskUpdateOriginKind 从 hub_task_update 的整条 task 里取 origin.kind。
func humanTaskUpdateOriginKind(ev map[string]any) string {
	task, _ := ev["task"].(map[string]any)
	origin, _ := task["origin"].(map[string]any)
	kind, _ := origin["kind"].(string)
	return kind
}

// TestHumanGroupAssign：人派任务 → 账本 origin=human、assign 消息 sender=human、
// 只唤醒 B（正文里的 @ 不路由），响应/任务板事件字段齐全。
func TestHumanGroupAssign(t *testing.T) {
	m, buf := testManager(t)
	b := createDispatchAgent(t, m, buf, 1, "人类派活B", false)
	c := createDispatchAgent(t, m, buf, 2, "旁听C", false)
	g := assignGroup(t, m, buf, 3, "人类派活群", map[agenthub.AgentID]string{
		b.ID: "owner", c.ID: "observer",
	})
	// B 用门闩式 fake：LLM 流不放开，任务就停在 dispatched —— 响应里的状态是确定的。
	fb := newConvProvider("收到人类指派，马上做")
	releaseB := fb.gated()
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	setGroupFakeLoop(t, m, bsB, g, string(b.ID), fb)
	fc := newConvProvider("我不该被叫醒")
	bsC := mustGroupSession(t, m, string(c.ID), string(g.ID))
	setGroupFakeLoop(t, m, bsC, g, string(c.ID), fc)

	// 正文里的 @C 是写给 B 的内容：结构化 mentions 只有 B，C 不能被路由/唤醒。
	instruction := "把三家价格表整理成一页；@旁听C 这活不用你接"
	buf.Reset()
	ok, errMsg, data := humanAssign(t, m, buf, 10, map[string]any{
		"group_id": string(g.ID), "to_agent": string(b.ID),
		"instruction": instruction, "title": "价格简报",
	})
	if !ok {
		t.Fatalf("hub_group_assign: %s", errMsg)
	}

	task := taskOf(t, data)
	if task.Origin.Kind != agenthub.OriginHuman || task.Origin.GroupID != g.ID || task.Origin.AgentID != "" {
		t.Fatalf("origin = %+v, want human/群/无 agent", task.Origin)
	}
	if task.Target.Kind != agenthub.TargetAgent || task.Target.AgentID != b.ID || task.Target.GroupID != g.ID {
		t.Fatalf("target = %+v", task.Target)
	}
	if task.Callback != agenthub.CallbackAgentEnd || task.Title != "价格简报" ||
		task.Instruction != instruction || task.Status != agenthub.TaskDispatched {
		t.Fatalf("task = %+v（status 应为 dispatched：B 的流被门闩挡住）", task)
	}
	if task.Origin.MessageSeq == 0 {
		t.Fatal("origin.message_seq 未绑定")
	}
	// 响应与 assign_task 同形。
	if data["task_id"] != string(task.ID) || data["to_agent"] != string(b.ID) {
		t.Fatalf("响应字段 = %+v", data)
	}
	if data["status"] != string(agenthub.TaskDispatched) {
		t.Fatalf("status = %v, want %q", data["status"], agenthub.TaskDispatched)
	}
	if dispatched, _ := data["dispatched"].(bool); !dispatched {
		t.Fatalf("dispatched = %v, want true（目标空闲）", data["dispatched"])
	}
	if seq, _ := data["seq"].(float64); int64(seq) != task.Origin.MessageSeq {
		t.Fatalf("seq = %v, want %d", data["seq"], task.Origin.MessageSeq)
	}

	// 群消息：sender=human、intent=assign、mentions=[B]、task_id、正文=instruction。
	total, assigns := countAssignMessages(t, m, g.ID)
	if total != 1 || len(assigns) != 1 {
		t.Fatalf("messages = %d, assign = %d", total, len(assigns))
	}
	msg := assigns[0]
	if msg.Kind != agenthub.KindText || msg.Sender.Kind != agenthub.SenderHuman ||
		msg.Sender.AgentID != "" || msg.Intent != agenthub.IntentAssign ||
		msg.TaskID != task.ID || msg.Text != instruction {
		t.Fatalf("assign message = %+v", msg)
	}
	if len(msg.Mentions) != 1 || msg.Mentions[0] != b.ID || msg.MentionAll {
		t.Fatalf("mentions = %v all=%v, want 只有 B", msg.Mentions, msg.MentionAll)
	}
	if msg.Seq != task.Origin.MessageSeq {
		t.Fatalf("message seq = %d, origin seq = %d", msg.Seq, task.Origin.MessageSeq)
	}
	// 路由回执只 targeting B（正文 @C 不参与路由）。
	st := mustHubStore(t, m)
	acts, err := st.ReadActivity(context.Background(), g.ID, 0, 50)
	if err != nil || len(acts) != 1 || acts[0].AfterSeq != msg.Seq+1 {
		t.Fatalf("activity = %+v, %v", acts, err)
	}

	// 任务板事件：hub_message（人发的 assign）→ queued → dispatched，且 origin=human。
	evMsg := -1
	eventsList := readEvents(t, buf)
	for i, ev := range eventsList {
		if ev["event_type"] != "hub_message" {
			continue
		}
		mm, _ := ev["message"].(map[string]any)
		if mm != nil && mm["intent"] == string(agenthub.IntentAssign) {
			sender, _ := mm["sender"].(map[string]any)
			if sender == nil || sender["kind"] != string(agenthub.SenderHuman) {
				t.Fatalf("hub_message sender = %v, want human", mm["sender"])
			}
			evMsg = i
		}
	}
	if evMsg < 0 {
		t.Fatalf("缺 hub_message(intent=assign)：\n%s", buf.String())
	}
	queued := humanTaskUpdate(buf, task.ID, agenthub.TaskQueued)
	dispatched := humanTaskUpdate(buf, task.ID, agenthub.TaskDispatched)
	if queued == nil || dispatched == nil {
		t.Fatalf("缺 hub_task_update(queued/dispatched)：\n%s", buf.String())
	}
	if got := humanTaskUpdateOriginKind(dispatched); got != string(agenthub.OriginHuman) {
		t.Fatalf("hub_task_update origin.kind = %q, want human", got)
	}
	if dispatched["group_id"] != string(g.ID) || dispatched["target_kind"] != string(agenthub.TargetAgent) {
		t.Fatalf("hub_task_update = %+v", dispatched)
	}

	// B 被唤醒：请求里有 hub_task header，且 header 的 origin=human。
	fb.waitStarted(t)
	if fc.callCount() != 0 {
		t.Fatal("正文里的 @旁听C 不应路由：C 被叫醒了")
	}
	req := fb.lastRequest()
	if req == nil {
		t.Fatal("B 没有被唤醒")
	}
	headers := 0
	for _, cm := range req.Messages {
		for _, c := range cm.Content {
			if c.Type != core.ContentTypeHubTask {
				continue
			}
			headers++
			h, err := agenthub.ParseTaskHeader(c.Content)
			if err != nil || h.TaskID != task.ID || h.Origin.Kind != agenthub.OriginHuman {
				t.Fatalf("hub_task header = %+v, %v", h, err)
			}
		}
	}
	if headers != 1 {
		t.Fatalf("hub_task header 块 = %d, want 1", headers)
	}

	// 放开 B：结算写一张群内结果卡，且不再回推任何人（人不需要唤醒）。
	close(releaseB)
	waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		return len(p3bTaskResultCards(msgs, task.ID)) == 1
	})
	if fc.callCount() != 0 {
		t.Fatal("结算不应唤醒旁观者")
	}
	if done := humanTaskUpdate(buf, task.ID, agenthub.TaskCompleted); done == nil {
		t.Fatalf("缺 hub_task_update(completed)：\n%s", buf.String())
	}
	if n := p3bEventCount(t, buf, string(events.TaskResultDeliveredType), task.ID); n != 0 {
		t.Fatalf("人类来源任务不应有 task_result_delivered，got %d", n)
	}
}

// TestHumanGroupAssignBusyQueuesAndPumps：B 忙 → 任务留 queued；B 的回合结束
// （hubOnRunEnd）后任务泵把它派出去，完成时照常写结果卡，但**不回推人类**。
func TestHumanGroupAssignBusyQueuesAndPumps(t *testing.T) {
	m, buf := testManager(t)
	b := createDispatchAgent(t, m, buf, 1, "忙目标B", false)
	g := assignGroup(t, m, buf, 2, "忙派活群", map[agenthub.AgentID]string{b.ID: "owner"})
	fake := &evalProvider{reply: "忙完这轮就做"}
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	setGroupFakeLoop(t, m, bsB, g, string(b.ID), fake)

	bsB.hubGroupBusy.Store(true) // B 正在跑别的群轮：AgentDispatch 不投
	t.Cleanup(func() { bsB.hubGroupBusy.Store(false) })

	buf.Reset()
	ok, errMsg, data := humanAssign(t, m, buf, 10, map[string]any{
		"group_id": string(g.ID), "to_agent": string(b.ID), "instruction": "整理成一页",
	})
	if !ok {
		t.Fatalf("hub_group_assign: %s", errMsg)
	}
	task := taskOf(t, data)
	if task.Status != agenthub.TaskQueued {
		t.Fatalf("status = %s, want queued（B 忙）", task.Status)
	}
	if dispatched, _ := data["dispatched"].(bool); dispatched {
		t.Fatalf("忙目标不能报 dispatched=true: %+v", data)
	}
	note, _ := data["note"].(string)
	if !strings.Contains(note, "已入队") || strings.Contains(note, "未能派发") {
		t.Fatalf("note = %q，应说明已入队而非终止", note)
	}
	if fake.callCount() != 0 {
		t.Fatal("B 忙时不该被投递")
	}
	if cards := p3bTaskResultCards(p3bMessages(t, m, g.ID), task.ID); len(cards) != 0 {
		t.Fatalf("排队中不应有结果卡: %+v", cards)
	}

	// B 的回合真正收尾：闩放开 + emit 末尾的 hubOnRunEnd → 任务泵派发这条任务。
	bsB.hubGroupBusy.Store(false)
	if bsB.hubOnRunEnd == nil {
		t.Fatal("群会话没有 hubOnRunEnd（生产接线缺失，忙目标任务永远卡 queued）")
	}
	bsB.hubOnRunEnd()

	msgs := waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		return len(p3bTaskResultCards(msgs, task.ID)) == 1
	})
	cards := p3bTaskResultCards(msgs, task.ID)
	if got := p3bRefString(t, cards[0].Refs, "status"); got != string(agenthub.TaskCompleted) {
		t.Fatalf("结果卡 status = %q, want completed", got)
	}
	if cards[0].Sender.Kind != agenthub.SenderAgent || cards[0].Sender.AgentID != b.ID {
		t.Fatalf("结果卡 sender = %+v, want 执行者 B", cards[0].Sender)
	}
	if cards[0].Seq <= task.Origin.MessageSeq {
		t.Fatalf("结果卡 seq = %d，应晚于指派消息 seq = %d", cards[0].Seq, task.Origin.MessageSeq)
	}
	done := p3bLoadTask(t, m, task.ID)
	if done.Status != agenthub.TaskCompleted {
		t.Fatalf("task status = %s, want completed", done.Status)
	}
	if !p3bHasTaskUpdate(buf, task.ID, agenthub.TaskCompleted) {
		t.Fatalf("缺 hub_task_update(completed)：\n%s", buf.String())
	}
	// 人不需要唤醒：没有 task_result_delivered，也没有 push 失败告警。
	if n := p3bEventCount(t, buf, string(events.TaskResultDeliveredType), task.ID); n != 0 {
		t.Fatalf("人类来源任务回推 %d 次, want 0", n)
	}
	for _, ev := range readAllLines(buf) {
		if ev["event_type"] != "hub_alert" || ev["task_id"] != string(task.ID) {
			continue
		}
		if ev["code"] == "push_failed" || ev["code"] == "notify_failed" {
			t.Fatalf("不应尝试唤醒人类：%+v", ev)
		}
	}
	if _, assigns := countAssignMessages(t, m, g.ID); len(assigns) != 1 {
		t.Fatalf("指派消息 = %d, want 1（任务泵不应重写消息）", len(assigns))
	}
}

// TestHumanGroupAssignRejectsNonMember：非成员/软删成员/空目标都要在**写之前**失败。
func TestHumanGroupAssignRejectsNonMember(t *testing.T) {
	m, buf := testManager(t)
	b := createDispatchAgent(t, m, buf, 1, "群内B", false)
	outsider := createDispatchAgent(t, m, buf, 2, "群外C", false)
	gone := createDispatchAgent(t, m, buf, 3, "将被删D", false)
	g := assignGroup(t, m, buf, 4, "成员校验群", map[agenthub.AgentID]string{
		b.ID: "owner", gone.ID: "member",
	})
	if ok, errMsg, _ := hubCmd(t, m, buf, 5, "hub_agent_delete", map[string]any{"agent_id": string(gone.ID)}); !ok {
		t.Fatalf("soft delete: %s", errMsg)
	}

	cases := []struct {
		name string
		to   string
		want string
	}{
		{"非成员", string(outsider.ID), "不是群内成员"},
		{"软删成员", string(gone.ID), "已删除"},
		{"空目标", "  ", "to_agent 不能为空"},
	}
	for i, tc := range cases {
		ok, errMsg, _ := humanAssign(t, m, buf, 20+i, map[string]any{
			"group_id": string(g.ID), "to_agent": tc.to, "instruction": "不该建任务",
		})
		if ok {
			t.Fatalf("%s: 应失败", tc.name)
		}
		if !strings.Contains(errMsg, tc.want) {
			t.Fatalf("%s: error = %q, want 含 %q", tc.name, errMsg, tc.want)
		}
	}
	if n := countTasks(t, m); n != 0 {
		t.Fatalf("拒绝路径建了 %d 条任务", n)
	}
	if total, assigns := countAssignMessages(t, m, g.ID); total != 0 || len(assigns) != 0 {
		t.Fatalf("拒绝路径写了消息：total=%d assign=%d", total, len(assigns))
	}
}

// TestHumanGroupAssignRejectsInvalidGroup：群不存在/已删除/空 id 都要给可读错误。
func TestHumanGroupAssignRejectsInvalidGroup(t *testing.T) {
	m, buf := testManager(t)
	b := createDispatchAgent(t, m, buf, 1, "群B", false)
	g := assignGroup(t, m, buf, 2, "会被删的群", map[agenthub.AgentID]string{b.ID: "owner"})
	if ok, errMsg, _ := hubCmd(t, m, buf, 3, "hub_group_delete", map[string]any{"group_id": string(g.ID)}); !ok {
		t.Fatalf("delete group: %s", errMsg)
	}

	cases := []struct {
		name    string
		groupID string
		want    string
	}{
		{"空群 id", "  ", "group_id 不能为空"},
		{"群不存在", "grp_doesnotexist", "群组不存在"},
		{"群已删除", string(g.ID), "群组不存在或已删除"},
	}
	for i, tc := range cases {
		ok, errMsg, _ := humanAssign(t, m, buf, 20+i, map[string]any{
			"group_id": tc.groupID, "to_agent": string(b.ID), "instruction": "不该建任务",
		})
		if ok {
			t.Fatalf("%s: 应失败", tc.name)
		}
		if !strings.Contains(errMsg, tc.want) {
			t.Fatalf("%s: error = %q, want 含 %q", tc.name, errMsg, tc.want)
		}
	}
	if n := countTasks(t, m); n != 0 {
		t.Fatalf("拒绝路径建了 %d 条任务", n)
	}
}

// TestHumanGroupAssignRejectsEmptyInstruction：空/空白指令在写之前失败。
func TestHumanGroupAssignRejectsEmptyInstruction(t *testing.T) {
	m, buf := testManager(t)
	b := createDispatchAgent(t, m, buf, 1, "空指令B", false)
	g := assignGroup(t, m, buf, 2, "空指令群", map[agenthub.AgentID]string{b.ID: "owner"})

	for i, instruction := range []string{"", "   ", "\n\t"} {
		ok, errMsg, _ := humanAssign(t, m, buf, 10+i, map[string]any{
			"group_id": string(g.ID), "to_agent": string(b.ID), "instruction": instruction,
		})
		if ok {
			t.Fatalf("空指令 %q 应失败", instruction)
		}
		if !strings.Contains(errMsg, "instruction 不能为空") {
			t.Fatalf("error = %q, want instruction 不能为空", errMsg)
		}
	}
	if n := countTasks(t, m); n != 0 {
		t.Fatalf("空指令建了 %d 条任务", n)
	}
	if total, _ := countAssignMessages(t, m, g.ID); total != 0 {
		t.Fatalf("空指令写了 %d 条消息", total)
	}
}

// TestHumanGroupAssignIdempotentClientMsgID：同一 client_msg_id → 一条任务/一条消息；
// 换一个客户端键则是新任务（人重复派同一件事不受智能体去重窗口约束）。
func TestHumanGroupAssignIdempotentClientMsgID(t *testing.T) {
	m, buf := testManager(t)
	b := createDispatchAgent(t, m, buf, 1, "幂等B", false)
	g := assignGroup(t, m, buf, 2, "幂等群", map[agenthub.AgentID]string{b.ID: "owner"})
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	bsB.hubGroupBusy.Store(true) // 留 queued：第二次调用走账本快路径，不依赖 run 时序
	t.Cleanup(func() { bsB.hubGroupBusy.Store(false) })

	payload := map[string]any{
		"group_id": string(g.ID), "to_agent": string(b.ID),
		"instruction": "整理成一页", "client_msg_id": "ui-msg-1",
	}
	ok, errMsg, first := humanAssign(t, m, buf, 10, payload)
	if !ok {
		t.Fatalf("first: %s", errMsg)
	}
	ok, errMsg, second := humanAssign(t, m, buf, 11, payload)
	if !ok {
		t.Fatalf("second: %s", errMsg)
	}
	if first["task_id"] != second["task_id"] {
		t.Fatalf("同一 client_msg_id 建了两条任务: %v vs %v", first["task_id"], second["task_id"])
	}
	if n := countTasks(t, m); n != 1 {
		t.Fatalf("任务数 = %d, want 1", n)
	}
	if total, assigns := countAssignMessages(t, m, g.ID); total != 1 || len(assigns) != 1 {
		t.Fatalf("消息数 = %d assign=%d, want 1", total, len(assigns))
	}

	// 不同客户端键：同一条指令也允许再派（人类来源跳过智能体去重窗口）。
	other := map[string]any{
		"group_id": string(g.ID), "to_agent": string(b.ID),
		"instruction": "整理成一页", "client_msg_id": "ui-msg-2",
	}
	ok, errMsg, third := humanAssign(t, m, buf, 12, other)
	if !ok {
		t.Fatalf("third: %s", errMsg)
	}
	if third["task_id"] == first["task_id"] {
		t.Fatal("不同 client_msg_id 不应复用同一条任务")
	}
	if n := countTasks(t, m); n != 2 {
		t.Fatalf("任务数 = %d, want 2", n)
	}
	if total, _ := countAssignMessages(t, m, g.ID); total != 2 {
		t.Fatalf("消息数 = %d, want 2", total)
	}
}

// TestHumanGroupAssignBudgetHardStop：人派的任务没有发起智能体（单任务/智能体日
// 预算不适用），但全局/群日预算的**运行中硬停**必须仍然生效 —— 否则人派的长任务
// 可以无视群预算一直烧（P3b-07a；与 hubHumanAssignGuard 的建账口径一致）。
func TestHumanGroupAssignBudgetHardStop(t *testing.T) {
	m, buf := testManager(t)
	b := createDispatchAgent(t, m, buf, 1, "预算B", false)
	g := assignGroup(t, m, buf, 2, "预算群", map[agenthub.AgentID]string{b.ID: "owner"})
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	bsB.hubGroupBusy.Store(true) // 任务留 queued：本测试只验证硬停判定
	t.Cleanup(func() { bsB.hubGroupBusy.Store(false) })

	ok, errMsg, data := humanAssign(t, m, buf, 10, map[string]any{
		"group_id": string(g.ID), "to_agent": string(b.ID), "instruction": "慢慢烧钱",
	})
	if !ok {
		t.Fatalf("hub_group_assign: %s", errMsg)
	}
	task := taskOf(t, data)
	if stop, _ := m.hubBudgetCheck(task.ID, 0); stop {
		t.Fatal("没有超预算时不应硬停")
	}
	stop, msg := m.hubBudgetCheck(task.ID, agenthub.DefaultGlobalDailyBudgetUSD+1)
	if !stop || !strings.Contains(msg, "全局日预算") {
		t.Fatalf("hubBudgetCheck = %v/%q, want 全局预算硬停", stop, msg)
	}
}
