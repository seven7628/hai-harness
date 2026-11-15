package main

// plugin_proxy_tool.go —— 插件工具在模型侧的唯一入口（09-agent-bridge §6.4 / §7.3 / D10）。
//
// 一条链路，三个不动点：
//
//	模型调用 plugin__<id>__<name>
//	  → 本文件（注册 waiter，持 bs.dispatchMu 下发 tool.call）
//	  → 插件 onEvent('tool.call') 处理后 host.command('tool.result')
//	  → surface_agent.go 的 handleSurfaceAgentToolResult（resolve waiter）
//	  → 结果文本回到模型（失败走 error，引擎标 IsError，模型可见）
//
// 为什么代理工具**不做**成「把参数转给插件再等 callback」的通用 RPC：tool.call 走的是
// 已有的事件信封（surface_id/run_id/seq），复用它可以白拿三件事 —— 按 surface 定向投递、
// 终态停投、以及「同一个 run 内事件有序」。另造一条通道就得把这三件事再做一遍。
//
// D10（只在插件自己的虚拟 Project 内注册）由注册点保证：唯一调用方是
// plugin_tools.go 的 buildPluginEngine —— 项目 / Hub 引擎没有这条路径。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/tools"
)

const (
	// pluginToolNamePrefix 模型侧名字前缀：plugin__<surfaceID>__<toolName>（§5.3）。
	pluginToolNamePrefix = "plugin__"

	// pluginToolWaiterGrace 等待窗口比 manifest 声明的 timeout 多出的余量。
	//
	// 为什么需要：waiter 与引擎的兜底计时器是两把表。若两者同点到期，引擎可能先杀
	// （模型看到的是「工具被引擎中止」而不是「插件超时」），而 §6.4 要求超时是可编程
	// 的 `timeout`。多给 2s 保证 waiter **先**返回，引擎那一路永远轮不到。
	pluginToolWaiterGrace = 2 * time.Second

	// maxPluginToolResultBytes 单次工具结果上限（§8.3：工具结果 ≤64 KiB）。
	maxPluginToolResultBytes = 64 << 10
	// pluginToolTruncatedMarker 超限追加的标记。**必须**出现：静默截断会让模型把
	// 半份 JSON 当完整结果用（那是最难查的一类错答）。
	pluginToolTruncatedMarker = "\n…[truncated: 插件工具结果超过 64 KiB 上限，已截断]"
)

// pluginToolName 模型侧工具名（§5.3）。工具名已在安装期判过 pattern（不含 `__`），
// 所以这里拼出来的名字按 `__` 反解是唯一的。
func pluginToolName(surfaceID, tool string) string {
	return pluginToolNamePrefix + surfaceID + "__" + tool
}

// surfaceToolCall 一次等待中的插件工具调用（hub 等待表的元素）。
//
// 它同时是**发送载荷的来源**（eventData）与**归属判据**（surfaceID/runID）：结果回来
// 时靠 surfaceID 拒跨插件、靠 runID 在终态时批量收尸。
type surfaceToolCall struct {
	callID        string
	surfaceID     string
	runID         string
	sessionID     string
	tool          string // 插件本地工具名（不带 plugin__ 前缀）
	readOnly      bool
	requiresPanel bool
	deadlineMS    int
	args          json.RawMessage
	// ch 容量 1：发送方（resolve / failToolCalls）在 hub 锁内完成「摘表 + 投递」，
	// 所以投递永远不阻塞，而 Call 侧「摘不到表项 ⇒ channel 里一定有值」成立。
	ch chan surfaceToolCallOutcome
}

// surfaceToolCallOutcome 一次调用的结局（成功文本 / 失败原因，reason 用封闭集合）。
type surfaceToolCallOutcome struct {
	ok     bool
	text   string
	err    string
	reason string
}

// eventData tool.call 的 data（§6.6 逐字形状 + P2a 的两个声明位）。
func (c *surfaceToolCall) eventData() map[string]any {
	return map[string]any{
		"call_id":        c.callID,
		"tool":           c.tool,
		"args":           c.args,
		"deadline_ms":    c.deadlineMS,
		"read_only":      c.readOnly,
		"requires_panel": c.requiresPanel,
	}
}

// surfaceToolCallSeq 进程内单调序号（与纳秒拼成 call_id，两次同纳秒也不会撞）。
var surfaceToolCallSeq atomic.Int64

// surfaceToolCallID 生成 call_id：`tc-` 前缀让它在一眼扫日志时区别于 run/session id。
func surfaceToolCallID() string {
	n := surfaceToolCallSeq.Add(1)
	return fmt.Sprintf("tc-%d-%d", time.Now().UnixNano(), n)
}

// ---- hub：等待表 ----
//
// 表与 runs/active/recent 同一个 hub、同一把锁：工具的归属判定（run 是否还活着）
// 与「结果能不能 resolve」必须看同一份状态，两个锁就一定会有一个「已 resolve 但
// run 已经收尸」的窗口。

// toolResultStatus resolve 的三种结局（调用方据此选回执与审计）。
type toolResultStatus int

const (
	toolResultNoCall   toolResultStatus = iota // 没有这个 call_id（幂等回执，无副作用）
	toolResultMismatch                         // call_id 存在但属于别的插件（denied）
	toolResultResolved                         // 已投递给等待者
)

// addToolCall 登记一个等待者（懒建表：manager 是字面量构造的）。
func (h *surfaceAgentHub) addToolCall(c *surfaceToolCall) {
	if h == nil || c == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.toolCalls == nil {
		h.toolCalls = map[string]*surfaceToolCall{}
	}
	h.toolCalls[c.callID] = c
}

// dropToolCall 摘掉等待者；返回 false = 表里已经没有它（结果已投递或已被批量失败）。
func (h *surfaceAgentHub) dropToolCall(callID string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.toolCalls[callID]; !ok {
		return false
	}
	delete(h.toolCalls, callID)
	return true
}

// pendingToolCalls 当前等待中的调用数（诊断 / 测试断言「不泄漏」）。
func (h *surfaceAgentHub) pendingToolCalls() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.toolCalls)
}

// resolveToolResult tool.result 的唯一入口：摘表 + 投递在**同一临界区**完成。
//
// 为什么投递要在锁内：Call 侧的超时/取消分支用「摘不到表项 ⇒ 结果已在 channel 里」
// 判定「这次是我的超时，还是结果刚刚到」。中途插一个锁外投递，这个判据就会有一个
// 「摘不到但 channel 还空」的窗口 —— 表现为偶发的 timeout（结果其实回来了）。
func (h *surfaceAgentHub) resolveToolResult(callID, surfaceID string, out surfaceToolCallOutcome) (toolResultStatus, *surfaceToolCall) {
	h.mu.Lock()
	defer h.mu.Unlock()
	call, ok := h.toolCalls[callID]
	if !ok {
		return toolResultNoCall, nil
	}
	if call.surfaceID != surfaceID {
		// 越权探测：**不动**这个 waiter（真正的插件还能回），只把这次回执拒掉。
		return toolResultMismatch, call
	}
	delete(h.toolCalls, callID)
	if call.ch != nil {
		select {
		case call.ch <- out:
		default: // 容量 1 且只有一个发送方：理论上不可达，防死锁兜底
		}
	}
	return toolResultResolved, call
}

// failToolCalls 批量失败匹配的等待者（run 终态 / 插件卸载 / 会话关闭）。
//
// match 为 nil = 全部。返回被唤醒的条数（诊断日志用）。同样的「锁内摘表 + 投递」纪律。
func (h *surfaceAgentHub) failToolCalls(match func(*surfaceToolCall) bool, out surfaceToolCallOutcome) int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for id, call := range h.toolCalls {
		if match != nil && !match(call) {
			continue
		}
		delete(h.toolCalls, id)
		if call.ch != nil {
			select {
			case call.ch <- out:
			default:
			}
		}
		n++
	}
	return n
}

// claimOutcome 在 ctx/超时先到时判定这次调用还有没有救：
//   - 摘到表项 = 还在等 → 真超时/真取消，调用方按失败收尾；
//   - 摘不到   = 结果已投递（锁内摘表+投递的不变式）→ 以结果为准，不能丢。
func (h *surfaceAgentHub) claimOutcome(callID string, ch chan surfaceToolCallOutcome) (surfaceToolCallOutcome, bool) {
	if h.dropToolCall(callID) {
		return surfaceToolCallOutcome{}, false
	}
	select {
	case out := <-ch:
		return out, true
	default:
		return surfaceToolCallOutcome{}, false
	}
}

// ---- 代理工具 ----

// pluginProxyTool 一个 manifest.tools[] 声明在模型侧的代理（§7.3）。
//
// 实例按 (插件, 会话, 声明) 构造：一个会话一个引擎，所以「这条调用属于哪个 run」
// 不需要模型侧传参 —— 它只能落在本会话当前的活动 run 上。
type pluginProxyTool struct {
	m         *manager
	home      string // 插件虚拟 Project 的 home（m.get 的第一段）
	sid       string // 本会话 id（═ run 的 session_id）
	surfaceID string
	decl      surfaceToolDecl
	name      string
}

func newPluginProxyTool(m *manager, home, sid, surfaceID string, decl surfaceToolDecl) *pluginProxyTool {
	return &pluginProxyTool{
		m: m, home: home, sid: sid, surfaceID: surfaceID, decl: decl,
		name: pluginToolName(surfaceID, decl.Name),
	}
}

func (t *pluginProxyTool) Name() string { return t.name }

// Description 含**插件 id 与声明原文**：审批卡片上那行「某个插件 · 某工具」只能来自
// 这里（§6.4「卡片显示插件名 · 工具名」，插件不能代点）。前缀是固定的，模型也能据此
// 判断「这条工具不在本机，去问插件」。
func (t *pluginProxyTool) Description() string {
	return fmt.Sprintf("%s（插件 %s 提供的工具：调用会下发到该插件的面板执行）", strings.TrimSpace(t.decl.Description), t.surfaceID)
}

// Parameters 原样返回 manifest 的 input schema（安装期已判过必须是 type=object）。
func (t *pluginProxyTool) Parameters() any { return t.decl.Input }

// CanParallel V1 一次一个：结果通道按 (会话, run) 关联，并行只会让「哪条结果对应哪次
// 调用」依赖 call_id 之外的顺序假设 —— 收益为零，复杂度为正。
func (t *pluginProxyTool) CanParallel() bool { return false }

// ReadOnly 声明透传：plan 模式的可见性判定读它（§6.4 的 read_only）。
func (t *pluginProxyTool) ReadOnly() bool { return t.decl.ReadOnly }

// RequiresApproval 非只读的插件工具走现有审批（宿主 UI 不改）。
func (t *pluginProxyTool) RequiresApproval(context.Context, core.ToolCall) bool {
	return !t.decl.ReadOnly
}

// MandatoryApproval 插件写工具是第三方代码的不可逆副作用：Auto / Full Access
// 也不能把它静默放行。只读工具返回 false（不弹卡）。
func (t *pluginProxyTool) MandatoryApproval(context.Context, core.ToolCall) bool {
	return !t.decl.ReadOnly
}

// ToolTimeout 引擎侧兜底：比 waiter 的 deadline 多 2s（见 pluginToolWaiterGrace）。
func (t *pluginProxyTool) ToolTimeout() time.Duration {
	return t.decl.timeout() + pluginToolWaiterGrace
}

// BeforeCall / AfterCall 不做拦截：审批走 RequiresApproval（§6.4「走现有工具审批」），
// 这里再拦一层就等于给插件工具单独造一套 HITL 语义。
func (t *pluginProxyTool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}

func (t *pluginProxyTool) AfterCall(context.Context, core.ToolCall) {}

// ValidParams 只判「能不能当参数对象用」：schema 的语义校验归插件，宿主多判一层只会
// 把合法调用判死（P2 不做 JSON Schema 校验）。
func (t *pluginProxyTool) ValidParams(_ context.Context, _ string, arguments string) error {
	_, err := pluginToolArgs(arguments)
	return err
}

// pluginToolArgs 把模型给的参数串规整成 JSON object（空串按 `{}`）。
func pluginToolArgs(arguments string) (json.RawMessage, error) {
	raw := strings.TrimSpace(arguments)
	if raw == "" {
		raw = "{}"
	}
	if raw[0] != '{' {
		return nil, fmt.Errorf("参数必须是 JSON object（实际以 %q 开头）", raw[0])
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return nil, fmt.Errorf("参数不是合法 JSON object：%v", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(raw)); err != nil {
		return nil, fmt.Errorf("参数不是合法 JSON：%v", err)
	}
	return json.RawMessage(compact.Bytes()), nil
}

// Call 下发 tool.call 并等待插件回 tool.result。
//
// 五步，顺序不能换：
//  1. 找会话与**活动 run**（没有就明确报错，不 panic、不静默成功）；
//  2. 先登记 waiter —— 反过来的话，插件的回执可能比登记更早到，结果是
//     「no_such_call」+ 模型等满 deadline；
//  3. 持 bs.dispatchMu 发事件（与 emit 同序：插件看到的事件顺序 = 会话事件顺序）；
//  4. 等 result / ctx / deadline，三条路都先把 waiter 摘掉（不泄漏）；
//  5. 结果文本回模型，失败用 error（引擎标 IsError）。
func (t *pluginProxyTool) Call(ctx context.Context, _ string, arguments string) (string, error) {
	args, err := pluginToolArgs(arguments)
	if err != nil {
		return "", err
	}
	bs := t.m.get(t.home, t.sid)
	if bs == nil {
		return "", fmt.Errorf("插件工具 %s 不可用：会话不在运行态", t.name)
	}
	run := bs.surfaceAgentRun.Load()
	if run == nil || run.surfaceID != t.surfaceID || run.terminal() {
		// 「没有活动 run」是插件侧最常见的误用（面板里手动点、run 已结束）：
		// 文案要能直接指向原因，否则模型会反复重试同一条工具。
		return "", fmt.Errorf("插件工具 %s 不可用：no active plugin run（插件的 agent.run 已结束或尚未开始）", t.name)
	}

	call := &surfaceToolCall{
		callID:        surfaceToolCallID(),
		surfaceID:     t.surfaceID,
		runID:         run.id,
		sessionID:     t.sid,
		tool:          t.decl.Name,
		readOnly:      t.decl.ReadOnly,
		requiresPanel: t.decl.RequiresPanel,
		deadlineMS:    int(t.decl.timeout() / time.Millisecond),
		args:          args,
		ch:            make(chan surfaceToolCallOutcome, 1),
	}
	hub := t.m.surfaceAgentHub()
	hub.addToolCall(call)

	// 发送与 run 的终态在同一条锁序上（dispatchMu → r.mu）：emit / abort 都先拿
	// dispatchMu，所以这里判到「未 closed」之后 send 一定投得出去。
	bs.dispatchMu.Lock()
	if run.terminal() {
		bs.dispatchMu.Unlock()
		hub.dropToolCall(call.callID)
		t.audit(run, false, 0, surfaceReasonCancelled)
		return "", fmt.Errorf("插件工具 %s 被取消：run 已结束", t.name)
	}
	run.send(surfaceToolCallEvent, call.eventData())
	bs.dispatchMu.Unlock()

	wait := t.decl.timeout()
	timer := time.NewTimer(wait)
	defer timer.Stop()

	var out surfaceToolCallOutcome
	var failReason string
	select {
	case out = <-call.ch:
	case <-ctx.Done():
		var arrived bool
		if out, arrived = hub.claimOutcome(call.callID, call.ch); !arrived {
			failReason = surfaceReasonCancelled
		}
	case <-timer.C:
		var arrived bool
		if out, arrived = hub.claimOutcome(call.callID, call.ch); !arrived {
			failReason = surfaceReasonTimeout
		}
	}

	switch {
	case failReason == surfaceReasonTimeout:
		t.audit(run, false, 0, surfaceReasonTimeout)
		return "", fmt.Errorf("插件工具 %s 超时（%s）：插件没有回 tool.result", t.name, wait)
	case failReason != "":
		t.audit(run, false, 0, surfaceReasonCancelled)
		return "", fmt.Errorf("插件工具 %s 被取消（run 结束或会话关闭）：%v", t.name, ctx.Err())
	}
	if out.ok {
		t.audit(run, true, len(out.text), "")
		return out.text, nil
	}
	reason := out.reason
	if reason == "" {
		reason = surfaceReasonToolError
	}
	t.audit(run, false, 0, reason)
	msg := strings.TrimSpace(out.err)
	if msg == "" {
		msg = "插件工具执行失败（插件没给 error 文本）"
	}
	return "", errors.New(msg)
}

// audit 记一行 tool_call 审计（§8.5：{id, action, ts, ok, session?, run?, tool?, bytes?, reason?}）。
//
// 由**模型侧**在调用收尾时写：这一行回答的是「模型调过什么、结果多大、为什么没成」。
// 插件回执那一侧（tool.result）只在异常时另记一行 —— 两边都记成功会让人以为调了两次。
func (t *pluginProxyTool) audit(run *surfaceAgentRun, ok bool, bytes int, reason string) {
	extra := map[string]any{"session": t.sid, "tool": t.decl.Name, "bytes": bytes}
	if run != nil {
		extra["run"] = run.id
	}
	t.m.auditSurfaceAgent(t.surfaceID, "tool_call", ok, extra, reason)
}

// ---- 注册点（D10）----

// registerPluginProxyTools 把本插件声明的工具注册进**这一会话**的引擎。
//
// 四道条件全都要满足（缺一不可，顺序 = 从便宜到贵）：
//  1. 引擎非空；
//  2. runtime 是插件虚拟 Project 且 pluginID 非空（项目 / Hub 永远走不到这里）；
//  3. 插件的 index 条目 enabled（**已卸载/停用**插件的历史会话照旧能装配，但拿不到工具）；
//  4. surface.json 读得出来（读不出来 = 无法证明它声明了什么，fail closed）。
//
// 返回注册成功的模型侧名字（诊断 / 测试用）。
func (m *manager) registerPluginProxyTools(eng *tools.Engine, ws *workspaceRuntime, sid string) []string {
	if eng == nil || ws == nil || ws.kind != runtimePlugin || ws.pluginID == "" {
		return nil
	}
	if !m.surfaces.Enabled(ws.pluginID) {
		return nil
	}
	mf, err := m.surfaces.Manifest(ws.pluginID)
	if err != nil || len(mf.Tools) == 0 {
		return nil
	}
	names := make([]string, 0, len(mf.Tools))
	for _, decl := range mf.Tools {
		if strings.TrimSpace(decl.Name) == "" {
			continue // 安装期已判死，这里只防手写的 surface.json
		}
		t := newPluginProxyTool(m, ws.path, sid, ws.pluginID, decl)
		eng.RegisterTool(context.Background(), t)
		names = append(names, t.Name())
	}
	return names
}

// pluginToolResultText 把 tool.result 的 result 值变成给模型的文本（§6.4）。
//
// 字符串原样（插件最常回自然语言），其它 JSON 值序列化（结构化结果）。
// 超 64 KiB 截断并追加标记：静默截断会让模型把半份 JSON 当完整结果用。
func pluginToolResultText(v any) string {
	if v == nil {
		return ""
	}
	var text string
	if s, ok := v.(string); ok {
		text = s
	} else if raw, err := json.Marshal(v); err == nil {
		text = string(raw)
	} else {
		text = fmt.Sprint(v)
	}
	if len(text) <= maxPluginToolResultBytes {
		return text
	}
	cut := maxPluginToolResultBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut-- // 不切坏 UTF-8：回退到字符边界（半截汉字的 JSON 送给模型等于噪声）
	}
	return text[:cut] + pluginToolTruncatedMarker
}
