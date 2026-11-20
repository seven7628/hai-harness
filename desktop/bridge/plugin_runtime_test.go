package main

// plugin_runtime_test.go —— 插件虚拟 Project 的 Go 侧验收（09-agent-bridge §4 / §7）。
//
// 这个文件钉住的是**分流表与不变式**，不是「装配能跑」：
//
//	1. runtimePlugin 是第三种容器：不在 m.workspaces、不进 workspace_list、knownWorkspace 失败；
//	2. record 键 = plugin-<id>，journal/codemode 落在 plugin-sessions/<id>/sessions；
//	3. hooks 只读用户级（插件 home 里的 .go-code/hooks.json 不生效）；
//	4. 现有会话命令（new_session/list/switch_model/delete_session）在虚拟路径上原样可用，
//	   删除只删目标会话；
//	5. 停用/卸载后 ask 前置拒绝（封闭 reason），会话仍可列/可删；
//	6. plugin_projects_list 覆盖「已安装无会话 / 已安装有会话 / 已卸载有会话」；
//	7. denylist：插件引擎没有越权工具、仍保留 memory_dream；项目引擎没有 plugin__*；
//	8. 负向守卫：workspaceSync/workspaceAdd 拒绝虚拟路径，workspaces.json 不变。
//
// 全部从**生产入口**进（dispatch / pluginRuntimeForHome / buildPluginEngine），不绕开命令层
// 直接改表 —— 否则「命令能不能用」这件事根本没被测到（00-conventions §4.7）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/cron"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/im"
	implugin "github.com/seven7628/hai-harness/plugin/im"
	"github.com/seven7628/hai-harness/session"
	"github.com/seven7628/hai-harness/skills"
	"github.com/seven7628/hai-harness/subagent"
)

// pluginResp 发一条命令并取回它的 command_response（缓冲先清空，避免读到上一条）。
func pluginResp(t *testing.T, m *manager, buf *syncBuf, id int, typ string, payload map[string]any) map[string]any {
	t.Helper()
	buf.Reset()
	m.dispatch(cmd(id, typ, payload))
	var resp map[string]any
	for _, line := range readLines(buf) {
		if line["event_type"] == "command_response" && intNumOf(line["id"]) == id {
			resp = line
		}
	}
	if resp == nil {
		t.Fatalf("%s 没有 id=%d 的 command_response", typ, id)
	}
	return resp
}

func pluginRespData(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	d, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 data：%#v", resp)
	}
	return d
}

// pluginSessionIDs 从 list 回执里取会话 id 集合。
func pluginSessionIDs(t *testing.T, data map[string]any) map[string]bool {
	t.Helper()
	raw, ok := data["sessions"].([]any)
	if !ok {
		t.Fatalf("list 回执缺 sessions：%#v", data)
	}
	out := map[string]bool{}
	for _, it := range raw {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := m["id"].(string); id != "" {
			out[id] = true
		}
	}
	return out
}

// pluginManager 带 workspaces.json 的 manager（testManager 的零值 surfaceStore 也指向临时 appData）。
func pluginManager(t *testing.T) (*manager, *syncBuf) {
	t.Helper()
	m, buf := testManager(t)
	m.wsReg = newWorkspaceRegistry(filepath.Join(appDataDir(), "workspaces.json"))
	return m, buf
}

// waitPluginFile 等一个文件出现（run 的落盘是异步的）。
func waitPluginFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等不到文件：%s", path)
}

// runPluginTurn 让插件会话真跑一轮（loop 换成 evalProvider），产生磁盘会话/record。
func runPluginTurn(t *testing.T, m *manager, buf *syncBuf, home, sid, reply string) {
	t.Helper()
	bs := m.get(home, sid)
	if bs == nil {
		t.Fatalf("插件会话 %s 不在运行态", sid)
	}
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(&evalProvider{reply: reply})))
	resp := pluginResp(t, m, buf, 900, "ask", map[string]any{
		"workspace": home, "session_id": sid, "text": "hi",
	})
	if resp["ok"] != true {
		t.Fatalf("ask 失败：%v", resp)
	}
	waitPluginFile(t, filepath.Join(pluginSessionsDir(bs.runtime.pluginID), sid+".jsonl"))
}

// pluginSurface 一条 surface 声明（测试用）。commands 可空（= manifest 不声明任何命令）。
type pluginSurface struct {
	id       string
	enabled  bool
	commands []string
}

// writePluginSurfaces 一次性写 surface 目录 + index.json。
//
// 不能用 fakeSurface 连调：它每次都重写整份 index（单条），第二个会覆盖第一个，
// 于是「同时装了 A 和 B」这件事根本不存在 —— 这正是 09 §4.1 要列多种状态的场景。
func writePluginSurfaces(t *testing.T, m *manager, items ...pluginSurface) {
	t.Helper()
	root := m.surfaces.Dir()
	entries := make([]map[string]any, 0, len(items))
	for _, it := range items {
		dir := filepath.Join(root, it.id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("建 %s 失败: %v", dir, err)
		}
		cmds := it.commands
		if cmds == nil {
			cmds = []string{}
		}
		mf := map[string]any{
			"id": it.id, "name": map[string]string{"zh": it.id}, "version": "1.0.0",
			"api": 1, "entry": "panel.js", "commands": cmds, "data": []string{},
		}
		raw, err := json.Marshal(mf)
		if err != nil {
			t.Fatalf("marshal manifest: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "surface.json"), raw, 0o600); err != nil {
			t.Fatalf("写 surface.json: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "panel.js"), []byte("export const surface = {}\n"), 0o600); err != nil {
			t.Fatalf("写 panel.js: %v", err)
		}
		entries = append(entries, map[string]any{
			"id": it.id, "version": "1.0.0", "hash": strings.Repeat("a", 64),
			"source": "/tmp/src-" + it.id, "enabled": it.enabled,
			"installedAt": "2026-11-12T00:00:00Z",
		})
	}
	idx := map[string]any{"version": 1, "items": entries}
	raw, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.json"), raw, 0o600); err != nil {
		t.Fatalf("写 index.json: %v", err)
	}
}

// pluginToolsFixture 造一份最小工具装配输入（与 plugins_test.go 同款）。
func pluginToolsFixture(t *testing.T) (*subagent.Registry, *events.QuestionWaiter, *skills.Registry) {
	t.Helper()
	sk, err := skills.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return subagent.NewRegistry(), events.NewQuestionWaiter(0), sk
}

// ---- ① 第三种容器：runtimePlugin 不进 m.workspaces ----

func TestPluginRuntimeIsNotProjectWorkspace(t *testing.T) {
	m, buf := pluginManager(t)
	fakeSurface(t, m, "demo", true, nil)

	home := pluginHomePath("demo")
	if !isPluginHomePath(home) {
		t.Fatalf("%q 应被判定为插件 home 路径", home)
	}
	ws, err := m.pluginRuntimeForHome(home)
	if err != nil {
		t.Fatalf("pluginRuntimeForHome: %v", err)
	}
	if ws.kind != runtimePlugin {
		t.Fatalf("kind = %v, want runtimePlugin", ws.kind)
	}
	if ws.pluginID != "demo" || ws.path != home {
		t.Fatalf("runtime = id %q path %q", ws.pluginID, ws.path)
	}
	// 存储根：home/sessions/events 三兄弟都在 plugin-sessions/<id> 下（09 §4.1）。
	if ws.storeDir != pluginSessionsDir("demo") || ws.eventsDir != pluginEventsDir("demo") {
		t.Fatalf("store/events = %q / %q", ws.storeDir, ws.eventsDir)
	}
	for _, dir := range []string{ws.path, ws.storeDir, ws.eventsDir} {
		st, serr := os.Stat(dir)
		if serr != nil || !st.IsDir() {
			t.Fatalf("目录没建出来：%s (%v)", dir, serr)
		}
	}
	// 不在 m.workspaces（那些遍历全是项目语义）
	m.mu.Lock()
	_, leaked := m.workspaces[home]
	projectCount := len(m.workspaces)
	m.mu.Unlock()
	if leaked || projectCount != 0 {
		t.Fatalf("插件虚拟 Project 混进了 m.workspaces：%v", m.workspaces)
	}
	// m.runtime 命中插件表 → 同一个实例
	if got := m.runtime(home); got != ws {
		t.Fatalf("m.runtime(plugin home) = %p, want %p", got, ws)
	}

	// workspace_list 看不到（虚拟路径不是项目）
	resp := pluginResp(t, m, buf, 1, "workspace_list", nil)
	list, _ := pluginRespData(t, resp)["workspaces"].([]workspaceEntry)
	for _, e := range list {
		if e.Path == home {
			t.Fatalf("workspace_list 返回了插件虚拟路径：%v", e.Path)
		}
	}

	// knownWorkspace 一律拒绝（Hub 不能把插件 home 当派发目标）
	ops := newHubWorkspaceOps(m)
	if p, kerr := ops.knownWorkspace(home); kerr == nil {
		t.Fatalf("knownWorkspace(%q) 应失败，得到 %q", home, p)
	}
	wss, werr := ops.ListWorkspaces(context.Background())
	if werr != nil {
		t.Fatalf("ListWorkspaces: %v", werr)
	}
	for _, info := range wss {
		if info.Path == home {
			t.Fatalf("Hub ListWorkspaces 返回了插件虚拟路径：%s", info.Path)
		}
	}

	// 未装配的插件 home：m.runtime 返回 nil，且**不**造项目 runtime（09 §4.3 最后一行）
	unknown := pluginHomePath("never-opened")
	if got := m.runtime(unknown); got != nil {
		t.Fatalf("未装配插件 home 的 runtime = %v, want nil", got)
	}
	m.mu.Lock()
	_, fabricated := m.workspaces[unknown]
	stillProject := len(m.workspaces)
	m.mu.Unlock()
	if fabricated || stillProject != projectCount {
		t.Fatalf("m.runtime 为插件 home 造出了项目 runtime：%v", m.workspaces)
	}
	// createOpts 同样硬拒绝（不是过滤成项目会话）
	if _, cerr := m.createOpts(home, "s-should-fail", "", sessionOpts{}); cerr == nil {
		t.Fatal("createOpts(插件 home) 应拒绝")
	}
}

// ---- ② record 键 + 存储根 ----

func TestPluginSessionRecordKeyAndSessionsRoot(t *testing.T) {
	m, buf := pluginManager(t)
	fakeSurface(t, m, "demo", true, nil)
	home := pluginHomePath("demo")

	ws, err := m.pluginRuntimeForHome(home)
	if err != nil {
		t.Fatal(err)
	}
	// 09 §4.3 第 2 点：record 键锚在 pluginID 上（不是 home 路径哈希）。
	if got := sessionRecordKey(ws, home, sessionOpts{}); got != "plugin-demo" {
		t.Fatalf("sessionRecordKey = %q, want plugin-demo", got)
	}
	// 对照：同一路径当项目会话时键不同（证明上面不是「恒等于某个常量」）。
	if got := sessionRecordKey(&workspaceRuntime{path: home}, home, sessionOpts{}); got == "plugin-demo" {
		t.Fatalf("项目 runtime 的 record 键错误地等于 plugin-demo")
	}

	sid := newSessionID(t, m, buf, home)
	bs := m.get(home, sid)
	if bs == nil || bs.runtime == nil {
		t.Fatalf("插件会话 %s 不在运行态", sid)
	}
	if bs.runtime.kind != runtimePlugin || bs.runtime.pluginID != "demo" {
		t.Fatalf("会话 runtime = kind %v id %q", bs.runtime.kind, bs.runtime.pluginID)
	}
	if bs.wsKey != "plugin-demo" {
		t.Fatalf("会话 wsKey = %q, want plugin-demo", bs.wsKey)
	}
	// journal（子任务输出）：sessionsRoot 必须是 plugin-sessions/<id>/sessions。
	root := bs.runtime.sessionsRoot()
	if root != pluginSessionsDir("demo") {
		t.Fatalf("sessionsRoot = %q, want %q", root, pluginSessionsDir("demo"))
	}
	jpath, jerr := journalPathFor(root, sid, "task-abcdefgh")
	if jerr != nil {
		t.Fatalf("journalPathFor: %v", jerr)
	}
	if !strings.HasPrefix(jpath, pluginSessionsDir("demo")+string(filepath.Separator)) {
		t.Fatalf("journal 落在 %q，不在 plugin-sessions/demo/sessions 下", jpath)
	}
	if st, serr := os.Stat(filepath.Dir(jpath)); serr != nil || !st.IsDir() {
		t.Fatalf("journal 目录没建出来：%s (%v)", filepath.Dir(jpath), serr)
	}
	// codemode store 同样落在会话子树根下（09 §4.3 的 codemode.go:232 一行）。
	if _, note := codemodeStore(root, sid); note != "" {
		t.Fatalf("codemode store 退化：%s", note)
	}
	if _, serr := os.Stat(filepath.Join(root, sid, codemodeStoreDirName)); serr != nil {
		t.Fatalf("codemode store 目录不在 %q 下：%v", root, serr)
	}
	// 真跑一轮 → 会话文件与 checkpoint record 都落在插件会话目录（不是 sessions/<wsKey>）。
	runPluginTurn(t, m, buf, home, sid, "plugin ok")
	recPath := filepath.Join(pluginSessionsDir("demo"), "plugin-demo", sid+".record.json")
	waitPluginFile(t, recPath)
	// 记下来给删除用（delete_session 必须删掉这一份，否则下次打开又「复活」）
	if _, serr := os.Stat(recPath); serr != nil {
		t.Fatalf("record 不在插件子树里：%v", serr)
	}
	if _, serr := os.Stat(filepath.Join(wsSessionsDir(home), sid+".jsonl")); serr == nil {
		t.Fatalf("插件会话文件落进了项目会话目录 %s", wsSessionsDir(home))
	}
	// snapshot 也从插件 events 目录读（卸载后仍可读的那条回退）
	if evDir := m.eventsDirFor(home); evDir != pluginEventsDir("demo") {
		t.Fatalf("eventsDirFor = %q, want %q", evDir, pluginEventsDir("demo"))
	}
}

// ---- ③ hooks 只读用户级 ----

func TestPluginHooksUserLevelOnly(t *testing.T) {
	m, buf := pluginManager(t)
	fakeSurface(t, m, "demo", true, nil)
	home := pluginHomePath("demo")

	// 插件 home 里放一份「项目形状」的 hooks：把它当 WorkspaceDir 读进来的话，
	// 打开插件就等于执行该目录里的命令（09 §4.3 第 1 点）。
	if err := os.MkdirAll(filepath.Join(home, ".go-code"), 0o700); err != nil {
		t.Fatal(err)
	}
	projectHooks := p404aStopSection(p404aAppendCmd(filepath.Join(t.TempDir(), "plugin-home.jsonl")), 0)
	projectHooks["allowProjectHooks"] = true // 若配置被读取，这一项会放行项目级 hooks
	raw, err := json.MarshalIndent(projectHooks, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".go-code", "hooks.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	sid := newSessionID(t, m, buf, home)
	bs := m.get(home, sid)
	if bs == nil {
		t.Fatalf("插件会话 %s 不在运行态", sid)
	}
	if h := bs.s.Hooks(); h != nil {
		t.Fatalf("插件 home 里的项目级 hooks 被装配了：%+v", h)
	}

	// 正对照：用户级 hooks 必须真的能进来（否则上面的 nil 也可能是「什么都没读」）。
	p404aWriteUserHooks(t, p404aStopSection(p404aAppendCmd(filepath.Join(t.TempDir(), "user.jsonl")), 0))
	sid2 := newSessionID(t, m, buf, home)
	bs2 := m.get(home, sid2)
	if bs2 == nil {
		t.Fatalf("第二个插件会话 %s 不在运行态", sid2)
	}
	h := bs2.s.Hooks()
	if h == nil || h.Stop == nil {
		t.Fatalf("用户级 hooks 没装进插件会话（正对照失败）：%+v", h)
	}
}

// ---- ④ 会话命令在虚拟路径上原样可用 ----

func TestPluginSessionCommands(t *testing.T) {
	m, buf := pluginManager(t)
	fakeSurface(t, m, "demo", true, nil)
	home := pluginHomePath("demo")

	sidA := newSessionID(t, m, buf, home)
	sidB := newSessionID(t, m, buf, home)
	if sidA == "" || sidB == "" || sidA == sidB {
		t.Fatalf("插件会话 id 异常：%q %q", sidA, sidB)
	}
	// list 看得到它们（按虚拟路径）
	data := pluginRespData(t, pluginResp(t, m, buf, 2, "list", map[string]any{"workspace": home}))
	ids := pluginSessionIDs(t, data)
	if !ids[sidA] || !ids[sidB] {
		t.Fatalf("list 缺会话：got %v, want %q/%q", ids, sidA, sidB)
	}

	// switch_model：普通会话语义（D3）——不是 hub 那种 Profile 归属。
	model := m.provCfg.defaultModel()
	if model == "" {
		t.Skip("测试环境没有默认模型，跳过 switch_model")
	}
	resp := pluginResp(t, m, buf, 3, "switch_model", map[string]any{
		"workspace": home, "session_id": sidA, "model": model,
	})
	if resp["ok"] != true {
		t.Fatalf("switch_model 失败：%v", resp)
	}
	if bs := m.get(home, sidA); bs == nil || bs.model != model {
		t.Fatalf("switch_model 没落到插件会话：%v", bs)
	}

	// delete_session：只删目标会话
	resp = pluginResp(t, m, buf, 4, "delete_session", map[string]any{
		"workspace": home, "session_id": sidA,
	})
	if resp["ok"] != true {
		t.Fatalf("delete_session 失败：%v", resp)
	}
	ids = pluginSessionIDs(t, pluginRespData(t, resp))
	if ids[sidA] {
		t.Fatalf("删掉的会话仍在 list 里：%v", ids)
	}
	if !ids[sidB] {
		t.Fatalf("删除一个会话把别的会话也删了：%v", ids)
	}
	if bs := m.get(home, sidB); bs == nil || bs.ctx.Err() != nil {
		t.Fatalf("另一个会话被一起停了：%v", bs)
	}
	// 项目会话目录里不该出现任何插件会话残留
	if entries, rerr := os.ReadDir(wsSessionsDir(home)); rerr == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), sidA) || strings.HasPrefix(e.Name(), sidB) {
				t.Fatalf("插件会话文件落进了项目会话目录：%s", e.Name())
			}
		}
	}
}

// ---- ⑤ 停用/卸载后 ask 拒绝，会话仍可列/删 ----

func TestPluginAskRejectedWhenDisabledOrUninstalled(t *testing.T) {
	m, buf := pluginManager(t)
	fakeSurface(t, m, "demo", true, nil)
	home := pluginHomePath("demo")
	sid := newSessionID(t, m, buf, home)
	runPluginTurn(t, m, buf, home, sid, "before disable")

	// 停用（surface_set_enabled 走生产命令：它会优雅关闭 runtime，会话保留）
	resp := pluginResp(t, m, buf, 10, "surface_set_enabled", map[string]any{"id": "demo", "enabled": false})
	if resp["ok"] != true {
		t.Fatalf("surface_set_enabled: %v", resp)
	}
	if m.pluginRuntimeIfExists("demo") != nil {
		t.Fatalf("停用后插件 runtime 仍在表里（应优雅关闭）")
	}
	if _, serr := os.Stat(filepath.Join(pluginSessionsDir("demo"), sid+".jsonl")); serr != nil {
		t.Fatalf("停用把会话文件删了：%v", serr)
	}

	resp = pluginResp(t, m, buf, 11, "ask", map[string]any{
		"workspace": home, "session_id": sid, "text": "should be rejected",
	})
	if resp["ok"] != false {
		t.Fatalf("停用后 ask 不该成功：%v", resp)
	}
	if code := str(resp, "code"); code != "denied" {
		t.Fatalf("停用后 ask 的 code = %q, want denied", code)
	}
	if reason := str(pluginRespData(t, resp), "reason"); reason != pluginReasonDisabled {
		t.Fatalf("停用后 ask 的 reason = %q, want %q", reason, pluginReasonDisabled)
	}

	// 停用后会话仍可列（磁盘回退路径）
	ids := pluginSessionIDs(t, pluginRespData(t, pluginResp(t, m, buf, 12, "list", map[string]any{"workspace": home})))
	if !ids[sid] {
		t.Fatalf("停用后 list 看不到会话：%v", ids)
	}

	// 卸载 → 会话目录保留、ask 拒绝 reason=plugin_uninstalled、仍可删
	resp = pluginResp(t, m, buf, 13, "surface_remove", map[string]any{"id": "demo"})
	if resp["ok"] != true {
		t.Fatalf("surface_remove: %v", resp)
	}
	resp = pluginResp(t, m, buf, 14, "ask", map[string]any{
		"workspace": home, "session_id": sid, "text": "should be rejected",
	})
	if resp["ok"] != false {
		t.Fatalf("卸载后 ask 不该成功：%v", resp)
	}
	if reason := str(pluginRespData(t, resp), "reason"); reason != pluginReasonUninstalled {
		t.Fatalf("卸载后 ask 的 reason = %q, want %q", reason, pluginReasonUninstalled)
	}
	ids = pluginSessionIDs(t, pluginRespData(t, pluginResp(t, m, buf, 15, "list", map[string]any{"workspace": home})))
	if !ids[sid] {
		t.Fatalf("卸载后 list 看不到会话：%v", ids)
	}
	resp = pluginResp(t, m, buf, 16, "delete_session", map[string]any{"workspace": home, "session_id": sid})
	if resp["ok"] != true {
		t.Fatalf("卸载后 delete_session 失败：%v", resp)
	}
	if ids := pluginSessionIDs(t, pluginRespData(t, resp)); ids[sid] {
		t.Fatalf("卸载后删不掉会话：%v", ids)
	}
}

// ---- ⑥ plugin_projects_list 三种状态 ----

func TestPluginProjectsList(t *testing.T) {
	m, buf := pluginManager(t)
	writePluginSurfaces(t, m,
		pluginSurface{id: "with-session", enabled: true}, // 已安装 + 有会话
		pluginSurface{id: "no-session", enabled: false},  // 已安装（停用）+ 无会话
	)
	home := pluginHomePath("with-session")
	sid := newSessionID(t, m, buf, home)

	// 已卸载有会话：index 里没有它，但 plugin-sessions/<id>/ 下有会话文件
	goneID := "gone-plugin"
	if err := os.MkdirAll(pluginSessionsDir(goneID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginSessionsDir(goneID), "s-123.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	resp0 := pluginResp(t, m, buf, 1, "plugin_projects_list", nil)
	// 契约位置：{ok:true, data:{projects:[...]}}（P1b1 客户端按这个读）。
	if resp0["ok"] != true {
		t.Fatalf("plugin_projects_list 失败：%v", resp0)
	}
	data := pluginRespData(t, resp0)
	// 回执是**走 stdout 的 JSON**（readLines 解回来），所以这里是 map 数组 ——
	// 顺带把字段名的协议形状也钉住了（json tag 写错会在这里失败）。
	raw, ok := data["projects"].([]any)
	if !ok {
		t.Fatalf("projects 形状不对：%#v", data["projects"])
	}
	byID := map[string]pluginProjectInfo{}
	// 跨任务契约（P1b1）：每条恒含这七个键 —— 缺一个客户端就拼不出左栏条目。
	contractKeys := []string{"path", "plugin_id", "name", "icon", "enabled", "installed", "session_count"}
	for _, it := range raw {
		obj, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("projects 元素不是对象：%#v", it)
		}
		for _, k := range contractKeys {
			if _, has := obj[k]; !has {
				t.Fatalf("projects 元素缺契约字段 %q：%#v", k, obj)
			}
		}
		if _, isStr := obj["path"].(string); !isStr {
			t.Fatalf("path 不是字符串：%#v", obj["path"])
		}
		if _, isBool := obj["enabled"].(bool); !isBool {
			t.Fatalf("enabled 不是布尔：%#v", obj["enabled"])
		}
		if _, isBool := obj["installed"].(bool); !isBool {
			t.Fatalf("installed 不是布尔：%#v", obj["installed"])
		}
		if _, isNum := obj["session_count"].(float64); !isNum {
			t.Fatalf("session_count 不是数字：%#v", obj["session_count"])
		}
		byID[str(obj, "plugin_id")] = pluginProjectInfo{
			Path:         str(obj, "path"),
			PluginID:     str(obj, "plugin_id"),
			Name:         str(obj, "name"),
			Icon:         str(obj, "icon"),
			Enabled:      obj["enabled"] == true,
			Installed:    obj["installed"] == true,
			SessionCount: int(intNumOf(obj["session_count"])),
		}
	}

	got, ok := byID["with-session"]
	if !ok {
		t.Fatalf("已安装有会话的插件没出现：%v", byID)
	}
	if !got.Installed || !got.Enabled || got.SessionCount != 1 {
		t.Fatalf("with-session = %+v, want installed/enabled/1 session", got)
	}
	if got.Path != pluginHomePath("with-session") {
		t.Fatalf("path = %q, want 虚拟 home", got.Path)
	}
	if got.Name != "with-session" {
		t.Fatalf("name = %q（fakeSurface 的 zh 名就是 id）", got.Name)
	}

	nos, ok := byID["no-session"]
	if !ok {
		t.Fatalf("已安装无会话的插件没出现：%v", byID)
	}
	if !nos.Installed || nos.Enabled || nos.SessionCount != 0 {
		t.Fatalf("no-session = %+v, want installed/disabled/0", nos)
	}

	un, ok := byID[goneID]
	if !ok {
		t.Fatalf("已卸载有会话的插件没出现（会话会变成看不见的孤儿）：%v", byID)
	}
	if un.Installed || un.Enabled || un.SessionCount != 1 {
		t.Fatalf("gone-plugin = %+v, want uninstalled/1 session", un)
	}
	if un.Path != pluginHomePath(goneID) {
		t.Fatalf("卸载插件的 path = %q", un.Path)
	}
	if sid == "" {
		t.Fatal("new_session 没有返回 session_id")
	}

	// 虚拟路径不进 workspaces.json（这条命令也不能顺手登记）
	if rawWS, rerr := os.ReadFile(filepath.Join(appDataDir(), "workspaces.json")); rerr == nil {
		if strings.Contains(string(rawWS), "plugin-sessions") {
			t.Fatalf("workspaces.json 里出现了虚拟路径：%s", rawWS)
		}
	}
}

// ---- ⑦ denylist：插件引擎无越权工具、保留 memory_dream；项目引擎无 plugin__ ----

func TestPluginToolDenylist(t *testing.T) {
	m, _ := pluginManager(t)
	fakeSurface(t, m, "demo", true, nil)
	home := pluginHomePath("demo")
	ws, err := m.pluginRuntimeForHome(home)
	if err != nil {
		t.Fatal(err)
	}

	reg, qw, sk := pluginToolsFixture(t)
	bg := func() context.Context { return context.Background() }
	eng, _ := m.buildPluginEngine(ws, "s-plugin-deny", m.provCfg.providerName(), m.provCfg.defaultModel(), "", nil, reg, qw, bg)

	// 可见工具面 = 模型真正能看到的清单（Hidden 的工具仍在 Tools() 里但不可调用，
	// 见 applyPluginToolPolicy 的两条防线）。断言用 VisibleTools。
	names := map[string]bool{}
	for _, tl := range eng.VisibleTools() {
		names[tl.Name()] = true
	}
	for name := range pluginToolDenyExact {
		if names[name] {
			t.Errorf("denylist 工具仍在插件引擎里：%s", name)
		}
	}
	for _, prefix := range pluginToolDenyPrefix {
		for name := range names {
			if strings.HasPrefix(name, prefix) {
				t.Errorf("denylist 前缀 %q 的工具仍在插件引擎里：%s", prefix, name)
			}
		}
	}
	// memory_dream 必须保留（D11 明确保留项）
	if !names["memory_dream"] {
		t.Fatalf("memory_dream 被误删（插件引擎工具面：%v）", names)
	}
	// 正对照：内置工具面确实装配了（否则上面的断言可能只是「引擎是空的」）
	for _, want := range []string{"read_file", "bash", "run_python"} {
		if !names[want] {
			t.Fatalf("插件引擎缺内置工具 %q（工具面 %v）", want, names)
		}
	}
	// 引擎级撤下：直呼名字也不该拿得到（codemode / 直呼两条路都拦）
	for name := range pluginToolDenyExact {
		if tl, gerr := eng.GetTool(context.Background(), name); gerr == nil && tl != nil {
			t.Errorf("%s 仍能从引擎取到：%v", name, tl)
		}
	}

	// 项目引擎绝不含 plugin__*（D10：插件工具只在本插件虚拟 Project 内）
	proj := m.runtime(t.TempDir())
	if proj == nil {
		t.Fatal("项目 runtime 为 nil")
	}
	projEng, _ := newSessionEngine(proj.path, "s-proj-deny", nil, nil, m.provCfg, m.provCfg.providerName(), m.provCfg.defaultModel(), "", bg, reg, qw, sk, proj.subagents, proj.mcp, proj.plugins, profiles["code"], nil, nil, nil, nil, nil)
	projNames := map[string]bool{}
	for _, tl := range projEng.VisibleTools() {
		projNames[tl.Name()] = true
		if strings.HasPrefix(tl.Name(), "plugin__") {
			t.Errorf("项目引擎出现了插件工具：%s", tl.Name())
		}
	}
	if len(projNames) == 0 {
		t.Fatal("项目引擎是空的（断言无意义）")
	}
}

// ---- ⑨ IM 事件桥按 kind 硬排除（复核项 1）----

func TestPluginSessionNotWiredToIMBridge(t *testing.T) {
	m, buf := pluginManager(t)
	fakeSurface(t, m, "demo", true, nil)
	home := pluginHomePath("demo")

	// 装一个**真实的**事件桥（空配置：没有镜像 → OnSessionEvent 直接返回，不会真的发消息）。
	// 用假 rt 会留下 nil 解引用风险（teardown 时 SessionClosed 会走 OnSessionEvent）。
	rt := &implugin.Runtime{Router: im.NewRouter(im.Config{}), Security: im.NewSecurity(im.Config{}, "")}
	m.setIMBridge(newIMBridge(rt))

	// 正对照：项目会话照旧挂上事件桥（本改动不得影响项目/Hub 行为）
	proj := t.TempDir()
	projSid := newSessionID(t, m, buf, proj)
	projBS := m.get(proj, projSid)
	if projBS == nil || projBS.im == nil {
		t.Fatalf("项目会话没有挂上 IM 事件桥（正对照失败）：%v", projBS)
	}

	// 插件虚拟 Project：**硬排除**（不能只靠「随机 sid 没有 mirror」兜底）
	sid := newSessionID(t, m, buf, home)
	bs := m.get(home, sid)
	if bs == nil {
		t.Fatalf("插件会话 %s 不在运行态", sid)
	}
	if bs.im != nil {
		t.Fatalf("插件会话挂上了 IM 事件桥（事件会外发到用户手机）：%v", bs.im)
	}
	// 再确认一次：即使桥存在、且会话是新建的，装配分支也不会把它接上。
	if m.imBridgeRef() == nil {
		t.Fatal("测试装置失效：IM 桥应仍非 nil")
	}
}

// ---- ⑩ 运行态未装配：读/改类的命令前置拒绝（复核项 2）----

func TestPluginCommandsDenyWhenRuntimeNotAssembled(t *testing.T) {
	m, buf := pluginManager(t)
	fakeSurface(t, m, "demo", true, nil)
	home := pluginHomePath("demo")

	// 前置：只有 surface index，没有运行态（也没建过目录）。
	if m.pluginRuntimeIfExists("demo") != nil {
		t.Fatal("测试装置失效：运行态已被装配")
	}

	type probe struct {
		typ     string
		payload map[string]any
	}
	probes := []probe{
		{"skills", map[string]any{"workspace": home}},
		{"skill_toggle", map[string]any{"workspace": home, "name": "x", "enabled": false}},
		{"skill_get", map[string]any{"workspace": home, "name": "x"}},
		{"mcp_list", map[string]any{"workspace": home}},
		{"mcp_refresh", map[string]any{"workspace": home, "name": "srv"}},
		{"mcp_reload", map[string]any{"workspace": home}},
		{"plugin_list", map[string]any{"workspace": home}},
		{"browser_status", map[string]any{"workspace": home}},
	}
	for i, pr := range probes {
		resp := pluginResp(t, m, buf, 100+i, pr.typ, pr.payload)
		if resp["ok"] != false {
			t.Fatalf("%s：未装配时不该成功：%v", pr.typ, resp)
		}
		if code := str(resp, "code"); code != pluginNotReadyCode {
			t.Fatalf("%s：code = %q, want %q", pr.typ, code, pluginNotReadyCode)
		}
		if reason := str(pluginRespData(t, resp), "reason"); reason != pluginNotReadyReason {
			t.Fatalf("%s：reason = %q, want %q", pr.typ, reason, pluginNotReadyReason)
		}
	}
	// 拒绝不得留下副作用：虚拟路径没进 m.workspaces、也没被顺手装配
	m.mu.Lock()
	_, leaked := m.workspaces[home]
	projectCount := len(m.workspaces)
	m.mu.Unlock()
	if leaked || projectCount != 0 {
		t.Fatalf("拒绝路径顺手套出了项目 runtime：%v", m.workspaces)
	}
	if m.pluginRuntimeIfExists("demo") != nil {
		t.Fatal("拒绝路径顺手套出了插件 runtime（只读命令不得有装配副作用）")
	}

	// 斜杠命令：没有会话时与项目路径同答复（session_not_found），关键是**不 panic**、
	// 也不顺手套运行态。这是"未装配 + 有会话"不可达的诚实口径 —— 会话活在 runtime 里，
	// 没有 runtime 就没有会话；handler 内部的 plugin_not_ready 判空是纵深防御。
	respCmd := pluginResp(t, m, buf, 150, "command", map[string]any{
		"workspace": home, "session_id": "s-x", "line": "/reload_skills",
	})
	if respCmd["ok"] != false || str(respCmd, "code") != "session_not_found" {
		t.Fatalf("未装配时的斜杠命令 = %v, want ok:false/session_not_found", respCmd)
	}
	if m.pluginRuntimeIfExists("demo") != nil {
		t.Fatal("斜杠命令顺手套出了插件运行态")
	}

	// 读类命令在未装配时照常可用（磁盘回退，不依赖运行态）
	for i, typ := range []string{"list", "trace"} {
		resp := pluginResp(t, m, buf, 200+i, typ, map[string]any{"workspace": home, "session_id": "s-x"})
		if resp["ok"] != true {
			t.Fatalf("%s 应在未装配时也能读（磁盘回退）：%v", typ, resp)
		}
	}

	// 装配后同两条命令正常工作（技能清单 / MCP 状态读的是插件自己的全局层）
	if _, err := m.pluginRuntimeForHome(home); err != nil {
		t.Fatalf("装配插件运行态: %v", err)
	}
	resp := pluginResp(t, m, buf, 300, "skills", map[string]any{"workspace": home})
	if resp["ok"] != true {
		t.Fatalf("装配后 skills 应成功：%v", resp)
	}
	if _, ok := pluginRespData(t, resp)["skills"].([]any); !ok {
		t.Fatalf("skills 回执缺 skills 数组：%#v", pluginRespData(t, resp))
	}
	resp = pluginResp(t, m, buf, 301, "mcp_list", map[string]any{"workspace": home})
	if resp["ok"] != true {
		t.Fatalf("装配后 mcp_list 应成功：%v", resp)
	}
	if _, ok := pluginRespData(t, resp)["servers"].([]any); !ok {
		t.Fatalf("mcp_list 回执缺 servers 数组：%#v", pluginRespData(t, resp))
	}
	// skill_toggle 装配后生效（读类之后这条是「改」的正对照）
	resp = pluginResp(t, m, buf, 302, "skill_get", map[string]any{"workspace": home, "name": "nope"})
	if resp["ok"] != false {
		t.Fatalf("skill_get 未知技能应报错（而不是 denied）：%v", resp)
	}
	if code := str(resp, "code"); code == pluginNotReadyCode {
		t.Fatalf("装配后不该再报 plugin_not_ready：%v", resp)
	}
}

// ---- ⑧ 负向守卫：虚拟路径永不进 workspaces.json ----

func TestPluginWorkspaceGuards(t *testing.T) {
	m, buf := pluginManager(t)
	fakeSurface(t, m, "demo", true, nil)
	home := pluginHomePath("demo")
	// 先把运行态装配起来：这样「delete_workspace 拒绝」这条断言才有可观察的后果
	//（目录必须还在），否则 plugin-sessions/<id> 根本没建过，测试会空真通过。
	if _, err := m.pluginRuntimeForHome(home); err != nil {
		t.Fatalf("装配插件运行态: %v", err)
	}
	wsRegPath := filepath.Join(appDataDir(), "workspaces.json")

	// workspace_add：硬拒绝
	resp := pluginResp(t, m, buf, 1, "workspace_add", map[string]any{"path": home})
	if resp["ok"] != false {
		t.Fatalf("workspace_add 接受了虚拟路径：%v", resp)
	}
	// workspace_sync：整批拒绝（sync 不校验目录，必须显式拦）
	project := t.TempDir()
	resp = pluginResp(t, m, buf, 2, "workspace_sync", map[string]any{"paths": []any{project, home}})
	if resp["ok"] != false {
		t.Fatalf("workspace_sync 接受了含虚拟路径的批次：%v", resp)
	}
	// 直接调 manager 入口（手机端/内部调用也走这里）
	if _, aerr := m.workspaceAdd(home); aerr == nil {
		t.Fatal("workspaceAdd(虚拟路径) 应拒绝")
	}
	if _, serr := m.workspaceSync([]string{project, home}); serr == nil {
		t.Fatal("workspaceSync(含虚拟路径) 应拒绝")
	}
	// 整批拒绝的副作用：连 project 也不能被写进去（不是「过滤掉坏的那条」）
	if entries := m.workspaceEntries(); len(entries) != 0 {
		t.Fatalf("整批拒绝却写了一半：%v", entries)
	}
	if raw, rerr := os.ReadFile(wsRegPath); rerr == nil {
		if strings.Contains(string(raw), "plugin-sessions") {
			t.Fatalf("workspaces.json 里出现了虚拟路径：%s", raw)
		}
		if strings.Contains(string(raw), project) {
			t.Fatalf("被拒绝的批次仍写进了 workspaces.json：%s", raw)
		}
	}
	// 正对照：正常的项目路径仍然能登记（证明拒绝不是「写盘全坏了」）
	if _, aerr := m.workspaceAdd(project); aerr != nil {
		t.Fatalf("正常项目路径被拒绝：%v", aerr)
	}
	if raw, rerr := os.ReadFile(wsRegPath); rerr != nil || !strings.Contains(string(raw), project) {
		t.Fatalf("正常项目路径没写进 workspaces.json：%v %s", rerr, raw)
	}

	// delete_workspace 也要拒绝虚拟路径（否则会 RemoveAll plugin-sessions/<id>）
	resp = pluginResp(t, m, buf, 3, "delete_workspace", map[string]any{"workspace": home})
	if resp["ok"] != false {
		t.Fatalf("delete_workspace 接受了虚拟路径：%v", resp)
	}
	if _, derr := os.Stat(pluginSessionsDir("demo")); derr != nil {
		t.Fatalf("delete_workspace 把插件会话目录删了：%v", derr)
	}
}

// ---- 分流点 6：mesh 注册 / hub 队列泵 / autoEnablePlugins / cron 全部跳过 ----

func TestPluginSkipsMeshCronAndAutoEnable(t *testing.T) {
	m, buf := pluginManager(t)
	// 把「容易误伤」的开关全部打开：mesh 会话投递 + cron 定时任务。
	if err := os.MkdirAll(filepath.Join(appDataDir()), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"mesh":{"session_enabled":true,"remote_enabled":false},"cron":{"enabled":true}}`
	if err := os.WriteFile(filepath.Join(appDataDir(), "settings.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m.hub = session.NewHub("node-plugin-test")
	m.cron = cron.New()
	fakeSurface(t, m, "demo", true, nil)
	home := pluginHomePath("demo")

	sid := newSessionID(t, m, buf, home)

	// ① 不进 Session Mesh：Hub 注册表里查不到这条会话
	if got := m.hub.Get(sid); got != nil {
		t.Fatalf("插件会话进了 Session Mesh：%v", sid)
	}
	// ② autoEnablePlugins 跳过：虚拟 Project 不做「按工作区恢复已启用插件」
	ws := m.pluginRuntimeIfExists("demo")
	if ws == nil {
		t.Fatal("插件 runtime 没装配")
	}
	if ws.pluginsAuto {
		t.Fatal("插件虚拟 Project 跑了 autoEnablePlugins（应跳过）")
	}
	// ③ cron / mesh 工具不注册（即使两个开关都开着）
	reg, qw, _ := pluginToolsFixture(t)
	eng, _ := m.buildPluginEngine(ws, sid, m.provCfg.providerName(), m.provCfg.defaultModel(), "", nil, reg, qw, func() context.Context { return context.Background() })
	for _, name := range []string{"session_send", "session_list", "CronCreate", "CronList", "CronUpdate", "CronDelete"} {
		if tl, gerr := eng.GetTool(context.Background(), name); gerr == nil && tl != nil {
			t.Errorf("插件引擎里有 %s（分流点 6 应跳过）", name)
		}
	}
	// ④ Hub 派发队列泵：插件会话跑完一轮后不推进任何队列（bs.runtime.kind 不是 project）
	if bs := m.get(home, sid); bs == nil || bs.runtime.kind != runtimePlugin {
		t.Fatalf("插件会话 runtime 不是 runtimePlugin：%v", bs)
	}
}

// ---- shutdown 路径要把插件 runtime 算进去（09 §4.3 第 5 点）----

func TestPluginShutdownCoversRuntime(t *testing.T) {
	m, buf := pluginManager(t)
	fakeSurface(t, m, "demo", true, nil)
	home := pluginHomePath("demo")
	sid := newSessionID(t, m, buf, home)
	runPluginTurn(t, m, buf, home, sid, "before shutdown")

	// shutdown_workspace（虚拟 home 不在 m.workspaces，必须单独遍历到）
	resp := pluginResp(t, m, buf, 1, "shutdown", map[string]any{"workspace": home})
	if resp["ok"] != true {
		t.Fatalf("shutdown: %v", resp)
	}
	if m.pluginRuntimeIfExists("demo") != nil {
		t.Fatal("shutdown 之后插件 runtime 仍在表里")
	}
	// 会话已优雅关闭：再 list 走磁盘回退仍能看到它（home/sessions/events 不删）
	ids := pluginSessionIDs(t, pluginRespData(t, pluginResp(t, m, buf, 2, "list", map[string]any{"workspace": home})))
	if !ids[sid] {
		t.Fatalf("shutdown 把会话目录也清了：%v", ids)
	}
	// shutdown_all 同样覆盖（再装一个插件，只调 shutdownAll）
	fakeSurface(t, m, "demo2", true, nil)
	if _, err := m.pluginRuntimeForHome(pluginHomePath("demo2")); err != nil {
		t.Fatal(err)
	}
	m.shutdownAll()
	if m.pluginRuntimeIfExists("demo2") != nil {
		t.Fatal("shutdownAll 之后插件 runtime 仍在表里")
	}
}
