package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/im"
	implugin "github.com/seven7628/hai-harness/plugin/im"
)

// imBridge IM 事件桥：把会话事件流翻译成 IM 消息（镜像推送），
// 并把 IM 收到的消息路由到会话（Ask）。
//
// 架构（docs/IM_INTEGRATION.md §2/§4）：
//   - 事件侧：会话 handler 里调用 OnSessionEvent → Renderer 产出 OutMessage → 发到镜像 Chat
//   - 消息侧：Gateway.OnMessage → 查路由 → 绑定/创建会话 → Session.Ask
//   - 审批侧：Gateway.OnAction → ApprovalManager → Session.Approve
//
// 线程安全：内部互斥；事件与消息处理并发安全。
type imBridge struct {
	mu sync.Mutex

	rt *implugin.Runtime // IM 运行时（Router/Security/Gateways/Approval）

	// 按 Chat 的审批管理器（chat_id → manager）
	approvals map[string]*im.ApprovalManager

	// renderers per-chat 渲染器（流式累积跨事件保持）
	renderers map[string]*im.Renderer

	// activeAlias 每个 Chat 当前会话别名（/switch 切换；空 = 默认会话）。
	// key = chat.String()；OnInMessage 派生会话 id 时使用。
	activeAlias map[string]string

	// streams 每个 Chat 的活跃流式会话（ContentChunk 实时 Append；LLMEnd Close）。
	// key = chat.String()。
	streams map[string]*activeStream

	// toolStart 工具开始时间（ToolStart 记录，ToolResponse 算耗时；流式路径用）。
	// key = tool id。
	toolStart map[string]time.Time

	// 回调注入（由 bridge 装配）
	askFn       func(wsPath, sessionID, text string, images []core.Content) // 驱动会话（Session.Ask；images = IM 图片附件）
	approveFn   func(wsPath, sessionID, approvalID string, approve bool) error
	interruptFn func(wsPath, sessionID string)
	// bindConfirmFn 陌生 Chat 首次消息 → 桌面端弹窗（返回确认的 workspace；空=拒绝）
	bindConfirmFn func(chat im.Chat, userID, text string) (workspace string, approved bool)
	// saveCfg 持久化配置（镜像表热更新落盘；注入 = im.SaveConfig(m.imConfigPath())）
	saveCfg func(cfg im.Config) error
}

// newIMBridge 创建 IM 事件桥。
// activeStream 活跃流式会话。
type activeStream struct {
	ctrl im.StreamController
	// 累计内容（Close 时若 Renderer 也会发最终消息，可避免重复；这里 Close 即终）
	requestID string
}

func newIMBridge(rt *implugin.Runtime) *imBridge {
	return &imBridge{
		rt:          rt,
		approvals:   make(map[string]*im.ApprovalManager),
		renderers:   make(map[string]*im.Renderer),
		activeAlias: make(map[string]string),
		streams:     make(map[string]*activeStream),
		toolStart:   make(map[string]time.Time),
	}
}

// --- 事件侧（镜像推送） ---

// OnSessionEvent 会话事件 → IM 消息（bridgeSession.handler 调用）。
// sessionID 为事件归属会话；wsPath 为会话 workspace。
func (b *imBridge) OnSessionEvent(wsPath, sessionID string, e events.Event) {
	// 以镜像表为权威（支持桌面端手动"分享到 IM"的会话：镜像表有、sessionChat 无）
	mirrors := b.rt.Router.MirrorsForSession(sessionID)
	if len(mirrors) == 0 {
		// 诊断：事件到达但无镜像（新会话镜像写入失败 / sessionID 不匹配）
		if _, ok := e.(*events.AgentStart); ok {
			b.rt.Security.Audit(im.AuditEntry{Gateway: "", Chat: "", UserID: "", Action: "event_no_mirror", Detail: fmt.Sprintf("session=%s event=%T", sessionID, e), Allowed: true, SessionID: sessionID})
		}
		return
	}
	// 只推送到与该会话关联的 Chat
	for _, mc := range mirrors {
		if !im.ShouldMirror(mc.Mode) {
			continue
		}
		b.renderAndSend(wsPath, sessionID, mc.Chat, e)
	}
}

// renderAndSend 渲染事件 → 发送到 Chat。
func (b *imBridge) renderAndSend(wsPath, sessionID string, chat im.Chat, e events.Event) {
	// 找到该 Chat 对应的 Gateway 实例
	gw := b.gatewayFor(chat)
	if gw == nil {
		// 诊断：gateway 实例缺失（rt.Gateways 空 / 类型不匹配）
		b.rt.Security.Audit(im.AuditEntry{Gateway: chat.Gateway, Chat: chat.String(), UserID: "", Action: "send_skip", Detail: "gateway 实例缺失（gatewayFor=nil）", Allowed: false, SessionID: sessionID})
		return
	}
	// 审批事件：先注册到 ApprovalManager（否则飞书卡片按钮回调查不到审批 → 静默失败）
	if ap, ok := e.(*events.ToolApprovalRequested); ok {
		b.RegisterApproval(wsPath, sessionID, chat, "", ap.Id, ap.Name, false)
	}
	// 流式分支：gateway 支持流式时，ContentChunk 实时 Append，LLMEnd Close。
	// （非流式平台走 Renderer 攒批路径，行为不变。）
	if st := gw.Streamer(); st != nil {
		switch ev := e.(type) {
		case *events.ContentChunk:
			if strings.TrimSpace(ev.Content) == "" {
				return
			}
			b.mu.Lock()
			as := b.streams[chat.String()]
			if as == nil {
				ctrl, err := st.Start(context.Background(), chat, "go-code")
				if err != nil {
					b.mu.Unlock()
					fmt.Fprintf(os.Stderr, "✗ im stream start %s: %v\n", chat.String(), err)
					b.rt.Security.Audit(im.AuditEntry{Gateway: chat.Gateway, Chat: chat.String(), UserID: "", Action: "stream_failed", Detail: fmt.Sprintf("start err=%v", err), Allowed: false, SessionID: sessionID})
					return
				}
				as = &activeStream{ctrl: ctrl, requestID: ev.RequestId}
				b.streams[chat.String()] = as
			}
			ctrl := as.ctrl
			b.mu.Unlock()
			_ = ctrl.Append(context.Background(), ev.Content)
			return
		case *events.LLMEnd:
			b.mu.Lock()
			as := b.streams[chat.String()]
			delete(b.streams, chat.String())
			b.mu.Unlock()
			if as != nil {
				if err := as.ctrl.Close(context.Background()); err != nil {
					fmt.Fprintf(os.Stderr, "✗ im stream close %s: %v\n", chat.String(), err)
					b.rt.Security.Audit(im.AuditEntry{Gateway: chat.Gateway, Chat: chat.String(), UserID: "", Action: "stream_failed", Detail: fmt.Sprintf("close err=%v", err), Allowed: false, SessionID: sessionID})
				} else {
					b.rt.Security.Audit(im.AuditEntry{Gateway: chat.Gateway, Chat: chat.String(), UserID: "", Action: "message_sent", Detail: "stream closed", Allowed: true, SessionID: sessionID})
				}
			}
			return
		case *events.ToolStart:
			// 记录工具开始时间（耗时计算；Renderer 不感知流式，这里记录）
			b.rt.Security.Audit(im.AuditEntry{Gateway: chat.Gateway, Chat: chat.String(), UserID: "", Action: "tool_start", Detail: ev.Name, Allowed: true, SessionID: sessionID})
			b.mu.Lock()
			if b.toolStart == nil {
				b.toolStart = make(map[string]time.Time)
			}
			b.toolStart[ev.Id] = ev.Timestamp
			b.mu.Unlock()
			return
		case *events.ToolResponse:
			// 工具结果：追加到活跃流卡片（与正文同卡片，桌面端样式）
			b.mu.Lock()
			var dur time.Duration
			if st, ok := b.toolStart[ev.Id]; ok {
				dur = ev.Timestamp.Sub(st)
				delete(b.toolStart, ev.Id)
			}
			as := b.streams[chat.String()]
			ctrl := im.StreamController(nil)
			if as != nil {
				ctrl = as.ctrl
			}
			b.mu.Unlock()
			line := im.ToolLine(ev, dur)
			if ctrl != nil {
				_ = ctrl.Append(context.Background(), "\n"+line)
			} else {
				// 无活跃流：单独发富卡片（有结构化信息时）
				_, err := gw.Send(context.Background(), chat, im.OutMessage{Kind: im.KindToolCard, Text: line, Tool: im.ToolInfoFromResponse(ev, dur)})
				if err != nil {
					b.rt.Security.Audit(im.AuditEntry{Gateway: chat.Gateway, Chat: chat.String(), UserID: "", Action: "send_failed", Detail: fmt.Sprintf("tool line err=%v", err), Allowed: false, SessionID: sessionID})
				}
			}
			return
		}
	}
	renderer := b.rendererFor(chat)
	msgs := renderer.Handle(e, chat)
	for _, msg := range msgs {
		mid, err := gw.Send(context.Background(), chat, msg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ im send %s: %v\n", chat.String(), err)
			b.rt.Security.Audit(im.AuditEntry{Gateway: chat.Gateway, Chat: chat.String(), UserID: "", Action: "send_failed", Detail: fmt.Sprintf("event=%T err=%v", e, err), Allowed: false, SessionID: sessionID})
		} else {
			b.rt.Security.Audit(im.AuditEntry{Gateway: chat.Gateway, Chat: chat.String(), UserID: "", Action: "message_sent", Detail: fmt.Sprintf("event=%T kind=%s mid=%s", e, msg.Kind, mid), Allowed: true, SessionID: sessionID})
		}
	}
}

// gatewayFor 按 Chat 查 Gateway 实例。
func (b *imBridge) gatewayFor(chat im.Chat) im.Gateway {
	for _, g := range b.rt.Gateways {
		if g.Type() == chat.Gateway {
			return g
		}
	}
	return nil
}

// rendererFor 取 Chat 的 Renderer（per-chat 单例，流式累积跨事件保持）。
func (b *imBridge) rendererFor(chat im.Chat) *im.Renderer {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.renderers[chat.String()]
	if !ok {
		r = im.NewRenderer()
		b.renderers[chat.String()] = r
	}
	return r
}

// --- 消息侧（接收 → 路由） ---

// OnInMessage IM 收到消息 → 安全校验 → 路由 → 驱动会话。
func (b *imBridge) OnInMessage(msg im.InMessage) {
	rt := b.rt
	// 纯图片消息（无文本）也是有效输入：只要带图片附件就继续处理（视觉模型可看图；
	// 无图且无文本才是空消息）。IM 侧图片在 Gateway 解析时就已下载为 data URL。
	if msg.Chat.IsZero() || (strings.TrimSpace(msg.Text) == "" && len(imImageBlocks(msg.Attachments)) == 0) {
		return
	}
	// ① 授权名单
	if !rt.Security.UserAllowed(msg.Chat.Gateway, msg.UserID) {
		rt.Security.Audit(im.AuditEntry{Gateway: msg.Chat.Gateway, Chat: msg.Chat.String(), UserID: msg.UserID, Action: "message_rejected", Detail: "用户不在授权名单", Allowed: false})
		return
	}
	// ② 路由
	wsPath, ok := rt.Router.RouteFor(msg.Chat)
	if !ok {
		// ③ 绑定确认（陌生 Chat）
		if rt.Security.ShouldRequireConfirm(msg.Chat) && b.bindConfirmFn != nil {
			wsPath2, approved := b.bindConfirmFn(msg.Chat, msg.UserID, msg.Text)
			if !approved || wsPath2 == "" {
				rt.Security.Audit(im.AuditEntry{Gateway: msg.Chat.Gateway, Chat: msg.Chat.String(), UserID: msg.UserID, Action: "bind_pending", Detail: "等待桌面端确认绑定", Allowed: true})
				return
			}
			wsPath = wsPath2
			rt.Security.ConfirmChat(msg.Chat)
		} else {
			rt.Security.Audit(im.AuditEntry{Gateway: msg.Chat.Gateway, Chat: msg.Chat.String(), UserID: msg.UserID, Action: "route_miss", Detail: "无路由且无绑定确认", Allowed: true})
			return
		}
	}
	auditDetail := truncate(msg.Text, 120)
	if strings.TrimSpace(auditDetail) == "" {
		auditDetail = fmt.Sprintf("[%d image(s)]", len(imImageBlocks(msg.Attachments)))
	}
	rt.Security.Audit(im.AuditEntry{Gateway: msg.Chat.Gateway, Chat: msg.Chat.String(), UserID: msg.UserID, Action: "message_received", Detail: auditDetail, Allowed: true})

	// ④ 文本指令（/new /list /help /switch）：不驱动会话，直接处理并回复
	if handled := b.handleCommand(wsPath, msg); handled {
		return
	}

	// ⑤ 驱动会话（按 Chat 找/建会话；会话 id 由 Chat 派生 + 当前别名）
	b.mu.Lock()
	alias := b.activeAlias[msg.Chat.String()]
	b.mu.Unlock()
	sessionID := chatSessionIDAlias(msg.Chat, alias)
	// 写镜像表（bidirectional）：IM 发起的会话自动双向——事件侧 MirrorsForSession 能查到
	// 并推送回复回该 Chat。这是 IM 对话主链路的另一半（收→执行→推回）。
	// 幂等：同一 session+chat 已存在则跳过（避免重复行）。
	cfg := rt.Config
	hasMirror := false
	for _, mc := range cfg.Mirrors {
		if mc.SessionID == sessionID && mc.Chat.Gateway == msg.Chat.Gateway && mc.Chat.ChatID == msg.Chat.ChatID {
			hasMirror = true
			break
		}
	}
	if !hasMirror {
		cfg.Mirrors = append(cfg.Mirrors, im.MirrorConfig{
			SessionID: sessionID,
			Chat:      msg.Chat,
			Mode:      im.MirrorBidirectional,
		})
		rt.Config = cfg
		rt.Router.Update(cfg)
		// 持久化镜像表（IM 发起的会话跨重启保留）
		if b.saveCfg != nil {
			if err := b.saveCfg(cfg); err != nil {
				fmt.Fprintf(os.Stderr, "✗ im 持久化镜像表: %v\n", err)
			}
		}
	}
	if b.askFn != nil {
		b.askFn(wsPath, sessionID, msg.Text, imImageBlocks(msg.Attachments))
	}
}

// imImageBlocks IM 附件 → core.Content 图片块（data URL）。非图片附件（未来扩展的
// 文件类型）不进上下文——由各 Gateway 自行降级为文本说明。
// 纯图片消息（text 为空）也产出内容块：调用方据此投递「只有图」的用户消息
// （Session/AgentLoop 侧 image-only 消息已支持，见 core.NewUserMessage 语义）。
func imImageBlocks(atts []im.Attachment) []core.Content {
	var out []core.Content
	for _, a := range atts {
		if a.Type != "image" || a.URL == "" {
			continue
		}
		mime := a.MimeType
		if mime == "" {
			mime = "image/png"
		}
		out = append(out, core.Content{Type: core.ContentTypeImage, Content: a.URL, MimeType: mime})
	}
	return out
}

// OnGatewayAction IM 按钮回调 → 审批决策。
func (b *imBridge) OnGatewayAction(a im.GatewayAction) {
	rt := b.rt
	// 授权校验（防止群里他人代批）
	if !rt.Security.UserAllowed(a.Chat.Gateway, a.UserID) {
		rt.Security.Audit(im.AuditEntry{Gateway: a.Chat.Gateway, Chat: a.Chat.String(), UserID: a.UserID, Action: "action_rejected", Detail: "用户不在授权名单", Allowed: false})
		return
	}
	b.mu.Lock()
	am := b.approvals[a.Chat.String()]
	b.mu.Unlock()
	if am == nil {
		return
	}
	// value = approval_id（Renderer.approvalMessage 里 value=Id）
	approvalID := a.Value
	if approvalID == "" {
		return
	}
	ap, ok := am.Get(approvalID)
	if !ok {
		return
	}
	switch a.ButtonID {
	case "approve":
		if ok, _ := am.Decide(approvalID, true, a.UserID); ok && b.approveFn != nil {
			_ = b.approveFn(ap.Workspace, ap.SessionID, approvalID, true)
		}
	case "reject":
		if ok, _ := am.Decide(approvalID, false, a.UserID); ok && b.approveFn != nil {
			_ = b.approveFn(ap.Workspace, ap.SessionID, approvalID, false)
		}
	case "later":
		// 稍后：保留待批，不决策
	}
	rt.Security.Audit(im.AuditEntry{Gateway: a.Chat.Gateway, Chat: a.Chat.String(), UserID: a.UserID, Action: "approval_" + a.ButtonID, Detail: approvalID, Allowed: true})
}

// --- 装配辅助 ---

// Attach 把 Gateway 的消息/动作回调接到桥。
func (b *imBridge) Attach() {
	for _, g := range b.rt.Gateways {
		g.OnMessage(b.OnInMessage)
		g.OnAction(b.OnGatewayAction)
	}
}

// preferredModel 新会话默认模型：最近一次 switch_model 的选择优先，
// 否则回退 provider 默认模型（与前端 globalPrefs 语义对齐：IM 会话复用桌面端当前选择）。
func (m *manager) preferredModel() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastModel != "" {
		return m.lastModel
	}
	return m.provCfg.defaultModel()
}

// attachManager 装配 bridge 回调（askFn/approveFn/bindConfirmFn）。
// m 为 manager；wsPath 用于会话路由。
func (b *imBridge) attachManager(m *manager, wsPath string) {
	ctx := context.Background()
	b.askFn = func(ws, sessionID, text string, images []core.Content) {
		bs := m.get(ws, sessionID)
		if bs == nil {
			// 会话不存在 → 创建（IM 发起）
			var err error
			bs, err = m.create(ws, sessionID, "")
			if err != nil {
				fmt.Fprintf(os.Stderr, "✗ im create session %s: %v\n", sessionID, err)
				return
			}
		}
		// 内容块：文本块（有文本时）+ 图片块（IM 图片附件）。纯图片消息（无文本）
		// 只发图片块 —— 与桌面端「空文本 + 附件」语义一致（isEmptyContent 认非文本块）。
		blocks := make([]core.Content, 0, len(images)+1)
		if strings.TrimSpace(text) != "" {
			blocks = append(blocks, core.Content{Type: core.ContentTypeText, Content: text})
		}
		blocks = append(blocks, images...)
		if len(blocks) == 0 {
			return // 空消息（无文本无图）：不投递
		}
		if err := bs.s.Ask(ctx, core.NewUserMessage(blocks...)); err != nil {
			fmt.Fprintf(os.Stderr, "✗ im ask %s: %v\n", sessionID, err)
			return
		}
		bs.lastActive.Store(time.Now().UnixMilli())
		bs.wakeRun()
	}
	b.saveCfg = func(cfg im.Config) error {
		return im.SaveConfig(m.imConfigPath(), cfg)
	}
	b.approveFn = func(ws, sessionID, approvalID string, approve bool) error {
		bs := m.get(ws, sessionID)
		if bs == nil {
			return fmt.Errorf("session %s not found", sessionID)
		}
		return bs.s.Approve(approvalID, approve)
	}
	// 绑定确认：发桌面端弹窗事件（bridge_im_bind_confirm）；UI 确认后经 im_bind_confirm 命令回填
	b.bindConfirmFn = func(chat im.Chat, userID, text string) (string, bool) {
		m.out.writeLine(map[string]any{
			"event_type": "im_bind_confirm_requested",
			"workspace":  wsPath,
			"data": map[string]any{
				"chat":    chat,
				"user_id": userID,
				"text":    truncate(text, 80),
			},
		})
		// 返回空 = 未确认（等待桌面端 im_bind_confirm 命令回填路由后重试）
		return "", false
	}
}

// RegisterApproval 注册审批（事件侧 ToolApprovalRequested 到达时调用）。
func (b *imBridge) RegisterApproval(wsPath, sessionID string, chat im.Chat, messageID, approvalID, toolName string, textMode bool) {
	b.mu.Lock()
	am, ok := b.approvals[chat.String()]
	if !ok {
		am = im.NewApprovalManager(0, func(a *im.Approval) {
			// 决策后更新卡片（可选）
		})
		b.approvals[chat.String()] = am
	}
	b.mu.Unlock()
	am.Register(approvalID, sessionID, wsPath, chat, messageID, toolName, textMode)
}

// chatSessionID 由 Chat 派生会话 id（稳定：同一 Chat 同一会话）。
// alias 非空 → 命名会话（/new <alias> 派生；同 chat 可多会话）。
func chatSessionID(chat im.Chat) string {
	return chatSessionIDAlias(chat, "")
}

// chatSessionIDAlias 带会话别名的派生：im-{gateway}-{chat_id} 或 im-{gateway}-{chat_id}-{alias}。
func chatSessionIDAlias(chat im.Chat, alias string) string {
	base := "im-" + chat.Gateway + "-" + chat.ChatID
	if alias == "" {
		return base
	}
	return base + "-" + alias
}

// imCommand IM 文本指令（@机器人 /xxx；单聊直接 /xxx）。
type imCommand struct {
	name  string // new | list | help | switch
	alias string // 指令参数（/new mytask → alias="mytask"）
}

// parseIMCommand 解析文本指令。非指令返回 ok=false。
// 指令：以 "/" 开头（群聊时 @ 前缀已剥）；空指令忽略。
func parseIMCommand(text string) (imCommand, bool) {
	t := strings.TrimSpace(text)
	if t == "" || t[0] != '/' {
		return imCommand{}, false
	}
	// 支持 "@br-claw /new xxx"：剥掉开头的 @ 提及（stripMentionKeys 已处理，但双保险）
	t = stripMentionKeysLocal(t)
	t = strings.TrimSpace(t)
	if t == "" || t[0] != '/' {
		return imCommand{}, false
	}
	fields := strings.Fields(t)
	name := strings.TrimPrefix(fields[0], "/")
	alias := ""
	if len(fields) > 1 {
		alias = strings.TrimSpace(strings.Join(fields[1:], " "))
	}
	switch name {
	case "new", "list", "help", "switch":
		return imCommand{name: name, alias: alias}, true
	default:
		return imCommand{}, false
	}
}

// handleCommand 处理 IM 文本指令。返回 true = 已处理（不再驱动会话）。
// 指令结果直接发回当前 Chat（经 Gateway.Send；不写会话历史）。
func (b *imBridge) handleCommand(wsPath string, msg im.InMessage) bool {
	cmd, ok := parseIMCommand(msg.Text)
	if !ok {
		return false
	}
	gw := b.gatewayFor(msg.Chat)
	if gw == nil {
		return true // 网关缺失：指令无法回复，但不再当普通消息
	}
	ctx := context.Background()
	send := func(text string) {
		_, err := gw.Send(ctx, msg.Chat, im.OutMessage{Kind: im.KindText, Text: text})
		if err != nil {
			b.rt.Security.Audit(im.AuditEntry{Gateway: msg.Chat.Gateway, Chat: msg.Chat.String(), UserID: msg.UserID, Action: "command_reply_failed", Detail: err.Error(), Allowed: true})
		}
	}

	switch cmd.name {
	case "help":
		send("📋 **go-code IM 指令**\n" +
			"`/new [名字]` — 新建会话（同群可开多个，名字可选）\n" +
			"`/list` — 列出本会话关联的会话\n" +
			"`/switch <名字>` — 切换到此会话\n" +
			"`/help` — 本帮助\n" +
			"\n直接发消息 = 在当前会话继续对话")
		return true

	case "new":
		// 新建会话：派生新 id（默认或别名），挂镜像，后续消息进新会话
		sessionID := chatSessionIDAlias(msg.Chat, cmd.alias)
		if sessionID == chatSessionID(msg.Chat) {
			// 无别名：默认会话已存在 → 清空上下文需换 id；用时间戳后缀区分
			cmd.alias = fmt.Sprintf("s%d", time.Now().Unix())
			sessionID = chatSessionIDAlias(msg.Chat, cmd.alias)
		}
		// 幂等挂镜像（新会话 → 该 chat 双向推送）
		cfg := b.rt.Config
		hasMirror := false
		for _, mc := range cfg.Mirrors {
			if mc.SessionID == sessionID && mc.Chat.Gateway == msg.Chat.Gateway && mc.Chat.ChatID == msg.Chat.ChatID {
				hasMirror = true
				break
			}
		}
		if !hasMirror {
			cfg.Mirrors = append(cfg.Mirrors, im.MirrorConfig{
				SessionID: sessionID,
				Chat:      msg.Chat,
				Mode:      im.MirrorBidirectional,
			})
			b.rt.Config = cfg
			b.rt.Router.Update(cfg)
			if b.saveCfg != nil {
				if err := b.saveCfg(cfg); err != nil {
					fmt.Fprintf(os.Stderr, "✗ im 持久化镜像表: %v\n", err)
				}
			}
		}
		// 记录当前别名：后续消息进新会话
		b.mu.Lock()
		b.activeAlias[msg.Chat.String()] = cmd.alias
		b.mu.Unlock()
		send(fmt.Sprintf("✅ 已新建会话 **%s**（id: %s）\n直接发消息即可开始", cmd.alias, sessionID))
		return true

	case "list":
		// 列出本 chat 关联的会话（镜像表里同 chat 的 session）
		mirrors := b.rt.Router.MirrorsForChat(msg.Chat)
		if len(mirrors) == 0 {
			send("📭 当前没有关联会话，发消息会自动创建")
			return true
		}
		lines := []string{"📚 **本群关联会话**"}
		for _, mc := range mirrors {
			lines = append(lines, "- `"+mc.SessionID+"`")
		}
		send(strings.Join(lines, "\n"))
		return true

	case "switch":
		// 切换到命名会话（id = im-{gateway}-{chat}-{alias}）；不存在则提示用 /new
		if cmd.alias == "" {
			send("⚠️ 用法：`/switch <名字>`（名字来自 /new 时指定的别名）")
			return true
		}
		target := chatSessionIDAlias(msg.Chat, cmd.alias)
		// 检查镜像表是否有该会话
		mirrors := b.rt.Router.MirrorsForChat(msg.Chat)
		found := false
		for _, mc := range mirrors {
			if mc.SessionID == target {
				found = true
				break
			}
		}
		if !found {
			send(fmt.Sprintf("⚠️ 会话 **%s** 不存在，先 `/new %s` 创建", cmd.alias, cmd.alias))
			return true
		}
		// 记录当前别名：后续消息进目标会话
		b.mu.Lock()
		b.activeAlias[msg.Chat.String()] = cmd.alias
		b.mu.Unlock()
		send(fmt.Sprintf("🔄 已切换到会话 **%s**（id: %s）\n直接发消息进入该会话", cmd.alias, target))
		return true
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// stripMentionKeysLocal 去掉文本里的 @ 占位符（@_user_N），保留用户实际输入。
// （与 im/feishu 内部实现一致；指令解析需要——群聊 @ 后带指令文本）
func stripMentionKeysLocal(s string) string {
	re := regexp.MustCompile(`@_user_\d+`)
	return strings.TrimSpace(re.ReplaceAllString(s, ""))
}
