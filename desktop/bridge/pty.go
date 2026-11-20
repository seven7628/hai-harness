// [implemented] 依据 docs/TERMINAL_BRIDGE_SPEC.md §1.3/§1.4/§1.5 与
// 已还原的 components/terminal/{frame,paneMachine}-test.ts 契约实现。
//
// 右栏终端（PTY）——桌面桥控制面 + 数据面。
//
// 控制面（NDJSON，与本进程既有命令同通道）：
//
//	pty_create {workspace, session_id?, cols, rows} → {pty_id, cwd, shell, pid, cols, rows}
//	pty_ticket {pty_id}                             → {url, ticket, expires_in}
//	pty_close  {pty_id}                             → {}
//
// 数据面（WebSocket，path=/pty，一次性 ticket 鉴权）：
//
//	帧格式见 docs/TERMINAL_BRIDGE_SPEC.md §1.2：首字节 0x00 = 控制帧（UTF-8 JSON），
//	否则整帧原样当 PTY 输出/输入。畸形控制帧**回落成数据**，绝不因此断连。
//
// 事件（走既有 event 通道，不是 WS）：pty_changed {pty_id, status, exit_code?, error?}。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/creack/pty"
	"nhooyr.io/websocket"
)

const (
	// ptyCtrlByte 控制帧首字节。与前端 frame.ts 的 CTRL_BYTE 必须一致。
	ptyCtrlByte byte = 0x00
	// ptyTicketTTL 一次性 ticket 有效期：前端拿到即连，30s 足够覆盖建连+重试。
	ptyTicketTTL = 30 * time.Second
	// ptyMaxWriteChunk 单次从 master 读到内存的上限（避免超大帧一次性占满内存）。
	ptyMaxWriteChunk = 32 * 1024
)

// ptySession 一个存活/已退出的 PTY 会话。master 关闭即视为会话结束。
type ptySession struct {
	id        string
	workspace string
	sessionID string
	shell     string
	cols      uint16
	rows      uint16

	cmd *exec.Cmd
	mf  *os.File // master

	// done 在子进程退出时关闭；exitCode 在关闭前写入（配合同一把 mu 读）。
	done     chan struct{}
	mu       sync.Mutex
	exitCode int
	exited   bool
	closed   bool
}

// ptyTicket 一次性连接票据（短 TTL，用完即删）。
type ptyTicket struct {
	ptyID   string
	expires time.Time
}

// ptyManager 终端会话与 ticket 表。懒初始化（首个 pty_create 时建服务器）。
type ptyManager struct {
	mu       sync.Mutex
	sessions map[string]*ptySession
	tickets  map[string]ptyTicket
	srv      *ptyServer
	out      *out
}

type ptyServer struct {
	pm   *ptyManager
	ln   net.Listener
	http *http.Server
	addr string
}

// ptyManagerFor 懒建 manager 级单例。
func (m *manager) ptyManagerFor() *ptyManager {
	m.ptyMu.Lock()
	defer m.ptyMu.Unlock()
	if m.pty == nil {
		m.pty = &ptyManager{
			sessions: map[string]*ptySession{},
			tickets:  map[string]ptyTicket{},
			out:      m.out,
		}
	}
	return m.pty
}

// handlePtyCommand 由 dispatch 前置调用；返回 true 表示该命令已被本文件消费。
func (m *manager) handlePtyCommand(c command) bool {
	switch c.Type {
	case CmdPtyCreate:
		m.ptyCreateCmd(c)
	case CmdPtyTicket:
		m.ptyTicketCmd(c)
	case CmdPtyClose:
		m.ptyCloseCmd(c)
	default:
		return false
	}
	return true
}

func (m *manager) ptyCreateCmd(c command) {
	pm := m.ptyManagerFor()
	workspace := str(c.Payload, "workspace")
	if workspace == "" {
		m.resp(c, false, "pty_create: 缺少 workspace", nil)
		return
	}
	if st, err := os.Stat(workspace); err != nil || !st.IsDir() {
		m.resp(c, false, "pty_create: workspace 不是目录: "+workspace, nil)
		return
	}
	cols := uint16(intVal(c.Payload, "cols", 80))
	rows := uint16(intVal(c.Payload, "rows", 24))
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	sess, err := pm.create(workspace, str(c.Payload, "session_id"), cols, rows)
	if err != nil {
		m.resp(c, false, "pty_create: "+err.Error(), nil)
		return
	}
	m.resp(c, true, "", map[string]any{
		"pty_id": sess.id,
		"cwd":    workspace,
		"shell":  sess.shell,
		"pid":    sess.cmd.Process.Pid,
		"cols":   int(cols),
		"rows":   int(rows),
	})
}

func (m *manager) ptyTicketCmd(c command) {
	pm := m.ptyManagerFor()
	id := str(c.Payload, "pty_id")
	if id == "" {
		m.resp(c, false, "pty_ticket: 缺少 pty_id", nil)
		return
	}
	srv, err := pm.ensureServer()
	if err != nil {
		m.resp(c, false, "pty_ticket: "+err.Error(), nil)
		return
	}
	ticket, err := pm.issueTicket(id)
	if err != nil {
		m.resp(c, false, "pty_ticket: "+err.Error(), nil)
		return
	}
	m.resp(c, true, "", map[string]any{
		"url":        "ws://" + srv.addr + "/pty",
		"ticket":     ticket,
		"expires_in": int(ptyTicketTTL / time.Second),
	})
}

func (m *manager) ptyCloseCmd(c command) {
	pm := m.ptyManagerFor()
	id := str(c.Payload, "pty_id")
	if id == "" {
		m.resp(c, false, "pty_close: 缺少 pty_id", nil)
		return
	}
	pm.closeSession(id, "close")
	m.resp(c, true, "", nil)
}

// —— ptyManager ——

func (pm *ptyManager) create(workspace, sessionID string, cols, rows uint16) (*ptySession, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		for _, cand := range []string{"/bin/zsh", "/bin/bash", "/bin/sh"} {
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				shell = cand
				break
			}
		}
	}
	if shell == "" {
		return nil, errors.New("找不到可用 shell")
	}
	cmd := exec.Command(shell, "-i")
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")

	mf, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	id, err := randomToken()
	if err != nil {
		_ = mf.Close()
		_ = cmd.Process.Kill()
		return nil, err
	}
	s := &ptySession{
		id: id, workspace: workspace, sessionID: sessionID,
		shell: shell, cols: cols, rows: rows, cmd: cmd, mf: mf,
		done: make(chan struct{}),
	}
	pm.mu.Lock()
	pm.sessions[id] = s
	pm.mu.Unlock()

	go func() {
		err := cmd.Wait()
		code := 0
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		} else if err != nil {
			code = -1
		}
		s.mu.Lock()
		s.exitCode = code
		s.exited = true
		s.mu.Unlock()
		close(s.done)
		// master 侧读到 EOF 后由连接协程收尾；这里保证 master 最终关闭。
		_ = mf.Close()
		pm.remove(s.id)
		pm.emitChanged(s.id, "exited", code, "")
	}()
	return s, nil
}

func (pm *ptyManager) get(id string) *ptySession {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.sessions[id]
}

func (pm *ptyManager) remove(id string) {
	pm.mu.Lock()
	delete(pm.sessions, id)
	pm.mu.Unlock()
}

func (pm *ptyManager) closeSession(id, reason string) {
	s := pm.get(id)
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = s.mf.Close()
	pm.remove(id)
	if reason == "close" {
		pm.emitChanged(id, "exited", -1, "")
	}
}

func (pm *ptyManager) emitChanged(ptyID, status string, exitCode int, errMsg string) {
	if pm.out == nil {
		return
	}
	line := map[string]any{"event_type": "pty_changed", "pty_id": ptyID, "status": status}
	if status == "exited" {
		line["exit_code"] = exitCode
	}
	if errMsg != "" {
		line["error"] = errMsg
	}
	pm.out.writeLine(line)
}

func (pm *ptyManager) issueTicket(ptyID string) (string, error) {
	s := pm.get(ptyID)
	if s == nil {
		return "", errors.New("pty 不存在或已结束")
	}
	s.mu.Lock()
	exited := s.exited
	s.mu.Unlock()
	if exited {
		return "", errors.New("pty 已退出")
	}
	tk, err := randomToken()
	if err != nil {
		return "", err
	}
	pm.mu.Lock()
	pm.tickets[tk] = ptyTicket{ptyID: ptyID, expires: time.Now().Add(ptyTicketTTL)}
	// 顺带清掉过期票据，避免长会话里无限增长。
	now := time.Now()
	for k, v := range pm.tickets {
		if now.After(v.expires) {
			delete(pm.tickets, k)
		}
	}
	pm.mu.Unlock()
	return tk, nil
}

// consumeTicket 一次性消费：无论是否有效都从表中删除（防重放）。
func (pm *ptyManager) consumeTicket(tk string) (string, bool) {
	pm.mu.Lock()
	t, ok := pm.tickets[tk]
	if ok {
		delete(pm.tickets, tk)
	}
	pm.mu.Unlock()
	if !ok || time.Now().After(t.expires) {
		return "", false
	}
	return t.ptyID, true
}

func (pm *ptyManager) ensureServer() (*ptyServer, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.srv != nil {
		return pm.srv, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	srv := &ptyServer{pm: pm, ln: ln, addr: ln.Addr().String()}
	srv.http = &http.Server{Handler: srv.routes(), ReadHeaderTimeout: 10 * time.Second}
	pm.srv = srv
	go func() {
		if err := srv.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "pty server:", err)
		}
	}()
	return srv, nil
}

func (pm *ptyManager) shutdown() {
	pm.mu.Lock()
	sessions := make([]*ptySession, 0, len(pm.sessions))
	for _, s := range pm.sessions {
		sessions = append(sessions, s)
	}
	srv := pm.srv
	pm.srv = nil
	pm.tickets = map[string]ptyTicket{}
	pm.mu.Unlock()
	for _, s := range sessions {
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		_ = s.mf.Close()
	}
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.http.Shutdown(ctx)
	}
}

func (s *ptyServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/pty", s.handlePty)
	return mux
}

func (s *ptyServer) handlePty(w http.ResponseWriter, r *http.Request) {
	tk := r.URL.Query().Get("ticket")
	ptyID, ok := s.pm.consumeTicket(tk)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sess := s.pm.get(ptyID)
	if sess == nil {
		http.Error(w, "gone", http.StatusGone)
		return
	}
	// ticket 已是硬门槛（一次性 + 短 TTL），本机回环连接；不额外校验 Origin，
	// 否则 file:// / vite dev 端口都会与 127.0.0.1:随机端口不同源而被拒。
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	ctx := r.Context()

	// 握手：hello 不改变前端状态、无副作用（paneMachine.ts case "hello"）。
	_ = c.Write(ctx, websocket.MessageBinary, encodeCtrl(map[string]any{"t": "hello"}))

	// 已退出的会话：直接补一条 exit 控制帧再关。
	sess.mu.Lock()
	exited, code := sess.exited, sess.exitCode
	sess.mu.Unlock()
	if exited {
		_ = c.Write(ctx, websocket.MessageBinary, encodeCtrl(map[string]any{"t": "exit", "code": code}))
		return
	}

	// 单写者：master→WS 的协程与客户端→master 的循环都往 c.Write；
	// nhooyr 允许并发写，但为保持帧序这里用一把写锁。
	var writeMu sync.Mutex
	writeWS := func(b []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return c.Write(wctx, websocket.MessageBinary, b)
	}

	// master → WS 协程。
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, ptyMaxWriteChunk)
		for {
			n, rerr := sess.mf.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				if werr := writeWS(chunk); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// 客户端 → master：数据帧原样写入；控制帧处理 resize。
	for {
		typ, data, rerr := c.Read(ctx)
		if rerr != nil {
			break
		}
		if typ != websocket.MessageBinary {
			continue
		}
		ctrl, payload := decodeFrame(data)
		if ctrl != nil {
			switch asString(ctrl["t"]) {
			case "resize":
				cols := uint16(asInt(ctrl["cols"]))
				rows := uint16(asInt(ctrl["rows"]))
				if cols > 0 && rows > 0 {
					sess.mu.Lock()
					sess.cols, sess.rows = cols, rows
					sess.mu.Unlock()
					_ = pty.Setsize(sess.mf, &pty.Winsize{Cols: cols, Rows: rows})
				}
			}
			continue
		}
		if len(payload) > 0 {
			if _, werr := sess.mf.Write(payload); werr != nil {
				break
			}
		}
	}

	// 客户端断开或 master EOF：收尾。会话本身由 pty_close / 进程退出关闭。
	sess.mu.Lock()
	exited, code = sess.exited, sess.exitCode
	sess.mu.Unlock()
	if exited {
		_ = writeWS(encodeCtrl(map[string]any{"t": "exit", "code": code}))
	}
	_ = sess.mf.Close()
	<-readDone
}

// encodeCtrl 控制帧 = 0x00 + UTF-8 JSON（frame.ts encodeCtrl 同构）。
func encodeCtrl(v any) []byte {
	body, err := json.Marshal(v)
	if err != nil {
		return []byte{ptyCtrlByte}
	}
	out := make([]byte, len(body)+1)
	out[0] = ptyCtrlByte
	copy(out[1:], body)
	return out
}

// decodeFrame 与前端 decodeFrame 同构：首字节非 0x00 / 长度不足 / 畸形 JSON /
// t 为空 → 一律回落成数据帧，绝不判定连接死亡。
func decodeFrame(buf []byte) (ctrl map[string]any, data []byte) {
	if len(buf) < 2 || buf[0] != ptyCtrlByte {
		return nil, buf
	}
	var c map[string]any
	if err := json.Unmarshal(buf[1:], &c); err != nil {
		return nil, buf
	}
	if asString(c["t"]) == "" {
		return nil, buf
	}
	return c, nil
}

// —— 小工具 ——

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func intVal(payload map[string]any, key string, def int) int {
	if payload == nil {
		return def
	}
	switch v := payload[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}
