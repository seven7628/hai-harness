// Package remote 的 Gateway：Session Mesh 远程多节点通信核心。
//
// 拓扑（docs/SESSION_MESH_COLLABORATION.md §4.6）：每节点同时是 server（监听入站）
// 与 client（按 peers 主动出站），多个节点互连成 mesh；消息直连优先，无直连时
// 经一跳中继转发（Router）。帧协议为事件信封（frame.go）：event 帧 payload 复用
// events.Event 形态，目标节点解包后走宿主 emit 漏斗。
//
// 安全（首版最小集 §4.6.5）：握手 token 校验（每条连接）+ 监听默认回环 + TLS 可选。
package remote

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// EventHandler 目标节点收到远程事件后的回调（宿主接线：解包 → 投递本机会话）。
type EventHandler func(ctx context.Context, fromNodeID string, msg *InboundMessage)

// Gateway 多节点 mesh 网关（server + client）。实现 NodeSender。
// 线程安全：内部 RWMutex。
type Gateway struct {
	cfg             Settings
	nodeID          string // 本机节点 id（握手广播用）
	onEvent         EventHandler
	sessionProvider func() []SessionInfo // 本机会话摘要（远程 session_list 数据源；nil = 空）
	router          *Router
	eventAcks       map[string]chan ackResult // msg_id → ack 等待通道（发送等待回执）
	seenMsgID       map[string]struct{}       // 接收去重集合（近期 msg_id，真 LRU）
	seenOrder       []string                  // 去重插入序（LRU 淘汰用；N3 修复）
	ackRoutes       map[string]string         // 中继转发：msg_id → 回程链路节点（event_ack 回传用）
	peerStatus      map[string]*peerState     // 配置 peer → 拨号状态（排障：最近错误/成功/尝试）
	pingWaits       map[string]*pingWait      // 主动 ping 等待：node_id → 等待（PingNode 用）
	peerTicker      *time.Ticker              // 周期会话摘要广播（sessionBroadcastInterval）
	stopPeerTicker  chan struct{}             // 停周期广播（Stop 时关闭）
	retryPeers      chan struct{}             // 链路断开信号：dialLoop 收到即重置退避重试（容量 1）

	mu         sync.RWMutex
	listener   net.Listener
	httpSrv    *http.Server
	links      map[string]*link // node_id → 活跃链路（每节点一条，握手后登记）
	knownNodes map[string]*NodeInfo
	started    bool
	closed     bool

	writeMu sync.Mutex // 序列化 WS 写（wsjson 非并发安全）
}

// link 一条活跃链路（入站或出站；对端经握手认证）。
type link struct {
	nodeID    string
	inbound   bool
	conn      *websocket.Conn
	connected time.Time
	lastSeen  time.Time
	ctx       context.Context
	cancel    context.CancelFunc
	// writeMu 每链路写锁（C4 修复：全局 writeMu 会让单慢连接阻塞全网；
	// nhooyr 要求每连接单写者，per-link 锁隔离慢 peer）。
	writeMu sync.Mutex
}

// ackResult event_ack 回执结果。
type ackResult struct {
	ok      bool
	error   string
	created string // 目标节点自动创建的新会话 id（target 无 sid 时）
}

// peerState 单个配置 peer（出站目标）的拨号状态（排障/设置页展示）。
type peerState struct {
	addr        string
	connected   bool
	lastError   string
	lastAttempt time.Time
	lastSuccess time.Time
	rttMs       int64 // 最近一次主动 ping RTT；0 = 未 ping 过
}

// NewGateway 创建网关。nodeID 为本机节点 id；onEvent 为收到远程事件后的回调
// （可为 nil = 只通不投递）。启动前须调用 Start。
// 可用 SetSessionProvider 注入本机会话摘要（远程 session_list 数据源；缺省空）。
func NewGateway(cfg Settings, nodeID string, onEvent EventHandler) *Gateway {
	g := &Gateway{
		cfg:        cfg,
		nodeID:     nodeID,
		onEvent:    onEvent,
		router:     NewRouter(),
		eventAcks:  make(map[string]chan ackResult),
		ackRoutes:  make(map[string]string),
		links:      make(map[string]*link),
		knownNodes: make(map[string]*NodeInfo),
		peerStatus: make(map[string]*peerState),
		pingWaits:  make(map[string]*pingWait),
		retryPeers: make(chan struct{}, 1),
	}
	for _, p := range cfg.Peers {
		g.peerStatus[p.ID] = &peerState{addr: p.Addr}
	}
	return g
}

// SetSessionProvider 注入本机会话摘要提供者（远程 session_list 数据源）。
// 桌面层接线为 hub.List 的本机部分。nil = 缺省空（不广播会话）。
func (g *Gateway) SetSessionProvider(fn func() []SessionInfo) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sessionProvider = fn
}

// ---------- 生命周期 ----------

// Start 启动网关：起监听（server）+ 起全部出站连接（client）。幂等。
// 支持 Stop 后再次 Start（热更新 off→on 重启网关；closed 是"关闭中"瞬态，
// Stop 结束时复位，不永久闩住——修复 M1）。
func (g *Gateway) Start(ctx context.Context) error {
	g.mu.Lock()
	if g.started || g.closed {
		g.mu.Unlock()
		return nil
	}
	g.started = true
	g.mu.Unlock()

	g.ensureNodeID()
	// server
	if err := g.startServer(); err != nil {
		g.Stop()
		return err
	}
	// client：每个 peer 独立 goroutine（带退避重连）
	for _, p := range g.cfg.Peers {
		go g.dialLoop(ctx, p)
	}
	// 周期会话摘要广播（对端 session_list 实时性；握手快照之外的变化靠它同步）
	g.startSessionBroadcast(ctx)
	return nil
}

// Stop 关闭网关：关监听 + 断全部链路 + 清路由。幂等。
// **不永久闩住**：结束后 closed 复位 false，Start 可再次启动（热更新要求）。
func (g *Gateway) Stop() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true // “关闭中”瞬态：阻止并发 Start 重入
	ln := g.listener
	srv := g.httpSrv
	links := make([]*link, 0, len(g.links))
	for _, l := range linksList(g.links) {
		links = append(links, l)
	}
	g.links = make(map[string]*link)
	g.mu.Unlock()

	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
	}
	if ln != nil {
		_ = ln.Close()
	}
	for _, l := range links {
		l.cancel()
		// N6：优雅断开先发 bye（尽力而为；对端收到即自行断开，不再重连本侧）
		_ = g.sendFrameLink(l.ctx, l, Frame{Type: FrameBye, Reason: "gateway stop"})
		_ = l.conn.Close(websocket.StatusNormalClosure, "gateway stop")
	}
	g.router.Clear()
	// 停周期广播 goroutine
	if g.stopPeerTicker != nil {
		close(g.stopPeerTicker)
		g.stopPeerTicker = nil
	}
	// 复位：允许 Start 再次启动（hot off→on）
	g.mu.Lock()
	g.started = false
	g.closed = false
	g.listener = nil
	g.httpSrv = nil
	g.mu.Unlock()
}

// startSessionBroadcast 起周期会话摘要广播：每 sessionBroadcastInterval 向全部直连节点
// 推送本机最新在线会话（topo 帧带 Sessions）。这样"连接建立后新建/关闭会话"也能在
// 对端 session_list 中及时可见，不再只依赖握手瞬间快照。goroutine 随 Stop 退出。
func (g *Gateway) startSessionBroadcast(ctx context.Context) {
	g.mu.Lock()
	if g.peerTicker != nil {
		g.mu.Unlock()
		return // 已在跑
	}
	g.peerTicker = time.NewTicker(sessionBroadcastInterval)
	stop := make(chan struct{})
	g.stopPeerTicker = stop
	g.mu.Unlock()
	go func() {
		for {
			select {
			case <-ctx.Done():
				g.stopSessionBroadcast()
				return
			case <-stop:
				return
			case <-g.peerTicker.C:
				// 只广播本机直连（Sessions 填本机在线摘要；拓扑摘要给对端更新 knownNodes）
				g.broadcastTopo()
			}
		}
	}()
}

func (g *Gateway) stopSessionBroadcast() {
	g.mu.Lock()
	if g.stopPeerTicker != nil {
		close(g.stopPeerTicker)
		g.stopPeerTicker = nil
	}
	g.mu.Unlock()
}

// ensureNodeID 缺省 node id 兜底（Start 前由宿主解析；此处仅防御空值）。
func (g *Gateway) ensureNodeID() {
	if g.nodeID == "" {
		g.nodeID = fmt.Sprintf("node-%d", time.Now().UnixNano())
	}
}

// startServer 起 HTTP server + WS accept 处理器（监听 ListenAddr）。
// ListenAddr 为空 = 纯客户端节点（不监听入站；如 dial-only 工作机）。
// 注意：远程开启时宿主（loadMeshSettings）会补默认 127.0.0.1:17890；
// 此处空串仍视为"不监听"，由宿主保证默认值。
//
// TLS（第三问修复）：cert/key **全部配置**才启用 wss；只配一半 / 证书文件不存在
// → **返回错误**（Start 传播 → applyMeshSettings 发 mesh_status ok:false，前端可见），
// 绝不静默。都留空 = 纯 ws（不报错，内网/受信网络可用）。
func (g *Gateway) startServer() error {
	addr := g.cfg.ListenAddr
	if addr == "" {
		return nil // 纯客户端：不监听
	}
	mux := http.NewServeMux()
	// mesh 专用监听：根路径即 WS 端点（对端可配任意路径，兼容性最好）
	mux.HandleFunc("/", g.handleWS)
	srv := &http.Server{Handler: mux}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("mesh listen %s: %w", addr, err)
	}
	g.mu.Lock()
	g.listener = ln
	g.httpSrv = srv
	g.mu.Unlock()

	// TLS 配置校验（同步，失败即返回错误——不静默）
	tlsCert, tlsKey := g.cfg.TLSCert, g.cfg.TLSKey
	switch {
	case tlsCert == "" && tlsKey == "":
		// 均未配 = ws（允许，内网/受信网络）；不报错
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("mesh server: %v", err)
			}
		}()
		return nil
	case tlsCert == "" || tlsKey == "":
		// 只配一半：配置错误，立即失败（不静默）
		_ = ln.Close()
		return fmt.Errorf("mesh TLS: cert 与 key 必须同时配置（当前 cert=%q key=%q）", tlsCert, tlsKey)
	}
	// 证书文件预检（存在且可解析）：失败立即返回，ServeTLS 不再异步吞错
	if _, err := tls.LoadX509KeyPair(tlsCert, tlsKey); err != nil {
		_ = ln.Close()
		return fmt.Errorf("mesh TLS: 加载证书失败（路径 %s / %s）: %w", tlsCert, tlsKey, err)
	}
	go func() {
		if err := srv.ServeTLS(ln, tlsCert, tlsKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("mesh TLS server: %v", err)
		}
	}()
	return nil
}

// handleWS 接受入站 WS 连接：握手认证 → 登记链路 → 读循环。
func (g *Gateway) handleWS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // 自签网格内网场景；TLS 由上层保证
	})
	if err != nil {
		return
	}
	// 握手（带超时）
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var hf Frame
	if err := wsjson.Read(ctx, c, &hf); err != nil {
		_ = c.Close(websocket.StatusPolicyViolation, "handshake read failed")
		return
	}
	if hf.Type != FrameHello {
		_ = c.Close(websocket.StatusPolicyViolation, "expected hello")
		return
	}
	// token 认证（空 token 拒绝一切远程连接；安全默认）
	if g.cfg.AuthToken == "" || hf.Token != g.cfg.AuthToken {
		_ = c.Close(websocket.StatusPolicyViolation, "auth failed")
		return
	}
	nodeID := hf.NodeID
	if nodeID == "" || nodeID == g.nodeID {
		_ = c.Close(websocket.StatusPolicyViolation, "bad node id")
		return
	}
	// 登记链路（入站）；被拒（同节点已有异向链路，防乒乓）→ 直接关闭返回
	l, ok := g.registerLink(nodeID, c, true)
	if !ok {
		return
	}
	// 记录对端在线会话（远程 session_list 数据源）
	g.upsertPeerSessions(nodeID, hf.Sessions)
	// 回 hello_ack（带本机会话摘要 + 已知节点）；仍在握手 ctx 内（10s 超时）
	ack := Frame{
		Type:     FrameHelloAck,
		NodeID:   g.nodeID,
		Version:  protocolVersion,
		Sessions: g.localSessions(),
		Nodes:    g.knownNodesSummary(),
	}
	if err := g.sendFrame(ctx, c, ack); err != nil {
		g.unregisterLink(nodeID, l)
		return
	}
	// 拓扑交换：把对端已知节点登记为中继
	for _, n := range hf.Nodes {
		if n.ID != g.nodeID && n.ID != nodeID {
			g.router.SetRelay(n.ID, nodeID)
		}
	}
	// 广播对端节点上线（拓扑变化）
	g.broadcastTopo()
	// 读循环（阻塞）：**绝不复用握手 ctx** —— 握手 ctx 带 10s 超时，
	// 若被 readLoop 使用会导致每个入站连接恰好存活 10s 后被 deadline 踢下线
	//（C1 review：入站 mesh 链路 10s 必死）。改用链路级 ctx（l.ctx，
	// 由 registerLink 派生；Stop/unregisterLink 时 cancel 结束读循环）。
	g.readLoop(l.ctx, c, l)
}

// dialLoop 出站连接循环：拨号 + 握手；断线按退避重连直到 Stop/进程退出。
// 每次失败记录 peerStatus（设置页展示拨号错误原因）。
func (g *Gateway) dialLoop(ctx context.Context, p Peer) {
	backoff := time.Second
	for {
		if g.isClosed() {
			return
		}
		conn, _, l, err := g.dialPeer(ctx, p)
		if err == nil {
			g.markPeerAttempt(p.ID, true, "")
			// 进入读循环（阻塞直到断开）；用链路级 ctx（Stop/unregister 时 cancel）
			g.readLoop(l.ctx, conn, l)
			backoff = time.Second // 成功后重置退避
		} else if errors.Is(err, ErrLinkDuplicated) {
			// 对端已入站连上本机（全互连防乒乓）：连接健康，不记录失败；
			// 中退避（30s）低频巡检——若对端入站断开，≤30s 内恢复出站接管。
			backoff = 30 * time.Second
			if ctx.Err() != nil || g.isClosed() {
				return
			}
		} else {
			g.markPeerAttempt(p.ID, false, err.Error())
			g.router.MarkOffline(p.ID)
			if ctx.Err() != nil || g.isClosed() {
				return
			}
		}
		// 退避重连（1s/2s/4s…max 60s）；retryPeers 信号（链路断开）→ 立即重置重试
		select {
		case <-ctx.Done():
			return
		case <-g.retryPeers:
			backoff = time.Second
			continue
		case <-time.After(backoff):
		}
		if backoff < 60*time.Second {
			backoff *= 2
			if backoff > 60*time.Second {
				backoff = 60 * time.Second
			}
		}
	}
}

// kickDialLoops 链路断开信号：向等待退避的 dialLoop 广播「立即重试」。
// 场景：全互连下本机出站被拒进入长退避，若对端入站链路断开（对端重启），
// 出站应立即接管而非空等退避到期。
func (g *Gateway) kickDialLoops() {
	select {
	case g.retryPeers <- struct{}{}:
	default:
	}
}

// markPeerAttempt 记录一次出站拨号结果（成功/失败 + 时间）。
func (g *Gateway) markPeerAttempt(peerID string, ok bool, errMsg string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, exists := g.peerStatus[peerID]
	if !exists {
		st = &peerState{}
		g.peerStatus[peerID] = st
	}
	st.lastAttempt = time.Now()
	if ok {
		st.connected = true
		st.lastError = ""
		st.lastSuccess = time.Now()
		return
	}
	// 失败不覆盖「仍有活跃链路」的健康状态：全互连场景本机出站被拒（对端已入站连我）
	// 时 links 仍健康——connected 应反映「节点当前是否可达/已连」，而非「出站拨号成败」。
	if _, hasLink := g.links[peerID]; hasLink {
		return
	}
	st.connected = false
	st.lastError = errMsg
}

// markPeerDisconnected 链路断开时标记对应 peer 失连（不覆盖已更新的 lastError）。
func (g *Gateway) markPeerDisconnected(peerID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if st, ok := g.peerStatus[peerID]; ok {
		st.connected = false
	}
}

// dialPeer 拨号一个 peer（WS 客户端），握手认证后返回连接、对端 node id 与登记的 link。
func (g *Gateway) dialPeer(ctx context.Context, p Peer) (*websocket.Conn, string, *link, error) {
	addr := p.Addr
	if addr == "" {
		return nil, "", nil, fmt.Errorf("peer %s: empty addr", p.ID)
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(dctx, addr, &websocket.DialOptions{
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	})
	if err != nil {
		return nil, "", nil, fmt.Errorf("dial %s: %w", p.ID, err)
	}
	// 握手 hello
	token := p.Token
	if token == "" {
		token = g.cfg.AuthToken
	}
	hello := Frame{
		Type:     FrameHello,
		NodeID:   g.nodeID,
		Version:  protocolVersion,
		Token:    token,
		Sessions: g.localSessions(),
		Nodes:    g.knownNodesSummary(),
	}
	if err := g.sendFrame(dctx, c, hello); err != nil {
		_ = c.Close(websocket.StatusPolicyViolation, "handshake send failed")
		return nil, "", nil, err
	}
	var ack Frame
	if err := wsjson.Read(dctx, c, &ack); err != nil {
		_ = c.Close(websocket.StatusPolicyViolation, "handshake ack failed")
		return nil, "", nil, fmt.Errorf("peer %s: handshake ack read failed: %w", p.ID, err)
	}
	if ack.Type != FrameHelloAck {
		_ = c.Close(websocket.StatusPolicyViolation, "handshake ack failed")
		return nil, "", nil, fmt.Errorf("peer %s: handshake ack failed (got type %q)", p.ID, ack.Type)
	}
	// 对端 node id 必须等于配置的 p.ID（防错连；p.ID 空时以对端自报为准）
	if p.ID != "" && ack.NodeID != p.ID {
		_ = c.Close(websocket.StatusPolicyViolation, "node id mismatch")
		return nil, "", nil, fmt.Errorf("peer %s: node id mismatch (got %s)", p.ID, ack.NodeID)
	}
	if ack.NodeID == "" {
		_ = c.Close(websocket.StatusPolicyViolation, "empty node id")
		return nil, "", nil, fmt.Errorf("peer %s: empty node id", p.ID)
	}
	// 登记链路（出站）；被拒（同节点已有异向活跃链路——对端已连我，无需再出站）
	l, ok := g.registerLink(ack.NodeID, c, false)
	if !ok {
		// 链路已由对端入站建立：本出站连接自我关闭。dialLoop 不应高频重试——
		// 用 ErrLinkDuplicated 信号让 dialLoop 走长退避（连接其实是健康的）。
		return nil, "", nil, ErrLinkDuplicated
	}
	// 记录对端在线会话（远程 session_list 数据源）
	g.upsertPeerSessions(ack.NodeID, ack.Sessions)
	// 拓扑：登记对端已知节点为中继
	for _, n := range ack.Nodes {
		if n.ID != g.nodeID && n.ID != ack.NodeID {
			g.router.SetRelay(n.ID, ack.NodeID)
		}
	}
	// 广播对端上线（拓扑变化）
	g.broadcastTopo()
	return c, ack.NodeID, l, nil
}

// registerLink 登记链路（入站/出站统一）：node_id → link。
// 返回登记的 link（供调用方用 l.ctx 跑读循环；l.ctx 在 unregister/Stop 时 cancel）。
// 第二个返回值 ok=false = 链路被拒（同对节点已存在异向活跃链路——全互连双向拨号
// 场景防乒乓：保留先建立的，新链路自我关闭，调用方不得跑读循环/登记会话）。
func (g *Gateway) registerLink(nodeID string, c *websocket.Conn, inbound bool) (*link, bool) {
	lctx, lcancel := context.WithCancel(context.Background())
	l := &link{
		nodeID:    nodeID,
		inbound:   inbound,
		conn:      c,
		connected: time.Now(),
		lastSeen:  time.Now(),
		ctx:       lctx,
		cancel:    lcancel,
	}
	g.mu.Lock()
	if old, ok := g.links[nodeID]; ok && old.inbound != inbound {
		// 同对节点双链路（我连对方 + 对方连我，双向 peers 配置）：
		// 保留先建立的，新的直接放弃——否则互相替换 → 双方 dialLoop/握手乒乓，
		// 稳定态可能一条链路都留不下（本地双开全互连实测）。
		g.mu.Unlock()
		lcancel()
		_ = c.Close(websocket.StatusNormalClosure, "duplicate opposite link (keep first)")
		return nil, false
	}
	// 同向重复（旧链路已断但未清理 / 重连）：替换取活
	if old, ok := g.links[nodeID]; ok {
		old.cancel()
		_ = old.conn.Close(websocket.StatusGoingAway, "replaced")
	}
	g.links[nodeID] = l
	// 对端是配置的 peer（出站目标）：连接建立（无论本侧出站还是对端入站）→ 状态转已连。
	// 修复：此前只标记出站成功，入站连接（对端主动连我）不会更新 peerStatus，
	// 表现为「links 已连但 peers 仍 connected:false + 残留错误」。
	if st, ok := g.peerStatus[nodeID]; ok {
		st.connected = true
		st.lastError = ""
		st.lastSuccess = time.Now()
		st.lastAttempt = time.Now()
	}
	g.mu.Unlock()
	g.router.SetDirect(nodeID, nodeID)
	// 心跳 goroutine
	go g.heartbeatLoop(l)
	return l, true
}

// unregisterLink 注销链路（断开时）。
// 只删「确实是同一个 link 实例」的条目（防 M5：同节点旧连接断开时
// 误删新替换的链路——传入旧 link 指针，校验 links[nodeID] == l 才删）。
func (g *Gateway) unregisterLink(nodeID string, l *link) {
	g.mu.Lock()
	removed := false
	if cur, ok := g.links[nodeID]; ok && (l == nil || cur == l) {
		cur.cancel()
		delete(g.links, nodeID)
		removed = true
	}
	g.mu.Unlock()
	g.router.MarkOffline(nodeID)
	if removed {
		g.markPeerDisconnected(nodeID) // 出站断开 → peer 状态转失连
		// 节点是配置 peer（可能有 dialLoop 在长退避等它）→ 唤醒立即重试接管
		g.mu.RLock()
		_, isPeer := g.peerStatus[nodeID]
		g.mu.RUnlock()
		if isPeer {
			g.kickDialLoops()
		}
	}
	if nodeID != "" {
		g.broadcastNodeOffline(nodeID)
	}
}

// isClosed 网关是否已关闭。
func (g *Gateway) isClosed() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.closed
}

// ---------- 心跳 ----------

// heartbeatLoop 每 30s ping；2 周期未收到 pong（lastSeen 未更新）→ 判离线断开。
func (g *Gateway) heartbeatLoop(l *link) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-ticker.C:
			g.mu.RLock()
			stale := time.Since(l.lastSeen) > 60*time.Second
			g.mu.RUnlock()
			if stale {
				g.unregisterLink(l.nodeID, l)
				_ = l.conn.Close(websocket.StatusGoingAway, "heartbeat timeout")
				return
			}
			// 发 ping
			if err := g.sendFrameLink(l.ctx, l, Frame{Type: FramePing, T: time.Now().UnixMilli()}); err != nil {
				g.unregisterLink(l.nodeID, l)
				return
			}
		}
	}
}

// ---------- 读循环 ----------

// readLoop 持续读帧：分派到 hello_ack/ping/pong/event/event_ack/topo/list_req 处理。
// 用链路级 ctx（l.ctx）：入站/出站统一；ctx 在 unregister/Stop 时 cancel。
// 传 *link 而非 nodeID：defer 注销时校验同一实例（防 M5 stale readLoop 删新链路）。
func (g *Gateway) readLoop(ctx context.Context, c *websocket.Conn, l *link) {
	nodeID := ""
	if l != nil {
		nodeID = l.nodeID
	}
	defer func() {
		if l != nil {
			g.unregisterLink(l.nodeID, l)
		}
		_ = c.Close(websocket.StatusNormalClosure, "read loop end")
	}()
	for {
		if g.isClosed() {
			return
		}
		var f Frame
		if err := wsjson.Read(ctx, c, &f); err != nil {
			return // 连接断开
		}
		switch f.Type {
		case FramePing:
			_ = g.sendFrame(ctx, c, Frame{Type: FramePong, T: f.T})
			g.touch(nodeID)
		case FramePong:
			g.touch(nodeID)
			g.signalPong(nodeID, f.T) // 命中主动 ping（PingNode）则唤醒
		case FrameEvent:
			g.handleEvent(ctx, c, &f)
		case FrameEventAck:
			g.handleEventAck(&f)
		case FrameListReq:
			_ = g.sendFrame(ctx, c, Frame{Type: FrameSessionList, Sessions: g.localSessions()})
		case FrameSessionList:
			// 对端主动推送的会话列表（list_req 的应答）；来源链路节点已知（l.nodeID）。
			g.upsertPeerSessions(nodeID, f.Sessions)
		case FrameTopo:
			// topo：来源链路节点的最新在线会话先回填（周期广播/上线），再登记中继节点
			if len(f.Sessions) > 0 {
				g.upsertPeerSessions(nodeID, f.Sessions)
			}
			g.handleTopo(&f)
		case FrameNodeOffline:
			// N5 修复：离线信息应传播（剔除 knownNodes + 透传给其它直连），
			// 否则 session_list 会残留已离线节点最多 60s（last_seen 过期才剔）。
			g.nodeOffline(f.NodeID)
		case FrameBye:
			return
		}
	}
}

// touch 更新链路 lastSeen（心跳活性）。
func (g *Gateway) touch(nodeID string) {
	if nodeID == "" {
		return
	}
	g.mu.Lock()
	if l, ok := g.links[nodeID]; ok {
		l.lastSeen = time.Now()
	}
	g.mu.Unlock()
}

// ---------- 事件处理 ----------

// handleEvent 收到 event 帧：目标是本机 → 投递；否则转发（一跳）。
func (g *Gateway) handleEvent(ctx context.Context, c *websocket.Conn, f *Frame) {
	if f.Payload == nil {
		_ = g.sendFrame(ctx, c, Frame{Type: FrameEventAck, MsgID: f.MsgID, OK: boolPtr(false), Error: AckBadEvent})
		return
	}
	// 去重：近期 msg_id
	if !g.dedup(f.MsgID) {
		return // 重复帧静默丢弃（转发放环场景）
	}
	target := f.TargetNode
	if target == "" {
		// 无 target_node = 给本机（对端直发）
		target = g.nodeID
	}
	if target == g.nodeID {
		// 目标本机 → 投递。桌面层 onEvent 对 SessionId=="" 的请求会自动建会话并回填
		// msg.SessionId（指针可变）——ack 据此把新会话 id 带回发送方。
		if g.onEvent != nil {
			fromNode := f.Payload.FromNode
			g.onEvent(ctx, fromNode, f.Payload)
		}
		ack := Frame{Type: FrameEventAck, MsgID: f.MsgID, OK: boolPtr(true), Error: AckOK}
		if f.Payload != nil && f.Payload.SessionId != "" && f.TargetSession == "" {
			ack.CreatedSession = f.Payload.SessionId // 目标节点自动创建的新会话
		}
		_ = g.sendFrame(ctx, c, ack)
		return
	}
	// 转发（一跳）：找下一跳
	next := g.router.NextHop(target)
	if next == "" {
		// 无下一跳 = 目标不可达（可重试的瞬时条件，非环路）——回 ttl_exceeded 会误导
		// 发送方"不可重试"（C2/C3 修复：err_node_offline 语义）
		_ = g.sendFrame(ctx, c, Frame{Type: FrameEventAck, MsgID: f.MsgID, OK: boolPtr(false), Error: AckNodeOffline})
		return
	}
	// TTL 检查
	ttl := f.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if f.Hops >= ttl {
		// 环路/TTL 耗尽：确实不可重试
		_ = g.sendFrame(ctx, c, Frame{Type: FrameEventAck, MsgID: f.MsgID, OK: boolPtr(false), Error: AckTTLExceeded})
		return
	}
	f.Hops++
	// 记录回程路由：event_ack 沿来源链路回传（中继转发）
	// 注意：linkNodeOf 内部要 RLock，必须先释放外层 mu 再调用（RWMutex 写锁内不可再读锁）
	backNode := g.linkNodeOf(c)
	if f.MsgID != "" && backNode != "" {
		g.mu.Lock()
		g.ackRoutes[f.MsgID] = backNode
		g.mu.Unlock()
	}
	if err := g.forwardFrame(f, next); err != nil {
		// 下一跳存在但转发失败（连接刚断）：可重试的瞬时错误
		_ = g.sendFrame(ctx, c, Frame{Type: FrameEventAck, MsgID: f.MsgID, OK: boolPtr(false), Error: AckNodeOffline})
	}
}

// linkNodeOf 找连接对应的节点 id（回程路由用；未登记返回 ""）。
func (g *Gateway) linkNodeOf(c *websocket.Conn) string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	for id, l := range g.links {
		if l.conn == c {
			return id
		}
	}
	return ""
}

// forwardFrame 经下一跳链路转发帧。
func (g *Gateway) forwardFrame(f *Frame, next string) error {
	g.mu.RLock()
	l, ok := g.links[next]
	g.mu.RUnlock()
	if !ok {
		return ErrNodeOffline
	}
	return g.sendFrameLink(l.ctx, l, *f)
}

// handleEventAck 收到 event_ack：是本机发送方等待 → 唤醒；否则是转发回程 → 沿路由回传。
// 主动 ping 与 SendMessage 共用 eventAcks 通道（ping 也注册 ack 等待，见 PingNode）。
func (g *Gateway) handleEventAck(f *Frame) {
	g.mu.Lock()
	ch, waiting := g.eventAcks[f.MsgID]
	if waiting {
		delete(g.eventAcks, f.MsgID)
	}
	// 中继回程：本机非发送方（有 ackRoutes 记录）→ 沿记录链路回传
	backNode := g.ackRoutes[f.MsgID]
	if !waiting && backNode != "" {
		delete(g.ackRoutes, f.MsgID)
	}
	g.mu.Unlock()

	if waiting && ch != nil {
		ch <- ackResult{ok: f.OK != nil && *f.OK, error: f.Error, created: f.CreatedSession}
		return
	}
	if !waiting && backNode != "" {
		g.mu.RLock()
		l, ok := g.links[backNode]
		g.mu.RUnlock()
		if ok {
			_ = g.sendFrameLink(l.ctx, l, *f)
		}
	}
}

// nodeOffline 处理节点离线：路由剔除 + knownNodes 剔除 + 透传给其它直连节点
// （离线 gossip 多跳传播；N5 修复）。
func (g *Gateway) nodeOffline(nodeID string) {
	g.router.MarkOffline(nodeID)
	g.mu.Lock()
	delete(g.knownNodes, nodeID)
	g.mu.Unlock()
	// 透传给其它直连（除来源链路外——broadcastNodeOffline 会发给所有，来源已断无妨）
	g.broadcastNodeOffline(nodeID)
}

// handleTopo 拓扑交换：登记已知节点及其会话（经发送方中继）。
// f.Nodes 来自发送方的 knownNodesSummary（每节点带其 sessions）；发送方自身的最新
// 会话在 f.Sessions，由 readLoop 归一到来源链路节点（本函数拿不到发送方 node id）。
func (g *Gateway) handleTopo(f *Frame) {
	for _, n := range f.Nodes {
		if n.ID != g.nodeID {
			g.upsertPeerSessions(n.ID, n.Sessions)
		}
	}
}

// upsertPeerSessions 更新已知节点及其在线会话（握手 hello/hello_ack.Sessions 数据源）。
// 修复 N7：远程 session_list 依赖此填充 NodeInfo.Sessions，否则永远为空。
func (g *Gateway) upsertPeerSessions(nodeID string, sessions []SessionInfo) {
	if nodeID == "" || nodeID == g.nodeID {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	n, ok := g.knownNodes[nodeID]
	if !ok {
		n = &NodeInfo{ID: nodeID}
		g.knownNodes[nodeID] = n
	}
	n.Sessions = sessions
	n.LastSeen = time.Now()
}

// broadcastTopo 广播本机已知节点 + 本机最新在线会话（拓扑变化 / 周期会话同步共用）。
// 简化：发给全部直连；Sessions 恒携带本机在线摘要——对端据此刷新 knownNodes 里的
// 本机会话列表（session_list 不再只是握手快照；topo 也带中继节点的摘要供其更新）。
func (g *Gateway) broadcastTopo() {
	g.mu.RLock()
	nodes := make([]*link, 0, len(g.links))
	for _, l := range g.links {
		nodes = append(nodes, l)
	}
	summary := g.knownNodesSummary()
	g.mu.RUnlock()
	f := Frame{Type: FrameTopo, Nodes: summary, Sessions: g.localSessions()}
	for _, l := range nodes {
		_ = g.sendFrameLink(l.ctx, l, f)
	}
}

// PushSessions 立即向全部直连节点广播一次本机最新会话摘要（不等待周期广播）。
// 用途：会话元数据变化（如 AI 标题生成完成）后即时同步给对端 —— 对端 session_list
// 不必等最长 sessionBroadcastInterval 才看到新标题。未连节点 = no-op（握手/周期自会补）。
func (g *Gateway) PushSessions() {
	if g.isClosed() {
		return
	}
	g.broadcastTopo()
}

// RequestPeerSessions 向全部直连节点发 list_req，请求对端**当前**在线会话列表
// （对端 readLoop 回 FrameSessionList → upsertPeerSessions 刷新 knownNodes）。
// 用途：设置页「刷新状态」主动拉取——不依赖推送/周期，错过推送也能立即对齐。
// 拉取是异步的（对端回帧后 knownNodes 更新）；调用方可稍后读 KnownNodes。
func (g *Gateway) RequestPeerSessions() {
	if g.isClosed() {
		return
	}
	g.mu.RLock()
	nodes := make([]*link, 0, len(g.links))
	for _, l := range g.links {
		nodes = append(nodes, l)
	}
	g.mu.RUnlock()
	f := Frame{Type: FrameListReq}
	for _, l := range nodes {
		_ = g.sendFrameLink(l.ctx, l, f)
	}
}

// broadcastNodeOffline 广播节点离线。
func (g *Gateway) broadcastNodeOffline(nodeID string) {
	g.mu.RLock()
	nodes := make([]*link, 0, len(g.links))
	for _, l := range g.links {
		nodes = append(nodes, l)
	}
	g.mu.RUnlock()
	f := Frame{Type: FrameNodeOffline, NodeID: nodeID}
	for _, l := range nodes {
		_ = g.sendFrameLink(l.ctx, l, f)
	}
}

// dedup 近期 msg_id 接收去重（真 LRU：map + 插入序 slice，容量 1000；
// 超限删最旧——修复 N3 的"清空重建"假 LRU，容量内去重记忆不丢失）。
func (g *Gateway) dedup(msgID string) bool {
	if msgID == "" {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seenMsgID == nil {
		g.seenMsgID = make(map[string]struct{})
	}
	if _, ok := g.seenMsgID[msgID]; ok {
		return false // 重复帧（转发/重试环）静默丢弃
	}
	g.seenMsgID[msgID] = struct{}{}
	g.seenOrder = append(g.seenOrder, msgID)
	if len(g.seenOrder) > 1000 {
		// 删最旧的 100 条（批量，避免每次删一条的频繁分配）
		drop := len(g.seenOrder) - 1000
		if drop > 100 {
			drop = 100
		}
		for _, old := range g.seenOrder[:drop] {
			delete(g.seenMsgID, old)
		}
		g.seenOrder = g.seenOrder[drop:]
	}
	return true
}

// ---------- NodeSender 接口 ----------

// SendMessage 向目标节点上的会话投递消息（直连优先，一跳转发）。
// targetSID 为空 = 请求目标节点自动创建新会话；返回 createdSID = 对端新建的会话 id。
// 等待 event_ack（≤5s）；ack 失败按错误码分类返回。
func (g *Gateway) SendMessage(ctx context.Context, targetNodeID, targetSID string, msg *InboundMessage) (string, error) {
	if g.isClosed() {
		return "", ErrNodeOffline
	}
	next := g.router.NextHop(targetNodeID)
	if next == "" {
		return "", ErrNodeOffline
	}
	msg.SessionId = targetSID
	f := Frame{
		Type:          FrameEvent,
		TargetNode:    targetNodeID,
		TargetSession: targetSID,
		Hops:          0,
		TTL:           DefaultTTL,
		Payload:       msg,
		MsgID:         msg.MsgID,
	}
	// 注册 ack 等待
	ackCh := make(chan ackResult, 1)
	g.mu.Lock()
	g.eventAcks[msg.MsgID] = ackCh
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.eventAcks, msg.MsgID)
		g.mu.Unlock()
	}()

	g.mu.RLock()
	l, ok := g.links[next]
	g.mu.RUnlock()
	if !ok {
		return "", ErrNodeOffline
	}
	if err := g.sendFrameLink(ctx, l, f); err != nil {
		return "", err
	}
	// 等 ack（≤5s）
	select {
	case res := <-ackCh:
		if res.ok || res.error == AckOK {
			return res.created, nil // created 非空 = 目标自动建了新会话
		}
		// node_offline 语义归一为 ErrNodeOffline（发送方 errors.Is 可判）
		if res.error == AckNodeOffline {
			return "", ErrNodeOffline
		}
		return "", &AckError{Code: res.error}
	case <-time.After(5 * time.Second):
		return "", fmt.Errorf("mesh send: ack timeout")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// AckError 事件送达失败（携带可重试/不可重试错误码）。
type AckError struct{ Code string }

func (e *AckError) Error() string { return "mesh ack: " + e.Code }

// KnownNodes 已知节点及在线会话摘要（session_list 远程部分）。
func (g *Gateway) KnownNodes(ctx context.Context) []NodeInfo {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]NodeInfo, 0, len(g.knownNodes))
	for _, n := range g.knownNodes {
		if time.Since(n.LastSeen) > 60*time.Second {
			continue // 过期剔除
		}
		out = append(out, *n)
	}
	return out
}

// DirectLinks 直连链路状态（排障）。
func (g *Gateway) DirectLinks(ctx context.Context) []LinkInfo {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]LinkInfo, 0, len(g.links))
	for _, l := range g.links {
		out = append(out, LinkInfo{
			NodeID:      l.nodeID,
			Inbound:     l.inbound,
			ConnectedAt: l.connected,
			LastSeen:    l.lastSeen,
		})
	}
	return out
}

// PeerStatuses 配置的出站节点（peer）拨号状态（mesh_status 排障）。
func (g *Gateway) PeerStatuses(ctx context.Context) []PeerStatus {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]PeerStatus, 0, len(g.peerStatus))
	for id, st := range g.peerStatus {
		ps := PeerStatus{
			ID:          id,
			Addr:        st.addr,
			Connected:   st.connected,
			LastError:   st.lastError,
			LastAttempt: st.lastAttempt,
			LastSuccess: st.lastSuccess,
		}
		if st.rttMs > 0 {
			ps.RTTMs = st.rttMs
		} else {
			ps.RTTMs = -1
		}
		out = append(out, ps)
	}
	return out
}

// pingWait 一次进行中的主动 ping（按目标节点唯一；并发 ping 同节点以最新为准）。
type pingWait struct {
	t    int64 // 等待回显的 ping T（pong 必须同 T 才算命中）
	done chan struct{}
}

// PingNode 对目标节点做一次主动往返 ping（连接检测 / 排障 / mesh_ping 命令）。
// 复用帧协议既有 ping/pong：发 ping{T:now}，对端（任意协议版本）原样回 pong{T}；
// readLoop 收到同 T 的 pong 即唤醒。目标无直连链路 → ErrNodeOffline；超时 5s 同。
func (g *Gateway) PingNode(ctx context.Context, targetNodeID string) (time.Duration, error) {
	if g.isClosed() {
		return 0, ErrNodeOffline
	}
	g.mu.RLock()
	l, ok := g.links[targetNodeID]
	g.mu.RUnlock()
	if !ok {
		return 0, ErrNodeOffline
	}
	t := time.Now().UnixMilli()
	w := &pingWait{t: t, done: make(chan struct{})}
	g.mu.Lock()
	g.pingWaits[targetNodeID] = w
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		if g.pingWaits[targetNodeID] == w {
			delete(g.pingWaits, targetNodeID)
		}
		g.mu.Unlock()
	}()

	start := time.Now()
	if err := g.sendFrameLink(l.ctx, l, Frame{Type: FramePing, T: t}); err != nil {
		return 0, err
	}
	select {
	case <-w.done:
		rtt := time.Since(start)
		g.mu.Lock()
		if st, ok := g.peerStatus[targetNodeID]; ok {
			st.rttMs = rtt.Milliseconds()
		}
		g.mu.Unlock()
		return rtt, nil
	case <-time.After(5 * time.Second):
		return 0, ErrNodeOffline
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// signalPong 收到 pong 帧：命中进行中的主动 ping（同 T）则唤醒。
func (g *Gateway) signalPong(nodeID string, t int64) {
	g.mu.RLock()
	w, ok := g.pingWaits[nodeID]
	g.mu.RUnlock()
	if ok && w != nil && w.t == t {
		select {
		case <-w.done: // 已唤醒（并发幂等）
		default:
			close(w.done)
		}
	}
}

// ---------- 内部辅助 ----------

// localSessions 本机会话摘要（宿主注入；缺省空——由 Hub.List 合并，这里仅握手携带）。
// localSessions 本机会话摘要（握手 hello/hello_ack 携带；宿主经 SetSessionProvider 注入）。
// 缺省 nil = 不广播（远程 session_list 看不到本机会话）。
func (g *Gateway) localSessions() []SessionInfo {
	g.mu.RLock()
	fn := g.sessionProvider
	g.mu.RUnlock()
	if fn == nil {
		return nil
	}
	return fn()
}

// knownNodesSummary 已知节点摘要（拓扑交换）。
func (g *Gateway) knownNodesSummary() []NodeInfo {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]NodeInfo, 0, len(g.knownNodes))
	for _, n := range g.knownNodes {
		if time.Since(n.LastSeen) > 60*time.Second {
			continue
		}
		out = append(out, *n)
	}
	return out
}

// sendFrame 发送一帧（握手阶段用：连接尚未登记为 link，无 per-link 锁可用，
// 退化为全局写锁——仅握手几帧，无并发压力）。
func (g *Gateway) sendFrame(ctx context.Context, c *websocket.Conn, f Frame) error {
	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	return g.writeFrame(ctx, c, f)
}

// sendFrameLink 经已登记链路发帧：**per-link 写锁**（C4 修复）——单个慢/死对端
// 只阻塞自己的链路，不影响其它链路（心跳/事件照常）。
func (g *Gateway) sendFrameLink(ctx context.Context, l *link, f Frame) error {
	if l == nil {
		return g.sendFrame(ctx, nil, f)
	}
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	return g.writeFrame(ctx, l.conn, f)
}

// writeFrame 底层写（无锁；调用方负责串行化）。
func (g *Gateway) writeFrame(ctx context.Context, c *websocket.Conn, f Frame) error {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return wsjson.Write(ctx2, c, f)
}

func boolPtr(b bool) *bool { return &b }

// linksList 展开链路 map（锁外调用；调用方已持锁或已复制）。
func linksList(m map[string]*link) []*link {
	out := make([]*link, 0, len(m))
	for _, l := range m {
		out = append(out, l)
	}
	return out
}

// encodeFrame/decodeFrame 帧序列化辅助（测试用）。
func encodeFrame(f Frame) ([]byte, error) { return json.Marshal(f) }
func decodeFrame(b []byte) (Frame, error) {
	var f Frame
	err := json.Unmarshal(b, &f)
	return f, err
}
