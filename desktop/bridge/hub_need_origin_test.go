package main

// 场景2：Need 归属后端。Project 主/子会话触发审批/Ask 时，Need 必须带
// 「哪个 Hub 智能体、哪个群、哪个 Project workspace/session、哪个 task、哪个 run」，
// 供 Hub 单聊/群内联展示；Hub 会话的归属语义保持原样（等的人=会话自己的智能体）。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// projectNeedSite 是「Hub 智能体把任务派给 Project 会话」的最小现场：
// 真项目会话 + 一条 origin/target 完整的账本任务，并把它绑到会话上。
type projectNeedSite struct {
	m      *manager
	buf    *syncBuf
	bs     *bridgeSession
	st     agenthub.Store
	task   *agenthub.Task
	wsPath string
}

func newProjectNeedSite(t *testing.T, origin agenthub.TaskOrigin) *projectNeedSite {
	t.Helper()
	m, buf := testManager(t)
	wsPath := t.TempDir()
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	bs, err := m.create(wsPath, "s-need-origin", "")
	if err != nil {
		t.Fatal(err)
	}
	st := mustHubStore(t, m)
	task, err := agenthub.NewLedger(st).Create(context.Background(), &agenthub.Task{
		Origin: origin,
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: wsPath, SessionID: bs.id},
		Title:  "项目侧归属", Instruction: "改一处沙箱", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	bs.bindHubTasks([]agenthub.TaskID{task.ID})
	return &projectNeedSite{m: m, buf: buf, bs: bs, st: st, task: task, wsPath: wsPath}
}

// singlePendingNeed 取落库里唯一一条待处理 need（多条/零条都算失败）。
func singlePendingNeed(t *testing.T, st agenthub.Store) *agenthub.Need {
	t.Helper()
	pending, err := st.ListNeeds(context.Background(), agenthub.NeedFilter{Status: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending needs = %d, want 1（%+v）", len(pending), pending)
	}
	return pending[0]
}

// assertProjectNeedAttribution 断言 Project need 的 origin/target/run 归属全对，
// 且「被阻塞的会话」仍是 Project 会话（hub_need_resolve 按它找会话）。
func assertProjectNeedAttribution(t *testing.T, n *agenthub.Need, site *projectNeedSite,
	wantRunID string, wantDepth int) {
	t.Helper()
	if n.TaskID != site.task.ID {
		t.Fatalf("task_id = %q, want %q", n.TaskID, site.task.ID)
	}
	// AgentID/GroupID 记 origin：hub_need_list{agent_id|group_id} 才能查到项目侧审批。
	if n.AgentID != "ag_need" || n.GroupID != "grp_need" {
		t.Fatalf("agent_id/group_id = %q/%q, want origin ag_need/grp_need", n.AgentID, n.GroupID)
	}
	if n.OriginAgentID != "ag_need" || n.OriginAgentName != "派单员" ||
		n.OriginKind != agenthub.OriginGroup {
		t.Fatalf("origin = id %q name %q kind %q, want ag_need/派单员/group",
			n.OriginAgentID, n.OriginAgentName, n.OriginKind)
	}
	if n.TargetKind != agenthub.TargetWorkspace || n.TargetWorkspace != site.wsPath ||
		n.TargetSessionID != site.bs.id {
		t.Fatalf("target = kind %q ws %q sid %q, want workspace/%s/%s",
			n.TargetKind, n.TargetWorkspace, n.TargetSessionID, site.wsPath, site.bs.id)
	}
	if n.SessionID != site.bs.id || n.Workspace != site.wsPath {
		t.Fatalf("blocked session = %q/%q", n.SessionID, n.Workspace)
	}
	if n.RunID != wantRunID || n.Depth != wantDepth {
		t.Fatalf("run_id = %q depth = %d, want %q/%d", n.RunID, n.Depth, wantRunID, wantDepth)
	}
	// RunName 预留给子智能体归属（事件里只有 RunId），本轮刻意留空。
	if n.RunName != "" {
		t.Fatalf("run_name = %q, want 空（子智能体归属尚未接）", n.RunName)
	}
}

// assertNeedListedBy 用 hub_need_list 的 group_id/agent_id 两种过滤查一次。
func assertNeedListedBy(t *testing.T, site *projectNeedSite, cmdID int, payload map[string]any, wantNeedID agenthub.NeedID) {
	t.Helper()
	ok, errMsg, data := hubCmd(t, site.m, site.buf, cmdID, "hub_need_list", payload)
	if !ok {
		t.Fatalf("hub_need_list %v: %s", payload, errMsg)
	}
	var out struct {
		Needs []*agenthub.Need `json:"needs"`
	}
	raw, _ := json.Marshal(data)
	_ = json.Unmarshal(raw, &out)
	if len(out.Needs) != 1 || out.Needs[0].ID != wantNeedID {
		t.Fatalf("hub_need_list %v = %s, want 唯一 %s", payload, raw, wantNeedID)
	}
}

// 场景2 主路径：Project 会话 + 绑定的 hub_task → 审批事件 → 归属齐全、两种查法都命中、
// 决策仍按 need.SessionID 找到 Project 会话并 Approve(anchor)。
func TestHubNeedOriginForProjectSessionApproval(t *testing.T) {
	site := newProjectNeedSite(t, agenthub.TaskOrigin{
		Kind: agenthub.OriginGroup, GroupID: "grp_need", AgentID: "ag_need", AgentName: "派单员",
	})
	// 真实注册审批：resolve 才不会回 ErrNoPendingApproval。
	if _, err := site.bs.s.RequestApproval(context.Background(),
		core.ToolCall{Id: "call-need-1", Name: "bash"}); err != nil {
		t.Fatalf("register approval: %v", err)
	}
	const anchor = "call-need-1"
	site.bs.emit(&events.ToolApprovalRequested{
		RunId: "run-need-1", Id: anchor, Name: "bash", Arguments: `{"command":"rm -rf out"}`,
		ParentCallId: "call-parent-1", Depth: 2,
		Timestamp: time.Now(), EventType: events.ToolApprovalType,
	})
	// 事件重放不重建（幂等 ID 仍是 kind+sid+anchor）。
	site.bs.emit(&events.ToolApprovalRequested{
		RunId: "run-need-1", Id: anchor, Name: "bash", Arguments: `{"command":"rm -rf out"}`,
		ParentCallId: "call-parent-1", Depth: 2,
		Timestamp: time.Now(), EventType: events.ToolApprovalType,
	})

	n := singlePendingNeed(t, site.st)
	if n.Kind != "approval" || n.Anchor != anchor {
		t.Fatalf("need = %+v", n)
	}
	assertProjectNeedAttribution(t, n, site, "run-need-1", 2)
	assertNeedListedBy(t, site, 2, map[string]any{"group_id": "grp_need"}, n.ID)
	assertNeedListedBy(t, site, 3, map[string]any{"agent_id": "ag_need"}, n.ID)

	// 决策路由不变：按 SessionID 找会话 → Approve(anchor)。
	ok, errMsg, _ := hubCmd(t, site.m, site.buf, 4, "hub_need_resolve", map[string]any{
		"need_id": string(n.ID), "resolution": "approve",
	})
	if !ok {
		t.Fatalf("resolve: %s", errMsg)
	}
	got, err := site.st.LoadNeed(context.Background(), n.ID)
	if err != nil || got.Status != "resolved" || got.ResolvedAt == nil {
		t.Fatalf("resolved need = %+v, %v", got, err)
	}
}

// Ask 事件同断言：归属一致，RunID 有值、Depth 恒 0（提问不是嵌套调用）。
func TestHubNeedOriginForProjectSessionQuestion(t *testing.T) {
	site := newProjectNeedSite(t, agenthub.TaskOrigin{
		Kind: agenthub.OriginGroup, GroupID: "grp_need", AgentID: "ag_need", AgentName: "派单员",
	})
	site.bs.emit(&events.AskUserQuestion{
		RunId: "run-need-ask", Id: "batch-need-ask",
		Questions: []events.QuestionItem{{Id: "q1", Question: "选哪个目录？"}},
		Timestamp: time.Now(), EventType: events.AskUserQuestionType,
	})

	n := singlePendingNeed(t, site.st)
	if n.Kind != "question" || n.Anchor != "batch-need-ask" {
		t.Fatalf("need = %+v", n)
	}
	assertProjectNeedAttribution(t, n, site, "run-need-ask", 0)
	assertNeedListedBy(t, site, 2, map[string]any{"group_id": "grp_need"}, n.ID)
	assertNeedListedBy(t, site, 3, map[string]any{"agent_id": "ag_need"}, n.ID)
}

// Hub DM 审批回归：归属仍是会话自己的智能体（GroupID 空、无项目 origin），
// 审批照旧能 resolve —— 场景2 不能把 Hub 会话的语义改掉。
func TestHubNeedHubDMSessionApprovalStillResolves(t *testing.T) {
	m, buf := testManager(t)
	hubAgent := createHubAgent(t, m, buf, 1, "NeedOwnAgent")
	bs, err := m.hubDMSession(string(hubAgent.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bs.s.RequestApproval(context.Background(),
		core.ToolCall{Id: "call-hub-need", Name: "bash"}); err != nil {
		t.Fatalf("register approval: %v", err)
	}
	bs.emit(&events.ToolApprovalRequested{
		RunId: "run-hub-need", Id: "call-hub-need", Name: "bash", Arguments: `{"command":"ls"}`,
		Timestamp: time.Now(), EventType: events.ToolApprovalType,
	})

	n := singlePendingNeed(t, mustHubStore(t, m))
	if n.AgentID != hubAgent.ID || n.GroupID != "" {
		t.Fatalf("Hub DM agent_id/group_id = %q/%q, want %q/空", n.AgentID, n.GroupID, hubAgent.ID)
	}
	if n.OriginAgentID != "" || n.OriginAgentName != "" || n.OriginKind != "" || n.TargetKind != "" {
		t.Fatalf("Hub DM need 混进了项目归属: %+v", n)
	}
	if n.RunID != "run-hub-need" || n.Depth != 0 || n.SessionID != bs.id {
		t.Fatalf("Hub DM need = %+v", n)
	}

	ok, errMsg, _ := hubCmd(t, m, buf, 2, "hub_need_resolve", map[string]any{
		"need_id": string(n.ID), "resolution": "approve",
	})
	if !ok {
		t.Fatalf("resolve: %s", errMsg)
	}
	got, err := mustHubStore(t, m).LoadNeed(context.Background(), n.ID)
	if err != nil || got.Status != "resolved" {
		t.Fatalf("resolved need = %+v, %v", got, err)
	}
}

// Hub 群会话执行自己的任务：AgentID/GroupID 保持「等的人=自己」，
// target 从任务回填（目标就是本群会话）。
func TestHubNeedHubGroupTaskFillsTarget(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "NeedGroupA", true)
	b := createDispatchAgent(t, m, buf, 2, "NeedGroupB", false)
	g := assignGroup(t, m, buf, 3, "归属群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member",
	})
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	task := p3bAgentTask(t, m, g, a.ID, b.ID, "群内任务审批", agenthub.TaskDispatched)
	bsB.bindHubTasks([]agenthub.TaskID{task.ID})
	bsB.emit(&events.ToolApprovalRequested{
		RunId: "run-group-need", Id: "call-group-need", Name: "bash", Arguments: `{"command":"ls"}`,
		Timestamp: time.Now(), EventType: events.ToolApprovalType,
	})

	n := singlePendingNeed(t, mustHubStore(t, m))
	if n.AgentID != b.ID || n.GroupID != g.ID {
		t.Fatalf("Hub 群 agent_id/group_id = %q/%q, want %q/%q", n.AgentID, n.GroupID, b.ID, g.ID)
	}
	if n.TargetKind != agenthub.TargetAgent || n.TargetSessionID != bsB.id ||
		n.TargetWorkspace != "" {
		t.Fatalf("Hub 群 target = kind %q ws %q sid %q, want agent//%s",
			n.TargetKind, n.TargetWorkspace, n.TargetSessionID, bsB.id)
	}
	if n.TaskID != task.ID || n.RunID != "run-group-need" || n.Depth != 0 {
		t.Fatalf("Hub 群 need = %+v", n)
	}
}

// 门槛回归：普通 Project 会话（没有 hub_task）不产生 Need；绑到不存在的任务时
// 仍落库但归属留空（不 panic、不阻断 resolve）。
func TestHubNeedSkippedWithoutHubTask(t *testing.T) {
	m, _ := testManager(t)
	wsPath := t.TempDir()
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	bs, err := m.create(wsPath, "s-plain-project", "")
	if err != nil {
		t.Fatal(err)
	}
	st := mustHubStore(t, m)
	ctx := context.Background()

	bs.emit(&events.ToolApprovalRequested{
		RunId: "run-plain", Id: "call-plain", Name: "bash", Arguments: "{}",
		Timestamp: time.Now(), EventType: events.ToolApprovalType,
	})
	if needs, err := st.ListNeeds(ctx, agenthub.NeedFilter{}); err != nil || len(needs) != 0 {
		t.Fatalf("普通 Project 会话产生了 need: %+v, %v", needs, err)
	}

	// 脏/过期的任务绑定：任务快照读不到时不得 panic，归属保持空。
	bs.bindHubTasks([]agenthub.TaskID{agenthub.TaskID("task_missing")})
	bs.emit(&events.AskUserQuestion{
		RunId: "run-orphan", Id: "batch-orphan",
		Questions: []events.QuestionItem{{Id: "q1", Question: "还在吗？"}},
		Timestamp: time.Now(), EventType: events.AskUserQuestionType,
	})
	n := singlePendingNeed(t, st)
	if n.Kind != "question" || n.Anchor != "batch-orphan" || n.TaskID != "task_missing" {
		t.Fatalf("orphan need = %+v", n)
	}
	if n.AgentID != "" || n.GroupID != "" || n.OriginAgentID != "" || n.OriginKind != "" ||
		n.TargetKind != "" || n.TargetWorkspace != "" || n.TargetSessionID != "" {
		t.Fatalf("任务缺失时归属应留空，got %+v", n)
	}
	if n.SessionID != bs.id || n.Workspace != wsPath || n.RunID != "run-orphan" {
		t.Fatalf("orphan need 会话/run = %+v", n)
	}
}
