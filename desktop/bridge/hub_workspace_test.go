package main

// P2-01：WorkspaceOps 适配 + 路径安全。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
)

func createWorkspaceAgent(t *testing.T, m *manager, buf *syncBuf, id int, name string, roots []string) agenthub.AgentProfile {
	t.Helper()
	ok, errMsg, data := hubCmd(t, m, buf, id, "hub_agent_create", map[string]any{
		"profile": map[string]any{
			"name": name, "provider": "deepseek", "model": "deepseek-v4-flash",
			"role": "派发", "policy": map[string]any{"workspace_roots": roots},
		},
	})
	if !ok {
		t.Fatalf("create agent failed: %s", errMsg)
	}
	return agentFromData(t, data)
}

// 新建路径：只允许 workspace_roots 下的新目录；分隔符/.. /已存在/符号链接逃逸全部拒绝。
func TestHubCreateWorkspacePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "projects")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	roots := []string{root}

	realRoot, err := filepath.EvalSymlinks(root) // macOS /tmp → /private/tmp
	if err != nil {
		t.Fatal(err)
	}
	got, err := hubCreateWorkspacePath(root, "alpha", roots)
	if err != nil || got != filepath.Join(realRoot, "alpha") {
		t.Fatalf("happy path = %q, %v", got, err)
	}
	if err := os.Mkdir(got, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := hubCreateWorkspacePath(root, "alpha", roots); !errors.Is(err, agenthub.ErrConflict) {
		t.Fatalf("existing dir must conflict, got %v", err)
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "x\x00y"} {
		if _, err := hubCreateWorkspacePath(root, bad, roots); err == nil {
			t.Errorf("name %q must be rejected", bad)
		}
	}
	outside := filepath.Join(home, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := hubCreateWorkspacePath(outside, "beta", roots); !errors.Is(err, agenthub.ErrForbidden) {
		t.Fatalf("parent outside roots must be forbidden, got %v", err)
	}
	// 符号链接逃逸：root/link → outside，从 link 里建目录必须被拒。
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := hubCreateWorkspacePath(link, "gamma", roots); !errors.Is(err, agenthub.ErrForbidden) {
		t.Fatalf("symlink parent outside roots must be forbidden, got %v", err)
	}
	// 敏感目录（~/.ssh）不能被当成根或父目录。
	ssh := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := hubCreateWorkspacePath(ssh, "leak", []string{ssh}); !errors.Is(err, agenthub.ErrForbidden) {
		t.Fatalf("sensitive parent must be forbidden, got %v", err)
	}
	// 允许根就是 ~/.go-code 时，受管子树（plugins/runtime）以外的子目录同样拒绝。
	cfg := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(filepath.Join(cfg, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := hubCreateWorkspacePath(cfg, "x", []string{home}); err == nil || !errors.Is(err, agenthub.ErrForbidden) {
		t.Fatalf("~/.go-code parent must be forbidden, got %v", err)
	}
}

// CreateWorkspace 从 policy 读白名单；未配置 roots 明确失败（不是「默认可写任意目录」）。
func TestHubWorkspaceOpsCreateAndList(t *testing.T) {
	m, buf := testManager(t)
	root := t.TempDir()
	roots := []string{root}

	noRoots := createWorkspaceAgent(t, m, buf, 1, "OpsNoRoots", nil)
	ops := newHubWorkspaceOps(m)
	if _, err := ops.CreateWorkspace(context.Background(), agenthub.CreateWorkspaceReq{
		AgentID: noRoots.ID, Parent: root, Name: "x",
	}); err == nil {
		t.Fatal("agent without workspace_roots must not be able to create workspaces")
	}

	p := createWorkspaceAgent(t, m, buf, 2, "OpsRoots", roots)
	info, err := ops.CreateWorkspace(context.Background(), agenthub.CreateWorkspaceReq{
		AgentID: p.ID, Parent: root, Name: "proj",
	})
	if err != nil {
		t.Fatalf("create workspace failed: %v", err)
	}
	if st, err := os.Stat(info.Path); err != nil || !st.IsDir() {
		t.Fatalf("workspace not created: %v", err)
	}
	// 重复创建同名 → 冲突（不接管已有目录）。
	if _, err := ops.CreateWorkspace(context.Background(), agenthub.CreateWorkspaceReq{
		AgentID: p.ID, Parent: root, Name: "proj",
	}); !errors.Is(err, agenthub.ErrConflict) {
		t.Fatalf("duplicate create must conflict, got %v", err)
	}
	// 父目录越界 → 禁止。
	if _, err := ops.CreateWorkspace(context.Background(), agenthub.CreateWorkspaceReq{
		AgentID: p.ID, Parent: t.TempDir(), Name: "p2",
	}); !errors.Is(err, agenthub.ErrForbidden) {
		t.Fatalf("out-of-roots create must be forbidden, got %v", err)
	}
	// ListWorkspaces 不含 Hub home。
	if _, err := m.hubDMSession(string(p.ID)); err != nil {
		t.Fatal(err)
	}
	list, err := ops.ListWorkspaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, ws := range list {
		if isHubHomePath(ws.Path) {
			t.Fatalf("ListWorkspaces leaked hub home %q", ws.Path)
		}
	}
}

// 未登记的工作区：ListSessions/CreateSession/Dispatch 一律拒绝，且不得装配 runtime。
func TestHubWorkspaceOpsRejectsUnknownWorkspace(t *testing.T) {
	m, buf := testManager(t)
	p := createWorkspaceAgent(t, m, buf, 1, "OpsStranger", []string{t.TempDir()})
	ops := newHubWorkspaceOps(m)
	stranger := t.TempDir()

	if _, err := ops.ListSessions(context.Background(), stranger); !errors.Is(err, agenthub.ErrNotFound) {
		t.Fatalf("ListSessions(stranger) = %v, want ErrNotFound", err)
	}
	if _, err := ops.CreateSession(context.Background(), agenthub.CreateSessionReq{
		AgentID: p.ID, Workspace: stranger, Title: "x",
	}); !errors.Is(err, agenthub.ErrNotFound) {
		t.Fatalf("CreateSession(stranger) = %v, want ErrNotFound", err)
	}
	if _, err := ops.Dispatch(context.Background(), agenthub.DispatchReq{
		AgentID: p.ID, Workspace: stranger, SessionID: "s1", Text: "hi",
	}); !errors.Is(err, agenthub.ErrNotFound) {
		t.Fatalf("Dispatch(stranger) = %v, want ErrNotFound", err)
	}
	m.mu.Lock()
	_, phantom := m.workspaces[stranger]
	m.mu.Unlock()
	if phantom {
		t.Fatal("rejected workspace must not assemble a runtime")
	}
	// hub home 也不能被当成工作区。
	home := m.hubRuntime(string(p.ID)).path
	if _, err := ops.ListSessions(context.Background(), home); !errors.Is(err, agenthub.ErrNotFound) {
		t.Fatalf("ListSessions(hub home) = %v, want ErrNotFound", err)
	}
}

// 派发到已登记工作区的会话：Ask 入队成功即 accepted；未知 session 明确失败。
func TestHubWorkspaceOpsDispatch(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	p := createWorkspaceAgent(t, m, buf, 1, "OpsDispatch", []string{wsPath})
	ops := newHubWorkspaceOps(m)

	// 登记工作区（模拟 UI 的 workspace_sync / 打开项目）。
	m.dispatch(command{Id: 2, Type: "workspace_sync", Payload: map[string]any{"paths": []any{wsPath}}})
	// 直接装配运行态并建会话（UI 路径同样会经过 new_session；这里只验 Ops 语义）。
	bs, err := m.create(wsPath, "s-dispatch", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ops.Dispatch(context.Background(), agenthub.DispatchReq{
		AgentID: p.ID, Workspace: wsPath, SessionID: "missing", Text: "hi",
	}); !errors.Is(err, agenthub.ErrNotFound) {
		t.Fatalf("dispatch to missing session = %v, want ErrNotFound", err)
	}
	res, err := ops.Dispatch(context.Background(), agenthub.DispatchReq{
		AgentID: p.ID, Workspace: wsPath, SessionID: bs.id, Text: "做一份对比表",
	})
	if err != nil || !res.Accepted || res.SessionID != bs.id {
		t.Fatalf("dispatch = %+v, %v", res, err)
	}
	// 空文本拒绝。
	if _, err := ops.Dispatch(context.Background(), agenthub.DispatchReq{
		AgentID: p.ID, Workspace: wsPath, SessionID: bs.id, Text: "   ",
	}); err == nil {
		t.Fatal("empty dispatch text must be rejected")
	}
}
