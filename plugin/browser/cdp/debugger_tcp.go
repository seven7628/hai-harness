package cdp

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seven7628/hai-harness/plugin/browser/trace"
)

// TCPDebugger 实现 Debugger 接口，经 JSON 行协议连接 Electron 内嵌视图的 debugger 代理
// （对应 desktop/app/electron/embed.ts 的 CDP TCP 代理）。每行一个 JSON 对象：
//
//	请求  → {"type":"cmd","id":N,"method":"...","params":{...}}
//	响应  → {"type":"resp","id":N,"result":{...}}  或  {"type":"resp","id":N,"error":"..."}
//	事件  → {"type":"event","method":"...","params":{...}}  （异步推送）
//
// 纯网络逻辑，可经 net.Pipe + 内存伪代理单测（无需绑定端口）。
type TCPDebugger struct {
	conn   net.Conn
	w      *bufio.Writer
	wmu    sync.Mutex
	pend   sync.Map // int64 -> chan respMsg
	seq    atomic.Int64
	events chan Event
	closed atomic.Bool
}

// respMsg 一条命令的响应（经 pend 回投给 SendCommand）。
type respMsg struct {
	Result map[string]any
	Err    string
}

// NewTCPDebugger 包装一条已建立的连接（client 侧），启动读取协程。
func NewTCPDebugger(conn net.Conn) *TCPDebugger {
	d := &TCPDebugger{conn: conn, w: bufio.NewWriter(conn), events: make(chan Event, 64)}
	go d.readLoop()
	return d
}

func (d *TCPDebugger) readLoop() {
	sc := bufio.NewScanner(d.conn)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for sc.Scan() {
		var m struct {
			Type   string         `json:"type"`
			ID     int64          `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
			Result map[string]any `json:"result"`
			Error  string         `json:"error"`
		}
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		switch m.Type {
		case "resp":
			if ch, ok := d.pend.LoadAndDelete(m.ID); ok {
				select {
				case ch.(chan respMsg) <- respMsg{Result: m.Result, Err: m.Error}:
				default: // 无人等待则丢弃（防御）
				}
			}
		case "event":
			select {
			case d.events <- Event{Method: m.Method, Params: m.Params}:
			default: // 背压：事件队列满则丢弃（防御）
			}
		}
	}
	close(d.events)
}

// SendCommand 发送页面级命令，等待匹配响应（带超时）。
func (d *TCPDebugger) SendCommand(method string, params map[string]any) (map[string]any, error) {
	trStart := time.Now()
	defer func() { trace.Phase("tcp.SendCommand("+method+")", trStart) }()
	if d.closed.Load() {
		return nil, errors.New("debugger 连接已关闭")
	}
	id := d.seq.Add(1)
	// 先登记 pend 再写：写入立即解除对端读取，响应可能先于 Store 到达；
	// 若后 Store，响应会被 readLoop 的 LoadAndDelete 误判为「无等待者」而丢弃 → 永久阻塞。
	ch := make(chan respMsg, 1)
	d.pend.Store(id, ch)

	raw, _ := json.Marshal(map[string]any{"type": "cmd", "id": id, "method": method, "params": params})

	d.wmu.Lock()
	_, werr := d.w.Write(append(raw, '\n'))
	if werr == nil {
		werr = d.w.Flush()
	}
	d.wmu.Unlock()
	if werr != nil {
		d.pend.Delete(id)
		return nil, werr
	}

	select {
	case r := <-ch:
		if r.Err != "" {
			return nil, errors.New(r.Err)
		}
		return r.Result, nil
	case <-time.After(30 * time.Second):
		d.pend.Delete(id)
		return nil, errors.New("CDP 命令超时")
	}
}

// Events 返回事件流。
func (d *TCPDebugger) Events() <-chan Event { return d.events }

// Detach 关闭连接并结束读取协程。
func (d *TCPDebugger) Detach() {
	if d.closed.CompareAndSwap(false, true) {
		_ = d.conn.Close()
	}
}
