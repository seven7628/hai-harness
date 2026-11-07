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
	g := assignGroup(t, m, buf, 3, "幂等群", map[agenthub.AgentID]string{a.ID: "owner", b.ID: "member"})
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
	// 另一个发起者用同一个 client_msg_id 是独立请求（键按 (from,to) 作用域）。
	if _, _, _, err := m.assignHubTask(context.Background(), hubAssignRequest{
		GroupID: g.ID, From: b.ID, To: string(a.ID), Instruction: "反向派活",
		ClientMsgID: "retry-1",
	}); err != nil && !errors.Is(err, agenthub.ErrForbidden) {
		// B 的 can_dispatch=true；反向应成功。
		t.Fatalf("反向指派: %v", err)
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
