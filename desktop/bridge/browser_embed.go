package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/seven7628/hai-harness/plugin"
	"github.com/seven7628/hai-harness/plugin/browser"
	"github.com/seven7628/hai-harness/plugin/browser/cdp"
	"github.com/seven7628/hai-harness/plugin/browser/trace"
)

// —— 嵌入式浏览器 CDP 转发（M2）——
//
// 拓扑：
//
//	chrome-devtools-mcp --ws://127.0.0.1:PORT/ --> embedServer（浏览器级 ws，gorilla）
//		--> cdp.Forwarder（浏览器级协议映射）
//			--> cdp.TCPDebugger（JSON 行协议，TCP）
//				--> Electron 内嵌视图 webContents.debugger 代理（electron/embed.ts）
//
// embedServer 的 ws 层是薄壳：收到浏览器级 JSON-RPC 消息 → cdp.HandleMessage 处理 →
// 回写响应；页面级事件经 Forwarder.Relay → 广播给各 ws 客户端。
// 网络层（ws/tcp）无法在本 shell 单测（无 GUI 且环境沙箱拦端口绑定），核心协议逻辑
// 在 cdp 包内已单测；端到端兼容性需在真实桌面运行时验证（见 docs/BROWSER_USE_EMBED.md）。

// embedClient 一个已升级的 ws 连接（读协程 + 独立写协程，避免 gorilla 同连接并发写）。
// §1.8.0 重构：**单通道 out**——响应与事件合并、顺序一致、不丢、无优先级饥饿。
// 此前双通道（send 事件 + resp 响应）：响应优先写协程会饿死事件（初始化时几十个
// 响应排队，lifecycle 事件延迟 20s+ 到达 → goto 等 load 超时）；事件通道满又丢事件。
type embedClient struct {
	conn *websocket.Conn
	out  chan []byte
}

// embedServer 浏览器级 CDP ws 服务器。
type embedServer struct {
	upgrader websocket.Upgrader
	fwd      *cdp.Forwarder
	ln       net.Listener
	mu       sync.Mutex
	clients  map[*embedClient]struct{}
}

func newEmbedServer(fwd *cdp.Forwarder) *embedServer {
	return &embedServer{
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		fwd:      fwd,
		clients:  map[*embedClient]struct{}{},
	}
}

// Start 监听 addr（如 "127.0.0.1:0" 随机端口），启动服务与事件转发。
func (s *embedServer) Start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.ln = ln
	httpSrv := &http.Server{Handler: http.HandlerFunc(s.handleWS)}
	go func() { _ = httpSrv.Serve(ln) }()
	go s.runRelay()
	return nil
}

// Endpoint 返回浏览器级 ws 端点（chrome-devtools-mcp --ws-endpoint 用）。
func (s *embedServer) Endpoint() string {
	if s.ln == nil {
		return ""
	}
	return "ws://" + s.ln.Addr().String() + "/"
}

// Stop 关闭监听与所有客户端，并释放转发器 Debugger。
func (s *embedServer) Stop() {
	if s.ln != nil {
		_ = s.ln.Close()
	}
	s.mu.Lock()
	for cl := range s.clients {
		_ = cl.conn.Close()
	}
	s.clients = map[*embedClient]struct{}{}
	s.mu.Unlock()
	// Forwarder/底层连接由 manager.embedStop 统一释放（fwd.Close + mc.Close）
}

func (s *embedServer) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	cl := &embedClient{conn: conn, out: make(chan []byte, 1024)}
	s.mu.Lock()
	s.clients[cl] = struct{}{}
	s.mu.Unlock()
	defer func() {
		// 摘除 client（幂等：断连路径已摘除 + close；此处防御性再摘）
		s.mu.Lock()
		if _, ok := s.clients[cl]; ok {
			delete(s.clients, cl)
			close(cl.out)
		}
		s.mu.Unlock()
		_ = conn.Close()
	}()

	// 写协程：串行写出（gorilla 同连接不允许并发写）。
	// §1.8.0 重构：**单通道顺序写**——响应与事件按产生顺序写出，不丢、无优先级饥饿。
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		for msg := range cl.out {
			// §1.8.0 诊断：ws 发出的消息（确认事件是否真的到 Puppeteer；响应也记）
			var head struct {
				Method    string `json:"method"`
				ID        *int64 `json:"id"`
				SessionID string `json:"sessionId"`
			}
			if json.Unmarshal(msg, &head) == nil {
				if head.Method != "" {
					trace.Log("ws.send", head.Method)
				} else if head.ID != nil {
					trace.Log("ws.send.resp", "id="+strconv.FormatInt(*head.ID, 10), "sid="+head.SessionID)
				}
			}
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		}
	}()

	// 读循环：浏览器级消息 → HandleMessage → 响应入队（out，与事件同序）。
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			// 断连：持锁原子「摘除 + close」——与 runRelay 广播（同样持 s.mu）串行化，
			// 杜绝「runRelay 向已 close 的 out 发送 → send on closed channel panic」。
			s.mu.Lock()
			delete(s.clients, cl)
			close(cl.out)
			s.mu.Unlock()
			break
		}
		// §1.8.0 诊断：ws 收到的消息（Puppeteer 发来的命令，看它收到事件后的反应）
		var head struct {
			Method    string `json:"method"`
			SessionID string `json:"sessionId"`
			ID        *int64 `json:"id"`
		}
		if json.Unmarshal(data, &head) == nil && head.Method != "" {
			trace.Log("ws.recv", "method="+head.Method, "sid="+head.SessionID)
		}
		if resp := cdp.HandleMessage(s.fwd, data); resp != nil {
			// 响应入 out（阻塞等待写协程消费；绝不丢弃）
			select {
			case cl.out <- resp:
			case <-writeDone:
			}
		}
	}
	<-writeDone
}

// runRelay 把页面级事件广播给所有 ws 客户端。
// §1.8.0 重构：**单通道不丢**——事件阻塞入 out（写协程持续消费，不会死锁）。
// 事件丢失会导致 Puppeteer 状态不同步（lifecycle 丢 → goto 超时）。
// runRelay 把页面级事件广播给所有 ws 客户端。
// §1.8.0 重构：**单通道不丢**——事件阻塞入 out（写协程持续消费，不会死锁）。
// 事件丢失会导致 Puppeteer 状态不同步（lifecycle 丢 → goto 超时）。
func (s *embedServer) runRelay() {
	events := make(chan map[string]any, 256)
	go s.fwd.Relay(events)
	for ev := range events {
		raw, _ := json.Marshal(ev)
		s.mu.Lock()
		for cl := range s.clients {
			select {
			case cl.out <- raw:
			case <-time.After(5 * time.Second):
				// 防御：写协程异常停止（断连路径 close out 后本分支不触发；5s 兜底
				// 防极端场景永久阻塞 runRelay）
				trace.Log("relay.timeout", "client-drop-event")
			}
		}
		s.mu.Unlock()
	}
}

// manager 级：单一 embed 转发器状态（跨工作区共享——内嵌视图池由 Electron 维护，
// bridge 只持有一个转发器 + 一条多路复用控制连接）。
type embedBridge struct {
	mu          sync.Mutex
	srv         *embedServer
	fwd         *cdp.Forwarder
	mc          *cdp.MultiClient // M6：多 target 复用连接（Opener + 归并事件源）
	addr        string           // Electron debugger 代理地址（attach 时记录）
	placeholder string           // 占位 target id（embedStart 记录，attach 时移除）

	// attachedCh attach 完成信号（事件驱动替代轮询）：首次成功 attach 后 close。
	// 等待方 select 该 channel 而非 100ms 轮询（TEMP-STARTUP 优化：消除 2s sleep）。
	attachedCh   chan struct{}
	attachedOnce bool // 已 close 标记（幂等：重复 attach 不重复 close）
}

// embedStart 起转发器 + ws 端点（不连视图）。返回浏览器级 ws 端点。
// enable 时调用：后台准备好端点（不可见），chrome-devtools-mcp 可连；视图在真正使用时 attach。
func (m *manager) embedStart(targetID string) (endpoint string, err error) {
	m.embed.mu.Lock()
	defer m.embed.mu.Unlock()
	if m.embed.srv != nil {
		return m.embed.srv.Endpoint(), nil // 幂等：已在运行
	}
	fwd := cdp.New(targetID, "", "about:blank", nil) // 视图后 Attach（attach 时按真实视图重绑）
	srv := newEmbedServer(fwd)
	if err := srv.Start("127.0.0.1:0"); err != nil {
		return "", err
	}
	m.embed.srv = srv
	m.embed.fwd = fwd
	m.embed.placeholder = targetID // attach 时精确移除该占位（防自定义 id 与常量失配）
	if m.embed.attachedCh == nil {
		m.embed.attachedCh = make(chan struct{})
	}
	return srv.Endpoint(), nil
}

// embedAttach 把 Electron debugger 代理 attach 到转发器（真正使用浏览器、创建内嵌视图时调用）。
// M6：握手即枚举真实视图并逐一绑定（首个为默认 target），注入 Opener 让 createTarget/closeTarget
// 真开真关；归并事件流泵入转发器（sessionId 归属按 target）。
func (m *manager) embedAttach(debuggerAddr string) error {
	fmt.Fprintln(os.Stderr, "[EMBED] attach enter addr=", debuggerAddr)
	m.embed.mu.Lock()
	fmt.Fprintln(os.Stderr, "[EMBED] attach locked")
	defer m.embed.mu.Unlock()
	if m.embed.fwd == nil {
		return fmt.Errorf("内嵌端点未启动（请先 browser_embed_start）")
	}
	// §7.2：相同 addr 已 attach → 幂等返回（前端自举/面板 effect 可能重复触发）
	if m.embed.addr == debuggerAddr && m.embed.mc != nil {
		return nil
	}
	// §7.2：不同 addr（Electron 重启/代理更换）→ 先释放旧连接，再换新
	if m.embed.mc != nil {
		m.embed.mc.Close()
		m.embed.mc = nil
	}
	conn, err := net.Dial("tcp", debuggerAddr)
	if err != nil {
		return err
	}
	fwd := m.embed.fwd
	mc := cdp.DialMulti(conn)
	m.embed.mc = mc
	m.embed.addr = debuggerAddr

	// 视图元数据随导航刷新（state 推送 → 仅更新 title/url，不动 Debugger 绑定）
	mc.OnState = func(s cdp.TargetSnapshot) {
		m.embed.fwd.EnsureTarget(s.ID, s.Title, s.URL, nil)
		// §1.8.0 实测补丁：导航后广播 targetInfoChanged——Puppeteer target 管理器
		// 靠它刷新 targetInfo（不广播则 list_pages/goto 的 target 停留在旧 URL）
		m.embed.fwd.NotifyTargetChanged(s.ID, s.Title, s.URL)
	}

	// 阶段 3（P1）：Electron 侧真关闭视图 → 移除 target 并广播
	//（targetDestroyed + detachedFromTarget，见 RemoveTarget 的长注释：不广播 detached
	// 会让 Puppeteer 的 Page.close() 挂满 300s）。幂等：重复/未知 id 在 RemoveTarget 里早退。
	mc.OnDestroyed = func(id string) {
		m.embed.fwd.RemoveTarget(id)
	}

	// 枚举真实视图：全量重同步（清除上一 Electron 实例遗留 target，防 tN 失配），
	// 再逐个绑定（顺序 = Electron 创建顺序，首个为默认）
	snaps, err := mc.Enumerate()
	if err != nil {
		// §7.2：失败保留旧连接（若旧 mc 已被关，则 fwd 保持无绑定的可重试态）
		fmt.Fprintln(os.Stderr, "[EMBED] attach enumerate FAIL:", err)
		return fmt.Errorf("枚举内嵌视图失败: %w", err)
	}
	fmt.Fprintln(os.Stderr, "[EMBED] attach enumerated", len(snaps), "targets")
	fwd.ResyncTargets(snaps)
	for _, s := range snaps {
		fwd.EnsureTarget(s.ID, s.Title, s.URL, mc.For(s.ID))
	}

	// attach 完成信号：close channel 通知等待方（事件驱动替代轮询等待）。
	// 注意：embedAttach 全程持 m.embed.mu（本函数入口 Lock + defer Unlock）→ 直接改状态。
	if !m.embed.attachedOnce {
		m.embed.attachedOnce = true
		close(m.embed.attachedCh)
	}

	// 归并事件流 → 转发器统一事件源
	go func() {
		for te := range mc.Events() {
			fwd.IngestTagged(te)
		}
	}()

	fwd.SetOpener(mc)
	return nil
}

// embedStop 停止转发器，释放连接。
func (m *manager) embedStop() {
	m.embed.mu.Lock()
	defer m.embed.mu.Unlock()
	if m.embed.srv != nil {
		m.embed.srv.Stop()
		m.embed.srv = nil
	}
	if m.embed.fwd != nil {
		m.embed.fwd.Close() // 停事件泵 + 关统一事件流（Relay 退出）
		m.embed.fwd = nil
	}
	if m.embed.mc != nil {
		m.embed.mc.Close()
		m.embed.mc = nil
	}
	m.embed.addr = ""
	// 重置 attach 信号（重启场景：下次 embedStart 重新初始化，等待方重新监听）
	m.embed.attachedOnce = false
	m.embed.attachedCh = nil
}

// waitEmbedAttached 等待内嵌视图 attach 完成（事件驱动：监听 attach 完成信号 channel，
// 替代 100ms 轮询 + 超时放行）。前端自举（subscribeEmbedEvents）独立完成
// embedShow→attach，attach 成功时 bridge 侧 close attachedCh 通知。
// 调用方需传合理 timeout（调用方决定等待预算）；超时返回（不阻塞，由调用方决定降级）。
func (m *manager) waitEmbedAttached(timeout time.Duration) {
	m.embed.mu.Lock()
	ch := m.embed.attachedCh
	attached := m.embed.attachedOnce
	fwd := m.embed.fwd
	m.embed.mu.Unlock()
	if attached || (fwd != nil && fwd.HasAttachedTarget()) {
		return // 已 attach（含 attach 早于等待方的场景）
	}
	if ch == nil {
		fmt.Fprintln(os.Stderr, "[EMBED] waitEmbedAttached: 无 attach 信号通道（端点未启动）")
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
		return
	case <-timer.C:
		fmt.Fprintln(os.Stderr, "[EMBED] waitEmbedAttached 超时放行（视图未 attach）")
	}
}

// ensureBrowserEmbed 确保内嵌浏览器端点已启动并注入各工作区 browser 插件（embedded 模式）。
// 幂等：端点已在运行则直接复用。用于手动启用 / 自动恢复 / 配置热应用前，保证
// embedded 插件 Enable 时 serverArgs 带 --ws-endpoint（否则 chrome-devtools-mcp 回退自启系统 Chrome）。
// **事件驱动等待 attach（上限 2s）**：attach 完成（前端 browser_embed_attach 到达，
// bridge close attachedCh）立即返回，不 sleep 满 2s；2s 未 attach 放行（前端自举兜底）。
func (m *manager) ensureBrowserEmbed() (string, error) {
	ep, err := m.embedStart("go-code-embed")
	if err != nil {
		return "", err
	}
	m.applyEmbedEndpoint(ep)
	m.waitEmbedAttached(2 * time.Second)
	return ep, nil
}

// applyEmbedEndpoint 把浏览器级 ws 端点注入各工作区 browser 插件（embedded 模式时
// serverArgs 据此发 --ws-endpoint）。仅当插件已启用才需重启用以应用新 args；未启用
// 则下次 Enable 时自动带上。
// §7.3 修复：已启用插件的重启**改为异步**——原实现同步 Disable+Enable 在 dispatch 主循环内
// 执行（阻塞所有命令 + 杀正在跑的工具），是「工具调用卡住」的根因之一。
// 异步重启：先 SetEmbedEndpoint（下次 Enable 带新端点），Disable+Enable 放后台 goroutine；
// 正在跑的工具由 wrapper 的懒 ensureReady 在下次调用时重连（不中断当前调用）。
func (m *manager) applyEmbedEndpoint(endpoint string) {
	ctx := context.Background()
	m.mu.Lock()
	wsList := make([]*workspaceRuntime, 0, len(m.workspaces))
	for _, ws := range m.workspaces {
		wsList = append(wsList, ws)
	}
	m.mu.Unlock()
	for _, ws := range wsList {
		plg := ws.plugins
		cap, ok := plg.Get(browser.ID)
		if !ok {
			continue
		}
		bp, ok := cap.(*browser.Plugin)
		if !ok {
			continue
		}
		bp.SetEmbedEndpoint(endpoint)
		if st, err := plg.State(ctx, browser.ID); err == nil && st.Status == plugin.StatusEnabled {
			// 重启以应用新 serverArgs（--ws-endpoint）→ 异步（不阻塞 dispatch）
			wsPath := ws.path
			go func() {
				_ = plg.Disable(ctx, browser.ID)
				if _, err := plg.Enable(ctx, browser.ID); err != nil {
					fmt.Fprintf(os.Stderr, "✗ 应用 embed 端点后重启用失败: %v\n", err)
					return
				}
				m.emitPluginStatus(wsPath) // 重启用成功 → 推状态
			}()
		}
	}
}
