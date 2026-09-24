package im

import "strings"

// i18n 轻量国际化：IM 消息文案（命令结果通知/工具调用信息）。
// 默认中文；后续可扩展按 Gateway 配置/用户语言切换。
//
// 设计：集中文案表 + 简单插值（{name} 占位符）。新增文案在此登记，
// 保证 IM 侧与桌面端 UI 文案分离（桌面端用前端 i18n，这里独立）。

// Lang 语言。
type Lang string

const (
	LangZH Lang = "zh"
	LangEN Lang = "en"
)

// ToolMsg 工具调用通知文案模板（ToolResponse 呈现）。
type ToolMsg struct {
	OK      string // 成功（含工具名）
	Err     string // 出错（含工具名 + 错误码）
	Changed string // 变更统计（含路径 + 增删行）
	Dur     string // 耗时
	Running string // 执行中（ToolStart）
}

// ApprovalMsg 审批卡片文案。
type ApprovalMsg struct {
	Request string // "{name} 请求执行"
	Approve string // 批准按钮
	Reject  string // 拒绝按钮
	Later   string // 稍后按钮
	Title   string // 卡片标题 "待你批准 · {name}"
}

// toolMsgs 文案表。
var toolMsgs = map[Lang]ToolMsg{
	LangZH: {
		OK:      "{icon} **{name}** ✓",
		Err:     "{icon} **{name}** ⚠️ 出错",
		Changed: "变更 `{path}` +{add} −{del}",
		Dur:     "⏱ {dur}",
		Running: "{icon} **{name}** …",
	},
	LangEN: {
		OK:      "{icon} **{name}** ✓",
		Err:     "{icon} **{name}** ⚠️ failed",
		Changed: "changed `{path}` +{add} −{del}",
		Dur:     "⏱ {dur}",
		Running: "{icon} **{name}** …",
	},
}

// approvalMsgs 审批文案表。
var approvalMsgs = map[Lang]ApprovalMsg{
	LangZH: {
		Request: "**{name}** 请求执行",
		Approve: "✓ 批准",
		Reject:  "✗ 拒绝",
		Later:   "稍后",
		Title:   "待你批准 · {name}",
	},
	LangEN: {
		Request: "**{name}** requests execution",
		Approve: "✓ Approve",
		Reject:  "✗ Reject",
		Later:   "Later",
		Title:   "Approval needed · {name}",
	},
}

// runErrMsgs 运行错误文案。
var runErrMsgs = map[Lang]string{
	LangZH: "运行出错",
	LangEN: "run error",
}

// renderToolMsg 渲染文案模板（{key} 插值）。
func renderToolMsg(tmpl string, vars map[string]string) string {
	out := tmpl
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{"+k+"}", v)
	}
	return out
}
