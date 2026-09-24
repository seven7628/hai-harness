package remote

// 路由（docs/SESSION_MESH_COLLABORATION.md §4.6.4）：
// 直连优先；无直连时找「有到目标节点链路」的中继节点做一跳转发；都无 → ErrNodeOffline。

import (
	"errors"
	"sync"
)

// ErrNodeOffline 目标节点不可达（无直连、无中继）。
var ErrNodeOffline = errors.New("target node offline")

// ErrLinkDuplicated 出站拨号被拒：同节点已有异向活跃链路（对端已连入本机，
// 全互连双向配置的防乒乓信号）。连接实际健康，调用方应长退避而非高频重连。
var ErrLinkDuplicated = errors.New("duplicate opposite link (peer already connected inbound)")

// Router 一跳转发路由：维护 node_id → 下一跳（直连节点）。线程安全。
type Router struct {
	mu      sync.RWMutex
	direct  map[string]string // node_id → 直连节点 id（= 自身可达的链路另一端）
	nextHop map[string]string // node_id → 中继下一跳（经该中继可达；首版一跳）
	offline map[string]bool   // 近期判定离线节点（避免反复尝试）
}

// NewRouter 创建路由表。
func NewRouter() *Router {
	return &Router{
		direct:  make(map[string]string),
		nextHop: make(map[string]string),
		offline: make(map[string]bool),
	}
}

// SetDirect 登记直连节点（握手成功时调用）：nodeID ↔ 直连。
func (r *Router) SetDirect(nodeID, via string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.direct[nodeID] = via
	delete(r.nextHop, nodeID) // 直连后不再走中继
	delete(r.offline, nodeID)
}

// SetRelay 登记经中继可达的节点（拓扑交换时调用）：nodeID 经 via 中继。
func (r *Router) SetRelay(nodeID, via string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.direct[nodeID]; ok {
		return // 直连优先，不覆盖
	}
	r.nextHop[nodeID] = via
}

// NextHop 目标节点下一跳：直连优先 → 中继；均无返回 ""。
func (r *Router) NextHop(nodeID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if via, ok := r.direct[nodeID]; ok {
		return via
	}
	return r.nextHop[nodeID]
}

// MarkOffline 标记节点离线（心跳超时/断开）：清直连与中继条目。
func (r *Router) MarkOffline(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.direct, nodeID)
	delete(r.nextHop, nodeID)
	r.offline[nodeID] = true
}

// IsOffline 节点是否被标记离线。
func (r *Router) IsOffline(nodeID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.offline[nodeID]
}

// Clear 清空全部路由（网关 Stop 时）。
func (r *Router) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.direct = make(map[string]string)
	r.nextHop = make(map[string]string)
	r.offline = make(map[string]bool)
}

// DirectNodes 全部直连节点（排障/状态）。
func (r *Router) DirectNodes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.direct))
	for n := range r.direct {
		out = append(out, n)
	}
	return out
}
