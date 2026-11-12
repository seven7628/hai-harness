package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// waitMCP 轮询 mcp_list 直到条件满足（watcher 是异步的：事件 → debounce → 应用）。
// 超时即失败并打印最后一次观测到的服务器与分层状态（排障用）。
func waitMCP(t *testing.T, m *manager, buf *syncBuf, ws string, timeout time.Duration, cond func(servers map[string]map[string]any, layers []any) bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last map[string]map[string]any
	var lastLayers []any
	for time.Now().Before(deadline) {
		servers, layers := mcpListFull(t, m, buf, ws)
		last, lastLayers = servers, layers
		if cond(servers, layers) {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("等待 MCP 配置生效超时（%s）\nservers=%v\nlayers=%v", timeout, last, lastLayers)
}

// mcpListFull 发 mcp_list，返回按 name 索引的服务器表 + 原始 layers 数组。
func mcpListFull(t *testing.T, m *manager, buf *syncBuf, ws string) (map[string]map[string]any, []any) {
	t.Helper()
	m.dispatch(cmd(0, "mcp_list", map[string]any{"workspace": ws}))
	resp, ok := lastResp(t, buf)
	if !ok || resp["ok"] != true {
		t.Fatalf("mcp_list 响应异常: %v", resp)
	}
	data, _ := resp["data"].(map[string]any)
	list, _ := data["servers"].([]any)
	layers, _ := data["layers"].([]any)
	out := map[string]map[string]any{}
	for _, s := range list {
		sm, _ := s.(map[string]any)
		if n, _ := sm["name"].(string); n != "" {
			out[n] = sm
		}
	}
	return out, layers
}

// layerOf 取某层状态（按 path 后缀匹配）。
func layerOf(layers []any, suffix string) map[string]any {
	for _, l := range layers {
		lm, _ := l.(map[string]any)
		if p, _ := lm["path"].(string); p != "" && filepath.Base(p) == filepath.Base(suffix) &&
			len(p) >= len(suffix) && p[len(p)-len(suffix):] == suffix {
			return lm
		}
	}
	return nil
}

// TestMCPWatchLifecycle 文件监听端到端（用户问的两个场景 + 损坏恢复）：
//  1. 文件最开始不存在 → 后来新增（{ws}/.mcp.json 与 {ws}/.go-code/settings.json 都是后建，
//     其中 .go-code 目录本身也是后建——覆盖「目录后建需重挂」）
//  2. 已存在文件的内容变更（改 command）
//  3. 文件被写坏（半截 JSON）→ 沿用上次有效（服务器不消失）+ 层状态报 stale
//  4. 修好 → 自动切回新内容
func TestMCPWatchLifecycle(t *testing.T) {
	m, buf := testManager(t)
	home := os.Getenv("HOME")
	ws := t.TempDir()

	// 用户层先有一条（证明分层共存）
	writeMCPConfig(t, filepath.Join(home, ".go-code", "settings.json"), `{
		"user-1":{"type":"stdio","command":"user-cmd"}
	}`)
	// 建立运行态：此刻项目层文件都还不存在
	m.runtime(ws)
	servers, _ := mcpListFull(t, m, buf, ws)
	if len(servers) != 1 || servers["user-1"] == nil {
		t.Fatalf("初始应只有用户层: %v", servers)
	}

	// 场景 1a：项目文件后建（.mcp.json）
	writeMCPConfig(t, filepath.Join(ws, ".mcp.json"), `{
		"late":{"type":"http","url":"http://late/mcp"}
	}`)
	waitMCP(t, m, buf, ws, 5*time.Second, func(s map[string]map[string]any, _ []any) bool {
		return s["late"] != nil && s["late"]["source"] == "project"
	})

	// 场景 1b：项目配置目录 + 原生项目文件后建（.go-code 目录此前不存在 → 需重挂）
	writeMCPConfig(t, filepath.Join(ws, ".go-code", "settings.json"), `{
		"native":{"type":"http","url":"http://native/mcp"}
	}`)
	waitMCP(t, m, buf, ws, 5*time.Second, func(s map[string]map[string]any, _ []any) bool {
		return s["native"] != nil
	})

	// 场景 2：内容变更（改 command）→ 自动重载
	writeMCPConfig(t, filepath.Join(ws, ".mcp.json"), `{
		"late":{"type":"http","url":"http://late-changed/mcp"}
	}`)
	waitMCP(t, m, buf, ws, 5*time.Second, func(s map[string]map[string]any, _ []any) bool {
		return s["late"] != nil && s["late"]["command"] == "http://late-changed/mcp"
	})

	// 场景 3：写坏（半截 JSON）→ 沿用上次有效（late/native 都还在）+ 层状态 stale
	if err := os.WriteFile(filepath.Join(ws, ".mcp.json"), []byte(`{"mcpServers":{"late":{"type":"htt`), 0o644); err != nil {
		t.Fatal(err)
	}
	waitMCP(t, m, buf, ws, 5*time.Second, func(s map[string]map[string]any, layers []any) bool {
		l := layerOf(layers, ".mcp.json")
		return l != nil && l["stale"] == true && l["ok"] == false && l["error"] != ""
	})
	servers, layers := mcpListFull(t, m, buf, ws)
	if servers["late"] == nil || servers["late"]["command"] != "http://late-changed/mcp" {
		t.Fatalf("坏文件不应清空该层（应沿用上次有效）: %v", servers)
	}
	if servers["native"] == nil || servers["user-1"] == nil {
		t.Fatalf("其他层不受影响: %v", servers)
	}
	if l := layerOf(layers, ".mcp.json"); l["servers"] != float64(1) {
		t.Errorf("stale 层应带上次有效的服务器数: %v", l)
	}

	// 场景 4：修好 → 自动切回（换成新服务器，且不再 stale）
	writeMCPConfig(t, filepath.Join(ws, ".mcp.json"), `{
		"late2":{"type":"http","url":"http://late2/mcp"}
	}`)
	waitMCP(t, m, buf, ws, 5*time.Second, func(s map[string]map[string]any, layers []any) bool {
		l := layerOf(layers, ".mcp.json")
		return s["late2"] != nil && s["late"] == nil && l != nil && l["stale"] == false && l["ok"] == true
	})
}

// TestMCPWatchUserLayerAndCorruption 用户层（~/.go-code/settings.json，Electron 写盘目标）
// 同样被监听：新增服务器自动生效；写坏时沿用上次有效（否则一次半写会让全部服务器消失）。
func TestMCPWatchUserLayerAndCorruption(t *testing.T) {
	m, buf := testManager(t)
	home := os.Getenv("HOME")
	ws := t.TempDir()
	userFile := filepath.Join(home, ".go-code", "settings.json")

	writeMCPConfig(t, userFile, `{"g1":{"type":"http","url":"http://g1/mcp"}}`)
	m.runtime(ws)
	waitMCP(t, m, buf, ws, 5*time.Second, func(s map[string]map[string]any, _ []any) bool {
		return s["g1"] != nil && s["g1"]["source"] == "user"
	})

	// 追加服务器（内容变更）
	writeMCPConfig(t, userFile, `{
		"g1":{"type":"http","url":"http://g1/mcp"},
		"g2":{"type":"http","url":"http://g2/mcp"}
	}`)
	waitMCP(t, m, buf, ws, 5*time.Second, func(s map[string]map[string]any, _ []any) bool {
		return s["g2"] != nil
	})

	// 半写（截断到空文件）→ 沿用上次有效，g1/g2 都不消失
	if err := os.WriteFile(userFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitMCP(t, m, buf, ws, 5*time.Second, func(_ map[string]map[string]any, layers []any) bool {
		l := layerOf(layers, "settings.json")
		return l != nil && l["stale"] == true
	})
	servers, _ := mcpListFull(t, m, buf, ws)
	if servers["g1"] == nil || servers["g2"] == nil {
		t.Fatalf("空文件（半写）不应让用户层服务器消失: %v", servers)
	}

	// 有意清空该层：写有效 JSON `{}` → 服务器消失（与「坏文件」区分开）
	writeMCPConfig(t, userFile, `{}`)
	waitMCP(t, m, buf, ws, 5*time.Second, func(s map[string]map[string]any, layers []any) bool {
		l := layerOf(layers, "settings.json")
		return s["g1"] == nil && s["g2"] == nil && l != nil && l["ok"] == true && l["stale"] == false
	})
}

// TestMCPWatchDebounce 多条事件合并成一次应用（编辑器一次保存会连发 CHMOD/WRITE/CREATE）：
// 直接单测 watcher，避开 bridge 其他异步噪声。
func TestMCPWatchDebounce(t *testing.T) {
	ws := t.TempDir()
	applied := make(chan struct{}, 16)
	w := newMCPWatcher(ws, func() { applied <- struct{}{} })
	w.debounce = 60 * time.Millisecond // 缩短静默期（测试提速；语义不变）
	if err := w.start(); err != nil {
		t.Fatal(err)
	}
	defer w.stop()

	target := filepath.Join(ws, ".mcp.json")
	for i := 0; i < 5; i++ {
		writeMCPConfig(t, target, `{"s":{"type":"http","url":"http://x/mcp"}}`)
		time.Sleep(10 * time.Millisecond) // 同一批（< debounce）
	}
	select {
	case <-applied:
	case <-time.After(3 * time.Second):
		t.Fatal("写入后未触发应用")
	}
	// 静默期内不应再有第二次应用（5 次写 → 1 次应用）
	select {
	case <-applied:
		t.Fatal("同一批事件应合并为一次应用（debounce 失效）")
	case <-time.After(400 * time.Millisecond):
	}
	if n, armed, err := w.stats(); n < 1 || armed == 0 || err != "" {
		t.Errorf("watcher 统计异常: applies=%d armed=%d err=%q", n, armed, err)
	}

	// 无关文件不触发（{ws} 目录里同名但不是任何层）
	other := filepath.Join(ws, "sub", "settings.json")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-applied:
		t.Fatal("非分层路径的 settings.json 不应触发重载")
	case <-time.After(300 * time.Millisecond):
	}

	// 停止后不再应用（幂等 stop）
	w.stop()
	writeMCPConfig(t, target, `{"s":{"type":"http","url":"http://y/mcp"}}`)
	select {
	case <-applied:
		t.Fatal("stop 后不应再应用")
	case <-time.After(400 * time.Millisecond):
	}
}

// TestMCPWatchStoppedOnDeleteWorkspace 删除工作区必须停掉监听：否则删完再改文件会经
// apply → m.runtime(ws) 把运行态「复活」（并新建目录），是真实的资源/状态泄漏。
func TestMCPWatchStoppedOnDeleteWorkspace(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	writeMCPConfig(t, filepath.Join(ws, ".mcp.json"), `{"a":{"type":"http","url":"http://a/mcp"}}`)
	m.runtime(ws)

	m.dispatch(cmd(1, "delete_workspace", map[string]any{"workspace": ws}))
	if !lastRespOK(t, buf, 1) {
		t.Fatal("delete_workspace 未 ok")
	}
	if _, ok := m.workspaces[ws]; ok {
		t.Fatal("删除后运行态应已摘除")
	}
	// 删完再写配置文件：若监听未停，apply 会重建运行态
	writeMCPConfig(t, filepath.Join(ws, ".mcp.json"), `{"b":{"type":"http","url":"http://b/mcp"}}`)
	time.Sleep(700 * time.Millisecond) // > debounce，足够让泄漏的 watcher 触发
	m.mu.Lock()
	_, revived := m.workspaces[ws]
	m.mu.Unlock()
	if revived {
		t.Fatal("删除工作区后监听未停止（运行态被配置文件变更复活）")
	}
}

// TestMCPWatchJSONShape 分层状态是可直接序列化的 JSON（UI 消费契约）：字段名稳定。
func TestMCPWatchJSONShape(t *testing.T) {
	m, buf := testManager(t)
	home := os.Getenv("HOME")
	ws := t.TempDir()
	writeMCPConfig(t, filepath.Join(home, ".go-code", "settings.json"), `{"u":{"type":"http","url":"http://u/mcp"}}`)
	m.runtime(ws)
	m.dispatch(cmd(1, "mcp_list", map[string]any{"workspace": ws}))
	resp, _ := lastResp(t, buf)
	raw, err := json.Marshal(resp["data"])
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Layers []struct {
			Path, Source string
			Present, OK  bool
			Stale        bool
			Servers      int
		} `json:"layers"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("layers 反序列化失败: %v (%s)", err, raw)
	}
	if len(decoded.Layers) != 3 {
		t.Fatalf("应有 3 层（用户 + 项目 .mcp.json + 项目 .go-code/settings.json）: %s", raw)
	}
	found := false
	for _, l := range decoded.Layers {
		if l.Source == "user" && l.Present && l.OK && l.Servers == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("用户层状态应 present+ok+1 台: %s", raw)
	}
}
