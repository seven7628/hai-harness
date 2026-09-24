package anthropic

import (
	"encoding/json"
	"strings"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// translateMessages 把抽象消息翻译为 Anthropic MessageParam。
// 返回 (system 顶层参数, messages)。
// 关键差异 vs OpenAI：
//   - **对话开始前**的 system 消息 → 顶层 system 参数（非 role 消息），带 cache_control
//     （对齐 pi：system 提示默认加 ephemeral 缓存标记，长对话省 token/降延迟）；
//     对话开始后出现的 system 消息（运行期提醒）**原位转 user 消息**（见 core.System 分支）
//   - tool 消息 → user 角色 + tool_result 块
//   - assistant thinking 回传（有 signature 才回传 —— 决策 #1，对齐 pi）
//   - 图片：data URL → base64 source；http(s) URL → URL source（Anthropic 支持）
//   - tool_call id 归一（对齐 pi normalizeToolCallId）：Anthropic 只收
//     ^[A-Za-z0-9_-]{1,64}$，跨协议会话（responses 450+ 字符含 | 的 id）必须归一。
//
// cacheControl：system 缓存断点开关（默认 true = 发；由调用方按 provider/模型决策，
// 第三方聚合网关（自建反代 / 聚合端点，如某些托管版 DeepSeek）支持 prompt cache 应开启）。
// 参数可省略（旧调用兼容）：省略 = true（发）；第二个可选参数 = 1h TTL 档位（省略 = 5m）。
func translateMessages(messages []core.Message, currentModel string, cacheControlOpt ...bool) ([]anthropic.TextBlockParam, []anthropic.MessageParam) {
	cacheControl := true
	if len(cacheControlOpt) > 0 {
		cacheControl = cacheControlOpt[0]
	}
	// 第二参：TTL 档位（true = 1h extended TTL；省略 = 5m 默认）。见 cacheControlEphemeral。
	oneHourTTL := false
	if len(cacheControlOpt) > 1 {
		oneHourTTL = cacheControlOpt[1]
	}
	var system []anthropic.TextBlockParam
	var out []anthropic.MessageParam
	// tool_call id → 归一 id 映射（tool_result 的 tool_use_id 必须与 assistant 的 tool_use id 一致）
	toolCallIDMap := map[string]string{}

	for _, m := range messages {
		switch m.Role {
		case core.System:
			for _, c := range m.Content {
				if !isSystemText(c) {
					continue
				}
				// 对话已开始（out 里已有 user/assistant/tool）→ 这条 system 是**运行期
				// 提醒**（待办收尾门控 / 在途任务 / Stop hook / 压缩交接），按时间序原位
				// 落成 user 文本块。
				//
				// 为什么不能上提到顶层 system：Anthropic 的缓存前缀顺序是
				// tools → system → messages，上提等于把「晚于整段对话出现的提醒」插到
				// 对话**之前**——上一轮的对话尾部断点（断点定义的是前缀边界）不再是本条
				// 请求的前缀，于是每追加一条提醒就要把整段上下文按全价重写一次缓存；
				// 原位保留则前缀逐字稳定，只有新提醒本身进增量写入。
				// 语义也不损失：提醒文案自带 <system-reminder> 包装（见 agents/prompt/
				// reminder.go），转 user 后模型仍视其为引擎提示而非用户发言。
				if len(out) > 0 {
					out = append(out, anthropic.NewUserMessage(anthropic.ContentBlockParamUnion{
						OfText: &anthropic.TextBlockParam{Text: provider.SanitizeSurrogates(c.Content)},
					}))
					continue
				}
				// 对话开始前的 system 段（系统提示词 / 压缩摘要 / 任务锚点）→ 顶层
				// system。断点稍后统一打在 system **末块**（下方 cacheControl 分支）：
				// 缓存断点定义的是「前缀边界」，同一次请求里多打几个 system 断点不增加
				// 命中面，只白耗 Anthropic 的 4 断点额度。
				system = append(system, anthropic.TextBlockParam{
					Text: provider.SanitizeSurrogates(c.Content),
				})
			}
		case core.User:
			var blocks []anthropic.ContentBlockParamUnion
			for _, c := range m.Content {
				switch c.Type {
				case core.ContentTypeImage:
					blocks = append(blocks, imageBlock(c))
				default:
					blocks = append(blocks, anthropic.ContentBlockParamUnion{
						OfText: &anthropic.TextBlockParam{Text: provider.SanitizeSurrogates(c.Content)},
					})
				}
			}
			// 空 user 消息（无可见块）跳过：上层 AgentLoop 不回写空轮（纯思考/全空轮），
			// 历史里不会出现；此处防御性跳过避免产生空消息打乱 user/assistant/tool 交错
			// （与 chatcompletions 的 isEmptyAssistantMessage 同思路——上游严格 schema
			// 会拒绝空消息；user 侧无内容即无信息量）。
			if len(blocks) > 0 {
				out = append(out, anthropic.NewUserMessage(blocks...))
			}
		case core.Assistant:
			var blocks []anthropic.ContentBlockParamUnion
			// thinking 回传：有 signature 且同模型才回传（对齐 pi isSameModel ——
			// Anthropic signature 模型绑定，跨模型回传旧 signature 会 400）。
			// 消息未记录模型（旧会话/未知）→ 保守回传（signature 存在性近似）。
			if m.Reasoning != "" && thinkingSignature(m) != "" && (m.Model == "" || m.Model == currentModel) {
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfThinking: &anthropic.ThinkingBlockParam{
						Thinking:  provider.SanitizeSurrogates(m.Reasoning),
						Signature: thinkingSignature(m),
					},
				})
			}
			for _, c := range m.Content {
				if c.Type == core.ContentTypeText {
					blocks = append(blocks, anthropic.ContentBlockParamUnion{
						OfText: &anthropic.TextBlockParam{Text: provider.SanitizeSurrogates(c.Content)},
					})
				}
			}
			for _, tc := range m.ToolCalls {
				normID := normalizeToolCallID(tc.Id)
				toolCallIDMap[tc.Id] = normID
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolUse: &anthropic.ToolUseBlockParam{
						ID:    normID,
						Name:  tc.Name,
						Input: jsonStringToAny(tc.Arguments),
					},
				})
			}
			if len(blocks) > 0 {
				out = append(out, anthropic.NewAssistantMessage(blocks...))
			}
		case core.Tool:
			// tool 结果 → user 角色 + tool_result 块（Content 是 []ToolResultBlockParamContentUnion）
			// 图片块原生支持（tool_result 的 content 是块数组）→ 与文本并列直接投放，
			// 无需像 OpenAI 系那样拆成后续 user 消息（Anthropic 协议更表达力强）。
			text := strings.Builder{}
			var imgs []core.Content
			for _, c := range m.Content {
				if c.Type != core.ContentTypeImage {
					text.WriteString(provider.SanitizeSurrogates(c.Content))
					continue
				}
				if c.Content != "" {
					imgs = append(imgs, c)
				}
			}
			if text.Len() == 0 && len(imgs) == 0 {
				text.WriteString("[empty tool result]")
			}
			toolUseID := m.ToolCallId
			if norm, ok := toolCallIDMap[toolUseID]; ok {
				toolUseID = norm
			}
			content := make([]anthropic.ToolResultBlockParamContentUnion, 0, len(imgs)+1)
			if text.Len() > 0 {
				content = append(content, anthropic.ToolResultBlockParamContentUnion{
					OfText: &anthropic.TextBlockParam{Text: text.String()},
				})
			}
			for _, c := range imgs {
				content = append(content, toolResultImageBlock(c))
			}
			out = append(out, anthropic.NewUserMessage(
				anthropic.ContentBlockParamUnion{
					OfToolResult: &anthropic.ToolResultBlockParam{
						ToolUseID: toolUseID,
						Content:   content,
					},
				},
			))
		}
	}
	// 系统提示词断点打在**末块**（见 core.System 分支注释）：一个断点即可把整段
	// system（行为契约 + 产品层 + 工作记忆 + 技能清单 + 交接提醒）纳入同一条缓存前缀。
	if cacheControl && len(system) > 0 {
		system[len(system)-1].CacheControl = cacheControlEphemeral(oneHourTTL)
	}
	return system, out
}

// cacheControlEphemeral 缓存断点参数（全仓 TTL 的唯一决策点）。
//
// TTL=1h（extended cache TTL，2026-09-21 用户决策）：默认 5 分钟对这个 Harness 太短 ——
// 一轮工具批、一个后台任务、用户离开几分钟就过期，下一轮等于冷启动把整段上下文重写一遍。
// 代价是写入价从 1.25× 涨到 2× 基础输入价（`providers.json` anthropic 段 `cache_write`
// 已按 1h 更新；读取价不变）。要求 1h 的请求需带 beta 头（见 anthropic.go 的 betas）。
//
// 兼容端点（非官方 base URL：智谱 / MiniMax / 第三方聚合）保持 5m 默认（不带 ttl 字段）：
// 它们只实现 Messages 基础协议，ttl 是 Claude 扩展字段，发了可能 400。
func cacheControlEphemeral(oneHourTTL bool) anthropic.CacheControlEphemeralParam {
	if !oneHourTTL {
		return anthropic.NewCacheControlEphemeralParam()
	}
	return anthropic.CacheControlEphemeralParam{
		Type: "ephemeral",
		TTL:  anthropic.CacheControlEphemeralTTLTTL1h,
	}
}

// isSystemText system 角色的哪个内容块该进请求（顶层 system 或原位 user）：
//   - text：行为契约 / 产品层 / 运行期提醒；
//   - summary：压缩摘要（agents/llm_compressor 产物 = system 角色 + Type="summary"）。
//
// 为什么必须带上 summary：旧实现只认 text，压缩摘要在 Anthropic / Responses 两条翻译
// 路径被**静默丢弃**（OpenAI 系协议按「非图片块即文本」处理，故只有这两条中招）——
// 压缩后模型看到的上下文里没有任何历史，只剩系统提示词与交接提醒（上下文等于归零）。
// command（内部指令，消费点在 agents 注入段剥出）与空块仍被丢弃。
func isSystemText(c core.Content) bool {
	if strings.TrimSpace(c.Content) == "" {
		return false
	}
	return c.Type == core.ContentTypeText || c.Type == core.ContentTypeSummary
}

// cacheBreakpointMax Anthropic 单请求 cache_control 断点硬上限（超限 400）。本仓布局
// 恒为 ≤3：system 末块（1）+ 工具末项（1，见 translateTools）+ 对话尾部（1，见
// markConversationTail）—— 留 1 个余量给将来（如多轮增量断点）。
const cacheBreakpointMax = 4

// markConversationTail 给「对话尾部」打滚动缓存断点：从最后一条消息往前找第一个可缓存
// 内容块（text / tool_result / tool_use / image / document）并在其上打 ephemeral 断点。
//
// 为什么需要：断点只打在 system + 最后一个工具上时，缓存前缀就固定为「系统提示词 + 工具
// schema」，对话主体（每轮都在涨的那部分）**永远写不进缓存** —— 会话越长，命中率越低、
// 单价越高。打在对话尾部后：本轮把「到上一轮为止的全部历史」写进缓存，下一轮请求按最长
// 匹配前缀直接命中上一轮的尾部断点，只有最新一轮的内容按全价计费（Anthropic 的滚动缓存
// 用法，与 Claude Code 一致）。
//
// 返回是否打上（最后一条消息全是不支持缓存_control 的块时返回 false，不报错）。
func markConversationTail(msgs []anthropic.MessageParam, oneHourTTL ...bool) bool {
	cc := cacheControlEphemeral(len(oneHourTTL) > 0 && oneHourTTL[0])
	for i := len(msgs) - 1; i >= 0; i-- {
		blocks := msgs[i].Content
		for j := len(blocks) - 1; j >= 0; j-- {
			b := blocks[j]
			switch {
			case b.OfText != nil:
				b.OfText.CacheControl = cc
			case b.OfToolResult != nil:
				b.OfToolResult.CacheControl = cc
			case b.OfToolUse != nil:
				b.OfToolUse.CacheControl = cc
			case b.OfImage != nil:
				b.OfImage.CacheControl = cc
			case b.OfDocument != nil:
				b.OfDocument.CacheControl = cc
			default:
				continue // thinking / server_tool 等块：跳过，继续往前找
			}
			blocks[j] = b
			msgs[i].Content = blocks
			return true
		}
	}
	return false
}

// normalizeToolCallID 归一 tool call id 到 Anthropic 允许的格式（对齐 pi normalizeToolCallId）：
// 非 [A-Za-z0-9_-] 字符替换为 _，截断到 64 字符。tool_result 与 assistant tool_use 用同一映射，
// 保证一一对应。
func normalizeToolCallID(id string) string {
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
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// imageBlock 构造图片内容块：data URL → base64 source；http(s) URL → URL source。
func imageBlock(c core.Content) anthropic.ContentBlockParamUnion {
	return anthropic.ContentBlockParamUnion{OfImage: imageParam(c)}
}

// toolResultImageBlock 构造 tool_result 内的图片块（Content 是块联合）；source 语义同 imageBlock。
func toolResultImageBlock(c core.Content) anthropic.ToolResultBlockParamContentUnion {
	return anthropic.ToolResultBlockParamContentUnion{OfImage: imageParam(c)}
}

// imageParam 构造图片块参数（user 消息与 tool_result 两条路径共用，避免 source 语义漂移）。
func imageParam(c core.Content) *anthropic.ImageBlockParam {
	mediaType, data := parseDataURL(c)
	if mediaType != "" && data != "" {
		return &anthropic.ImageBlockParam{
			Source: anthropic.ImageBlockParamSourceUnion{
				OfBase64: &anthropic.Base64ImageSourceParam{
					MediaType: anthropic.Base64ImageSourceMediaType(mediaType),
					Data:      data,
				},
			},
		}
	}
	// http(s) URL → URL source（Anthropic 官方支持）
	return &anthropic.ImageBlockParam{
		Source: anthropic.ImageBlockParamSourceUnion{
			OfURL: &anthropic.URLImageSourceParam{URL: c.Content},
		},
	}
}

// translateTools core.ToolSchema → anthropic.ToolUnionParam。
// 对齐 pi convertTools：eager_input_streaming=true（工具流式输入；supportsEagerToolInputStreaming
// 默认 true）+ 最后一个工具加 cache_control（supportsCacheControlOnTools 默认 true，长工具列表
// 跨轮命中缓存）。
// compat=true（兼容端点，智谱 GLM / MiniMax 等）：去掉 eager_input_streaming ——
// 兼容端点不认识该 Claude 扩展字段，发了会 400 invalid_request_params。
// cacheControl：最后一个工具是否加 cache_control（与 eager 解耦；GLM/Kimi 官方
// 直连不支持、第三方聚合网关（自建反代 / 聚合端点）支持 → 由调用方决策）。
// 参数可省略（旧调用兼容）：省略时 compat=false、cacheControl=true、oneHourTTL=false（5m）。
func translateTools(tools []core.ToolSchema, compatOpt ...bool) []anthropic.ToolUnionParam {
	compat := false
	cacheControl := true
	oneHourTTL := false
	if len(compatOpt) > 0 {
		compat = compatOpt[0]
	}
	if len(compatOpt) > 1 {
		cacheControl = compatOpt[1]
	}
	if len(compatOpt) > 2 {
		oneHourTTL = compatOpt[2] // 缓存 TTL 档位（见 cacheControlEphemeral）
	}
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropic.ToolUnionParam, 0, len(tools))
	for i, s := range tools {
		// input_schema = 完整 JSON Schema（type/properties/required/任意扩展键平级）。
		// SDK 的 ToolInputSchemaParam 把 properties/required/type 拆成结构体字段——
		// 若把整个 schema 塞 Properties 会产出嵌套 properties{properties:{...}} 的
		// 非法结构（GLM 等端点 400 1210「API 调用参数有误」实证 2026-09）。
		// 正确做法：整份 schema 经 ExtraFields/Override 输出（见 toolInputSchema）。
		schema, _ := s.Parameters.(map[string]any)
		input := toolInputSchema(schema)
		tool := &anthropic.ToolParam{
			Name:        s.Name,
			Description: anthropic.String(s.Description),
			InputSchema: input,
		}
		if !compat {
			tool.EagerInputStreaming = anthropic.Bool(true)
		}
		// 最后一个工具加 ephemeral 缓存（对齐 pi：params.length > 0 时最后一个 tool）
		if cacheControl && i == len(tools)-1 {
			tool.CacheControl = cacheControlEphemeral(oneHourTTL)
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: tool})
	}
	return out
}

// toolInputSchema 工具 input_schema 参数（整份 schema 原样投放）。
//
// 为什么不用 SDK 的 ExtraFields（虽然它也能输出任意键）：ExtraFields 是 map，
// SDK 的 MarshalWithExtras 按 Go map 的**随机迭代序**逐键写入 —— 同一份 schema 在同一
// 进程里序列化 200 次会得到 3 种不同字节（实测 `{"type":..,"properties":..,"required":..}`
// 的键序随机轮换）。tools 是 Anthropic 缓存前缀的**第一段**（顺序 tools → system →
// messages）：请求字节一旦抖动，整条前缀（含 system 与全部对话）与实际缓存条目逐字不同
// ⇒ **缓存恒 miss**，每轮把整段上下文按 1.25× 写一遍。这是「Anthropic 协议缓存命中率
// 恒 0」的根因（对话尾部滚动断点本身没问题，见 markConversationTail）。
//
// 修法：自己用 encoding/json 序列化（map 键按字典序、结构体按字段序，稳定），再经
// param.Override 以原始 JSON 投放 —— 「同一 schema → 同一字节」，跨轮/跨进程一致。
func toolInputSchema(schema map[string]any) anthropic.ToolInputSchemaParam {
	if schema == nil {
		return anthropic.ToolInputSchemaParam{} // SDK 零值默认（type:"object"）
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		// 不可序列化的 schema（func/NaN 等）：退回 SDK 的结构化路径，请求由端点报错，
		// 不因序列化失败丢掉工具定义。
		return anthropic.ToolInputSchemaParam{ExtraFields: schema}
	}
	return param.Override[anthropic.ToolInputSchemaParam](json.RawMessage(raw))
}

// parseDataURL 解析 data:image/png;base64,xxx → (mediaType, data)。
// 返回空串表示不是 base64 data URL（调用方改用 URL source）。
func parseDataURL(c core.Content) (string, string) {
	const prefix = "data:"
	if !strings.HasPrefix(c.Content, prefix) {
		return "", ""
	}
	rest := strings.TrimPrefix(c.Content, prefix)
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", ""
	}
	meta := rest[:comma]
	data := rest[comma+1:]
	parts := strings.Split(meta, ";")
	mediaType := parts[0]
	base64 := false
	for _, p := range parts[1:] {
		if strings.TrimSpace(p) == "base64" {
			base64 = true
		}
	}
	if !base64 || mediaType == "" || data == "" {
		return "", ""
	}
	return mediaType, data
}

// thinkingSignature 从 core.Message 取 thinking signature（core.Message.ReasoningSignature）。
func thinkingSignature(m core.Message) string {
	return m.ReasoningSignature
}

// jsonStringToAny 把 tool arguments JSON 字符串解析为 any（Anthropic ToolUseBlockParam.Input）。
// 解析失败时返回原始字符串。
func jsonStringToAny(s string) any {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err == nil {
		return v
	}
	return s
}
