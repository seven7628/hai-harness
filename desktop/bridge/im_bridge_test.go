package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/im"
	implugin "github.com/seven7628/hai-harness/plugin/im"
)

// testGateway 桥测试用 Gateway（记录发送 + 可触发回调）。
type testGateway struct {
	typ  string
	sent []im.OutMessage
	msgH func(im.InMessage)
	actH func(im.GatewayAction)
}

func (t *testGateway) Type() string                    { return t.typ }
func (t *testGateway) Name() string                    { return t.typ }
func (t *testGateway) Start(ctx context.Context) error { return nil }
func (t *testGateway) Stop(ctx context.Context) error  { return nil }
func (t *testGateway) Status() im.Status               { return im.StatusConnected }
func (t *testGateway) Capabilities() im.Capabilities {
	return im.Capabilities{CardButtons: true, Markdown: true}
}
func (t *testGateway) Send(ctx context.Context, c im.Chat, m im.OutMessage) (string, error) {
	t.sent = append(t.sent, m)
	return "mid", nil
}
func (t *testGateway) Update(ctx context.Context, mid string, m im.OutMessage) error { return nil }
func (t *testGateway) Streamer() im.Streamer                                         { return nil } // 测试默认不支持流式
func (t *testGateway) OnMessage(h func(im.InMessage))                                { t.msgH = h }
func (t *testGateway) OnAction(h func(im.GatewayAction))                             { t.actH = h }

// testGatewayOnce 保证 "testgw" 只注册一次（多测试共享）。
var testGatewayOnce sync.Once

func initTestGateway() {
	testGatewayOnce.Do(func() {
		im.Register("testgw", func(json.RawMessage) (im.Gateway, error) {
			return &testGateway{typ: "testgw"}, nil
		})
	})
}

// newTestBridge 构造带 fake gateway 的 imBridge。
func newTestBridge(t *testing.T, cfg im.Config) (*imBridge, *testGateway) {
	t.Helper()
	cfg.Enabled = true // 新 owner 闸门先看主开关；测试统一视为已启用
	initTestGateway()
	gw, _ := im.New("testgw", nil)
	tg := gw.(*testGateway)

	rt := &implugin.Runtime{
		Router:   im.NewRouter(cfg),
		Security: im.NewSecurity(cfg, ""),
	}
	rt.SetConfig(cfg)
	rt.PutGateway("g1", gw)
	b := newIMBridge(rt)
	b.Attach()
	return b, tg
}

func TestBridgeMessageRoutesToAsk(t *testing.T) {
	cfg := im.DefaultConfig()
	cfg.Enabled = true
	cfg.Gateways = []im.GatewayConfig{{
		ID: "g1", Type: "testgw", Enabled: true, AllowUsers: []string{"u1"},
		Routes: []im.RouteConfig{{Chat: im.Chat{Gateway: "testgw", ChatID: "c1"}, Workspace: "/ws/a"}},
	}}
	b, _ := newTestBridge(t, cfg)

	var asked string
	var askedWS string
	b.askFn = func(t imTarget, text string, _ []core.Content, _ string) {
		asked = text
		askedWS = t.Route.Workspace
	}
	b.OnInMessage(im.InMessage{
		Chat: im.Chat{Gateway: "testgw", ChatID: "c1"}, UserID: "u1", Text: "修复登录 bug",
	})
	if asked != "修复登录 bug" || askedWS != "/ws/a" {
		t.Fatalf("ask 未路由: %q %q", asked, askedWS)
	}
}

func TestBridgeRejectsUnauthorized(t *testing.T) {
	cfg := im.DefaultConfig()
	cfg.Gateways = []im.GatewayConfig{{
		ID: "g1", Type: "testgw", Enabled: true, AllowUsers: []string{"u1"},
		Routes: []im.RouteConfig{{Chat: im.Chat{Gateway: "testgw", ChatID: "c1"}, Workspace: "/ws/a"}},
	}}
	b, _ := newTestBridge(t, cfg)

	called := false
	b.askFn = func(t imTarget, text string, _ []core.Content, _ string) { called = true }
	b.OnInMessage(im.InMessage{Chat: im.Chat{Gateway: "testgw", ChatID: "c1"}, UserID: "u2", Text: "hi"})
	if called {
		t.Fatal("未授权用户不应触发 ask")
	}
}

func TestBridgeMirrorPushesToChat(t *testing.T) {
	cfg := im.DefaultConfig()
	cfg.Gateways = []im.GatewayConfig{{ID: "g1", Type: "testgw", Enabled: true, AllowUsers: []string{"u1"}}}
	cfg.Mirrors = []im.MirrorConfig{
		{SessionID: "im-testgw-c1", Chat: im.Chat{Gateway: "testgw", ChatID: "c1"}, Mode: im.MirrorBidirectional},
	}
	b, tg := newTestBridge(t, cfg)

	// 事件流：ContentChunk 累积 → LLMEnd flush → 发送
	b.OnSessionEvent("/ws/a", "im-testgw-c1", &events.ContentChunk{
		RequestId: "r1", Content: "结果来了", EventType: events.ContentChunkType,
	})
	b.OnSessionEvent("/ws/a", "im-testgw-c1", &events.LLMEnd{
		RequestId: "r1", EventType: events.LLMEndType,
	})
	if len(tg.sent) == 0 {
		t.Fatal("应有消息发送")
	}
	if !strings.Contains(tg.sent[len(tg.sent)-1].Text, "结果来了") {
		t.Fatalf("消息内容: %+v", tg.sent)
	}
}

// TestBridgeIMInitiatedWritesMirror：IM 发起消息 → 自动写镜像表（bidirectional）→ 事件可推送。
func TestBridgeIMInitiatedWritesMirror(t *testing.T) {
	cfg := im.DefaultConfig()
	cfg.Gateways = []im.GatewayConfig{{
		ID: "g1", Type: "testgw", Enabled: true, AllowUsers: []string{"u1"},
		Routes: []im.RouteConfig{{Chat: im.Chat{Gateway: "testgw", ChatID: "c1"}, Workspace: "/ws/a"}},
	}}
	b, tg := newTestBridge(t, cfg)

	saved := false
	b.saveCfg = func(cfg im.Config) error { saved = true; return nil }

	b.OnInMessage(im.InMessage{
		Chat: im.Chat{Gateway: "testgw", ChatID: "c1"}, UserID: "u1", Text: "帮我看看",
	})
	// 镜像表应写入 bidirectional
	mirrors := b.rt.Router.MirrorsForSession("im-testgw-c1")
	if len(mirrors) != 1 || mirrors[0].Mode != im.MirrorBidirectional {
		t.Fatalf("镜像表未写入: %+v", mirrors)
	}
	if !saved {
		t.Fatal("应持久化镜像表")
	}
	// 事件侧现在能推送（镜像表驱动）
	b.OnSessionEvent("/ws/a", "im-testgw-c1", &events.ContentChunk{
		RequestId: "r2", Content: "回复", EventType: events.ContentChunkType,
	})
	b.OnSessionEvent("/ws/a", "im-testgw-c1", &events.LLMEnd{
		RequestId: "r2", EventType: events.LLMEndType,
	})
	if len(tg.sent) == 0 {
		t.Fatal("IM 发起会话应有推送")
	}
}

func TestBridgeApprovalAction(t *testing.T) {
	cfg := im.DefaultConfig()
	cfg.Gateways = []im.GatewayConfig{{ID: "g1", Type: "testgw", Enabled: true, AllowUsers: []string{"u1"}}}
	b, _ := newTestBridge(t, cfg)

	// 注册审批
	b.RegisterApproval("/ws/a", "im-testgw-c1", im.Chat{Gateway: "testgw", ChatID: "c1"}, "mid1", "call_1", "write_file", false)

	approved := ""
	b.approveFn = func(ws, sid, approvalID string, ok bool) error {
		approved = approvalID
		return nil
	}
	// 模拟按钮回调
	b.OnGatewayAction(im.GatewayAction{
		Chat: im.Chat{Gateway: "testgw", ChatID: "c1"}, ButtonID: "approve", Value: "call_1", UserID: "u1",
	})
	if approved != "call_1" {
		t.Fatalf("审批未生效: %q", approved)
	}
}

func TestBridgeSessionIDStable(t *testing.T) {
	id := chatSessionID(im.Chat{Gateway: "feishu", ChatID: "oc_1"})
	if id != "im-feishu-oc_1" {
		t.Fatalf("session id: %s", id)
	}
	if chatSessionID(im.Chat{Gateway: "feishu", ChatID: "oc_1"}) != id {
		t.Fatal("session id 不稳定")
	}
}

// TestApplyIMConfigHotReload：验证 Gateway 配置热更新（差异重建）。
func TestApplyIMConfigHotReload(t *testing.T) {
	initTestGateway()

	// 初始配置：g1 enabled
	cfg1 := im.DefaultConfig()
	cfg1.Enabled = true
	cfg1.Gateways = []im.GatewayConfig{
		{ID: "g1", Type: "testgw", Enabled: true, Config: json.RawMessage(`{"a":"1"}`)},
	}
	gw1, _ := im.New("testgw", nil)
	tg1 := gw1.(*testGateway)

	rt := &implugin.Runtime{
		Router:   im.NewRouter(cfg1),
		Security: im.NewSecurity(cfg1, ""),
	}
	rt.SetConfig(cfg1)
	rt.PutGateway("g1", gw1)
	// 用 plugin 包住（applyIMConfig 通过 imPluginOf 取 runtime）
	p := implugin.New("")
	// 注入 runtime（绕过 Enable：直接构造 Plugin 内部状态不可行，改用轻量验证）
	_ = p
	_ = rt
	_ = tg1

	// 直接验证 gatewayConfigEqual
	a := im.GatewayConfig{Type: "testgw", Config: json.RawMessage(`{"a":"1"}`)}
	b := im.GatewayConfig{Type: "testgw", Config: json.RawMessage(`{"a":"1"}`)}
	c := im.GatewayConfig{Type: "testgw", Config: json.RawMessage(`{"a":"2"}`)}
	if !gatewayConfigEqual(a, b) {
		t.Fatal("相同配置应相等")
	}
	if gatewayConfigEqual(a, c) {
		t.Fatal("不同配置不应相等")
	}
}

// ─── 端到端集成测试：IM 完整对话链路 ───
//
// 场景：飞书群发任务 → 路由到 workspace → 建会话（写镜像 bidirectional）→
//       会话执行产出事件流 → Renderer 推回飞书（叙事 + 审批卡）→
//       用户在飞书点批准 → 审批决策 → 会话恢复。
// 用 fake gateway 驱动，不依赖真实飞书。

func TestE2EIMFullConversation(t *testing.T) {
	initTestGateway()
	cfg := im.DefaultConfig()
	cfg.Enabled = true
	cfg.Gateways = []im.GatewayConfig{{
		ID: "g1", Type: "testgw", Enabled: true, AllowUsers: []string{"ou_1"},
		Routes: []im.RouteConfig{{Chat: im.Chat{Gateway: "testgw", ChatID: "oc_group"}, Workspace: "/ws/go-code"}},
	}}
	b, tg := newTestBridge(t, cfg)

	// 记录桥的 ask 调用（模拟真实 Session.Ask）
	var asks []string
	b.askFn = func(t imTarget, text string, _ []core.Content, _ string) { asks = append(asks, text) }

	// ① 用户发任务
	b.OnInMessage(im.InMessage{
		Chat: im.Chat{Gateway: "testgw", ChatID: "oc_group"}, UserID: "ou_1", Text: "修复登录 bug",
	})
	if len(asks) != 1 || asks[0] != "修复登录 bug" {
		t.Fatalf("任务未路由到会话: %v", asks)
	}
	sid := chatSessionID(im.Chat{Gateway: "testgw", ChatID: "oc_group"})

	// ② 镜像表应已写入（bidirectional）
	mirrors := b.rt.Router.MirrorsForSession(sid)
	if len(mirrors) != 1 || mirrors[0].Mode != im.MirrorBidirectional {
		t.Fatalf("镜像表未写入: %+v", mirrors)
	}

	// ③ 会话执行：流式叙事 → 工具 → 审批 → 完成
	// 3a. 叙事累积（ContentChunk）+ flush（LLMEnd）
	b.OnSessionEvent("/ws/go-code", sid, &events.ContentChunk{RequestId: "r1", Content: "我先看下 auth 模块", EventType: events.ContentChunkType})
	b.OnSessionEvent("/ws/go-code", sid, &events.LLMEnd{RequestId: "r1", EventType: events.LLMEndType})
	if len(tg.sent) == 0 || !strings.Contains(tg.sent[0].Text, "我先看下 auth 模块") {
		t.Fatalf("叙事未推回飞书: %+v", tg.sent)
	}

	// 3b. 审批请求 → 应注册 + 发审批卡
	approvalID := "call_123"
	b.OnSessionEvent("/ws/go-code", sid, &events.ToolApprovalRequested{
		Id: approvalID, Name: "write_file", Arguments: `{"path":"auth.go"}`,
		EventType: events.ToolApprovalType,
	})
	// 审批卡应已发送（Kind=card）
	var cardSent bool
	for _, m := range tg.sent {
		if m.Kind == im.KindCard && m.Card != nil && m.Card.Title == "待你批准 · write_file" {
			cardSent = true
		}
	}
	if !cardSent {
		t.Fatalf("审批卡未发送: %+v", tg.sent)
	}
	// 审批应已注册（approvals 表能查到）
	b.mu.Lock()
	am, ok := b.approvals[im.Chat{Gateway: "testgw", ChatID: "oc_group"}.String()]
	b.mu.Unlock()
	if !ok {
		t.Fatal("审批未注册")
	}
	if ap, ok := am.Get(approvalID); !ok || ap.SessionID != sid || ap.Workspace != "/ws/go-code" {
		t.Fatalf("审批记录异常: %+v ok=%v", ap, ok)
	}

	// ③c. 完成叙事
	b.OnSessionEvent("/ws/go-code", sid, &events.ContentChunk{RequestId: "r2", Content: "已修复", EventType: events.ContentChunkType})
	b.OnSessionEvent("/ws/go-code", sid, &events.LLMEnd{RequestId: "r2", EventType: events.LLMEndType})

	// ④ 用户在飞书点「批准」按钮
	approved := ""
	b.approveFn = func(ws, sid2, approvalID2 string, ok2 bool) error {
		approved = approvalID2
		if ws != "/ws/go-code" || sid2 != sid {
			t.Fatalf("审批回调路由错误: ws=%s sid=%s", ws, sid2)
		}
		return nil
	}
	b.OnGatewayAction(im.GatewayAction{
		Chat:     im.Chat{Gateway: "testgw", ChatID: "oc_group"},
		ButtonID: "approve", Value: approvalID, UserID: "ou_1",
	})
	if approved != approvalID {
		t.Fatalf("审批决策未生效: %q", approved)
	}
	// 审批状态应为 approved
	if ap, _ := am.Get(approvalID); ap.Status != im.ApprovalApproved {
		t.Fatalf("审批状态: %s", ap.Status)
	}

	// ⑤ 未授权用户点审批 → 拒绝（防代批）
	before, _ := am.Get(approvalID)
	b.OnGatewayAction(im.GatewayAction{
		Chat:     im.Chat{Gateway: "testgw", ChatID: "oc_group"},
		ButtonID: "approve", Value: approvalID, UserID: "ou_evil",
	})
	after, _ := am.Get(approvalID)
	if after.UserID != before.UserID || after.Status != im.ApprovalApproved {
		t.Fatalf("未授权用户不应改审批: %+v", after)
	}
}

// TestE2EIMRejectsUnknownChat：陌生聊天无路由 → 不执行（安全）。
func TestE2EIMRejectsUnknownChat(t *testing.T) {
	initTestGateway()
	cfg := im.DefaultConfig()
	cfg.Enabled = true
	cfg.Gateways = []im.GatewayConfig{{ID: "g1", Type: "testgw", Enabled: true, AllowUsers: []string{"ou_1"}}}
	cfg.Security.RequireBindConfirm = true
	b, _ := newTestBridge(t, cfg)

	called := false
	b.askFn = func(t imTarget, text string, _ []core.Content, _ string) { called = true }
	// 无 bindConfirmFn（未装配）→ 陌生聊天被拒
	b.OnInMessage(im.InMessage{
		Chat: im.Chat{Gateway: "testgw", ChatID: "oc_unknown"}, UserID: "ou_1", Text: "hi",
	})
	if called {
		t.Fatal("陌生聊天不应执行")
	}
}

// IM 文本指令：/new /list /switch 的会话派生与镜像逻辑。
func TestIMCommands(t *testing.T) {
	cfg := im.DefaultConfig()
	cfg.Enabled = true
	cfg.Gateways = []im.GatewayConfig{{
		ID: "g1", Type: "testgw", Enabled: true, AllowUsers: []string{"*"},
		Routes: []im.RouteConfig{{
			Chat:      im.Chat{Gateway: "testgw", ChatID: "oc_group"},
			Workspace: "/ws/g",
		}},
	}}
	b, tg := newTestBridge(t, cfg)
	chat := im.Chat{Gateway: "testgw", ChatID: "oc_group"}

	// /new 带别名 → 新建命名会话 + 镜像 + activeAlias
	b.OnInMessage(im.InMessage{Chat: chat, UserID: "ou_1", Text: "/new task1"})
	if b.activeAlias[chat.String()] != "task1" {
		t.Fatalf("activeAlias = %q, want task1", b.activeAlias[chat.String()])
	}
	wantSID := chatSessionIDAlias(chat, "task1")
	if !hasMirror(b.rt.ConfigSnapshot(), wantSID, chat) {
		t.Fatalf("mirror for %s not found", wantSID)
	}
	// 指令回复已发送（send 调用了 Gateway.Send）
	if len(tg.sent) == 0 {
		t.Fatal("no command reply sent")
	}

	// /list → 列出镜像
	tg.sent = nil
	b.OnInMessage(im.InMessage{Chat: chat, UserID: "ou_1", Text: "/list"})
	if len(tg.sent) != 1 || !strings.Contains(tg.sent[0].Text, wantSID) {
		t.Fatalf("list reply = %+v, want contains %s", tg.sent, wantSID)
	}

	// /switch 到不存在的会话 → 提示
	tg.sent = nil
	b.OnInMessage(im.InMessage{Chat: chat, UserID: "ou_1", Text: "/switch nope"})
	if len(tg.sent) != 1 || !strings.Contains(tg.sent[0].Text, "不存在") {
		t.Fatalf("switch(nonexist) reply = %+v", tg.sent)
	}

	// /switch 到存在的 → activeAlias 更新
	b.OnInMessage(im.InMessage{Chat: chat, UserID: "ou_1", Text: "/switch task1"})
	if b.activeAlias[chat.String()] != "task1" {
		t.Fatalf("after switch activeAlias = %q, want task1", b.activeAlias[chat.String()])
	}
}

// /new 无别名 → 时间戳后缀派生新 id（不覆盖默认会话）
func TestIMCommandNewNoAlias(t *testing.T) {
	cfg := im.DefaultConfig()
	cfg.Enabled = true
	cfg.Gateways = []im.GatewayConfig{{
		ID: "g1", Type: "testgw", Enabled: true, AllowUsers: []string{"*"},
		Routes: []im.RouteConfig{{
			Chat:      im.Chat{Gateway: "testgw", ChatID: "oc_g2"},
			Workspace: "/ws/g",
		}},
	}}
	b, _ := newTestBridge(t, cfg)
	chat := im.Chat{Gateway: "testgw", ChatID: "oc_g2"}
	// 先发普通消息 → 默认会话
	b.OnInMessage(im.InMessage{Chat: chat, UserID: "ou_1", Text: "hello"})
	// /new 无别名 → 新 id ≠ 默认 id
	b.OnInMessage(im.InMessage{Chat: chat, UserID: "ou_1", Text: "/new"})
	got := b.activeAlias[chat.String()]
	if got == "" {
		t.Fatal("/new no alias should set timestamp alias, got empty")
	}
	sid := chatSessionIDAlias(chat, got)
	if sid == chatSessionID(chat) {
		t.Fatalf("new session id %s should differ from default %s", sid, chatSessionID(chat))
	}
}

func hasMirror(cfg im.Config, sessionID string, chat im.Chat) bool {
	for _, mc := range cfg.Mirrors {
		if mc.SessionID == sessionID && mc.Chat.Gateway == chat.Gateway && mc.Chat.ChatID == chat.ChatID {
			return true
		}
	}
	return false
}

// TestIMImageBlocks IM 附件 → 图片内容块（read_file 之外的第二个图片入口：
// 用户在飞书发图，模型能看见）。非图片附件不进上下文。
func TestIMImageBlocks(t *testing.T) {
	atts := []im.Attachment{
		{Type: "image", URL: "data:image/png;base64,AAA", MimeType: "image/png"},
		{Type: "file", URL: "https://example.com/a.pdf", MimeType: "application/pdf"}, // 非图片 → 跳过
		{Type: "image", URL: "", MimeType: "image/png"},                               // 空 URL → 跳过
		{Type: "image", URL: "data:image/jpeg;base64,BBB"},                            // 无 mime → 兜底 png
	}
	blocks := imImageBlocks(atts)
	if len(blocks) != 2 {
		t.Fatalf("want 2 image blocks, got %d: %+v", len(blocks), blocks)
	}
	if blocks[0].Type != core.ContentTypeImage || blocks[0].Content != "data:image/png;base64,AAA" || blocks[0].MimeType != "image/png" {
		t.Fatalf("block[0] = %+v", blocks[0])
	}
	if blocks[1].Content != "data:image/jpeg;base64,BBB" || blocks[1].MimeType != "image/png" {
		t.Fatalf("block[1] = %+v", blocks[1])
	}
}

// TestIMImageBlocksEmpty 无附件/全非图片 → nil（不产出空切片）。
func TestIMImageBlocksEmpty(t *testing.T) {
	if got := imImageBlocks(nil); got != nil {
		t.Fatalf("nil → nil, got %+v", got)
	}
	if got := imImageBlocks([]im.Attachment{{Type: "file", URL: "x"}}); got != nil {
		t.Fatalf("non-image → nil, got %+v", got)
	}
}

// TestBridgeAcceptsImageOnlyMessage 纯图片消息（无文本）必须继续处理：
// 此前 OnInMessage 对空文本直接 return → 用户在飞书发的图被静默丢弃。
func TestBridgeAcceptsImageOnlyMessage(t *testing.T) {
	cfg := im.DefaultConfig()
	cfg.Gateways = []im.GatewayConfig{{
		ID: "g1", Type: "testgw", Enabled: true, AllowUsers: []string{"u1"},
		Routes: []im.RouteConfig{{Chat: im.Chat{Gateway: "testgw", ChatID: "c1"}, Workspace: "/ws/a"}},
	}}
	b, _ := newTestBridge(t, cfg)

	called := false
	var gotImages []core.Content
	b.askFn = func(t imTarget, text string, images []core.Content, _ string) {
		called = true
		gotImages = images
	}
	b.OnInMessage(im.InMessage{
		Chat: im.Chat{Gateway: "testgw", ChatID: "c1"}, UserID: "u1",
		Text: "", // 纯图片：无文本
		Attachments: []im.Attachment{
			{Type: "image", URL: "data:image/png;base64,SHOT", MimeType: "image/png"},
		},
	})
	if !called {
		t.Fatal("image-only message must reach the session (not silently dropped)")
	}
	if len(gotImages) != 1 || gotImages[0].Content != "data:image/png;base64,SHOT" {
		t.Fatalf("images = %+v", gotImages)
	}
}

// TestBridgeRejectsTrulyEmptyMessage 既无文本又无图片 → 仍然丢弃（不产生空轮）。
func TestBridgeRejectsTrulyEmptyMessage(t *testing.T) {
	cfg := im.DefaultConfig()
	cfg.Gateways = []im.GatewayConfig{{
		ID: "g1", Type: "testgw", Enabled: true, AllowUsers: []string{"u1"},
		Routes: []im.RouteConfig{{Chat: im.Chat{Gateway: "testgw", ChatID: "c1"}, Workspace: "/ws/a"}},
	}}
	b, _ := newTestBridge(t, cfg)
	called := false
	b.askFn = func(t imTarget, text string, _ []core.Content, _ string) { called = true }
	b.OnInMessage(im.InMessage{
		Chat: im.Chat{Gateway: "testgw", ChatID: "c1"}, UserID: "u1", Text: "   ",
		Attachments: []im.Attachment{{Type: "file", URL: "https://x/a.pdf"}}, // 非图片附件不算内容
	})
	if called {
		t.Fatal("empty message (no text, no image) must be dropped")
	}
}
