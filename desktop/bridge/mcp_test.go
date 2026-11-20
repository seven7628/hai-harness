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

// lastResp 取缓冲区内最后一条 command_response（dispatch 响应）。
func lastResp(t *testing.T, buf *syncBuf) (map[string]any, bool) {
	t.Helper()
	var resp map[string]any
	found := false
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			continue
		}
		if m["event_type"] == "command_response" {
			resp = m
			found = true
		}
	}
	buf.Reset()
	return resp, found
}

// —— MCP 配置分层（项目级，2026-09）——

// writeMCPConfig 写 MCP 配置文件（serversJSON = mcpServers 段的内容，目录自动创建）。
func writeMCPConfig(t *testing.T, path, serversJSON string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"mcpServers":`+serversJSON+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mcpList 发 mcp_list 并按 name 索引返回服务器列表（断言分层合并结果）。
func mcpList(t *testing.T, m *manager, buf *syncBuf, id int, ws string) map[string]map[string]any {
	t.Helper()
	m.dispatch(cmd(id, "mcp_list", map[string]any{"workspace": ws}))
	resp, ok := lastResp(t, buf)
	if !ok || resp["ok"] != true {
		t.Fatalf("mcp_list 响应异常: %v", resp)
	}
	list, _ := resp["data"].(map[string]any)["servers"].([]any)
	out := map[string]map[string]any{}
	for _, s := range list {
		sm, _ := s.(map[string]any)
		if n, _ := sm["name"].(string); n != "" {
			out[n] = sm
		}
	}
	return out
}

// TestMCPProjectLayers 项目级 MCP 分层（2026-09）：用户 ~/.go-code/settings.json
// < 项目 {ws}/.mcp.json < 项目 {ws}/.go-code/settings.json。
// 覆盖：三层合并与同名覆盖、enabled:false 关闭、source 标注、每工作区隔离、
// mcp_set（用户层全量保存）不抹掉项目层、mcp_reload 免重启生效（增/删/改）。
func TestMCPProjectLayers(t *testing.T) {
	m, buf := testManager(t) // 内部把 HOME 指向临时目录（用户层 settings.json 隔离）
	home := os.Getenv("HOME")
	wsA, wsB := t.TempDir(), t.TempDir()

	// 用户层（跨工作区）：user-1 / both / off
	writeMCPConfig(t, filepath.Join(home, ".go-code", "settings.json"), `{
		"user-1":{"type":"stdio","command":"user-cmd"},
		"both":{"type":"stdio","command":"user-both"},
		"off":{"type":"stdio","command":"keep"}
	}`)
	// 项目 A：.mcp.json（中间层）定义 proj-a + 覆盖 both + 关闭 off
	writeMCPConfig(t, filepath.Join(wsA, ".mcp.json"), `{
		"proj-a":{"type":"http","url":"http://a/mcp"},
		"both":{"type":"stdio","command":"mcpjson-both"},
		"off":{"type":"stdio","command":"keep","enabled":false}
	}`)
	// 项目 A：.go-code/settings.json（最高层）再覆盖 both
	writeMCPConfig(t, filepath.Join(wsA, ".go-code", "settings.json"), `{
		"both":{"type":"stdio","command":"native-both"}
	}`)
	// 项目 B：只有原生项目文件
	writeMCPConfig(t, filepath.Join(wsB, ".go-code", "settings.json"), `{
		"proj-b":{"type":"http","url":"http://b/mcp"}
	}`)

	// 工作区 A：4 项（user-1 / both / off / proj-a），both 由最高层胜出，off 被关闭
	got := mcpList(t, m, buf, 1, wsA)
	if len(got) != 4 {
		t.Fatalf("wsA 服务器数 = %d, want 4: %v", len(got), got)
	}
	if got["both"]["command"] != "native-both" || got["both"]["source"] != "project" {
		t.Errorf("wsA both 应被 .go-code/settings.json 覆盖: %+v", got["both"])
	}
	if got["proj-a"]["source"] != "project" || got["proj-a"]["type"] != "http" {
		t.Errorf("wsA proj-a 应来自项目层: %+v", got["proj-a"])
	}
	if got["user-1"]["source"] != "user" || got["user-1"]["command"] != "user-cmd" {
		t.Errorf("wsA user-1 应来自用户层: %+v", got["user-1"])
	}
	if got["off"]["enabled"] != false {
		t.Errorf("wsA off 应被项目层 enabled:false 关闭: %+v", got["off"])
	}

	// 工作区 B：项目层独立——用户层三项照旧 + 自己的 proj-b；wsA 的项目级覆盖/关闭不串味
	gotB := mcpList(t, m, buf, 2, wsB)
	if len(gotB) != 4 || gotB["proj-b"] == nil {
		t.Fatalf("wsB 应为用户层三项 + proj-b: %v", gotB)
	}
	if gotB["both"]["command"] != "user-both" || gotB["both"]["source"] != "user" {
		t.Errorf("wsB both 不应受 wsA 项目层覆盖: %+v", gotB["both"])
	}
	if gotB["off"]["enabled"] != true {
		t.Errorf("wsB off 不应被 wsA 的 enabled:false 关闭: %+v", gotB["off"])
	}
	if _, ok := gotB["proj-a"]; ok {
		t.Error("项目层不应跨工作区串味（wsB 出现了 wsA 的 proj-a）")
	}

	// mcp_set（MCP tab 保存 = 用户层全量）后项目层仍在：不能把项目层从活管理器里抹掉
	m.dispatch(cmd(3, "mcp_set", map[string]any{"servers": map[string]any{
		"user-1": map[string]any{"type": "stdio", "command": "user-cmd"},
		"both":   map[string]any{"type": "stdio", "command": "user-both"},
		"off":    map[string]any{"type": "stdio", "command": "keep"},
	}}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != true {
		t.Fatalf("mcp_set 应通过: %v", resp)
	}
	got = mcpList(t, m, buf, 4, wsA)
	if got["proj-a"] == nil {
		t.Fatalf("mcp_set 后项目层服务器被抹掉: %v", got)
	}
	if got["both"]["command"] != "native-both" || got["off"]["enabled"] != false {
		t.Errorf("mcp_set 后项目层覆盖应保持: %v", got)
	}

	// mcp_reload：项目里改配置（新增 proj-a2 / 删除 proj-a）免重启生效
	writeMCPConfig(t, filepath.Join(wsA, ".mcp.json"), `{
		"proj-a2":{"type":"http","url":"http://a2/mcp"}
	}`)
	m.dispatch(cmd(5, "mcp_reload", map[string]any{"workspace": wsA}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != true {
		t.Fatalf("mcp_reload 应通过: %v", resp)
	}
	got = mcpList(t, m, buf, 6, wsA)
	if got["proj-a2"] == nil || got["proj-a"] != nil {
		t.Errorf("mcp_reload 未生效（应新增 proj-a2 且移除 proj-a）: %v", got)
	}
	if got["both"]["command"] != "native-both" {
		t.Errorf("mcp_reload 后原生项目层覆盖应保持: %v", got)
	}
	// 须 workspace
	m.dispatch(cmd(7, "mcp_reload", nil))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != false {
		t.Fatalf("mcp_reload 缺 workspace 应报错: %v", resp)
	}
}

// TestSessionEngineMCPTools 端到端集成：settings.json 的 mcpServers（真实 stdio 子进程）
// → 工作区 manager 连接 → newSessionEngine 注册 mcp__e2e__echo 工具（引擎可见）。
// 同轮验证项目级分层：{ws}/.mcp.json 的服务器同样注册（mcp__e2e-proj__echo）。
func TestSessionEngineMCPTools(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("需要 go 工具链")
	}
	// HOME 隔离前先取真实模块缓存（t.Setenv HOME 会让嵌套 go build 找不到 $HOME/go/pkg/mod）
	gomod := ""
	if out, err := exec.Command("go", "env", "GOMODCACHE").Output(); err == nil {
		gomod = strings.TrimSpace(string(out))
	}
	t.Setenv("HOME", t.TempDir())
	// 构建 fixture MCP 服务器（mcp/testdata/server）
	bin := filepath.Join(t.TempDir(), "e2e-server")
	build := exec.Command("go", "build", "-o", bin, "../../mcp/testdata/server")
	build.Env = append(os.Environ(), "GOMODCACHE="+gomod, "GOPROXY=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建 fixture: %v\n%s", err, out)
	}
	// 写 settings.json（Electron 持久化的 mcpServers 段）。必须先 testManager()：
	// 它会重设 HOME 到隔离临时目录，之后写入才会被 manager 的 homeCfgPath 读到
	//（先写后建 manager 会被覆盖的 HOME 丢弃配置 → MCP 工具注册为空）。
	m, _ := testManager(t)
	ws := t.TempDir()
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"e2e": map[string]any{"type": "stdio", "command": bin},
		},
	}
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".go-code"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".go-code", "settings.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	// 项目层（{ws}/.mcp.json，同一 fixture 二进制作第二台服务器）：须在建 manager 之前落盘
	projCfg, _ := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"e2e-proj": map[string]any{"type": "stdio", "command": bin},
		},
	})
	if err := os.WriteFile(filepath.Join(ws, ".mcp.json"), projCfg, 0o644); err != nil {
		t.Fatal(err)
	}

	reg := subagent.NewRegistry()
	qw := events.NewQuestionWaiter(0)
	sk := buildSkillsRegistry(ws)
	eng, _ := newSessionEngine(ws, "", nil, nil, m.provCfg, "deepseek", m.provCfg.defaultModel(), string(provider.ReasoningEffortLevelLow), func() context.Context { return context.Background() }, reg, qw, sk, buildSubagentRegistry(t.TempDir()), m.runtime(ws).mcp, m.runtime(ws).plugins, profiles["code"], nil, nil, nil, nil, nil)

	// MCP 工具已注册（连接真实子进程并列出 echo）
	tool, err := eng.GetTool(context.Background(), "mcp__e2e__echo")
	if err != nil {
		t.Fatalf("MCP 工具未注册: %v", err)
	}
	if tool.Description() == "" {
		t.Fatalf("MCP 工具描述为空")
	}
	// 调用走真实子进程
	res, err := tool.Call(context.Background(), "mcp__e2e__echo", `{"text":"集成测试"}`)
	if err != nil {
		t.Fatalf("MCP 调用失败: %v", err)
	}
	if res != "echo: 集成测试" {
		t.Fatalf("结果 = %q", res)
	}
	// 引擎 ToolParams 含该工具（模型可见）
	schemas, err := eng.ToolParams(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range schemas {
		if s.Name == "mcp__e2e__echo" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ToolParams 缺 mcp__e2e__echo")
	}

	// 项目层服务器（{ws}/.mcp.json）同样连上并注册工具 —— 分层端到端
	pj, err := eng.GetTool(context.Background(), "mcp__e2e-proj__echo")
	if err != nil {
		t.Fatalf("项目层 MCP 工具未注册: %v", err)
	}
	res2, err := pj.Call(context.Background(), "mcp__e2e-proj__echo", `{"text":"项目层"}`)
	if err != nil {
		t.Fatalf("项目层 MCP 调用失败: %v", err)
	}
	if res2 != "echo: 项目层" {
		t.Fatalf("项目层结果 = %q", res2)
	}
	// 状态标注来源层（MCP tab 展示依据）
	srcBy := map[string]string{}
	for _, st := range m.runtime(ws).mcp.Status() {
		srcBy[st.Name] = st.Source
	}
	if srcBy["e2e"] != "user" || srcBy["e2e-proj"] != "project" {
		t.Fatalf("MCP 来源标注错: %v", srcBy)
	}
}

// TestMCPCommands：mcp_set 校验 + 全量应用 + mcp_list 状态 + mcp_refresh 错误路径。
// HOME 指向临时目录，避免读到真实 ~/.go-code/settings.json（隔离配置）。
func TestMCPCommands(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, buf := testManager(t)
	ws := t.TempDir()

	// 先建工作区（mcp_set 应用到已存在工作区管理器）
	m.runtime(ws)

	// 非法 type → 报错
	m.dispatch(cmd(1, "mcp_set", map[string]any{"servers": map[string]any{
		"x": map[string]any{"type": "badtype"},
	}}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != false {
		t.Fatalf("非法 type 应拒绝: %v", resp)
	}
	// 非法 stdio（缺 command）→ 报错
	m.dispatch(cmd(2, "mcp_set", map[string]any{"servers": map[string]any{
		"x": map[string]any{"type": "stdio"},
	}}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != false {
		t.Fatalf("缺 command 应拒绝: %v", resp)
	}

	// 合法全量配置 → ok；列表反映（未连接 = idle）
	m.dispatch(cmd(3, "mcp_set", map[string]any{"servers": map[string]any{
		"github": map[string]any{"type": "stdio", "command": "npx", "args": []any{"-y", "@modelcontextprotocol/server-github"}, "env": map[string]any{"TOKEN": "x"}},
		"memory": map[string]any{"type": "http", "url": "http://localhost:8080/mcp"},
	}}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != true {
		t.Fatalf("合法配置应通过: %v", resp)
	}

	m.dispatch(cmd(4, "mcp_list", map[string]any{"workspace": ws}))
	resp, _ := lastResp(t, buf)
	if resp["ok"] != true {
		t.Fatalf("mcp_list 应 ok: %v", resp)
	}
	servers, _ := resp["data"].(map[string]any)["servers"].([]any)
	if len(servers) != 2 {
		t.Fatalf("mcp_list 服务器数 = %d, want 2: %v", len(servers), resp["data"])
	}
	byName := map[string]map[string]any{}
	for _, s := range servers {
		sm := s.(map[string]any)
		byName[sm["name"].(string)] = sm
	}
	gh := byName["github"]
	if gh == nil {
		t.Fatalf("缺 github: %v", servers)
	}
	if gh["state"] != "idle" || gh["enabled"] != true || gh["tool_count"] != float64(0) {
		t.Errorf("github 应 idle enabled: %+v", gh)
	}
	if byName["memory"]["type"] != "http" || byName["memory"]["command"] != "http://localhost:8080/mcp" {
		t.Errorf("memory 配置展示错: %+v", byName["memory"])
	}

	// mcp_refresh 未知名 → 报错
	m.dispatch(cmd(5, "mcp_refresh", map[string]any{"workspace": ws, "name": "nope"}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != false {
		t.Fatalf("mcp_refresh 未知名应报错: %v", resp)
	}

	// 移除（全量 map 不含 github）→ 列表只剩 memory
	m.dispatch(cmd(6, "mcp_set", map[string]any{"servers": map[string]any{
		"memory": map[string]any{"type": "http", "url": "http://localhost:8080/mcp"},
	}}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != true {
		t.Fatalf("移除应通过: %v", resp)
	}
	m.dispatch(cmd(7, "mcp_list", map[string]any{"workspace": ws}))
	resp, _ = lastResp(t, buf)
	servers, _ = resp["data"].(map[string]any)["servers"].([]any)
	if len(servers) != 1 || servers[0].(map[string]any)["name"] != "memory" {
		t.Fatalf("移除后列表 = %v", resp["data"])
	}
}
