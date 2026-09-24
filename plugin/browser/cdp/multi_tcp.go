// M6：MultiClient —— 一条 JSON 行协议连接复用为多 target 的 CDP 通道。
//
// 协议 v2（Electron embed.ts 调试代理对等实现）：
//
//	命令   → {"type":"cmd","id":N,"method":"...","params":{...},"targetId":"t2"}
//	响应   → {"type":"resp","id":N,"result":{...}} | {"type":"resp","id":N,"error":"..."}
//	事件   → {"type":"event","method":"...","params":{...},"targetId":"t2"}
//	控制   → {"type":"open","rid":N,"url":"..."}      ⇒ {"type":"opened","rid":N,"targetId":"t3"}
//	       → {"type":"close","rid":N,"targetId":"t3"} ⇒ {"type":"closed","rid":N}
//	       → {"type":"enumerate","rid":N}             ⇒ {"type":"targets","rid":N,"targets":[...]}
//	推送   → {"type":"state","targetId":"t3","url":"..","title":"..","loading":bool}
//	       → {"type":"destroyed","targetId":"t3"}      // 阶段 3：单向通知「target 真的没了」
//	                                                  // （关闭；休眠不推）→ RemoveTarget
//
// id 为 client 全局唯一序列（跨 target 共用计数器），响应按 id 匹配；控制操作用独立 rid 空间。
// 事件由 readLoop 打上 targetId 归并进共享流（实现 UntaggedSource：Forwarder 不再起泵）。
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

// TargetSnapshot Electron 侧真实视图快照（enumerate 应答 / state 推送）。
type TargetSnapshot struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	Loading bool   `json:"loading"`
}

// MultiClient 单连接多 target 客户端。实现 Opener（供 Forwarder createTarget/closeTarget）。
type MultiClient struct {
	conn    net.Conn
	w       *bufio.Writer
	wmu     sync.Mutex
	pendCmd sync.Map // int64 -> chan respMsg（命令响应）
	pendCtl sync.Map // int64 -> chan map[string]any（控制应答）
	events  chan TaggedEvent
	OnState func(snap TargetSnapshot) // 可选：state 推送回调（readLoop 内调用，须非阻塞）
	// OnDestroyed 可选：Electron 侧「target 真的没了」的推送回调（阶段 3，readLoop 内调用，
	// 须非阻塞）。语义边界：只有**真关闭**（用户关标签 / AI close_page）才推它；视图池
	// LRU **休眠**（渲染进程被回收、条目与 id 仍在）不推 —— 那种情况 target 在 AI 眼里仍存在。
	OnDestroyed func(id string)
	closed      atomic.Bool
	seq         atomic.Int64 // 命令/控制共用一个单调序列（不同 type 字段天然隔离）
}

// DialMulti 包装一条已建立连接，启动读取协程。
func DialMulti(conn net.Conn) *MultiClient {
	m := &MultiClient{conn: conn, w: bufio.NewWriter(conn), events: make(chan TaggedEvent, 256)}
	go m.readLoop()
	return m
}

// Events 返回归并的带 targetId 事件流。
func (m *MultiClient) Events() <-chan TaggedEvent { return m.events }

func (m *MultiClient) readLoop() {
	sc := bufio.NewScanner(m.conn)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for sc.Scan() {
		var head struct {
			Type string `json:"type"`
			ID   int64  `json:"id"`
			RID  int64  `json:"rid"`
		}
		if err := json.Unmarshal(sc.Bytes(), &head); err != nil {
			continue
		}
		switch head.Type {
		case "resp":
			if ch, ok := m.pendCmd.LoadAndDelete(head.ID); ok {
				var body struct {
					Result map[string]any `json:"result"`
					Error  string         `json:"error"`
				}
				_ = json.Unmarshal(sc.Bytes(), &body)
				select {
				case ch.(chan respMsg) <- respMsg{Result: body.Result, Err: body.Error}:
				default:
				}
			}
		case "event":
			var body struct {
				Method   string         `json:"method"`
				Params   map[string]any `json:"params"`
				TargetID string         `json:"targetId"`
			}
			if json.Unmarshal(sc.Bytes(), &body) != nil {
				continue
			}
			// 全量事件日志（诊断链路用；所有事件都记，定位后移除）
			trace.Log("multi.event", body.Method, "target="+body.TargetID)
			select {
			case m.events <- TaggedEvent{TargetID: body.TargetID, Event: Event{Method: body.Method, Params: body.Params}}:
			default: // 背压丢弃（防御）
			}
		case "opened", "closed", "targets", "ctl-error":
			if ch, ok := m.pendCtl.LoadAndDelete(head.RID); ok {
				raw := make(map[string]any)
				_ = json.Unmarshal(sc.Bytes(), &raw)
				select {
				case ch.(chan map[string]any) <- raw:
				default:
				}
			}
		case "state":
			var body struct {
				TargetID string `json:"targetId"`
				URL      string `json:"url"`
				Title    string `json:"title"`
				Loading  bool   `json:"loading"`
			}
			if json.Unmarshal(sc.Bytes(), &body) != nil || m.OnState == nil {
				continue
			}
			m.OnState(TargetSnapshot{ID: body.TargetID, URL: body.URL, Title: body.Title, Loading: body.Loading})
		case "destroyed":
			// 阶段 3（P1）：Electron 侧「这个 target 真的没了」（用户关标签 / AI close_page）。
			// 补上这条链路之前，Electron 关视图**不通知** Go 侧 → forwarder 的页表留着幽灵页
			// （list_pages 仍列它、命令路由报 target 不存在），见 docs/RIGHT_PANEL_TABS_PLAN.md §11.1。
			var body struct {
				TargetID string `json:"targetId"`
			}
			if json.Unmarshal(sc.Bytes(), &body) != nil || body.TargetID == "" || m.OnDestroyed == nil {
				continue
			}
			m.OnDestroyed(body.TargetID)
		}
	}
	// 连接关闭：结束事件流、唤醒所有等待者（避免永久阻塞）
	m.closed.Store(true)
	close(m.events)
	m.pendCmd.Range(func(_, v any) bool {
		select {
		case v.(chan respMsg) <- respMsg{Err: "连接已关闭"}:
		default:
		}
		return true
	})
	m.pendCtl.Range(func(_, v any) bool {
		select {
		case v.(chan map[string]any) <- map[string]any{"type": "ctl-error", "error": "连接已关闭"}:
		default:
		}
		return true
	})
}

// writeLine 序列化写出（加锁串行）。
func (m *MultiClient) writeLine(v any) error {
	if m.closed.Load() {
		return errors.New("multi 连接已关闭")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	m.wmu.Lock()
	defer m.wmu.Unlock()
	if _, err := m.w.Write(append(raw, '\n')); err != nil {
		return err
	}
	return m.w.Flush()
}

// SendCommand 向指定 target 发页面级命令并等待响应。
func (m *MultiClient) SendCommand(targetID, method string, params map[string]any) (map[string]any, error) {
	trStart := time.Now()
	defer func() { trace.Phase("multi.SendCommand("+method+", target="+targetID+")", trStart) }()
	id := m.seq.Add(1)
	ch := make(chan respMsg, 1)
	m.pendCmd.Store(id, ch)
	err := m.writeLine(map[string]any{"type": "cmd", "id": id, "method": method, "params": params, "targetId": targetID})
	if err != nil {
		m.pendCmd.Delete(id)
		return nil, err
	}
	select {
	case r := <-ch:
		if r.Err != "" {
			return nil, errors.New(r.Err)
		}
		return r.Result, nil
	case <-time.After(30 * time.Second):
		m.pendCmd.Delete(id)
		return nil, errors.New("CDP 命令超时")
	}
}

// OpenTarget 实现 Opener：让 Electron 新建视图，返回 (targetId, 绑定该视图的 Debugger)。
func (m *MultiClient) OpenTarget(url string) (string, Debugger, error) {
	rid := m.seq.Add(1)
	ch := make(chan map[string]any, 1)
	m.pendCtl.Store(rid, ch)
	if err := m.writeLine(map[string]any{"type": "open", "rid": rid, "url": url}); err != nil {
		m.pendCtl.Delete(rid)
		return "", nil, err
	}
	select {
	case r := <-ch:
		if e, _ := r["error"].(string); e != "" {
			return "", nil, errors.New(e)
		}
		id, _ := r["targetId"].(string)
		if id == "" {
			return "", nil, errors.New("opened 应答缺 targetId")
		}
		return id, m.For(id), nil
	case <-time.After(30 * time.Second):
		m.pendCtl.Delete(rid)
		return "", nil, errors.New("打开新视图超时")
	}
}

// CloseTarget 实现 Opener：关闭指定视图。
func (m *MultiClient) CloseTarget(id string) error {
	rid := m.seq.Add(1)
	ch := make(chan map[string]any, 1)
	m.pendCtl.Store(rid, ch)
	if err := m.writeLine(map[string]any{"type": "close", "rid": rid, "targetId": id}); err != nil {
		m.pendCtl.Delete(rid)
		return err
	}
	select {
	case r := <-ch:
		if e, _ := r["error"].(string); e != "" {
			return errors.New(e)
		}
		return nil
	case <-time.After(15 * time.Second):
		m.pendCtl.Delete(rid)
		return errors.New("关闭视图超时")
	}
}

// Enumerate 列出 Electron 当前全部真实视图（attach 握手时绑定默认 target 用）。
func (m *MultiClient) Enumerate() ([]TargetSnapshot, error) {
	rid := m.seq.Add(1)
	ch := make(chan map[string]any, 1)
	m.pendCtl.Store(rid, ch)
	if err := m.writeLine(map[string]any{"type": "enumerate", "rid": rid}); err != nil {
		m.pendCtl.Delete(rid)
		return nil, err
	}
	select {
	case r := <-ch:
		if e, _ := r["error"].(string); e != "" {
			return nil, errors.New(e)
		}
		raw, _ := r["targets"].([]any)
		out := make([]TargetSnapshot, 0, len(raw))
		for _, it := range raw {
			o, _ := it.(map[string]any)
			id, _ := o["id"].(string)
			u, _ := o["url"].(string)
			t, _ := o["title"].(string)
			out = append(out, TargetSnapshot{ID: id, URL: u, Title: t})
		}
		return out, nil
	case <-time.After(10 * time.Second):
		m.pendCtl.Delete(rid)
		return nil, errors.New("枚举视图超时")
	}
}

// Close 关闭底层连接（各 target 的 Debugger 随之失效）。
func (m *MultiClient) Close() {
	if m.closed.CompareAndSwap(false, true) {
		_ = m.conn.Close()
	}
}

// multiDebugger 单个 target 的 Debugger 视图（共享连接 + targetId 标记）。
// 实现 UntaggedSource：事件已由归并流投递，Forwarder 不起泵。
type multiDebugger struct {
	m  *MultiClient
	id string
}

func (m *MultiClient) For(targetID string) Debugger { return &multiDebugger{m: m, id: targetID} }

func (d *multiDebugger) SendCommand(method string, params map[string]any) (map[string]any, error) {
	return d.m.SendCommand(d.id, method, params)
}

// Events 返回空通道：本实现的事件走归并流（UntaggedSource），不单独提供。
func (d *multiDebugger) Events() <-chan Event { return nil }

func (d *multiDebugger) UntaggedEvents() {}

// Detach 为 no-op：连接由 MultiClient.Close 统一释放（多个 target 共享）。
func (d *multiDebugger) Detach() {}
