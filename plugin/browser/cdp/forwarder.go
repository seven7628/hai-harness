// Package cdp 实现 M2/M6 的 CDP 多路转发器核心：把内嵌 webContents 的页面级
// CDP 会话暴露成 chrome-devtools-mcp / Puppeteer 可连接的「浏览器级」端点所需
// 的最小协议映射。
//
// M2：单 target（一个内嵌视图复用为唯一 target）。
// M6：多 target——每个真实 WebContents 视图一个 targetId；Target.createTarget 经
// Opener 控制通道让 Electron 真建视图，closeTarget 真关；flatten 会话按 target 路由；
// 页面级事件带 targetId 回推（sessionId 归属正确，pageId 路由从此为真）。
//
// 设计决策：
//   - Debugger 抽象可注入（无 Electron 也能对协议映射做纯逻辑单测）。
//   - socket/ws 层是薄壳（desktop/bridge），把浏览器级方法派发到这里。
//   - 未识别的浏览器级方法保守转发到默认 target 的页面会话。
//   - opener 为 nil 时 createTarget/closeTarget 保持 M2 兼容行为（复用/空成功），
//     既有测试与旧链路不破。
package cdp

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/plugin/browser/trace"
)

// EmbedBrowserContextID 单视图内嵌浏览器暴露给 Puppeteer 的浏览器级 context id。
// 用于 Target.getBrowserContexts / TargetInfo.browserContextId（两者必须一致，否则
// Puppeteer 以 contextId 关联 target 时会失配）。
const EmbedBrowserContextID = "go-code-embedded"

// DefaultTargetID 端点启动时的占位 target id（视图 attach 前对外可见；
// embedAttach 枚举真实视图后按序替换绑定）。
const DefaultTargetID = "go-code-embed"

// Debugger 抽象单个 target 的 CDP 会话（对应 Electron webContents.debugger，页面级）。
type Debugger interface {
	// SendCommand 发送页面级 CDP 命令，返回结果（result 对象）。
	SendCommand(method string, params map[string]any) (map[string]any, error)
	// Events 返回页面级 CDP 事件流（导航/DOM/console 等），用于回推到 ws 客户端。
	Events() <-chan Event
	// Detach 释放 CDP 会话。
	Detach()
}

// Event 一条页面级 CDP 事件。
type Event struct {
	Method string
	Params map[string]any
}

// TaggedEvent 带 targetId 的事件（多 target 事件归并流的原子单元）。
// Level 区分事件层级：page = 页面级（回推时包成 Target.receivedMessageFromTarget）；
// browser = 浏览器级（如 Target.attachedToTarget，原样回推，不包 session）。
type TaggedEvent struct {
	TargetID string
	Level    string // "page"（默认）| "browser"
	Event    Event
}

// Target 一个受控页面。
type Target struct {
	ID        string
	Type      string
	Title     string
	URL       string
	Attached  bool
	SessionID string
}

// Opener 经控制通道开/关真实 Electron 视图（bridge 注入 MultiClient 实现）。
type Opener interface {
	// OpenTarget 创建新视图并加载 url，返回 (targetId, 该视图的 Debugger)。
	OpenTarget(url string) (string, Debugger, error)
	// CloseTarget 关闭并销毁指定视图。
	CloseTarget(id string) error
}

// Forwarder 浏览器级 CDP 协议映射核心（纯逻辑，可注入 Debugger/Opener 单测）。
// 支持延迟 attach：enable 时先起端点（占位 target 无 debugger），真正使用浏览器时再绑定。
type Forwarder struct {
	mu      sync.Mutex
	order   []string                // targetId，创建顺序（首个 = 默认）
	targets map[string]*targetState // targetId -> 状态
	events  chan TaggedEvent        // 统一事件源（Attach 后由各 dbg 泵入；Relay 读这里）
	opener  Opener                  // nil = 单视图兼容模式（createTarget 复用/close no-op）
	seq     int                     // sessionId 分配序号
	closed  bool                    // Close 后不再泵事件
}

type targetState struct {
	info      *Target
	dbg       Debugger // 可为 nil（占位 / 视图未创建）
	sessionID string   // 首次 attachToTarget 分配后稳定
}

// New 构造转发器。targetID 为首个（默认）target 的 id；title/url 为初始值。
// dbg 可为 nil（端点先就绪，视图后 attach）。
func New(targetID, title, url string, dbg Debugger) *Forwarder {
	f := &Forwarder{
		targets: map[string]*targetState{},
		events:  make(chan TaggedEvent, 128),
	}
	f.targets[targetID] = &targetState{info: &Target{ID: targetID, Type: "page", Title: title, URL: url}, dbg: dbg}
	f.order = []string{targetID}
	if dbg != nil {
		go f.pump(targetID, dbg)
	}
	return f
}

// SetOpener 注入视图开/关通道（embedAttach 时调用；注入前 createTarget 走兼容路径）。
func (f *Forwarder) SetOpener(op Opener) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opener = op
}

// Debugger 返回默认 target 的 Debugger（可能为 nil：视图未创建/未绑定）。
func (f *Forwarder) Debugger() Debugger {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st := f.state(""); st != nil {
		return st.dbg
	}
	return nil
}

// HasAttachedTarget 是否已有绑定 Debugger 的真实视图（attach 完成判据）。
// 占位 target（New 时注入，dbg=nil）不算；用于消除「MCP 端点已就绪但零页面」
// 窗口期——chrome-devtools-mcp 在零页面时 McpContext 初始化不出选中页，之后
// 所有工具调用报 No page selected。
func (f *Forwarder) HasAttachedTarget() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, st := range f.targets {
		if st.dbg != nil {
			return true
		}
	}
	return false
}

// Attach 延迟绑定默认 target 的 debugger（M2 兼容入口；幂等防护同 EnsureTarget）。
func (f *Forwarder) Attach(dbg Debugger) {
	f.mu.Lock()
	id := f.defaultID()
	f.mu.Unlock()
	if id == "" {
		id = DefaultTargetID
	}
	f.EnsureTarget(id, "", "", dbg)
}

// EnsureTarget 注册/更新一个真实 target 并绑定其 Debugger（幂等：重复调用仅刷新元数据）。
// dbg 为 nil 时保留已绑定的 Debugger（仅刷新 title/url）。实现了 UntaggedSource 的 dbg
// 表示其事件由外层归并流统一投递（Forwarder 不再为其起泵）。
func (f *Forwarder) EnsureTarget(id, title, url string, dbg Debugger) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.targets[id]
	if !ok {
		st = &targetState{info: &Target{ID: id, Type: "page"}}
		f.targets[id] = st
		f.order = append(f.order, id)
	}
	if title != "" {
		st.info.Title = title
	}
	if url != "" {
		st.info.URL = url
	}
	if dbg == nil {
		return
	}
	prev := st.dbg
	st.dbg = dbg
	if _, external := dbg.(UntaggedSource); external {
		return // 外层归并投递，不起泵
	}
	if prev == nil && !f.closed {
		go f.pump(id, dbg)
	}
}

// UntaggedSource 标记 Debugger 的事件已经由外层（MultiClient 归并流）带 targetId 投递，
// Forwarder 不再为其单独起事件泵（避免重复转发其他 target 的事件）。
type UntaggedSource interface{ UntaggedEvents() }

// IngestTagged 供外层归并流（MultiClient.Events）把带 targetId 的事件注入统一事件源。
func (f *Forwarder) IngestTagged(te TaggedEvent) {
	f.mu.Lock()
	stopped := f.closed
	f.mu.Unlock()
	if stopped {
		return
	}
	select {
	case f.events <- te:
	default: // 背压：满则丢弃（防御）
	}
}

// RemoveTarget 移除 target 并广播 Target.targetDestroyed（浏览器级事件）——
// Puppeteer/MCP 据此清理页面表，避免「MCP 页 ↔ embed.ts 视图」两侧失配
// （Electron 重启后旧 target 残留时，路由命令会报 target 不存在）。
//
// 已 attach（有 sessionId）的 target 还**必须**再补发 Target.detachedFromTarget：
// chrome-devtools-mcp 的 Puppeteer CdpTargetManager 只在 targetDestroyed 里处理
// type=="service_worker"，page 类型什么都不做（index.js #onTargetDestroyed）；
// 真正触发清理链的是 detachedFromTarget → #onDetachedFromTarget → targetGone →
// target._isClosedDeferred.resolve()。而 Page.close() 会
// `await this.#tabTarget._isClosedDeferred.valueOrThrow()`——只发 destroyed 时该
// deferred 永不落地，browser_close_page 必然挂满 300s（历史运行日志实测 3/3）。
func (f *Forwarder) RemoveTarget(id string) {
	f.mu.Lock()
	st := f.targets[id]
	sid := ""
	if st != nil {
		sid = st.sessionID
	}
	delete(f.targets, id)
	for i, x := range f.order {
		if x == id {
			f.order = append(f.order[:i], f.order[i+1:]...)
			break
		}
	}
	stopped := f.closed
	f.mu.Unlock()
	if stopped || st == nil {
		return
	}
	params := map[string]any{"targetId": id}
	if sid != "" {
		params["sessionId"] = sid
	}
	select {
	case f.events <- TaggedEvent{
		TargetID: id,
		Level:    "browser",
		Event:    Event{Method: "Target.targetDestroyed", Params: params},
	}:
	default: // 背压：满则丢弃（防御）
	}
	if sid == "" {
		return // 没有会话可解绑：发 detach 只会是脏事件
	}
	// 补发 detach（见函数注释）：Puppeteer 只有收到它才会 resolve 页面的关闭 deferred。
	// 必须 Level=browser：Relay 对 browser 级事件原样透传（不带外层 sessionId），
	// 而 CdpConnection.onMessage 只在**无**外层 sessionId 时才 emit 到浏览器级
	// 监听者；带外层 sessionId 会被路由进子会话，永远到不了 #onDetachedFromTarget。
	select {
	case f.events <- TaggedEvent{
		TargetID: id,
		Level:    "browser",
		Event: Event{
			Method: "Target.detachedFromTarget",
			Params: map[string]any{"sessionId": sid, "targetId": id},
		},
	}:
	default: // 背压：满则丢弃（防御）
	}
}

// ResyncTargets 全量重同步 target 表（重 attach 时调用）：不在 snaps 中的遗留 target
// （上一 Electron 实例的视图，其 Debugger 已随旧实例失效）经 RemoveTarget 移除并广播；
// 顺序以 snaps 为准。
func (f *Forwarder) ResyncTargets(snaps []TargetSnapshot) {
	f.mu.Lock()
	keep := make(map[string]bool, len(snaps))
	for _, s := range snaps {
		keep[s.ID] = true
	}
	var stale []string
	for _, id := range f.order {
		if !keep[id] {
			stale = append(stale, id)
		}
	}
	f.mu.Unlock()
	for _, id := range stale {
		f.RemoveTarget(id)
	}
}

// pump 把某 target 的页面级事件打上 targetId 泵入统一事件流。
// dbg 事件流关闭时泵结束（共享 events 由 Close 统一关闭，避免多泵竞态 close）。
func (f *Forwarder) pump(id string, dbg Debugger) {
	for ev := range dbg.Events() {
		f.mu.Lock()
		stopped := f.closed
		f.mu.Unlock()
		if stopped {
			return
		}
		select {
		case f.events <- TaggedEvent{TargetID: id, Event: ev}:
		default: // 背压：满则丢弃（防御）
		}
	}
}

// Close 结束转发器：停泵、关统一事件流（Relay 据此退出）。
func (f *Forwarder) Close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	dbgs := make([]Debugger, 0, len(f.targets))
	for _, st := range f.targets {
		if st.dbg != nil {
			dbgs = append(dbgs, st.dbg)
		}
	}
	f.targets = map[string]*targetState{}
	f.order = nil
	f.mu.Unlock()
	for _, d := range dbgs {
		d.Detach()
	}
	close(f.events)
}

// defaultID 返回默认 target（可能为 ""：全部移除后）。
func (f *Forwarder) defaultID() string {
	if len(f.order) == 0 {
		return ""
	}
	return f.order[0]
}

// state 取指定/默认 target（锁内使用）。
func (f *Forwarder) state(id string) *targetState {
	if id == "" {
		id = f.defaultID()
	}
	return f.targets[id]
}

// TargetInfo 返回当前默认 target 信息（兼容 M2 单 target 用法）。
func (f *Forwarder) TargetInfo() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.state("")
	if st == nil {
		return map[string]any{}
	}
	return targetInfoMap(st.info)
}

func targetInfoMap(t *Target) map[string]any {
	return map[string]any{
		"targetId":         t.ID,
		"type":             t.Type,
		"title":            t.Title,
		"url":              t.URL,
		"attached":         t.Attached,
		"openerId":         "",
		"canAccessOpener":  false,
		"browserContextId": EmbedBrowserContextID,
	}
}

// Update 更新默认 target 元数据（标题/URL 变化时由调用方刷新）。
func (f *Forwarder) Update(title, url string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st := f.state(""); st != nil {
		st.info.Title = title
		st.info.URL = url
	}
}

// NotifyTargetChanged 更新指定 target 元数据并广播浏览器级 Target.targetInfoChanged
// 事件（Puppeteer target 管理器据此刷新 targetInfo——导航后不广播则 list_pages /
// goto 的 target 信息停留在旧 URL）。幂等：target 不存在时静默。
func (f *Forwarder) NotifyTargetChanged(id, title, url string) {
	f.mu.Lock()
	st := f.targets[id]
	if st == nil {
		f.mu.Unlock()
		return
	}
	if title != "" {
		st.info.Title = title
	}
	if url != "" {
		st.info.URL = url
	}
	ti := targetInfoMap(st.info)
	stopped := f.closed
	f.mu.Unlock()
	if stopped {
		return
	}
	select {
	case f.events <- TaggedEvent{
		TargetID: id,
		Level:    "browser",
		Event: Event{
			Method: "Target.targetInfoChanged",
			Params: map[string]any{"targetInfo": ti},
		},
	}:
	default: // 背压：满则丢弃（防御）
	}
}

// Handle 处理浏览器级 CDP 方法，返回 response.result 内容。
// 未识别的浏览器级方法尝试直接转发到默认 target 会话（部分工具直发）。
func (f *Forwarder) Handle(method string, params map[string]any) (map[string]any, error) {
	trStart := time.Now()
	defer func() { trace.Phase("forwarder.Handle("+method+")", trStart) }()
	switch method {
	case "Target.getTargets":
		f.mu.Lock()
		infos := make([]any, 0, len(f.order))
		for _, id := range f.order {
			st := f.targets[id]
			if st == nil || st.dbg == nil {
				continue // 过滤占位 target（无真实视图，与 emitAutoAttachEvents 一致）
			}
			infos = append(infos, targetInfoMap(st.info))
		}
		f.mu.Unlock()
		return map[string]any{"targetInfos": infos}, nil

	case "Target.getTargetInfo":
		id, _ := params["targetId"].(string)
		f.mu.Lock()
		st := f.state(id)
		var res map[string]any
		if st != nil {
			res = map[string]any{"targetInfo": targetInfoMap(st.info)}
		}
		f.mu.Unlock()
		if res == nil {
			return nil, fmt.Errorf("target %q 不存在", id)
		}
		return res, nil

	case "Target.setDiscoverTargets", "Target.setAutoAttach", "Target.setAttachToFrames",
		"Target.activateTarget", "Target.setRemoteLocations":
		// M6：flatten 模式下 Puppeteer 连接后即发 setAutoAttach，随后**等待每个已有 target
		// 的 Target.attachedToTarget 事件**来建立会话——此前 no-op 导致其永远拿不到页面
		// （"No page selected"）。此处为所有已知 target 分配 sessionId 并补发浏览器级事件。
		if method == "Target.setAutoAttach" {
			f.emitAutoAttachEvents()
		}
		return map[string]any{}, nil

	case "Target.attachToTarget":
		return f.attach(params)

	case "Target.detachFromTarget":
		f.markDetached(params)
		return map[string]any{}, nil

	case "Target.sendMessageToTarget":
		return f.handleSessionMessage(params)

	case "Target.createTarget":
		url, _ := params["url"].(string)
		return f.createTarget(url)

	case "Target.closeTarget":
		id, _ := params["targetId"].(string)
		return f.closeTarget(id)

	case "Browser.getVersion":
		return map[string]any{
			"protocolVersion": "1.3",
			"product":         "go-code-embedded",
			"revision":        "6",
			"userAgent":       "go-code-embedded",
			"jsVersion":       "go-code-embedded",
		}, nil

	case "Browser.close":
		return map[string]any{}, nil

	case "Target.getBrowserContexts":
		// 浏览器级 connect 握手的第一步（chrome-devtools-mcp / Puppeteer 连接端点时
		// 第一句即 await Target.getBrowserContexts）。必须返回默认 context（不依赖页面视图
		// attach），否则握手在视图就绪前就失败 → 浏览器连接永远建立不起来。
		return map[string]any{"browserContextIds": []any{EmbedBrowserContextID}}, nil

	case "Target.getDevToolsTarget":
		// §1.8.0 实测补丁：内嵌浏览器**没有 DevTools target**。chrome-devtools-mcp 的
		// McpPage.init → createTargetUniverse（DevTools universe）会调用
		// hasDevTools()/openDevTools() 查 DevTools target——转发到 Electron 报
		// "'Target.getDevToolsTarget' wasn't found" 后 Puppeteer 内部等待/重试
		// 约 9s（new_page 超时主因之一：goto 1s 返回 + init 9s > timeout 10s）。
		// 直接返回空 targetId（hasDevTools → false，快速跳过 DevTools 分支）。
		return map[string]any{"targetId": ""}, nil

	default:
		// 浏览器级方法未识别：转发到默认 target 会话（若未 bind 则报错）
		f.mu.Lock()
		st := f.state("")
		var dbg Debugger
		if st != nil {
			dbg = st.dbg
		}
		f.mu.Unlock()
		if dbg == nil {
			return nil, fmt.Errorf("浏览器视图尚未附加（内嵌浏览器未创建）")
		}
		return dbg.SendCommand(method, params)
	}
}

// createTarget 新开页面：有 opener → 真建视图注册新 target；无 opener → M2 兼容（复用默认）。
func (f *Forwarder) createTarget(url string) (map[string]any, error) {
	f.mu.Lock()
	op := f.opener
	def := f.defaultID()
	f.mu.Unlock()
	if op == nil {
		// M2 兼容：单视图场景复用现有 target（不开新页面）
		if def == "" {
			return nil, fmt.Errorf("无可复用 target")
		}
		return map[string]any{"targetId": def}, nil
	}
	id, dbg, err := op.OpenTarget(url)
	if err != nil {
		return nil, fmt.Errorf("打开新页面失败: %w", err)
	}
	f.EnsureTarget(id, "", url, dbg)
	// flatten auto-attach：Puppeteer 等新 target 的 attachedToTarget 事件建立页面会话，
	// 不补发则 new_page 挂到 protocolTimeout。
	// §1.8.0 实测补丁：Puppeteer CdpTargetManager 先靠 Target.targetCreated 建立 target
	// 对象，再靠 attachedToTarget 建会话——只补 attachedToTarget 时 waitForTarget 仍挂 30s。
	f.emitTargetCreated(id)
	f.emitAttachedToTarget(id)
	return map[string]any{"targetId": id}, nil
}

// closeTarget 关页：有 opener → 真关并移除；无 opener → M2 兼容（返回成功）。
func (f *Forwarder) closeTarget(id string) (map[string]any, error) {
	f.mu.Lock()
	op := f.opener
	f.mu.Unlock()
	if op == nil || id == "" {
		return map[string]any{"success": true}, nil // M2 兼容
	}
	if err := op.CloseTarget(id); err != nil {
		return nil, err
	}
	f.RemoveTarget(id)
	return map[string]any{"success": true}, nil
}

// resolveBySession 按 flatten sessionId 找 target id（找不到返回 ""）。
func (f *Forwarder) resolveBySession(sid string) string {
	for _, id := range f.order {
		if f.targets[id].sessionID == sid {
			return id
		}
	}
	return ""
}

// HandleSession 处理 flatten 模式页面级命令：Puppeteer 经单 ws 直发顶层 sessionId
// （非 Target.sendMessageToTarget 包装），按会话路由到对应 target 的 Debugger。
// 会话级握手命令本地应答（不转发）：
//   - Target.setAutoAttach 等 Target.* 开关：页面级自动附加在单层会话模型下无嵌套 target，
//     且若落入浏览器级分支会重发 attachToTarget → 死循环（1.8.0 Puppeteer 实测）；
//   - Runtime.runIfWaitingForDebugger：我们补发的 attachToTarget 恒 waitingForDebugger=false，
//     无「解除等待」语义，转发 Electron 实测挂起（30s 超时）→ 空应答即可。
func (f *Forwarder) HandleSession(sessionID, method string, params map[string]any) (map[string]any, error) {
	switch method {
	case "Target.setAutoAttach", "Target.setDiscoverTargets", "Target.setAttachToFrames",
		"Runtime.runIfWaitingForDebugger":
		return map[string]any{}, nil
	case "Target.attachToTarget":
		return f.attach(params)
	case "Target.detachFromTarget":
		f.markDetached(params)
		return map[string]any{}, nil
	}
	f.mu.Lock()
	id := f.resolveBySession(sessionID)
	if id == "" {
		f.mu.Unlock()
		return nil, fmt.Errorf("sessionId %q 不存在", sessionID)
	}
	st := f.targets[id]
	dbg := st.dbg
	// §1.8.0 实测补丁（关键）：**始终采用 Puppeteer 的 sessionId**（覆盖我们分配的）。
	// 此前仅 dbg==nil 时登记——真实 target（dbg!=nil）保留我们的 sessionId，与
	// Puppeteer 实际使用的 id 不一致 → 事件回推挂错 session → Puppeteer 丢弃
	// （表现为 lifecycle/executionContext 等事件全丢，goto/evaluate 等满超时）。
	if st.sessionID != sessionID {
		trace.Log("forwarder.sessionId.rebind", "target="+id, "old="+st.sessionID, "new="+sessionID)
		st.sessionID = sessionID
	}
	f.mu.Unlock()
	trace.Log("forwarder.session.cmd", "sid="+sessionID, "method="+method, "target="+id)
	if dbg == nil {
		return nil, fmt.Errorf("浏览器视图尚未附加（target %s）", id)
	}
	return dbg.SendCommand(method, params)
}

// attach 处理 Target.attachToTarget：按 targetId（缺省默认）分配稳定 sessionId。
// §1.8.0 实测补丁：**占位 target（dbg=nil，无真实视图）拒绝 attach**——Puppeteer 首次
// attach（无 targetId → 默认）会命中占位 go-code-embed 并分配 sessionId=1，后续命令
// 路由到占位报「尚未附加」或挂起（fill/evaluate 30s 超时根因之一）。真实视图才可 attach。
func (f *Forwarder) attach(params map[string]any) (map[string]any, error) {
	id, _ := params["targetId"].(string)
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.state(id)
	if st == nil {
		return nil, fmt.Errorf("target %q 不存在", id)
	}
	if st.dbg == nil {
		return nil, fmt.Errorf("target %q 尚未附加真实视图", id)
	}
	st.info.Attached = true
	if st.sessionID == "" {
		f.seq++
		st.sessionID = fmt.Sprintf("%x", f.seq)
	}
	trace.Log("forwarder.attach", "target="+id, "sessionId="+st.sessionID)
	return map[string]any{"sessionId": st.sessionID}, nil
}

// markDetached 处理 Target.detachFromTarget（sessionId 或 targetId 定位）。
func (f *Forwarder) markDetached(params map[string]any) {
	sid, _ := params["sessionId"].(string)
	id, _ := params["targetId"].(string)
	f.mu.Lock()
	defer f.mu.Unlock()
	if sid != "" {
		id = f.resolveBySession(sid)
	}
	if st := f.state(id); st != nil {
		st.info.Attached = false
	}
}

// handleSessionMessage 处理 Target.sendMessageToTarget（flatten 模式）：从 message
// 解析页面级命令，按 sessionId/targetId 路由到对应 target 的 Debugger，返回 {sessionId, result}。
func (f *Forwarder) handleSessionMessage(params map[string]any) (map[string]any, error) {
	raw, ok := params["message"].(string)
	if !ok {
		return nil, fmt.Errorf("Target.sendMessageToTarget: message 缺失")
	}
	var cmd struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(raw), &cmd); err != nil {
		return nil, fmt.Errorf("Target.sendMessageToTarget: message 解析失败: %w", err)
	}

	sidParam, _ := params["sessionId"].(string)
	tidParam, _ := params["targetId"].(string)

	f.mu.Lock()
	id := tidParam
	if id == "" && sidParam != "" {
		id = f.resolveBySession(sidParam)
	}
	if id == "" {
		id = f.defaultID()
	}
	st := f.targets[id]
	if st == nil {
		f.mu.Unlock()
		return nil, fmt.Errorf("target %q 不存在", id)
	}
	if sidParam != "" && st.sessionID != sidParam {
		// §1.8.0 实测补丁：始终采用上游 sessionId（与 HandleSession 一致）——事件回推
		// 必须挂 Puppeteer 实际使用的 id，否则事件被丢
		trace.Log("forwarder.sessionId.rebind", "target="+id, "old="+st.sessionID, "new="+sidParam)
		st.sessionID = sidParam
	}
	sid := st.sessionID
	dbg := st.dbg
	f.mu.Unlock()

	if dbg == nil {
		return nil, fmt.Errorf("浏览器视图尚未附加（target %s）", id)
	}
	res, err := dbg.SendCommand(cmd.Method, cmd.Params)
	if err != nil {
		return nil, err
	}
	return map[string]any{"sessionId": sid, "result": res}, nil
}

// emitAutoAttachEvents 为所有**真实** target（已绑 Debugger）补发
// Target.targetCreated + Target.attachedToTarget（浏览器级事件），满足 Puppeteer
// flatten 连接的会话建立握手。幂等：sessionID 已存在则复用。
// 顺序关键：Puppeteer CdpTargetManager 先靠 targetCreated 建立 target 对象，
// 再靠 attachedToTarget 建立页面会话——缺前者 waitForTarget 挂 30s（1.8.0 实测）。
// **过滤占位 target（dbg=nil）**：占位无真实视图，暴露给 Puppeteer 会建出
// 「无 Debugger 的会话」（sessionId "1"），后续命令路由报「尚未附加」。
func (f *Forwarder) emitAutoAttachEvents() {
	f.mu.Lock()
	ids := make([]string, 0, len(f.order))
	for _, id := range f.order {
		if st := f.targets[id]; st != nil && st.dbg != nil {
			ids = append(ids, id)
		}
	}
	f.mu.Unlock()
	for _, id := range ids {
		f.emitTargetCreated(id)
		f.emitAttachedToTarget(id)
	}
}

// emitTargetCreated 补发浏览器级 Target.targetCreated 事件（Puppeteer target 管理器
// 靠它维护 #discoveredTargetsByTargetId 并建立 target 对象）。createTarget / 重 attach
// 后必须补发，否则 Puppeteer 的 waitForTarget 永远等不到新 target → new_page 挂超时。
func (f *Forwarder) emitTargetCreated(id string) {
	f.mu.Lock()
	st := f.targets[id]
	if st == nil || st.dbg == nil {
		f.mu.Unlock()
		return // 占位 target（无真实视图）不暴露给 Puppeteer
	}
	ti := targetInfoMap(st.info)
	stopped := f.closed
	f.mu.Unlock()
	if stopped {
		return
	}
	select {
	case f.events <- TaggedEvent{
		TargetID: id,
		Level:    "browser",
		Event: Event{
			Method: "Target.targetCreated",
			Params: map[string]any{"targetInfo": ti},
		},
	}:
	default: // 背压：满则丢弃（防御）
	}
}

// emitAttachedToTarget 为单个 target 补发 Target.attachedToTarget（浏览器级事件），
// 无 sessionId 则分配稳定 id。flatten auto-attach 下 Puppeteer 靠该事件建立页面会话——
// createTarget 后必须补发，否则 new_page 挂到 protocolTimeout（1.8.0 实测 30s）。
func (f *Forwarder) emitAttachedToTarget(id string) {
	f.mu.Lock()
	st := f.targets[id]
	if st == nil {
		f.mu.Unlock()
		return
	}
	if st.sessionID == "" {
		f.seq++
		st.sessionID = fmt.Sprintf("%x", f.seq)
	}
	st.info.Attached = true
	sid := st.sessionID
	ti := targetInfoMap(st.info)
	stopped := f.closed
	f.mu.Unlock()
	if stopped {
		return
	}
	select {
	case f.events <- TaggedEvent{
		TargetID: id,
		Level:    "browser",
		Event: Event{
			Method: "Target.attachedToTarget",
			Params: map[string]any{
				"sessionId":          sid,
				"targetInfo":         ti,
				"waitingForDebugger": false,
			},
		},
	}:
	default: // 背压：满则丢弃（防御）
	}
}

// Relay 从统一事件源读取带 targetId 的页面级事件，转换成 **扁平 CDP 事件**（外层带
// sessionId，如 {"method":"Page.lifecycleEvent","params":{...},"sessionId":"5"}），
// 写入 out 通道（由 ws 层回推给客户端）。
//
// 格式依据（Puppeteer 1.8.0 源码 Connection.onMessage）：
//   - 扁平事件（外层 sessionId）→ `#sessions.get(sessionId).onMessage()` → FrameManager 收到；
//   - `Target.receivedMessageFromTarget` 嵌套格式 → `this.emit(method, params)` →
//     **无任何监听者 → 事件丢失**（实测 Electron 内嵌全部 lifecycle 事件丢失、
//     goto/evaluate 超时的最终根因；真实 Chrome 原生发扁平格式所以正常）。
//
// 浏览器级事件（Target.attachedToTarget/targetCreated 等）保持原样（无 sessionId，
// Puppeteer 对 attachedToTarget 有特判、targetCreated 走浏览器级 emit）。
func (f *Forwarder) Relay(out chan<- map[string]any) {
	for te := range f.events {
		if te.Level == "browser" {
			// 浏览器级事件（如 Target.attachedToTarget）：原样回推，不包 session
			out <- map[string]any{"method": te.Event.Method, "params": te.Event.Params}
			continue
		}
		f.mu.Lock()
		st := f.targets[te.TargetID]
		if st == nil {
			st = f.state("") // 已移除的 target 的事件挂到默认（防御）
		}
		var sid string
		if st != nil {
			sid = st.sessionID
		}
		f.mu.Unlock()
		if sid == "" {
			trace.Log("relay.drop", "target="+te.TargetID, "method="+te.Event.Method, "reason=no-session")
			continue
		}
		// §1.8.0 诊断：回推事件日志（重点看 lifecycle/executionContext 是否带对 session 发出）
		if te.Event.Method == "Page.lifecycleEvent" || te.Event.Method == "Runtime.executionContextCreated" {
			trace.Log("relay.send", "target="+te.TargetID, "sid="+sid, "method="+te.Event.Method)
		}
		// **扁平格式**：事件 method/params 直接放外层 + sessionId（Puppeteer 路由依据）
		out <- map[string]any{
			"method":    te.Event.Method,
			"params":    te.Event.Params,
			"sessionId": sid,
		}
	}
}
