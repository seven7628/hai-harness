package main

// IM 的归属（owner）隔离回归（BR-21 产品决策）：连接共享，但**启用/路由/管理面**按归属隔离。
//
// 核心负例：A 群关闭 IM 只停 A 群的投递，B 智能体照常收发；plugin_list 不能把
// 「A 群启用」显示成「B 群也启用」。

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/im"
	"github.com/seven7628/hai-harness/owner"
	implugin "github.com/seven7628/hai-harness/plugin/im"
)

// —— IM ——

// imTestBridge 构造一个只做路由/闸门的最小 IM 桥（无真网关：Gateways 为空表）。
func imTestBridge(t *testing.T, cfg im.Config) (*imBridge, *[]imTarget) {
	t.Helper()
	rt := &implugin.Runtime{
		Router:   im.NewRouter(cfg),
		Security: im.NewSecurity(cfg, filepath.Join(t.TempDir(), "audit.jsonl")),
	}
	rt.SetConfig(cfg)
	b := newIMBridge(rt)
	asked := []imTarget{}
	b.askFn = func(target imTarget, text string, images []core.Content, _ string) { asked = append(asked, target) }
	return b, &asked
}

func TestIMOwnerGateBlocksOnlyThatOwner(t *testing.T) {
	// 两条路由：一个 agent 归属（关），一个 workspace 归属（开）。
	cfg := im.Config{
		Enabled: true,
		Gateways: []im.GatewayConfig{{
			ID: "gw1", Type: "feishu", Enabled: true, AllowUsers: []string{"*"},
			Routes: []im.RouteConfig{
				{Chat: im.Chat{Gateway: "feishu", ChatID: "chat-agent"}, Agent: "a"},
				{Chat: im.Chat{Gateway: "feishu", ChatID: "chat-ws"}, Workspace: "/proj/x"},
			},
		}},
	}
	off := false
	if err := cfg.SetOwnerEnabled(owner.Agent("a"), &off); err != nil {
		t.Fatal(err)
	}
	b, asked := imTestBridge(t, cfg)

	b.OnInMessage(im.InMessage{Chat: im.Chat{Gateway: "feishu", ChatID: "chat-agent"}, UserID: "u1", Text: "hi"})
	if len(*asked) != 0 {
		t.Fatalf("agent:a 已关闭 → 不得投递，得到 %+v", *asked)
	}
	b.OnInMessage(im.InMessage{Chat: im.Chat{Gateway: "feishu", ChatID: "chat-ws"}, UserID: "u1", Text: "hi"})
	if len(*asked) != 1 {
		t.Fatalf("workspace 归属仍启用 → 应投递，得到 %+v", *asked)
	}
	if got := (*asked)[0].Owner; got != owner.Workspace("/proj/x") {
		t.Fatalf("投递归属 = %q", got)
	}
	if (*asked)[0].SessionID != "im-feishu-chat-ws" {
		t.Fatalf("项目会话 id = %q", (*asked)[0].SessionID)
	}
}

func TestIMPassesExternalSenderName(t *testing.T) {
	cfg := im.Config{
		Enabled: true,
		Gateways: []im.GatewayConfig{{
			ID: "gw1", Type: "feishu", Enabled: true, AllowUsers: []string{"*"},
			Routes: []im.RouteConfig{{Chat: im.Chat{Gateway: "feishu", ChatID: "dm"}, Agent: "a"}},
		}},
	}
	b, _ := imTestBridge(t, cfg)
	var got string
	b.askFn = func(_ imTarget, _ string, _ []core.Content, senderName string) { got = senderName }
	b.OnInMessage(im.InMessage{Chat: im.Chat{Gateway: "feishu", ChatID: "dm"}, UserID: "u", UserName: "飞书张三", Text: "x"})
	if got != "飞书张三" {
		t.Fatalf("senderName = %q, want 飞书张三", got)
	}
}

func TestIMOwnerTargetsRouteSessions(t *testing.T) {
	cfg := im.Config{
		Enabled: true,
		Gateways: []im.GatewayConfig{{
			ID: "gw1", Type: "feishu", Enabled: true, AllowUsers: []string{"*"},
			Routes: []im.RouteConfig{
				{Chat: im.Chat{Gateway: "feishu", ChatID: "dm"}, Agent: "a"},
				{Chat: im.Chat{Gateway: "feishu", ChatID: "grp"}, Agent: "a", Group: "g1"},
			},
		}},
	}
	b, asked := imTestBridge(t, cfg)
	b.OnInMessage(im.InMessage{Chat: im.Chat{Gateway: "feishu", ChatID: "dm"}, UserID: "u", Text: "x"})
	b.OnInMessage(im.InMessage{Chat: im.Chat{Gateway: "feishu", ChatID: "grp"}, UserID: "u", Text: "y"})
	if len(*asked) != 2 {
		t.Fatalf("两条路由都应投递，得到 %+v", *asked)
	}
	if (*asked)[0].SessionID != "hub.a.dm" || (*asked)[0].Owner != owner.Agent("a") {
		t.Fatalf("DM 目标: %+v", (*asked)[0])
	}
	if (*asked)[1].SessionID != "hub.a.g.g1" || (*asked)[1].Owner != owner.Group("g1") {
		t.Fatalf("群目标: %+v", (*asked)[1])
	}
}

// TestIMMigrationKeepsMessagesFlowing 旧配置（无 owners 段、路由只有 workspace）：
// 升级后必须照常投递（默认启用），并在首次保存时补出显式 owner 状态。
func TestIMMigrationKeepsMessagesFlowing(t *testing.T) {
	cfg := im.Config{
		Enabled: true,
		Gateways: []im.GatewayConfig{{
			ID: "gw1", Type: "feishu", Enabled: true, AllowUsers: []string{"*"},
			Routes: []im.RouteConfig{{Chat: im.Chat{Gateway: "feishu", ChatID: "old"}, Workspace: "/proj/old"}},
		}},
	}
	if cfg.Owners != nil {
		t.Fatal("前置：旧配置没有 owners 段")
	}
	if !cfg.OwnerEnabled(owner.Workspace("/proj/old")) {
		t.Fatal("有路由但未登记的归属：迁移默认必须启用（不能升级后静默丢消息）")
	}
	// 未登记归属 + 无路由 → 不启用（不猜）。
	if cfg.OwnerEnabled(owner.Agent("ghost")) {
		t.Fatal("没有路由也没有登记 → 不应启用")
	}
	n := cfg.MigrateOwners()
	if n != 1 {
		t.Fatalf("迁移应补 1 条显式状态，得到 %d", n)
	}
	if st, ok := cfg.Owners[owner.Workspace("/proj/old")]; !ok || st.Enabled == nil || !*st.Enabled {
		t.Fatalf("迁移后应有显式 enabled=true: %+v", cfg.Owners)
	}
	// 再跑一次幂等
	if n2 := cfg.MigrateOwners(); n2 != 0 {
		t.Fatalf("迁移必须幂等，第二次补了 %d 条", n2)
	}
}

// TestIMRouteOwnerConsistency 显式 owner 与投递目标必须一致（防「关 A 停 B」）。
func TestIMRouteOwnerConsistency(t *testing.T) {
	base := im.Config{Enabled: true, Gateways: []im.GatewayConfig{{
		ID: "gw1", Type: "feishu", Enabled: false, AllowUsers: []string{"*"},
	}}}
	bad := base
	bad.Gateways = append([]im.GatewayConfig(nil), base.Gateways...)
	bad.Gateways[0].Routes = []im.RouteConfig{{Chat: im.Chat{Gateway: "feishu", ChatID: "c"}, Workspace: "/p", Owner: owner.Agent("a")}}
	if _, err := bad.Validate(); err == nil {
		t.Fatal("owner 与目标不一致必须被拒绝")
	}
	ok := base
	ok.Gateways = append([]im.GatewayConfig(nil), base.Gateways...)
	ok.Gateways[0].Routes = []im.RouteConfig{{Chat: im.Chat{Gateway: "feishu", ChatID: "c"}, Agent: "a"}}
	if _, err := ok.Validate(); err != nil {
		t.Fatalf("agent 路由应合法: %v", err)
	}
	group := base
	group.Gateways = append([]im.GatewayConfig(nil), base.Gateways...)
	group.Gateways[0].Routes = []im.RouteConfig{{Chat: im.Chat{Gateway: "feishu", ChatID: "c"}, Group: "g", Agent: "a"}}
	if _, err := group.Validate(); err != nil {
		t.Fatalf("群路由（带执行者）应合法: %v", err)
	}
	groupOnly := base
	groupOnly.Gateways = append([]im.GatewayConfig(nil), base.Gateways...)
	groupOnly.Gateways[0].Routes = []im.RouteConfig{{Chat: im.Chat{Gateway: "feishu", ChatID: "c"}, Group: "g"}}
	if _, err := groupOnly.Validate(); err == nil {
		t.Fatal("群路由缺 agent 必须被拒绝（不知道谁执行）")
	}
}

// —— plugin_list / plugin_enable / plugin_disable 的归属表达 ——

// newIMPluginForTest 造一个真实（可落盘）的 IM 插件实例，供管理面测试用。
func newIMPluginForTest(t *testing.T, cfg im.Config) (*implugin.Plugin, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "im-config.json")
	if err := im.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	p := implugin.New(path)
	p.LoadConfig = im.LoadConfig
	return p, path
}

// TestPluginListExpressesIMOwner plugin_list 必须按**当前归属**表达 IM 的启停：
// A 群启用不能显示成 B 群也启用（连接共享 ≠ 状态共享）。
func TestPluginListExpressesIMOwner(t *testing.T) {
	m, buf := testManager(t)
	cfg := im.Config{
		Enabled: true,
		Gateways: []im.GatewayConfig{{
			ID: "gw1", Type: "feishu", Enabled: false, AllowUsers: []string{"*"},
			Routes: []im.RouteConfig{
				{Chat: im.Chat{Gateway: "feishu", ChatID: "a"}, Agent: "a"},
				{Chat: im.Chat{Gateway: "feishu", ChatID: "b"}, Group: "g1", Agent: "a"},
			},
		}},
	}
	off := false
	if err := cfg.SetOwnerEnabled(owner.Group("g1"), &off); err != nil {
		t.Fatal(err)
	}
	p, _ := newIMPluginForTest(t, cfg)
	m.imPlugin = p
	wsPath := t.TempDir()
	_ = m.runtime(wsPath)

	imEntry := func(ownerID string) map[string]any {
		ok, errMsg, data := runCmd(t, m, buf, CmdPluginList, map[string]any{"workspace": wsPath, "owner": ownerID})
		if !ok {
			t.Fatalf("plugin_list(%s) 失败: %s", ownerID, errMsg)
		}
		if data["owner"] != ownerID {
			t.Fatalf("响应归属 = %v, want %s", data["owner"], ownerID)
		}
		caps, _ := data["plugins"].([]any)
		for _, c := range caps {
			cm, _ := c.(map[string]any)
			if cm["id"] == implugin.ID {
				return cm
			}
		}
		t.Fatalf("plugin_list 里没有 IM 条目: %#v", data["plugins"])
		return nil
	}

	a := imEntry(owner.Agent("a"))
	if a["ownerEnabled"] != true {
		t.Fatalf("agent:a 有路由且未显式关闭 → 应 enabled，得到 %#v", a)
	}
	if a["owner"] != owner.Agent("a") {
		t.Fatalf("条目归属 = %v", a["owner"])
	}
	if a["enabled"] != true || a["status"] != "enabled" {
		t.Fatalf("启用态字段不对: enabled=%v status=%v", a["enabled"], a["status"])
	}

	g := imEntry(owner.Group("g1"))
	if g["ownerEnabled"] != false {
		t.Fatalf("group:g1 已显式关闭 → 应 disabled，得到 %#v", g)
	}
	if g["enabled"] != false || g["status"] != "ready" {
		t.Fatalf("关闭态：enabled 应 false、status=ready，得到 enabled=%v status=%v", g["enabled"], g["status"])
	}
	if a["enabled"] == g["enabled"] {
		t.Fatal("A（agent）与 B（group）的启用状态必须可区分")
	}
}

// TestPluginEnableDisablePerOwner plugin_enable/disable 只改**自己的归属**，
// 且缺少归属时拒绝（不把启停写成全局）。
func TestPluginEnableDisablePerOwner(t *testing.T) {
	m, buf := testManager(t)
	cfg := im.Config{Enabled: true, Gateways: []im.GatewayConfig{{
		ID: "gw1", Type: "feishu", Enabled: false, AllowUsers: []string{"*"},
		Routes: []im.RouteConfig{{Chat: im.Chat{Gateway: "feishu", ChatID: "a"}, Agent: "a"}},
	}}}
	p, path := newIMPluginForTest(t, cfg)
	m.imPlugin = p
	wsPath := t.TempDir()
	_ = m.runtime(wsPath)

	// 显式给了**非法**归属 → 拒绝（不静默回落成 workspace）。
	if ok, _, _ := runCmd(t, m, buf, CmdPluginEnable, map[string]any{
		"workspace": wsPath, "id": implugin.ID, "owner": "global",
	}); ok {
		t.Fatal("非法 owner 必须拒绝")
	}
	// 只给 workspace（无 owner/session）：按 workspace:<path> 归属启用 —— 这是**项目会话**
	// 的合法归属，不是全局（产品口径：普通会话按 workspace）。断言它没被写进其它归属。
	if ok, errMsg, _ := runCmd(t, m, buf, CmdPluginEnable, map[string]any{
		"workspace": wsPath, "id": implugin.ID,
	}); !ok {
		t.Fatalf("workspace 归属应可启用: %s", errMsg)
	}
	fromWS, _ := im.LoadConfig(path)
	if !fromWS.OwnerEnabled(owner.Workspace(wsPath)) {
		t.Fatal("workspace 归属应已启用")
	}
	if _, exists := fromWS.Owners[owner.Agent("a")]; exists {
		t.Fatal("workspace 归属的启用不得写出 agent:a 的条目（不扩散）")
	}

	// 启用 agent:a
	if ok, errMsg, _ := runCmd(t, m, buf, CmdPluginEnable, map[string]any{
		"workspace": wsPath, "id": implugin.ID, "owner": owner.Agent("a"),
	}); !ok {
		t.Fatalf("启用失败: %s", errMsg)
	}
	saved, err := im.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.OwnerEnabled(owner.Agent("a")) {
		t.Fatal("落盘后 agent:a 应为启用")
	}
	// 关闭 group:g9（另一个归属）不得影响 agent:a
	if ok, errMsg, _ := runCmd(t, m, buf, CmdPluginDisable, map[string]any{
		"workspace": wsPath, "id": implugin.ID, "owner": owner.Group("g9"),
	}); !ok {
		t.Fatalf("关闭其它归属失败: %s", errMsg)
	}
	saved2, _ := im.LoadConfig(path)
	if !saved2.OwnerEnabled(owner.Agent("a")) {
		t.Fatal("关闭 group:g9 不得影响 agent:a（owner 隔离）")
	}
	if saved2.OwnerEnabled(owner.Group("g9")) {
		t.Fatal("group:g9 应已关闭")
	}
}

// TestIMConfigSetKeepsOwnerLedger im_config_set 是「设置页保存整份配置」的入口，
// 而 owners 账本不在编辑表单里。判「没携带」必须按 len==0，否则 `owners: {}`
// 会把别人刚设的显式 false 清空（升级后/多窗口下表现为「关掉的归属又自己开了」）。
func TestIMConfigSetKeepsOwnerLedger(t *testing.T) {
	m, buf := testManager(t)
	base := im.Config{Enabled: true, Gateways: []im.GatewayConfig{{
		ID: "gw1", Type: "feishu", Enabled: false, AllowUsers: []string{"*"},
		Routes: []im.RouteConfig{{Chat: im.Chat{Gateway: "feishu", ChatID: "c1"}, Agent: "a"}},
	}}}
	off := false
	base.Owners = map[string]im.OwnerState{owner.Agent("a"): {Enabled: &off}}
	if err := im.SaveConfig(m.imConfigPath(), base); err != nil {
		t.Fatal(err)
	}
	p, _ := newIMPluginForTest(t, base)
	m.imPlugin = p
	wsPath := t.TempDir()

	// 表单序列化出的 config：没有 owners 字段
	raw, _ := json.Marshal(im.Config{Enabled: true, Gateways: base.Gateways})
	var formCfg map[string]any
	_ = json.Unmarshal(raw, &formCfg)

	if ok, errMsg, _ := runCmd(t, m, buf, "im_config_set", map[string]any{
		"workspace": wsPath, "config": formCfg,
	}); !ok {
		t.Fatalf("保存配置失败: %s", errMsg)
	}
	saved, err := im.LoadConfig(m.imConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if st, exists := saved.Owners[owner.Agent("a")]; !exists || st.Enabled == nil || *st.Enabled {
		t.Fatalf("无 owners 字段的保存不得清掉显式 false: %+v", saved.Owners)
	}

	// 显式空对象（前端可能序列化成 {}）同样视为「未携带」
	formCfg["owners"] = map[string]any{}
	if ok, errMsg, _ := runCmd(t, m, buf, "im_config_set", map[string]any{
		"workspace": wsPath, "config": formCfg,
	}); !ok {
		t.Fatalf("保存配置失败: %s", errMsg)
	}
	saved2, _ := im.LoadConfig(m.imConfigPath())
	if st, exists := saved2.Owners[owner.Agent("a")]; !exists || st.Enabled == nil || *st.Enabled {
		t.Fatalf("owners:{} 的保存不得清掉显式 false: %+v", saved2.Owners)
	}
}

// TestIMBindConfirmMigratesOwners 绑定确认新写一条项目路由时必须同时补归属账本：
// 否则新路由的 owner 是「未登记」状态 —— 开启的配置里它靠有路由即启用的迁移默认还能跑，
// 但只要用户在任何地方关过一个人，后续行为就取决于「谁先写」，不可解释。
func TestIMBindConfirmMigratesOwners(t *testing.T) {
	m, buf := testManager(t)
	base := im.Config{Enabled: true, Gateways: []im.GatewayConfig{{
		ID: "gw1", Type: "feishu", Enabled: false, AllowUsers: []string{"*"},
		Routes: []im.RouteConfig{{Chat: im.Chat{Gateway: "feishu", ChatID: "old"}, Workspace: "/proj/old"}},
	}}}
	if base.Owners != nil {
		t.Fatal("前置：旧配置没有 owners 段")
	}
	if err := im.SaveConfig(m.imConfigPath(), base); err != nil {
		t.Fatal(err)
	}
	p, _ := newIMPluginForTest(t, base)
	m.imPlugin = p
	wsPath := t.TempDir()

	if ok, errMsg, _ := runCmd(t, m, buf, "im_bind_confirm", map[string]any{
		"workspace": wsPath, "gateway_id": "gw1",
		"chat": map[string]any{"gateway": "feishu", "chat_id": "new"}, "approve": true,
	}); !ok {
		t.Fatalf("绑定确认失败: %s", errMsg)
	}
	saved, err := im.LoadConfig(m.imConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, own := range []string{owner.Workspace("/proj/old"), owner.Workspace(wsPath)} {
		st, exists := saved.Owners[own]
		if !exists || st.Enabled == nil || !*st.Enabled {
			t.Fatalf("绑定确认后 %s 应有显式 enabled=true（升级不静默丢消息）: %+v", own, saved.Owners)
		}
	}
}

// TestIMRuntimeConfigConcurrentAccess Runtime 的配置与连接表被两个 goroutine 同时用：
// Gateway 回调（OnInMessage 里的归属闸门/镜像表）与命令循环（applyIMConfig/
// SetOwnerEnabled 整体替换配置）。这组用例在 -race 下必须干净 —— 未加锁时
// 「读 cfg.OwnerEnabled + 写 cfg.Owners」会被捕获成 data race（UB，线上偶发串数据）。
func TestIMRuntimeConfigConcurrentAccess(t *testing.T) {
	cfg := im.Config{
		Enabled: true,
		Gateways: []im.GatewayConfig{{
			ID: "gw1", Type: "feishu", Enabled: false, AllowUsers: []string{"*"},
			Routes: []im.RouteConfig{{Chat: im.Chat{Gateway: "feishu", ChatID: "c"}, Agent: "a"}},
		}},
	}
	rt := &implugin.Runtime{Router: im.NewRouter(cfg), Security: im.NewSecurity(cfg, filepath.Join(t.TempDir(), "audit.jsonl"))}
	rt.SetConfig(cfg)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// 读者：模拟 Gateway 回调里的归属判定与路由查询
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				c := rt.ConfigSnapshot()
				_ = c.OwnerEnabled(owner.Agent("a"))
				for _, g := range c.Gateways {
					_ = len(g.Routes)
				}
				_ = rt.GatewaySnapshot()
			}
		}
	}()
	// 写者：模拟 applyIMConfig / SetOwnerEnabled 的「改副本 → 整体发布」
	for i := 0; i < 200; i++ {
		c := rt.ConfigSnapshot()
		on := i%2 == 0
		if c.Owners == nil {
			c.Owners = map[string]im.OwnerState{}
		}
		c.Owners[owner.Agent("a")] = im.OwnerState{Enabled: &on}
		c.Mirrors = append(c.Mirrors, im.MirrorConfig{SessionID: "s", Chat: im.Chat{Gateway: "feishu", ChatID: "c"}})
		rt.SetConfig(c)
		rt.PutGateway("gw1", nil) // 连接表同锁：与 GatewaySnapshot 并发
	}
	close(stop)
	wg.Wait()
}
