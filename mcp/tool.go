package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/tools"
	"strings"

	gomcp "github.com/mark3labs/mcp-go/mcp"
)

// Tool 单个 MCP 工具描述（ListTools 结果 → 引擎适配器）。
type Tool struct {
	server      string // 所属服务器名（工具名前缀）
	name        string
	description string
	schema      map[string]any // JSON schema（引擎 core.ToolSchema.Parameters 同构）
	readOnly    bool           // 服务器 readOnlyHint → 只读桶/并行/审批
}

// newTool 从 mcp-go Tool 构造描述：schema 取自 RawInputSchema（有则优先）或 InputSchema，
// 归一化为 map[string]any JSON schema。
func newTool(server string, t gomcp.Tool) Tool {
	return Tool{
		server:      server,
		name:        t.Name,
		description: t.Description,
		schema:      inputSchema(t),
		readOnly:    t.Annotations.ReadOnlyHint != nil && *t.Annotations.ReadOnlyHint,
	}
}

// inputSchema 把 mcp-go 工具的输入 schema 归一化为 map[string]any。
func inputSchema(t gomcp.Tool) map[string]any {
	var schema map[string]any
	raw := t.RawInputSchema
	if len(raw) == 0 {
		var err error
		if raw, err = json.Marshal(t.InputSchema); err != nil {
			raw = nil
		}
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &schema)
	}
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return schema
}

// EngineName 引擎内工具名（mcp__<server>__<tool>，Claude Code 同款前缀约定，
// 平面引擎避撞名；适配器持有 server 引用，Call 时还原回传）。
func (t Tool) EngineName() string {
	return "mcp__" + t.server + "__" + t.name
}

// mcpTool 是 MCP 工具 → 引擎 tools.Tool 的适配器。持有 Server 引用（共享连接），
// Call 经 client.CallTool 代理到远端。引擎的能力（审批/todo/hooks/并行/截断/超时）全部白拿。
type mcpTool struct {
	server *Server
	tool   Tool
}

// NewAdapter 为单个 MCP 工具建适配器（Manager 装配时调用）。
func NewAdapter(s *Server, t Tool) tools.Tool {
	return &mcpTool{server: s, tool: t}
}

func (a *mcpTool) Name() string { return a.tool.EngineName() }

func (a *mcpTool) Description() string {
	if a.tool.description == "" {
		return fmt.Sprintf("MCP 工具（服务器 %s）：%s", a.tool.server, a.tool.name)
	}
	return a.tool.description
}

func (a *mcpTool) Parameters() any { return a.tool.schema }

// CanParallel 只读 MCP 工具可并行（readOnlyHint）；可变工具串行防远端竞态。
func (a *mcpTool) CanParallel() bool { return a.tool.readOnly }

// ReadOnly 实现 tools.ReadOnlyTool：只读工具进 plan 模式可见集 + 只读桶分桶。
func (a *mcpTool) ReadOnly() bool { return a.tool.readOnly }

// RequiresApproval 实现 tools.ApprovalRequired：非只读（可能产生外部副作用）的 MCP
// 工具纳入审批通道（有 Approver 注入即 HITL 时）。保守安全：远端副作用引擎无法判断。
func (a *mcpTool) RequiresApproval(_ context.Context, _ core.ToolCall) bool {
	return !a.tool.readOnly
}

func (a *mcpTool) ValidParams(_ context.Context, _, arguments string) error {
	if strings.TrimSpace(arguments) == "" || arguments == "null" {
		return nil // 无参数调用合法
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(arguments), &m); err != nil {
		return fmt.Errorf("%s: 参数非合法 JSON: %w", a.Name(), err)
	}
	return nil
}

func (a *mcpTool) BeforeCall(_ context.Context, _ core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}

func (a *mcpTool) AfterCall(_ context.Context, _ core.ToolCall) {}

// Call 执行 MCP 工具调用：arguments JSON → 参数表 → CallTool → 结果序列化回字符串。
// 服务器返回 IsError 视为工具失败（引擎标 ToolResult.IsError，模型可见）。
//
// 图片内容（ImageContent）不再降级为占位文本，而是经 Images()（ToolImageProvider）
// 交给引擎并最终进模型上下文 —— MCP 生态里截图类工具（浏览器/桌面/图表）因此
// 真正「可被看见」。每次调用先清空暂存（引擎在本次返回后立即读取）。
func (a *mcpTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var args map[string]any
	if strings.TrimSpace(arguments) != "" && arguments != "null" {
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return "", fmt.Errorf("%s: %w", a.Name(), err)
		}
	}
	res, err := a.server.callTool(ctx, a.tool.name, args)
	if err != nil {
		return "", err
	}
	tools.ImageSinkFrom(ctx).Add(imagesOfResult(res)...)
	if res.IsError {
		return serializeResult(res), fmt.Errorf("mcp %s.%s: %s", a.tool.server, a.tool.name, shortResult(res))
	}
	return serializeResult(res), nil
}

// imagesOfResult 从 MCP 结果提取图片内容块（ImageContent → data URL）。
// 体积不在此过滤：引擎统一按 maxImageBytes 过闸（单点治理，避免两处口径漂移）。
// 音频/嵌入资源不含图片，仍走 serializeResult 的文本占位。
func imagesOfResult(res *gomcp.CallToolResult) []core.Content {
	if res == nil {
		return nil
	}
	var out []core.Content
	for _, c := range res.Content {
		img, ok := c.(gomcp.ImageContent)
		if !ok || img.Data == "" {
			continue
		}
		mime := img.MIMEType
		if mime == "" {
			mime = "image/png"
		}
		// MCP 的 Data 已是 base64（非 data URL）→ 组装成 data URL 供各协议统一消费
		//（OpenAI/Responses 要 URL 形态；Anthropic 翻译层会再解析回 base64）。
		out = append(out, core.Content{
			Type:     core.ContentTypeImage,
			Content:  "data:" + mime + ";base64," + img.Data,
			MimeType: mime,
		})
	}
	return out
}

// serializeResult 把 CallToolResult 序列化为模型可读文本：text 内容拼接 +
// 音频给占位 + 结构化内容 JSON（**不把 base64 二进制倒进上下文**）。
// 图片：文本侧只留一行说明（体积/编码），图像本体经 imagesOfResult → Images()
// 作为内容块进上下文（模型真正看见画面）；此处若是「图片被体积闸门挡掉」的场景，
// 引擎会在结果文本里追加 omitted 说明（tools.Engine.limitImages）。
// Content 元素为值类型（mcp-go UnmarshalContent 返回 TextContent 等非指针）。
func serializeResult(res *gomcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		switch ct := c.(type) {
		case gomcp.TextContent:
			if ct.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(ct.Text)
			}
		case gomcp.ImageContent:
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			// 图像本体随内容块送达（模型可见）；文本侧只声明存在与体积。
			fmt.Fprintf(&b, "[image %s, %d bytes — attached for direct viewing]", ct.MIMEType, len(ct.Data))
		case gomcp.AudioContent:
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "[audio %s: %d bytes base64]", ct.MIMEType, len(ct.Data))
		case gomcp.EmbeddedResource:
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "[embedded resource %s]", resourceURI(ct.Resource))
		default:
			if raw, err := json.Marshal(c); err == nil {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.Write(raw)
			}
		}
	}
	if res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString("structuredContent: ")
			b.Write(raw)
		}
	}
	if b.Len() == 0 {
		return "[empty tool result]"
	}
	return b.String()
}

// resourceURI 取嵌入式资源的 URI（ResourceContents 是标记接口，按具体类型取字段）。
func resourceURI(rc gomcp.ResourceContents) string {
	switch v := rc.(type) {
	case *gomcp.TextResourceContents:
		return v.URI
	case *gomcp.BlobResourceContents:
		return v.URI
	}
	return ""
}

// shortResult 错误结果截断摘要（避免整份错误内容进日志/事件）。
func shortResult(res *gomcp.CallToolResult) string {
	s := serializeResult(res)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
