package tools

import (
	"encoding/json"
	"strings"
)

// RepairArguments 语法级安全修复 LLM 返回的工具参数 JSON（jsonpair 修复）。
// 只修语法、不动语义：trim → 单引号→双引号 → 去尾逗号 → 补全未闭合的 {/[ 与字符串引号。
// 修出的结果必须是合法 JSON 才采用，否则原样返回（无法修复 → 引擎校验错误回传 LLM，
// 模型按「缺什么字段 / 非法 JSON」提示自行修正）。
//
// 安全边界：绝不做字段级语义推断（如裸字符串 → {path:...}）——猜错字段值会静默
// 执行错工具，比失败一轮更糟。修复失败一律回退原串走校验错误路径。
func RepairArguments(args string) string {
	s := strings.TrimSpace(args)
	if s == "" || json.Valid([]byte(s)) {
		return s
	}
	repaired := trimTrailingCommas(balanceJSON(repairQuotes(s)))
	if json.Valid([]byte(repaired)) {
		return repaired
	}
	return s
}

// repairQuotes 单引号 → 双引号：只在字符串边界转换（用原始引号类型决定边界，
// 保证「单引号串内嵌双引号」这类输入产出非法 JSON → 调用方回退，失败闭合）。
// 常见模型输出（{'path': 'x'}）可安全修复；"it's" 这类双引号串内单引号是内容、保留。
func repairQuotes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inStr := false
	quote := byte(0) // 打开当前字符串的原始引号类型（' 或 "）
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if escaped {
				b.WriteByte(c)
				escaped = false
			} else if c == '\\' {
				b.WriteByte(c)
				escaped = true
			} else if c == quote {
				b.WriteByte('"') // 闭合：统一输出双引号
				inStr = false
			} else {
				b.WriteByte(c)
			}
			continue
		}
		switch c {
		case '"', '\'':
			inStr = true
			quote = c
			b.WriteByte('"') // 统一输出双引号
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// trimTrailingCommas 删除 ] 或 } 前的逗号（可含空白）。
func trimTrailingCommas(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			j := i + 1
			for j < len(s) && isSpaceByte(s[j]) {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				continue // 跳过尾逗号
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// balanceJSON 补全未闭合的结构：栈记录嵌套的 }/]，末尾若在字符串内先补引号再依次闭合。
// 多余的闭合符（栈已空）忽略——修复目标是补缺而非改已有结构。
func balanceJSON(s string) string {
	var stack []byte
	inStr := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}':
			if len(stack) > 0 && stack[len(stack)-1] == '}' {
				stack = stack[:len(stack)-1]
			}
		case ']':
			if len(stack) > 0 && stack[len(stack)-1] == ']' {
				stack = stack[:len(stack)-1]
			}
		}
	}
	if inStr {
		s += `"` // 先闭合字符串
	}
	for i := len(stack) - 1; i >= 0; i-- {
		s += string(stack[i]) // 再逆序闭合结构
	}
	return s
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// missingFieldsHint 中央化「缺字段」提示：参数是合法 JSON 但缺 schema 必填字段时，
// 明确告诉 LLM 少了什么（全部工具的 Parameters() 都经 Obj(props, required...) 声明
// required，故无需改每个工具的手写校验器）。
func missingFieldsHint(tool Tool, arguments string) string {
	if !json.Valid([]byte(arguments)) {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(arguments), &obj); err != nil {
		return ""
	}
	required := requiredFields(tool.Parameters())
	if len(required) == 0 {
		return ""
	}
	var missing []string
	for _, f := range required {
		if _, ok := obj[f]; !ok {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return "missing required field(s): " + strings.Join(missing, ", ")
}

// requiredFields 从工具 schema（Parameters()）取 required 字段名数组。
// Parameters() 可能是 map（Obj/内联对象）或结构体（custom 工具）；非 map 则跳过提示。
// required 数组两种形态都处理：Obj() 与内联 map 均用 []string，防御性兼容 []any。
func requiredFields(params any) []string {
	m, ok := params.(map[string]any)
	if !ok {
		return nil
	}
	switch r := m["required"].(type) {
	case []string:
		return r
	case []any:
		out := make([]string, 0, len(r))
		for _, x := range r {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
