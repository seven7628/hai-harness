package feishu

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/seven7628/hai-harness/im"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// Gateway 飞书 IM Gateway 实现（im.Gateway 接口）。
//
// 底层用官方 SDK（larksuite/oapi-sdk-go/v3）：
//   - 发送：sdkClient（client.Im.V1.Message.Create / Patch）
//   - 接收：larkws（长连接，本地无公网首选）；webhook 模式用 larkevent HTTP 回调
//   - 事件：im.message.receive_v1 → InMessage；card.action.trigger → GatewayAction
//
// Interface 在 github.com/seven7628/hai-harness/im 包（业务零感知）；换库/换平台只动本包。
type Gateway struct {
	cfg Config

	mu     sync.Mutex
	status im.Status
	sdk    *sdkClient
	msgH   func(im.InMessage)
	actH   func(im.GatewayAction)

	// 长连接生命周期
	wsClient *larkws.Client
	cancel   context.CancelFunc
	done     chan struct{}
}

// New 构造飞书 Gateway（im.GatewayConstructor）。
func New(cfgJSON json.RawMessage) (im.Gateway, error) {
	var cfg Config
	if len(cfgJSON) > 0 {
		if err := json.Unmarshal(cfgJSON, &cfg); err != nil {
			return nil, fmt.Errorf("feishu: 解析配置: %w", err)
		}
	}
	cfg, err := cfg.Validate()
	if err != nil {
		return nil, err
	}
	return &Gateway{cfg: cfg, status: im.StatusDisconnected}, nil
}

// Type 实现 im.Gateway。
func (g *Gateway) Type() string { return Type }

// Name 实现 im.Gateway。
func (g *Gateway) Name() string { return "飞书" }

// Capabilities 实现 im.Gateway：飞书支持卡片按钮 + 话题 + markdown；卡片可更新。
func (g *Gateway) Capabilities() im.Capabilities {
	return im.Capabilities{
		CardButtons:   true,
		UpdateMessage: true, // 卡片可更新（Patch）
		Thread:        true,
		Markdown:      true,
		Files:         false, // v2
		Streaming:     true,  // 占位卡片 + Patch 模拟流式
	}
}

// Status 实现 im.Gateway。
func (g *Gateway) Status() im.Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.status
}

func (g *Gateway) setStatus(s im.Status) {
	g.mu.Lock()
	g.status = s
	g.mu.Unlock()
}

// Start 实现 im.Gateway：初始化 SDK client + 启动接收循环（长连接默认 / webhook）。
func (g *Gateway) Start(ctx context.Context) error {
	sdk, err := newSDKClient(g.cfg, &http.Client{Timeout: 15e9}) // 15s
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.sdk = sdk
	g.mu.Unlock()

	switch g.cfg.Mode {
	case "webhook":
		return g.startWebhook(ctx)
	default:
		return g.startWS(ctx)
	}
}

// Stop 实现 im.Gateway。
func (g *Gateway) Stop(ctx context.Context) error {
	g.mu.Lock()
	cancel := g.cancel
	g.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if g.done != nil {
		select {
		case <-g.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	g.setStatus(im.StatusDisconnected)
	return nil
}

// Send 实现 im.Gateway：按 OutMessage.Kind 映射为飞书消息。
func (g *Gateway) Send(ctx context.Context, chat im.Chat, msg im.OutMessage) (string, error) {
	g.mu.Lock()
	sdk := g.sdk
	g.mu.Unlock()
	if sdk == nil {
		return "", fmt.Errorf("feishu: gateway 未启动")
	}
	switch msg.Kind {
	case im.KindText:
		return sdk.sendText(ctx, chat.ChatID, msg.Text)
	case im.KindMarkdown:
		// 富文本正文：用卡片 markdown 元素渲染（post 消息不支持 lark_md 标签；
		// 卡片 schema 2.0 的 markdown 元素支持 **加粗**/`代码`/链接渲染）
		return sdk.sendInteractive(ctx, chat.ChatID, msgToMarkdownCard(msg.Text))
	case im.KindCard:
		return sdk.sendInteractive(ctx, chat.ChatID, msgToCard(msg))
	case im.KindToolCard:
		// 工具调用富卡片（div fields 双列布局 + hr 分割；已真实联调验证 V2 支持）
		if msg.Tool != nil {
			return sdk.sendInteractive(ctx, chat.ChatID, msgToToolCard(msg.Tool))
		}
		// 无结构化信息：降级 markdown 文本
		return sdk.sendInteractive(ctx, chat.ChatID, msgToMarkdownCard(msg.Text))
	default:
		return "", im.ErrUnsupported
	}
}

// Update 实现 im.Gateway：更新已发卡片。
func (g *Gateway) Update(ctx context.Context, messageID string, msg im.OutMessage) error {
	g.mu.Lock()
	sdk := g.sdk
	g.mu.Unlock()
	if sdk == nil {
		return fmt.Errorf("feishu: gateway 未启动")
	}
	if msg.Kind != im.KindCard && msg.Kind != im.KindMarkdown {
		return im.ErrUnsupported
	}
	return sdk.updateCard(ctx, messageID, msgToCard(msg))
}

// OnMessage 实现 im.Gateway。
func (g *Gateway) OnMessage(h func(im.InMessage)) {
	g.mu.Lock()
	g.msgH = h
	g.mu.Unlock()
}

// OnAction 实现 im.Gateway。
func (g *Gateway) OnAction(h func(im.GatewayAction)) {
	g.mu.Lock()
	g.actH = h
	g.mu.Unlock()
}

// ListChats 实现 im.ChatLister：列出机器人所在群（路由绑定下拉数据源）。
func (g *Gateway) ListChats(ctx context.Context) ([]im.ChatInfo, error) {
	g.mu.Lock()
	sdk := g.sdk
	g.mu.Unlock()
	if sdk == nil {
		return nil, fmt.Errorf("feishu: gateway 未启动")
	}
	infos, err := sdk.ListChats(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]im.ChatInfo, 0, len(infos))
	for _, c := range infos {
		out = append(out, im.ChatInfo{ChatID: c.ChatID, Name: c.Name})
	}
	return out, nil
}

// --- 内部：接收循环 ---

// startWS 长连接接收（官方 larkws）。断线重连由 SDK 内部处理。
func (g *Gateway) startWS(ctx context.Context) error {
	ctx2, cancel := context.WithCancel(ctx)
	g.mu.Lock()
	g.cancel = cancel
	g.done = make(chan struct{})
	g.mu.Unlock()

	d := dispatcher.NewEventDispatcher(g.cfg.VerifyToken, g.cfg.EncryptKey).
		// 消息接收
		OnP2MessageReceiveV1(func(ctx context.Context, ev *larkim.P2MessageReceiveV1) error {
			g.handleMessageEvent(ctx, ev)
			return nil
		}).
		// 卡片按钮回调
		OnP2CardActionTrigger(func(ctx context.Context, ev *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
			g.handleActionEvent(ctx, ev)
			return &callback.CardActionTriggerResponse{}, nil
		})

	wsClient := larkws.NewClient(g.cfg.AppID, g.cfg.AppSecret,
		larkws.WithEventHandler(d),
		larkws.WithLogLevel(larkcore.LogLevelError),
		larkws.WithAutoReconnect(true),
	)
	g.mu.Lock()
	g.wsClient = wsClient
	g.mu.Unlock()

	// 启动长连接（SDK 内部管理重连；ctx2 取消即停止）
	go func() {
		defer close(g.done)
		defer cancel()
		g.setStatus(im.StatusConnecting)
		wsClient.Start(ctx2) // 阻塞直到 ctx 取消
		g.setStatus(im.StatusDisconnected)
	}()
	return nil
}

// startWebhook Webhook 接收（官方 larkevent，需公网 + 签名/加密）。
// v1 骨架：需要 http.Server + larkevent 的 HTTP 处理函数；本地无公网，默认用长连接。
func (g *Gateway) startWebhook(ctx context.Context) error {
	// 预留：http.Server 接收 larkevent 回调（dispatcher + 签名校验 / AES 解密）。
	// v1 建议 websocket 模式；webhook 待公网环境就绪后补全。
	return fmt.Errorf("feishu: webhook 模式暂未实现，请使用 websocket 模式（mode=websocket）")
}

// --- 内部：事件解析 ---

// handleMessageEvent 消息事件 → InMessage → msgH。
func (g *Gateway) handleMessageEvent(ctx context.Context, ev *larkim.P2MessageReceiveV1) {
	if ev == nil || ev.Event == nil || ev.Event.Message == nil {
		return
	}
	g.mu.Lock()
	msgH := g.msgH
	g.mu.Unlock()
	if msgH == nil {
		return
	}
	msg := ev.Event.Message
	// 机器人自身消息忽略（避免回环）
	if ev.Event.Sender != nil && ev.Event.Sender.SenderType != nil && *ev.Event.Sender.SenderType == "bot" {
		return
	}
	chatID := ""
	if msg.ChatId != nil {
		chatID = *msg.ChatId
	}
	userID := ""
	if ev.Event.Sender != nil && ev.Event.Sender.SenderId != nil && ev.Event.Sender.SenderId.OpenId != nil {
		userID = *ev.Event.Sender.SenderId.OpenId
	}
	// 群聊：只响应 @ 机器人的消息（防闲聊触发 + 防噪声）。
	// 单聊（p2p）：全部响应。
	isGroup := msg.ChatType != nil && *msg.ChatType == "group"
	if isGroup {
		mentionedBot := false
		for _, m := range msg.Mentions {
			if m != nil && m.MentionedType != nil && *m.MentionedType == "bot" {
				mentionedBot = true
				break
			}
		}
		if !mentionedBot {
			return // 群聊未 @ 机器人 → 忽略
		}
	}
	text, attachments := g.parseContent(ctx, msg)
	// 去掉 @ 占位符（飞书文本消息里 @ 显示为 @_user_N；单聊无此问题）
	text = stripMentionKeys(text)
	msgH(im.InMessage{
		Chat: im.Chat{
			Gateway: Type,
			ChatID:  chatID,
		},
		UserID:      userID,
		Text:        text,
		Attachments: attachments,
	})
}

// parseContent 解析消息内容 → (文本, 附件)。
// 支持的消息类型（按 msg_type）：
//   - text：JSON {"text": "..."}
//   - post（富文本）：递归抽 text 节点 + 图片节点（富文本里的图同样可取）
//   - image：{"image_key": "..."} → 下载为 data URL 附件（视觉模型可看图）
//
// 图片下载需要 message_id（resource API 的路径参数），失败不阻断文本处理
// （降级为文本说明，用户/模型至少知道「有一条图片消息但没取到」）。
// 纯图片消息（无文本）也返回附件 —— 由上层按 image 内容块投递。
func (g *Gateway) parseContent(ctx context.Context, msg *larkim.EventMessage) (string, []im.Attachment) {
	if msg == nil || msg.Content == nil {
		return "", nil
	}
	msgType := ""
	if msg.MessageType != nil {
		msgType = *msg.MessageType
	}
	raw := *msg.Content
	switch msgType {
	case "image":
		var c struct {
			ImageKey string `json:"image_key"`
		}
		if err := json.Unmarshal([]byte(raw), &c); err != nil || c.ImageKey == "" {
			return "", nil
		}
		att := g.fetchImage(ctx, msg, c.ImageKey)
		if att == nil {
			return "[image: failed to download — the image could not be retrieved from Feishu]", nil
		}
		return "", []im.Attachment{*att}
	case "post":
		text, keys := parsePostContent(raw)
		var atts []im.Attachment
		for _, key := range keys {
			if att := g.fetchImage(ctx, msg, key); att != nil {
				atts = append(atts, *att)
			}
		}
		return text, atts
	default: // text 及其他类型：按 {"text": "..."} 解析（未知类型得到空文本）
		var c struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal([]byte(raw), &c)
		return c.Text, nil
	}
}

// fetchImage 下载飞书图片资源 → data URL 附件（失败返回 nil，调用方降级）。
// 飞书图片上限较大（默认 10MB），此处不额外压缩：视觉模型侧由 tools 引擎/
// provider 降级路径处理，IM 侧保持「原样取到」最简单可靠。
func (g *Gateway) fetchImage(ctx context.Context, msg *larkim.EventMessage, imageKey string) *im.Attachment {
	if g.sdk == nil || msg.MessageId == nil || *msg.MessageId == "" {
		return nil
	}
	req := larkim.NewGetMessageResourceReqBuilder().
		MessageId(*msg.MessageId).
		FileKey(imageKey).
		Type("image").
		Build()
	resp, err := g.sdk.client.Im.V1.MessageResource.Get(ctx, req)
	if err != nil || resp == nil || !resp.Success() || resp.File == nil {
		return nil
	}
	data, err := io.ReadAll(resp.File)
	if err != nil || len(data) == 0 {
		return nil
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		mime = "image/jpeg" // 飞书图片实际多为 jpeg/png；探测不出时按 jpeg
	}
	return &im.Attachment{
		Type:     "image",
		Name:     imageKey,
		URL:      "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data),
		MimeType: mime,
	}
}

// parsePostContent 解析飞书富文本（post）内容：递归抽取 text 节点文本与 img 节点
// 的 image_key。结构形如 {"title":"...","content":[[{"tag":"text","text":"..."},{"tag":"img","image_key":"..."}]]}。
func parsePostContent(raw string) (string, []string) {
	var post struct {
		Title   string          `json:"title"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal([]byte(raw), &post); err != nil {
		return "", nil
	}
	var textParts []string
	var keys []string
	var walk func(nodes []any)
	walk = func(nodes []any) {
		for _, n := range nodes {
			obj, ok := n.(map[string]any)
			if !ok {
				continue
			}
			switch obj["tag"] {
			case "text":
				if t, ok := obj["text"].(string); ok && t != "" {
					textParts = append(textParts, t)
				}
			case "a":
				// 链接：name + href（保链接对模型有用）
				name, _ := obj["text"].(string)
				href, _ := obj["href"].(string)
				if name != "" || href != "" {
					textParts = append(textParts, name+href)
				}
			case "img":
				if k, ok := obj["image_key"].(string); ok && k != "" {
					keys = append(keys, k)
				}
			case "at":
				// @提及：飞书以 user_id 表达，文本侧不展开（stripMentionKeys 处理占位符）
			}
		}
	}
	// content 标准形态是「段落数组」：[[节点...], [节点...]]。
	// 两种形态都是 JSON 数组，故按**首元素的形态**区分（数组 → 段落；对象 → 扁平节点
	// 列表，兼容部分事件/旧版本形态），不能靠「外层解析是否成功」判断。
	var elems []json.RawMessage
	if err := json.Unmarshal(post.Content, &elems); err != nil {
		return "", nil
	}
	if len(elems) > 0 && json.Unmarshal(elems[0], &[]any{}) == nil {
		// 首元素是数组 → 段落形态
		for _, p := range elems {
			var nodes []any
			if err := json.Unmarshal(p, &nodes); err == nil {
				walk(nodes)
			}
		}
	} else {
		// 首元素是对象（或空）→ 扁平节点列表
		var nodes []any
		if err := json.Unmarshal(post.Content, &nodes); err == nil {
			walk(nodes)
		}
	}
	out := strings.Join(textParts, "")
	if post.Title != "" {
		out = post.Title + "\n" + out
	}
	return strings.TrimSpace(out), keys
}

// stripMentionKeys 去掉文本里的 @ 占位符（@_user_1 等），保留用户实际输入。
func stripMentionKeys(s string) string {
	// 飞书 @ 占位符格式：@_user_N
	re := regexp.MustCompile(`@_user_\d+`)
	return strings.TrimSpace(re.ReplaceAllString(s, ""))
}

// handleActionEvent 卡片按钮回调 → GatewayAction → actH。
func (g *Gateway) handleActionEvent(ctx context.Context, ev *callback.CardActionTriggerEvent) {
	if ev == nil || ev.Event == nil {
		return
	}
	g.mu.Lock()
	actH := g.actH
	g.mu.Unlock()
	if actH == nil {
		return
	}
	e := ev.Event
	// 按钮 value 结构：{action, value}
	actionID, value := "", ""
	if e.Action != nil && e.Action.Value != nil {
		actionID, _ = e.Action.Value["action"].(string)
		value, _ = e.Action.Value["value"].(string)
	}
	chatID := ""
	if e.Context != nil {
		chatID = e.Context.OpenChatID
	}
	userID := ""
	if e.Operator != nil {
		userID = e.Operator.OpenID
	}
	actH(im.GatewayAction{
		Chat: im.Chat{
			Gateway: Type,
			ChatID:  chatID,
		},
		ButtonID: actionID,
		Value:    value,
		UserID:   userID,
	})
}

// --- 内部：卡片构造 ---

// msgToCard OutMessage → 飞书卡片结构（markdown 正文 + 按钮）。
func msgToCard(msg im.OutMessage) map[string]any {
	body := msg.Text
	if msg.Card != nil {
		body = msg.Card.Body
	}
	if body == "" {
		body = " "
	}
	elements := []any{
		map[string]any{
			"tag":     "markdown",
			"content": body,
		},
	}
	if msg.Card != nil && len(msg.Card.Buttons) > 0 {
		// V2 卡片：button 直接作元素 + behaviors 声明回调（V2 不再用 action 包裹，
		// 回调经 behaviors:[{type:callback}] 声明；SDK v3.11 的 card 模型无此字段，
		// 手写 JSON 支持）
		for _, b := range msg.Card.Buttons {
			elements = append(elements, map[string]any{
				"tag": "button",
				"text": map[string]any{
					"tag":     "plain_text",
					"content": b.Label,
				},
				"type": "primary",
				"behaviors": []any{
					map[string]any{
						"type": "callback",
						"value": map[string]any{
							"action": b.ID,
							"value":  b.Value,
						},
					},
				},
			})
		}
	}
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"title": map[string]any{"tag": "plain_text", "content": cardTitle(msg)},
		},
		"body": map[string]any{
			"elements": elements,
		},
	}
	return card
}

func cardTitle(msg im.OutMessage) string {
	if msg.Card != nil && msg.Card.Title != "" {
		return msg.Card.Title
	}
	return "go-code"
}

// msgToToolCard 工具调用卡片（紧凑版）：
// 无 header / 无 hr / 无双列——单 markdown 元素一行，尺寸接近普通消息，
// 保留加粗/代码/颜色渲染（状态着色、变更着色）。避免大卡片刷屏。
func msgToToolCard(t *im.ToolInfo) map[string]any {
	// 状态着色
	statusText := "<font color='green'>✓</font>"
	if t.Status == "error" {
		statusText = "<font color='red'>⚠️</font>"
	}
	// 内容：icon name `args` status · ⏱ dur · 变更
	var sb strings.Builder
	sb.WriteString(t.Icon + " **" + t.Name + "**")
	if t.Args != "" {
		sb.WriteString(" `" + t.Args + "`")
	}
	sb.WriteString(" " + statusText)
	if t.Dur != "" {
		sb.WriteString(" · ⏱ " + t.Dur)
	}
	if t.DiffPath != "" {
		sb.WriteString(" · <font color='red'>变更 +" + fmt.Sprintf("%d", t.DiffAdd) + " −" + fmt.Sprintf("%d", t.DiffDel) + "</font>")
	}
	if t.Status == "error" && t.Error != "" {
		sb.WriteString(" <font color='red'>`" + t.Error + "`</font>")
	}
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{"wide_screen_mode": true},
		"body": map[string]any{
			"elements": []any{
				map[string]any{"tag": "markdown", "content": sb.String()},
			},
		},
	}
}

// truncateStr 按字符截断（标题过长保护）。
func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// msgToMarkdownCard 纯文本回复的 markdown 渲染卡片（V2 schema）。
// post 消息不支持 lark_md 标签（200621/230001），卡片 markdown 元素支持
// **加粗** / `代码` / [链接] 等基础渲染——手机 IM 阅读体验更好。
// 无按钮（非审批），不加 header 保持轻量。
func msgToMarkdownCard(text string) map[string]any {
	if text == "" {
		text = " "
	}
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{"wide_screen_mode": true},
		"body": map[string]any{
			"elements": []any{
				map[string]any{
					"tag":     "markdown",
					"content": text,
				},
			},
		},
	}
}

// 确保 larkevent 被使用（webhook 骨架后续会用到）。
var _ = larkevent.EventDecrypt
