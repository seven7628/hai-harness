package im

import (
	"sync"
)

// SessionMeta 会话来源元数据（扩展存储记录，新增可选字段，不改变既有格式语义）。
// 侧栏按 Source 分组显示；IM 会话带 GatewayChat 标识。
type SessionMeta struct {
	Source      string `json:"source,omitempty"`       // local | feishu | telegram | …
	GatewayChat *Chat  `json:"gateway_chat,omitempty"` // 发起该会话的 Chat（IM 发起时非空）
	Mirror      string `json:"mirror,omitempty"`       // none | push | bidirectional
}

// Router 路由与镜像的运行时视图（从 Config 加载，支持运行期热更新）。
//
// 职责：
//   - 路由：Chat → workspace（IM 消息该驱动哪个工作区）
//   - 镜像：session ↔ Chat（哪些会话推送到哪个聊天框，双向/单向）
//
// 线程安全：内部互斥，配置热更新（Update）与查询并发安全。
type Router struct {
	mu       sync.RWMutex
	routes   []RouteConfig
	mirrors  []MirrorConfig
	byChat   map[Chat]string           // Chat → workspace（路由快照）
	byMirror map[string][]MirrorConfig // session_id → 镜像（快照）
}

// NewRouter 从配置构建 Router。
func NewRouter(cfg Config) *Router {
	r := &Router{}
	r.Update(cfg)
	return r
}

// Update 用新配置重建快照（热更新入口）。
func (r *Router) Update(cfg Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// 路由是渠道维度：聚合所有 Gateway 实例的 Routes
	var routes []RouteConfig
	for _, g := range cfg.Gateways {
		routes = append(routes, g.Routes...)
	}
	r.routes = routes
	r.mirrors = append([]MirrorConfig(nil), cfg.Mirrors...)
	r.byChat = make(map[Chat]string, len(routes))
	for _, rc := range routes {
		if c, ok := rc.Chat.Normalize(); ok {
			r.byChat[c] = rc.Workspace
		}
	}
	r.byMirror = make(map[string][]MirrorConfig, len(cfg.Mirrors))
	for _, mc := range cfg.Mirrors {
		r.byMirror[mc.SessionID] = append(r.byMirror[mc.SessionID], mc)
	}
}

// RouteFor 查 Chat 路由（命中返回 workspace + true）。
func (r *Router) RouteFor(chat Chat) (string, bool) {
	c, ok := chat.Normalize()
	if !ok {
		return "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	ws, ok := r.byChat[c]
	return ws, ok
}

// MirrorsForSession 查某会话的全部镜像（可关联多个 Chat）。
func (r *Router) MirrorsForSession(sessionID string) []MirrorConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]MirrorConfig(nil), r.byMirror[sessionID]...)
}

// MirrorsForChat 查某 Chat 关联的全部镜像（一个 Chat 可关联多个会话）。
func (r *Router) MirrorsForChat(chat Chat) []MirrorConfig {
	c, ok := chat.Normalize()
	if !ok {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []MirrorConfig
	for _, m := range r.mirrors {
		if m.Chat.Gateway == c.Gateway && m.Chat.ChatID == c.ChatID && m.Chat.ThreadID == c.ThreadID {
			out = append(out, m)
		}
	}
	return out
}

// ShouldMirror 判断某会话是否需要推送到某 Chat（镜像模式决定）。
func ShouldMirror(m MirrorMode) bool {
	return m == MirrorPush || m == MirrorBidirectional
}

// SessionMetaFor 生成会话来源元数据（IM 发起会话时调用）。
func SessionMetaFor(chat Chat, mode MirrorMode) SessionMeta {
	c, _ := chat.Normalize()
	cc := c // copy
	return SessionMeta{
		Source:      c.Gateway,
		GatewayChat: &cc,
		Mirror:      string(mode),
	}
}
