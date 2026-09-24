package im

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Status Gateway 连接状态。
type Status string

const (
	StatusDisconnected Status = "disconnected"
	StatusConnecting   Status = "connecting"
	StatusConnected    Status = "connected"
	StatusError        Status = "error"
)

// Capabilities 能力声明（Renderer 降级的依据）。
type Capabilities struct {
	CardButtons   bool // 交互按钮（审批卡片）
	UpdateMessage bool // 可更新已发消息（类流式/占位刷新）
	Thread        bool // 话题/thread 多会话
	Markdown      bool // 富文本渲染
	Files         bool // 文件发送
	Streaming     bool // 流式输出（ContentChunk 实时 Append，非攒批）
}

// Streamer 流式发送能力（Capabilities.Streaming=true 时非 nil）。
type Streamer interface {
	// Start 开一条流（先发占位消息），返回控制器。title 为占位标题（可空）。
	Start(ctx context.Context, chat Chat, title string) (StreamController, error)
}

// StreamController 流式控制器：追加/冲刷/收尾。
type StreamController interface {
	// Append 追加文本到流尾（实现方负责节流）。
	Append(ctx context.Context, text string) error
	// Flush 立即推送缓冲内容。
	Flush(ctx context.Context) error
	// Close 完成流（收尾；之后不可再操作）。
	Close(ctx context.Context) error
}

// Gateway 一个具体 IM 平台的接入适配器。实现者只负责：收发 + 回调 + 能力声明。
//
// 实现一个 Gateway 的最低要求：Type/Name/Start/Stop/Status/Send/OnMessage + Capabilities；
// Update/OnAction/Files 按平台能力实现，无能力则返回 ErrUnsupported / 不注册回调。
type Gateway interface {
	Type() string // "feishu" / "telegram" / …
	Name() string // 展示名："飞书"

	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Status() Status

	Capabilities() Capabilities

	// Send 发送消息，返回平台侧消息 id（供 Update 用）。
	Send(ctx context.Context, chat Chat, msg OutMessage) (messageID string, err error)
	// Update 更新已发消息（能力缺失时返回 ErrUnsupported）。
	Update(ctx context.Context, messageID string, msg OutMessage) error
	// Streamer 流式发送器；不支持返回 nil（Capabilities.Streaming=false）。
	Streamer() Streamer

	// OnMessage 注册收到消息的回调（单聊/群聊）。
	OnMessage(handler func(InMessage))
	// OnAction 注册按钮回调（审批等）。
	OnAction(handler func(GatewayAction))
}

// ErrUnsupported 能力缺失时返回。
var ErrUnsupported = errors.New("im: unsupported by gateway")

// ErrUnknownType 注册表查不到该 Gateway 类型。
var ErrUnknownType = errors.New("im: unknown gateway type")

// GatewayConstructor 构造器：接收该 Gateway 类型的 config（json.RawMessage），返回实例。
type GatewayConstructor func(cfg json.RawMessage) (Gateway, error)

// registry 内建 Gateway 注册表：type 字符串 → 构造器。
// 新增平台：实现 Gateway + 在此注册（v1 需重编译；v2 可设计外部进程协议，见 docs §10）。
var (
	registryMu sync.RWMutex
	registry   = map[string]GatewayConstructor{}
	schemas    = map[string]GatewaySchema{}
)

// Register 注册一个 Gateway 类型（包 init 或显式调用）。重复注册 panic（编程错误）。
// 若 ctor 构造出的实例实现了 GatewayWithSchema，其 Schema 自动登记（Schemas 返回）。
func Register(typ string, ctor GatewayConstructor) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[typ]; dup {
		panic(fmt.Sprintf("im: gateway type %q already registered", typ))
	}
	registry[typ] = ctor
	// Schema 独立登记：不依赖构造器探测（空配置可能构造失败，如 feishu 缺 app_id）。
	// 通过类型断言的注册表模式：构造器无法在无配置时给 Schema，故用 RegisterSchema 显式登记。
}

// New 按 type 构造 Gateway（配置即注册：type 查注册表）。
func New(typ string, cfg json.RawMessage) (Gateway, error) {
	registryMu.RLock()
	ctor, ok := registry[typ]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownType, typ)
	}
	return ctor(cfg)
}

// Types 返回全部已注册 Gateway 类型（排序稳定）。
func Types() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// RegisterSchema 显式登记某 Gateway 类型的配置 Schema（UI 动态表单数据源）。
// Gateway 实现方可直接在包 init 里调用，避免依赖构造器探测（空配置可能构造失败）。
// 重复登记同 type 允许（幂等，后登记覆盖——便于测试注入）。
func RegisterSchema(typ string, s GatewaySchema) {
	registryMu.Lock()
	defer registryMu.Unlock()
	s.Type = typ // 强制对齐 type（防调用方笔误）
	schemas[typ] = s
}

// Schemas 返回全部已注册 Gateway 的配置 Schema（前端动态表单数据源）。
// 来源：RegisterSchema 显式登记；未登记的 type 回退为最小 Schema（仅 type 字段）。
func Schemas() []GatewaySchema {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]GatewaySchema, 0, len(registry))
	for t := range registry {
		if s, ok := schemas[t]; ok {
			out = append(out, s)
		} else {
			out = append(out, GatewaySchema{Type: t})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// ChatLister 可选接口：Gateway 支持列出可绑定的聊天（群列表，供路由绑定下拉）。
type ChatLister interface {
	ListChats(ctx context.Context) ([]ChatInfo, error)
}

// ChatInfo 可绑定聊天信息（渠道无关）。
type ChatInfo struct {
	ChatID string `json:"chat_id"`
	Name   string `json:"name"`
}
