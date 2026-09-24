package provider

import (
	"strings"
	"unicode/utf8"
)

// SanitizeSurrogates 移除未配对的 Unicode 代理项（对齐 pi sanitizeSurrogates）。
// 未配对代理项（高代理 0xD800-0xDBFF 无配对低代理，或反之）会导致很多 API 提供方
// JSON 序列化失败（400）。合法 emoji 等 BMP 外字符使用正确配对的代理项，不受影响。
func SanitizeSurrogates(text string) string {
	if !hasUnpairedSurrogate(text) {
		return text
	}
	// Go 的 string 是 UTF-8；未配对代理项在 UTF-8 里是无效序列（RuneError）。
	// 逐 rune 检查：合法 rune 保留，RuneError 且原始字节是代理项编码 → 丢弃。
	var b strings.Builder
	b.Grow(len(text))
	for len(text) > 0 {
		r, size := utf8.DecodeRuneInString(text)
		if r == utf8.RuneError && size == 1 {
			// 单字节 RuneError：可能是真 0xFFFD 或非法字节。检查是否为代理项编码
			//（UTF-8 中代理项是 3 字节 ED A0-BF 80-BF，DecodeRuneInString 返回 RuneError,1）。
			// 命中 → 删除完整 3 字节（S4 修复：原实现只删 1 字节留 2 字节残片 → U+FFFD）。
			if isSurrogateEncoding(text) {
				text = text[3:]
				continue
			}
			b.WriteByte(text[0])
			text = text[1:]
			continue
		}
		b.WriteString(text[:size])
		text = text[size:]
	}
	return b.String()
}

// isSurrogateEncoding 判断 text 开头是否为 UTF-8 编码的代理项（ED A0-BF ..）。
func isSurrogateEncoding(text string) bool {
	if len(text) < 3 || text[0] != 0xED {
		return false
	}
	b1 := text[1]
	if b1 < 0xA0 || b1 > 0xBF {
		return false
	}
	b2 := text[2]
	return b2 >= 0x80 && b2 <= 0xBF
}

// hasUnpairedSurrogate 快速预检：含 0xED 开头序列才需要处理。
func hasUnpairedSurrogate(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] == 0xED {
			return true
		}
	}
	return false
}
