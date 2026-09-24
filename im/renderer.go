package im

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/events"
)

// Renderer 把事件流翻译成 IM 消息（OutMessage），按 Gateway 能力降级表达。
//
// 原则（docs/IM_INTEGRATION.md §5）：
//   - 业务层只产出一个 OutMessage（含 Card 语义）；Renderer 按 Capabilities 决定最终形态。
//   - IM 无逐 token 流式：ContentChunk 累积，一轮回复完成发一条；全能力平台可用「占位 + Update」模拟。
//   - 审批（HITL）是手机场景核心交互：卡片按钮优先，文本指令兜底。
//
// 线程安全：Renderer 无内部可变状态（除累积缓冲，由调用方串行使用或加锁）。
type Renderer struct {
	mu sync.Mutex // 保护 toolStart（ToolStart/ToolResponse 可能跨 goroutine）

	// 累积缓冲：request_id → 累积文本（ContentChunk 流式）
	buf map[string]*streamBuf
	// 占位消息 id：request_id → 平台消息 id（UpdateMessage 能力平台先发占位）
	placeholder map[string]string
	// toolStart 工具开始时间：tool id → 开始时间（ToolStart 记录，ToolResponse 算耗时）
	toolStart map[string]time.Time
}

type streamBuf struct {
	text string
	chat Chat
	last time.Time
	// tool 徽标行（同一轮内工具事件，完成时并入总结）
	tools []string
}

// NewRenderer 创建 Renderer。
func NewRenderer() *Renderer {
	return &Renderer{
		buf:         make(map[string]*streamBuf),
		placeholder: make(map[string]string),
		toolStart:   make(map[string]time.Time),
	}
}

// Reset 清空累积缓冲（会话切换/重置时调用）。
func (r *Renderer) Reset() {
	r.buf = make(map[string]*streamBuf)
	r.placeholder = make(map[string]string)
	r.toolStart = make(map[string]time.Time)
}

// --- 事件处理 ---

// Handle 处理单个事件，返回要发送的消息列表（可能为空）。
// 需要会话归属信息（workspace/session_id），由调用方在事件里注入（bridge emit 已注入）。
// chat 为当前会话关联的 Chat（无关联则传零值，返回空）。
func (r *Renderer) Handle(e events.Event, chat Chat) []OutMessage {
	switch ev := e.(type) {
	case *events.ContentChunk:
		return r.handleContent(ev, chat)
	case *events.ReasoningChunk:
		return nil // 思考折叠：不单独发（完成时并入总结行）
	case *events.ToolApprovalRequested:
		return []OutMessage{approvalMessage(ev, chat)}
	case *events.ToolStart:
		// 记录工具开始时间（耗时计算用：ToolResponse - ToolStart）
		r.mu.Lock()
		if r.toolStart == nil {
			r.toolStart = make(map[string]time.Time)
		}
		r.toolStart[ev.Id] = ev.Timestamp
		r.mu.Unlock()
		return nil // 工具开始不发（等结果或完成时并入）
	case *events.ToolResponse:
		return r.handleToolResponse(ev, chat)
	case *events.AgentStart:
		return nil
	case *events.AgentEnd:
		return nil
	case *events.SessionRunError:
		return []OutMessage{{
			Kind: KindText,
			Text: fmt.Sprintf("⚠️ %s：%s", runErrMsgs[LangZH], ev.Error),
		}}
	case *events.SessionClosed:
		return nil
	case *events.LLMEnd:
		// 一轮 LLM 结束，flush 累积的正文（若存在）
		return r.flush(ev.RequestId, chat)
	default:
		return nil
	}
}

// Flush 强制冲刷指定 request 的累积正文（轮次结束时调用）。
func (r *Renderer) Flush(requestID string, chat Chat) []OutMessage {
	return r.flush(requestID, chat)
}

func (r *Renderer) handleContent(ev *events.ContentChunk, chat Chat) []OutMessage {
	if strings.TrimSpace(ev.Content) == "" {
		return nil
	}
	b, ok := r.buf[ev.RequestId]
	if !ok {
		b = &streamBuf{chat: chat}
		r.buf[ev.RequestId] = b
	}
	b.text += ev.Content
	b.last = time.Now()
	// 流式：不逐块发。全能力平台可在一定间隔发占位（Update 刷新），v1 先不发占位。
	return nil
}

func (r *Renderer) handleToolResponse(ev *events.ToolResponse, chat Chat) []OutMessage {
	// 计算工具耗时（ToolResponse.Timestamp - ToolStart.Timestamp）
	r.mu.Lock()
	var dur time.Duration
	if st, ok := r.toolStart[ev.Id]; ok {
		dur = ev.Timestamp.Sub(st)
		delete(r.toolStart, ev.Id)
	}
	r.mu.Unlock()
	// 结构化工具信息（富卡片平台用；纯文本平台由 Send 降级）
	info := ToolInfoFromResponse(ev, dur)
	line := ToolLine(ev, dur)
	b, ok := r.buf[ev.RequestId]
	if ok {
		b.tools = append(b.tools, line)
		return nil
	}
	return []OutMessage{{
		Kind: KindToolCard,
		Text: line,
		Tool: info,
	}}
}

// ToolInfoFromResponse 从 ToolResponse 构造结构化工具信息。
func ToolInfoFromResponse(ev *events.ToolResponse, dur time.Duration) *ToolInfo {
	info := &ToolInfo{
		Name:   ev.Name,
		Icon:   toolEmoji(ev.Name),
		Args:   toolArgsSummary(ev.Name, ev.Arguments),
		Status: "ok",
		Dur:    ToolDur(dur),
	}
	if ev.IsError {
		info.Status = "error"
		info.Error = ev.ErrorCode
	}
	if ev.Diff != nil && ev.Diff.Path != "" {
		info.DiffPath = ev.Diff.Path
		info.DiffAdd = ev.Diff.Added
		info.DiffDel = ev.Diff.Removed
	} else if ev.Changed != "" {
		info.DiffPath = ev.Changed
	}
	return info
}

func (r *Renderer) flush(requestID string, chat Chat) []OutMessage {
	b, ok := r.buf[requestID]
	if !ok {
		return nil
	}
	delete(r.buf, requestID)
	delete(r.placeholder, requestID)

	var parts []string
	if strings.TrimSpace(b.text) != "" {
		parts = append(parts, strings.TrimSpace(b.text))
	}
	if len(b.tools) > 0 {
		parts = append(parts, strings.Join(b.tools, "\n"))
	}
	if len(parts) == 0 {
		return nil
	}
	return []OutMessage{{
		Kind: KindMarkdown,
		Text: strings.Join(parts, "\n\n"),
	}}
}

// --- 消息构造 ---

// approvalMessage 审批卡（卡片按钮优先；无按钮平台由 Send 侧降级为文本指令）。
func approvalMessage(ev *events.ToolApprovalRequested, chat Chat) OutMessage {
	msgs := approvalMsgs[LangZH]
	body := renderToolMsg(msgs.Request, map[string]string{"name": ev.Name}) + "\n```\n" + ev.Arguments + "\n```"
	// Value 携带 approval_id + chat（按钮回调时定位审批）
	val := ev.Id
	return OutMessage{
		Kind: KindCard,
		Card: &Card{
			Title: renderToolMsg(msgs.Title, map[string]string{"name": ev.Name}),
			Body:  body,
			Buttons: []Button{
				{ID: "approve", Label: msgs.Approve, Value: val},
				{ID: "reject", Label: msgs.Reject, Value: val},
				{ID: "later", Label: msgs.Later, Value: val},
			},
		},
	}
}

// toolLine 工具调用信息行（对齐桌面端 ToolRow：名称 + 参数摘要 + 状态 + 变更 + 耗时）。
// 文案走 i18n（im/i18n.go），默认中文。dur 为工具耗时（0 = 未记录不显示）。
func ToolLine(ev *events.ToolResponse, dur time.Duration) string {
	lang := LangZH
	msgs := toolMsgs[lang]
	ic := toolEmoji(ev.Name)
	vars := map[string]string{"icon": ic, "name": ev.Name}
	if ev.IsError {
		line := renderToolMsg(msgs.Err, vars)
		if ev.ErrorCode != "" {
			line += " `" + ev.ErrorCode + "`"
		}
		if d := ToolDur(dur); d != "" {
			line += " · " + renderToolMsg(msgs.Dur, map[string]string{"dur": d})
		}
		return line
	}
	line := renderToolMsg(msgs.OK, vars)
	// 参数摘要（对齐桌面端 toolArgsSummary：bash 命令前 44 字符、文件路径等）
	if s := toolArgsSummary(ev.Name, ev.Arguments); s != "" {
		line += " `" + s + "`"
	}
	// 变更统计（diff）
	if ev.Diff != nil && ev.Diff.Path != "" {
		line += " · " + renderToolMsg(msgs.Changed, map[string]string{
			"path": ev.Diff.Path, "add": fmt.Sprintf("%d", ev.Diff.Added), "del": fmt.Sprintf("%d", ev.Diff.Removed),
		})
	} else if ev.Changed != "" {
		line += " · " + renderToolMsg(msgs.Changed, map[string]string{
			"path": ev.Changed, "add": "?", "del": "?",
		})
	}
	if d := ToolDur(dur); d != "" {
		line += " · " + renderToolMsg(msgs.Dur, map[string]string{"dur": d})
	}
	return line
}

// toolDur 格式化工具耗时：<1s 显示 ms；<60s 显示 1.2s；≥60s 显示 2m30s。
func ToolDur(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm%02ds", m, s)
}

// toolEmoji 工具图标（语义化分类；统一 emoji 视觉风格）。
// 分类：文件📄 命令💻 搜索🔎 任务✅ 交互💬 后台🧵 变更📝 其他🛠
func toolEmoji(name string) string {
	emoji := map[string]string{
		// 文件类
		"read_file":    "📄",
		"write_file":   "📝",
		"edit_file":    "📝",
		"list_dir":     "📁",
		"glob":         "🗂",
		"file_preview": "📄",
		// 命令/执行类
		"bash":              "💻",
		"run":               "▶️",
		"browser":           "🌐",
		"browser_navigate":  "🧭",
		"browser_click":     "🖱",
		"browser_type_text": "⌨️",
		// 搜索类
		"grep": "🔎",
		// 任务/计划类
		"todo_add":    "✅",
		"todo_update": "✅",
		"TaskList":    "🗒",
		"cron":        "⏰",
		// 交互类
		"ask_user":        "💬",
		"agent_spawn":     "🧵",
		"agent_send":      "📨",
		"agent_interrupt": "⏹",
		"TaskOutput":      "📤",
		"load_skill":      "🧩",
		"skills":          "🧩",
		// 其他
		"checkpoint": "💾",
		"git":        "🌿",
	}
	if ic := emoji[name]; ic != "" {
		return ic
	}
	return "🛠"
}

// toolArgsSummary 工具参数摘要（Go 版，对齐前端 lib/toolSummary.ts）：
// bash 命令前 44 字符、文件路径、grep 模式、agent_spawn 任务名等。
func toolArgsSummary(name, args string) string {
	if args == "" {
		return ""
	}
	var a map[string]any
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return "" // 非 JSON（明文串）：不显示
	}
	switch name {
	case "read_file", "write_file", "edit_file", "list_dir":
		if p, _ := a["path"].(string); p != "" {
			return p
		}
	case "grep":
		if p, _ := a["pattern"].(string); p != "" {
			return p
		}
		if p, _ := a["path"].(string); p != "" {
			return p
		}
	case "bash":
		if c, _ := a["command"].(string); c != "" {
			return truncateUTF8(c, 44)
		}
	case "agent_spawn":
		if t, _ := a["task"].(string); t != "" {
			return truncateUTF8(t, 24)
		}
		if p, _ := a["prompt"].(string); p != "" {
			return truncateUTF8(p, 24)
		}
	case "todo_add", "todo_update":
		if t, _ := a["title"].(string); t != "" {
			return truncateUTF8(t, 24)
		}
		if id, _ := a["id"].(string); id != "" {
			return "#" + id
		}
	case "ask_user":
		if qs, ok := a["questions"].([]any); ok && len(qs) > 0 {
			if q, ok := qs[0].(map[string]any); ok {
				if s, _ := q["question"].(string); s != "" {
					return truncateUTF8(s, 32)
				}
			}
		}
	case "glob":
		if p, _ := a["pattern"].(string); p != "" {
			return p
		}
	}
	return ""
}

// truncateUTF8 按字符截断（避免按字节切坏多字节字符）。
func truncateUTF8(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// CardToText 把卡片降级为文本指令（无按钮平台：WhatsApp 等）。
// 返回文本形式（"回复 1 批准 / 2 拒绝"）。
func CardToText(c *Card) string {
	if c == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(c.Title)
	sb.WriteString("\n")
	sb.WriteString(c.Body)
	if len(c.Buttons) > 0 {
		sb.WriteString("\n\n请回复：\n")
		for i, b := range c.Buttons {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, b.Label))
		}
	}
	return sb.String()
}

// ParseTextAction 解析文本指令回复（无按钮平台的审批决策）。
// 输入 "1" / "2" / "批准" / "拒绝" 等，返回 (buttonID, value, ok)。
func ParseTextAction(text string, card *Card) (string, string, bool) {
	if card == nil {
		return "", "", false
	}
	t := strings.TrimSpace(strings.ToLower(text))
	// 数字索引
	for i, b := range card.Buttons {
		if t == fmt.Sprintf("%d", i+1) || t == strings.ToLower(b.Label) {
			return b.ID, b.Value, true
		}
	}
	// 关键词兜底
	switch {
	case strings.Contains(t, "批准") || strings.Contains(t, "approve") || t == "1":
		return "approve", "", len(card.Buttons) > 0
	case strings.Contains(t, "拒绝") || strings.Contains(t, "reject") || t == "2":
		return "reject", "", len(card.Buttons) > 1
	case strings.Contains(t, "稍后") || strings.Contains(t, "later") || t == "3":
		return "later", "", len(card.Buttons) > 2
	}
	return "", "", false
}
