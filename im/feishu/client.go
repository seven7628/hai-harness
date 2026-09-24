// Package feishu 实现飞书（Lark）IM Gateway。
//
// 接入形态（docs/IM_INTEGRATION.md §7）：
//   - 应用机器人（企业自建应用）：开启机器人能力，申请 im:message / im:message:send_as_bot 权限
//   - 接收：官方 SDK 长连接（larkws）或 Webhook（larkevent，需公网 + 签名/加密）
//   - 发送：官方 SDK client.Im.V1.Message.Create（内部处理 tenant_access_token）
//   - 审批：interactive 卡片 + card.action.trigger 回调（larkcard 处理）
//
// 实现策略：**Interface 在 github.com/seven7628/hai-harness/im 包定义（业务零感知），本包内部用官方 SDK
// （larksuite/oapi-sdk-go/v3）驱动**——适配层隔离，换库只动本包（同 mcp 包原则）。
package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// Type Gateway 类型标识。
const Type = "feishu"

// Config 飞书 Gateway 配置（对应 im-config.json 里 type=feishu 的 config 段）。
type Config struct {
	AppID       string `json:"app_id"`                 // 应用 App ID（开放平台 → 凭证与基础信息）
	AppSecret   string `json:"app_secret"`             // 应用 App Secret
	EncryptKey  string `json:"encrypt_key,omitempty"`  // 事件加密 key（Webhook 模式必填）
	VerifyToken string `json:"verify_token,omitempty"` // 验证令牌（Webhook 模式可选）
	Mode        string `json:"mode"`                   // websocket（默认）| webhook
	BaseURL     string `json:"base_url,omitempty"`     // 自定义域名（默认 open.feishu.cn）
}

// Normalize 归一化配置。
func (c Config) Normalize() Config {
	c.AppID = strings.TrimSpace(c.AppID)
	c.AppSecret = strings.TrimSpace(c.AppSecret)
	c.EncryptKey = strings.TrimSpace(c.EncryptKey)
	c.VerifyToken = strings.TrimSpace(c.VerifyToken)
	if c.Mode == "" {
		c.Mode = "websocket"
	}
	if c.BaseURL == "" {
		c.BaseURL = "https://open.feishu.cn"
	}
	return c
}

// Validate 校验必填。返回归一化配置。
func (c Config) Validate() (Config, error) {
	c = c.Normalize()
	if c.AppID == "" || c.AppSecret == "" {
		return c, fmt.Errorf("feishu: app_id / app_secret 必填")
	}
	if c.Mode != "websocket" && c.Mode != "webhook" {
		return c, fmt.Errorf("feishu: mode 必须是 websocket 或 webhook")
	}
	if c.Mode == "webhook" && c.EncryptKey == "" {
		return c, fmt.Errorf("feishu: webhook 模式需要 encrypt_key")
	}
	return c, nil
}

// sdkClient 官方 SDK 客户端封装（发消息 + 更新卡片）。
// 生命周期与 Gateway 一致；内部自动管理 tenant_access_token 缓存。
type sdkClient struct {
	client *lark.Client
}

// newSDKClient 创建 SDK 客户端。
func newSDKClient(cfg Config, httpClient *http.Client) (*sdkClient, error) {
	opts := []lark.ClientOptionFunc{
		lark.WithEnableTokenCache(true),
	}
	if httpClient != nil {
		opts = append(opts, lark.WithHttpClient(httpClient))
	}
	if cfg.BaseURL != "" && cfg.BaseURL != "https://open.feishu.cn" {
		opts = append(opts, lark.WithOpenBaseUrl(cfg.BaseURL))
	}
	cli := lark.NewClient(cfg.AppID, cfg.AppSecret, opts...)
	return &sdkClient{client: cli}, nil
}

// sendText 发文本消息（receiveID 为 chat_id）。
func (s *sdkClient) sendText(ctx context.Context, receiveID, text string) (string, error) {
	content, _ := json.Marshal(map[string]string{"text": text})
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(receiveID).
			MsgType("text").
			Content(string(content)).
			Build()).
		Build()
	resp, err := s.client.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("feishu: send text: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("feishu: send text code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.MessageId == nil {
		return "", fmt.Errorf("feishu: send text 无 message_id")
	}
	return *resp.Data.MessageId, nil
}

// sendPost 发 post 富文本消息（KindMarkdown）。用 lark_md 标签渲染 markdown 语法
// （**加粗** / `行内代码` / [链接]）；text 标签会原样显示不渲染。
func (s *sdkClient) sendPost(ctx context.Context, receiveID, text string) (string, error) {
	// post 富文本：lark_md 标签支持 **bold** / `code` / [link] 等基础渲染
	content, _ := json.Marshal(map[string]any{
		"zh_cn": map[string]any{
			"content": [][]map[string]any{
				{{"tag": "lark_md", "text": text}},
			},
		},
	})
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(receiveID).
			MsgType("post").
			Content(string(content)).
			Build()).
		Build()
	resp, err := s.client.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("feishu: send post: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("feishu: send post code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.MessageId == nil {
		return "", fmt.Errorf("feishu: send post 无 message_id")
	}
	return *resp.Data.MessageId, nil
}

// sendInteractive 发交互卡片（新版卡片 schema 2.0：content 直接是卡片 JSON）。
func (s *sdkClient) sendInteractive(ctx context.Context, receiveID string, card any) (string, error) {
	content, _ := json.Marshal(card)
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(receiveID).
			MsgType("interactive").
			Content(string(content)).
			Build()).
		Build()
	resp, err := s.client.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("feishu: send card: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("feishu: send card code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.MessageId == nil {
		return "", fmt.Errorf("feishu: send card 无 message_id")
	}
	return *resp.Data.MessageId, nil
}

// updateCard 更新已发卡片消息（新版 schema 2.0：content 直接是卡片 JSON）。
func (s *sdkClient) updateCard(ctx context.Context, messageID string, card any) error {
	content, _ := json.Marshal(card)
	req := larkim.NewPatchMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().
			Content(string(content)).
			Build()).
		Build()
	resp, err := s.client.Im.V1.Message.Patch(ctx, req)
	if err != nil {
		return fmt.Errorf("feishu: update card: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("feishu: update card code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

// ChatInfo 群信息（im_chat_list 数据源）。
type ChatInfo struct {
	ChatID string `json:"chat_id"`
	Name   string `json:"name"`
}

// ListChats 获取机器人所在的群列表（im/v1/chats；供路由绑定下拉选择）。
func (s *sdkClient) ListChats(ctx context.Context) ([]ChatInfo, error) {
	var out []ChatInfo
	var pageToken string
	for {
		req := larkim.NewListChatReqBuilder().
			SortType(larkim.ListChatSortTypeByCreateTimeAsc).
			PageSize(50).
			PageToken(pageToken).
			Build()
		resp, err := s.client.Im.V1.Chat.List(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("feishu: list chats: %w", err)
		}
		if !resp.Success() {
			return nil, fmt.Errorf("feishu: list chats code=%d msg=%s", resp.Code, resp.Msg)
		}
		if resp.Data != nil {
			for _, item := range resp.Data.Items {
				if item != nil && item.ChatId != nil {
					name := ""
					if item.Name != nil {
						name = *item.Name
					}
					out = append(out, ChatInfo{ChatID: *item.ChatId, Name: name})
				}
			}
			if resp.Data.HasMore != nil && *resp.Data.HasMore && resp.Data.PageToken != nil {
				pageToken = *resp.Data.PageToken
				continue
			}
		}
		break
	}
	return out, nil
}
