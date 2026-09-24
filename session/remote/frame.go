package remote

import "time"

// 帧协议（事件信封模型，docs/SESSION_MESH_COLLABORATION.md §4.6.3）。
// 所有帧为 JSON 对象，type 区分；业务载荷（event 帧 payload）复用 events.Event 形态。

// FrameType 帧类型。
type FrameType string

const (
	FrameHello       FrameType = "hello"        // 连接握手（携带 node_id/sessions/token/version）
	FrameHelloAck    FrameType = "hello_ack"    // 握手确认（携带 node_id/sessions/nodes/version）
	FramePing        FrameType = "ping"         // 心跳
	FramePong        FrameType = "pong"         // 心跳回包
	FrameBye         FrameType = "bye"          // 优雅断开
	FrameEvent       FrameType = "event"        // 业务事件（payload = events.Event 形态）
	FrameEventAck    FrameType = "event_ack"    // 事件送达回执（msg_id + ok + error）
	FrameListReq     FrameType = "list_req"     // 请求对端会话列表
	FrameSessionList FrameType = "session_list" // 会话列表响应/推送
	FrameTopo        FrameType = "topo"         // 拓扑交换（gossip）
	FrameNodeOffline FrameType = "node_offline" // 节点离线广播
)

// Frame 统一信封：type + 可选载荷字段。
type Frame struct {
	Type FrameType `json:"type"`

	// --- 握手 ---
	NodeID   string        `json:"node_id,omitempty"`  // 本机/对端节点 id
	Sessions []SessionInfo `json:"sessions,omitempty"` // 在线会话摘要
	Token    string        `json:"token,omitempty"`    // 握手凭据（仅 hello）
	Version  int           `json:"version,omitempty"`  // 协议版本（hello/hello_ack）
	Nodes    []NodeInfo    `json:"nodes,omitempty"`    // 已知节点摘要（hello_ack/topo）

	// --- 心跳 ---
	T int64 `json:"t,omitempty"`

	// --- event / event_ack ---
	TargetNode     string          `json:"target_node,omitempty"`
	TargetSession  string          `json:"target_session,omitempty"`
	Hops           int             `json:"hops,omitempty"`            // 转发计数
	TTL            int             `json:"ttl,omitempty"`             // 转发上限（默认 3）
	Payload        *InboundMessage `json:"payload,omitempty"`         // event 帧：业务载荷
	MsgID          string          `json:"msg_id,omitempty"`          // event_ack 锚点
	OK             *bool           `json:"ok,omitempty"`              // event_ack 结果
	Error          string          `json:"error,omitempty"`           // event_ack 错误载荷
	CreatedSession string          `json:"created_session,omitempty"` // event_ack：目标节点自动创建的新会话 id（target 无 sid 时）

	// --- bye / node_offline ---
	Reason string `json:"reason,omitempty"`
}

// protocolVersion 当前协议版本（hello/hello_ack 协商；不兼容断开）。
const protocolVersion = 1

// AckError 事件回执错误码（event_ack.error 载荷；可重试/不可重试分类见 §4.6.4）。
const (
	AckOK              = ""
	AckSessionNotFound = "session_not_found" // 不可重试
	AckSessionClosed   = "session_closed"    // 不可重试
	AckQueueFull       = "queue_full"        // 可重试
	AckBudgetExceeded  = "budget_exceeded"   // 可重试
	AckTTLExceeded     = "ttl_exceeded"      // 不可重试（转发环）
	AckBadEvent        = "bad_event"         // 不可重试（未知 event_type）
	AckNodeOffline     = "node_offline"      // 可重试（下一跳/目标瞬时不可达；C2/C3 修复）
)

// DefaultTTL event 帧默认转发上限（防环）。
const DefaultTTL = 3

// sessionBroadcastInterval 会话摘要周期广播间隔：握手之外，直连节点间的会话变更靠
// 周期 topo 推送同步（session_list 不再只是"握手快照"）。30s 与心跳同频，开销可忽略
// （全量摘要很小）。测试可用 setSessionBroadcastIntervalForTest 缩短。
var sessionBroadcastInterval = 30 * time.Second

// setSessionBroadcastIntervalForTest 测试专用：缩短周期广播间隔（不并发安全；仅测试用）。
func setSessionBroadcastIntervalForTest(d time.Duration) { sessionBroadcastInterval = d }
