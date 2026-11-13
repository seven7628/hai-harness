// ui_state.go —— `ui` 通道的桥侧缓存（PR-12）。
//
// 协议：渲染层 → 桥 `{"type":"ui","payload":{"op","data"}}`，op ∈ {snapshot, surface_status}。
// 硬约束（02-pr-plan PR-12 / 00-conventions §4.4）：
//
//	· **不进** events.Event 的 28 类 —— 它不是 agent 事件，没有 run/session 归属；
//	· **不落盘、不重放、不进会话 transcript** —— 重放一份过期快照比没有快照更糟：
//	  模型会拿旧 UI 下判断（「这个面板明明开着」），而真相早已变了；
//	· 未知 op **静默忽略**（不报错、不写日志）：滚动升级时新版渲染层会推旧桥不认识的
//	  op，那是正常现象，不该在日志里刷噪音。
//
// 为什么缓存的是**原始 JSON 字节**而不是 map[string]any：工具侧要按 what 取子对象，
// 用 json.RawMessage 可以零解析地整段返回（快照 ≤256 KiB，没必要为每个字段建索引）。
package main

import (
	"bytes"
	"encoding/json"
	"sync"
	"time"
)

// maxUISnapshotBytes 单条快照的上限：256 KiB。
//
// 快照只放**元数据**（布局/面板清单/标签 id 与标题/计数/主题/视口），几十个标签也就
// 几 KB。超出只有一种解释：渲染层把文件内容或页面 HTML 塞进来了（协议明文禁止）。
// 那时拒绝整条并保留旧值 —— 让模型读到一份被截断的巨型快照，比读到上一份正确的更糟。
const maxUISnapshotBytes = 256 << 10

// uiStaleAfter 超过这个年龄的快照标 stale（与 ui_inspect 的 stale 判定同一常量）。
//
// 渲染层 500ms debounce，正常情况下 ageMs 是三位数以内；5s 意味着渲染层卡死/断开/
// 刚启动还没推快照 —— 这时「有数据」和「数据是对的」是两件事，必须让模型看见区别。
const uiStaleAfter = 5 * time.Second

// uiSurfaceStatus 单个面板的注册/禁用状态（`surface_status` op；PR-13 的安装器读它）。
type uiSurfaceStatus struct {
	Mounted  bool
	Disabled bool
	Error    string
	At       time.Time
}

// uiState 渲染层最近一次自述的唯一缓存。
//
// 零值可用（不放指针）：manager 由多处字面量构造（含测试），要求显式初始化必然会漏一处，
// 而漏的那处表现为「工具永远说 no_snapshot」—— 是最难查的假绿形态（00 §4.7 / F53）。
//
// 刻意**不做 sync.Pool**：快照是低频（debounce 500ms）、小对象，池化只会引入
// 「借出去的切片被下一次写入改掉」这一类生命周期 bug，省不下可观测的量。
type uiState struct {
	mu   sync.Mutex
	snap []byte    // 最新快照 JSON（顶层对象），nil = 还没收到过
	at   time.Time // 收到时刻（age 的起点）
	stat map[string]uiSurfaceStatus
}

// put 写入一份快照；不合规的整条拒绝（保留旧值），返回是否接受。
//
// 判据三条，缺一不可：
//  1. 非空且 ≤ maxUISnapshotBytes；
//  2. 是合法 JSON（渲染层拼字符串拼坏了的情况真实存在）；
//  3. 顶层是**对象**（`data` 必须是对象；数组/标量过不了 ui_inspect 的按字段取值）。
func (u *uiState) put(raw []byte) bool {
	if len(raw) == 0 || len(raw) > maxUISnapshotBytes {
		return false
	}
	if !json.Valid(raw) || !isJSONObject(raw) {
		return false
	}
	// 拷一份：调用方（json.Marshal 的临时缓冲）之后可能复用/改写同一底层数组。
	snap := append([]byte(nil), raw...)
	u.mu.Lock()
	u.snap = snap
	u.at = time.Now()
	u.mu.Unlock()
	return true
}

// Snapshot 只读拷出最新快照、年龄与是否可用。
//
// 返回拷贝而不是内部切片：调用方（工具）会把它交给 json.Marshal / 引擎，
// 若共享底层数组，下一次 put 就会在读取过程中改掉内容（数据竞态）。
func (u *uiState) Snapshot() (raw []byte, age time.Duration, ok bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.snap == nil {
		return nil, 0, false
	}
	return append([]byte(nil), u.snap...), time.Since(u.at), true
}

// setStatus 记录单个面板的注册/禁用状态（`surface_status`）。
func (u *uiState) setStatus(kind string, s uiSurfaceStatus) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.stat == nil {
		u.stat = make(map[string]uiSurfaceStatus)
	}
	u.stat[kind] = s
}

// status 读单个面板的状态（PR-13 的安装器用它确认「真的挂上了」；测试也读它）。
func (u *uiState) status(kind string) (uiSurfaceStatus, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok := u.stat[kind]
	return s, ok
}

// isJSONObject 顶层是不是 JSON 对象（跳过前导空白后首字节为 '{'）。
//
// 不用「Unmarshal 到 map 再判 nil」：那会在 256 KiB 上多分配一份完整的结构体树，
// 而这里只需要回答一个问题。
func isJSONObject(raw []byte) bool {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// handleUI 处理 `{type:"ui", payload:{op,data}}`。
//
// 只有内存写入 —— 不 emitApp（那是「应用级事件」的通道，会广播给手机端）、
// 不进 checkpoint、不碰 transcript。未知 op 直接返回。
func (m *manager) handleUI(c command) {
	switch str(c.Payload, "op") {
	case "snapshot":
		data, ok := c.Payload["data"].(map[string]any)
		if !ok { // data 必须是对象（数组/字符串/缺失一律拒绝）
			return
		}
		raw, err := json.Marshal(data)
		if err != nil {
			return
		}
		m.ui.put(raw)

	case "surface_status":
		data, ok := c.Payload["data"].(map[string]any)
		if !ok {
			return
		}
		kind := str(data, "kind")
		if kind == "" {
			return
		}
		m.ui.setStatus(kind, uiSurfaceStatus{
			Mounted:  boolVal(data["mounted"]),
			Disabled: boolVal(data["disabled"]),
			Error:    str(data, "error"),
			At:       time.Now(),
		})
	}
}

// emitUI 写一条 `ui` 信封到 stdout（与事件同一条流，靠 type 区分）。
//
// 用 writeLine（立即 flush）：UI 通道是交互路径（surfaces_changed 之后渲染层要重拉
// 面板清单），攒 16ms 批量没有收益，反而让「装完没反应」多一层解释。
func (m *manager) emitUI(op string, data any) {
	if m == nil || m.out == nil {
		return
	}
	m.out.writeLine(map[string]any{"type": "ui", "op": op, "data": data})
}

// PushSurfacesChanged 通知渲染层「面板清单变了」（安装/卸载后重新注册）。
//
// PR-12 只建通道：真正的调用点在 PR-13 的安装器（applied / removed 之后）。
// 留成导出方法是刻意的 —— 它是这条通道唯一的**生产入口**，测试与 PR-13 都走它，
// 而不是各自拼一条 map（拼错了没人发现，因为没有任何东西读它）。
func (m *manager) PushSurfacesChanged() {
	m.emitUI("surfaces_changed", map[string]any{"at": time.Now().UnixMilli()})
}
