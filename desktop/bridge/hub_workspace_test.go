package main

// P2-01：WorkspaceOps 适配 + 路径安全。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
func TestHubWorkspaceCreatePath(t *testing.T) {
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
	regPath := filepath.Join(t.TempDir(), "workspaces.json")
	m.wsReg = newWorkspaceRegistry(regPath)
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
	// 落在 workspaces.json：重启后（新 registry 实例）仍能看到，且不必先装配 runtime。
	reloaded := newWorkspaceRegistry(regPath).list()
	found := false
	for _, e := range reloaded {
		if e.Path == info.Path {
			found = true
		}
	}
	if !found {
		t.Fatalf("workspace %q 未写入 workspaces.json: %+v", info.Path, reloaded)
	}
	list, err := ops.ListWorkspaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !hasWorkspacePath(list, info.Path) {
		t.Fatalf("ListWorkspaces 漏掉新建但未装配的工作区: %+v", list)
	}
	// 已登记但未装配：可以开会话（装配后 Dispatch 才有目标）。
	if _, ok := m.workspaces[info.Path]; ok {
		t.Fatal("CreateWorkspace 不应顺手装配 runtime（装配留给首次使用）")
	}
	activeBefore := m.provCfg.providerName()
	sessInfo, err := ops.CreateSession(context.Background(), agenthub.CreateSessionReq{
		AgentID: p.ID, Workspace: info.Path, Title: "首个会话",
	})
	if err != nil || sessInfo.ID == "" {
		t.Fatalf("CreateSession in fresh workspace = %+v, %v", sessInfo, err)
	}
	// 标题必须真落盘（session_manual_titles）：只回显会让 list 显示 New Chat。
	rows, err := ops.ListSessions(context.Background(), info.Path)
	if err != nil || len(rows) != 1 || rows[0].Title != "首个会话" {
		t.Fatalf("session title not persisted: %+v, %v", rows, err)
	}
	if got := m.provCfg.providerName(); got != activeBefore {
		t.Fatalf("CreateSession changed global active provider: %q -> %q", activeBefore, got)
	}
	// ListWorkspaces 不含 Hub home。
	if _, err := m.hubDMSession(string(p.ID)); err != nil {
		t.Fatal(err)
	}
	list, err = ops.ListWorkspaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, ws := range list {
		if isHubHomePath(ws.Path) {
			t.Fatalf("ListWorkspaces leaked hub home %q", ws.Path)
		}
	}
}

func hasWorkspacePath(list []agenthub.WorkspaceInfo, path string) bool {
	for _, ws := range list {
		if ws.Path == path {
			return true
		}
	}
	return false
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
	if _, err := ops.ListSessions(context.Background(), home); !errors.Is(err, agenthub.ErrForbidden) {
		t.Fatalf("ListSessions(hub home) = %v, want ErrForbidden", err)
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
	// 中断：已存在的会话幂等成功（没有在跑也不报错），未知 sid 明确失败。
	if err := ops.InterruptSession(context.Background(), bs.id); err != nil {
		t.Fatalf("interrupt idle session = %v, want nil", err)
	}
	if err := ops.InterruptSession(context.Background(), "missing"); !errors.Is(err, agenthub.ErrNotFound) {
		t.Fatalf("interrupt missing session = %v, want ErrNotFound", err)
	}
	if err := ops.InterruptSession(context.Background(), " "); err == nil {
		t.Fatal("interrupt with empty sid must fail")
	}
}

// 敏感目录必须按 inode 判（macOS 大小写不敏感：~/.SSH 与 ~/.ssh 是同一目录）。
func TestHubWorkspaceSensitiveCaseVariant(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ssh := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	upper := filepath.Join(home, ".SSH")
	lowerInfo, err := os.Stat(ssh)
	if err != nil {
		t.Fatal(err)
	}
	upperInfo, err := os.Stat(upper)
	if err != nil || !os.SameFile(lowerInfo, upperInfo) {
		t.Skip("文件系统大小写敏感，跳过大小写变体用例")
	}
	if !hubSensitivePath(upper) {
		t.Fatal("hubSensitivePath must treat ~/.SSH as ~/.ssh (case-insensitive FS)")
	}
	if _, err := hubCreateWorkspacePath(upper, "proj", []string{home}); !errors.Is(err, agenthub.ErrForbidden) {
		t.Fatalf("create under ~/.SSH = %v, want ErrForbidden", err)
	}
}

// 超长名字（>64 rune）必须拒绝（P2-01 复审 M6）。
func TestHubWorkspaceNameTooLong(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "projects")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("名", 65)
	if _, err := hubCreateWorkspacePath(root, long, []string{root}); !errors.Is(err, agenthub.ErrInvalid) {
		t.Fatalf("65-rune name = %v, want ErrInvalid", err)
	}
}

// 没有 registry 时不能「假成功」：目录不能留下，错误要可读（P2-01 复审 N3）。
func TestHubWorkspaceRequiresRegistry(t *testing.T) {
	m, buf := testManager(t)
	root := t.TempDir()
	p := createWorkspaceAgent(t, m, buf, 1, "NoRegistry", []string{root})
	ops := newHubWorkspaceOps(m) // m.wsReg == nil
	_, err := ops.CreateWorkspace(context.Background(), agenthub.CreateWorkspaceReq{
		AgentID: p.ID, Parent: root, Name: "proj",
	})
	if err == nil || !strings.Contains(err.Error(), "登记未初始化") {
		t.Fatalf("create without registry = %v, want 登记未初始化", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "proj")); !os.IsNotExist(statErr) {
		t.Fatalf("directory left behind: %v", statErr)
	}
}

// registry 里被登记成敏感目录（手机/UI 的 workspace_add 不查敏感目录）也必须拒绝（N5）。
func TestHubWorkspaceRegistrySensitiveRejected(t *testing.T) {
	m, buf := testManager(t)
	home := os.Getenv("HOME")
	ssh := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	m.wsReg = newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"))
	if _, err := m.wsReg.add(ssh); err != nil {
		t.Fatal(err)
	}
	p := createWorkspaceAgent(t, m, buf, 1, "SensitiveRegistry", []string{home})
	ops := newHubWorkspaceOps(m)
	if _, err := ops.ListSessions(context.Background(), ssh); !errors.Is(err, agenthub.ErrForbidden) {
		t.Fatalf("ListSessions(~/.ssh) = %v, want ErrForbidden", err)
	}
	if _, err := ops.CreateSession(context.Background(), agenthub.CreateSessionReq{
		AgentID: p.ID, Workspace: ssh,
	}); !errors.Is(err, agenthub.ErrForbidden) {
		t.Fatalf("CreateSession(~/.ssh) = %v, want ErrForbidden", err)
	}
}

// Hub home 即使被塞进 m.workspaces / workspaces.json 也不能当工作区（复审 M9/M10）。
func TestHubWorkspaceNeverTreatsHubHomeAsWorkspace(t *testing.T) {
	m, buf := testManager(t)
	p := createWorkspaceAgent(t, m, buf, 1, "HubHomeGuard", []string{t.TempDir()})
	home := m.hubRuntime(string(p.ID)).path
	m.wsReg = newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"))
	if _, err := m.wsReg.add(home); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.workspaces[home] = &workspaceRuntime{kind: runtimeProject, path: home, sessions: map[string]*bridgeSession{}}
	m.mu.Unlock()
	ops := newHubWorkspaceOps(m)
	list, err := ops.ListWorkspaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hasWorkspacePath(list, home) {
		t.Fatalf("ListWorkspaces leaked hub home %q", home)
	}
	if _, err := ops.ListSessions(context.Background(), home); !errors.Is(err, agenthub.ErrForbidden) {
		t.Fatalf("ListSessions(hub home) = %v, want ErrForbidden", err)
	}
}

// 未登记工作区的错误必须区分于「会话不存在」（复审 mutation M4 的断言缺口）。
func TestHubWorkspaceStrangerErrorMentionsUnregistered(t *testing.T) {
	m, buf := testManager(t)
	p := createWorkspaceAgent(t, m, buf, 1, "StrangerText", []string{t.TempDir()})
	ops := newHubWorkspaceOps(m)
	_, err := ops.Dispatch(context.Background(), agenthub.DispatchReq{
		AgentID: p.ID, Workspace: t.TempDir(), SessionID: "s1", Text: "hi",
	})
	if err == nil || !strings.Contains(err.Error(), "未登记") {
		t.Fatalf("dispatch(stranger) = %v, want 未登记", err)
	}
}

// policy.workspace_roots 支持 ~ 展开（文档默认值就是 ~/hai-workspaces）。
func TestHubWorkspaceExpandsHomeRoot(t *testing.T) {
	m, buf := testManager(t)
	home := os.Getenv("HOME")
	root := filepath.Join(home, "spaces")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	m.wsReg = newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"))
	p := createWorkspaceAgent(t, m, buf, 1, "HomeRoot", []string{"~/spaces"})
	ops := newHubWorkspaceOps(m)
	info, err := ops.CreateWorkspace(context.Background(), agenthub.CreateWorkspaceReq{
		AgentID: p.ID, Parent: "~/spaces", Name: "proj",
	})
	if err != nil {
		t.Fatalf("create under ~ root: %v", err)
	}
	realRoot, err := filepath.EvalSymlinks(root) // macOS /tmp → /private/tmp
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(realRoot, "proj"); info.Path != want {
		t.Fatalf("path = %q, want %q", info.Path, want)
	}
}

// parent 省略 = 第一个 workspace root（工具 schema 只要求 name）。
func TestHubWorkspaceCreateDefaultsParent(t *testing.T) {
	m, buf := testManager(t)
	root := t.TempDir()
	m.wsReg = newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"))
	p := createWorkspaceAgent(t, m, buf, 1, "DefaultParent", []string{root})
	ops := newHubWorkspaceOps(m)
	info, err := ops.CreateWorkspace(context.Background(), agenthub.CreateWorkspaceReq{
		AgentID: p.ID, Name: "auto",
	})
	if err != nil {
		t.Fatal(err)
	}
	realRoot, _ := filepath.EvalSymlinks(root)
	if want := filepath.Join(realRoot, "auto"); info.Path != want {
		t.Fatalf("path = %q, want %q", info.Path, want)
	}
}

// client_msg_id 进程内幂等：同一个 id 只投递一次，且不会因重试改 task 归属。
func TestHubWorkspaceDispatchDedupClientMsgID(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	p := createWorkspaceAgent(t, m, buf, 1, "DedupAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"))
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	if _, err := m.create(wsPath, "s-dedup", ""); err != nil {
		t.Fatal(err)
	}
	ops := newHubWorkspaceOps(m)
	req := agenthub.DispatchReq{
		AgentID: p.ID, Workspace: wsPath, SessionID: "s-dedup",
		Text: "只该执行一次", ClientMsgID: "cm-1",
	}
	if _, err := ops.Dispatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	second, err := ops.Dispatch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if second.Message != "duplicate" {
		t.Fatalf("second dispatch = %+v, want duplicate", second)
	}
	m.mu.Lock()
	seen := len(m.hubMsgDedup)
	m.mu.Unlock()
	if seen != 1 {
		t.Fatalf("dedup entries = %d, want 1", seen)
	}
	// 不同 id 正常投递。
	req.ClientMsgID = "cm-2"
	res, err := ops.Dispatch(context.Background(), req)
	if err != nil || res.Message == "duplicate" {
		t.Fatalf("different client id = %+v, %v", res, err)
	}
	m.mu.Lock()
	seen = len(m.hubMsgDedup)
	m.mu.Unlock()
	if seen != 2 {
		t.Fatalf("dedup entries after distinct id = %d, want 2", seen)
	}
}
