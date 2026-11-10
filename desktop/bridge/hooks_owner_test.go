package main

// hooks 信任的归属（owner）隔离回归（BR-21 产品决策）。
//
// 两条负例是本文件的重点：
//  1. A 智能体确认过的命令，在 B 智能体的派发里必须仍是「未信任」（不执行）；
//  2. A 群的确认不得让同一 agent 的 DM 免确认（DM 与群是不同的 owner）。

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/seven7628/hai-harness/hooks"
	"github.com/seven7628/hai-harness/owner"
)

// TestHooksTrustCommandRequiresOwner 没有归属就拒绝：绝不把确认写成全局。
func TestHooksTrustCommandRequiresOwner(t *testing.T) {
	m, buf := testManager(t)
	ok, errMsg := hooksTrustCmd(t, m, buf, map[string]any{
		"commands": []any{"echo hi"},
		"scope":    "project",
	})
	if ok {
		t.Fatal("缺少 owner 时必须拒绝（不能默认写成全局）")
	}
	if errMsg == "" {
		t.Fatal("拒绝必须有可解释的原因")
	}
	// 显式非法 owner 同样拒绝（不静默回落）。
	if ok, _ := hooksTrustCmd(t, m, buf, map[string]any{
		"owner": "global", "commands": []any{"echo hi"},
	}); ok {
		t.Fatal("非法 owner 必须拒绝")
	}
}

// TestHooksTrustOwnerFromPayloadAndSession 归属派生：owner > session_id > workspace。
func TestHooksTrustOwnerFromPayloadAndSession(t *testing.T) {
	m, buf := testManager(t)
	store := m.hookTrustStore()

	// 显式 owner
	if ok, errMsg := hooksTrustCmd(t, m, buf, map[string]any{
		"owner": owner.Agent("a"), "commands": []any{"echo a"}, "scope": "project",
	}); !ok {
		t.Fatalf("显式 owner 应写入成功: %s", errMsg)
	}
	if got := store.Check(owner.Agent("a"), "echo a"); got != hooks.TrustTrusted {
		t.Fatalf("agent:a 应 trusted，得到 %q", got)
	}
	if got := store.Check(owner.Agent("b"), "echo a"); got != hooks.TrustUntrusted {
		t.Fatalf("agent:b 不得 trusted（A 的确认不影响 B），得到 %q", got)
	}

	// session_id 派生（Hub 群会话 → group）
	if ok, errMsg := hooksTrustCmd(t, m, buf, map[string]any{
		"session_id": "hub.a.g.g1", "commands": []any{"echo g"}, "scope": "project",
	}); !ok {
		t.Fatalf("session_id 派生应成功: %s", errMsg)
	}
	if got := store.Check(owner.Group("g1"), "echo g"); got != hooks.TrustTrusted {
		t.Fatalf("group:g1 应 trusted，得到 %q", got)
	}
	if got := store.Check(owner.Agent("a"), "echo g"); got != hooks.TrustUntrusted {
		t.Fatalf("同一 agent 的 DM 不得继承群确认，得到 %q", got)
	}

	// workspace 派生
	if ok, errMsg := hooksTrustCmd(t, m, buf, map[string]any{
		"workspace": "/proj/x", "commands": []any{"echo w"}, "scope": "project",
	}); !ok {
		t.Fatalf("workspace 派生应成功: %s", errMsg)
	}
	if got := store.Check(owner.Workspace("/proj/x"), "echo w"); got != hooks.TrustTrusted {
		t.Fatalf("workspace owner 应 trusted，得到 %q", got)
	}
	if got := store.Check(owner.Workspace("/proj/y"), "echo w"); got != hooks.TrustUntrusted {
		t.Fatalf("别的项目不得继承，得到 %q", got)
	}
}

// TestHooksListReportsOwnerPerSource hooks_list：项目来源按**该项目**归属判定，
// 用户级来源按当前会话归属判定，响应里带 owner 让 UI 能显示「确认给谁」。
func TestHooksListReportsOwnerPerSource(t *testing.T) {
	m, buf := testManager(t)
	proj := t.TempDir()
	if err := writeFileJSON(projectHooksPath(proj), map[string]any{
		"events": map[string]any{"Stop": []any{map[string]any{"hooks": []any{
			map[string]any{"type": "command", "command": "echo proj"},
		}}}},
	}); err != nil {
		t.Fatal(err)
	}
	// 只信任 workspace:<proj>，不信任 agent:a
	if err := m.hookTrustStore().Trust(owner.Workspace(proj), "echo proj", "project", "test"); err != nil {
		t.Fatal(err)
	}

	ok, errMsg, data := runCmd(t, m, buf, CmdHooksList, map[string]any{
		"workspaces": []any{proj},
		"session_id": "hub.a.dm",
	})
	if !ok {
		t.Fatalf("hooks_list 失败: %s", errMsg)
	}
	if data["owner"] != owner.Agent("a") {
		t.Fatalf("当前归属应为 agent:a，得到 %v", data["owner"])
	}
	projects, _ := data["projects"].([]any)
	if len(projects) != 1 {
		t.Fatalf("projects 数量不对: %#v", data["projects"])
	}
	p0, _ := projects[0].(map[string]any)
	if p0["owner"] != owner.Workspace(proj) {
		t.Fatalf("项目来源归属应为 workspace:<proj>，得到 %v", p0["owner"])
	}
	trustMap, _ := p0["trust"].(map[string]any)
	if trustMap["echo proj"] != string(hooks.TrustTrusted) {
		t.Fatalf("项目来源按自己的 owner 判定应 trusted，得到 %v", trustMap)
	}
}

// TestHooksRunnerChecksOwner 运行期闸门：同一个 dispatcher，换 owner 装配，
// 只有被确认的那一个 owner 会真的跑命令。
func TestHooksRunnerChecksOwner(t *testing.T) {
	m, _ := testManager(t) // 只需要隔离 HOME 的信任库
	store := m.hookTrustStore()
	if err := store.Trust(owner.Agent("a"), "true", "project", "test"); err != nil {
		t.Fatal(err)
	}
	// 用**项目级**来源构造配置：项目层必须显式信任（user/inline 层默认可信，
	// 那样测不出信任闸门）。走 hooks.Load 才能拿到 scope=project 的 group。
	proj := t.TempDir()
	if err := writeFileJSON(filepath.Join(proj, ".go-code", "hooks.json"), map[string]any{
		"allowProjectHooks": true,
		"events": map[string]any{"Stop": []any{map[string]any{"hooks": []any{
			map[string]any{"type": "command", "command": "true"},
		}}}},
	}); err != nil {
		t.Fatal(err)
	}
	cfg, warns := hooks.Load(hooks.LoadOptions{WorkspaceDir: proj})
	if len(warns) > 0 {
		t.Fatalf("配置告警: %v", warns)
	}
	if len(cfg.Events[hooks.EventStop]) == 0 {
		t.Fatal("项目层 Stop 未加载")
	}

	dA := hooks.NewDispatcher(cfg, store, func(string, string) {})
	outA := dA.Runner().Dispatch(context.Background(), hooks.EventStop,
		hooks.Payload{SessionID: "hub.a.dm", Owner: owner.Agent("a")}, hooks.MatchValue{})
	if len(outA.Runs) != 1 {
		t.Fatalf("agent:a 已确认 → 应执行一次，得到 runs=%d warnings=%v", len(outA.Runs), outA.Warnings)
	}

	dB := hooks.NewDispatcher(cfg, store, func(string, string) {})
	outB := dB.Runner().Dispatch(context.Background(), hooks.EventStop,
		hooks.Payload{SessionID: "hub.b.dm", Owner: owner.Agent("b")}, hooks.MatchValue{})
	if len(outB.Runs) != 0 {
		t.Fatalf("agent:b 未确认 → 不得执行，得到 runs=%d", len(outB.Runs))
	}
	if len(outB.Warnings) == 0 {
		t.Fatal("跳过必须有可解释的告警（含 owner）")
	}

	// 缺归属（空 owner）同样不得执行：不猜归属。
	outU := hooks.NewDispatcher(cfg, store, func(string, string) {}).
		Runner().Dispatch(context.Background(), hooks.EventStop, hooks.Payload{}, hooks.MatchValue{})
	if len(outU.Runs) != 0 {
		t.Fatalf("无归属不得执行，得到 runs=%d", len(outU.Runs))
	}
}

// TestHooksListRejectsInvalidOwner 显式非法 owner 必须**响亮失败**：
// 返回 200 + 空 owner 会让前端把「确认」按钮接到另一个归属（或让服务端把 owner 判成空），
// 而用户看到的是「点过了没反应」。缺省（不带 owner）仍要能列出来（只读聚合）。
func TestHooksListRejectsInvalidOwner(t *testing.T) {
	m, buf := testManager(t)
	ok, errMsg, _ := runCmd(t, m, buf, CmdHooksList, map[string]any{
		"workspaces": []any{t.TempDir()},
		"owner":      "global",
	})
	if ok {
		t.Fatal("显式非法 owner 必须报错，不能 200 + 空归属")
	}
	if errMsg == "" {
		t.Fatal("报错必须有可解释的原因")
	}
	// 不带 owner：只读聚合照常（当前归属为空 → 前端显示「未绑定」，写操作才会被拒）
	ok2, err2, data := runCmd(t, m, buf, CmdHooksList, map[string]any{"workspaces": []any{}})
	if !ok2 {
		t.Fatalf("缺省 owner 的 hooks_list 应成功: %s", err2)
	}
	if data["owner"] != "" {
		t.Fatalf("没有上下文时 owner 应为空，得到 %v", data["owner"])
	}
}
