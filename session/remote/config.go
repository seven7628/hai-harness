// Package remote 提供 Session Mesh 的远程多节点通信组件：
// Settings/Peer 配置类型、Gateway（WS server+client）、Link 多连接管理、
// 帧协议编解码与一跳转发路由。
//
// 包归属设计（docs/SESSION_MESH_COLLABORATION.md §4.1）：
// Settings/Peer 类型定义在本包（与 Gateway 同包），因为 session.Hub 与
// remote.Gateway 都要消费它，而它们不能 import desktop 层（desktop import
// session，反向会循环依赖）。desktop/bridge 只负责「从 settings.json 读
// mesh 段 → 产出 remote.Settings」。
package remote

// Settings settings.json 顶层 mesh.remote 段（Session Mesh 远程多节点配置）。
// 首版最小集 + 后续增强字段（敏感扫描/会话名隐藏）已预留。
type Settings struct {
	// ListenAddr 本节点作为 server 的监听地址（缺省 127.0.0.1:17890，防意外暴露）。
	ListenAddr string `json:"listen_addr"`
	// TLSCert / TLSKey 非空 → wss://；空 → ws://（仅受信网络；跨公网必须配 TLS）。
	TLSCert string `json:"tls_cert"`
	TLSKey  string `json:"tls_key"`
	// AuthToken 握手凭据（两端必须一致；空 = 拒绝连接，安全默认）。
	AuthToken string `json:"auth_token"`
	// NodeID 本机节点标识（"auto" = 首次启动生成 UUID 持久化到 ~/.go-code/mesh_node_id）。
	NodeID string `json:"node_id"`
	// Peers 本机作为客户端主动连接的对端节点列表（出站连接）。
	Peers []Peer `json:"peers"`
	// HideSessionNames 后续增强：向对端广播时隐藏本机会话名（只留 id）。
	HideSessionNames bool `json:"hide_session_names"`
	// SensitiveRules 后续增强：出站敏感扫描正则（空 = 内置默认规则表）。
	SensitiveRules []string `json:"sensitive_rules"`
}

// Peer 一个远程对端节点（出站连接配置）。
type Peer struct {
	// ID 对端节点 id（用于寻址去歧义；须与对端 NodeID 一致）。
	ID string `json:"id"`
	// Addr 对端监听地址（ws:// 或 wss://）。
	Addr string `json:"addr"`
	// Token 连接对端时使用的握手凭据（须与对端 AuthToken 一致）。
	Token string `json:"token"`
}
