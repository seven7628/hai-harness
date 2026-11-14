// mobile_server.go —— 手机端接入：HTTP 命令代理 + WS 事件流 + 静态 PWA。
//
// 为什么放在 bridge 而不是 Electron 主进程（TECH_PLAN §1）：
// 命令分发、Session 管理、事件注入全在 bridge；放主进程等于把协议实现第二遍。
//
// 安全默认：只绑回环、必须配对、命令白名单（不能删会话/改设置/装技能）。
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"nhooyr.io/websocket"
)

const (
	mobileCmdTimeout   = 120 * time.Second
	mobileBodyLimit    = 1 << 20 // 1MB：手机只发命令，不该有更大体积
	mobileChunkBatch   = 16      // 一次最多连续写多少帧（chunk 合流的轻量版）
	mobilePingInterval = 25 * time.Second
	// mobileAppPingInterval 应用层心跳间隔：手机 45s 收不到任何帧即判定链路已死并重连。
	mobileAppPingInterval = 15 * time.Second
)

// mobileAllowedCommands 手机能调的**全部**命令。默认拒绝：新增能力要显式加进来。
var mobileAllowedCommands = map[Command]bool{
	CmdList:       true,
	CmdNewSession: true,
	CmdAsk:        true,
	// ask_batch：P8-02 手机主会话页复用桌面 Composer，发送走的是「暂存队列整队推送」
	// （store 的 flushPending → ask_batch），不是单条 ask。它与 ask 是**同一个能力**
	// （把用户消息投进会话 inbox，payload 带 workspace/session_id 归属），不放行的话
	// 手机上点发送只会拿 403 —— 而「发消息」本来就在白名单里。
	CmdAskBatch:       true,
	CmdInterrupt:      true,
	CmdApprove:        true,
	CmdAnswerQuestion: true,
	CmdBlocksPage:     true,
	CmdWorkspaceList:  true,
	// plugin_projects_list：手机左栏也要能列出插件虚拟 Project（09 §4.6 第 9 点）。
	// 只读清单，与 workspace_list 同级；开会话仍走 new_session（workspace = 虚拟路径）。
	CmdPluginProjectsList: true,
	CmdWorkspaceAdd:       true,
	CmdWorkspaceRemove:    true,
	CmdWorkspaceSync:      true,
	CmdWorkspacePick:      true,
	CmdWorkspaceUpdate:    true,
	CmdSessionRename:      true,
	CmdSessionPin:         true,
	CmdDeleteSession:      true,
	// 子智能体（子 Session）：列表由 new_session.agents 带回，下面是查看与对话
	CmdAgentHistory:  true,
	CmdAgentSend:     true,
	CmdInterruptTask: true,
	CmdMobileStatus:  true,
	// —— Agent Hub（P4-03a）——
	// 只开手机 UI 日常需要的读/发/安全写；智能体 CRUD、记忆/提示词/回滚、
	// 群组 CRUD/成员增删仍只在桌面端（误操作代价高，且不是移动场景的日常动作）。
	// 只读 / 打开 / 读历史
	CmdHubAgentList:     true, // hub_agent_list
	CmdHubAgentGet:      true, // hub_agent_get
	CmdHubGroupList:     true, // hub_group_list
	CmdHubGroupGet:      true, // hub_group_get
	CmdHubOpen:          true, // hub_open
	CmdHubHistory:       true, // hub_history
	CmdHubGroupHistory:  true, // hub_group_history
	CmdHubGroupActivity: true, // hub_group_activity
	CmdHubTaskList:      true, // hub_task_list
	CmdHubTaskGet:       true, // hub_task_get
	CmdHubNeedList:      true, // hub_need_list
	CmdHubMarkRead:      true, // hub_mark_read
	CmdHubArtifactGet:   true, // hub_artifact_get
	CmdHubSearch:        true, // hub_search（人类命令；手机端只读）
	// 安全写：发消息 / 派任务 / 停止 / 取消 / 重试 / 解决 need / 清空 DM
	CmdHubSend:             true, // hub_send
	CmdHubGroupSend:        true, // hub_group_send
	CmdHubGroupAssign:      true, // hub_group_assign
	CmdHubConversationStop: true, // hub_conversation_stop
	CmdHubTaskCancel:       true, // hub_task_cancel
	CmdHubTaskRetry:        true, // hub_task_retry
	CmdHubNeedResolve:      true, // hub_need_resolve
	CmdHubClear:            true, // hub_clear
}

type mobileServer struct {
	mgr  *manager
	pair *mobilePairing
	hub  *mobileHub
	addr string
	lan  bool   // true = 绑 0.0.0.0（手机经局域网/Tailscale 访问）
	cfg  string // ~/.go-code/mobile/config.json

	ln   net.Listener
	http *http.Server

	dispatchMu sync.Mutex // 与 stdin 循环串行化：manager.dispatch 不是并发安全的
	// dispatch 可注入（测试替身）。nil = 走 mgr.dispatch。
	dispatch func(command)
	mu       sync.Mutex
	nextID   int
	pending  map[int]chan map[string]any
}

func newMobileServer(m *manager, dataDir string) *mobileServer {
	dir := filepath.Join(dataDir, "mobile")
	s := &mobileServer{
		mgr:     m,
		cfg:     filepath.Join(dir, "config.json"),
		pair:    newMobilePairing(filepath.Join(dir, "devices.json")),
		hub:     newMobileHub(),
		nextID:  1_000_000_000, // 与 stdin 命令 id 错开，避免响应串线
		pending: map[int]chan map[string]any{},
	}
	// 响应拦截：命令响应不再只是写 stdout，先问手机端有没有在等这个 id。
	if m != nil {
		m.respTap = s.tap
	}
	return s
}

// attachEvents 把 hub_* 事件接到 bridge 输出总线（只挂一次，见 out.writeLine）。
func (s *mobileServer) attachEvents() {
	if s == nil || s.mgr == nil || s.mgr.out == nil {
		return
	}
	s.mgr.out.setLineTap(s.onOutLine)
}

// onOutLine 输出总线钩子：只把 hub_* 行投给 hub 作用域订阅者。
// 运行在 out 写锁内：只做非阻塞 channel 投递，不 dispatch/Ask/写 stdout。
func (s *mobileServer) onOutLine(line []byte) {
	if s == nil || s.hub == nil || !isHubEvent(line) {
		return
	}
	s.hub.broadcastHub(line)
}

// start 绑定监听。优先复用上次的端口（手机端保存的 URL 才不会每次失效）。
//
// 监听范围由 ~/.go-code/mobile/config.json 的 lan 决定：
//   - false（默认）：只绑回环，手机必须经 Tailscale/端口转发才够得着；
//   - true：绑 0.0.0.0，同 Wi-Fi 直连。任何能拿到 token 的设备都可访问，
//     所以这是用户显式开关，不是默认值。
func (s *mobileServer) start(preferred string) error {
	s.lan = s.loadLan()
	// preferred 只用来复用端口，绑定接口由 lan 决定：调用方（main.go）固定传
	// "127.0.0.1:47800"，若整串照用，config 里的 lan 就永远被覆盖 —— 表现为
	// 「面板里开了局域网，重启后又只剩回环，手机连不上」。
	port := "47800"
	if _, p, err := net.SplitHostPort(preferred); err == nil && p != "" {
		port = p
	}
	bindHost := "127.0.0.1"
	if s.lan {
		bindHost = "0.0.0.0"
	}
	preferred = net.JoinHostPort(bindHost, port)
	ln, err := net.Listen("tcp", preferred)
	if err != nil && !strings.HasSuffix(preferred, ":0") {
		// 上次的端口被别的进程占了 → 退到随机端口（只影响 URL，不影响 token）
		ln, err = net.Listen("tcp", s.listenAddrRandom())
	}
	if err != nil {
		return err
	}
	s.ln = ln
	s.addr = ln.Addr().String()
	s.saveAddr(s.addr)
	// Hub 事件由各 hub_* 生产方走 m.out.writeLine（不是 bs.emit），
	// 在输出总线上挂一次钩子即可覆盖全部事件类型；onOutLine 只转发 hub_*。
	s.attachEvents()

	s.http = &http.Server{Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "mobile server:", err)
		}
	}()
	return nil
}

// listenAddr 固定端口 + 当前模式；listenAddrRandom 同模式随机端口（端口被占时兜底）。
func (s *mobileServer) listenAddr() string {
	if s.lan {
		return "0.0.0.0:47800"
	}
	return "127.0.0.1:47800"
}

func (s *mobileServer) listenAddrRandom() string {
	if s.lan {
		return "0.0.0.0:0"
	}
	return "127.0.0.1:0"
}

func (s *mobileServer) loadLan() bool {
	b, err := os.ReadFile(s.cfg)
	if err != nil {
		return false
	}
	var doc struct {
		Lan bool `json:"lan"`
	}
	return json.Unmarshal(b, &doc) == nil && doc.Lan
}

func (s *mobileServer) saveLan(lan bool) error {
	if err := os.MkdirAll(filepath.Dir(s.cfg), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(map[string]any{"lan": lan}, "", "  ")
	return writeMobileAtomic(s.cfg, b)
}

// setLan 切换监听范围：落盘 → 重建 listener。已连接的手机 WS 会断开并自动重连。
func (s *mobileServer) setLan(lan bool) error {
	if s.lan == lan {
		return nil
	}
	if err := s.saveLan(lan); err != nil {
		return err
	}
	return s.relisten(lan)
}

func (s *mobileServer) relisten(lan bool) error {
	if s.http != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.http.Shutdown(ctx)
		cancel()
	}
	s.lan = lan
	ln, err := net.Listen("tcp", s.listenAddr())
	if err != nil {
		ln, err = net.Listen("tcp", s.listenAddrRandom())
		if err != nil {
			return err
		}
	}
	s.ln = ln
	s.addr = ln.Addr().String()
	s.http = &http.Server{Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "mobile server:", err)
		}
	}()
	s.saveAddr(s.addr)
	return nil
}

// status 给桌面设置页与手机端自身用：端口、配对码、设备列表。

func (s *mobileServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ping", s.handlePing)
	mux.HandleFunc("/api/pair", s.handlePair)
	mux.HandleFunc("/api/cmd", s.handleCmd)
	mux.HandleFunc("/api/events", s.handleEvents)
	// 手机页不可用时**不能静默 404**：/m/ 返回 500 + 原因（页面打不开时一眼看到为什么），
	// 同时写 stderr 进 bridge 日志。
	sub, subErr := mobileWebSub(os.Getenv("HAI_MOBILE_WEB_DIR"))
	if subErr != nil {
		fmt.Fprintln(os.Stderr, "mobile web:", subErr)
		mux.HandleFunc("/m/", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "手机页面不可用: "+subErr.Error(), http.StatusInternalServerError)
		})
	} else {
		mux.Handle("/m/", http.StripPrefix("/m/", noStore(http.FileServer(sub))))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/m/", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})
	return mux
}

// mobileWebSub 手机端静态资源根。
//
// P8 方案 A：手机页和桌面渲染页一样是**一个磁盘目录**，不 embed、不 zip、不缓存：
//
//	· 开发态：electron/main.ts 设 HAI_MOBILE_WEB_DIR=<repo>/desktop/app/dist-mobile；
//	· 打包态：electron-builder 把 dist-mobile 复制成 Resources/mobile-web，
//	  main.ts 设 HAI_MOBILE_WEB_DIR=<resources>/mobile-web。
//
// 目录里必须有 index.html（mobile:build 会把 mobile.html 改名），否则 /m/ 直接报错。
// bridge 单独运行（没有 Electron 资源目录）时 /m/ 返回 500 + 明确原因，而不是静默 404。
func mobileWebSub(diskDir string) (http.FileSystem, error) {
	if diskDir == "" {
		return nil, fmt.Errorf("HAI_MOBILE_WEB_DIR 未设置：手机页需要随应用分发 dist-mobile（开发态先跑 npm run mobile:build）")
	}
	st, err := os.Stat(diskDir)
	if err != nil {
		return nil, fmt.Errorf("HAI_MOBILE_WEB_DIR=%q 不可用: %w（开发态先跑 npm run mobile:build）", diskDir, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("HAI_MOBILE_WEB_DIR=%q 不是目录", diskDir)
	}
	if st, err := os.Stat(filepath.Join(diskDir, "index.html")); err != nil || st.IsDir() {
		return nil, fmt.Errorf("HAI_MOBILE_WEB_DIR=%q 缺少 index.html（mobile:build 会把 mobile.html 改名）", diskDir)
	}
	return http.Dir(diskDir), nil
}

func (s *mobileServer) close() {
	if s == nil || s.http == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.http.Shutdown(ctx)
}

func noStore(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		h.ServeHTTP(w, r)
	})
}

// ---------- 鉴权 ----------

func (s *mobileServer) auth(r *http.Request) (string, bool) {
	token := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		token = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if token == "" {
		token = r.URL.Query().Get("token") // 浏览器 WebSocket 不能设头
	}
	return s.pair.validate(token, time.Now())
}

// ---------- HTTP 端点 ----------

// handlePing 同时充当凭据自检：手机带着 token 打一次就知道还用不用重新配对。
// 不带 token 也返回 200（用于探测服务是否在线），只是 paired=false。
func (s *mobileServer) handlePing(w http.ResponseWriter, r *http.Request) {
	_, paired := s.auth(r)
	writeMobileJSON(w, http.StatusOK, map[string]any{
		"app":     "hai",
		"version": "mobile-p0",
		"paired":  paired,
	})
}

func (s *mobileServer) handlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMobileJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method"})
		return
	}
	var req struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := decodeMobileJSON(r, &req); err != nil {
		writeMobileJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	token, deviceID, err := s.pair.pair(strings.TrimSpace(req.Code), strings.TrimSpace(req.Name), time.Now())
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errMobileRateLimited) {
			status = http.StatusTooManyRequests
		}
		writeMobileJSON(w, status, map[string]any{"error": err.Error()})
		return
	}
	writeMobileJSON(w, http.StatusOK, map[string]any{
		"token":     token,
		"device_id": deviceID,
		"addr":      s.addr,
	})
}

func (s *mobileServer) handleCmd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMobileJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method"})
		return
	}
	if _, ok := s.auth(r); !ok {
		writeMobileJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	var req struct {
		ID      int            `json:"id"`
		Cmd     string         `json:"cmd"`
		Payload map[string]any `json:"payload"`
	}
	if err := decodeMobileJSON(r, &req); err != nil {
		writeMobileJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	cmd := Command(req.Cmd)
	if !mobileAllowedCommands[cmd] {
		writeMobileJSON(w, http.StatusForbidden, map[string]any{"error": "command not allowed: " + req.Cmd})
		return
	}
	if req.Payload == nil {
		req.Payload = map[string]any{}
	}

	id, ch := s.expect()
	defer s.forget(id)

	call := command{Id: id, Type: cmd, Payload: req.Payload}
	s.dispatchMu.Lock()
	if s.dispatch != nil {
		s.dispatch(call)
	} else {
		s.mgr.dispatch(call)
	}
	s.dispatchMu.Unlock()

	select {
	case resp := <-ch:
		writeMobileJSON(w, http.StatusOK, resp)
	case <-time.After(mobileCmdTimeout):
		writeMobileJSON(w, http.StatusGatewayTimeout, map[string]any{"error": "命令超时"})
	case <-r.Context().Done():
		// 手机走了：响应没人收，直接放弃（tap 会因为没有管道而落回 stdout）
	}
}

// handleEvents 事件流。scope 空 = 会话流（按 workspace + session 过滤）；
// scope=hub = Hub 应用级流（只收 hub_*，不绑定会话）。两类订阅严格隔离。
// liveFrame 当前会话「正在流的那一段」（正文 + 思考）快照。
// 手机 WS 重连、或弱网丢帧后据此直接替换 live 屏，不必整页重拉会话快照。
// 返回 nil 表示会话不存在（此时调用方退回 resync）。
func (s *mobileServer) liveFrame(ws, sid string) []byte {
	if s == nil || s.mgr == nil || sid == "" {
		return nil
	}
	bs := s.mgr.get(ws, sid)
	if bs == nil {
		return nil
	}
	text, think := bs.liveSnapshot()
	return mustMobileJSON(map[string]any{
		"type":     "live",
		"text":     text,
		"thinking": think,
		"running":  bs.running.Load(),
	})
}

func (s *mobileServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	device, ok := s.auth(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	_ = device
	scope := r.URL.Query().Get("scope")
	if scope != "" && scope != "hub" {
		http.Error(w, "bad scope", http.StatusBadRequest)
		return
	}
	ws := r.URL.Query().Get("workspace")
	sid := r.URL.Query().Get("session_id")
	var sub *mobileSub
	if scope == "hub" {
		// Hub 是应用级数据面：不绑定任何 workspace/session，只收 hub_*。
		ws, sid = "", ""
		sub = s.hub.subscribeHub()
	} else {
		sub = s.hub.subscribe(ws, sid)
	}
	defer s.hub.unsubscribe(sub)

	// 页面由本进程托管（/m/），同源即可；token 是硬门槛，不额外放开跨站 Origin。
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	ctx := r.Context()
	hello := map[string]any{"type": "hello", "workspace": ws, "session_id": sid}
	if scope == "hub" {
		hello["scope"] = "hub"
	}
	_ = c.Write(ctx, websocket.MessageText, mustMobileJSON(hello))
	// 连上先把「正在流的那一段」补上：手机息屏/切后台期间断流，重连时不能空屏等下一个 token。
	// Hub 订阅没有会话 live 快照，直接等 hub_* 事件（丢帧时主循环会发 resync）。
	if scope != "hub" {
		if f := s.liveFrame(ws, sid); f != nil {
			if err := c.Write(ctx, websocket.MessageText, f); err != nil {
				return
			}
		}
	}

	ping := time.NewTicker(mobilePingInterval)
	defer ping.Stop()
	appPing := time.NewTicker(mobileAppPingInterval)
	defer appPing.Stop()
	pingCtx, cancelPing := context.WithCancel(ctx)
	defer cancelPing()
	go func() {
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-ping.C:
				if err := c.Ping(pingCtx); err != nil {
					cancelPing()
					return
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-appPing.C:
			// 应用层心跳（主循环单写者）：iOS 息屏常出现「链路已死但 socket 未 close」，
			// 协议级 ping 由浏览器自动回 pong、看不出问题；手机靠这条帧判断该不该重连。
			if err := c.Write(ctx, websocket.MessageText, mustMobileJSON(map[string]any{"type": "ping"})); err != nil {
				return
			}
		case line, ok := <-sub.ch:
			if !ok {
				return
			}
			if sub.dropped.Swap(false) {
				// 丢过帧：先补「正在流的那一段」快照，再发积压里剩下的事件。
				// 积压的 chunk 已包含在快照文本里，必须丢弃 —— 否则同一段话会出现两遍；
				// 控制事件（审批/提问/agent_end）不能丢，攒起来按原序补发。
				kept, closed := drainControl(sub)
				if closed {
					return
				}
				frame := s.liveFrame(ws, sid)
				if frame == nil {
					frame = mustMobileJSON(map[string]any{"type": "resync"})
				}
				if err := c.Write(ctx, websocket.MessageText, frame); err != nil {
					return
				}
				for _, f := range kept {
					if err := c.Write(ctx, websocket.MessageText, f); err != nil {
						return
					}
				}
				if isDroppableEvent(line) {
					continue // 当前帧同样已在快照里（会话 chunk / Hub delta）
				}
			}
			frames := [][]byte{line}
			for len(frames) < mobileChunkBatch {
				if sub.dropped.Load() {
					break
				}
				select {
				case next, ok := <-sub.ch:
					if !ok {
						return
					}
					frames = append(frames, next)
				default:
					goto flush
				}
			}
		flush:
			for _, f := range frames {
				if err := c.Write(ctx, websocket.MessageText, f); err != nil {
					return
				}
			}
		}
	}
}

func (s *mobileServer) expect() (int, chan map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	ch := make(chan map[string]any, 1)
	s.pending[id] = ch
	return id, ch
}

func (s *mobileServer) forget(id int) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

// tap 在 respCode 里被调用：命中手机在等的 id → 投进管道，并阻止写 stdout。
func (s *mobileServer) tap(line map[string]any) bool {
	if line["event_type"] != "command_response" {
		return false
	}
	id, ok := line["id"].(int)
	if !ok {
		return false
	}
	s.mu.Lock()
	ch, hit := s.pending[id]
	s.mu.Unlock()
	if !hit {
		return false
	}
	select {
	case ch <- line:
	default:
	}
	return true
}

// status 给桌面设置页与手机端自身用：端口、配对码、设备列表。
func (s *mobileServer) status() map[string]any {
	code, exp := s.pair.newCode(time.Now())
	devices := s.pair.list()
	safe := make([]map[string]any, 0, len(devices))
	for _, d := range devices {
		safe = append(safe, map[string]any{
			"id": d.ID, "name": d.Name,
			"created_at": d.CreatedAt, "last_seen": d.LastSeen,
		})
	}
	urls := mobileURLs(s.addr, s.lan)
	return map[string]any{
		"enabled": s != nil && s.addr != "",
		"addr":    s.addr,
		"lan":     s.lan,
		"urls":    urls,
		"qr":      mobilePairQR(urls, code),
		"code":    code,
		"expires": exp.Unix(),
		"devices": safe,
	}
}

// ---------- 工具函数 ----------

func decodeMobileJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, mobileBodyLimit))
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("请求体解析失败: %w", err)
	}
	return nil
}

func writeMobileJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func mustMobileJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func (s *mobileServer) saveAddr(addr string) {
	dir := filepath.Dir(s.pair.path)
	_ = os.MkdirAll(dir, 0o700)
	b, _ := json.MarshalIndent(map[string]any{"addr": addr, "pid": os.Getpid()}, "", "  ")
	_ = writeMobileAtomic(filepath.Join(dir, "server.json"), b)
}

// mobileURLs 手机可用的入口地址：回环模式只给 127.0.0.1；局域网模式枚举
// 非回环 IPv4（含 Tailscale 的 100.x），跳过链路本地 169.254。
func mobileURLs(addr string, lan bool) []string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	if !lan {
		return []string{"http://127.0.0.1:" + port + "/m/"}
	}
	out := []string{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, "http://"+ip.String()+":"+port+"/m/")
		}
	}
	return out
}

// mobilePairQR 把「入口地址 + 配对码」编成二维码（data URL）。
//
// 只编局域网地址：回环地址手机扫了也打不开，宁可不给码也别给错码。
// 码本身已经在同一块面板上明文显示，编进二维码不增加暴露面（5 分钟、单次）。
func mobilePairQR(urls []string, code string) string {
	if len(urls) == 0 || code == "" {
		return ""
	}
	target := pickPairURL(urls)
	if target == "" {
		return ""
	}
	png, err := qrcode.Encode(target+"#pair="+code, qrcode.Medium, 320)
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

// pickPairURL 选二维码里编哪个地址：优先 Tailscale（100.64.0.0/10），
// 其次任一非回环地址。理由：同一 Wi-Fi 下两个地址都能用，而在外面时
// 只有 100.x 能用 —— 选它 = 两种场景都不出错。
func pickPairURL(urls []string) string {
	fallback := ""
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		host := u.Hostname()
		if host == "" || host == "127.0.0.1" || host == "localhost" {
			continue
		}
		if ip := net.ParseIP(host); ip != nil && isTailscaleIP(ip) {
			return raw
		}
		if fallback == "" {
			fallback = raw
		}
	}
	return fallback
}

// isTailscaleIP Tailscale 用 CGNAT 段 100.64.0.0/10 分配节点地址。
func isTailscaleIP(ip net.IP) bool {
	_, cg, err := net.ParseCIDR("100.64.0.0/10")
	return err == nil && cg.Contains(ip)
}
