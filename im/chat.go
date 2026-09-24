// Package im 实现 go-code 外部集成 IM Gateway 抽象层（IM 无关）。
//
// 设计（见 docs/IM_INTEGRATION.md）：
//   - 执行对称、控制不对称：任何输入方（桌面 UI / CLI / IM Gateway）都能 Ask/Approve/Interrupt/订阅事件流；
//     但 workspace 管理、配置、绑定、审计等控制面只收口在桌面端。
//   - 能力声明 + 渲染降级：Gateway 声明 Capabilities，Renderer 按能力降级表达，业务层不感知平台差异。
//   - 配置即注册：核心代码不感知具体 IM；新增/启用一个 Gateway = 在 im-config.json 加一条配置，
//     按 type 查注册表实例化。
//   - 显式关联、不全量推送：桌面端手动新建的会话默认不推送任何 IM；只有显式"分享到 IM 聊天"或
//     IM 侧发起的会话才镜像。
//
// 本包不依赖任何具体 IM 平台；平台适配器在 github.com/seven7628/hai-harness/im/<type> 子包（如 im/feishu）。
package im

import (
	"fmt"
	"strings"
	"time"
)

// Chat 唯一标识一个可寻址的 IM 对话端点（路由/镜像的主键）。
//
// 一个 Chat = 群聊 / 单聊 / 话题（thread）端点。路由表按 Chat 寻址，
// 镜像表按 Chat 关联会话。
type Chat struct {
	Gateway  string `json:"gateway"`   // Gateway 类型：feishu / telegram / …
	ChatID   string `json:"chat_id"`   // 平台侧 chat id（飞书 oc_xxx / ou_xxx；TG chat id）
	ThreadID string `json:"thread_id"` // 话题/thread 维度；无话题的平台恒为空
}

// String 规范化表示（日志/审计/UI 显示用）。
func (c Chat) String() string {
	if c.ThreadID != "" {
		return fmt.Sprintf("%s:%s:%s", c.Gateway, c.ChatID, c.ThreadID)
	}
	return fmt.Sprintf("%s:%s", c.Gateway, c.ChatID)
}

// IsZero 判断 Chat 是否为空（未初始化）。
func (c Chat) IsZero() bool {
	return c.Gateway == "" || c.ChatID == ""
}

// Normalize 归一化：去空白、gateway 小写。返回是否有效。
func (c Chat) Normalize() (Chat, bool) {
	c.Gateway = strings.ToLower(strings.TrimSpace(c.Gateway))
	c.ChatID = strings.TrimSpace(c.ChatID)
	c.ThreadID = strings.TrimSpace(c.ThreadID)
	return c, !c.IsZero()
}

// Attachment 消息附件（文件/图片等，v2 扩展）。
type Attachment struct {
	Type     string // "file" | "image" | …
	Name     string
	URL      string
	MimeType string
}

// InMessage 收到的用户消息（已按平台归一化）。
type InMessage struct {
	Chat        Chat
	UserID      string // 平台用户标识（飞书 open_id / TG user id）
	UserName    string
	Text        string   // 纯文本（平台富文本已降级为文本）
	Mentions    []string // 被 @ 的机器人/用户（群聊场景；单聊为空）
	Attachments []Attachment
	Timestamp   time.Time
}

// OutKind 消息种类。
type OutKind string

const (
	KindText     OutKind = "text"      // 纯文本（无 Markdown 渲染的平台）
	KindMarkdown OutKind = "markdown"  // 富文本（卡片正文 / post 富文本）
	KindCard     OutKind = "card"      // 交互卡片（审批等带按钮）
	KindToolCard OutKind = "tool_card" // 工具调用富卡片（结构化：名称/参数/状态/耗时/变更）
	KindFile     OutKind = "file"      // 文件（v2）
)

// Button 交互卡片按钮（审批等）。
type Button struct {
	ID    string // 按钮 id："approve" | "reject" | "later" | …
	Label string // 展示文案
	Value string // 回调载荷（如 approval_id），平台无关
}

// Card 交互卡片。
type Card struct {
	Title   string   // 卡片标题（如「待你批准 · write_file」）
	Body    string   // 卡片正文（markdown）
	Buttons []Button // 按钮（能力缺失时 Renderer 降级为文本指令）
}

// OutMessage 要发送的消息（业务层只表达"发什么"，怎么发由 Gateway 决定）。
type OutMessage struct {
	Kind OutKind
	Text string    // markdown 或纯文本
	Card *Card     // Kind == KindCard 时非空
	Tool *ToolInfo // Kind == KindToolCard 时非空
	File *File     // Kind == KindFile 时非空（v2）
}

// ToolInfo 工具调用信息（KindToolCard：结构化呈现，平台可渲染富卡片）。
type ToolInfo struct {
	Name     string // 工具名（bash/read_file/...）
	Icon     string // emoji 图标（平台可用）
	Args     string // 参数摘要（bash 命令/文件路径等）
	Status   string // ok | error
	Error    string // 错误码（Status=error 时）
	Dur      string // 耗时（"3.4s"/"12ms"）
	DiffPath string // 变更文件（可空）
	DiffAdd  int    // 新增行（DiffPath 非空时）
	DiffDel  int    // 删除行
}

// File 文件消息（v2）。
type File struct {
	Name string
	URL  string
	Size int64
}

// GatewayAction 按钮回调（审批等）。
type GatewayAction struct {
	Chat      Chat
	MessageID string
	ButtonID  string
	Value     string
	UserID    string
	Timestamp time.Time
}
