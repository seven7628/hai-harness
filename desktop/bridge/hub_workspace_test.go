package main

// P2-01：WorkspaceOps 适配 + 路径安全。

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
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

// 大小写敏感 FS 上也能跑的折叠兜底断言（不依赖 inode）。
func TestHubSensitivePathCaseFoldFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// .kube 不存在 → hubPathWithin 走折叠比较；旧实现（大小写敏感前缀）会返回 false。
	if !hubSensitivePath(filepath.Join(home, ".KUBE")) {
		t.Fatal("hubSensitivePath must fold case when the protected dir does not exist")
	}
	if hubSensitivePath(filepath.Join(home, ".kube2")) {
		t.Fatal("sibling prefix must not be treated as sensitive")
	}
}

// 登记的敏感目录不能出现在 workspace_list（与 knownWorkspace 口径一致）。
func TestHubWorkspaceListFiltersSensitive(t *testing.T) {
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
	p := createWorkspaceAgent(t, m, buf, 1, "FilterSensitive", []string{home})
	ops := newHubWorkspaceOps(m)
	list, err := ops.ListWorkspaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hasWorkspacePath(list, ssh) {
		t.Fatalf("ListWorkspaces leaked sensitive path %q", ssh)
	}
	_ = p
}

// 尾斜杠命中的 runtime 必须按原键寻址：不能另装配一个（runtimes 数量不变且派发成功）。
func TestHubWorkspaceTrailingSlashUsesSameRuntime(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	p := createWorkspaceAgent(t, m, buf, 1, "SlashKey", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"))
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	// 用带尾斜杠的原键装配 runtime（模拟 UI 传参）。
	key := wsPath + string(filepath.Separator)
	if _, err := m.create(key, "s-slash", ""); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	before := len(m.workspaces)
	m.mu.Unlock()
	ops := newHubWorkspaceOps(m)
	res, err := ops.Dispatch(context.Background(), agenthub.DispatchReq{
		AgentID: p.ID, Workspace: wsPath, SessionID: "s-slash", Text: "hi",
	})
	if err != nil || !res.Accepted {
		t.Fatalf("dispatch through trailing-slash key = %+v, %v", res, err)
	}
	m.mu.Lock()
	after := len(m.workspaces)
	m.mu.Unlock()
	if after != before {
		t.Fatalf("knownWorkspace assembled a duplicate runtime: %d -> %d", before, after)
	}
}

// CreateWorkspace 在登记失败时必须删掉刚建的空目录。
func TestHubWorkspaceCreateRollsBackOnRegistryFailure(t *testing.T) {
	m, buf := testManager(t)
	root := t.TempDir()
	// registry 的 path 是目录 → save 必失败。
	bad := filepath.Join(t.TempDir(), "is-a-dir")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	m.wsReg = newWorkspaceRegistry(bad)
	p := createWorkspaceAgent(t, m, buf, 1, "RegFail", []string{root})
	ops := newHubWorkspaceOps(m)
	if _, err := ops.CreateWorkspace(context.Background(), agenthub.CreateWorkspaceReq{
		AgentID: p.ID, Parent: root, Name: "proj",
	}); err == nil {
		t.Fatal("registry save failure must surface")
	}
	if _, err := os.Stat(filepath.Join(root, "proj")); !os.IsNotExist(err) {
		t.Fatalf("created dir left behind: %v", err)
	}
}

// 父路径是普通文件（ENOTDIR）→ ErrInvalid，而不是裸 err。
func TestHubWorkspaceParentNotDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := hubCreateWorkspacePath(file, "proj", []string{root}); !errors.Is(err, agenthub.ErrInvalid) {
		t.Fatalf("ENOTDIR = %v, want ErrInvalid", err)
	}
}

// P2-04 复审 NB-1：坏 header 必须在 client_msg_id 占位之前拒绝；否则同 id
// 的合法重试会命中残留占位，被误报 duplicate（从未投递）。
func TestHubDispatchBadHeaderDoesNotPoisonDedup(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	createWorkspaceAgent(t, m, buf, 1, "BadHeaderAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	if _, err := m.create(wsPath, "s-badheader", ""); err != nil {
		t.Fatal(err)
	}
	ops := newHubWorkspaceOps(m)
	base := agenthub.DispatchReq{
		Workspace: wsPath, SessionID: "s-badheader", Text: "做一件事", ClientMsgID: "bad-header-key",
	}
	bad := base
	bad.TaskHeader = "{not-json"
	if _, err := ops.Dispatch(context.Background(), bad); err == nil {
		t.Fatal("bad header must be rejected")
	}
	good := base
	header, err := agenthub.EncodeTaskHeader(&agenthub.Task{
		ID: "task_probe_badheader", RootTaskID: "task_probe_badheader",
		Origin:   agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: "ag_probe"},
		Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	good.TaskHeader = header
	res, err := ops.Dispatch(context.Background(), good)
	if err != nil {
		t.Fatalf("legal retry after bad header: %v", err)
	}
	if res.Message == agenthub.DispatchDuplicateMessage {
		t.Fatal("bad header poisoned client_msg_id dedup: legal retry saw duplicate")
	}
}

// 场景4：命令与正文同批 → 目标会话 inbox 里命令在前、正文在后（FIFO，同一批）。
func TestHubDispatchCommandBeforeText(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	p := createWorkspaceAgent(t, m, buf, 1, "CmdAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"))
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	bs, err := m.create(wsPath, "s-cmd", "")
	if err != nil {
		t.Fatal(err)
	}
	ops := newHubWorkspaceOps(m)
	res, err := ops.Dispatch(context.Background(), agenthub.DispatchReq{
		AgentID: p.ID, Workspace: wsPath, SessionID: "s-cmd",
		Text: "正文", Command: "compact",
	})
	if err != nil || !res.Accepted {
		t.Fatalf("dispatch = %+v, %v", res, err)
	}
	msgs := bs.s.PendingSnapshot()
	if len(msgs) != 2 {
		t.Fatalf("inbox = %d messages, want command + text", len(msgs))
	}
	if !msgs[0].IsCommandMessage() || !strings.HasPrefix(msgs[0].Content[0].Content, "compact") {
		t.Fatalf("first inbox message = %+v, want the command first", msgs[0])
	}
	if msgs[1].Role != core.User || len(msgs[1].Content) != 1 || msgs[1].Content[0].Content != "正文" {
		t.Fatalf("second inbox message = %+v, want the text", msgs[1])
	}
	if !bs.hubDispatchPending.Load() {
		t.Fatal("dispatch must leave the pending gate set until AgentStart")
	}
}

// 场景4：空正文 + 图片/文件引用也能派发；图片成 image 块，文件只拼 `@路径`。
func TestHubDispatchAttachments(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	p := createWorkspaceAgent(t, m, buf, 1, "AttachAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"))
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	bs, err := m.create(wsPath, "s-att", "")
	if err != nil {
		t.Fatal(err)
	}
	ops := newHubWorkspaceOps(m)
	ctx := context.Background()
	pngPath := filepath.Join(wsPath, "shot.png")
	writeTestPNG(t, pngPath, 8, 8)
	mdPath := filepath.Join(wsPath, "notes.md")
	if err := os.WriteFile(mdPath, []byte("# notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	lastMessage := func() core.Message {
		t.Helper()
		msgs := bs.s.PendingSnapshot()
		if len(msgs) == 0 {
			t.Fatal("no message in inbox")
		}
		return msgs[len(msgs)-1]
	}

	// 纯图片：占位正文 + image 块（正文为空不能变成「空消息」）。
	if _, err := ops.Dispatch(ctx, agenthub.DispatchReq{
		AgentID: p.ID, Workspace: wsPath, SessionID: "s-att", Images: []string{pngPath},
	}); err != nil {
		t.Fatalf("image-only dispatch: %v", err)
	}
	msg := lastMessage()
	if len(msg.Content) != 2 || msg.Content[0].Type != core.ContentTypeText || msg.Content[0].Content != hubImageOnlyText {
		t.Fatalf("image-only content = %+v", msg.Content)
	}
	if msg.Content[1].Type != core.ContentTypeImage || !strings.HasPrefix(msg.Content[1].Content, "data:image/png;base64,") {
		t.Fatalf("image block = %+v", msg.Content[1])
	}

	// 纯文件：正文里只有 `@绝对路径`（不读内容、不塞 data URL）。
	if _, err := ops.Dispatch(ctx, agenthub.DispatchReq{
		AgentID: p.ID, Workspace: wsPath, SessionID: "s-att", Files: []string{mdPath},
	}); err != nil {
		t.Fatalf("file-only dispatch: %v", err)
	}
	msg = lastMessage()
	if len(msg.Content) != 1 || msg.Content[0].Content != "@"+mdPath {
		t.Fatalf("file-only content = %+v", msg.Content)
	}

	// header + 正文 + 文件 + 图片：块序必须是 header → text → image。
	header, err := agenthub.EncodeTaskHeader(&agenthub.Task{
		ID: "task_probe_attach", RootTaskID: "task_probe_attach",
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: "ag_probe"}, Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ops.Dispatch(ctx, agenthub.DispatchReq{
		AgentID: p.ID, Workspace: wsPath, SessionID: "s-att",
		Text: "看这个", Files: []string{mdPath}, Images: []string{pngPath}, TaskHeader: header,
	}); err != nil {
		t.Fatalf("batch dispatch: %v", err)
	}
	msg = lastMessage()
	if len(msg.Content) != 3 {
		t.Fatalf("batch content = %+v", msg.Content)
	}
	if msg.Content[0].Type != core.ContentTypeHubTask || msg.Content[1].Type != core.ContentTypeText || msg.Content[2].Type != core.ContentTypeImage {
		t.Fatalf("block order = %v, %v, %v", msg.Content[0].Type, msg.Content[1].Type, msg.Content[2].Type)
	}
	if msg.Content[1].Content != "看这个\n@"+mdPath {
		t.Fatalf("text block = %q", msg.Content[1].Content)
	}
	if strings.Contains(msg.Content[1].Content, "data:image") {
		t.Fatalf("data URL leaked into the text block: %q", msg.Content[1].Content)
	}
}

// 场景4：坏附件（相对路径/~/.ssh/工作区外/软链逃逸/超限/非图片）全部在占 dedup 之前拒绝。
func TestHubDispatchRejectsBadAttachmentsBeforeDedup(t *testing.T) {
	m, buf := testManager(t)
	home := os.Getenv("HOME")
	wsPath := t.TempDir()
	outside := t.TempDir()
	p := createWorkspaceAgent(t, m, buf, 1, "BadAttachAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"))
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	if _, err := m.create(wsPath, "s-bad", ""); err != nil {
		t.Fatal(err)
	}
	ops := newHubWorkspaceOps(m)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sshKey := filepath.Join(sshDir, "id_rsa")
	if err := os.WriteFile(sshKey, []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 允许根 = home 的智能体：~/.ssh 必须在 hubSensitivePath 这一层被拒（而不是只靠根白名单）。
	homeAgent := createWorkspaceAgent(t, m, buf, 2, "HomeRootAgent", []string{home})

	outsideFile := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(wsPath, "escape.png")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Fatal(err)
	}
	mdPath := filepath.Join(wsPath, "notes.md")
	if err := os.WriteFile(mdPath, []byte("# notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	many := make([]string, 0, agenthub.MaxSessionAttachments+1)
	for i := 0; i <= agenthub.MaxSessionAttachments; i++ {
		p := filepath.Join(wsPath, fmt.Sprintf("f-%d.md", i))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		many = append(many, p)
	}

	cases := []struct {
		name   string
		agent  agenthub.AgentID
		files  []string
		images []string
	}{
		{"relative path", p.ID, []string{"notes.md"}, nil},
		{"home alias", p.ID, []string{"~/.ssh/id_rsa"}, nil},
		{"outside roots", p.ID, []string{outsideFile}, nil},
		{"sensitive ssh", homeAgent.ID, []string{sshKey}, nil},
		{"symlink escape", p.ID, []string{link}, nil},
		{"too many files", p.ID, many, nil},
		{"missing file", p.ID, []string{filepath.Join(wsPath, "nope.md")}, nil},
		{"non-image in images", p.ID, nil, []string{mdPath}},
	}
	ctx := context.Background()
	for i, c := range cases {
		key := fmt.Sprintf("cm-bad-%d", i)
		_, err := ops.Dispatch(ctx, agenthub.DispatchReq{
			AgentID: c.agent, Workspace: wsPath, SessionID: "s-bad",
			Text: "x", Files: c.files, Images: c.images, ClientMsgID: key,
		})
		if err == nil {
			t.Fatalf("%s: dispatch must fail", c.name)
		}
		m.mu.Lock()
		_, seen := m.hubMsgDedup[wsPath+"\x00s-bad\x00"+key]
		m.mu.Unlock()
		if seen {
			t.Fatalf("%s: rejected attachment left a client_msg_id placeholder", c.name)
		}
	}
	// 一个都没投出去：目标会话的 inbox 里应该一条都没有。
	badBS := m.get(wsPath, "s-bad")
	if badBS == nil {
		t.Fatal("session s-bad disappeared")
	}
	if n := badBS.s.PendingCount(); n != 0 {
		t.Fatalf("rejected dispatches reached the session: %d pending", n)
	}
}

// 场景4：clear 与正文同批 → 直接拒绝（clear 会清空同批正文），不占 dedup。
func TestHubDispatchClearWithTextRejected(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	p := createWorkspaceAgent(t, m, buf, 1, "ClearAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"))
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	if _, err := m.create(wsPath, "s-clear", ""); err != nil {
		t.Fatal(err)
	}
	ops := newHubWorkspaceOps(m)
	_, err := ops.Dispatch(context.Background(), agenthub.DispatchReq{
		AgentID: p.ID, Workspace: wsPath, SessionID: "s-clear",
		Text: "正文", Command: "clear", ClientMsgID: "cm-clear",
	})
	if !errors.Is(err, agenthub.ErrInvalid) {
		t.Fatalf("clear+text = %v, want ErrInvalid", err)
	}
	m.mu.Lock()
	_, seen := m.hubMsgDedup[wsPath+"\x00s-clear\x00cm-clear"]
	m.mu.Unlock()
	if seen {
		t.Fatal("rejected clear+text left a client_msg_id placeholder")
	}
}

// 场景4：只有命令走 CommandSession（不建 Task、不进队列）；未知命令被拒。
func TestHubCommandSessionDirect(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	p := createWorkspaceAgent(t, m, buf, 1, "DirectCmdAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"))
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	bs, err := m.create(wsPath, "s-direct", "")
	if err != nil {
		t.Fatal(err)
	}
	ops := newHubWorkspaceOps(m)
	ctx := context.Background()
	if err := ops.CommandSession(ctx, agenthub.CommandSessionReq{
		AgentID: p.ID, Workspace: wsPath, SessionID: "s-direct", Command: "compact",
	}); err != nil {
		t.Fatalf("command session: %v", err)
	}
	msgs := bs.s.PendingSnapshot()
	if len(msgs) != 1 || !msgs[0].IsCommandMessage() {
		t.Fatalf("inbox = %+v, want one command message", msgs)
	}
	if err := ops.CommandSession(ctx, agenthub.CommandSessionReq{
		AgentID: p.ID, Workspace: wsPath, SessionID: "s-direct", Command: "nope",
	}); !errors.Is(err, agenthub.ErrInvalid) {
		t.Fatalf("unknown command = %v, want ErrInvalid", err)
	}
	// 命令通道不碰账本：队列里没有任何任务。
	q, err := m.hubQueueFor()
	if err != nil {
		t.Fatal(err)
	}
	if got := q.QueuedCount(wsPath, "s-direct"); got != 0 {
		t.Fatalf("queued = %d, want 0", got)
	}
}

// 场景4：3 张图的派发在账本/任务事件里只留路径 —— 不含 base64/data URL，事件 < 100KB。
func TestHubSessionSendImageAttachmentsStayOutOfLedger(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	p := createWorkspaceAgent(t, m, buf, 1, "LedgerAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"))
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	bs, err := m.create(wsPath, "s-ledger", "")
	if err != nil {
		t.Fatal(err)
	}
	bs.running.Store(true) // 目标忙 → 任务留在队列，账本里能看到完整快照与事件
	paths := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		p := filepath.Join(wsPath, fmt.Sprintf("shot-%d.png", i))
		writeTestPNG(t, p, 8, 8)
		paths = append(paths, p)
	}
	q, err := m.hubQueueFor()
	if err != nil {
		t.Fatal(err)
	}
	svc := &agenthub.WorkspaceService{Ops: newHubWorkspaceOps(m), AgentID: agenthub.AgentID(p.ID), Queue: q}
	ctx := context.Background()
	res, err := svc.SendBatch(ctx, agenthub.SendInput{
		Workspace: wsPath, SessionID: "s-ledger", Text: "看三张图", Images: paths,
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !res.Queued || res.TaskID == "" {
		t.Fatalf("send result = %+v, want queued with task_id", res)
	}
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	events, err := st.ReadTaskEvents(ctx, agenthub.TaskID(res.TaskID))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= 100*1024 {
		t.Fatalf("task events = %d bytes, want < 100KB", len(raw))
	}
	for _, needle := range []string{"base64", "data:image", "iVBORw0KGgo"} {
		if strings.Contains(string(raw), needle) {
			t.Fatalf("task events contain %q", needle)
		}
	}
	task, err := st.LoadTask(ctx, agenthub.TaskID(res.TaskID))
	if err != nil {
		t.Fatal(err)
	}
	taskJSON, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(taskJSON), "base64") || strings.Contains(string(taskJSON), "data:image") {
		t.Fatalf("task snapshot contains image payload: %s", taskJSON)
	}
	if len(task.Attachments) != 3 || task.Attachments[0].Kind != agenthub.AttachmentKindImage ||
		task.Attachments[0].Path == "" || task.Attachments[0].Bytes <= 0 {
		t.Fatalf("attachments = %+v", task.Attachments)
	}
}

// 场景4：图片总量超 3MiB → 建账前拒绝（不落账、不占 dedup）。
func TestHubSessionSendImageBudgetRejectedBeforeLedger(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	p := createWorkspaceAgent(t, m, buf, 1, "ImageBudgetAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"))
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	if _, err := m.create(wsPath, "s-budget", ""); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		p := filepath.Join(wsPath, fmt.Sprintf("noise-%d.png", i))
		writeTestNoisePNG(t, p, 640, 640)
		paths = append(paths, p)
	}
	ops := newHubWorkspaceOps(m)
	ctx := context.Background()

	// 建账前（CheckAttachments）就拒绝。
	if _, err := ops.CheckAttachments(ctx, agenthub.AttachmentReq{
		AgentID: p.ID, Workspace: wsPath, Images: paths,
	}); !errors.Is(err, agenthub.ErrInvalid) {
		t.Fatalf("CheckAttachments = %v, want ErrInvalid", err)
	}
	q, err := m.hubQueueFor()
	if err != nil {
		t.Fatal(err)
	}
	svc := &agenthub.WorkspaceService{Ops: ops, AgentID: agenthub.AgentID(p.ID), Queue: q}
	if _, err := svc.SendBatch(ctx, agenthub.SendInput{
		Workspace: wsPath, SessionID: "s-budget", Text: "看图", Images: paths,
	}); !errors.Is(err, agenthub.ErrInvalid) {
		t.Fatalf("SendBatch over image budget = %v, want ErrInvalid", err)
	}
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	if tasks, err := st.ListTasks(ctx, agenthub.TaskFilter{}); err != nil || len(tasks) != 0 {
		t.Fatalf("over-budget send must not create tasks: %+v, %v", tasks, err)
	}

	// 派发路径同样在占 dedup 之前拒绝。
	if _, err := ops.Dispatch(ctx, agenthub.DispatchReq{
		AgentID: p.ID, Workspace: wsPath, SessionID: "s-budget",
		Images: paths, ClientMsgID: "cm-big",
	}); !errors.Is(err, agenthub.ErrInvalid) {
		t.Fatalf("Dispatch over image budget = %v, want ErrInvalid", err)
	}
	m.mu.Lock()
	_, seen := m.hubMsgDedup[wsPath+"\x00s-budget\x00cm-big"]
	m.mu.Unlock()
	if seen {
		t.Fatal("over-budget dispatch left a client_msg_id placeholder")
	}
}

// writeTestPNG 写一张可解码的小 PNG。
func writeTestPNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 0xFF // alpha
	}
	writePNG(t, path, img)
}

// writeTestNoisePNG 写一张接近不可压缩的 PNG（用于构造超过图片总量闸门的批次）。
func writeTestNoisePNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if _, err := crand.Read(img.Pix); err != nil {
		t.Fatal(err)
	}
	writePNG(t, path, img)
}

func writePNG(t *testing.T, path string, img image.Image) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatalf("encode png: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
