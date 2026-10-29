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
	// def server 未显式声明 exposure 时用的引擎档（Manager.DefaultExposure；空 = 包级默认 deferred）。
	def tools.ToolExposure
}

// NewAdapter 为单个 MCP 工具建适配器（Manager 装配时调用）。
func NewAdapter(s *Server, t Tool) tools.Tool {
	return &mcpTool{server: s, tool: t}
}

// newAdapterWithDefault 带「未声明档位」默认值的适配器（Manager.Tools 走这条）。
// 保留 NewAdapter 的公开签名不变：既有调用方零迁移。
func newAdapterWithDefault(s *Server, t Tool, def tools.ToolExposure) tools.Tool {
	return &mcpTool{server: s, tool: t, def: def}
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

// 编译期断言：适配器必须满足暴露层接口（见 tools/exposure.go）。
var (
	_ tools.ExposureProvider     = (*mcpTool)(nil)
	_ tools.NamespaceProvider    = (*mcpTool)(nil)
	_ tools.CodemodeAdvertiser   = (*mcpTool)(nil)
	_ tools.OutputSchemaProvider = (*mcpTool)(nil)
)

// AdvertiseCodemode 实现 tools.CodemodeAdvertiser：MCP 工具是脚本编排的**真正对象**，
// 当它被声明给模型（exposure=direct，即宿主关掉 codemode 后又打开、或显式 direct）时，
// 描述里该带一句「也能从脚本调」。
//
// 注意这条**不是**让 MCP 工具挤进 codemode 目录：deferred 档依旧靠 searchTools 现查
// （描述字节必须随 MCP 连/断保持稳定，见 Exposure 的注释）。它只负责「该被推销的时候
// 别漏掉自己」，与内置工具（内置工具不实现本接口）正好相反。
func (a *mcpTool) AdvertiseCodemode() bool { return true }

// Exposure 实现 tools.ExposureProvider：MCP 工具的暴露档位（配置档 → 引擎档；
// 出厂默认 deferred，见 ServerConfig.Exposure 与 toToolExposure）。
// 每调用一次读 Server.Config()：配置热重载会重建 Server/适配器，缓存一份反而可能读到旧值。
func (a *mcpTool) Exposure() tools.ToolExposure {
	raw := strings.TrimSpace(a.server.Config().Exposure)
	if raw == "" {
		if a.def != "" {
			return a.def // 宿主的装配决策（见 Manager.DefaultExposure：Wave 3 起按 codemode 开关在 direct/deferred 间条件化）
		}
		return tools.ExposureDeferred // 包级出厂默认（设计文档 §18）
	}
	return toToolExposure(raw) // 显式配置永远优先
}

// toToolExposure 配置档 → 引擎档的两层映射（照抄 pi 的 toToolExposure，
// mcp/src/pi-src/mcp/tools.ts:39-41；设计文档 §18）。
//
// 关键一条：配置里的 "codemode" **折成引擎的 deferred**，不是引擎的 ExposureCodemode。
// 两档在本仓里只差「由哪个工具负责激活」，但落到引擎后语义不同：引擎 codemode 档 =
// 可编排 **且进 codemode 描述**，而 MCP 工具必须**永不进描述** —— 一旦写成引擎 codemode，
// 几十个 server 的工具描述会灌进 codemode 的描述，且描述字节随 MCP 连接/断线抖动
// （provider 前缀缓存全失效），即重新引入 pi #10212（0.99.0 引入，0.99.2 修）：
// 这就是这里必须折成 deferred 的全部理由。
//
// 空/未识别值 → deferred（最保守且不丢能力：不进模型工具表、不进描述，但脚本仍可达；
// 正常路径上 Validate 已把非法值拦成显式错误，这里只管手工构造的 ServerConfig）。
func toToolExposure(raw string) tools.ToolExposure {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "direct":
		return tools.ExposureDirect
	case "codemode", "deferred":
		return tools.ExposureDeferred
	case "hidden":
		return tools.ExposureHidden
	default:
		return tools.ExposureDeferred
	}
}

// Namespace 实现 tools.NamespaceProvider：把 MCP server 声明成 codemode 描述里的一个分组
// （工具名里的 mcp__<server>__ 前缀天然就是分组键）。
//
//   - Description 只用**配置信息**合成，与连接状态无关：描述字节必须稳定
//     （同 exposure 的缓存不变量，见 tools/engine.go 的 ToolParams 注释）。
//   - Instructions 只在 server **已连接**时透传（mcp.Server.Instructions 会惰性建连）：
//     Namespace() 在描述生成路径上被调用（工具轮内），在那里做一次上限 15s 的 initialize
//     会卡住整个 turn。已连接时读的是 initialize 缓存下来的字段，零成本；未连接时给空串
//     —— MCP 工具的适配器本身只在连接成功后才存在，所以正常路径上 instructions 不会丢。
//     「已连接」的判定复用 ServerStatus.State（server.go 的公开状态标签）；标签改名会让
//     这里静默失效，故 exposure_test.go 里有「连接前空 / 连接后透传」的双向用例钉住。
func (a *mcpTool) Namespace() *tools.ToolNamespace {
	ns := &tools.ToolNamespace{
		Name:        a.tool.server,
		Description: fmt.Sprintf("MCP server %q — tools grouped here. Server usage guidance is available on demand, not inlined.", a.tool.server),
	}
	if a.server.Connected() {
		ns.Instructions = a.server.Instructions(context.Background())
	}
	return ns
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
	// 每次调用前确保连接（幂等：已连接只是一次锁内判断）。这是「用一次就自愈」的落点 ——
	// 适配器此前**只**调用 callTool，连接完全依赖 Tools() 那一刻建好的那个 client：
	// 连接一死，每次调用都打在同一具尸体上，且没有任何东西会再拨一次（真机上表现为
	// 「接口一直空/一直错，重开新会话也没用」）。markBroken 已把 lastAttempt 归零，
	// 所以这里会立刻重拨而不是等 retryCooldown。
	if connErr := a.server.EnsureConnected(ctx); connErr != nil {
		return "", connErr
	}
	res, err := a.server.callTool(ctx, a.tool.name, args)
	if err != nil {
		return "", err
	}
	tools.ImageSinkFrom(ctx).Add(imagesOfResult(res)...)
	out := serializeResult(res)
	if !res.IsError && tools.IsScriptCall(ctx) {
		// 结构化结果（**只在编排脚本路径**写）：模型路径照旧只看文本，与 bash 同款
		// （tools/builtin/bash.go 也只在 IsScriptCall 时写槽）——结构化结果只有 codemode
		// 消费（bridge.go 的 toScriptResult），在模型路径重复落一份没有读者。
		//
		// IsError 刻意不写：脚本侧「IsError + 有结构化」是 **resolve**（codemode §15.4），
		// 写进去会让错误变成一个能点字段的对象、脚本的 try/catch 收不到，失败被静默吃掉。
		tools.StructuredSinkFrom(ctx).Set(structuredResult(res, out))
	}
	if res.IsError {
		return out, fmt.Errorf("mcp %s.%s: %s", a.tool.server, a.tool.name, shortResult(res))
	}
	return out, nil
}

// OutputSchema 实现 tools.OutputSchemaProvider：声明「本工具会写结构化结果」。
//
// 返回 nil（不声明 schema）：MCP 服务器不告诉调用方输出的 schema，硬编一个就是撒谎；
// 引擎只按「接口是否实现」决定读不读槽位（见 tools/engine.go），schema 仅用于描述生成。
func (a *mcpTool) OutputSchema() any { return nil }

// structuredResult 取本次调用可编程的结构化值（nil = 本次退回文本路径）。
//
// 优先级：服务器给的 StructuredContent（协议原生字段）> 文本内容本身是**严格合法 JSON**。
//
// 为什么必须补这一层：codemode 的表（§15.4）是「仅文本 → resolve 成单个 string」，
// 而 MCP 工具的历史实现只返回文本 —— 于是脚本拿到的是一个 JSON **字符串**，
// `r.count` 恒 undefined、`(r.items||[]).length` 恒 0，**零值且不报错**。实测代价：
// 同一会话里同一条 ai-test 调用，打印原文拿到 25 条/scanned=154，改成取字段就变成
// got=0 scanned=undefined，模型据此判成「MCP 会话死了」，白等一轮人工重连。
// 而同机的 bash 工具（OutputSchemaProvider + StructuredSink）一直给对象，这个不对称
// 正是「从 bash 学来的写法在 MCP 上静默归零」的来源。
//
// "null"/空白不算结构化：那与「本次没有结构化结果」无法区分，交回文本路径。
func structuredResult(res *gomcp.CallToolResult, text string) []byte {
	if res != nil && res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			return raw
		}
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || trimmed == "null" || !json.Valid([]byte(trimmed)) {
		return nil
	}
	return []byte(trimmed)
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
