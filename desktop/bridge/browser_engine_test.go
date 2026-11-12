package main

// Browser 引擎开关测试（ego/mcp 互斥，2026-09）：
//   - 引擎读取/规范化（settings.json 缺省 = mcp；ego 显式）
//   - autoEnablePlugins 按引擎跳过 browser 插件
//   - plugin_enable 在 ego 引擎下拒绝 browser
//   - browser_engine_set 切换编排（切 ego 禁用、切 mcp 恢复）

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/plugin"
	"github.com/seven7628/hai-harness/plugin/browser"
)

// writeSettingsEngine 写 settings.json 的 browser_engine（测试隔离 HOME）。
func writeSettingsEngine(t *testing.T, eng BrowserEngine) {
	t.Helper()
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".go-code")
	_ = os.MkdirAll(dir, 0o700)
	raw := map[string]any{"browser_engine": string(eng)}
	b, _ := json.Marshal(raw)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// mkBrowserWs 建含 fake browser 插件的工作区（复用 fakePluginCap，id=browser）。
func mkBrowserWs(t *testing.T, m *manager, wsPath string) *fakePluginCap {
	t.Helper()
	fake := &fakePluginCap{id: browser.ID, tool: fakeBridgeTool{name: "browser_test_tool"}}
	injectWS(m, wsPath, plugin.NewRegistry(fake))
	return fake
}

// fakeIsEnabled 便捷读 fakePluginCap 的 enabled（走 IsEnabled 带锁）。
func fakeIsEnabled(f *fakePluginCap) bool { return f.IsEnabled() }

// TestNormalizeBrowserEngine 规范化：合法值 + 非法/空回退 mcp。
func TestNormalizeBrowserEngine(t *testing.T) {
	cases := []struct {
		in   string
		want BrowserEngine
	}{
		{"mcp", EngineMCP}, {"ego", EngineEgo},
		{"", EngineMCP}, {"MCP", EngineMCP}, {"chrome", EngineMCP}, {" ", EngineMCP},
	}
	for _, c := range cases {
		if got := normalizeBrowserEngine(c.in); got != c.want {
			t.Errorf("normalizeBrowserEngine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLoadBrowserEngine 读取：settings 有值 / 缺省。
func TestLoadBrowserEngine(t *testing.T) {
	m, _ := testManager(t)
	// 无 settings.json → 默认 mcp
	if got := loadBrowserEngine(); got != EngineMCP {
		t.Fatalf("无 settings 默认 = %q, want mcp", got)
	}
	writeSettingsEngine(t, EngineEgo)
	m.refreshBrowserEngine()
	if got := m.getBrowserEngine(); got != EngineEgo {
		t.Fatalf("读 settings ego 后 = %q, want ego", got)
	}
	writeSettingsEngine(t, EngineMCP)
	m.refreshBrowserEngine()
	if got := m.getBrowserEngine(); got != EngineMCP {
		t.Fatalf("读 settings mcp 后 = %q, want mcp", got)
	}
}

// TestAutoEnableSkipsBrowserOnEgo 引擎=ego：autoEnablePlugins 跳过 browser 插件。
func TestAutoEnableSkipsBrowserOnEgo(t *testing.T) {
	m, _ := testManager(t)
	wsPath := filepath.Join(t.TempDir(), "ws")
	fake := mkBrowserWs(t, m, wsPath)
	fake.mu.Lock()
	fake.wasEnabled = true
	fake.mu.Unlock()

	m.setBrowserEngine(EngineEgo)
	m.autoEnablePlugins(m.runtime(wsPath))
	// 等异步 goroutine 结束（跳过路径不 Enable）
	time.Sleep(300 * time.Millisecond)
	if fakeIsEnabled(fake) {
		t.Fatal("引擎=ego 时 browser 插件不应被自动启用")
	}
	if fake.installs != 0 || fake.enabled {
		t.Fatal("引擎=ego 时 browser 不应 Enable")
	}
}

// TestAutoEnableRestoresOnMCP 引擎=mcp：autoEnablePlugins 照常恢复 browser（回归现状）。
func TestAutoEnableRestoresOnMCP(t *testing.T) {
	m, _ := testManager(t)
	wsPath := filepath.Join(t.TempDir(), "ws")
	fake := mkBrowserWs(t, m, wsPath)
	fake.mu.Lock()
	fake.wasEnabled = true
	fake.mu.Unlock()

	m.setBrowserEngine(EngineMCP)
	m.autoEnablePlugins(m.runtime(wsPath))
	waitFor(t, func() bool { return fakeIsEnabled(fake) }, "mcp 引擎应恢复 browser 启用")
}

// TestPluginEnableRejectedOnEgo 引擎=ego：plugin_enable 拒绝 browser 并给指引。
func TestPluginEnableRejectedOnEgo(t *testing.T) {
	m, buf := testManager(t)
	wsPath := filepath.Join(t.TempDir(), "ws")
	fake := mkBrowserWs(t, m, wsPath)

	m.setBrowserEngine(EngineEgo)
	m.handlePluginCommand(command{
		Type:    "plugin_enable",
		Payload: map[string]any{"workspace": wsPath, "id": browser.ID},
	})
	lines := readLines(buf)
	if len(lines) == 0 {
		t.Fatal("应有响应")
	}
	if ok, _ := lines[0]["ok"].(bool); ok {
		t.Fatalf("引擎=ego 时 plugin_enable(browser) 应被拒绝，got ok: %v", lines[0])
	}
	if errMsg, _ := lines[0]["error"].(string); errMsg == "" {
		t.Fatal("拒绝应带指引信息")
	}
	if fakeIsEnabled(fake) {
		t.Fatal("被拒绝后不应 enabled")
	}
}

// TestPluginEnableAllowedOnMCP 引擎=mcp：plugin_enable 正常启用 browser。
func TestPluginEnableAllowedOnMCP(t *testing.T) {
	m, buf := testManager(t)
	wsPath := filepath.Join(t.TempDir(), "ws")
	fake := mkBrowserWs(t, m, wsPath)

	m.setBrowserEngine(EngineMCP)
	m.handlePluginCommand(command{
		Type:    "plugin_enable",
		Payload: map[string]any{"workspace": wsPath, "id": browser.ID},
	})
	lines := readLines(buf)
	if len(lines) == 0 {
		t.Fatal("应有响应")
	}
	if ok, _ := lines[0]["ok"].(bool); !ok {
		t.Fatalf("引擎=mcp plugin_enable(browser) 应成功: %v", lines[0])
	}
	if !fakeIsEnabled(fake) {
		t.Fatal("引擎=mcp 插件应 enabled")
	}
}

// TestBrowserEngineSetSwitch 命令切换：ego 禁用已启用 browser；mcp 恢复（wasEnabled）。
func TestBrowserEngineSetSwitch(t *testing.T) {
	m, buf := testManager(t)
	wsPath := filepath.Join(t.TempDir(), "ws")
	fake := mkBrowserWs(t, m, wsPath)

	// 先 mcp 引擎启用插件
	m.setBrowserEngine(EngineMCP)
	m.handlePluginCommand(command{
		Type:    "plugin_enable",
		Payload: map[string]any{"workspace": wsPath, "id": browser.ID},
	})
	_ = readLines(buf)
	if !fakeIsEnabled(fake) {
		t.Fatal("预启用后应 enabled")
	}

	// 切 ego → 插件被禁用
	m.dispatch(command{Id: 1, Type: "browser_engine_set", Payload: map[string]any{"engine": "ego"}})
	lines := readLines(buf)
	// 过滤 plugin_status 事件（emitPluginStatus 同步写），找 command_response
	var respLine map[string]any
	for _, l := range lines {
		if l["event_type"] == "command_response" {
			respLine = l
			break
		}
	}
	if respLine == nil {
		t.Fatalf("browser_engine_set 无 command_response: %v", lines)
	}
	if ok, _ := respLine["ok"].(bool); !ok {
		t.Fatalf("browser_engine_set 应成功: %v", respLine)
	}
	data, _ := respLine["data"].(map[string]any)
	if data["browser_engine"] != "ego" {
		t.Fatalf("引擎应为 ego: %v", data)
	}
	if fakeIsEnabled(fake) {
		t.Fatal("切 ego 后 browser 插件应被禁用")
	}
	if m.getBrowserEngine() != EngineEgo {
		t.Fatal("manager 引擎应为 ego")
	}

	// 切回 mcp → wasEnabled 标记仍在 → 异步恢复
	m.dispatch(command{Id: 2, Type: "browser_engine_set", Payload: map[string]any{"engine": "mcp"}})
	waitFor(t, func() bool { return fakeIsEnabled(fake) }, "切回 mcp 应恢复 browser 启用")
	if m.getBrowserEngine() != EngineMCP {
		t.Fatal("manager 引擎应为 mcp")
	}
}

// TestBrowserEngineGet 查询命令返回当前引擎 + ego 状态结构。
func TestBrowserEngineGet(t *testing.T) {
	m, buf := testManager(t)
	m.setBrowserEngine(EngineEgo)
	m.dispatch(command{Id: 1, Type: "browser_engine_get", Payload: map[string]any{}})
	lines := readLines(buf)
	if len(lines) == 0 {
		t.Fatal("browser_engine_get 应有响应")
	}
	if ok, _ := lines[0]["ok"].(bool); !ok {
		t.Fatalf("browser_engine_get 应成功: %v", lines[0])
	}
	data, _ := lines[0]["data"].(map[string]any)
	if data["browser_engine"] != "ego" {
		t.Fatalf("引擎应为 ego: %v", data)
	}
	ego, ok := data["ego"].(map[string]any)
	if !ok {
		t.Fatalf("应有 ego 状态: %v", data)
	}
	for _, k := range []string{"available", "app_running", "cli_found"} {
		if _, ok := ego[k]; !ok {
			t.Fatalf("ego 状态缺字段 %s: %v", k, ego)
		}
	}
}
