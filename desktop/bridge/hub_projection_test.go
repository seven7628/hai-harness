package main

// P3b-06：hub_group_list 左栏投影（未读/预览/任务结论）与 hub_mark_read（§6.5.7 / §7.1.1 / §7.1.3）。
//
// 覆盖：任务结果卡 → unread/结论；失败卡文案；mark_read 单调 + DM 复用；人发言
// 自动前移群游标；分页读日志不把 >500 条未读静默截断。全部走真实 FileStore
//（testManager 的临时目录），不碰网络；只有分页用 MemoryStore 提速。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
)

// projGroup 建一个 owner + member 的群。
func projGroup(t *testing.T, m *manager, buf *syncBuf, cmdID int, owner, member agenthub.AgentID) *agenthub.Group {
	t.Helper()
	return groupWithMembers(t, m, buf, cmdID, "投影群",
		struct {
			id   agenthub.AgentID
			role string
		}{owner, "owner"},
		struct {
			id   agenthub.AgentID
			role string
		}{member, "member"},
	)
}

// projTask 构造一张结果卡需要的账本快照（appendAgentTaskResultCard 只读这些字段）。
func projTask(id agenthub.TaskID, g *agenthub.Group, origin, target agenthub.AgentID,
	status agenthub.TaskStatus, summary, errText string) *agenthub.Task {
	task := &agenthub.Task{
		ID:     id,
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginGroup, GroupID: g.ID, AgentID: origin},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetAgent, AgentID: target, GroupID: g.ID},
		Title:  "价格表简报",
		Status: status,
		Error:  errText,
	}
	if summary != "" {
		task.Result = &agenthub.TaskResult{Summary: summary}
	}
	return task
}

func projAppendCard(t *testing.T, m *manager, task *agenthub.Task) {
	t.Helper()
	if _, err := m.appendAgentTaskResultCard(task); err != nil {
		t.Fatalf("append task_result card: %v", err)
	}
}

func projListRow(t *testing.T, m *manager, buf *syncBuf, cmdID int, gid agenthub.GroupID) map[string]any {
	t.Helper()
	ok, errMsg, data := hubCmd(t, m, buf, cmdID, "hub_group_list", map[string]any{})
	if !ok {
		t.Fatalf("hub_group_list: %s", errMsg)
	}
	groups, _ := data["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("groups = %v, want 1（groups 数组必须保持原样）", data["groups"])
	}
	if raw, _ := groups[0].(map[string]any); raw["id"] != string(gid) {
		t.Fatalf("listed group = %v, want %s", groups[0], gid)
	}
	progress, _ := data["progress"].(map[string]any)
	row, _ := progress[string(gid)].(map[string]any)
	if row == nil {
		t.Fatalf("progress[%s] missing: %v", gid, data["progress"])
	}
	return row
}

func projNum(t *testing.T, row map[string]any, key string) int64 {
	t.Helper()
	f, ok := row[key].(float64)
	if !ok {
		t.Fatalf("progress[%s] = %T(%v), want number", key, row[key], row[key])
	}
	return int64(f)
}

func projStr(t *testing.T, row map[string]any, key string) string {
	t.Helper()
	s, _ := row[key].(string)
	return s
}

// 任务结果卡进入左栏：未读 >0，预览/结论是「任务完成：…」，并且 groups 数组不变。
func TestHubGroupListProgressTaskResult(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "投影A")
	b := createHubAgent(t, m, buf, 2, "投影B")
	g := projGroup(t, m, buf, 3, a.ID, b.ID)

	// 人先说一句（hub_group_send 会前移已读，见下一个测试）；这条不能算未读。
	if ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "开始整理价格表",
	}); !ok {
		t.Fatalf("group_send: %s", errMsg)
	}
	projAppendCard(t, m, projTask("task_proj1", g, a.ID, b.ID, agenthub.TaskCompleted,
		"价格表出来了：三家", ""))

	row := projListRow(t, m, buf, 5, g.ID)
	if got := projNum(t, row, "unread"); got != 1 {
		t.Fatalf("unread = %d, want 1（人自己的消息不算）", got)
	}
	if got := projNum(t, row, "last_seq"); got != 2 {
		t.Fatalf("last_seq = %d, want 2", got)
	}
	if got := projStr(t, row, "last_sender"); got != string(agenthub.SenderAgent) {
		t.Fatalf("last_sender = %q", got)
	}
	if got := projStr(t, row, "last_text"); got != "价格表出来了：三家" {
		t.Fatalf("last_text = %q", got)
	}
	if got := projStr(t, row, "last_outcome"); got != "任务完成：价格表出来了：三家" {
		t.Fatalf("last_outcome = %q", got)
	}
	if got := projStr(t, row, "last_task_id"); got != "task_proj1" {
		t.Fatalf("last_task_id = %q", got)
	}
	if got := projStr(t, row, "last_task_status"); got != string(agenthub.TaskCompleted) {
		t.Fatalf("last_task_status = %q", got)
	}
	if got := projStr(t, row, "last_task_summary"); got != "价格表出来了：三家" {
		t.Fatalf("last_task_summary = %q", got)
	}
	if projStr(t, row, "last_at") == "" {
		t.Fatal("last_at 不能为空（左栏相对时间用）")
	}
}

// 失败卡的文案是「任务失败：<error>」（用错误原因，不是摘要）。
func TestHubGroupListProgressFailedTask(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "失败A")
	b := createHubAgent(t, m, buf, 2, "失败B")
	g := projGroup(t, m, buf, 3, a.ID, b.ID)

	projAppendCard(t, m, projTask("task_proj_fail", g, a.ID, b.ID, agenthub.TaskFailed,
		"只拿到两家", "工具超时"))

	row := projListRow(t, m, buf, 4, g.ID)
	if got := projNum(t, row, "unread"); got != 1 {
		t.Fatalf("unread = %d, want 1", got)
	}
	if got := projStr(t, row, "last_outcome"); got != "任务失败：工具超时" {
		t.Fatalf("last_outcome = %q, want 任务失败：工具超时", got)
	}
	if got := projStr(t, row, "last_task_status"); got != string(agenthub.TaskFailed) {
		t.Fatalf("last_task_status = %q", got)
	}
	if got := projStr(t, row, "last_text"); got != "只拿到两家" {
		t.Fatalf("last_text = %q（预览永远是卡正文）", got)
	}
}

// hub_mark_read：群游标单调（不能倒退），也复用 DM 游标；参数错误要说清楚。
func TestHubMarkReadMonotonic(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "已读A")
	b := createHubAgent(t, m, buf, 2, "已读B")
	g := projGroup(t, m, buf, 3, a.ID, b.ID)

	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	appendAgentText := func(text string) int64 {
		t.Helper()
		seq, err := st.AppendMessage(ctx, &agenthub.Message{
			GroupID: g.ID, Kind: agenthub.KindText,
			Sender: agenthub.Sender{Kind: agenthub.SenderAgent, AgentID: b.ID}, Text: text,
		})
		if err != nil {
			t.Fatal(err)
		}
		return seq
	}
	seq1 := appendAgentText("进展一")
	if row := projListRow(t, m, buf, 4, g.ID); projNum(t, row, "unread") != 1 {
		t.Fatalf("unread = %d, want 1", projNum(t, row, "unread"))
	}
	ok, errMsg, data := hubCmd(t, m, buf, 5, "hub_mark_read", map[string]any{
		"group_id": string(g.ID), "seq": seq1,
	})
	if !ok {
		t.Fatalf("mark_read: %s", errMsg)
	}
	if got := int64(data["seq"].(float64)); got != seq1 {
		t.Fatalf("mark_read seq = %d, want %d", got, seq1)
	}
	if row := projListRow(t, m, buf, 6, g.ID); projNum(t, row, "unread") != 0 {
		t.Fatalf("unread after mark = %d, want 0", projNum(t, row, "unread"))
	}

	// 再来一条 → 读到它 → 再请求旧位置：生效值仍是新的（不能倒退）。
	seq2 := appendAgentText("进展二")
	if ok, errMsg, _ := hubCmd(t, m, buf, 7, "hub_mark_read", map[string]any{
		"group_id": string(g.ID), "seq": seq2,
	}); !ok {
		t.Fatalf("mark_read seq2: %s", errMsg)
	}
	ok, errMsg, data = hubCmd(t, m, buf, 8, "hub_mark_read", map[string]any{
		"group_id": string(g.ID), "seq": seq1,
	})
	if !ok {
		t.Fatalf("backward mark_read: %s", errMsg)
	}
	if got := int64(data["seq"].(float64)); got != seq2 {
		t.Fatalf("backward mark_read returned %d, want %d", got, seq2)
	}
	if got, err := st.GroupReadSeq(ctx, g.ID); err != nil || got != seq2 {
		t.Fatalf("stored read seq = %d, %v; want %d", got, err, seq2)
	}

	// 参数校验：两个都给 / 都不给 / seq<=0 / 群不存在。
	for name, payload := range map[string]map[string]any{
		"both":    {"group_id": string(g.ID), "agent_id": string(a.ID), "seq": 1},
		"neither": {"seq": 1},
		"zero":    {"group_id": string(g.ID), "seq": 0},
		"unknown": {"group_id": "grp_missing", "seq": 1},
	} {
		if ok, errMsg, _ := hubCmd(t, m, buf, 90, "hub_mark_read", payload); ok || errMsg == "" {
			t.Fatalf("%s: ok=%v err=%q, want rejection", name, ok, errMsg)
		}
	}

	// DM 复用现有未读游标：mark_read(agent) 后 hub_agent_list 的 dm.unread 归零。
	for i := 0; i < 3; i++ {
		if _, err := st.AppendDirectMessage(ctx, a.ID, &agenthub.Message{
			Kind: agenthub.KindText, Sender: agenthub.Sender{Kind: agenthub.SenderHuman}, Text: "dm",
		}); err != nil {
			t.Fatal(err)
		}
	}
	ok, errMsg, data = hubCmd(t, m, buf, 9, "hub_mark_read", map[string]any{
		"agent_id": string(a.ID), "seq": 3,
	})
	if !ok {
		t.Fatalf("mark_read dm: %s", errMsg)
	}
	if got := int64(data["seq"].(float64)); got != 3 {
		t.Fatalf("dm mark_read seq = %d, want 3", got)
	}
	ok, errMsg, data = hubCmd(t, m, buf, 10, "hub_agent_list", map[string]any{})
	if !ok {
		t.Fatalf("agent_list: %s", errMsg)
	}
	dm, _ := data["dm"].(map[string]any)
	row, _ := dm[string(a.ID)].(map[string]any)
	if row == nil || int64(row["unread"].(float64)) != 0 {
		t.Fatalf("dm unread = %v, want 0", dm[string(a.ID)])
	}
}

// 人发消息 = 已看到这里：hub_group_send 成功即前移群已读游标（多端共享）。
func TestHubGroupListSendAdvancesReadSeq(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "发送A")
	g := groupWithMembers(t, m, buf, 2, "发送群", struct {
		id   agenthub.AgentID
		role string
	}{a.ID, "owner"})

	st := mustHubStore(t, m)
	ctx := context.Background()
	ok, errMsg, data := hubCmd(t, m, buf, 3, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "第一句",
	})
	if !ok {
		t.Fatalf("send: %s", errMsg)
	}
	seq := int64(data["seq"].(float64))
	if got, err := st.GroupReadSeq(ctx, g.ID); err != nil || got != seq {
		t.Fatalf("read seq after send = %d, %v; want %d", got, err, seq)
	}
	// 别人回一条 → 未读 1；人再发一条 → 自然归零。
	if _, err := st.AppendMessage(ctx, &agenthub.Message{
		GroupID: g.ID, Kind: agenthub.KindText,
		Sender: agenthub.Sender{Kind: agenthub.SenderAgent, AgentID: a.ID}, Text: "收到",
	}); err != nil {
		t.Fatal(err)
	}
	if row := projListRow(t, m, buf, 4, g.ID); projNum(t, row, "unread") != 1 {
		t.Fatalf("unread = %d, want 1", projNum(t, row, "unread"))
	}
	ok, errMsg, data = hubCmd(t, m, buf, 5, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "第二句",
	})
	if !ok {
		t.Fatalf("second send: %s", errMsg)
	}
	seq = int64(data["seq"].(float64))
	if got, _ := st.GroupReadSeq(ctx, g.ID); got != seq {
		t.Fatalf("read seq after second send = %d, want %d", got, seq)
	}
	if row := projListRow(t, m, buf, 6, g.ID); projNum(t, row, "unread") != 0 {
		t.Fatalf("unread after own message = %d, want 0", projNum(t, row, "unread"))
	}
}

// 分页读日志：未读超过一页（500）时不能静默截断；读到 readSeq 就停。
func TestHubGroupListUnreadPagingBeyondPage(t *testing.T) {
	st := agenthub.NewMemoryStore()
	ctx := context.Background()
	g := &agenthub.Group{ID: "grp_paging", Name: "分页群"}
	if err := st.SaveGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	const total = hubGroupProgressPage + 25
	for i := 0; i < total; i++ {
		if _, err := st.AppendMessage(ctx, &agenthub.Message{
			GroupID: g.ID, Kind: agenthub.KindText,
			Sender: agenthub.Sender{Kind: agenthub.SenderAgent, AgentID: "ag_bbbbbbbb"},
			Text:   fmt.Sprintf("消息 %d", i+1),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 未读游标 0：要翻完所有页，525 条都算未读（只读一页会得到 500）。
	msgs, err := readGroupMessagesSince(ctx, st, g.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != total {
		t.Fatalf("read %d messages, want %d", len(msgs), total)
	}
	if got := agenthub.BuildGroupPerception(msgs, 0, 0).Unread; got != total {
		t.Fatalf("unread = %d, want %d（不能把 501+ 静默截成 500）", got, total)
	}
	// 游标在最新一页内：只读一页就够，未读正好 5。
	readSeq := int64(total - 5)
	msgs, err = readGroupMessagesSince(ctx, st, g.ID, readSeq)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != hubGroupProgressPage {
		t.Fatalf("read %d messages, want one page", len(msgs))
	}
	if got := agenthub.BuildGroupPerception(msgs, readSeq, 0).Unread; got != 5 {
		t.Fatalf("unread = %d, want 5", got)
	}
	// 预览始终是最新一条（跨页也不丢）。
	if got := agenthub.BuildGroupPerception(msgs, readSeq, 0).LastText; !strings.HasPrefix(got, "消息 ") {
		t.Fatalf("last_text = %q", got)
	}
}
