package hooks

import "strings"

// truncateBytes 按字节截断（不切断 UTF-8 字符），返回 (结果, 是否截断)。
func truncateBytes(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	const suffix = "\n…[truncated]"
	cut := limit - len(suffix)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + suffix, true
}

// utf8Start 判断某字节是否是 UTF-8 字符的首字节（续字节形如 10xxxxxx）。
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// InjectionPlan 描述"注入到哪里"（供接线层使用；本包不关心消息树与 UI）。
type InjectionPlan struct {
	Event   Event
	System  []string // SessionStart：进 system 层（可被 prompt cache 覆盖，成本最低）
	UserMsg []string // UserPromptSubmit：追加到本轮 user 消息
	ToolOut []string // PostToolUse：追加到工具结果之后
	Compact []string // PreCompact / PostToolBatch：进压缩输入
}

// Plan 依事件把 Outcome 归位到注入点。
func (o Outcome) Plan(ev Event) InjectionPlan {
	p := InjectionPlan{Event: ev}
	for _, c := range o.Contexts {
		switch ev {
		case EventSessionStart:
			p.System = append(p.System, c)
		case EventUserPromptSubmit:
			p.UserMsg = append(p.UserMsg, c)
		case EventPostToolUse:
			p.ToolOut = append(p.ToolOut, c)
		case EventPreCompact, EventPostToolBatch:
			p.Compact = append(p.Compact, c)
		}
	}
	return p
}

// BuildInject 把多个上下文片段合成为最终注入文本（超限截断）。
func BuildInject(parts []string, limit int) (string, bool) {
	var kept []string
	for _, p := range parts {
		if strings.TrimSpace(p) == "" {
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == 0 {
		return "", false
	}
	return truncateBytes(strings.Join(kept, "\n\n"), limit)
}
