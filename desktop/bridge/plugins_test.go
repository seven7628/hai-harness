package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/plugin"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/skills"
	"github.com/seven7628/hai-harness/subagent"
	"github.com/seven7628/hai-harness/tools"
)

// fakePluginCap 测试能力（验证 plugin_* 命令按 id 寻址、生命周期协调）。
// mu：plugin_install 已异步化（后台 goroutine 读 State），fake 字段需并发安全。
type fakePluginCap struct {
	mu          sync.Mutex
	id          string
	tool        tools.Tool
	enabled     bool
	wasEnabled  bool // 跨进程持久标记（模拟 WasEnabled）
	installs    int
	uninstalls  int
	failInstall bool // Install 注入失败（验证错误透传路径）
}

func (f *fakePluginCap) ID() string          { return f.id }
func (f *fakePluginCap) Name() string        { return "fake-" + f.id }
func (f *fakePluginCap) Description() string { return "fake cap" }
func (f *fakePluginCap) State(_ context.Context) plugin.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.enabled {
		return plugin.State{Status: plugin.StatusEnabled}
	}
	return plugin.State{Status: plugin.StatusNotInstalled}
}
func (f *fakePluginCap) Install(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failInstall {
		return fmt.Errorf("注入的安装失败")
	}
	f.installs++
	return nil
}
func (f *fakePluginCap) Uninstall(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uninstalls++
	f.enabled = false
	return nil
}
func (f *fakePluginCap) Enable(_ context.Context) ([]tools.Tool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled = true
	f.wasEnabled = true
	return []tools.Tool{f.tool}, nil
}
func (f *fakePluginCap) Disable(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled = false
	f.wasEnabled = false
	return nil
}

// ClosePreserve 关闭语义：释放运行时但保留持久标记（对齐 browser.Plugin——
// 引擎切换编排用，切回后可自动恢复）。
func (f *fakePluginCap) ClosePreserve(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled = false
	return nil
}

// WasEnabled 跨进程持久标记（模拟 browser.Plugin 的 .enabled 标记文件）。
func (f *fakePluginCap) WasEnabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wasEnabled
}

// IsEnabled 运行时事实（对齐 browser.Plugin.IsEnabled——升级编排/引擎切换据此判定）。
func (f *fakePluginCap) IsEnabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled
}

// fakeBridgeTool 最小 tools.Tool（验证工具经插件 → 引擎流转）。
type fakeBridgeTool struct{ name string }

func (t fakeBridgeTool) Name() string                                      { return t.name }
func (t fakeBridgeTool) Description() string                               { return "fake bridge tool" }
func (t fakeBridgeTool) Parameters() any                                   { return map[string]any{"type": "object"} }
func (t fakeBridgeTool) CanParallel() bool                                 { return true }
func (t fakeBridgeTool) ValidParams(context.Context, string, string) error { return nil }
func (t fakeBridgeTool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}
func (t fakeBridgeTool) Call(context.Context, string, string) (string, error) { return "ok", nil }
func (t fakeBridgeTool) AfterCall(context.Context, core.ToolCall)             {}

// injectWS 注入带指定插件注册表的工作区（绕过 runtime() 的默认 buildPlugins）。
func injectWS(m *manager, ws string, plg *plugin.Registry) {
	m.workspaces[ws] = &workspaceRuntime{path: ws, plugins: plg, sessions: map[string]*bridgeSession{}}
}

// TestBrowserHeadlessEnv：GO_CODE_BROWSER_HEADLESS env → browserHeadless() 布尔映射。
// E2E/CI 用无窗口运行；默认（未设置）有头可见。
func TestBrowserHeadlessEnv(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},      // 未设置 → 有头（产品默认）
		{"0", false},     // 显式 0 → 有头
		{"false", false}, // 显式 false → 有头
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"1 ", true}, // 容忍空白
	}
	for _, c := range cases {
		t.Setenv("GO_CODE_BROWSER_HEADLESS", c.val)
		if got := browserHeadless(); got != c.want {
			t.Errorf("browserHeadless(%q) = %v, want %v", c.val, got, c.want)
		}
	}
}

// TestPluginListRealBrowser：真实 registry（buildPlugins）→ plugin_list 返回 browser，
// 隔离 HOME 下（无 GO_CODE_BROWSER_VENDOR_DIR 注入）组件无法同步 → error（组件缺失）。
func TestPluginListRealBrowser(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GO_CODE_BROWSER_VENDOR_DIR", "") // 测试无内置 vendor：组件缺失
	m, buf := testManager(t)
	ws := t.TempDir()
	m.runtime(ws) // 默认 registry（含 Browser Use）

	m.dispatch(cmd(1, "plugin_list", map[string]any{"workspace": ws}))
	resp, ok := lastResp(t, buf)
	if !ok || resp["ok"] != true {
		t.Fatalf("plugin_list 应 ok: %v", resp)
	}
	list, _ := resp["data"].(map[string]any)["plugins"].([]any)
	var browserCap map[string]any
	for _, p := range list {
		if pm := p.(map[string]any); pm["id"] == "browser" {
			browserCap = pm
		}
	}
	if browserCap == nil {
		t.Fatalf("plugin_list 缺 browser: %v", list)
	}
	// 2026-09：组件内置（vendor 同步）；测试无 vendor → error（组件缺失，reason 说明）
	// 而非旧 not-installed（无手动安装态）。
	if browserCap["name"] != "Browser Use" || browserCap["status"] != "error" {
		t.Errorf("browser 状态错: %+v", browserCap)
	}
	if reason, _ := browserCap["reason"].(string); !strings.Contains(reason, "组件") {
		t.Errorf("browser reason 应说明组件缺失: %+v", browserCap)
	}
	// computer 插件也在默认 registry：卡片数据含 StatusSnapshot 详情（未启用 → 权限 false）
	var computerCap map[string]any
	for _, p := range list {
		if pm := p.(map[string]any); pm["id"] == "computer" {
			computerCap = pm
		}
	}
	if computerCap == nil {
		t.Fatalf("plugin_list 缺 computer: %v", list)
	}
	if computerCap["configurable"] != true {
		t.Errorf("computer 应 configurable（详情设置入口）: %+v", computerCap)
	}
	cd, _ := computerCap["computer"].(map[string]any)
	if cd == nil {
		t.Fatalf("computer 详情缺失: %+v", computerCap)
	}
	if cd["enabled"] != false || cd["accessibility"] != false || cd["screenCapture"] != false {
		t.Errorf("computer 未启用详情应 enabled=false/权限 false: %+v", cd)
	}
	if cd["helperPath"] == "" {
		t.Errorf("computer 详情应带 helperPath: %+v", cd)
	}
}

// TestPluginCommandsDispatch：安装/启用/禁用/卸载/未知 id/缺 workspace 的派发。
func TestPluginCommandsDispatch(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	fake := &fakePluginCap{id: "fakebrowser", tool: fakeBridgeTool{name: "fakebrowser_tool"}}
	injectWS(m, ws, plugin.NewRegistry(fake))

	// 安装（异步：命令立即 ok，后台跑；轮询等委托完成）
	m.dispatch(cmd(1, "plugin_install", map[string]any{"workspace": ws, "id": "fakebrowser"}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != true {
		t.Fatalf("plugin_install 应 ok: %v", resp)
	}
	waitFor(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.installs == 1
	}, "install 委托")
	// 启用 → 工具进引擎缓存
	m.dispatch(cmd(2, "plugin_enable", map[string]any{"workspace": ws, "id": "fakebrowser"}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != true {
		t.Fatalf("plugin_enable 应 ok: %v", resp)
	}
	if !fake.enabled {
		t.Errorf("enable 未委托")
	}
	if got := m.runtime(ws).plugins.EnabledTools(context.Background()); len(got) != 1 {
		t.Errorf("EnabledTools = %d, want 1", len(got))
	}
	// 列表反映 enabled
	m.dispatch(cmd(3, "plugin_list", map[string]any{"workspace": ws}))
	if resp, ok := lastResp(t, buf); ok {
		if list, _ := resp["data"].(map[string]any)["plugins"].([]any); len(list) != 1 ||
			list[0].(map[string]any)["status"] != "enabled" {
			t.Errorf("启用后列表 = %v", resp["data"])
		}
	}
	// 未知 id → 报错
	m.dispatch(cmd(4, "plugin_enable", map[string]any{"workspace": ws, "id": "nope"}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != false {
		t.Fatalf("未知 id 应报错: %v", resp)
	}
	// 禁用 → 工具摘除
	m.dispatch(cmd(5, "plugin_disable", map[string]any{"workspace": ws, "id": "fakebrowser"}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != true {
		t.Fatalf("plugin_disable 应 ok: %v", resp)
	}
	if fake.enabled {
		t.Errorf("disable 未委托")
	}
	if got := m.runtime(ws).plugins.EnabledTools(context.Background()); len(got) != 0 {
		t.Errorf("禁用后 EnabledTools = %d, want 0", len(got))
	}
	// 卸载
	m.dispatch(cmd(6, "plugin_uninstall", map[string]any{"workspace": ws, "id": "fakebrowser"}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != true {
		t.Fatalf("plugin_uninstall 应 ok: %v", resp)
	}
	if fake.uninstalls != 1 {
		t.Errorf("uninstall 未委托: %d", fake.uninstalls)
	}
	// 缺 workspace → 报错
	m.dispatch(cmd(7, "plugin_list", map[string]any{}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != false {
		t.Fatalf("缺 workspace 应报错: %v", resp)
	}
}

// staleEnabledCap 模拟 browser.Plugin 的 State 语义：版本落后（升级窗口）时 State 返回
// update-available、掩盖 enabled——回归「升级被拒：插件已启用，请先禁用再重装」。
type staleEnabledCap struct{ *fakePluginCap }

func (s *staleEnabledCap) State(_ context.Context) plugin.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enabled {
		return plugin.State{Status: plugin.StatusUpdateAvail, Installed: "1.7.0", Pinned: "1.8.0"}
	}
	return plugin.State{Status: plugin.StatusNotInstalled}
}

// IsEnabled 运行时事实（对齐 browser.Plugin.IsEnabled；升级编排据此判定）。
func (s *staleEnabledCap) IsEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled
}

// waitFor 轮询等条件成立（异步操作委托完成等；2s 超时）。
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// TestPluginInstallAsyncEmitsStatus：plugin_install 异步化——命令立即 ok（不阻塞 dispatch），
// 后台跑完后发 plugin_status 事件（UI 刷新 installing → ready/error）。
func TestPluginInstallAsyncEmitsStatus(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	fake := &fakePluginCap{id: "browser"}
	injectWS(m, ws, plugin.NewRegistry(fake))

	m.dispatch(cmd(1, "plugin_install", map[string]any{"workspace": ws, "id": "browser"}))
	// 后台跑完 → plugin_status 事件。必须先于 lastResp 检查：lastResp/readLines 会 drain+Reset
	// buffer，若异步的 plugin_status 与同步的 command_response 落在同一批 buffer 里会被吞掉
	// （-race 调度下更容易撞上）。故先用非破坏性 buf.String() 等 plugin_status 出现；
	// command_response 是 dispatch 内同步写的，plugin_status 出现时它必已在 buffer 中。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "plugin_status") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), "plugin_status") {
		t.Fatalf("install 完成后应发 plugin_status 事件")
	}
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != true {
		t.Fatalf("plugin_install 应立即 ok（异步不阻塞）: %v", resp)
	}
	if fake.installs != 1 {
		t.Errorf("install 未委托: %d", fake.installs)
	}
}

// TestPluginUpgradeReenablesAfterInstall：一键升级编排——已启用的插件安装时先禁用、
// 装完恢复启用（此前启用态直接被拒"请先禁用再重装"，点「升级」闪烁后无反应）。
func TestPluginUpgradeReenablesAfterInstall(t *testing.T) {
	m, _ := testManager(t)
	ws := t.TempDir()
	fake := &fakePluginCap{id: "browser"}
	injectWS(m, ws, plugin.NewRegistry(fake))

	// 先启用（模拟重启后 autoEnablePlugins 恢复的启用态）
	m.dispatch(cmd(1, "plugin_enable", map[string]any{"workspace": ws, "id": "browser"}))
	waitFor(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.enabled
	}, "enable")

	// 升级：不应被「已启用」拒绝，且完成后回到启用态
	m.dispatch(cmd(2, "plugin_install", map[string]any{"workspace": ws, "id": "browser"}))
	waitFor(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.installs == 1 && fake.enabled
	}, "升级后恢复启用")
}

// TestPluginUpgradeDetectsEnabledUnderUpdateAvailable：State 报 update-available（掩盖
// enabled，模拟 1.7.0 已装/内置 1.8.0 的升级窗口）时点升级——编排必须用运行时事实
// IsEnabled 判定「先禁用」，否则 Install 被"插件已启用，请先禁用再重装"拒绝、升级无效。
func TestPluginUpgradeDetectsEnabledUnderUpdateAvailable(t *testing.T) {
	m, _ := testManager(t)
	ws := t.TempDir()
	base := &fakePluginCap{id: "browser"}
	injectWS(m, ws, plugin.NewRegistry(&staleEnabledCap{fakePluginCap: base}))

	// 先启用（此后 State 一直报 update-available）
	m.dispatch(cmd(1, "plugin_enable", map[string]any{"workspace": ws, "id": "browser"}))
	waitFor(t, func() bool {
		base.mu.Lock()
		defer base.mu.Unlock()
		return base.enabled
	}, "enable")

	// 升级：须走 禁用→安装→恢复启用，而非被"已启用"拒绝
	m.dispatch(cmd(2, "plugin_install", map[string]any{"workspace": ws, "id": "browser"}))
	waitFor(t, func() bool {
		base.mu.Lock()
		defer base.mu.Unlock()
		return base.installs == 1 && base.enabled
	}, "升级后恢复启用")
}

// TestPluginUpgradeInstallFailRestores：升级中安装失败 → 恢复启用旧版本组件，
// 且错误随 plugin_status 事件透传（UI 可展示），而非只进 stderr。
func TestPluginUpgradeInstallFailRestores(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	fake := &fakePluginCap{id: "browser", failInstall: true}
	injectWS(m, ws, plugin.NewRegistry(fake))

	m.dispatch(cmd(1, "plugin_enable", map[string]any{"workspace": ws, "id": "browser"}))
	waitFor(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.enabled
	}, "enable")

	m.dispatch(cmd(2, "plugin_install", map[string]any{"workspace": ws, "id": "browser"}))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "注入的安装失败") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), "注入的安装失败") {
		t.Fatalf("安装失败应随 plugin_status 事件透传错误信息")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.enabled {
		t.Errorf("安装失败后应恢复启用（旧版本仍可用）")
	}
}

// TestPluginToolsInEngine：已启用插件工具经 newSessionEngine 注册进引擎（接线验证）。
func TestPluginToolsInEngine(t *testing.T) {
	assertNoUserPrices(t)
	t.Setenv("HOME", t.TempDir()) // 封闭：不读真实 settings.json（否则污染全局 userPriceTable，跨用例串价）
	reg := subagent.NewRegistry()
	bg := func() context.Context { return context.Background() }
	sk, err := skills.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakePluginCap{id: "fakebrowser", tool: fakeBridgeTool{name: "fakebrowser_tool"}}
	plg := plugin.NewRegistry(fake)
	if _, err := plg.Enable(context.Background(), "fakebrowser"); err != nil {
		t.Fatal(err)
	}

	eng, _ := newSessionEngine(t.TempDir(), "", nil, loadProviderConfig(), "deepseek", loadProviderConfig().defaultModel(), string(provider.ReasoningEffortLevelLow), bg, reg, events.NewQuestionWaiter(0), sk, buildSubagentRegistry(t.TempDir()), nil, plg, profiles["code"], nil, nil, nil, nil, nil)
	if _, err := eng.GetTool(context.Background(), "fakebrowser_tool"); err != nil {
		t.Fatalf("插件工具未注册: %v", err)
	}
	// 未启用的插件工具不注册
	eng2, _ := newSessionEngine(t.TempDir(), "", nil, loadProviderConfig(), "deepseek", loadProviderConfig().defaultModel(), string(provider.ReasoningEffortLevelLow), bg, reg, events.NewQuestionWaiter(0), sk, buildSubagentRegistry(t.TempDir()), nil, plugin.NewRegistry(&fakePluginCap{id: "off"}), profiles["code"], nil, nil, nil, nil, nil)
	if _, err := eng2.GetTool(context.Background(), "off_tool"); err == nil {
		t.Fatalf("未启用插件工具不应注册")
	}
}

// TestAutoEnablePlugins：会话创建自动恢复「上次已启用」的插件（持久化标记）。
// WasEnabled=false 的插件不自动启用；用户显式禁用后不恢复；每工作区只跑一次。
func TestAutoEnablePlugins(t *testing.T) {
	m, _ := testManager(t)
	ws := t.TempDir()

	enabled := &fakePluginCap{id: "was-on", tool: fakeBridgeTool{name: "was_on_tool"}, wasEnabled: true}
	never := &fakePluginCap{id: "never", tool: fakeBridgeTool{name: "never_tool"}}
	m.workspaces[ws] = &workspaceRuntime{path: ws, plugins: plugin.NewRegistry(enabled, never), sessions: map[string]*bridgeSession{}}

	// §启动优化：autoEnablePlugins 异步化——调用立即返回，Enable 在后台完成，需等待。
	m.autoEnablePlugins(m.runtime(ws))
	waitFor(t, func() bool { enabled.mu.Lock(); defer enabled.mu.Unlock(); return enabled.enabled }, "异步自动启用（was-on）")
	if never.enabled {
		t.Fatalf("WasEnabled=false 的插件不应自动启用")
	}
	// 幂等：再次调用不重复 Enable（pluginsAuto 已置位，后台任务不会重跑）
	m.autoEnablePlugins(m.runtime(ws))
	// 用户显式禁用后不再恢复
	_ = enabled.Disable(context.Background())
	m.autoEnablePlugins(m.runtime(ws))
	waitFor(t, func() bool { enabled.mu.Lock(); defer enabled.mu.Unlock(); return !enabled.enabled }, "禁用后不恢复")
}
