// Package owner 定义「隐式状态的归属维度」：一条状态到底属于谁。
//
// 为什么需要单独一个包（而不是各处拼字符串）：
//   - hooks 信任库要按 owner 存（agent / group / workspace）；
//   - IM 的启用状态与路由要按 owner 存；
//   - bridge 要从会话 id / 工作区路径派生 owner，并在两条链上给出一致答案。
//
// 三处各写一份前缀拼接必然漂移（同一个 agent 在信任库里是 agent:x、在 IM 里拼成
// hub.x.dm，跨表就再也对不上）。故：**编码只在这里定义一次**。
//
// 语法（大小写敏感，前缀后必须非空）：
//
//	workspace:<工作区绝对路径>   —— 普通项目会话（含该项目派生的 cron 会话）
//	agent:<agentID>             —— Agent Hub 智能体的 DM 会话
//	group:<groupID>             —— Agent Hub 群聊会话
//	legacy                      —— 只有 hook 信任库的**迁移兼容**会用到（见 hooks/trust.go）
//
// 刻意**不**做的事：
//   - 不做 owner 的「包含关系」（如 group 继承 agent）：产品已定「同一 agent 的 DM 与群聊
//     不共享授权」，任何隐式继承都会把这条决策悄悄推翻；
//   - 不做归一化/大小写折叠：agentID 与工作区路径都是大小写敏感的身份，改一个字符就是
//     另一个 owner（与信任库的内容哈希同风格：宁可多确认一次，不可少确认一次）。
package owner

import "strings"

// 前缀常量（唯一来源；禁止在别处硬编码）。
const (
	prefixWorkspace = "workspace:"
	prefixAgent     = "agent:"
	prefixGroup     = "group:"
	// Legacy 迁移期 owner：只用于读取旧版全局信任条目，**绝不**由写路径产生。
	Legacy = "legacy"
)

// Workspace 项目会话 owner。
func Workspace(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	return prefixWorkspace + path
}

// Agent 智能体 DM owner。
func Agent(id string) string {
	if strings.TrimSpace(id) == "" {
		return ""
	}
	return prefixAgent + id
}

// Group 群聊 owner。
func Group(id string) string {
	if strings.TrimSpace(id) == "" {
		return ""
	}
	return prefixGroup + id
}

// Kind 返回 owner 的维度：workspace | agent | group | legacy | ""（无法识别）。
func Kind(s string) string {
	switch {
	case strings.HasPrefix(s, prefixWorkspace) && len(s) > len(prefixWorkspace):
		return "workspace"
	case strings.HasPrefix(s, prefixAgent) && len(s) > len(prefixAgent):
		return "agent"
	case strings.HasPrefix(s, prefixGroup) && len(s) > len(prefixGroup):
		return "group"
	case s == Legacy:
		return "legacy"
	default:
		return ""
	}
}

// Valid 是否是合法且**可写入**的 owner（legacy 只读，不算）。
func Valid(s string) bool {
	switch Kind(s) {
	case "workspace", "agent", "group":
		return true
	default:
		return false
	}
}

// ID 取 owner 的标识部分（workspace 取路径，agent/group 取 id）。无法识别返回空串。
func ID(s string) string {
	for _, p := range []string{prefixWorkspace, prefixAgent, prefixGroup} {
		if strings.HasPrefix(s, p) {
			return s[len(p):]
		}
	}
	return ""
}

// IsWorkspace 是否项目工作区 owner（hook 信任的 legacy 兼容只对这一维生效）。
func IsWorkspace(s string) bool { return Kind(s) == "workspace" }

// FromSessionID 从会话 id 派生 owner（**Hub 会话 id 是身份真相源**）：
//
//	hub.<agent>.dm          → agent:<agent>
//	hub.<agent>.g.<group>   → group:<group>
//	其它（项目会话 / cron 派生会话）→ workspace:<workspace>
//
// 为什么群聊取 group 而不是 (agent, group)：产品决策是「A 群的信任/启用只作用于 A 群」，
// 同一群里的多个 agent 共享该群的授权（否则同一条群消息会被各 agent 分别判一次，
// 用户体验上等于每条都要确认 N 次），而不同群的 owner 严格隔离。
func FromSessionID(sessionID, workspace string) string {
	const prefix = "hub."
	if strings.HasPrefix(sessionID, prefix) {
		rest := sessionID[len(prefix):]
		if i := strings.Index(rest, ".g."); i >= 0 {
			if g := rest[i+len(".g."):]; g != "" {
				return Group(g)
			}
			return ""
		}
		agent := strings.TrimSuffix(rest, ".dm")
		if agent != "" && agent != rest { // 必须以 .dm 结尾
			return Agent(agent)
		}
		return ""
	}
	return Workspace(workspace)
}

// Label 人类可读的短标签（设置页/日志展示用，不参与判定）。
func Label(s string) string {
	switch Kind(s) {
	case "workspace":
		return "项目会话（" + ID(s) + "）"
	case "agent":
		return "智能体 " + ID(s)
	case "group":
		return "群聊 " + ID(s)
	case "legacy":
		return "迁移前的全局信任"
	default:
		return s
	}
}
