package core

import (
	"errors"
	"fmt"
	"strings"
)

type MessageRole string

const (
	User      MessageRole = "user"
	Tool      MessageRole = "tool"
	Assistant MessageRole = "assistant"
	System    MessageRole = "system"
)

// 内容块类型常量（Content.Type）：全栈共享，避免裸字符串散落。
const (
	ContentTypeText    = "text"    // 文本块
	ContentTypeImage   = "image"   // 图片块（URL / data URL）
	ContentTypeSummary = "summary" // 压缩摘要块（system 角色，仅内部压缩产物）
	// ContentTypeCommand 内部指令消息（命令框架，仅 session.Command 经 inbox 注入；
	// 不持久化、不送 LLM——消费点在 agents 注入段剥出，见 agents/commands.go）
	ContentTypeCommand = "command"
	// ContentTypeTaskResult 后台任务完成消息（仅 Session.PushTaskResult 注入）：
	// user 角色 + 非 text 块 —— provider 按文本翻译送 LLM（模型看到 Markdown 结果续跑），
	// collectUserTexts 只认 text 块所以不被当作「用户输入」上报。
	ContentTypeTaskResult = "task_result"
	// ContentTypeSkill 用户主动加载的技能指令块（命令框架：/技能名 与技能面板「加载」，
	// 见 agents/commands.go）：user 角色 + 非 text 块 —— provider 按文本翻译送 LLM
	//（模型据此遵循该技能，与模型 load_skill 的工具结果同构），但 collectUserContents /
	// 标题 fallback / 历史恢复渲染只认 text 块 → 不算用户输入、不污染会话标题与对话正文。
	ContentTypeSkill = "skill"
	// ContentTypeMeshFrom 跨会话/远程投递消息的来源提示块（session_send / mesh 入站注入）：
	// user 角色 + 非 text 块 —— provider 按文本翻译送 LLM（模型看到「来自哪个会话」可回信），
	// 但 collectUserTexts / 标题 fallback / UI 只认 text 块 → 来源提示不进对话正文、
	// 不当用户输入、不污染会话标题（正文块紧随其后，保持纯净）。
	ContentTypeMeshFrom = "mesh_from"
)

// Message 是全栈共享的会话消息模型：
// agents 层持它作为会话上下文，provider 层把它翻译为厂商 SDK 参数，
// 可整体序列化（Session 断点恢复）。角色-字段不变量由构造器与 Validate 保证。
type Message struct {
	Role    MessageRole `json:"role"`
	Content []Content   `json:"content,omitempty"`

	// Reasoning assistant 消息的思考内容。
	// 推理模型（DeepSeek 等）在工具调用场景必须完整回传，否则 API 返回 400。
	Reasoning string `json:"reasoning,omitempty"`
	// ReasoningSignature Anthropic thinking 块的 signature（2026-08 新增）。
	// Anthropic 要求 thinking 块原样回传（含 signature，缺失/篡改 400）；OpenAI 系不用。
	// 向后兼容：旧消息无此字段 = 空串，Anthropic 翻译层丢弃 thinking 块（对齐 pi 跨模型行为）。
	ReasoningSignature string `json:"reasoning_signature,omitempty"`
	// Model 生成该消息的模型（2026-09 新增）：thinking 回传的同模型判断用
	//（对齐 pi isSameModel —— 跨模型回传旧 signature 会 400）。空 = 未知（兼容旧消息，
	// Anthropic 翻译层保守处理：signature 存在即回传）。
	Model string `json:"model,omitempty"`
	// ToolCalls assistant 消息发起的工具调用（同样必须完整回传）。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// ToolCallId tool 消息回执关联的调用 Id（与 assistant 的 ToolCalls 配对，
	// 协议对 tool 角色消息的硬性要求）。
	ToolCallId string `json:"tool_call_id,omitempty"`
	// ToolName tool 消息的工具名（requiresToolResultName 端点要求 name 字段；2026-09 新增）。
	ToolName string `json:"tool_name,omitempty"`
	// AddedToolNames tool 结果新增的工具名清单（kimi 延迟工具 deferredToolsMode；
	// 2026-09 新增，对齐 pi toolResult.addedToolNames —— 工具结果里动态声明的工具）。
	AddedToolNames []string `json:"added_tool_names,omitempty"`
}

// Content 一条消息的内容块（text / image），支持图文混排。
type Content struct {
	Type    string `json:"type"`    // text / image
	Content string `json:"content"` // 文本，或图片的 URL / data URL

	MimeType string `json:"mime_type,omitempty"` // image/jpeg, image/png
}

// NewSystemMessage 构造系统消息（纯文本）。
func NewSystemMessage(text string) Message {
	return Message{Role: System, Content: []Content{{Type: "text", Content: text}}}
}

// NewUserMessage 构造用户消息，支持文本/图片内容块混排。
func NewUserMessage(content ...Content) Message {
	return Message{Role: User, Content: content}
}

// NewAssistantMessage 构造助手消息；纯工具调用轮 content 可为空，
// reasoning / toolCalls 随消息完整回传（推理模型工具调用场景必须）。
func NewAssistantMessage(content []Content, reasoning string, toolCalls []ToolCall) Message {
	return Message{Role: Assistant, Content: content, Reasoning: reasoning, ToolCalls: toolCalls}
}

// NewAssistantMessageWithSignature 构造助手消息并携带 Anthropic thinking signature
// （多轮 thinking 回传必需；OpenAI 系 signature 为空）。2026-08 新增。
func NewAssistantMessageWithSignature(content []Content, reasoning, reasoningSignature string, toolCalls []ToolCall) Message {
	m := NewAssistantMessage(content, reasoning, toolCalls)
	m.ReasoningSignature = reasoningSignature
	return m
}

// NewToolMessage 构造工具回执消息，签名强制关联调用 Id；内容为纯文本（协议约束）。
func NewToolMessage(toolCallId, text string) Message {
	return Message{
		Role:       Tool,
		ToolCallId: toolCallId,
		Content:    []Content{{Type: "text", Content: text}},
	}
}

// NewToolMessageWithImages 构造带图片的工具回执消息（text 块 + image 块）：
// 图片类工具（read_file 读图 / 截图 / MCP image）的结果——模型据此「看见」画面。
// 注意：tool 角色的图片块是否原生支持由协议决定（Anthropic 原生；OpenAI 系
// tool 角色只收字符串 → 翻译层把 image 块拆成紧随的 user 消息），见各 provider 翻译。
// images 为空时退化为 NewToolMessage（保持单文本块语义，历史/缓存前缀稳定）。
func NewToolMessageWithImages(toolCallId, text string, images []Content) Message {
	if len(images) == 0 {
		return NewToolMessage(toolCallId, text)
	}
	blocks := make([]Content, 0, len(images)+1)
	blocks = append(blocks, Content{Type: ContentTypeText, Content: text})
	blocks = append(blocks, images...)
	return Message{Role: Tool, ToolCallId: toolCallId, Content: blocks}
}

// NewSkillMessage 构造「用户主动加载技能」的指令消息（命令框架：skills load）。
//
// 与模型 load_skill 的唯一区别是决策来源（用户 vs 模型）：指令正文同构，且同样
// **追加进对话历史**而非拼进系统提示词。为什么必须走历史（2026-09-24）：
// 系统提示词是 provider 缓存前缀的首段（缓存顺序：tools → system → messages），
// 会话中途给它加层 = 已有整段上下文不再是本次请求的前缀 → 按全价重写一次缓存
// （1h 档写入价 2× 基础输入价，上下文越长代价越大）；历史尾部追加只增量写入
// 新增的消息，前缀逐字不变。
func NewSkillMessage(text string) Message {
	return Message{
		Role:    User,
		Content: []Content{{Type: ContentTypeSkill, Content: text}},
	}
}

// NewCommandMessage 构造内部指令消息（命令框架）：system 角色 + command 内容块，
// 经 inbox 注入，消费点在 agents 注入段（剥出执行、不送 LLM、不持久化）。
func NewCommandMessage(name, args string) Message {
	return Message{
		Role:    System,
		Content: []Content{{Type: ContentTypeCommand, Content: name + "\n" + args}},
	}
}

// IsCommandMessage 判断是否内部指令消息（Content[0].Type == command）。
func (m *Message) IsCommandMessage() bool {
	return len(m.Content) > 0 && m.Content[0].Type == ContentTypeCommand
}

// NewTaskResultMessage 构造后台任务完成/失败消息（兼容入口）。
func NewTaskResultMessage(taskID, name, result string, err error) Message {
	status := "completed"
	if err != nil {
		status = "failed"
	}
	return NewTaskResultMessageWithStatus(taskID, name, result, status, err)
}

// NewTaskResultMessageWithStatus 构造后台任务终态消息：user 角色 + task_result 内容块。
// status 是 completed/interrupted/failed/abandoned；result 原样嵌入、不截断、不解释。
func NewTaskResultMessageWithStatus(taskID, name, result, status string, err error) Message {
	var b strings.Builder
	label := "完成"
	switch status {
	case "interrupted":
		label = "已中断"
	case "failed":
		label = "失败"
	case "abandoned":
		label = "已弃置"
	}
	fmt.Fprintf(&b, "〔后台任务 %s %s〕", taskID, label)
	if name != "" {
		fmt.Fprintf(&b, "（%s）", name)
	}
	b.WriteString("\n\n")
	switch {
	case err != nil:
		b.WriteString(err.Error())
	case result != "":
		b.WriteString(result)
	case status == "interrupted":
		b.WriteString("任务已按请求中断，未产生最终结果。")
	default:
		b.WriteString("任务未返回结果。")
	}
	if status == "interrupted" {
		b.WriteString("\n\n—— 该任务未完成，不要将其视为成功结果；按当前用户意图继续。")
	} else {
		b.WriteString("\n\n—— 后台任务结果，据此继续当前工作。")
	}
	return Message{
		Role:    User,
		Content: []Content{{Type: ContentTypeTaskResult, Content: b.String()}},
	}
}

// Validate 校验消息不变量：角色合法、tool 消息必须有关联 Id、
// ToolCalls 仅限 assistant、内容块类型合法。
func (m *Message) Validate() error {
	switch m.Role {
	case System, User, Assistant, Tool:
	default:
		return fmt.Errorf("invalid role %q", m.Role)
	}
	if m.Role == Tool && m.ToolCallId == "" {
		return errors.New("tool message requires ToolCallId")
	}
	if m.Role != Tool && m.ToolCallId != "" {
		return fmt.Errorf("role %s cannot carry ToolCallId", m.Role)
	}
	if m.Role != Assistant && len(m.ToolCalls) > 0 {
		return fmt.Errorf("role %s cannot carry ToolCalls", m.Role)
	}
	for i, c := range m.Content {
		// summary：LLM 压缩器生成的摘要消息类型（provider 翻译时按文本发送）；
		// command：内部指令消息（命令框架，仅 Session.Command 注入，不持久化）；
		// task_result：后台任务完成消息（仅 Session.PushTaskResult 注入，非用户输入）；
		// skill：用户主动加载的技能指令（命令框架，仅 agents 注入，非用户输入）
		if c.Type != "" && c.Type != ContentTypeText && c.Type != ContentTypeImage && c.Type != ContentTypeSummary && c.Type != ContentTypeCommand && c.Type != ContentTypeTaskResult && c.Type != ContentTypeMeshFrom && c.Type != ContentTypeSkill {
			return fmt.Errorf("content[%d] has invalid type %q", i, c.Type)
		}
	}
	return nil
}
