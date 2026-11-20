package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/subagent"
)

// jsonFile 读 JSON 对象文件（断言写盘内容）。
func jsonFile(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("解析 %s 失败: %v\n%s", path, err, raw)
	}
	return m
}

// mcp_projects_test.go —— 项目级 MCP 汇总/写盘（契约 §5.1/§5.2、§8）。

// mcpProjects 发 mcp_projects 并按 path 索引返回项目视图。
func mcpProjects(t *testing.T, m *manager, buf *syncBuf, id int, workspaces []string) map[string]map[string]any {
	t.Helper()
	paths := make([]any, 0, len(workspaces))
	for _, w := range workspaces {
		paths = append(paths, w)
	}
	m.dispatch(cmd(id, "mcp_projects", map[string]any{"workspaces": paths}))
	resp, ok := lastResp(t, buf)
	if !ok || resp["ok"] != true {
		t.Fatalf("mcp_projects 响应异常: %v", resp)
	}
	list, _ := resp["data"].(map[string]any)["projects"].([]any)
	out := map[string]map[string]any{}
	for _, p := range list {
		pm, _ := p.(map[string]any)
		if path, _ := pm["path"].(string); path != "" {
			out[path] = pm
		}
	}
	return out
}

// mcpProjectServers 项目视图里的 servers 按 name 索引。
func mcpProjectServers(t *testing.T, proj map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	list, _ := proj["servers"].([]any)
	for _, s := range list {
		sm, _ := s.(map[string]any)
		if n, _ := sm["name"].(string); n != "" {
			out[n] = sm
		}
	}
	return out
}

// mcpProjectLayer 项目视图里的 layers 按路径后缀取。
func mcpProjectLayer(proj map[string]any, suffix string) map[string]any {
	list, _ := proj["layers"].([]any)
	for _, l := range list {
		lm, _ := l.(map[string]any)
		if p, _ := lm["path"].(string); strings.HasSuffix(p, suffix) {
			return lm
		}
	}
	return nil
}

// TestMCPProjectsSummary：多项目汇总（项目层合并 / editable / skipped / exists 契约），
// 且**不创建运行态**（打开设置页不得建会话、连 MCP、起 watcher）。
func TestMCPProjectsSummary(t *testing.T) {
	m, buf := testManager(t)
	wsA, wsB := t.TempDir(), t.TempDir()
	wsGone := filepath.Join(t.TempDir(), "deleted-project") // 目录不存在（前端据 exists=false 过滤）

	// wsA：.mcp.json 两台（一台坏条目）+ .go-code/settings.json 一台新增 + 一台同名覆盖
	writeMCPConfig(t, filepath.Join(wsA, ".mcp.json"), `{
		"repo-tools": {"type": "stdio", "command": "npx", "args": ["-y", "x"]},
		"broken": {"type": "unknown-transport"}
	}`)
	writeMCPConfig(t, filepath.Join(wsA, ".go-code", "settings.json"), `{
		"native": {"type": "http", "url": "http://native/mcp"},
		"repo-tools": {"type": "stdio", "command": "local-tools"}
	}`)

	// 同一路径重复 + 目录不存在的项目：都要按契约返回（前端去重/过滤）
	projects := mcpProjects(t, m, buf, 1, []string{wsA, wsB, wsGone, wsA})
	if len(projects) != 3 {
		t.Fatalf("项目数 = %d, want 3（重复路径应去重）: %v", len(projects), projects)
	}
	if projects[wsA] == nil || projects[wsB] == nil || projects[wsGone] == nil {
		t.Fatalf("缺项目视图: %v", projects)
	}
	// 不创建运行态：三个项目都不该被"打开"
	for _, p := range []string{wsA, wsB, wsGone} {
		if rt := m.peekRuntime(p); rt != nil {
			t.Fatalf("mcp_projects 不得创建运行态: %s", p)
		}
	}

	a := projects[wsA]
	if a["name"] != filepath.Base(wsA) || a["exists"] != true || a["running"] != false {
		t.Fatalf("wsA 基础字段错: %v", a)
	}
	if layers, _ := a["layers"].([]any); len(layers) != 2 {
		t.Fatalf("项目层应只有两层（.mcp.json + .go-code/settings.json）: %v", a["layers"])
	}
	if l := mcpProjectLayer(a, ".mcp.json"); l == nil || l["present"] != true || l["servers"] != float64(1) {
		t.Fatalf(".mcp.json 层状态错: %v", mcpProjectLayer(a, ".mcp.json"))
	}
	// 坏条目跳过并计入该层 skipped（名字可见），不牵连整层
	if l := mcpProjectLayer(a, ".mcp.json"); len(l["skipped"].([]any)) != 1 || l["skipped"].([]any)[0] != "broken" {
		t.Fatalf("坏条目应进 skipped: %v", l)
	}
	if l := mcpProjectLayer(a, ".go-code/settings.json"); l == nil || l["present"] != true || l["servers"] != float64(2) {
		t.Fatalf("原生项目层状态错: %v", mcpProjectLayer(a, ".go-code/settings.json"))
	}

	servers := mcpProjectServers(t, a)
	if len(servers) != 2 {
		t.Fatalf("服务器数 = %d, want 2（坏条目不算）: %v", len(servers), servers)
	}
	// 同名后层覆盖 + file/editable 标注
	rt := servers["repo-tools"]
	if rt == nil || rt["command"] != "local-tools" {
		t.Fatalf("同名应被 .go-code/settings.json 覆盖: %v", servers)
	}
	if !strings.HasSuffix(rt["file"].(string), ".go-code/settings.json") || rt["editable"] != true {
		t.Fatalf("覆盖后的来源应标为可编辑文件: %v", rt)
	}
	native := servers["native"]
	if native == nil || native["type"] != "http" || native["url"] != "http://native/mcp" || native["editable"] != true {
		t.Fatalf("原生项目层服务器错: %v", native)
	}
	if _, hasState := native["state"]; hasState {
		t.Fatalf("无运行态不该有连接状态: %v", native)
	}
	// 只读层（.mcp.json 独有）不标 editable
	writeMCPConfig(t, filepath.Join(wsB, ".mcp.json"), `{"only-repo": {"type": "stdio", "command": "x"}}`)
	projects = mcpProjects(t, m, buf, 2, []string{wsB})
	if s := mcpProjectServers(t, projects[wsB])["only-repo"]; s == nil || s["editable"] != false ||
		!strings.HasSuffix(s["file"].(string), ".mcp.json") {
		t.Fatalf(".mcp.json 的服务器应只读且标 file: %v", s)
	}
	if m.peekRuntime(wsB) != nil {
		t.Fatalf("mcp_projects 不得创建运行态: %s", wsB)
	}

	// wsB 未配置时的空形态
	empty := mcpProjects(t, m, buf, 3, []string{wsGone})[wsGone]
	if empty["exists"] != false || empty["running"] != false {
		t.Fatalf("目录不存在的项目应 exists=false: %v", empty)
	}
	if s, _ := empty["servers"].([]any); len(s) != 0 {
		t.Fatalf("未配置项目 servers 应为空数组: %v", empty["servers"])
	}
	if l, _ := empty["layers"].([]any); len(l) != 2 {
		t.Fatalf("未配置项目也应给出两层状态（present=false）: %v", empty["layers"])
	}
	// 缺 workspaces 字段 → 空列表（不报错：前端列表可能为空）
	m.dispatch(cmd(4, "mcp_projects", nil))
	resp, _ := lastResp(t, buf)
	if resp["ok"] != true {
		t.Fatalf("缺 workspaces 应 ok: %v", resp)
	}
	if projects, _ := resp["data"].(map[string]any)["projects"].([]any); projects == nil || len(projects) != 0 {
		t.Fatalf("缺 workspaces → 空数组: %v", resp["data"])
	}
}

// TestMCPProjectsRunningState：有运行态时复用 Loader 层状态 + 连接状态（且不额外创建运行态）。
func TestMCPProjectsRunningState(t *testing.T) {
	m, buf := testManager(t)
	wsLive, wsIdle := t.TempDir(), t.TempDir()
	writeMCPConfig(t, filepath.Join(wsLive, ".go-code", "settings.json"), `{"proj": {"type": "stdio", "command": "npx"}}`)
	writeMCPConfig(t, filepath.Join(wsIdle, ".go-code", "settings.json"), `{"proj": {"type": "stdio", "command": "npx"}}`)
	m.runtime(wsLive) // 只有它被"打开"

	projects := mcpProjects(t, m, buf, 1, []string{wsLive, wsIdle})
	live, idle := projects[wsLive], projects[wsIdle]
	if live["running"] != true || idle["running"] != false {
		t.Fatalf("running 应反映「是否有运行态」: live=%v idle=%v", live["running"], idle["running"])
	}
	if s := mcpProjectServers(t, live)["proj"]; s == nil || s["state"] != "idle" {
		t.Fatalf("有运行态时应带连接状态（未连接 = idle）: %v", s)
	}
	if s := mcpProjectServers(t, idle)["proj"]; s == nil {
		t.Fatalf("无运行态也要列出服务器: %v", s)
	}
	if _, has := mcpProjectServers(t, idle)["proj"]["state"]; has {
		t.Fatalf("无运行态不该有连接状态")
	}
	if m.peekRuntime(wsIdle) != nil {
		t.Fatalf("peek 不应创建运行态: %s", wsIdle)
	}
}

// TestMCPProjectSetWrites：写盘（原子/权限/保留其他键/删键/未识别字段保留）+ 校验拒绝。
func TestMCPProjectSetWrites(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	settingsPath := filepath.Join(ws, ".go-code", "settings.json")
	writeMCPConfig(t, settingsPath, `{"old": {"type": "stdio", "command": "old-cmd"}}`)
	// 加一个"其他顶层键"（同一文件里非 mcpServers 的段不能因保存被抹掉）
	raw := map[string]any{
		"otherSection": map[string]any{"keep": true},
		"mcpServers":   map[string]any{"old": map[string]any{"type": "stdio", "command": "old-cmd"}},
	}
	b, _ := json.MarshalIndent(raw, "", "  ")
	if err := os.WriteFile(settingsPath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	set := func(id int, servers map[string]any) map[string]any {
		t.Helper()
		m.dispatch(cmd(id, "mcp_project_set", map[string]any{"workspace": ws, "servers": servers}))
		resp, ok := lastResp(t, buf)
		if !ok {
			t.Fatalf("mcp_project_set 无响应")
		}
		return resp
	}

	// 合法保存：old 被替换为 new（全量语义），其他顶层键保留
	resp := set(1, map[string]any{"new": map[string]any{"type": "stdio", "command": "npx", "args": []any{"-y", "x"}, "env": map[string]any{"K": "V"}}})
	if resp["ok"] != true {
		t.Fatalf("合法保存应通过: %v", resp)
	}
	if d, _ := resp["data"].(map[string]any); d["changed"] != true {
		t.Fatalf("响应 data.changed 应为 true: %v", resp["data"])
	}
	file := jsonFile(t, settingsPath)
	if _, keep := file["otherSection"]; !keep {
		t.Fatalf("其他顶层键被抹掉: %v", file)
	}
	srv := file["mcpServers"].(map[string]any)
	if len(srv) != 1 || srv["new"] == nil {
		t.Fatalf("mcpServers 应被整段替换（全量语义）: %v", srv)
	}
	if srv["new"].(map[string]any)["command"] != "npx" {
		t.Fatalf("服务器内容写错: %v", srv["new"])
	}
	// 权限：文件 0600（配置含明文 env；目录权限见下面的"全新目录"用例）
	fi, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("文件权限应为 0600，实为 %v", fi.Mode().Perm())
	}

	// 全新目录（.go-code 尚不存在）→ 目录 0700 + 文件 0600
	fresh := t.TempDir()
	m.dispatch(cmd(5, "mcp_project_set", map[string]any{"workspace": fresh, "servers": map[string]any{
		"only": map[string]any{"type": "stdio", "command": "x"},
	}}))
	if resp, _ := lastResp(t, buf); resp["ok"] != true {
		t.Fatalf("全新目录保存应通过: %v", resp)
	}
	di, err := os.Stat(filepath.Join(fresh, ".go-code"))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("新建目录权限应为 0700，实为 %v", di.Mode().Perm())
	}
	fi, err = os.Stat(filepath.Join(fresh, ".go-code", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("新建文件权限应为 0600，实为 %v", fi.Mode().Perm())
	}

	// 空 map → 删除该键（不留空壳），其他键仍在
	resp = set(2, map[string]any{})
	if resp["ok"] != true {
		t.Fatalf("空 map 应通过（删键）: %v", resp)
	}
	file = jsonFile(t, settingsPath)
	if _, has := file["mcpServers"]; has {
		t.Fatalf("空 map 应删除 mcpServers 键: %v", file)
	}
	if _, keep := file["otherSection"]; !keep {
		t.Fatalf("删段不该动其他键: %v", file)
	}

	// 非法配置拒绝：未知 type / stdio 缺 command / 空名字 / 非对象
	for i, bad := range []map[string]any{
		{"x": map[string]any{"type": "carrier-pigeon"}},
		{"x": map[string]any{"type": "stdio"}},
		{"": map[string]any{"type": "stdio", "command": "x"}},
	} {
		resp = set(10+i, bad)
		if resp["ok"] != false || resp["error"] == "" {
			t.Fatalf("非法配置应拒绝并给出原因: %v", resp)
		}
	}
	if _, has := jsonFile(t, settingsPath)["mcpServers"]; has {
		t.Fatalf("拒绝保存不得写盘")
	}

	// 条目级未识别字段保留：同名条目里模型不认识的键不因一次保存丢失
	writeMCPConfig(t, settingsPath, `{"new": {"type": "stdio", "command": "npx", "cwd": "/tmp/wd", "futureField": 7}}`)
	resp = set(20, map[string]any{"new": map[string]any{"type": "stdio", "command": "changed-cmd"}})
	if resp["ok"] != true {
		t.Fatalf("保存应通过: %v", resp)
	}
	entry := jsonFile(t, settingsPath)["mcpServers"].(map[string]any)["new"].(map[string]any)
	if entry["command"] != "changed-cmd" {
		t.Fatalf("模型认识的键应以模型为准: %v", entry)
	}
	if entry["cwd"] != "/tmp/wd" || entry["futureField"] != float64(7) {
		t.Fatalf("未识别字段被抹掉: %v", entry)
	}

	// 坏文件拒绝覆盖（用户可手修，不该被清空）
	if err := os.WriteFile(settingsPath, []byte(`{"mcpServers": {`), 0o600); err != nil {
		t.Fatal(err)
	}
	resp = set(21, map[string]any{"new": map[string]any{"type": "stdio", "command": "x"}})
	if resp["ok"] != false || !strings.Contains(resp["error"].(string), "不是有效 JSON") {
		t.Fatalf("坏文件应拒绝并说明原因: %v", resp)
	}
	broken, _ := os.ReadFile(settingsPath)
	if !strings.Contains(string(broken), `"mcpServers": {`) {
		t.Fatalf("坏文件被改动: %s", broken)
	}

	// workspace 缺失 / 目录不存在 → 拒绝（不为不存在的项目造目录树）
	m.dispatch(cmd(30, "mcp_project_set", map[string]any{"servers": map[string]any{}}))
	if resp, _ := lastResp(t, buf); resp["ok"] != false || !strings.Contains(resp["error"].(string), "workspace 为空") {
		t.Fatalf("缺 workspace 应拒绝: %v", resp)
	}
	gone := filepath.Join(t.TempDir(), "nope")
	m.dispatch(cmd(31, "mcp_project_set", map[string]any{"workspace": gone, "servers": map[string]any{}}))
	if resp, _ := lastResp(t, buf); resp["ok"] != false {
		t.Fatalf("目录不存在的项目应拒绝: %v", resp)
	}
	if _, err := os.Stat(gone); err == nil {
		t.Fatalf("拒绝路径不得创建目录: %s", gone)
	}
}

// TestMCPProjectSetHotApply：有运行态 → 写盘后立即重读并重建会话 loop（工具面变化）。
// 用**真实 stdio fixture 服务器**端到端验证（与 TestSessionEngineMCPTools 同一套夹具）：
// 项目层保存后，新建的会话引擎里能真的列出 mcp__proj__echo。
func TestMCPProjectSetHotApply(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("需要 go 工具链")
	}
	// HOME 隔离前先取真实模块缓存（t.Setenv HOME 会让嵌套 go build 找不到 $HOME/go/pkg/mod）
	gomod := ""
	if out, err := exec.Command("go", "env", "GOMODCACHE").Output(); err == nil {
		gomod = strings.TrimSpace(string(out))
	}
	t.Setenv("HOME", t.TempDir())
	bin := filepath.Join(t.TempDir(), "e2e-server")
	build := exec.Command("go", "build", "-o", bin, "../../mcp/testdata/server")
	build.Env = append(os.Environ(), "GOMODCACHE="+gomod, "GOPROXY=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建 fixture: %v\n%s", err, out)
	}
	m, buf := testManager(t)
	ws := t.TempDir()
	m.runtime(ws) // 运行态（有会话的"已打开项目"）
	bs, err := m.create(ws, "s-proj-hot", "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	loopBefore := bs.loop

	// 保存项目层（写 {ws}/.go-code/settings.json 的 mcpServers 段）
	m.dispatch(cmd(1, "mcp_project_set", map[string]any{"workspace": ws, "servers": map[string]any{
		"proj": map[string]any{"type": "stdio", "command": bin},
	}}))
	if resp, _ := lastResp(t, buf); resp["ok"] != true {
		t.Fatalf("保存应通过: %v", resp)
	}
	// 1) 活管理器已应用（mcpApplied 变了）
	wsRT := m.peekRuntime(ws)
	wsRT.mu.Lock()
	applied, hasApplied := wsRT.mcpApplied["proj"]
	wsRT.mu.Unlock()
	if !hasApplied || applied.Command != bin {
		t.Fatalf("有运行态时保存应立即应用到活管理器: %v", wsRT.mcpApplied)
	}
	// 2) 会话 loop 已重建（工具面变化下一次运行生效）
	if bs.loop == loopBefore {
		t.Fatalf("有运行态时应重建会话 loop（工具面变化）")
	}
	// 3) 端到端：从活管理器建引擎，项目层服务器真的连上并列出工具
	eng, _ := newSessionEngine(ws, "s-proj-hot", nil, nil, m.provCfg, "deepseek", m.provCfg.defaultModel(),
		string(provider.ReasoningEffortLevelLow), func() context.Context { return context.Background() },
		subagent.NewRegistry(), events.NewQuestionWaiter(0), buildSkillsRegistry(ws),
		buildSubagentRegistry(t.TempDir()), wsRT.mcp, wsRT.plugins, profiles["code"], nil, nil, nil, nil, nil)
	if _, err := eng.GetTool(context.Background(), "mcp__proj__echo"); err != nil {
		t.Fatalf("项目层服务器应已连接并注册工具: %v", err)
	}
	// 项目层可见（mcp_projects 也从活管理器读到工具数）
	projects := mcpProjects(t, m, buf, 2, []string{ws})
	if projects[ws]["running"] != true {
		t.Fatalf("已打开的项目 running 应为 true: %v", projects[ws])
	}
	if s := mcpProjectServers(t, projects[ws])["proj"]; s == nil || s["file"] == nil {
		t.Fatalf("项目服务器应可见: %v", s)
	}

	// 无运行态的项目：只写盘，不建运行态（下次打开读取）
	wsCold := t.TempDir()
	m.dispatch(cmd(3, "mcp_project_set", map[string]any{"workspace": wsCold, "servers": map[string]any{
		"cold": map[string]any{"type": "stdio", "command": bin},
	}}))
	if resp, _ := lastResp(t, buf); resp["ok"] != true {
		t.Fatalf("无运行态写盘应通过: %v", resp)
	}
	if m.peekRuntime(wsCold) != nil {
		t.Fatalf("无运行态的项目不该因此被创建运行态: %s", wsCold)
	}
	if _, has := jsonFile(t, filepath.Join(wsCold, ".go-code", "settings.json"))["mcpServers"]; !has {
		t.Fatalf("无运行态也应写盘")
	}
}
