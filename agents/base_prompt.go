package agents

import "github.com/seven7628/hai-harness/agents/prompt"

// DefaultSystemPrompt 是 SDK 内置的基础系统提示词（行为契约层，字节最稳定）。
//
// 定位：只描述"harness 机制对模型行为的要求"——压缩行为、审批拒绝、诚实报告、
// 工具并行、语言跟随。刻意排除：
//
//   - 引擎机制本身（重试退避/截断/批预算由引擎静默处理，模型无需感知）；
//   - 输出渲染规范、环境信息、人格与角色（产品层 WithSystemPrompt 的职责；
//     CLI/桌面端模板见 docs/EXECUTION.md §3.6）；
//   - 项目级指令（工作记忆层 Agent.md）。
//
// 分层拼接序（稳定字节在前 = provider 前缀缓存命中最大化）：
//
//	层1a 本常量（SDK 拥有，最稳定）→ 层1b cfg.SystemPrompt（产品层）
//	→ 层1c cfg.WorkingDir（Current Workspace 环境层）→ 层2 Agent.md → 层3 技能清单
//
// 变更注意：内容变更会使前缀缓存失效（SDK 升级后首次请求），应保持精简稳定。
// 常量本体已迁移到 agents/prompt/base.go（提示词唯一定义地，见 docs/PROMPT_STANDARD.md），
// 此处保留导出别名以维持 SDK API 兼容（外部 import agents.DefaultSystemPrompt 不受影响）。
const DefaultSystemPrompt = prompt.DefaultSystemPrompt

// WithBaseSystemPrompt 替换 SDK 内置的基础系统提示词（DefaultSystemPrompt）。
// 传 "" 可禁用基础层（系统提示词完全由产品层 WithSystemPrompt 提供）。
// 区别于 WithSystemPrompt：后者是拼接在基础层之后的"产品层"内容（输出规范/
// 人格/环境），前者替换的是 SDK 拥有的"行为契约层"。
func WithBaseSystemPrompt(s string) Option {
	return func(c *Config) { c.BaseSystemPrompt = s }
}
