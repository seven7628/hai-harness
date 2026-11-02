// Package jsonwire 是「事件行」的字节级改写工具：bridge 转发 SDK 事件时要在 JSON 首部
// 注入宿主字段（session_id / workspace / cost_usd / todos…）。做成独立包是为了能**单元
// 测试** —— 这些函数原来住在 package main（`desktop/bridge` 的测试包因缺 helper 编不过）。
//
// 为什么要「先删再注入」（2026-11 用户报障的真实事故）：
// bridge 的注入是**前置**的（`{"session_id":"…","workspace":"…", …原对象}`），而
// events.TaskPromoted 这类事件**自己也带** `session_id` 字段（没有 omitempty，值为空串）。
// JSON 重复键的解析是**后者胜**：后端读到的是空串 → 该事件被路由到「当前打开的会话」
// → cybertron-gogroup 的「已摘为后台任务」出现在 agent-server-g 的转录里。
// 所以 InjectKeys 的调用方必须先 RemoveField 掉事件自带的同名键，只留宿主那一份。
package jsonwire

import (
	"encoding/json"
	"strings"
)

// KV 一个待注入的键值对；Value 已经是合法 JSON 字节（通常来自 json.Marshal，可缓存复用）。
type KV struct {
	Key   string
	Value []byte
}

// InjectKeys 把键值对注入 JSON 对象首部（保持合法 JSON）。
// 每个 kv.Value 必须是已序列化好的 JSON 字节（字符串来自 json.Marshal，可缓存复用）。
//
// 注意：注入是**前置**的，若 base 里本来就有同名键，读取方会拿到 base 里那份（重复键后者胜）。
// 所以「宿主要覆盖的键」必须先 RemoveField 再 InjectKeys —— 见包注释里的事故。
func InjectKeys(base []byte, kvs []KV) ([]byte, error) {
	if len(kvs) == 0 {
		return base, nil
	}
	out := make([]byte, 0, len(base)+128)
	out = append(out, '{')
	for i, kv := range kvs {
		if i > 0 {
			out = append(out, ',')
		}
		kb, _ := json.Marshal(kv.Key)
		out = append(out, kb...)
		out = append(out, ':')
		out = append(out, kv.Value...)
	}
	// 追加原对象内容（跳过首 { 与尾 }）
	rest := base
	if len(rest) > 0 && rest[0] == '{' {
		rest = rest[1:]
	}
	if len(rest) > 0 && rest[len(rest)-1] == '}' {
		rest = rest[:len(rest)-1]
	}
	if len(rest) > 0 {
		out = append(out, ',')
		out = append(out, rest...)
	}
	out = append(out, '}')
	return out, nil
}

// RemoveField 从 JSON 对象字节中删除指定键（顶层，**所有**同名出现都删）。返回 nil 表示无法安全处理
// （非对象/键不存在/结构异常——调用方回退 Unmarshal 路径）。
// 实现：扫描 `"key":` 的键值对并跳过其值（支持字符串/数字/对象/数组/布尔/null），
// 拼接剩余部分。仅处理顶层（不递归），值内嵌套同名键不影响（按引号/括号配对跳过）。
func RemoveField(raw []byte, key string) []byte {
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "{") {
		return nil
	}
	keyPat := `"` + key + `"`
	out := make([]byte, 0, len(raw))
	i := 0
	// 跳过首 {
	for i < len(trimmed) && trimmed[i] != '{' {
		i++
	}
	out = append(out, trimmed[i]) // {
	i++
	first := true
	for i < len(trimmed) {
		// 跳过空白与逗号
		for i < len(trimmed) && (trimmed[i] == ' ' || trimmed[i] == '\t' || trimmed[i] == '\n' || trimmed[i] == '\r') {
			i++
		}
		if i >= len(trimmed) {
			break
		}
		if trimmed[i] == ',' {
			i++
			continue
		}
		if trimmed[i] == '}' {
			break
		}
		// 读键名
		if trimmed[i] != '"' {
			return nil // 异常结构
		}
		keyStart := i
		i++
		for i < len(trimmed) && trimmed[i] != '"' {
			if trimmed[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(trimmed) {
			return nil
		}
		i++ // 跳过闭合引号
		keyEnd := i
		// 跳过空白，检查冒号
		for i < len(trimmed) && (trimmed[i] == ' ' || trimmed[i] == '\t' || trimmed[i] == '\n' || trimmed[i] == '\r') {
			i++
		}
		if i >= len(trimmed) || trimmed[i] != ':' {
			return nil
		}
		i++ // 跳过冒号
		for i < len(trimmed) && (trimmed[i] == ' ' || trimmed[i] == '\t' || trimmed[i] == '\n' || trimmed[i] == '\r') {
			i++
		}
		// 读值（支持嵌套）
		valEnd, ok := skipValue(trimmed, i)
		if !ok {
			return nil
		}
		// 判断是否目标键
		isTarget := string(trimmed[keyStart:keyEnd]) == keyPat
		if !isTarget {
			if !first {
				out = append(out, ',')
			}
			first = false
			out = append(out, trimmed[keyStart:valEnd]...)
		}
		i = valEnd
		// 跳过值后的空白与逗号（逗号在下一轮处理）
	}
	out = append(out, '}')
	return out
}

// skipJSONValue 跳过 JSON 值（字符串/数字/对象/数组/布尔/null），返回结束下标。
func skipValue(s string, i int) (int, bool) {
	if i >= len(s) {
		return i, false
	}
	switch s[i] {
	case '"': // 字符串
		i++
		for i < len(s) {
			if s[i] == '\\' {
				i += 2
				continue
			}
			if s[i] == '"' {
				return i + 1, true
			}
			i++
		}
		return i, false
	case '{', '[': // 对象/数组
		open := s[i]
		close := byte('}')
		if open == '[' {
			close = ']'
		}
		depth := 0
		inStr := false
		for i < len(s) {
			c := s[i]
			if inStr {
				if c == '\\' {
					i += 2
					continue
				}
				if c == '"' {
					inStr = false
				}
				i++
				continue
			}
			switch c {
			case '"':
				inStr = true
			case open:
				depth++
			case close:
				depth--
				if depth == 0 {
					return i + 1, true
				}
			}
			i++
		}
		return i, false
	default: // 数字/布尔/null（到逗号或 } 结束）
		start := i
		for i < len(s) && s[i] != ',' && s[i] != '}' && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' {
			i++
		}
		if i == start {
			return i, false
		}
		return i, true
	}
}
