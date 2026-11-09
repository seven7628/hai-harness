package main

// P4-04a：Hub 外部 AgentEnd/Stop hook 接线（默认关闭，仅用户级）。
//
// 钉住四件事：
//  1. 默认关：没有用户级 hooks 配置 → Hub 会话 hooks 为 nil，run 行为不变；
//  2. 用户级 Stop 配好且可信 → 自然结束（AgentEnd 锚点）恰好执行一次，payload 带
//     Hub 会话 id / cwd / hook_event_name=Stop；
//  3. 未信任 / disableAllHooks / hook 自身失败 → fail-open，任务照常结算；
//  4. 群会话同样接线；Hub home 里的项目级 hooks 绝不被读取（安全边界）。

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
			t.Fatalf("hook payload is not JSON: %q: %v", line, err)
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

// p404aSendDM 发一条 DM 并等到最终回复进索引（AgentEnd 之后，hook 必已执行完）。
func p404aSendDM(t *testing.T, m *manager, buf *syncBuf, cmdID int, p agenthub.AgentProfile, text string) {
	t.Helper()
	ok, errMsg, _ := hubCmd(t, m, buf, cmdID, "hub_send", map[string]any{
		"target": map[string]any{"kind": "dm", "agent_id": string(p.ID)},
		"text":   text,
	})
	if !ok {
		t.Fatalf("hub_send: %s", errMsg)
	}
	msgs := waitHubIndex(t, m, p.ID, 2)
	if msgs[len(msgs)-1].Sender.Kind != agenthub.SenderAgent {
		t.Fatalf("agent reply not indexed: %+v", msgs)
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
	p404aSendDM(t, m, buf, 2, p, "hello")
	if prov.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", prov.callCount())
	}
}

// TestHubSessionStopHookFiresAtNaturalEnd 用户级 Stop：自然结束后恰好一次，payload 对齐。
func TestHubSessionStopHookFiresAtNaturalEnd(t *testing.T) {
	m, buf := testManager(t)
	marker := filepath.Join(t.TempDir(), "stop.jsonl")
	cmd := "cat >> " + p404aShellQuote(marker)
	p404aWriteUserHooks(t, p404aStopSection(cmd, 0))

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
	p404aSendDM(t, m, buf, 2, p, "run once")

	payloads := p404aReadPayloads(t, marker)
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
			p404aWriteUserHooks(t, tc.section("cat >> "+p404aShellQuote(marker)))

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
			p404aSendDM(t, m, buf, 2, p, "go")
			if payloads := p404aReadPayloads(t, marker); len(payloads) != 0 {
				t.Fatalf("guarded hook must not execute, got %+v", payloads)
			}
		})
	}
}

// TestHubSessionStopHookFailureKeepsTaskSettlement hook 失败 fail-open：
// 真实 hub_task 仍从 AgentEnd 结算到 completed（结果卡/回推不因 hook 异常被卡住）。
func TestHubSessionStopHookFailureKeepsTaskSettlement(t *testing.T) {
	m, buf := testManager(t)
	marker := filepath.Join(t.TempDir(), "attempted.log")
	// 先留下"真的跑过"的证据，再以非零退出（default failureMode=fail_open）。
	cmd := "echo ran >> " + p404aShellQuote(marker) + "; exit 3"
	p404aWriteUserHooks(t, p404aStopSection(cmd, 0))

	p := createHubAgent(t, m, buf, 1, "HookTask")
	bs, err := m.hubDMSession(string(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	if h := bs.s.Hooks(); h == nil || h.Stop == nil {
		t.Fatalf("hub session Stop hook not wired: %+v", h)
	}
	prov := &evalProvider{reply: "task payload done"}
	p404aSetDMLoop(t, bs, prov)

	st := mustHubStore(t, m)
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		// 源指向不存在的智能体：故意不触发回推，专注"结算不被 hook 卡住"。
		Origin:      agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: "ag_p404a_ghost"},
		Target:      agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: hubAgentHome(string(p.ID)), SessionID: bs.id},
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
	if err := bs.s.Ask(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	bs.wakeRun()

	deadline := time.Now().Add(20 * time.Second)
	var got *agenthub.Task
	for time.Now().Before(deadline) {
		got, err = st.LoadTask(context.Background(), task.ID)
		if err == nil && got != nil && got.Status.Terminal() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got == nil || got.Status != agenthub.TaskCompleted {
		t.Fatalf("hub task not settled after failing Stop hook: %+v (err=%v)", got, err)
	}
	if raw, rerr := os.ReadFile(marker); rerr != nil || strings.TrimSpace(string(raw)) != "ran" {
		t.Fatalf("failing hook did not run as expected (err=%v, body=%q)", rerr, string(raw))
	}
}

// TestHubGroupSessionStopHookFiresWithGroupSessionID 群会话同样接线，payload 拿群会话 id。
func TestHubGroupSessionStopHookFiresWithGroupSessionID(t *testing.T) {
	m, buf := testManager(t)
	marker := filepath.Join(t.TempDir(), "stop.jsonl")
	p404aWriteUserHooks(t, p404aStopSection("cat >> "+p404aShellQuote(marker), 0))

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

	payloads := p404aReadPayloads(t, marker)
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
	goCodeCmd := "cat >> " + p404aShellQuote(goCodeMarker)
	claudeCmd := "cat >> " + p404aShellQuote(claudeMarker)

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
	p404aSendDM(t, m, buf, 2, p, "run without project hooks")

	for _, marker := range []string{goCodeMarker, claudeMarker} {
		if payloads := p404aReadPayloads(t, marker); len(payloads) != 0 {
			t.Fatalf("project-level hook from hub home executed: %s %+v", marker, payloads)
		}
	}
}
