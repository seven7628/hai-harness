package main

// P4-04a：Hub 外部 AgentEnd/Stop hook 接线（默认关闭，仅用户级）。
//
// 钉住六件事：
//  1. 默认关：没有用户级 hooks 配置 → Hub 会话 hooks 为 nil，run 行为不变；
//  2. 用户级 Stop 配好且可信 → 自然结束（AgentEnd 锚点）每次 Run 恰好执行一次，
//     payload 带 Hub 会话 id / cwd / hook_event_name=Stop（多次 Run 也能逐条解析）；
//  3. 未信任 / disableAllHooks / hook 失败 / 超时 → fail-open，任务照常结算与回推；
//  4. 群会话同样接线；Hub home 里的项目级 hooks 绝不被读取（安全边界）；
//  5. 用户级 hooks_set 热替换必须覆盖 Hub DM / 群会话（B1：新增、禁用都即刻生效）；
//  6. 项目会话的用户 + 项目两层装配未被重构破坏；Hub home 走项目路径被明确拒绝。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/hooks"
)

// p404aShellQuote 单引号包一个 shell 参数（路径里可能有空格；单引号自身按 POSIX 转义）。
func p404aShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// p404aAppendCmd 把 stdin payload 追加到文件，并**补一个换行**。
// hook stdin 本身没有尾随换行；直接 `cat >>` 会让多次 Run 的 JSON 粘成
// `{...}{...}`，多条 payload 无法逐行解析（N4）。
func p404aAppendCmd(path string) string {
	q := p404aShellQuote(path)
	return "cat >> " + q + "; echo >> " + q
}

// p404aStopSection 构造 hooks 段（go-code 原生形态 1）。
func p404aStopSection(cmd string, timeout float64) map[string]any {
	h := map[string]any{"type": "command", "command": cmd}
	if timeout > 0 {
		h["timeout"] = timeout
	}
	return map[string]any{"events": map[string]any{"Stop": []any{
		map[string]any{"hooks": []any{h}},
	}}}
}

// p404aWriteUserHooks 写 ~/.go-code/settings.json（HOME 已由 testManager 隔离到 TempDir）。
func p404aWriteUserHooks(t *testing.T, section map[string]any) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Fatalf("user home: %v (%q)", err, home)
	}
	dir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(map[string]any{"hooks": section}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// p404aHooksSet 走**真实命令路径**（m.dispatch → handleHooksSet → 用户级写盘 + 热替换）。
func p404aHooksSet(t *testing.T, m *manager, buf *syncBuf, id int, body map[string]any) {
	t.Helper()
	before := buf.String()
	m.dispatch(command{Id: id, Type: CmdHooksSet, Payload: map[string]any{
		"scope": "user", "body": body,
	}})
	after := buf.String()
	for _, line := range strings.Split(strings.TrimPrefix(after, before), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var msg map[string]any
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue
		}
		if msg["event_type"] != "command_response" {
			continue
		}
		rawID, ok := msg["id"].(float64)
		if !ok || int(rawID) != id {
			continue
		}
		if ok, _ := msg["ok"].(bool); ok {
			return
		}
		errMsg, _ := msg["error"].(string)
		t.Fatalf("hooks_set 失败: %s", errMsg)
	}
	t.Fatalf("hooks_set 没有 command_response（输出:\n%s）", after)
}

// p404aPayloadCount 只数**能解析**的 JSON 行；轮询窗口里可能碰上半行写入。
func p404aPayloadCount(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var p map[string]any
		if json.Unmarshal([]byte(line), &p) == nil {
			n++
		}
	}
	return n
}

// p404aWaitPayloads 等 hook 文件里的可解析 payload 达到 want（hook 在 AgentEnd 之前完成，
// 但不要依赖"索引先到"的偶然顺序）。
func p404aWaitPayloads(t *testing.T, path string, want int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if p404aPayloadCount(t, path) >= want {
			return p404aReadPayloads(t, path)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("hook payloads at %s: got %d, want %d", path, p404aPayloadCount(t, path), want)
	return nil
}

// p404aReadPayloads 读取 hook 以 JSON-lines 追加的 stdin payload。
func p404aReadPayloads(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			t.Fatalf("hook payload is not JSON (multiple runs concatenated?): %q: %v", line, err)
		}
		out = append(out, p)
	}
	return out
}

// p404aSetDMLoop 把 DM 会话换成确定性 fake loop（不碰网络）。
func p404aSetDMLoop(t *testing.T, bs *bridgeSession, p *evalProvider) {
	t.Helper()
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(p)))
}

// p404aSendDM 发一条 DM 并等到索引里至少有 want 条消息（AgentEnd 之后，hook 必已执行完）。
func p404aSendDM(t *testing.T, m *manager, buf *syncBuf, cmdID, want int, p agenthub.AgentProfile, text string) {
	t.Helper()
	ok, errMsg, _ := hubCmd(t, m, buf, cmdID, "hub_send", map[string]any{
		"target": map[string]any{"kind": "dm", "agent_id": string(p.ID)},
		"text":   text,
	})
	if !ok {
		t.Fatalf("hub_send: %s", errMsg)
	}
	msgs := waitHubIndex(t, m, p.ID, want)
	if msgs[len(msgs)-1].Sender.Kind != agenthub.SenderAgent {
		t.Fatalf("agent reply not indexed: %+v", msgs)
	}
}

// p404aWaitGroupIdle 等群会话放闩（下一次 send 才不会被判忙）。
func p404aWaitGroupIdle(t *testing.T, bs *bridgeSession) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for bs.hubGroupBusy.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if bs.hubGroupBusy.Load() {
		t.Fatal("group session stayed busy")
	}
}

// TestHubSessionHooksDefaultOff 无用户级配置：hooks 为 nil，run 行为与接线前一致。
func TestHubSessionHooksDefaultOff(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "NoHooks")
	bs, err := m.hubDMSession(string(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	if h := bs.s.Hooks(); h != nil {
		t.Fatalf("hub session hooks = %+v, want nil (no user-level config)", h)
	}
	prov := &evalProvider{reply: "unchanged"}
	p404aSetDMLoop(t, bs, prov)
	p404aSendDM(t, m, buf, 2, 2, p, "hello")
	if prov.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", prov.callCount())
	}
}

// TestHubSessionStopHookFiresAtNaturalEnd 用户级 Stop：每次自然结束恰好一次，多次 Run 可解析。
func TestHubSessionStopHookFiresAtNaturalEnd(t *testing.T) {
	m, buf := testManager(t)
	marker := filepath.Join(t.TempDir(), "stop.jsonl")
	p404aWriteUserHooks(t, p404aStopSection(p404aAppendCmd(marker), 0))

	p := createHubAgent(t, m, buf, 1, "HookDM")
	bs, err := m.hubDMSession(string(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	if h := bs.s.Hooks(); h == nil || h.Stop == nil {
		t.Fatalf("hub session Stop hook not wired: %+v", h)
	}
	prov := &evalProvider{reply: "hook done"}
	p404aSetDMLoop(t, bs, prov)
	p404aSendDM(t, m, buf, 2, 2, p, "run once")

	payloads := p404aWaitPayloads(t, marker, 1)
	if len(payloads) != 1 {
		t.Fatalf("Stop hook ran %d times, want exactly 1 (payloads=%+v)", len(payloads), payloads)
	}
	got := payloads[0]
	if got["session_id"] != bs.id {
		t.Fatalf("payload session_id = %v, want %q", got["session_id"], bs.id)
	}
	if got["cwd"] != hubAgentHome(string(p.ID)) {
		t.Fatalf("payload cwd = %v, want %q", got["cwd"], hubAgentHome(string(p.ID)))
	}
	if got["hook_event_name"] != "Stop" {
		t.Fatalf("payload hook_event_name = %v, want Stop", got["hook_event_name"])
	}
	if got["turn_id"] == "" || got["turn_id"] == nil {
		t.Fatalf("payload turn_id missing: %+v", got)
	}

	// 第二次 Run 再写一条：证明每次自然结束恰好一次，且多次 payload 可逐行解析（N4）。
	p404aSendDM(t, m, buf, 3, 4, p, "run twice")
	payloads = p404aWaitPayloads(t, marker, 2)
	if len(payloads) != 2 {
		t.Fatalf("Stop hook payloads after two runs = %d, want 2", len(payloads))
	}
	if payloads[0]["turn_id"] == payloads[1]["turn_id"] {
		t.Fatalf("two runs share turn_id %v (hook did not run per run)", payloads[0]["turn_id"])
	}
	for i, pl := range payloads {
		if pl["session_id"] != bs.id || pl["hook_event_name"] != "Stop" {
			t.Fatalf("payload[%d] = %+v", i, pl)
		}
	}
}

// TestHubSessionStopHookGuardsFailOpen 未信任 / disableAllHooks：不执行，但 run 照常收尾。
func TestHubSessionStopHookGuardsFailOpen(t *testing.T) {
	cases := []struct {
		name     string
		section  func(cmd string) map[string]any
		wantHook bool // 装配层是否仍有派发器（未信任仍装配；disableAll 不装配）
	}{
		{
			name: "untrusted",
			section: func(cmd string) map[string]any {
				s := p404aStopSection(cmd, 0)
				s["requireTrust"] = true // 用户级也要求显式信任；测试不写信任库 → 未信任
				return s
			},
			wantHook: true,
		},
		{
			name: "disable_all",
			section: func(cmd string) map[string]any {
				s := p404aStopSection(cmd, 0)
				s["disableAllHooks"] = true
				return s
			},
			wantHook: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, buf := testManager(t)
			marker := filepath.Join(t.TempDir(), "stop.jsonl")
			p404aWriteUserHooks(t, tc.section(p404aAppendCmd(marker)))

			p := createHubAgent(t, m, buf, 1, "Guarded")
			bs, err := m.hubDMSession(string(p.ID))
			if err != nil {
				t.Fatal(err)
			}
			if got := bs.s.Hooks() != nil; got != tc.wantHook {
				t.Fatalf("hooks assembled = %v, want %v", got, tc.wantHook)
			}
			prov := &evalProvider{reply: "still settles"}
			p404aSetDMLoop(t, bs, prov)
			p404aSendDM(t, m, buf, 2, 2, p, "go")
			if payloads := p404aReadPayloads(t, marker); len(payloads) != 0 {
				t.Fatalf("guarded hook must not execute, got %+v", payloads)
			}
		})
	}
}

// TestHubSessionStopHookTimeoutFailsOpen hook 超时 fail-open：run 照常收尾并发出告警（N3）。
func TestHubSessionStopHookTimeoutFailsOpen(t *testing.T) {
	m, buf := testManager(t)
	// exec：让 sleep 替换 shell 自身，CommandContext 到点能直接杀掉，不会留下持管道的孤儿。
	p404aWriteUserHooks(t, p404aStopSection("exec sleep 60", 0.2))

	p := createHubAgent(t, m, buf, 1, "HookTimeout")
	bs, err := m.hubDMSession(string(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	if h := bs.s.Hooks(); h == nil || h.Stop == nil {
		t.Fatalf("hub session Stop hook not wired: %+v", h)
	}
	prov := &evalProvider{reply: "timeout still settles"}
	p404aSetDMLoop(t, bs, prov)
	p404aSendDM(t, m, buf, 2, 2, p, "timeout")

	warned := false
	for _, ev := range hubEvents(t, buf, "tool_hook_warning") {
		msg, _ := ev["message"].(string)
		if strings.Contains(msg, "timed out") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("timeout hook must emit tool_hook_warning (got %d warnings)", len(hubEvents(t, buf, "tool_hook_warning")))
	}
	if prov.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", prov.callCount())
	}
}

// TestHubSessionStopHookFailureKeepsSettlementAndPush hook 失败 fail-open：
// 真实 hub_task 仍从 AgentEnd 结算，并把结果回推给**真实**发起智能体（N2）。
func TestHubSessionStopHookFailureKeepsSettlementAndPush(t *testing.T) {
	m, buf := testManager(t)
	marker := filepath.Join(t.TempDir(), "attempted.log")
	// 先留下"真的跑过"的证据，再以非零退出（default failureMode=fail_open）。
	cmd := "echo ran >> " + p404aShellQuote(marker) + "; exit 3"
	p404aWriteUserHooks(t, p404aStopSection(cmd, 0))

	origin := createHubAgent(t, m, buf, 1, "OriginAgent")
	executor := createHubAgent(t, m, buf, 2, "ExecAgent")
	originBS, err := m.hubDMSession(string(origin.ID))
	if err != nil {
		t.Fatal(err)
	}
	p404aSetDMLoop(t, originBS, &evalProvider{reply: "origin got result"})
	execBS, err := m.hubDMSession(string(executor.ID))
	if err != nil {
		t.Fatal(err)
	}
	prov := &evalProvider{reply: "task payload done"}
	p404aSetDMLoop(t, execBS, prov)

	st := mustHubStore(t, m)
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin:      agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: origin.ID},
		Target:      agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: hubAgentHome(string(executor.ID)), SessionID: execBS.id},
		Title:       "hook fail-open",
		Instruction: "hook fail-open",
		Callback:    agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched,
		agenthub.TransitionOptions{At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	header, err := agenthub.EncodeTaskHeader(task)
	if err != nil {
		t.Fatal(err)
	}
	msg := core.NewUserMessage(
		core.Content{Type: core.ContentTypeHubTask, Content: header},
		core.Content{Type: core.ContentTypeText, Content: "do the task"},
	)
	if err := execBS.s.Ask(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	execBS.wakeRun()

	deadline := time.Now().Add(20 * time.Second)
	var got *agenthub.Task
	for time.Now().Before(deadline) {
		got, err = st.LoadTask(context.Background(), task.ID)
		if err == nil && got != nil && got.Status == agenthub.TaskDelivered &&
			originBS.s.HasTaskResult(string(task.ID)) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got == nil || got.Status != agenthub.TaskDelivered {
		t.Fatalf("hub task not settled+delivered after failing Stop hook: %+v (err=%v)", got, err)
	}
	if !originBS.s.HasTaskResult(string(task.ID)) {
		t.Fatal("task result was not pushed into the real origin session")
	}
	if len(got.NotifiedRuns) == 0 {
		t.Fatalf("task notified_run_ids empty: %+v", got)
	}
	pushed := false
	for _, ev := range hubEvents(t, buf, "task_result_delivered") {
		if ev["task_id"] == string(task.ID) {
			pushed = true
		}
	}
	if !pushed {
		t.Fatal("task_result_delivered event missing (result notification did not reach the host)")
	}
	if raw, rerr := os.ReadFile(marker); rerr != nil || !strings.Contains(string(raw), "ran") {
		t.Fatalf("failing hook did not run as expected (err=%v, body=%q)", rerr, string(raw))
	}
}

// TestHubGroupSessionStopHookFiresWithGroupSessionID 群会话同样接线，payload 拿群会话 id。
func TestHubGroupSessionStopHookFiresWithGroupSessionID(t *testing.T) {
	m, buf := testManager(t)
	marker := filepath.Join(t.TempDir(), "stop.jsonl")
	p404aWriteUserHooks(t, p404aStopSection(p404aAppendCmd(marker), 0))

	a := createHubAgent(t, m, buf, 1, "HookGroupA")
	g := groupWithMembers(t, m, buf, 2, "hook 群", struct {
		id   agenthub.AgentID
		role string
	}{a.ID, "owner"})

	bs, err := m.hubGroupSession(string(a.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	if h := bs.s.Hooks(); h == nil || h.Stop == nil {
		t.Fatalf("group hub session Stop hook not wired: %+v", h)
	}
	setGroupFakeLoop(t, m, bs, g, string(a.ID), &evalProvider{reply: "group hook done"})

	ok, errMsg, _ := hubCmd(t, m, buf, 3, "hub_group_send", map[string]any{
		"group_id": string(g.ID),
		"text":     "@HookGroupA 看这条",
	})
	if !ok {
		t.Fatalf("hub_group_send: %s", errMsg)
	}
	waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		for _, msg := range msgs {
			if msg.Sender.Kind == agenthub.SenderAgent && msg.Sender.AgentID == a.ID &&
				strings.Contains(msg.Text, "group hook done") {
				return true
			}
		}
		return false
	})

	payloads := p404aWaitPayloads(t, marker, 1)
	if len(payloads) != 1 {
		t.Fatalf("group Stop hook ran %d times, want exactly 1 (payloads=%+v)", len(payloads), payloads)
	}
	wantSID := hubSessionID(string(a.ID), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID})
	if payloads[0]["session_id"] != wantSID {
		t.Fatalf("group payload session_id = %v, want %q", payloads[0]["session_id"], wantSID)
	}
	if payloads[0]["hook_event_name"] != "Stop" {
		t.Fatalf("group payload hook_event_name = %v, want Stop", payloads[0]["hook_event_name"])
	}
}

// TestHubSessionIgnoresProjectHooksInHubHome 安全边界：Hub home 的 .go-code/hooks.json
// 与 .claude/settings.json 都是"项目形态"，即使 allowProjectHooks=true 且命令已信任，
// 打开智能体也不能执行它们。用户级配置是正对照（另一个测试）——这里只放项目层。
func TestHubSessionIgnoresProjectHooksInHubHome(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Boundary")
	home := hubAgentHome(string(p.ID))
	if err := os.MkdirAll(filepath.Join(home, ".go-code"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	goCodeMarker := filepath.Join(t.TempDir(), "project-gocode.jsonl")
	claudeMarker := filepath.Join(t.TempDir(), "project-claude.jsonl")
	goCodeCmd := p404aAppendCmd(goCodeMarker)
	claudeCmd := p404aAppendCmd(claudeMarker)

	projectHooks := map[string]any{
		"allowProjectHooks": true,
		"events": map[string]any{"Stop": []any{
			map[string]any{"hooks": []any{map[string]any{"type": "command", "command": goCodeCmd}}},
		}},
	}
	raw, err := json.MarshalIndent(projectHooks, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".go-code", "hooks.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	claudeSettings := map[string]any{"hooks": map[string]any{"Stop": []any{
		map[string]any{"hooks": []any{map[string]any{"type": "command", "command": claudeCmd}}},
	}}}
	raw, err = json.MarshalIndent(claudeSettings, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// 两条命令都预先写入信任库：若有人把 Hub home 当 WorkspaceDir 去加载项目层，
	// 信任闸门会放行、hook 就会真的执行 —— 这条测试因此不是"反正都没信任"的假绿。
	trust, err := hooks.LoadTrust(hooksStatePath())
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{goCodeCmd, claudeCmd} {
		if err := trust.Trust(cmd, "test", "p4-04a boundary"); err != nil {
			t.Fatal(err)
		}
	}

	bs, err := m.hubDMSession(string(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	if h := bs.s.Hooks(); h != nil {
		t.Fatalf("hub session loaded project-level hooks from hub home: %+v", h)
	}
	prov := &evalProvider{reply: "boundary ok"}
	p404aSetDMLoop(t, bs, prov)
	p404aSendDM(t, m, buf, 2, 2, p, "run without project hooks")

	for _, marker := range []string{goCodeMarker, claudeMarker} {
		if payloads := p404aReadPayloads(t, marker); len(payloads) != 0 {
			t.Fatalf("project-level hook from hub home executed: %s %+v", marker, payloads)
		}
	}
}

// TestHubHooksSetHotAppliesToDMSession B1：用户级 hooks_set 新增 Stop 后，
// 已打开的 Hub DM 会话立即装配，下一次 Run 恰好执行一次。
func TestHubHooksSetHotAppliesToDMSession(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "HotDM")
	bs, err := m.hubDMSession(string(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	if bs.s.Hooks() != nil {
		t.Fatal("precondition: hub session starts without hooks")
	}
	marker := filepath.Join(t.TempDir(), "stop.jsonl")
	p404aHooksSet(t, m, buf, 2, p404aStopSection(p404aAppendCmd(marker), 0))
	if h := bs.s.Hooks(); h == nil || h.Stop == nil {
		t.Fatalf("hooks_set(user) did not hot-apply to the existing Hub DM session: %+v", h)
	}

	prov := &evalProvider{reply: "hot dm"}
	p404aSetDMLoop(t, bs, prov)
	p404aSendDM(t, m, buf, 3, 2, p, "after hooks_set")

	payloads := p404aWaitPayloads(t, marker, 1)
	if len(payloads) != 1 {
		t.Fatalf("hot-applied Stop hook ran %d times, want 1 (%+v)", len(payloads), payloads)
	}
	if payloads[0]["session_id"] != bs.id {
		t.Fatalf("payload session_id = %v, want %q", payloads[0]["session_id"], bs.id)
	}
}

// TestHubHooksSetDisableHotAppliesToDMSession B1：hooks_set(disableAllHooks=true) 后，
// 已打开的 Hub DM 会话必须丢掉旧 dispatcher，下一次 Run 一次都不执行。
func TestHubHooksSetDisableHotAppliesToDMSession(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "HotDisable")
	marker := filepath.Join(t.TempDir(), "stop.jsonl")
	p404aHooksSet(t, m, buf, 2, p404aStopSection(p404aAppendCmd(marker), 0))

	bs, err := m.hubDMSession(string(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	if h := bs.s.Hooks(); h == nil || h.Stop == nil {
		t.Fatalf("precondition: Stop hook should be wired, got %+v", h)
	}
	prov := &evalProvider{reply: "before disable"}
	p404aSetDMLoop(t, bs, prov)
	p404aSendDM(t, m, buf, 3, 2, p, "run with hook")
	if payloads := p404aWaitPayloads(t, marker, 1); len(payloads) != 1 {
		t.Fatalf("baseline Stop hook payloads = %d, want 1", len(payloads))
	}

	p404aHooksSet(t, m, buf, 4, map[string]any{"disableAllHooks": true})
	if h := bs.s.Hooks(); h != nil {
		t.Fatalf("disableAllHooks did not hot-apply to the existing Hub DM session: %+v", h)
	}
	p404aSendDM(t, m, buf, 5, 4, p, "run after disable")
	if payloads := p404aReadPayloads(t, marker); len(payloads) != 1 {
		t.Fatalf("disabled Stop hook still executed: %d payloads (want 1 from baseline)", len(payloads))
	}
}

// TestHubHooksSetHotAppliesToGroupSession B1：群会话同一套热替换（新增 + 禁用），
// payload 的 session_id/cwd 用群作用域，不与 DM 混。
func TestHubHooksSetHotAppliesToGroupSession(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "HotGroup")
	g := groupWithMembers(t, m, buf, 2, "hot 群", struct {
		id   agenthub.AgentID
		role string
	}{a.ID, "owner"})
	bs, err := m.hubGroupSession(string(a.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	if bs.s.Hooks() != nil {
		t.Fatal("precondition: group session starts without hooks")
	}
	marker := filepath.Join(t.TempDir(), "stop.jsonl")
	p404aHooksSet(t, m, buf, 3, p404aStopSection(p404aAppendCmd(marker), 0))
	if h := bs.s.Hooks(); h == nil || h.Stop == nil {
		t.Fatalf("hooks_set(user) did not hot-apply to the existing group session: %+v", h)
	}

	setGroupFakeLoop(t, m, bs, g, string(a.ID), &evalProvider{reply: "group hot one"})
	ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@HotGroup 第一条",
	})
	if !ok {
		t.Fatalf("group send #1: %s", errMsg)
	}
	waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		for _, msg := range msgs {
			if msg.Sender.Kind == agenthub.SenderAgent && strings.Contains(msg.Text, "group hot one") {
				return true
			}
		}
		return false
	})
	p404aWaitGroupIdle(t, bs)
	payloads := p404aWaitPayloads(t, marker, 1)
	if len(payloads) != 1 {
		t.Fatalf("group hot-applied Stop payloads = %d, want 1", len(payloads))
	}
	wantSID := hubSessionID(string(a.ID), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID})
	if payloads[0]["session_id"] != wantSID {
		t.Fatalf("group payload session_id = %v, want %q", payloads[0]["session_id"], wantSID)
	}
	if payloads[0]["cwd"] != hubAgentHome(string(a.ID)) {
		t.Fatalf("group payload cwd = %v, want %q", payloads[0]["cwd"], hubAgentHome(string(a.ID)))
	}

	p404aHooksSet(t, m, buf, 5, map[string]any{"disableAllHooks": true})
	if h := bs.s.Hooks(); h != nil {
		t.Fatalf("disableAllHooks did not hot-apply to the existing group session: %+v", h)
	}
	setGroupFakeLoop(t, m, bs, g, string(a.ID), &evalProvider{reply: "group hot two"})
	ok, errMsg, _ = hubCmd(t, m, buf, 6, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@HotGroup 第二条",
	})
	if !ok {
		t.Fatalf("group send #2: %s", errMsg)
	}
	waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		for _, msg := range msgs {
			if msg.Sender.Kind == agenthub.SenderAgent && strings.Contains(msg.Text, "group hot two") {
				return true
			}
		}
		return false
	})
	if got := p404aReadPayloads(t, marker); len(got) != 1 {
		t.Fatalf("disabled group Stop hook still executed: %d payloads (want 1 from baseline)", len(got))
	}
}

// TestProjectSessionHooksAssembleUserAndProjectLayers N6：setupHooks → dispatcherFor
// 重构后，项目会话仍同时装配用户层与项目层（两层都真的执行一次）。
func TestProjectSessionHooksAssembleUserAndProjectLayers(t *testing.T) {
	m, _ := testManager(t)
	proj := t.TempDir()
	userMarker := filepath.Join(t.TempDir(), "user.jsonl")
	projMarker := filepath.Join(t.TempDir(), "project.jsonl")
	userCmd := p404aAppendCmd(userMarker)
	projCmd := p404aAppendCmd(projMarker)
	p404aWriteUserHooks(t, p404aStopSection(userCmd, 0))
	if err := os.MkdirAll(filepath.Join(proj, ".go-code"), 0o700); err != nil {
		t.Fatal(err)
	}
	projHooks := map[string]any{
		"allowProjectHooks": true,
		"events": map[string]any{"Stop": []any{
			map[string]any{"hooks": []any{map[string]any{"type": "command", "command": projCmd}}},
		}},
	}
	raw, err := json.MarshalIndent(projHooks, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, ".go-code", "hooks.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	trust, err := hooks.LoadTrust(hooksStatePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := trust.Trust(projCmd, "test", "p4-04a project layer"); err != nil {
		t.Fatal(err)
	}

	bs, err := m.create(proj, "s-p404a-project", "")
	if err != nil {
		t.Fatal(err)
	}
	if h := bs.s.Hooks(); h == nil || h.Stop == nil {
		t.Fatalf("project session hooks missing after refactor: %+v", h)
	}
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(&evalProvider{reply: "project done"})))
	if err := bs.s.Ask(context.Background(), core.NewUserMessage(
		core.Content{Type: core.ContentTypeText, Content: "run project session"},
	)); err != nil {
		t.Fatal(err)
	}
	bs.wakeRun()

	for _, marker := range []string{userMarker, projMarker} {
		payloads := p404aWaitPayloads(t, marker, 1)
		if len(payloads) != 1 {
			t.Fatalf("%s: payloads = %d, want 1 (user+project layers must both run)", marker, len(payloads))
		}
		if payloads[0]["cwd"] != proj {
			t.Fatalf("%s: cwd = %v, want %q", marker, payloads[0]["cwd"], proj)
		}
		if payloads[0]["hook_event_name"] != "Stop" {
			t.Fatalf("%s: hook_event_name = %v, want Stop", marker, payloads[0]["hook_event_name"])
		}
	}
}

// TestProjectPathRefusesHubHomeHooks N7：项目装配入口对 Hub home 拒绝式失败，
// setupHooks 也自身兜底不读 hub-home 项目层（防御纵深；命令已信任也必须不执行）。
func TestProjectPathRefusesHubHomeHooks(t *testing.T) {
	m, _ := testManager(t)
	home := hubAgentHome("ag_p404a_guard")
	if err := os.MkdirAll(filepath.Join(home, ".go-code"), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "guard.jsonl")
	cmd := p404aAppendCmd(marker)
	raw, err := json.MarshalIndent(map[string]any{
		"allowProjectHooks": true,
		"events": map[string]any{"Stop": []any{
			map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd}}},
		}},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".go-code", "hooks.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	trust, err := hooks.LoadTrust(hooksStatePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := trust.Trust(cmd, "test", "p4-04a guard"); err != nil {
		t.Fatal(err)
	}

	if d := m.setupHooks(home, nil); d != nil {
		t.Fatalf("setupHooks(hub home) must return nil, got %+v", d)
	}
	if d := m.setupHooks(filepath.Join(home, "nested"), nil); d != nil {
		t.Fatalf("setupHooks(hub home subpath) must return nil, got %+v", d)
	}
	if _, err := m.create(home, "s-p404a-guard", ""); err == nil || !strings.Contains(err.Error(), "Hub") {
		t.Fatalf("create(hub home) error = %v, want clear Hub-home rejection", err)
	}
	if len(m.workspaces) != 0 {
		t.Fatalf("hub home must not create a project runtime, got %v", m.workspaces)
	}
	if payloads := p404aReadPayloads(t, marker); len(payloads) != 0 {
		t.Fatalf("hub-home project hook executed via project path: %+v", payloads)
	}
}
