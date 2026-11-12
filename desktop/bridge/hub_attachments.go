package main

// hub_attachments.go：Hub DM/群消息的附件协议（图片来源进模型上下文）。
//
// 前端把文件引用拼成 `@绝对路径` 放进正文，后端**不读文件**：这里只做协议解析、
// 校验与图片块装配。纯图片消息（正文为空）用占位正文 `[图片]` 落账 —— 群快照/
// perception 依赖正文非空判断「可用消息」，占位符让纯图消息不被当成空消息丢掉。

import (
	"encoding/json"
	"fmt"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
)

const (
	// hubImageOnlyText 纯图片消息的占位正文（前端可识别并隐藏该气泡文字）。
	hubImageOnlyText = "[图片]"
	// hubFileOnlyText 纯文件引用消息的占位正文（群消息允许只有附件）。
	hubFileOnlyText = "[文件]"
)

// hubAttachmentsFromPayload 解析 payload.attachments（JSON 数组）。
// 缺字段/为 null → nil（旧消息行为完全不变）；类型不对 → ErrInvalid。
func hubAttachmentsFromPayload(payload map[string]any) ([]agenthub.Attachment, error) {
	raw, ok := payload["attachments"]
	if !ok || raw == nil {
		return nil, nil
	}
	if atts, ok := raw.([]agenthub.Attachment); ok {
		return atts, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: attachments 必须是附件对象数组", agenthub.ErrInvalid)
	}
	var atts []agenthub.Attachment
	if err := json.Unmarshal(b, &atts); err != nil {
		return nil, fmt.Errorf("%w: attachments 必须是附件对象数组", agenthub.ErrInvalid)
	}
	return atts, nil
}

// hubParseAttachments 解析并校验附件；任何失败都不落账。
func hubParseAttachments(payload map[string]any) ([]agenthub.Attachment, error) {
	atts, err := hubAttachmentsFromPayload(payload)
	if err != nil {
		return nil, err
	}
	if err := agenthub.ValidateAttachments(atts); err != nil {
		return nil, err
	}
	return atts, nil
}

// hubHasImageAttachment 判断是否有图片附件（纯图消息允许正文为空）。
func hubHasImageAttachment(atts []agenthub.Attachment) bool {
	for _, a := range atts {
		if a.IsImage() {
			return true
		}
	}
	return false
}

// hubAttachmentImageBlocks 把 image 附件转成模型内容块。
// 文件附件不进上下文：前端已把 `@路径` 放进正文，由模型按需读取。
func hubAttachmentImageBlocks(atts []agenthub.Attachment) []core.Content {
	var out []core.Content
	for _, a := range atts {
		if !a.IsImage() || a.DataURL == "" {
			continue
		}
		out = append(out, core.Content{
			Type:     core.ContentTypeImage,
			Content:  a.DataURL,
			MimeType: a.ImageMIME(),
		})
	}
	return out
}

// hubUserContentBlocks 组装一条人类消息的模型内容：非空正文一个 text 块 + 每个
// 图片一个 image 块。纯图消息没有 text 块（正文只是落账占位）。
func hubUserContentBlocks(text string, atts []agenthub.Attachment) []core.Content {
	var blocks []core.Content
	if text != "" {
		blocks = append(blocks, core.Content{Type: core.ContentTypeText, Content: text})
	}
	return append(blocks, hubAttachmentImageBlocks(atts)...)
}

// hubDisplayText 返回落账/展示用的正文：正文为空时按附件类型给占位。
// 纯图必须给占位，否则快照/perception 会把这条消息当成空消息（不可用）。
func hubDisplayText(text string, atts []agenthub.Attachment) string {
	if text != "" {
		return text
	}
	if hubHasImageAttachment(atts) {
		return hubImageOnlyText
	}
	if len(atts) > 0 {
		return hubFileOnlyText
	}
	return ""
}
