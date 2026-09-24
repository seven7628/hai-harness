package remote

import (
	"context"
	"time"
)

// NodeSender 是 Session Hub 对远程网关注入面的最小接口（Gateway 实现）。
// 定义在 remote 包内：session.Hub import remote（remote 不 import session），
// 无循环依赖；同时 Gateway 天然满足它。
type NodeSender interface {
	// SendMessage 向目标节点上的会话投递一条消息（直连，或经一跳转发路由）。
	// targetSID 为空 = 请求目标节点自动创建新会话（返回 createdSID = 新会话 id）。
	// 返回 error：ErrNodeOffline（目标不可达）/ 传输错误。
	SendMessage(ctx context.Context, targetNodeID, targetSID string, msg *InboundMessage) (createdSID string, err error)
	// KnownNodes 已知节点及其在线会话摘要（session_list 远程部分）。
	KnownNodes(ctx context.Context) []NodeInfo
	// DirectLinks 直连链路状态（排障 / session_remote_status）。
	DirectLinks(ctx context.Context) []LinkInfo
	// PeerStatuses 配置的出站节点（peer）拨号状态（含最近错误；mesh_status 展示）。
	PeerStatuses(ctx context.Context) []PeerStatus
	// PingNode 主动对目标节点做一次往返 ping（连接检测 / 排障）。
	// 返回 RTT；节点不可达返回 ErrNodeOffline。
	PingNode(ctx context.Context, targetNodeID string) (time.Duration, error)
}

// InboundMessage session_send 的投递载荷。
// 事件信封：目标节点解包后重建 events.Event（TaskMessage）走 emit 漏斗。
type InboundMessage struct {
	// EventType 复用现有事件类型（首版恒 task_message）。
	EventType string `json:"event_type"`
	// RunId 来源运行（回信/审计/UI 归树关联）。
	RunId string `json:"run_id,omitempty"`
	// SessionId 目标 session id（emit 注入同键）。
	SessionId string `json:"session_id"`
	// Workspace 目标 workspace（emit 注入同键）。
	Workspace string `json:"workspace,omitempty"`

	// FromNode 来源节点（本机=nodeID；远程=对端 node id）。
	FromNode string `json:"from_node"`
	// FromSession 来源 session id（回信寻址）。
	FromSession string `json:"from_session"`
	// FromName 来源会话展示名/标签（可选）。
	FromName string `json:"from_name,omitempty"`
	// Body 消息正文（LLM 文本；不透明字符串）。
	Body string `json:"body"`
	// SendAt 发送时间。
	SendAt time.Time `json:"send_at"`
	// MsgID 消息唯一 id（去重/回执锚点）。
	MsgID string `json:"msg_id"`
}

// SessionInfo 可寻址会话摘要（session_list 项；本机 + 远程合并）。
type SessionInfo struct {
	// Node 归属节点（本机=本地 nodeID；远程=对端 node id）。
	Node string `json:"node"`
	// ID 会话 id。
	ID string `json:"id"`
	// Name 会话展示名/标签（可选；hide_session_names 时远程侧为空）。
	Name string `json:"name,omitempty"`
	// Status online / busy。
	Status string `json:"status"`
	// LastSeen 最近心跳时间（远程节点；本机=now）。
	LastSeen time.Time `json:"last_seen,omitempty"`
}

// NodeInfo 已知节点及其在线会话摘要（拓扑交换 / KnownNodes）。
type NodeInfo struct {
	ID       string        `json:"id"`
	Sessions []SessionInfo `json:"sessions"`
	LastSeen time.Time     `json:"last_seen"`
}

// LinkInfo 直连链路状态（排障 / session_remote_status）。
type LinkInfo struct {
	NodeID      string    `json:"node_id"`
	Inbound     bool      `json:"inbound"` // true=对方连我；false=我连对方
	ConnectedAt time.Time `json:"connected_at"`
	LastSeen    time.Time `json:"last_seen"`
}

// PeerStatus 配置的出站节点（peer）拨号状态（mesh_status 排障展示）。
// Connected=true 表示当前存在活跃直连链路；否则展示最近一次拨号错误原因。
type PeerStatus struct {
	// ID 配置的 peer id（出站目标节点 id）。
	ID string `json:"id"`
	// Addr 配置的连接地址。
	Addr string `json:"addr"`
	// Connected 当前是否已连上（直连链路活跃）。
	Connected bool `json:"connected"`
	// LastError 最近一次拨号/握手失败原因（空 = 无失败或已连上）。
	LastError string `json:"last_error,omitempty"`
	// LastAttempt 最近一次拨号尝试时间。
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	// LastSuccess 最近一次成功连上时间（无 = 从未连上）。
	LastSuccess time.Time `json:"last_success,omitempty"`
	// RTTMs 最近一次主动 ping 往返耗时毫秒（仅最近 PingNode 后填充；-1 = 未知）。
	RTTMs int64 `json:"rtt_ms,omitempty"`
}
