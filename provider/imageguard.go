package provider

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"strings"

	"github.com/seven7628/hai-harness/core"

	// 图片头解码（DecodeConfig 只读头部）：png/jpeg/gif 注册。
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// imageHeadBytes data URL 正文里用于读尺寸的前缀长度（base64 字符数，取 64KiB 并保证
// 是 4 的倍数）。尺寸信息都在文件头（PNG 的 IHDR 在 16 字节处；JPEG 的 SOF 通常在
// 前 64KB 内），解码这一段就够，不把整张图搬进内存。
const imageHeadBytes = 64 << 10

// RepairMessages 请求组装前的确定性修复（三个协议入口都过这一关）：
//  1. SanitizeOversizeImages —— 单边超限的图片降级为文本（否则上游 400 杀整轮）；
//  2. core.NormalizeToolBatches —— 工具批次不变量（否则上游 400 杀整轮）。
//
// 都是幂等的纯函数：合法上下文原样返回（不分配），缓存前缀稳定。
func RepairMessages(msgs []core.Message) []core.Message {
	return core.NormalizeToolBatches(SanitizeOversizeImages(msgs))
}

// SanitizeOversizeImages 把单边超过 core.MaxImageSidePx 的图片块替换成文本说明。
//
// 为什么需要这一层（即使 tools 侧已经闸过）：图片来源不止 read_file —— MCP 工具的
// image 结果、computer 截图、IM 附件、codemode 的 image() 都直接进内容块，任何一处
// 漏判都会让上游以「unsupported image」400 掉整轮，而且报错文案会把人引向「格式
// 不支持」的死胡同（2026-11-04 真实事故：390x9697 的合法 PNG）。
//
// 只处理 data URL（能就地读尺寸）；远程 URL / 路径交给上游自取 —— 我们看不到尺寸，
// 不能凭空判死。尺寸读不出来（损坏/不支持的格式）也不在这里拦：那不是本函数的判据。
func SanitizeOversizeImages(msgs []core.Message) []core.Message {
	var out []core.Message // 惰性分配：没有超限图就原样返回（缓存前缀稳定）
	for i, m := range msgs {
		if out != nil {
			out = append(out, m)
			continue
		}
		fixed, changed := sanitizeMessageImages(m)
		if !changed {
			continue
		}
		if out == nil {
			out = make([]core.Message, 0, len(msgs))
			out = append(out, msgs[:i]...)
		}
		out = append(out, fixed)
	}
	if out == nil {
		return msgs
	}
	return out
}

// sanitizeMessageImages 处理单条消息的图片块，返回 (修复后消息, 是否有改动)。
func sanitizeMessageImages(m core.Message) (core.Message, bool) {
	changed := false
	blocks := make([]core.Content, len(m.Content))
	copy(blocks, m.Content)
	for i, c := range blocks {
		if c.Type != core.ContentTypeImage {
			continue
		}
		w, h, ok := imageDataURLSize(c.Content)
		if !ok || (w <= core.MaxImageSidePx && h <= core.MaxImageSidePx) {
			continue
		}
		blocks[i] = core.Content{
			Type: core.ContentTypeText,
			Content: fmt.Sprintf("[image omitted: %dx%d exceeds the %dpx single-side limit of the model gateway "+
				"(the gateway rejects such images with a misleading \"unsupported image\" error). "+
				"Re-capture it smaller: crop to the region you need, or split the page into sections.]",
				w, h, core.MaxImageSidePx),
		}
		changed = true
	}
	if !changed {
		return m, false
	}
	m.Content = blocks
	return m, true
}

// imageDataURLSize 从 data URL 的头部读图尺寸（只解码前缀，不解整图）。
// 非 data URL / 头部不可解 → ok=false（调用方不据此判死）。
func imageDataURLSize(url string) (w, h int, ok bool) {
	if !strings.HasPrefix(url, "data:") {
		return 0, 0, false
	}
	comma := strings.IndexByte(url, ',')
	if comma < 0 {
		return 0, 0, false
	}
	if !strings.HasSuffix(strings.ToLower(url[:comma]), ";base64") {
		return 0, 0, false // 百分号编码等非 base64 data URL：不处理
	}
	body := url[comma+1:]
	if len(body) > imageHeadBytes {
		body = body[:imageHeadBytes-len(body)%4] // 4 的倍数（合法 base64 前缀）
	}
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return 0, 0, false
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}
