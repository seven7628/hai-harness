package main

// P3b-03：assign_task —— 群内智能体指派可追踪任务（账本 + 群消息 + hub_task 派发）。
//
// 全部用确定性 fake provider：派发链路只在进程内跑，不碰网络。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
)

// createDispatchAgent 建一个可配置 policy.can_dispatch 的智能体。
func createDispatchAgent(t *testing.T, m *manager, buf *syncBuf, id int, name string, canDispatch bool) agenthub.AgentProfile {
	t.Helper()
	ok, errMsg, data := hubCmd(t, m, buf, id, "hub_agent_create", map[string]any{
		"profile": map[string]any{
			"name": name, "provider": "deepseek", "model": "deepseek-v4-flash", "role": "测试角色",
			"policy": map[string]any{"can_dispatch": canDispatch},
		},
	})
	if !ok {
		t.Fatalf("create agent %s failed: %s", name, errMsg)
	}
	return agentFromData(t, data)
}

// assignGroup 建群（不夹带人类种子消息：assign 的轮次上限不应被别的消息干扰）。
func assignGroup(t *testing.T, m *manager, buf *syncBuf, cmdID int, name string, members map[agenthub.AgentID]string) *agenthub.Group {
	t.Helper()
	raw := make([]map[string]any, 0, len(members))
	for id, role := range members {
		raw = append(raw, map[string]any{"agent_id": string(id), "role": role})
	}
	ok, errMsg, data := hubCmd(t, m, buf, cmdID, "hub_group_create", map[string]any{
		"name": name, "members": raw,
	})
	if !ok {
		t.Fatalf("create group: %s", errMsg)
	}
	return groupFromData(t, data)
}

func assignTool(t *testing.T, m *manager, g *agenthub.Group, agentID agenthub.AgentID) *hubAssignTaskTool {
	t.Helper()
	bs, err := m.hubGroupSession(string(agentID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	tool, err := bs.hubEngine.GetTool(context.Background(), "assign_task")
	if err != nil {
		t.Fatalf("assign_task not registered in group session: %v", err)
	}
	got, ok := tool.(*hubAssignTaskTool)
	if !ok {
		t.Fatalf("assign_task tool = %T", tool)
	}
	return got
}

func callAssign(t *testing.T, tool *hubAssignTaskTool, args map[string]any) (map[string]any, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Call(context.Background(), "assign_task", string(raw))
	if err != nil {
		return nil, err
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("assign_task 结果不是 JSON: %s (%v)", out, err)
	}
	return res, nil
}

func taskOf(t *testing.T, res map[string]any) *agenthub.Task {
	t.Helper()
	raw, ok := res["task"]
	if !ok {
		t.Fatalf("结果缺 task: %+v", res)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var task agenthub.Task
	if err := json.Unmarshal(b, &task); err != nil {
		t.Fatal(err)
	}
	return &task
}

// readEvents 把写出的 JSONL 事件解析成有序列表（用于断言事件顺序）。
func readEvents(t *testing.T, buf *syncBuf) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("事件不是 JSON: %s (%v)", line, err)
		}
		out = append(out, ev)
	}
	return out
}

func lastIndexOfEvent(events []map[string]any, typ string, pred func(map[string]any) bool) int {
	idx := -1
	for i, ev := range events {
		if ev["event_type"] != typ {
			continue
		}
		if pred == nil || pred(ev) {
			idx = i
		}
	}
	return idx
}

func countTasks(t *testing.T, m *manager) int {
	t.Helper()
	st := mustHubStore(t, m)
	tasks, err := st.ListTasks(context.Background(), agenthub.TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return len(tasks)
}

func countAssignMessages(t *testing.T, m *manager, gid agenthub.GroupID) (messages int, assigns []agenthub.Message) {
	t.Helper()
	st := mustHubStore(t, m)
	msgs, err := st.ReadMessages(context.Background(), gid, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range msgs {
		if msg.Kind == agenthub.KindRouting {
			continue
		}
		messages++
		if msg.Intent == agenthub.IntentAssign {
			assigns = append(assigns, msg)
		}
	}
	return messages, assigns
}

func TestAssignTaskCreatesLedgerTaskAndAssignMessage(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "AssignA", true)
	b := createDispatchAgent(t, m, buf, 2, "AssignB", true)
	g := assignGroup(t, m, buf, 3, "指派群", map[agenthub.AgentID]string{a.ID: "owner", b.ID: "member"})
	st := mustHubStore(t, m)
	art := &agenthub.Artifact{
		Name: "价格表.md", Path: "reports/价格表.md", Kind: "created", OwnerAgentID: a.ID, GroupID: g.ID,
		Visibility: agenthub.VisibilityGroup,
	}
	if err := agenthub.RegisterArtifact(context.Background(), st, art); err != nil {
		t.Fatalf("artifact: %v", err)
	}

	fake := &evalProvider{reply: "做完了"}
	bsB, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	setGroupFakeLoop(t, m, bsB, g, string(b.ID), fake)

	tool := assignTool(t, m, g, a.ID)
	buf.Reset()
	res, err := callAssign(t, tool, map[string]any{
		"to_agent":     string(b.ID),
		"instruction":  "写一份三家价格对比",
		"title":        "价格对比",
		"artifact_ids": []string{string(art.ID)},
	})
	if err != nil {
		t.Fatalf("assign_task: %v", err)
	}
	task := taskOf(t, res)

	// 账本：同一本账，target.kind=agent + 确定性群会话。
	if task.Target.Kind != agenthub.TargetAgent || task.Target.AgentID != b.ID ||
		task.Target.GroupID != g.ID {
		t.Fatalf("target = %+v", task.Target)
	}
	wantSID := hubSessionID(string(b.ID), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID})
	if task.Target.SessionID != wantSID {
		t.Fatalf("session_id = %q, want %q", task.Target.SessionID, wantSID)
	}
	if task.Origin.Kind != agenthub.OriginGroup || task.Origin.GroupID != g.ID || task.Origin.AgentID != a.ID {
		t.Fatalf("origin = %+v", task.Origin)
	}
	if task.Callback != agenthub.CallbackAgentEnd || task.Title != "价格对比" ||
		task.Instruction != "写一份三家价格对比" {
		t.Fatalf("task = %+v", task)
	}
	if len(task.ArtifactIDs) != 1 || task.ArtifactIDs[0] != string(art.ID) {
		t.Fatalf("artifact_ids = %v, want [%s]", task.ArtifactIDs, art.ID)
	}
	if task.ID == "" || task.RootTaskID != task.ID || len(task.Chain) != 1 || task.Chain[0] != task.ID {
		t.Fatalf("id/root/chain = %s/%s/%v", task.ID, task.RootTaskID, task.Chain)
	}
	if task.Status != agenthub.TaskDispatched {
		t.Fatalf("status = %q, want dispatched（目标空闲）", task.Status)
	}
	if task.Origin.MessageSeq == 0 {
		t.Fatal("origin.message_seq 未绑定")
	}

	// 群消息：kind=text + intent=assign + task_id + mentions=[B]，且不是普通聊天。
	total, assigns := countAssignMessages(t, m, g.ID)
	if total != 1 || len(assigns) != 1 {
		t.Fatalf("messages = %d, assign = %d", total, len(assigns))
	}
	msg := assigns[0]
	if msg.Kind != agenthub.KindText || msg.Sender.Kind != agenthub.SenderAgent ||
		msg.Sender.AgentID != a.ID || msg.TaskID != task.ID || msg.Text != "写一份三家价格对比" {
		t.Fatalf("assign message = %+v", msg)
	}
	if len(msg.Mentions) != 1 || msg.Mentions[0] != b.ID {
		t.Fatalf("mentions = %v", msg.Mentions)
	}
	if msg.Seq != task.Origin.MessageSeq {
		t.Fatalf("message seq = %d, task origin seq = %d", msg.Seq, task.Origin.MessageSeq)
	}
	// 路由回执（B 被 targeting）：routing 消息 + activity。
	acts, err := st.ReadActivity(context.Background(), g.ID, 0, 50)
	if err != nil || len(acts) != 1 || acts[0].AfterSeq != msg.Seq+1 {
		t.Fatalf("activity = %+v, %v", acts, err)
	}

	// 事件顺序：hub_message → hub_task_update{queued} → hub_task_update{dispatched}。
	events := readEvents(t, buf)
	iMsg := lastIndexOfEvent(events, "hub_message", func(ev map[string]any) bool {
		m, _ := ev["message"].(map[string]any)
		return m != nil && m["intent"] == string(agenthub.IntentAssign)
	})
	iQueued := lastIndexOfEvent(events, "hub_task_update", func(ev map[string]any) bool {
		return ev["status"] == string(agenthub.TaskQueued)
	})
	iDispatched := lastIndexOfEvent(events, "hub_task_update", func(ev map[string]any) bool {
		return ev["status"] == string(agenthub.TaskDispatched)
	})
	if iMsg < 0 || iQueued < 0 || iDispatched < 0 {
		t.Fatalf("事件缺失: message=%d queued=%d dispatched=%d\n%s", iMsg, iQueued, iDispatched, buf.String())
	}
	if !(iMsg < iQueued && iQueued < iDispatched) {
		t.Fatalf("事件顺序错: message=%d queued=%d dispatched=%d", iMsg, iQueued, iDispatched)
	}
	if ev := events[iDispatched]; ev["task_id"] != string(task.ID) || ev["target_kind"] != agenthub.TargetAgent {
		t.Fatalf("hub_task_update payload = %+v", ev)
	}

	// 派发边界：B 的请求里必须有 hub_task header（P3b-05 据此结算）。
	deadline := time.Now().Add(20 * time.Second)
	for fake.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	req := fake.lastRequest()
	if req == nil {
		t.Fatal("目标群会话没有被唤醒（provider 从未调用）")
	}
	headers := 0
	for _, msg := range req.Messages {
		for _, c := range msg.Content {
			if c.Type != core.ContentTypeHubTask {
				continue
			}
			headers++
			h, err := agenthub.ParseTaskHeader(c.Content)
			if err != nil || h.TaskID != task.ID {
				t.Fatalf("hub_task header = %+v, %v", h, err)
			}
		}
	}
	if headers != 1 {
		t.Fatalf("hub_task header 块 = %d, want 1", headers)
	}
	if bsB.s.PendingCount() != 0 {
		t.Fatalf("目标会话还有 %d 条待处理输入：一次指派只应投一条", bsB.s.PendingCount())
	}
}

func TestAssignTaskBusyLeavesQueuedAndCapsPair(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "BusyA", true)
	b := createDispatchAgent(t, m, buf, 2, "BusyB", true)
	g := assignGroup(t, m, buf, 3, "忙群", map[agenthub.AgentID]string{a.ID: "owner", b.ID: "member"})

	fake := &evalProvider{reply: "不该跑"}
	bsB, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	setGroupFakeLoop(t, m, bsB, g, string(b.ID), fake)
	// 模拟 B 正在跑一轮普通群回复：群轮闩已被占。
	bsB.hubGroupBusy.Store(true)

	tool := assignTool(t, m, g, a.ID)
	for i, text := range []string{"第一件", "第二件"} {
		res, err := callAssign(t, tool, map[string]any{"to_agent": string(b.ID), "instruction": text})
		if err != nil {
			t.Fatalf("第 %d 个在途任务: %v", i+1, err)
		}
		task := taskOf(t, res)
		if task.Status != agenthub.TaskQueued {
			t.Fatalf("忙时 status = %q, want queued", task.Status)
		}
		if res["dispatched"] == true {
			t.Fatalf("忙时不应标记 dispatched: %+v", res)
		}
	}
	// 第三个同对任务：ErrTooConcurrent，且不落任务/消息。
	beforeTasks := countTasks(t, m)
	beforeMsgs, _ := countAssignMessages(t, m, g.ID)
	_, err = callAssign(t, tool, map[string]any{"to_agent": string(b.ID), "instruction": "第三件"})
	if !errors.Is(err, agenthub.ErrTooConcurrent) {
		t.Fatalf("第三个同对任务 = %v, want ErrTooConcurrent", err)
	}
	if got := countTasks(t, m); got != beforeTasks {
		t.Fatalf("被拒的第三个任务落账了：%d → %d", beforeTasks, got)
	}
	if got, _ := countAssignMessages(t, m, g.ID); got != beforeMsgs {
		t.Fatalf("被拒的第三个任务写了群消息：%d → %d", beforeMsgs, got)
	}
	// 忙时不注入第二份快照：目标会话既没有新输入，也没跑过 provider。
	if bsB.s.PendingCount() != 0 {
		t.Fatalf("目标会话收到 %d 条待处理输入，want 0", bsB.s.PendingCount())
	}
	if fake.callCount() != 0 {
		t.Fatalf("忙时目标被唤醒 %d 次", fake.callCount())
	}
	if !bsB.hubGroupBusy.Load() {
		t.Fatal("忙闩被误清：普通群轮会被第二个快照插队")
	}

	// 账本/队列：两条 queued 都在队列里。
	q, err := m.hubQueueFor()
	if err != nil {
		t.Fatal(err)
	}
	target := agenthub.TaskTarget{Kind: agenthub.TargetAgent, AgentID: b.ID, GroupID: g.ID,
		SessionID: hubSessionID(string(b.ID), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID})}
	if n := q.QueuedCountFor(target); n != 2 {
		t.Fatalf("queued = %d, want 2", n)
	}

	// 目标空闲后（闩释放）泵出队首：同一个目标只派发一条。
	bsB.hubGroupBusy.Store(false)
	pumped, err := q.Pump(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if pumped == nil || pumped.Status != agenthub.TaskDispatched {
		t.Fatalf("空闲 pump = %+v", pumped)
	}
	deadline := time.Now().Add(20 * time.Second)
	for fake.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fake.callCount() != 1 {
		t.Fatalf("泵出后 provider 调用 = %d, want 1（一次一个）", fake.callCount())
	}
}

func TestAssignTaskRejectsInvalidTargets(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "RejectA", true)
	b := createDispatchAgent(t, m, buf, 2, "RejectB", true)
	c := createDispatchAgent(t, m, buf, 3, "RejectC", true)
	outsider := createDispatchAgent(t, m, buf, 4, "Outsider", true)
	noDispatch := createDispatchAgent(t, m, buf, 5, "NoDispatch", false)
	g := assignGroup(t, m, buf, 6, "拒绝群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member", c.ID: "member", noDispatch.ID: "member",
	})
	tool := assignTool(t, m, g, a.ID)

	// 自己。
	if _, err := callAssign(t, tool, map[string]any{"to_agent": string(a.ID), "instruction": "自己做"}); err == nil {
		t.Fatal("派给自己必须被拒")
	}
	// 非成员。
	if _, err := callAssign(t, tool, map[string]any{"to_agent": string(outsider.ID), "instruction": "外人"}); err == nil {
		t.Fatal("非成员必须被拒")
	}
	// 未知成员名。
	if _, err := callAssign(t, tool, map[string]any{"to_agent": "不存在的人", "instruction": "x"}); err == nil {
		t.Fatal("未知成员名必须被拒")
	}
	// CanDispatch=false 的发起者：直接走 manager 路径（DM/群会话都装不出这种引擎）。
	if _, _, _, err := m.assignHubTask(context.Background(), hubAssignRequest{
		GroupID: g.ID, From: noDispatch.ID, To: string(b.ID), Instruction: "没权限",
	}); !errors.Is(err, agenthub.ErrForbidden) {
		t.Fatalf("CanDispatch=false = %v, want ErrForbidden", err)
	}
	// 非成员发起者。
	if _, _, _, err := m.assignHubTask(context.Background(), hubAssignRequest{
		GroupID: g.ID, From: outsider.ID, To: string(b.ID), Instruction: "外人派活",
	}); err == nil {
		t.Fatal("非成员发起者必须被拒")
	}
	// 不存在的产物 id。
	if _, err := callAssign(t, tool, map[string]any{
		"to_agent": string(b.ID), "instruction": "带坏产物", "artifact_ids": []string{"art_missing"},
	}); err == nil {
		t.Fatal("不存在的产物必须被拒")
	}
	// 软删成员：从群里删之前先软删档案。
	if ok, errMsg, _ := hubCmd(t, m, buf, 7, "hub_agent_delete", map[string]any{"agent_id": string(c.ID)}); !ok {
		t.Fatalf("soft delete: %s", errMsg)
	}
	if _, err := callAssign(t, tool, map[string]any{"to_agent": string(c.ID), "instruction": "给已删除的人"}); err == nil {
		t.Fatal("已删除成员必须被拒")
	}

	// 所有拒绝都不留任务/消息。
	if n := countTasks(t, m); n != 0 {
		t.Fatalf("拒绝路径留下了 %d 条任务", n)
	}
	if total, _ := countAssignMessages(t, m, g.ID); total != 0 {
		t.Fatalf("拒绝路径留下了 %d 条群消息", total)
	}
}

func TestAssignTaskMemberNameAndIdempotency(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "NameA", true)
	b := createDispatchAgent(t, m, buf, 2, "NameB", true)
	c := createDispatchAgent(t, m, buf, 4, "NameC", true)
	g := assignGroup(t, m, buf, 3, "幂等群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member", c.ID: "member",
	})
	// 目标忙：稳定停在 queued，避免 run 收尾影响断言。
	bsB, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	bsB.hubGroupBusy.Store(true)
	tool := assignTool(t, m, g, a.ID)

	args := map[string]any{
		"to_agent": "NameB", "instruction": "按名字派活", "client_msg_id": "retry-1",
	}
	first, err := callAssign(t, tool, args)
	if err != nil {
		t.Fatalf("按成员名指派: %v", err)
	}
	firstTask := taskOf(t, first)
	if firstTask.Target.AgentID != b.ID {
		t.Fatalf("按名字解析到 %s, want %s", firstTask.Target.AgentID, b.ID)
	}
	// 同一 client_msg_id 重试：同一任务、同一条消息。
	second, err := callAssign(t, tool, args)
	if err != nil {
		t.Fatal(err)
	}
	secondTask := taskOf(t, second)
	if secondTask.ID != firstTask.ID {
		t.Fatalf("重试建了新任务: %s vs %s", secondTask.ID, firstTask.ID)
	}
	if n := countTasks(t, m); n != 1 {
		t.Fatalf("tasks = %d, want 1", n)
	}
	if total, assigns := countAssignMessages(t, m, g.ID); total != 1 || len(assigns) != 1 {
		t.Fatalf("messages = %d, assign = %d", total, len(assigns))
	}
	// 另一个发起者用同一个 client_msg_id 派给**同一个目标**是独立请求：
	// 幂等键按 (from,to) 作用域，聚合到 (to, client_msg_id) 会把两个人的指派并成一条
	// （P3b-03 N5）。
	cTask, _, _, err := m.assignHubTask(context.Background(), hubAssignRequest{
		GroupID: g.ID, From: c.ID, To: string(b.ID), Instruction: "另一个发起者派活",
		ClientMsgID: "retry-1",
	})
	if err != nil {
		t.Fatalf("C→B 指派: %v", err)
	}
	if cTask.ID == firstTask.ID {
		t.Fatalf("C→B 命中了 A→B 的任务 %s：幂等键没有按 from 作用域", cTask.ID)
	}
	if n := countTasks(t, m); n != 2 {
		t.Fatalf("tasks = %d, want 2（C→B 不能与 A→B 撞键）", n)
	}
	if total, assigns := countAssignMessages(t, m, g.ID); total != 2 || len(assigns) != 2 {
		t.Fatalf("messages = %d, assign = %d, want 2/2", total, len(assigns))
	}
}

func TestAssignTaskGroupScopedToCurrentSession(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "ScopeA", true)
	b := createDispatchAgent(t, m, buf, 2, "ScopeB", true)
	g := assignGroup(t, m, buf, 3, "作用域群", map[agenthub.AgentID]string{a.ID: "owner", b.ID: "member"})

	// DM 会话没有 assign_task：群作用域才有可指派的成员。
	dm, err := m.hubDMSession(string(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dm.hubEngine.GetTool(context.Background(), "assign_task"); err == nil {
		t.Fatal("DM 会话不应注册 assign_task")
	}

	bsB, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	bsB.hubGroupBusy.Store(true)
	tool := assignTool(t, m, g, a.ID)
	// 模型塞一个 group_id 参数也不能换群：群来自会话作用域。
	res, err := callAssign(t, tool, map[string]any{
		"group_id": "grp_elsewhere", "to_agent": string(b.ID), "instruction": "别的群",
	})
	if err != nil {
		t.Fatal(err)
	}
	task := taskOf(t, res)
	if task.Target.GroupID != g.ID || task.Origin.GroupID != g.ID {
		t.Fatalf("跨群泄漏: target=%s origin=%s", task.Target.GroupID, task.Origin.GroupID)
	}
}

// assignRound 是本轮 hub_round 事件里的路由结果（只取断言需要的字段）。
type assignRound struct {
	Mentions   []string
	Targets    []string
	MentionAll bool
}

// assignRoundOf 取某个群最近一次 hub_round 的路由结果。
func assignRoundOf(t *testing.T, buf *syncBuf, gid agenthub.GroupID) assignRound {
	t.Helper()
	strs := func(v any) []string {
		items, _ := v.([]any)
		var list []string
		for _, item := range items {
			if s, ok := item.(string); ok {
				list = append(list, s)
			}
		}
		return list
	}
	var out assignRound
	found := false
	for _, ev := range readEvents(t, buf) {
		if ev["event_type"] != "hub_round" || ev["group_id"] != string(gid) {
			continue
		}
		raw, ok := ev["round"].(map[string]any)
		if !ok {
			continue
		}
		out = assignRound{
			Mentions:   strs(raw["mentions"]),
			Targets:    strs(raw["targets"]),
			MentionAll: raw["mention_all"] == true,
		}
		found = true
	}
	if !found {
		t.Fatalf("没有 hub_round 事件（群 %s）:\n%s", gid, buf.String())
	}
	return out
}

// P3b-03 R1：路由只看结构化 mentions=[B]。instruction 正文里的 @C 是给 B 的正文，
// 不能把 C 也拉进本轮（变异：把正文交给路由 → mentions/hub_round 出现 C → 本测试红）。
func TestAssignTaskRoutesByStructuredTargetOnly(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "RouteA", true)
	b := createDispatchAgent(t, m, buf, 2, "RouteB", true)
	c := createDispatchAgent(t, m, buf, 3, "RouteC", true)
	g := assignGroup(t, m, buf, 4, "结构化路由群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member", c.ID: "member",
	})
	fakeB := &evalProvider{reply: "收到"}
	fakeC := &evalProvider{reply: "我不该被叫"}
	bsB, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	setGroupFakeLoop(t, m, bsB, g, string(b.ID), fakeB)
	bsC, err := m.hubGroupSession(string(c.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	setGroupFakeLoop(t, m, bsC, g, string(c.ID), fakeC)

	tool := assignTool(t, m, g, a.ID)
	buf.Reset()
	const instruction = "@RouteC 别插手，帮我查价格"
	res, err := callAssign(t, tool, map[string]any{
		"to_agent": string(b.ID), "instruction": instruction,
	})
	if err != nil {
		t.Fatalf("assign_task: %v", err)
	}
	task := taskOf(t, res)

	// ① 任务只指向 B：没有第二条任务，也没有 C。
	if task.Target.Kind != agenthub.TargetAgent || task.Target.AgentID != b.ID {
		t.Fatalf("target = %+v, want only B", task.Target)
	}
	st := mustHubStore(t, m)
	tasks, err := st.ListTasks(context.Background(), agenthub.TaskFilter{})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks = %d, %v; want 1", len(tasks), err)
	}

	// ② 指派消息：正文原样保留，mentions 只有 B。
	total, assigns := countAssignMessages(t, m, g.ID)
	if total != 1 || len(assigns) != 1 {
		t.Fatalf("messages = %d, assign = %d", total, len(assigns))
	}
	msg := assigns[0]
	if msg.Text != instruction {
		t.Fatalf("正文被改写: %q", msg.Text)
	}
	if msg.MentionAll || len(msg.Mentions) != 1 || msg.Mentions[0] != b.ID {
		t.Fatalf("mentions = %v (all=%v), want [%s]", msg.Mentions, msg.MentionAll, b.ID)
	}

	// ③ 路由回执（hub_round）也只含 B：C 没有进本轮目标。
	round := assignRoundOf(t, buf, g.ID)
	if round.MentionAll || len(round.Mentions) != 1 || round.Mentions[0] != string(b.ID) ||
		len(round.Targets) != 1 || round.Targets[0] != string(b.ID) {
		t.Fatalf("round = %+v, want only B", round)
	}

	// ④ C 没有被唤醒：没有待处理输入、没占闩、provider 从未被调用、
	// 也没有任何事件落在 C 的会话上。（assign 本身不调用 wakeGroupAgent，
	// 所以这组断言是对照；真正杀「按正文路由」变异的是 ②③。）
	if bsC.s.PendingCount() != 0 || bsC.hubGroupBusy.Load() || bsC.hubGroupOwner.Load() != 0 {
		t.Fatalf("C 被唤醒: pending=%d busy=%v owner=%d",
			bsC.s.PendingCount(), bsC.hubGroupBusy.Load(), bsC.hubGroupOwner.Load())
	}
	if fakeC.callCount() != 0 {
		t.Fatalf("C 的 provider 被调用 %d 次", fakeC.callCount())
	}
	for _, ev := range readEvents(t, buf) {
		if sid, _ := ev["session_id"].(string); sid == bsC.id {
			t.Fatalf("assign 不该在 C 的会话上产生事件: %+v", ev)
		}
	}

	// 对照：B 真的被唤醒并拿到 hub_task header。
	deadline := time.Now().Add(20 * time.Second)
	for fakeB.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	req := fakeB.lastRequest()
	if req == nil {
		t.Fatal("B 没有被唤醒（provider 从未调用）")
	}
	headers := 0
	for _, msg := range req.Messages {
		for _, ct := range msg.Content {
			if ct.Type == core.ContentTypeHubTask {
				headers++
			}
		}
	}
	if headers != 1 {
		t.Fatalf("B 的 hub_task header 块 = %d, want 1", headers)
	}
}

// P3b-03 R2：写指派消息失败必须回滚已建任务 —— 不能留「有任务、无消息」的指派。
func TestAssignTaskMessageWriteFailureRollsBackTask(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "RollbackA", true)
	b := createDispatchAgent(t, m, buf, 2, "RollbackB", true)
	g := assignGroup(t, m, buf, 3, "回滚群", map[agenthub.AgentID]string{a.ID: "owner", b.ID: "member"})
	bsB, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	bsB.hubGroupBusy.Store(true) // 目标忙：任何残留任务都会留在 queued，便于断言
	var createdID agenthub.TaskID
	hubAssignMessageHook = func(id agenthub.TaskID) error {
		createdID = id
		return errors.New("注入：群消息写失败")
	}
	t.Cleanup(func() { hubAssignMessageHook = nil })

	tool := assignTool(t, m, g, a.ID)
	buf.Reset()
	_, err = callAssign(t, tool, map[string]any{
		"to_agent": string(b.ID), "instruction": "帮我查价格",
	})
	if err == nil {
		t.Fatal("写消息失败必须回给调用方")
	}
	if !strings.Contains(err.Error(), "写指派消息失败") {
		t.Fatalf("err = %v, want 写指派消息失败", err)
	}
	if createdID == "" {
		t.Fatal("屏障没有拿到已建任务 id（任务可能根本没建）")
	}
	st := mustHubStore(t, m)
	got, err := st.LoadTask(context.Background(), createdID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agenthub.TaskCanceled {
		t.Fatalf("回滚后 status = %q, want canceled", got.Status)
	}
	// 该对 0 条在途任务（账本只留一条 canceled 审计行）/ 队列 0 条 / 群 0 条消息。
	tasks, err := st.ListTasks(context.Background(), agenthub.TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != createdID {
		t.Fatalf("回滚后账本 = %d 条（want 只有 1 条 canceled 审计行 %s）", len(tasks), createdID)
	}
	for _, task := range tasks {
		if !task.Status.Terminal() {
			t.Fatalf("回滚后仍有在途任务: %s %s", task.ID, task.Status)
		}
	}
	q, err := m.hubQueueFor()
	if err != nil {
		t.Fatal(err)
	}
	if n := q.QueuedCountFor(got.Target); n != 0 {
		t.Fatalf("回滚后队列还有 %d 条", n)
	}
	if total, assigns := countAssignMessages(t, m, g.ID); total != 0 || len(assigns) != 0 {
		t.Fatalf("回滚后群里 messages=%d assign=%d, want 0/0", total, len(assigns))
	}
}

// P3b-03 N3：回滚取消失败不能静默（任务可能在写消息窗口里被别的路径结算）：
// 必须落一条任务级 hub_alert，否则调用方只看到「写消息失败」。
func TestAssignTaskRollbackCancelFailureEmitsAlert(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "AlertA", true)
	b := createDispatchAgent(t, m, buf, 2, "AlertB", true)
	g := assignGroup(t, m, buf, 3, "告警群", map[agenthub.AgentID]string{a.ID: "owner", b.ID: "member"})
	bsB, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	bsB.hubGroupBusy.Store(true)
	st := mustHubStore(t, m)
	var createdID agenthub.TaskID
	hubAssignMessageHook = func(id agenthub.TaskID) error {
		createdID = id
		// 模拟写消息窗口里任务已被取消：回滚的 CancelQueued 会 ErrInvalid。
		if _, terr := agenthub.NewLedger(st).Transition(context.Background(), id, agenthub.TaskCanceled,
			agenthub.TransitionOptions{At: time.Now().UTC(), Error: "并发取消"}); terr != nil {
			t.Errorf("模拟并发取消失败: %v", terr)
		}
		return errors.New("注入：群消息写失败")
	}
	t.Cleanup(func() { hubAssignMessageHook = nil })

	tool := assignTool(t, m, g, a.ID)
	buf.Reset()
	if _, err := callAssign(t, tool, map[string]any{
		"to_agent": string(b.ID), "instruction": "帮我查价格",
	}); err == nil {
		t.Fatal("写消息失败必须回给调用方")
	}
	found := false
	for _, ev := range readEvents(t, buf) {
		if ev["event_type"] == "hub_alert" && ev["kind"] == "assign_rollback_failed" &&
			ev["task_id"] == string(createdID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("回滚取消失败没有落 hub_alert（task=%s）:\n%s", createdID, buf.String())
	}
}

// P3b-03 N6：统一忙判据 + Ask 失败必须还闩。
// 群任务已投递（hubGroupBusy）但根 run 未起：状态词/闸门必须读成忙；Ask 失败则
// 闩与派发窗口标记立即归还 —— 否则该智能体会被永久标忙，队列再也不派。
func TestAgentDispatchAskFailureReleasesGroupLatch(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "LatchA", true)
	b := createDispatchAgent(t, m, buf, 2, "LatchB", true)
	g := assignGroup(t, m, buf, 3, "闩群", map[agenthub.AgentID]string{a.ID: "owner", b.ID: "member"})
	bsB, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}

	// 统一忙判据：闩被占（已 Ask、根 run 未起）→ running；释放 → idle。
	bsB.hubGroupBusy.Store(true)
	if busy := hubSessionBusy(bsB); !busy {
		t.Fatal("hubSessionBusy 漏了群轮闩：Ask→AgentStart 窗口会被读成空闲")
	}
	if status, where := m.hubAgentLiveStatus(string(b.ID), g.ID); status != hubToolStatusRunning || where != "group" {
		t.Fatalf("闩被占时状态 = %s/%s, want running/group", status, where)
	}
	bsB.hubGroupBusy.Store(false)
	if status, _ := m.hubAgentLiveStatus(string(b.ID), g.ID); status != hubToolStatusIdle {
		t.Fatalf("闩释放后状态 = %s, want idle", status)
	}

	// Ask 失败（会话已关闭）：闩与 hubDispatchPending 都必须归还。
	if err := bsB.s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	ops := newHubWorkspaceOps(m)
	if _, err := ops.AgentDispatch(context.Background(), agenthub.AgentDispatchReq{
		OriginAgent: a.ID,
		Target: agenthub.TaskTarget{
			Kind: agenthub.TargetAgent, AgentID: b.ID, GroupID: g.ID, SessionID: bsB.id,
		},
		Text: "投不出去的任务",
	}); err == nil {
		t.Fatal("已关闭的会话不应接受派发")
	}
	if bsB.hubGroupBusy.Load() || bsB.hubGroupOwner.Load() != 0 {
		t.Fatalf("Ask 失败后闩没还: busy=%v owner=%d", bsB.hubGroupBusy.Load(), bsB.hubGroupOwner.Load())
	}
	if bsB.hubDispatchPending.Load() {
		t.Fatal("Ask 失败后 hubDispatchPending 泄漏：该会话会被永久读成忙")
	}
	if status, _ := m.hubAgentLiveStatus(string(b.ID), g.ID); status != hubToolStatusIdle {
		t.Fatalf("Ask 失败后状态 = %s, want idle", status)
	}
}
