package memory

import "strings"

// memoryBlocks 从 `## Memory` 段里切出每个 <memory> 块（测试辅助）。
func memoryBlocks(text string) []string {
	parts := strings.Split(text, "<memory>")
	out := make([]string, 0, len(parts))
	for _, p := range parts[1:] {
		out = append(out, p)
	}
	return out
}

// blockField 取块里某个字段的值（单行字段，取到行尾）。
func blockField(block, key string) string {
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(line, key+": ") {
			return strings.TrimPrefix(line, key+": ")
		}
	}
	return ""
}
