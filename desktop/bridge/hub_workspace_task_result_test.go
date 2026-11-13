package main

// 会话 3：群内智能体用 session_send 把任务派给 Project 会话 —— 项目主会话 AgentEnd
// 之后，结果必须回到**发起者**的群会话（task_result 进模型上下文）并在群日志里留一张
// 可见结果卡（归属：哪个 workspace / 哪个 session / 哪个发起智能体）。
//
// 全部确定性：目标项目会话与群会话都装 fake provider，不碰网络；等待用轮询。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

const gwProjectSummary = "三家对比表已完成：都支持导出，定价见 out/pricing.md"

type gwFixture struct {
	m    *manager
	buf  *syncBuf
	g    *agenthub.Group
	a    agenthub.AgentProfile
	b    agenthub.AgentProfile
	bsA  *bridgeSession
	bsB  *bridgeSession
	fa   *convProvider
	fb   *convProvider
	ws   string
	sid  string
	task *agenthub.Task
}

// newGroupWorkspaceFixture 建群 + 项目工作区/会话，让群内 A 用 session_send 真派一条
// Project 任务，并等到它结算 + 回推完成（账本 delivered）。
func newGroupWorkspaceFixture(t *testing.T) *gwFixture {
	t.Helper()
	m, buf := testManager(t)
	root := t.TempDir()
	a := createWorkspaceAgent(t, m, buf, 1, "群派发A", []string{root})
	b := createWorkspaceAgent(t, m, buf, 2, "旁观B", []string{root})
	g := assignGroup(t, m, buf, 3, "项目群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member",
	})
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	ws := filepath.Join(root, "proj")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := m.wsReg.add(ws); err != nil {
		t.Fatal(err)
	}
	// 群会话：A 是发起者，B 是旁观者（证明结果卡不唤醒别人、不产生 @ 路由）。
	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))
	fa := newConvProvider("收到项目结果，我来总结")
	setGroupFakeLoop(t, m, bsA, g, string(a.ID), fa)
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	fb := newConvProvider("我不该被叫醒")
	setGroupFakeLoop(t, m, bsB, g, string(b.ID), fb)
	// 项目主会话：确定性 provider 真跑一轮，AgentEnd 走生产的 emit → 结算路径。
	target, err := m.create(ws, "s-proj", "")
	if err != nil {
		t.Fatal(err)
	}
	target.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(&fixedEvalProvider{reply: gwProjectSummary})))

	tool, err := bsA.hubEngine.GetTool(context.Background(), "session_send")
	if err != nil {
		t.Fatalf("群会话里没有 session_send: %v", err)
	}
	out, err := tool.Call(context.Background(), "session_send",
		`{"workspace":`+jsonString(ws)+`,"session_id":"s-proj","text":"做一份三家对比表"}`)
	if err != nil {
		t.Fatalf("session_send: %v", err)
	}
	var sent struct {
		TaskID string `json:"task_id"`
	}
	if json.Unmarshal([]byte(out), &sent) != nil || sent.TaskID == "" {
		t.Fatalf("session_send = %s", out)
	}
	id := agenthub.TaskID(sent.TaskID)
	// 回推是异步的（通知簿）：delivered 说明 MarkNotified 已经落账，回推已完成。
	convWait(t, "Project 任务结算并回推完成", func() bool {
		return p3bLoadTask(t, m, id).Status == agenthub.TaskDelivered
	})
	waitResultNotifyDrained(t, m)
	return &gwFixture{m: m, buf: buf, g: g, a: a, b: b, bsA: bsA, bsB: bsB,
		fa: fa, fb: fb, ws: ws, sid: "s-proj", task: p3bLoadTask(t, m, id)}
}

// gwGroupRounds 统计某群的 @ 路由事件数（结果卡不是触发消息，必须为 0）。
func gwGroupRounds(buf *syncBuf, gid agenthub.GroupID) int {
	n := 0
	for _, ev := range readAllLines(buf) {
		if ev["event_type"] == "hub_round" && ev["group_id"] == string(gid) {
			n++
		}
	}
	return n
}

func TestGroupWorkspaceTaskResultPushedToOrigin(t *testing.T) {
	f := newGroupWorkspaceFixture(t)
	task := f.task

	// 账本归属：群内发起 → Project 工作区会话。
	if task.Origin.Kind != agenthub.OriginGroup || task.Origin.GroupID != f.g.ID || task.Origin.AgentID != f.a.ID {
		t.Fatalf("origin = %+v, want group/%s/%s", task.Origin, f.g.ID, f.a.ID)
	}
	if task.Target.Kind != agenthub.TargetWorkspace || task.Target.Workspace != f.ws || task.Target.SessionID != f.sid {
		t.Fatalf("target = %+v", task.Target)
	}
	if task.Result == nil || !strings.Contains(task.Result.Summary, "三家对比表已完成") {
		t.Fatalf("result = %+v", task.Result)
	}

	// 1) 群日志：恰好一张 task_result 卡，署名**发起者** A（它在群里，UI 能画头像）。
	msgs := p3bMessages(t, f.m, f.g.ID)
	cards := p3bTaskResultCards(msgs, task.ID)
	if len(cards) != 1 {
		t.Fatalf("task_result 卡 = %d, want 1（msgs=%+v）", len(cards), msgs)
	}
	if n := p3bKindCount(msgs, agenthub.KindTaskResult); n != 1 {
		t.Fatalf("群里 task_result 消息 = %d, want 1（msgs=%+v）", n, msgs)
	}
	card := cards[0]
	if card.Sender.Kind != agenthub.SenderAgent || card.Sender.AgentID != f.a.ID {
		t.Fatalf("card sender = %+v, want 发起者 %s", card.Sender, f.a.ID)
	}
	if !strings.Contains(card.Text, "三家对比表已完成") {
		t.Fatalf("card text 缺项目会话摘要: %q", card.Text)
	}
	// 群里没有任务详情面板：正文必须先说清是哪个 workspace / session 跑出来的。
	if !strings.Contains(card.Text, f.ws) || !strings.Contains(card.Text, f.sid) {
		t.Fatalf("card text 看不出项目归属: %q", card.Text)
	}
	// round=nil：不 @ 任何人、不走路由。
	if len(card.Mentions) != 0 || card.MentionAll || card.RoundID != "" || card.ReplyToSeq != 0 {
		t.Fatalf("结果卡不能带 mentions/路由: %+v", card)
	}

	// 2) refs：workspace / session / 发起智能体三件套 + 账本快照。
	if got := p3bRefString(t, card.Refs, "client_msg_id"); got != "task_result:"+string(task.ID) {
		t.Fatalf("refs client_msg_id = %q", got)
	}
	if got := p3bRefString(t, card.Refs, "origin_kind"); got != agenthub.OriginGroup {
		t.Fatalf("refs origin_kind = %q, want group", got)
	}
	if got := p3bRefString(t, card.Refs, "origin_agent_id"); got != string(f.a.ID) {
		t.Fatalf("refs origin_agent_id = %q", got)
	}
	// session_send 建账不带 agent_name：名字必须回落到 profile。
	if got := p3bRefString(t, card.Refs, "origin_agent_name"); got != "群派发A" {
		t.Fatalf("refs origin_agent_name = %q, want 群派发A", got)
	}
	if got := p3bRefString(t, card.Refs, "target_kind"); got != agenthub.TargetWorkspace {
		t.Fatalf("refs target_kind = %q, want workspace", got)
	}
	if got := p3bRefString(t, card.Refs, "target_workspace"); got != f.ws {
		t.Fatalf("refs target_workspace = %q, want %q", got, f.ws)
	}
	if got := p3bRefString(t, card.Refs, "target_session_id"); got != f.sid {
		t.Fatalf("refs target_session_id = %q, want %q", got, f.sid)
	}
	if got := p3bRefString(t, card.Refs, "task_id"); got != string(task.ID) {
		t.Fatalf("refs task_id = %q", got)
	}
	if got := p3bRefString(t, card.Refs, "status"); got != string(agenthub.TaskCompleted) {
		t.Fatalf("refs status = %q, want completed", got)
	}
	if got := p3bRefString(t, card.Refs, "title"); got != "做一份三家对比表" {
		t.Fatalf("refs title = %q", got)
	}
	for _, key := range []string{"usage", "artifacts", "error", "duration_ms"} {
		if _, has := card.Refs[key]; !has {
			t.Fatalf("refs 缺 %s: %+v", key, card.Refs)
		}
	}

	// 3) 唤醒层：发起者的群会话收到一次 task_result（模型上下文），只推一次。
	if !f.bsA.s.HasTaskResult(string(task.ID)) {
		t.Fatal("发起者的群会话没有收到 task_result")
	}
	convWait(t, "发起者被唤醒并开跑", func() bool { return f.fa.callCount() >= 1 })
	if n := p3bEventCount(t, f.buf, string(events.TaskResultDeliveredType), task.ID); n != 1 {
		t.Fatalf("task_result_delivered = %d, want 1", n)
	}
	convWait(t, "发起者上下文里有项目摘要", func() bool {
		return len(p3bTaskResultTexts(f.bsA, task.ID)) == 1
	})
	if texts := p3bTaskResultTexts(f.bsA, task.ID); !strings.Contains(texts[0], "三家对比表已完成") {
		t.Fatalf("结果正文 = %q", texts[0])
	}

	// 4) 不唤醒旁观者、不产生 @ 路由。
	if n := f.fb.callCount(); n != 0 {
		t.Fatalf("旁观者被叫醒 %d 次（结果卡不能路由 @）", n)
	}
	if f.bsB.s.HasTaskResult(string(task.ID)) {
		t.Fatal("旁观者收到 task_result：只应唤醒发起者")
	}
	if n := gwGroupRounds(f.buf, f.g.ID); n != 0 {
		t.Fatalf("结果卡产生了 %d 条 @ 路由（hub_round）, want 0", n)
	}
}

// 同一 run 的重复结算 + 重复排通知都不能写第二张卡、推第二份结果。
func TestGroupWorkspaceTaskResultCardIdempotent(t *testing.T) {
	f := newGroupWorkspaceFixture(t)
	task := f.task
	runID := task.Dispatch.RunID
	if runID == "" {
		t.Fatal("结算后账本里没有 run_id：无法复现重放")
	}
	// 重放同一个 AgentEnd（事件重放）+ 再排一次通知（结算/恢复并发）。
	ae := &events.AgentEnd{RunId: runID, Content: gwProjectSummary, FinishReason: core.FinishReasonStop}
	for i := 0; i < 2; i++ {
		f.m.finishHubTask(&bridgeSession{ctx: context.Background(), hubTaskIDs: []agenthub.TaskID{task.ID}}, ae)
	}
	f.m.scheduleTaskResultNotification(task.ID)
	waitResultNotifyDrained(t, f.m)

	msgs := p3bMessages(t, f.m, f.g.ID)
	if n := len(p3bTaskResultCards(msgs, task.ID)); n != 1 {
		t.Fatalf("task_result 卡 = %d, want 1（重放/重复通知不能写第二张）", n)
	}
	if n := p3bEventCount(t, f.buf, string(events.TaskResultDeliveredType), task.ID); n != 1 {
		t.Fatalf("task_result_delivered = %d, want 1（只推一次）", n)
	}
	convWait(t, "发起者上下文里仍然只有一份结果", func() bool {
		return len(p3bTaskResultTexts(f.bsA, task.ID)) == 1
	})
	if n := f.fb.callCount(); n != 0 {
		t.Fatalf("旁观者被叫醒 %d 次", n)
	}
	// 重放不能撞状态机产生假告警（delivered→completed 之类）。
	for _, ev := range readAllLines(f.buf) {
		if ev["event_type"] != "hub_alert" || ev["task_id"] != string(task.ID) {
			continue
		}
		switch ev["kind"] {
		case "settle_failed", "push_failed", "task_result_card_failed", "notify_failed":
			t.Fatalf("重放产生了假告警: %+v", ev)
		}
	}
	if got := p3bLoadTask(t, f.m, task.ID); got.Status != agenthub.TaskDelivered {
		t.Fatalf("status = %s, want delivered", got.Status)
	}
}
