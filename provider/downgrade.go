package provider

import "github.com/seven7628/hai-harness/core"

// 图片降级占位文本（pi 的 NON_VISION_USER_IMAGE_PLACEHOLDER / NON_VISION_TOOL_IMAGE_PLACEHOLDER 同款）。
const (
	UserImagePlaceholder = "(image omitted: model does not support images)"
	ToolImagePlaceholder = "(tool image omitted: model does not support images)"
)

// ReplaceImagesWithPlaceholder 把内容块中的图片替换为占位文本。
// 连续多图合并为一个占位（pi 的 previousWasPlaceholder 逻辑）：N 张图 → 1 个占位，避免刷屏。
// 返回新切片，不修改入参。
func ReplaceImagesWithPlaceholder(blocks []core.Content, placeholder string) []core.Content {
	out := make([]core.Content, 0, len(blocks))
	prevWasPlaceholder := false
	for _, c := range blocks {
		if c.Type == core.ContentTypeImage {
			if !prevWasPlaceholder {
				out = append(out, core.Content{Type: core.ContentTypeText, Content: placeholder})
			}
			prevWasPlaceholder = true
			continue
		}
		out = append(out, c)
		prevWasPlaceholder = c.Type == core.ContentTypeText && c.Content == placeholder
	}
	return out
}

// DowngradeImages 按模型是否支持图片降级消息（翻译层入口调用；Registry 方法，依赖注入）。
// 决策 #4：统一用 ReplaceImagesWithPlaceholder（连续图合并一个占位，对齐 pi）。
// 不支持图片的模型：user 消息与 tool 消息里的 image 块 → 占位文本；其余消息原样。
// 返回新切片，不修改入参。
func (r *Registry) DowngradeImages(provider, model string, messages []core.Message) []core.Message {
	if r.SupportsImageInput(provider, model) {
		return messages
	}
	out := make([]core.Message, len(messages))
	for i, m := range messages {
		hasImage := false
		for _, c := range m.Content {
			if c.Type == core.ContentTypeImage {
				hasImage = true
				break
			}
		}
		if !hasImage {
			out[i] = m
			continue
		}
		ph := UserImagePlaceholder
		if m.Role == core.Tool {
			ph = ToolImagePlaceholder
		}
		out[i] = m
		out[i].Content = ReplaceImagesWithPlaceholder(m.Content, ph)
	}
	return out
}
