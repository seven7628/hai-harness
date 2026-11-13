package main

// P6-06：群模式（auto / ask / readonly）真接线。
//
// 反证（改动前红）：接线前 createIn 给所有会话的默认模式是项目口径的 auto
// （Approve 恒 Auto）→ 群策略的 readonly/ask 对工具执行零影响：bash 在 readonly
// 群里照跑、ask 群不产生任何审批 Need。下面两个 e2e 用**生产装配路径**（群唤醒 →
// 会话 Run → 引擎）钉死这一点，不是直接调工具。

import (
	"context"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/tools"
)

// groupModeTestGroup 建一个带 policy.mode 的群（走 hub_group_create 生产路径）。
func groupModeTestGroup(t *testing.T, m *manager, buf *syncBuf, cmdID int, mode string, members ...struct {
	id   agenthub.AgentID
	role string
}) *agenthub.Group {
	t.Helper()
	rows := make([]map[string]any, 0, len(members))
	for _, a := range members {
		rows = append(rows, map[string]any{"agent_id": string(a.id), "role": a.role})
	}
	ok, errMsg, data := hubCmd(t, m, buf, cmdID, "hub_group_create", map[string]any{
		"name": "模式群-" + mode, "members": rows, "policy": map[string]any{"mode": mode},
	})
	if !ok {
		t.Fatalf("create group: %s", errMsg)
	}
	return groupFromData(t, data)
}

// TestHubGroupModeReadonlyGate：readonly 的只读闸门按工具元数据分类，
// 群通信工具恒开，其余一律拒绝并给出明确原因。
func TestHubGroupModeReadonlyGate(t *testing.T) {
	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), &readOnlyProbeTool{name: "read_probe"})
	eng.RegisterTool(context.Background(), &readOnlyProbeTool{name: "write_probe"})
	eng.RegisterTool(context.Background(), newHubGroupSayTool(nil, "ag_x", "grp_x"))

	mode := hubGroupModeFor("readonly", eng)
	if mode.Name != groupModeNameReadonly {
		t.Fatalf("mode name = %q", mode.Name)
	}
	if mode.AllowTool == nil || mode.BlockReason == nil {
		t.Fatal("readonly 必须有 AllowTool + BlockReason（执行前防御 + 明确错误）")
	}
	for _, name := range []string{"group_say", "group_members", "group_history", "agent_status"} {
		// 群通信工具按**名字**恒开（引擎里没注册也要允许：撤下/未装配不是只读群该管的事）。
		if !mode.AllowTool(&readOnlyProbeTool{name: name}) {
			t.Fatalf("%s 必须允许（群通信原语）", name)
		}
	}
	if tool, err := eng.GetTool(context.Background(), "read_probe"); err != nil {
		t.Fatalf("GetTool(read_probe): %v", err)
	} else if !mode.AllowTool(tool) {
		t.Fatal("read_probe 必须允许（ReadOnly_ 元数据）")
	}
	for _, name := range []string{"bash", "write_file", "edit_file", "run_python", "session_send", "assign_task", "handoff", "codemode", "agent_spawn", "write_probe"} {
		if got := mode.AllowTool(&readOnlyProbeTool{name: name}); got {
			t.Fatalf("%s 在 readonly 群必须被拒", name)
		}
	}
	reason := mode.BlockReason(&readOnlyProbeTool{name: "bash"})
	for _, want := range []string{"readonly", "bash", "did not run", "group_say"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("拒绝原因缺 %q: %s", want, reason)
		}
	}
}

// TestHubGroupModeAskApprovalPolicy：ask 只把**非只读**调用送审批通道，
// 读工具/群通信工具直接放行（否则 group_history 每调一次都要人点）。
func TestHubGroupModeAskApprovalPolicy(t *testing.T) {
	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), &readOnlyProbeTool{name: "read_probe"})
	mode := hubGroupModeFor("ask", eng)
	if mode.Name != groupModeNameAsk || mode.AllowTool != nil {
		t.Fatalf("ask mode = %+v（ask 不靠可见性过滤，靠审批）", mode.Name)
	}
	for _, name := range []string{"group_say", "group_members", "group_history", "agent_status", "read_probe"} {
		if got := mode.Approve(core.ToolCall{Name: name}); got != agents.ApprovalAuto {
			t.Fatalf("ask 下 %s 应自动放行，得到 %v", name, got)
		}
	}
	for _, name := range []string{"bash", "write_file", "edit_file", "run_python", "session_send", "assign_task", "handoff", "handoff_ack", "codemode", "agent_spawn"} {
		if got := mode.Approve(core.ToolCall{Name: name}); got != agents.ApprovalRequired {
			t.Fatalf("ask 下 %s 必须经审批，得到 %v", name, got)
		}
	}
}

// TestHubGroupModeAutoHasNoGroupApproval：auto 不加群级审批（Approve 恒 Auto）。
func TestHubGroupModeAutoHasNoGroupApproval(t *testing.T) {
	mode := hubGroupModeFor("auto", nil)
	if mode.Name != groupModeNameAuto {
		t.Fatalf("mode name = %q", mode.Name)
	}
	for _, name := range []string{"bash", "write_file", "run_python", "group_say"} {
		if got := mode.Approve(core.ToolCall{Name: name}); got != agents.ApprovalAuto {
			t.Fatalf("auto 下 %s 不应要群级审批，得到 %v", name, got)
		}
	}
	// 未配置/非法值按默认 ask 处理（EffectiveMode 的既有口径）。
	if got := hubGroupModeFor("", nil); got.Name != groupModeNameAsk {
		t.Fatalf("空 mode 应落 ask，得到 %q", got.Name)
	}
}

// TestHubGroupModeAskRejectFailsTool：ask 的审批被拒 → 工具**不执行**、结果 IsError。
// 走引擎 + 模式包装（与 session.Run 里 WrapApprover 同一条路径）。
func TestHubGroupModeAskRejectFailsTool(t *testing.T) {
	ran := false
	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), &readOnlyProbeTool{name: "write_probe", onCall: func() { ran = true }})
	eng.RegisterTool(context.Background(), &readOnlyProbeTool{name: "read_probe", onCall: func() { ran = true }})
	mode := hubGroupModeFor("ask", eng)

	approver := agents.WrapApprover(rejectingApprover{}, mode)
	calls := []core.ToolCall{
		{Id: "call_write", Name: "write_probe", Arguments: "{}"},
		{Id: "call_read", Name: "read_probe", Arguments: "{}"},
	}
	ctx := events.WithToolContext(context.Background(), "run_1", 0, nil, approver, nil)
	res, err := eng.Sequence(ctx, calls, nil)
	if err != nil {
		t.Fatalf("Sequence: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("results = %d", len(res))
	}
	if !res[0].IsError || !strings.Contains(res[0].Result, "rejected") {
		t.Fatalf("被拒的写工具应失败并说明拒绝: %+v", res[0])
	}
	if res[1].IsError || !strings.Contains(res[1].Result, "read_probe ran") {
		t.Fatalf("只读工具不该进审批、必须执行: %+v", res[1])
	}
	if !ran {
		t.Fatal("只读工具没有真正执行")
	}
}

// TestHubGroupModeAskTimeoutFailsTool：ask 的审批窗口用尽 → 工具失败，
// 文案必须区分「没人决策」而不是「用户否决」（引擎既有语义）。
func TestHubGroupModeAskTimeoutFailsTool(t *testing.T) {
	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), &readOnlyProbeTool{name: "write_probe"})
	mode := hubGroupModeFor("ask", eng)
	approver := agents.WrapApprover(timeoutApprover{}, mode)
	ctx := events.WithToolContext(context.Background(), "run_1", 0, nil, approver, nil)
	res, err := eng.Sequence(ctx, []core.ToolCall{{Id: "call_write", Name: "write_probe", Arguments: "{}"}}, nil)
	if err != nil {
		t.Fatalf("Sequence: %v", err)
	}
	if len(res) != 1 || !res[0].IsError {
		t.Fatalf("超时应判失败: %+v", res)
	}
	if !strings.Contains(res[0].Result, "timed out") || strings.Contains(res[0].Result, "user rejected") {
		t.Fatalf("超时文案必须与拒绝分开: %s", res[0].Result)
	}
}

// TestHubGroupReadonlyBlocksBashInWake：生产路径 e2e —— readonly 群里被 @ 唤醒的
// 智能体硬调 bash，工具不执行，模型拿到明确错误（不是静默失败/假成功）。
func TestHubGroupReadonlyBlocksBashInWake(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "ReadonlyA")
	g := groupModeTestGroup(t, m, buf, 2, "readonly", struct {
		id   agenthub.AgentID
		role string
	}{a.ID, "owner"})

	bs := mustGroupSession(t, m, string(a.ID), string(g.ID))
	if got := bs.s.Mode(); got == nil || got.Name != groupModeNameReadonly {
		t.Fatalf("群会话模式 = %+v, want %s（策略必须落到会话）", got, groupModeNameReadonly)
	}
	fake := newP3bEvalProvider(
		p3bEvalRound{tool: &core.ToolCall{Id: "call_bash", Index: 0, Name: "bash", Arguments: `{"command":"echo p6-06"}`}},
		p3bEvalRound{content: "只读群，我不该跑命令。"},
	)
	setGroupFakeLoopIters(t, m, bs, g, string(a.ID), fake, 2)

	if ok, errMsg, _ := hubCmd(t, m, buf, 3, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@" + a.Name + " 跑个命令看看",
	}); !ok {
		t.Fatalf("人类 @ 失败: %s", errMsg)
	}
	convWait(t, "readonly 群的工具轮收尾", func() bool { return fake.callCount() >= 2 })

	// 第二次请求里必须带着 bash 的 IsError 结果（模型可见的明确错误）。
	text := p3bEvalRequestText(fake.requestAt(1))
	for _, want := range []string{"readonly mode", "did not run"} {
		if !strings.Contains(text, want) {
			t.Fatalf("工具结果缺 %q:\n%s", want, text)
		}
	}
	// 工具真的没执行：没有 bash 的 ToolResponse（执行过就会有）。
	for _, ev := range readAllLines(buf) {
		if ev["event_type"] != "tool_run_start" {
			continue
		}
		if name, _ := ev["name"].(string); name == "bash" {
			t.Fatalf("readonly 群的 bash 被执行了: %+v", ev)
		}
	}
}

// TestHubGroupAskCreatesApprovalNeedForBash：生产路径 e2e —— ask 群里 bash 调用
// 落成一条群审批 Need；人类批准后工具才执行（复用 ToolApprovalRequested/Needs）。
func TestHubGroupAskCreatesApprovalNeedForBash(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "AskA")
	g := groupModeTestGroup(t, m, buf, 2, "ask", struct {
		id   agenthub.AgentID
		role string
	}{a.ID, "owner"})

	bs := mustGroupSession(t, m, string(a.ID), string(g.ID))
	if got := bs.s.Mode(); got == nil || got.Name != groupModeNameAsk {
		t.Fatalf("群会话模式 = %+v, want %s", got, groupModeNameAsk)
	}
	fake := newP3bEvalProvider(
		p3bEvalRound{tool: &core.ToolCall{Id: "call_bash", Index: 0, Name: "bash", Arguments: `{"command":"echo p6-06-ask"}`}},
		p3bEvalRound{content: "命令跑完了。"},
	)
	setGroupFakeLoopIters(t, m, bs, g, string(a.ID), fake, 2)

	if ok, errMsg, _ := hubCmd(t, m, buf, 3, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@" + a.Name + " 跑个命令看看",
	}); !ok {
		t.Fatalf("人类 @ 失败: %s", errMsg)
	}

	st := mustHubStore(t, m)
	var need *agenthub.Need
	convWait(t, "ask 群为 bash 落一条审批 Need", func() bool {
		needs, err := st.ListNeeds(context.Background(), agenthub.NeedFilter{GroupID: g.ID, Status: "pending"})
		if err != nil {
			return false
		}
		for _, n := range needs {
			if n.Kind == "approval" && n.Status == "pending" && n.GroupID == g.ID && strings.Contains(n.Title, "bash") {
				need = n
				return true
			}
		}
		return false
	})
	if need.AgentID != a.ID {
		t.Fatalf("审批 Need 归属 = %s, want %s", need.AgentID, a.ID)
	}
	// 批准前工具不能执行（第 2 次请求还没发生）。
	if fake.callCount() != 1 {
		t.Fatalf("批准前模型就续跑了（callCount=%d）", fake.callCount())
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_need_resolve", map[string]any{
		"need_id": string(need.ID), "resolution": "approve",
	}); !ok {
		t.Fatalf("批准失败: %s", errMsg)
	}
	convWait(t, "批准后 bash 执行并收尾", func() bool { return fake.callCount() >= 2 })
	text := p3bEvalRequestText(fake.requestAt(1))
	if !strings.Contains(text, "p6-06-ask") {
		t.Fatalf("批准后模型应看到 bash 输出:\n%s", text)
	}
}

// TestHubGroupUpdateModeReachesLiveSession：改群模式必须推到**已装配**会话
// （否则 UI 显示 readonly、实际仍按旧模式跑 = 假开关）。
func TestHubGroupUpdateModeReachesLiveSession(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "SwitchA")
	g := groupModeTestGroup(t, m, buf, 2, "auto", struct {
		id   agenthub.AgentID
		role string
	}{a.ID, "owner"})
	bs := mustGroupSession(t, m, string(a.ID), string(g.ID))
	if got := bs.s.Mode(); got == nil || got.Name != groupModeNameAuto {
		t.Fatalf("初始模式 = %+v", got)
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 3, "hub_group_update", map[string]any{
		"group_id": string(g.ID), "patch": map[string]any{"policy": map[string]any{"mode": "readonly"}},
	}); !ok {
		t.Fatalf("update mode: %s", errMsg)
	}
	if got := bs.s.Mode(); got == nil || got.Name != groupModeNameReadonly {
		t.Fatalf("更新后模式 = %+v, want %s", got, groupModeNameReadonly)
	}
	// 新装配的会话同样按群策略（不是 createIn 的默认 auto）。
	second := createHubAgent(t, m, buf, 4, "SwitchB")
	if ok, errMsg, _ := hubCmd(t, m, buf, 5, "hub_group_member_add", map[string]any{
		"group_id": string(g.ID), "agent_id": string(second.ID),
	}); !ok {
		t.Fatalf("member add: %s", errMsg)
	}
	bsB := mustGroupSession(t, m, string(second.ID), string(g.ID))
	if got := bsB.s.Mode(); got == nil || got.Name != groupModeNameReadonly {
		t.Fatalf("新成员会话模式 = %+v, want %s", got, groupModeNameReadonly)
	}
}

// —— 测试替身 ——

// readOnlyProbeTool 只读/写探针：name 以 read_ 开头 = 只读（元数据口径）。
type readOnlyProbeTool struct {
	tools.BaseTool
	name   string
	onCall func()
}

func (p *readOnlyProbeTool) Name() string { return p.name }
func (p *readOnlyProbeTool) Description() string {
	return "probe"
}
func (p *readOnlyProbeTool) Parameters() any { return tools.Obj(map[string]any{}) }
func (p *readOnlyProbeTool) CanParallel() bool {
	return false
}
func (p *readOnlyProbeTool) ValidParams(context.Context, string, string) error { return nil }
func (p *readOnlyProbeTool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}
func (p *readOnlyProbeTool) Call(context.Context, string, string) (string, error) {
	if p.onCall != nil {
		p.onCall()
	}
	return p.name + " ran", nil
}
func (p *readOnlyProbeTool) AfterCall(context.Context, core.ToolCall) {}
func (p *readOnlyProbeTool) ReadOnly() bool                           { return strings.HasPrefix(p.name, "read_") }

// rejectingApprover 人否决（与 session.approvalWaiter 的拒绝同语义）。
type rejectingApprover struct{}

func (rejectingApprover) BeginApproval(core.ToolCall) func(context.Context) (bool, error) {
	return func(context.Context) (bool, error) { return false, nil }
}

// timeoutApprover 窗口用尽、没人决策（引擎必须与「拒绝」分开措辞）。
type timeoutApprover struct{}

func (timeoutApprover) BeginApproval(core.ToolCall) func(context.Context) (bool, error) {
	return func(context.Context) (bool, error) { return false, events.ErrApprovalTimeout }
}

// TestHubGroupModePolicyFieldIsEffective（P6-13 生效性口径）：同一个已装配的群会话，
// 只改 policy.mode → 会话拿到的 agents.Mode 必须跟着换。这是「字段 → 行为」的接线
// 判据：P6-06 之前它是个假开关（UI 显示 readonly，引擎照旧 auto）。
//
// 与上面三个纯函数用例的分工：那三个钉每种模式的语义，这里钉「改字段真的会被读」。
func TestHubGroupModePolicyFieldIsEffective(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "模式主")
	g := groupModeTestGroup(t, m, buf, 2, agenthub.GroupModeReadonly,
		struct {
			id   agenthub.AgentID
			role string
		}{a.ID, "owner"},
	)
	bs, err := m.hubGroupSession(string(a.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	modeName := func() string {
		t.Helper()
		got := bs.s.Mode()
		if got == nil {
			t.Fatal("会话没有模式")
		}
		return got.Name
	}
	if got := modeName(); got != groupModeNameReadonly {
		t.Fatalf("新建 readonly 群后会话模式 = %q", got)
	}
	// 只改 mode 字段（其它策略字段原样保留）→ 已装配会话的模式必须换。
	ok, errMsg, data := hubCmd(t, m, buf, 3, "hub_group_update", map[string]any{
		"group_id": string(g.ID), "patch": map[string]any{"policy": map[string]any{"mode": agenthub.GroupModeAuto}},
	})
	if !ok {
		t.Fatalf("update mode: %s", errMsg)
	}
	if got := groupFromData(t, data).Policy.Mode; got != agenthub.GroupModeAuto {
		t.Fatalf("落盘 mode = %q", got)
	}
	if got := modeName(); got != groupModeNameAuto {
		t.Fatalf("改字段后会话模式仍是 %q（假开关）", got)
	}
	ok, errMsg, _ = hubCmd(t, m, buf, 4, "hub_group_update", map[string]any{
		"group_id": string(g.ID), "patch": map[string]any{"policy": map[string]any{"mode": agenthub.GroupModeAsk}},
	})
	if !ok {
		t.Fatalf("update mode: %s", errMsg)
	}
	if got := modeName(); got != groupModeNameAsk {
		t.Fatalf("改字段后会话模式仍是 %q（假开关）", got)
	}
}
