package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/session"
	"github.com/seven7628/hai-harness/skills"
	"github.com/seven7628/hai-harness/subagent"
)

// TestMeshToolsRegisteredWhenEnabled 验收 #11：mesh.session_enabled=true → newSessionEngine 注册
// session_send/session_list；false → 不注册（零侵入）。
func TestMeshToolsRegisteredWhenEnabled(t *testing.T) {
	assertNoUserPrices(t)
	home := t.TempDir()
	dir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 开启 mesh（本机互聊，不开远程）
	cfg := `{"mesh":{"session_enabled":true,"remote_enabled":false}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	hub := session.NewHub("node-test")
	eng, _ := newSessionEngine(t.TempDir(), "s1", nil, loadProviderConfig(), "deepseek",
		loadProviderConfig().defaultModel(), string(providerReasoningEffortLow()),
		func() context.Context { return context.Background() }, subagent.NewRegistry(),
		events.NewQuestionWaiter(0), mustSkillsReg(t), buildSubagentRegistry(t.TempDir()),
		nil, nil, profiles["code"], nil, hub, nil, nil, nil)
	for _, name := range []string{"session_send", "session_list"} {
		if _, err := eng.GetTool(context.Background(), name); err != nil {
			t.Fatalf("mesh enabled: 工具 %s 未注册: %v", name, err)
		}
	}
	// 常规工具不受影响
	if _, err := eng.GetTool(context.Background(), "read_file"); err != nil {
		t.Fatalf("read_file should still be registered: %v", err)
	}
}

// TestMeshToolsNotRegisteredWhenDisabled mesh 未开启 → session_* 工具不存在（默认关闭）。
func TestMeshToolsNotRegisteredWhenDisabled(t *testing.T) {
	assertNoUserPrices(t)
	t.Setenv("HOME", t.TempDir()) // 无 settings.json → 默认全关
	hub := session.NewHub("node-test")
	eng, _ := newSessionEngine(t.TempDir(), "s1", nil, loadProviderConfig(), "deepseek",
		loadProviderConfig().defaultModel(), string(providerReasoningEffortLow()),
		func() context.Context { return context.Background() }, subagent.NewRegistry(),
		events.NewQuestionWaiter(0), mustSkillsReg(t), buildSubagentRegistry(t.TempDir()),
		nil, nil, profiles["code"], nil, hub, nil, nil, nil)
	for _, name := range []string{"session_send", "session_list"} {
		if _, err := eng.GetTool(context.Background(), name); err == nil {
			t.Fatalf("mesh disabled: %s 不应注册", name)
		}
	}
}

// TestApplyMeshSettingsTogglesGate applyMeshSettings 幂等 + 开关翻转 Hub 门。
func TestApplyMeshSettingsTogglesGate(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 默认全关
	m := &manager{hub: session.NewHub("node-test")}
	m.applyMeshSettings()
	if m.hub.ToolsEnabled() {
		t.Fatal("tools should be disabled by default")
	}
	if m.hub.RemoteEnabled() {
		t.Fatal("remote should be disabled by default")
	}
	// 幂等：重复调用不 panic
	m.applyMeshSettings()
}

// mustSkillsReg 空技能注册表 helper（对齐 TestSessionEngineRegistersAllTools 模式）。
func mustSkillsReg(t *testing.T) *skills.Registry {
	t.Helper()
	sk, err := skills.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("skills registry: %v", err)
	}
	return sk
}

// providerReasoningEffortLow 返回低推理档（避免引入 provider 包 import 冲突）。
func providerReasoningEffortLow() string { return "low" }

// TestApplyMeshSettingsStartupSelfHeal 回归（启动自愈）：settings.json 已持久化
// remote_enabled=true + peer → serve() 等价调用（构造 manager 后立即 applyMeshSettings）
// 应启动网关并拨号 peer；修复前网关启动依赖前端 reload_settings，bridge 重启后静默停。
func TestApplyMeshSettingsStartupSelfHeal(t *testing.T) {
	home := t.TempDir()
	// 开启 session+remote，指向一个真实会监听的 B 节点地址（占位：不监听则拨号失败，
	// 但网关本身必须启动 = RemoteEnabled true + meshGW 非 nil）
	writeMeshSettings(t, home, `{"session_enabled":true,"remote_enabled":true,"remote":{"listen_addr":"127.0.0.1:0","auth_token":"tok","node_id":"node-a","peers":[]}}`)
	hub := session.NewHub("node-a")
	m := &manager{hub: hub, out: newOut(io.Discard)}
	m.applyMeshSettings()
	if !m.hub.ToolsEnabled() {
		t.Fatal("tools should be enabled")
	}
	if m.meshGW == nil {
		t.Fatal("gateway should be started on applyMeshSettings (startup self-heal)")
	}
	if !m.hub.RemoteEnabled() {
		t.Fatal("remote should be enabled")
	}
	m.meshGW.Stop()
	m.meshGW = nil
}

// TestApplyMeshSettingsPeerChangeRestartsGateway 回归（保存节点热生效）：网关在跑时
// peers 增删 → applyMeshSettings 应 Stop 旧网关并按新配置重建（旧网关不会拨号新 peer）。
func TestApplyMeshSettingsPeerChangeRestartsGateway(t *testing.T) {
	home := t.TempDir()
	writeMeshSettings(t, home, `{"session_enabled":true,"remote_enabled":true,"remote":{"listen_addr":"127.0.0.1:0","auth_token":"tok","node_id":"node-a","peers":[]}}`)
	hub := session.NewHub("node-a")
	m := &manager{hub: hub, out: newOut(io.Discard)}
	m.applyMeshSettings()
	oldGW := m.meshGW
	if oldGW == nil {
		t.Fatal("gateway should be running")
	}
	// peers 变化（保存节点）→ 热重启
	writeMeshSettings(t, home, `{"session_enabled":true,"remote_enabled":true,"remote":{"listen_addr":"127.0.0.1:0","auth_token":"tok","node_id":"node-a","peers":[{"id":"node-b","addr":"ws://127.0.0.1:1","token":"tok"}]}}`)
	m.applyMeshSettings()
	if m.meshGW == nil || m.meshGW == oldGW {
		t.Fatalf("gateway should be restarted with new peers (old=%p new=%p)", oldGW, m.meshGW)
	}
	// 新网关能报告 peer 拨号状态（哪怕连不上——状态机要能记录失败）
	waitForBridge(t, func() bool {
		sts := m.meshGW.PeerStatuses(context.Background())
		return len(sts) == 1
	}, 5*time.Second)
}

// writeMeshSettings 写 mesh 段 settings.json 到指定 HOME（~/.go-code/settings.json）。
func writeMeshSettings(t *testing.T, home, meshJSON string) {
	t.Helper()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(`{"mesh":`+meshJSON+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

// waitForBridge 轮询条件（bridge 测试用，不 t.Fatal 直到超时）。
func waitForBridge(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// TestSessionTitlesStore 回归：titles store 幂等 + 跨实例持久（AI 标题写入后不重复）。
func TestSessionTitlesStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	wsKey := "ws-key-test"
	// 首次写入
	if !sessionTitles.set(wsKey, "s1", "修 Bug") {
		t.Fatal("first set should succeed")
	}
	// 幂等：已存在不覆盖
	if sessionTitles.set(wsKey, "s1", "另一个") {
		t.Fatal("existing title should not be overwritten")
	}
	if got := sessionTitles.get(wsKey, "s1"); got != "修 Bug" {
		t.Fatalf("get mismatch: %s", got)
	}
	// 空标题拒绝
	if sessionTitles.set(wsKey, "s2", "  ") {
		t.Fatal("blank title should be rejected")
	}
}

// TestListSessionsNewChatDefault 回归：新建（无任何 user 消息）会话 list title = New Chat
// （此前回退 sid 长串；需求：新会话默认 New Chat，首次输入后 AI 生成替换）。
func TestListSessionsNewChatDefault(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	sid := newSessionID(t, m, buf, ws) // 新建会话（无消息）
	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session missing")
	}
	if got := resolveLiveSessionTitle(workspaceKey(ws), bs, sid); got != defaultSessionTitle {
		t.Fatalf("fresh session should show %q, got %q", defaultSessionTitle, got)
	}
}

// TestNormalizeGeneratedTitle 标题清洗：≤15 字符 + 去引号 + 去换行。
func TestNormalizeGeneratedTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"修数据库连接池的 Bug"`, "修数据库连接池的 Bug"},
		{"「优化登录流程」", "优化登录流程"},
		{"Fix the\nflaky test", "Fix the flaky t"}, // 折叠空白后截断到 15 字符
		{"  调查用户反馈的问题  ", "调查用户反馈的问题"},
		{`'短'`, "短"},
	}
	for _, c := range cases {
		if got := normalizeGeneratedTitle(c.in); got != c.want {
			t.Fatalf("normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// 长标题截断到 15 runes（中文按字符计）
	long := normalizeGeneratedTitle("这是一个非常非常长的中文会话标题需要被截断")
	if r := []rune(long); len(r) > maxGeneratedTitleRunes {
		t.Fatalf("title too long: %d runes: %q", len(r), long)
	}
}

// TestMaybeGenSessionTitleOnce 回归（单次生成）：已生成/已有 user 历史 → 不触发；
// in-flight 并发去重；cron 会话不触发。
func TestMaybeGenSessionTitleOnce(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	sid := newSessionID(t, m, buf, ws)
	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("missing")
	}
	// 首次输入 → 应触发 in-flight（不阻塞；因无真实 provider 会失败降级）
	m.maybeGenSessionTitle(ws, bs, "你好")
	m.titleGenMu.Lock()
	inFlight := m.titleInFlight[sid]
	m.titleGenMu.Unlock()
	if !inFlight {
		t.Fatal("first input should start generation")
	}
	// 等待异步任务结束（LLM 失败静默，in-flight 清除）
	waitForBridge(t, func() bool {
		m.titleGenMu.Lock()
		defer m.titleGenMu.Unlock()
		return !m.titleInFlight[sid]
	}, 5*time.Second)
	// 结束（失败）后标题未写入；但历史已有 user 文本（Ask 已入 history？——未 Run，
	// Messages 无 user → 仍可能再次触发。验证：模拟已生成场景不再触发）
	// 预置标题后再次输入不应触发
	sessionTitles.set(workspaceKey(ws), sid, "已有标题")
	m.maybeGenSessionTitle(ws, bs, "第二条")
	m.titleGenMu.Lock()
	again := m.titleInFlight[sid]
	m.titleGenMu.Unlock()
	if again {
		t.Fatal("session with existing title should not regenerate")
	}
}

// TestBroadcastSessionTitleEvent 标题生成完成的事件行：session_title 携带
// workspace/session_id/title（前端左栏据此就地更新）；meshGW nil 时安全 no-op。
func TestBroadcastSessionTitleEvent(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t) // meshGW nil
	m.broadcastSessionTitle(ws, "s1", "修数据库 Bug")
	got := findFirst(buf.String(), func(mm map[string]any) bool { return mm["event_type"] == "session_title" })
	if got == nil {
		t.Fatal("session_title event missing")
	}
	if got["session_id"] != "s1" || got["title"] != "修数据库 Bug" || got["workspace"] != ws {
		t.Fatalf("session_title payload mismatch: %+v", got)
	}
}
