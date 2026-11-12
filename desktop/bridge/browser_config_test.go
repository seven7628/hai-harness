package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/seven7628/hai-harness/plugin"
	"github.com/seven7628/hai-harness/plugin/browser"
)

// configPayload 便捷构造 browser_config_set payload。
func configPayload(ws string, overrides map[string]any) map[string]any {
	m := map[string]any{
		"workspace":              ws,
		"session_mode":           browser.SessionPersistent,
		"headless":               false,
		"viewport":               "1280x720",
		"channel":                "",
		"redact_network_headers": true,
		"usage_statistics":       false,
		"url_mode":               browser.URLModeNone,
		"url_patterns":           []string{},
		"accept_insecure_certs":  false,
		"user_data_dir":          "",
	}
	for k, v := range overrides {
		m[k] = v
	}
	return m
}

// TestBrowserConfigGet：HOME 隔离下 browser_config_get 返回默认配置（隐私友好）。
func TestBrowserConfigGet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, buf := testManager(t)
	ws := t.TempDir()
	m.runtime(ws) // 默认 registry（读 HOME 下配置 → 无文件 → 默认）

	m.dispatch(cmd(1, "browser_config_get", map[string]any{"workspace": ws}))
	resp, ok := lastResp(t, buf)
	if !ok || resp["ok"] != true {
		t.Fatalf("browser_config_get 应 ok: %v", resp)
	}
	cfg := resp["data"].(map[string]any)["config"].(map[string]any)
	if cfg["session_mode"] != browser.SessionPersistent {
		t.Errorf("默认会话模式 = %v", cfg["session_mode"])
	}
	if cfg["redact_network_headers"] != true {
		t.Errorf("默认应脱敏: %+v", cfg)
	}
	if cfg["usage_statistics"] != false {
		t.Errorf("默认不应发统计: %+v", cfg)
	}
	if cfg["viewport"] != "1280x720" {
		t.Errorf("默认视口 = %v", cfg["viewport"])
	}
}

// TestBrowserConfigSetPersistsAndSwaps：合法保存 → 落盘 + 工作区 registry 换新（browser 未启用则静默）。
func TestBrowserConfigSetPersistsAndSwaps(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	m, buf := testManager(t)
	ws := t.TempDir()
	m.runtime(ws)
	prev := m.runtime(ws).plugins

	payload := configPayload(ws, map[string]any{
		"session_mode": browser.SessionIsolated,
		"url_mode":     browser.URLModeBlock,
		"url_patterns": []string{"https://ads.example/*"},
	})
	m.dispatch(cmd(1, "browser_config_set", payload))
	resp, ok := lastResp(t, buf)
	if !ok || resp["ok"] != true {
		t.Fatalf("合法保存应 ok: %v", resp)
	}

	// 落盘（roundtrip 一致）
	cfg := browser.LoadConfig(browser.DefaultPluginsDir())
	if cfg.SessionMode != browser.SessionIsolated || cfg.URLMode != browser.URLModeBlock || len(cfg.URLPatterns) != 1 {
		t.Errorf("落盘配置错: %+v", cfg)
	}
	// registry 换新
	if m.runtime(ws).plugins == prev {
		t.Errorf("registry 未换新")
	}

	// 保存后再 get → 反映新配置
	m.dispatch(cmd(2, "browser_config_get", map[string]any{"workspace": ws}))
	resp, ok = lastResp(t, buf)
	if !ok || resp["ok"] != true {
		t.Fatalf("get 应 ok: %v", resp)
	}
	got := resp["data"].(map[string]any)["config"].(map[string]any)
	if got["session_mode"] != browser.SessionIsolated {
		t.Errorf("get 未反映新配置: %+v", got)
	}
}

// TestBrowserConfigSetInvalid：非法配置 → 拒绝（错误响应，不落盘，registry 不变）。
func TestBrowserConfigSetInvalid(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, buf := testManager(t)
	ws := t.TempDir()
	m.runtime(ws)
	prev := m.runtime(ws).plugins
	configPath := filepath.Join(browser.DefaultPluginsDir(), browser.ID, "config.json")

	cases := map[string]map[string]any{
		"坏会话模式":  {"session_mode": "garbage"},
		"坏视口":    {"viewport": "abc"},
		"坏渠道":    {"channel": "nightly"},
		"坏URL模式": {"url_mode": "everything"},
		"模式含空白":  {"url_mode": browser.URLModeBlock, "url_patterns": []string{"https://a b.com/*"}},
	}
	i := 0
	for name, over := range cases {
		i++
		m.dispatch(cmd(i, "browser_config_set", configPayload(ws, over)))
		resp, ok := lastResp(t, buf)
		if !ok || resp["ok"] != false {
			t.Fatalf("%s 应被拒: %v", name, resp)
		}
		if msg := resp["error"].(string); msg == "" {
			t.Errorf("%s 应有错误信息", name)
		}
	}
	// 未落盘 + registry 未换
	if _, err := os.Stat(configPath); err == nil {
		t.Errorf("非法保存不应写文件: %s", configPath)
	}
	if m.runtime(ws).plugins != prev {
		t.Errorf("非法保存不应换 registry")
	}
}

// TestBrowserConfigSetReEnablesWhenEnabled：browser 原先启用 → 热应用换新后自动重启用
// （旧 registry 被关闭：fake 的 enabled 复位；新 registry 因组件未装重启用失败仅记日志）。
func TestBrowserConfigSetReEnablesWhenEnabled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, buf := testManager(t)
	ws := t.TempDir()

	fake := &fakePluginCap{id: browser.ID, tool: fakeBridgeTool{name: "browser_take_snapshot"}, enabled: true}
	injectWS(m, ws, plugin.NewRegistry(fake))
	prev := m.runtime(ws).plugins

	m.dispatch(cmd(1, "browser_config_set", configPayload(ws, map[string]any{
		"session_mode": browser.SessionExisting,
	})))
	resp, ok := lastResp(t, buf)
	if !ok || resp["ok"] != true {
		t.Fatalf("保存应 ok: %v", resp)
	}
	// 旧 registry 被关闭（fake 复位 enabled）
	fake.mu.Lock()
	wasClosed := !fake.enabled
	fake.mu.Unlock()
	if !wasClosed {
		t.Errorf("旧 registry 未关闭（browser 未禁用）")
	}
	if m.runtime(ws).plugins == prev {
		t.Errorf("registry 未换新")
	}
}
