package responses

import (
	"encoding/json"
	"strings"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"

	"github.com/sashabaranov/go-openai"
)

// —— Responses 输入 item 结构 ——
// go-openai v1.42.0 的 ResponseInputMessage.Content 是 any，且没有
// ResponseOutputMessage / ResponseFunctionToolCall 结构体 —— 用本地 struct 构造，
// 序列化后与 Responses API 的 input item 形态一致（type 字段区分）。

type inputItem struct {
	Type      string `json:"type"`
	Role      string `json:"role,omitempty"`
	Content   any    `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    any    `json:"output,omitempty"`
	Status    string `json:"status,omitempty"`
	ID        string `json:"id,omitempty"`
}

// responsesMessageItemType 输入消息 item 的类型恒为 "message"（官方 item 联合类型里
// 只有 "message"：EasyInputMessage / ResponseOutputMessage 同名，user/assistant 共用）。
// 历史实现按 pi 文档错写成 "input_message"，上游按联合类型逐项校验 → 整轮 400：
//
//	[invalid_value] Invalid value: 'input_message'. Supported values are: ... 'message' ...
//
// （OpenCode Go 网关转发官方 Responses 上游，2026-09 用户实测；见 translateMessages。）
const responsesMessageItemType = "message"

type inputContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// normalizeResponsesItemID 归一 Responses item id（对齐 pi normalizeIdPart）：
// 非 [A-Za-z0-9_-] → _，截断 64，去尾部 _。function_call item id 必须 fc_ 前缀
// （buildForeignResponsesItemId）。
func normalizeResponsesItemID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
		if b.Len() >= 64 {
			break
		}
	}
	s := b.String()
	s = strings.TrimRight(s, "_")
	if s == "" {
		return "fc_"
	}
	return s
}

// translateMessages 把抽象消息翻译为 Responses API 输入 item 数组。
// system 消息 → instructions（返回）；user/assistant/tool → input items。
// 兼容（对齐 pi openai-responses-shared.ts convertResponsesMessages）：
//   - developer role：推理模型 system → developer（仅 openai 官方端点）
//   - tool_call id 归一（fc_ 前缀 + 64 字符）
//   - thinking 回传：core.Message.ReasoningSignature 存 reasoning item JSON → 原样回传
func translateMessages(messages []core.Message, useDeveloperRole bool) (string, []any) {
	var instructions string
	var out []any
	// pendingImages 缓冲 tool 结果的图片块：Responses 的 function_call_output 只接受
	// 字符串 output，图片无处安放 → tool 段结束后合并成一条 user message item
	//（多工具批只发一条，避免在 function_call/output 配对之间插入消息）。
	var pendingImages []core.Content
	flushImages := func() {
		if len(pendingImages) == 0 {
			return
		}
		content := make([]inputContentPart, 0, len(pendingImages))
		for _, c := range pendingImages {
			content = append(content, inputContentPart{Type: "input_image", ImageURL: c.Content, Detail: "auto"})
		}
		pendingImages = nil
		out = append(out, inputItem{Type: responsesMessageItemType, Role: "user", Content: content})
	}
	for _, m := range messages {
		if m.Role != core.Tool {
			flushImages()
		}
		switch m.Role {
		case core.System:
			for _, c := range m.Content {
				// text（行为契约/提醒）+ summary（压缩摘要，agents/llm_compressor 产物，
				// Type="summary"）：只认 text 会把压缩摘要整块丢掉 —— 压缩后模型看不到
				// 任何历史（与 Anthropic 翻译层同一根因，见 provider/anthropic/translate.go
				// isSystemText）。command（内部指令）与空块仍丢弃。
				if c.Type == core.ContentTypeText || c.Type == core.ContentTypeSummary {
					if strings.TrimSpace(c.Content) == "" {
						continue
					}
					if instructions != "" {
						instructions += "\n\n"
					}
					instructions += provider.SanitizeSurrogates(c.Content)
				}
			}
		case core.User:
			var content []inputContentPart
			for _, c := range m.Content {
				if c.Type == core.ContentTypeImage {
					content = append(content, inputContentPart{Type: "input_image", ImageURL: c.Content, Detail: "auto"})
				} else {
					content = append(content, inputContentPart{Type: "input_text", Text: provider.SanitizeSurrogates(c.Content)})
				}
			}
			// 空 user 消息跳过：上层 AgentLoop 不回写空轮（见 chatcompletions 的
			// isEmptyAssistantMessage 同思路）；防御性跳过避免空 content 的 message item
			// 被上游拒绝（Responses API 对空 content 的消息 item 严格校验）。
			if len(content) == 0 {
				continue
			}
			out = append(out, inputItem{Type: responsesMessageItemType, Role: "user", Content: content})
		case core.Assistant:
			// 思考回传：ReasoningSignature 存 reasoning item JSON（响应侧捕获的
			// reasoning.encrypted_content；对齐 pi 原样回传，多轮同模型推理必需）
			if m.ReasoningSignature != "" {
				var reasoningItem map[string]any
				if json.Unmarshal([]byte(m.ReasoningSignature), &reasoningItem) == nil {
					out = append(out, reasoningItem)
				}
			}
			// 工具调用轮 → function_call item（id 归一 fc_ 前缀）
			for _, tc := range m.ToolCalls {
				callID := normalizeResponsesItemID(tc.Id)
				out = append(out, inputItem{
					Type:      "function_call",
					CallID:    callID,
					ID:        "fc_" + callID,
					Name:      tc.Name,
					Arguments: tc.Arguments,
				})
			}
			// 可见文本 → assistant message item
			var text string
			for _, c := range m.Content {
				if c.Type == core.ContentTypeText {
					if text != "" {
						text += "\n"
					}
					text += c.Content
				}
			}
			if text != "" {
				// 历史 assistant 文本 → message item，content part 必须用 output_text：
				// assistant 角色的消息 item 按 ResponseOutputMessage 校验，content part 只收
				// output_text / refusal —— 实测发 input_text 被同一条校验拒绝：
				//   invalid_value "Invalid value: 'input_text'. Supported values are:
				//   'output_text' and 'refusal'."（param=input[2].content[0]）
				// id / annotations 上游并不强求（curl 四种组合实测均 200），按最小形态发。
				// 注意 user 侧相反：那里只认 input_text/input_image（见上）。
				out = append(out, inputItem{
					Type:    responsesMessageItemType,
					Role:    "assistant",
					Content: []map[string]string{{"type": "output_text", "text": provider.SanitizeSurrogates(text)}},
					Status:  "completed",
				})
			}
		case core.Tool:
			for _, c := range m.Content {
				if c.Type == core.ContentTypeImage && c.Content != "" {
					pendingImages = append(pendingImages, c)
				}
			}
			out = append(out, inputItem{Type: "function_call_output", CallID: normalizeResponsesItemID(m.ToolCallId), Output: toolOutputText(m)})
		}
	}
	flushImages() // 尾部 tool 段（正常每轮都会命中）
	return instructions, out
}

func toolOutputText(m core.Message) string {
	var sb strings.Builder
	for _, c := range m.Content {
		if c.Type != core.ContentTypeImage {
			sb.WriteString(c.Content)
		}
	}
	if sb.Len() == 0 {
		sb.WriteString("[empty tool result]")
	}
	return provider.SanitizeSurrogates(sb.String())
}

// translateTools 映射 core.ToolSchema → openai.ResponseTool（NewResponseFunctionTool）。
func translateTools(tools []core.ToolSchema) []openai.ResponseTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]openai.ResponseTool, 0, len(tools))
	for _, s := range tools {
		out = append(out, openai.NewResponseFunctionTool(openai.FunctionDefinition{
			Name:        s.Name,
			Description: s.Description,
			Parameters:  s.Parameters,
		}))
	}
	return out
}
