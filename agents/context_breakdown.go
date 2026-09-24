package agents

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/seven7628/hai-harness/core"
)

// 上下文构成分区的稳定键（前端按 key 展示与配色；跨版本保持字面量不变）。
const (
	ContextPartSystem   = "system_prompt" // 行为契约 + 产品层 + 工作记忆（Agent.md）+ 模式提示
	ContextPartSkills   = "skills"        // 技能清单（系统提示词末段，单独计量）
	ContextPartTools    = "tools"         // 内置工具 schema
	ContextPartMCP      = "mcp"           // MCP 工具 schema（mcp__<server>__<tool>）
	ContextPartMessages = "messages"      // 会话消息（不含系统提示词）
	ContextPartOther    = "other"         // 其他：subagent 清单 + 每条消息的框架开销（静态侧，随消息数微增）
)

// contextPartOrder 分区输出序（稳定：前端行序按占用排序前先有确定骨架）。
var contextPartOrder = []string{
	ContextPartSystem, ContextPartSkills, ContextPartTools,
	ContextPartMCP, ContextPartMessages, ContextPartOther,
}

// mcpToolPrefix MCP 工具在引擎内的名字前缀（与 mcp.Tool.EngineName 同约定，
// Claude Code 同款 mcp__<server>__<tool>）。此处不 import mcp 包：agents 只做
// 名字前缀判定，避免为一条分类规则引入 mcp → tools 依赖链。
const mcpToolPrefix = "mcp__"

// messageFrameChars 单条消息的框架开销（role / 分隔符 / 包装标记）折算字符数。
// 与字符/token 口径一致（4 字符 ≈ 1 token）：一条消息的 role 与边界标记约 4 token。
// 计入「其他」分区而不是消息分区——它不属于消息内容，避免消息占比被系统性高估。
const messageFrameChars = 16

// ContextPart 一个分区的估算占用。Chars 是唯一事实源，Tokens 为其 /4 折算。
type ContextPart struct {
	Key    string `json:"key"`
	Chars  int64  `json:"chars"`
	Tokens int64  `json:"tokens"` // Chars/4（口径同 estimateMessagesTokens / estimator 冷启动）
}

// ContextBreakdown 当前「下一次请求」的上下文构成估算（ctx 占用面板数据源）。
//
// 为什么是估算：provider 只按请求回报总量（usage.Input），不按分区回报。分区只能
// 由 SDK 侧「请求组装口径」反推（系统提示词分层字节、工具 schema JSON 字节、消息
// 字符数），再按 /4 折算 token。总量与「消息上下文」由调用方结算（见 bridge
// ctxBreakdownPayload）：静态分区（系统提示词/技能/工具/MCP/其他）按此处的估算
// **固定**展示，消息分区取「真实锚点 − 静态合计」的残差 —— 这里只负责如实量出
// 静态部分的字节，不再参与任何按比例摊派。
type ContextBreakdown struct {
	Parts        []ContextPart `json:"parts"`         // 恒含全部分区键（无内容时为 0）
	TotalChars   int64         `json:"total_chars"`   // 各分区字符合计
	TotalTokens  int64         `json:"total_tokens"`  // 各分区 /4 估算合计
	MessageCount int           `json:"message_count"` // 计量的消息条数（框架开销的依据）
}

// RequestTokens 估算「下一次请求」的上下文总量（请求组装口径）：系统提示词 + 技能清单 +
// 子 agent 清单 + 工具/MCP schema + 消息内容 + 每条消息框架开销。
//
// 与 ctx 分区面板同一估算源（ContextBreakdown.TotalTokens），供压缩后的 ctx 锚点使用：
// 只算消息会漏掉工具 schema（本仓库实测 ~6k token），压后占用会系统性低于下一轮真实
// 输入，展示层（ctx 环）就会给出与真实 usage 对不上的「估算锚点」。装配缺失时退化为
// 消息字符估算 —— 压缩路径的锚点绝不能退化成 0（0 会被当作「缺数」）。
func (a *AgentLoop) RequestTokens(ctx context.Context, messages []core.Message, mode *Mode) int64 {
	if bd := a.ContextBreakdown(ctx, messages, mode); bd.TotalTokens > 0 {
		return bd.TotalTokens
	}
	return estimateMessagesTokens(messages)
}

// ContextBreakdown 估算下一次请求的上下文构成。
//
// messages 传会话当前有效上下文（Session.Messages）：首条为 system 时**跳过**——
// 它的内容与「系统提示词/技能」分区重复（下一轮请求会由 composeSystemPrompt 重建，
// 且可能因 Agent.md / 技能增删与历史不同），重复计量会把系统提示词算两遍。
// mode 传当前模式（nil = 不过滤工具），与 streamOnce 的请求组装口径保持一致：
// 模式不可见的工具不会进请求，也不该进面板。
//
// 与真实请求的差异（都是小量，落在「其他」分区的容差内）：系统提示词按当前配置
// 实时组装的字节数计（而非历史里那条 system 消息）；@引用展开、ephemeral 提醒
// （GoalAlignmentReminder / PostToolNudge）属运行期注入，此处不计。
func (a *AgentLoop) ContextBreakdown(ctx context.Context, messages []core.Message, mode *Mode) ContextBreakdown {
	// —— 系统提示词 / 技能 / 其他：按 composeSystemPrompt 的分层口径拆分 ——
	system := a.composeSystemPrompt()
	skillsManifest := ""
	if a.cfg.Skills != nil {
		skillsManifest = buildSkillManifest(a.cfg.Skills)
	}
	subagentManifest := ""
	if a.cfg.Subagents != nil {
		subagentManifest = buildSubagentManifest(a.cfg.Subagents)
	}
	if mode != nil && mode.SystemHint != "" {
		if system != "" {
			system += "\n\n"
		}
		system += mode.SystemHint
	}
	// 两个清单是拼进系统提示词的连续字节，直接减去即得「纯提示词」部分。
	systemChars := int64(len(system)) - int64(len(skillsManifest)) - int64(len(subagentManifest))
	if systemChars < 0 {
		systemChars = 0
	}

	// —— 消息：跳过首条 system（已由系统提示词分区计量）——
	msgs := messages
	if len(msgs) > 0 && msgs[0].Role == core.System {
		msgs = msgs[1:]
	}
	messageCharsTotal := messagesChars(msgs)

	// —— 工具 / MCP：schema 的 JSON 字节（provider 原样发送 Name/Description/Parameters）——
	var toolChars, mcpChars int64
	for _, schema := range a.toolSchemas(ctx, mode) {
		b, err := json.Marshal(schema)
		if err != nil {
			continue // 不可序列化的 schema 不会进请求，跳过
		}
		if strings.HasPrefix(schema.Name, mcpToolPrefix) {
			mcpChars += int64(len(b))
			continue
		}
		toolChars += int64(len(b))
	}

	chars := map[string]int64{
		ContextPartSystem:   systemChars,
		ContextPartSkills:   int64(len(skillsManifest)),
		ContextPartTools:    toolChars,
		ContextPartMCP:      mcpChars,
		ContextPartMessages: messageCharsTotal,
		ContextPartOther:    int64(len(subagentManifest)) + int64(len(msgs))*messageFrameChars,
	}

	out := ContextBreakdown{Parts: make([]ContextPart, 0, len(contextPartOrder)), MessageCount: len(msgs)}
	for _, key := range contextPartOrder {
		c := chars[key]
		out.Parts = append(out.Parts, ContextPart{Key: key, Chars: c, Tokens: c / 4})
		out.TotalChars += c
		out.TotalTokens += c / 4
	}
	return out
}
