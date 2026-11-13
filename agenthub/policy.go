package agenthub

// policy.go：工具面的运行时判定（引擎装配时用）。
//
// 设计口径（D18）：**零值 = 全开**。安全靠 cwd/沙箱/审批/预算，不靠默认隐藏工具；
// 新增工具默认可见，不会因为忘记加字段而被静默关闭。
//
// 匹配规则只有两种：**精确名**或**尾随 `*` 的前缀**（`browser_*`、`mcp__github__*`）。
// 刻意不支持正则/中间通配 —— 配置来自用户手写与 UI 开关，两种形式足够且可解释；
// 写错的多半整条不匹配，不会突然关掉一片工具。
//
// 只读预设计算需要工具自身声明（tools.ReadOnlyTool），而 agenthub 是领域层、
// 不 import 执行层：由调用方把 readOnly 传进来（见 ToolVisible）。

import "strings"

// ToolVisible 某工具在本 Profile 下是否可见。
// readOnly 由调用方取工具自身的只读声明（未声明 = false，保守）。
func (p ToolPolicy) ToolVisible(name string, readOnly bool) bool {
	name = strings.TrimSpace(name)
	switch p.Preset {
	case "", ToolPresetAll:
		// 全开，继续看 Disable
	case ToolPresetReadonly:
		// 只读预设 = 只保留自身声明只读的工具。**不看 Allow**：Validate 禁止
		// readonly + Allow，若这里还认 Allow，一条绕过校验的加载路径就能把
		// readonly 扩成可写（P1-01 复核的 non-blocking）。
		if !readOnly {
			return false
		}
	case ToolPresetCustom:
		if !MatchesPatterns(p.Allow, name) {
			return false
		}
	default:
		// 非法 preset（Validate 会拒）：按最保守的自定义白名单处理
		if !MatchesPatterns(p.Allow, name) {
			return false
		}
	}
	return !MatchesPatterns(p.Disable, name)
}

// MCPAllowed 做 server/tool 级判定：server 是否启用、tool 是否被开关或不在白名单。
func (p ToolPolicy) MCPAllowed(server, tool string) bool {
	m := p.MCP
	if m.Disable {
		return false
	}
	if MatchesPatterns(m.DisableServers, server) {
		return false
	}
	full := server + "." + tool
	if len(m.AllowTools) > 0 && !MatchesPatterns(m.AllowTools, full) {
		return false
	}
	return !MatchesPatterns(m.DisableTools, full)
}

// SplitMCPToolName 拆引擎内的 MCP 工具名 mcp__<server>__<tool>（只作兜底：
// 真实 MCP 工具实现 MCPServerName/MCPToolName 访问器，策略应优先用它）。
//
// 语义：在**第一个** "__" 处切分，server 里不允许再出现 "__"（那会让切分歧义，
// 按非 MCP 处理）；tool 里允许 "__"（MCP 工具名可以带双下划线，如 "a__b"）。
// 之前「tool 含 __ 就返回 ok=false」会让这类工具的 Disable/AllowTools 整体失效
// （P1-01 复核的 blocking），所以宁可把余下整段当 tool。
func SplitMCPToolName(name string) (server, tool string, ok bool) {
	const prefix = "mcp__"
	if !strings.HasPrefix(name, prefix) {
		return "", "", false
	}
	rest := name[len(prefix):]
	i := strings.Index(rest, "__")
	if i <= 0 || i+2 >= len(rest) {
		return "", "", false
	}
	server, tool = rest[:i], rest[i+2:]
	if strings.Contains(server, "__") || server == "" || tool == "" {
		return "", "", false
	}
	return server, tool, true
}

// MCPRestricted 该 Profile 是否对 MCP 做过任何限制。
// 用于「名字反解有歧义」时的保守判定（见 MCPNameAmbiguous）：没有任何限制时，
// 歧义不影响结果；有限制时宁可拒绝，也不让限制被名字歧义静默绕过。
func (p ToolPolicy) MCPRestricted() bool {
	m := p.MCP
	return m.Disable || len(m.DisableServers) > 0 || len(m.AllowTools) > 0 || len(m.DisableTools) > 0
}

// MCPNameAmbiguous 名字 mcp__<server>__<tool> 是否无法无歧义反解。
//
// EngineName 是裸拼接 "mcp__" + server + "__" + tool：server 自带 "__" 时边界分不清
// （"my__server" + "search" 与 "my" + "server__search" 同名）。实现 MCPServerName /
// MCPToolName 访问器的适配器不受影响；只影响「只有名字」的第三方工具。
func MCPNameAmbiguous(name string) bool {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return false
	}
	return strings.Count(rest, "__") > 1
}

// MatchesPatterns 精确名或尾随 * 的前缀匹配；patterns 为空 = 不匹配。
func MatchesPatterns(patterns []string, name string) bool {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(name, strings.TrimSuffix(p, "*")) {
				return true
			}
			continue
		}
		if p == name {
			return true
		}
	}
	return false
}
