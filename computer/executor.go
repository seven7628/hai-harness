package computer

// Executor 决策中枢：把模型给的「文本目标」翻译成 Backend 调用，并在动作后
// 做验证闭环，最终输出模型可见的文本结果/纠错。
//
// 设计原则（COMPUTER_USE_DESIGN.md §3.1/§6.2/§6.3）：
//   - 模型只给目标（uid / 应用名 / 菜单路径 / 文本），不给坐标；
//   - 坐标永远由 executor 从元素框换算，模型不手算；
//   - 每个动作工具内部统一收尾：动作前锚定状态 → 执行 → 短稳定等待 →
//     Verify 断言（失败自动重试一次）→ 结构化文本结果/错误；
//   - 平台细节全在 Backend（Swift helper）；Executor 只依赖 Backend 接口。
//
// Executor 无状态跨调用？——否：它持有「最近一次快照 uid → 元素」映射
// （§6.1 uid 稳定策略）与 per-App 审批记忆、缩放账本（P1）等会话级状态。
// 但注意：Executor 不持有 Backend 生命周期（由装配方创建/关闭），只消费。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Executor 桌面动作决策中枢。非并发安全（会话内动作全局串行，§6.7）。
type Executor struct {
	backend Backend

	// lastSnapshot 最近一次快照的树（uid 索引），供 press/find 引用。
	mu          sync.Mutex
	lastTree    *Element
	lastApp     string
	lastPid     int
	lastWindows []Window

	// appApproved 已获用户授权的 per-App（统一授权：设置页维护，永久记录；
	// 未授权 app 的 computer_open_app/activate 拒绝并提示去设置添加）。
	// 运行时判定 + ExecutorConfig.ApprovedApps 注入（bridge 读 settings.json）。
	appApproved map[string]bool

	// config
	verifyPollMs   int                    // Verify 轮询间隔（默认 200）
	verifyWaitMs   int                    // 动作后默认验证时长（默认 2000）
	stabilizeWait  time.Duration          // 动作后短稳定等待（默认 200ms）
	reloadApproved func() map[string]bool // 授权懒刷新回调（nil = 不刷新）
}

// ExecutorConfig 构造参数。
type ExecutorConfig struct {
	// VerifyPollInterval Verify 轮询间隔（默认 200ms）。
	VerifyPollInterval time.Duration
	// VerifyDefaultTimeout 动作后默认验证超时（默认 2s）。
	VerifyDefaultTimeout time.Duration
	// StabilizeDelay 动作后短稳定等待（默认 200ms；0 可关）。
	StabilizeDelay time.Duration
	// ApprovedApps 已授权可操作的 App 集合（统一授权：设置页维护，桥接层从
	// settings.json permissions.computer_approved_apps 读入注入）。nil = 全部放行
	// （库默认，便于单测/直跑；产品装配必须显式注入）。
	ApprovedApps map[string]bool
	// ReloadApproved 授权懒刷新回调（可选）：门禁未命中时调用一次重读最新集合再判——
	// 覆盖「用户刚在设置页加授权、进程内快照旧」的场景（UI bridge 长时间运行不重启）。
	// 实现方（bridge）读 settings.json；nil = 不懒刷新（仅依赖 SetApprovedApps 热更新）。
	ReloadApproved func() map[string]bool
}

// NewExecutor 构造 Executor。
func NewExecutor(backend Backend, cfg ExecutorConfig) *Executor {
	e := &Executor{
		backend:        backend,
		appApproved:    cfg.ApprovedApps, // nil = 全部放行（库默认）；产品注入已授权集合
		verifyPollMs:   200,
		verifyWaitMs:   2000,
		reloadApproved: cfg.ReloadApproved,
	}
	if cfg.VerifyPollInterval > 0 {
		e.verifyPollMs = int(cfg.VerifyPollInterval / time.Millisecond)
	}
	if cfg.VerifyDefaultTimeout > 0 {
		e.verifyWaitMs = int(cfg.VerifyDefaultTimeout / time.Millisecond)
	}
	if cfg.StabilizeDelay > 0 {
		e.stabilizeWait = cfg.StabilizeDelay
	} else if cfg.StabilizeDelay < 0 {
		e.stabilizeWait = 0
	} else {
		e.stabilizeWait = 200 * time.Millisecond
	}
	return e
}

// ---- 快照与 uid 解析 ----

// Snapshot 取快照并缓存树/uid 映射。返回面向模型的文本树。
// opts.MaxChars 控制文本预算（≤0 用默认 6000）。
// app 非空 = 目标 app 查看语义（工具参数 Filter by app name）：
// 只读工具不切前台（plan 模式可见、不抢用户焦点）——前台已是该 app 则正常
// 渲染；前台是别的 app 则在首行如实标注「目标 app 未在前台」，引导模型先
// computer_open_app / computer_activate（2026-09 真机教训：模型带 app 参数
// snapshot 期望拿该 app 界面，实际拿到前台树 → 信息不对且不自知）。
func (e *Executor) Snapshot(ctx context.Context, app string, maxChars int) (string, error) {
	if maxChars <= 0 {
		maxChars = 6000
	}
	sn, err := e.backend.Snapshot(SnapshotOpts{MaxDepth: 9})
	if err != nil {
		return e.textErr("snapshot", err)
	}
	e.mu.Lock()
	e.lastTree = sn.Tree
	e.lastApp = sn.FrontmostApp
	e.lastPid = sn.Pid
	e.lastWindows = sn.Windows
	e.mu.Unlock()
	return e.renderSnapshot(sn, app, maxChars), nil
}

// renderSnapshot 把 Snapshot 渲染成模型可读文本树（§6.1 格式）。
// 大树处理：完整树可能超字符预算——渲染时把「交互元素（有 action/focusable）」
// 前置为摘要清单，再附树（截断），保证模型总能看到可操作目标与 uid。
// 摘要行数也按预算限幅（interactiveMax），超出部分引导 computer_find 增量查
// （真机教训 2026-09：WPS 首页交互元素 >40 时旧逻辑整段丢弃摘要 → 模型只能
// 看到被截断的树 → 拿不到可操作目标；摘要必须始终输出，宁缺树不缺目标）。
const interactiveMax = 60

func (e *Executor) renderSnapshot(sn *Snapshot, appFilter string, maxChars int) string {
	var b strings.Builder
	// 首行标注：前台应用 + 请求的 app 是否一致（不一致 = 未激活/未运行，防模型
	// 把「当前看到的 app」误当「目标 app」——旧格式只有一行 App: X [frontmost]，
	// 模型无法区分快照是哪个 app 的）。
	if appFilter != "" && sn.FrontmostApp != "" && !appsMatch(sn.FrontmostApp, appFilter) {
		b.WriteString(fmt.Sprintf("App: %s (PID %d) [frontmost; NOT the requested %q — requested app is not frontmost/not running; activate or open it first]\n", sn.FrontmostApp, sn.Pid, appFilter))
	} else {
		b.WriteString(fmt.Sprintf("App: %s (PID %d) [frontmost]\n", sn.FrontmostApp, sn.Pid))
	}
	for i, w := range sn.Windows {
		if i > 4 {
			b.WriteString(fmt.Sprintf("… %d windows total\n", len(sn.Windows)))
			break
		}
		b.WriteString(fmt.Sprintf("Window %d: %s (x:%.0f y:%.0f w:%.0f h:%.0f)\n",
			i+1, w.Title, w.Frame.X, w.Frame.Y, w.Frame.W, w.Frame.H))
	}
	// 交互摘要（对话框/弹窗/按钮优先可见）：始终输出，行数限幅 + 超出引导 find。
	// 排序（2026-09 真机教训：WPS 首页摘要前 60 被 Apple 菜单「最近使用的项目」
	// 历史列表占满，真正窗口控件被挤到 "N more"）——窗口内容控件优先于菜单栏
	// 静态项；同优先级按树序稳定。
	if sn.Tree != nil {
		interactive := collectInteractive(sn.Tree)
		if len(interactive) > 0 {
			b.WriteString("— actionable targets —\n")
			shown := 0
			for _, el := range interactive {
				if shown >= interactiveMax {
					b.WriteString(fmt.Sprintf("… %d more actionable targets (run computer_find with text/role to search)\n", len(interactive)-shown))
					break
				}
				line := fmt.Sprintf("%s [%s", el.UID, el.Role)
				if el.Title != "" {
					line += " \"" + el.Title + "\""
				}
				if el.Value != "" {
					line += " =" + el.Value
				}
				if el.Description != "" && el.Description != el.Title && el.Description != el.Value {
					line += " ‹" + el.Description + "›"
				}
				if el.Selected {
					line += " [selected]"
				}
				if el.Focused && (el.Role == "AXTextField" || el.Role == "AXTextArea" ||
					el.Role == "AXButton" || el.Role == "AXCheckBox" || el.Role == "AXMenuItem") {
					line += " [focused]"
				}
				line += "]"
				if el.Frame.W > 0 || el.Frame.H > 0 {
					line += fmt.Sprintf(" (x:%.0f y:%.0f w:%.0f h:%.0f)", el.Frame.X, el.Frame.Y, el.Frame.W, el.Frame.H)
				}
				b.WriteString(line + "\n")
				shown++
			}
		}
	}
	// 完整树（截断保护；交互摘要已保证可操作目标可见）
	if sn.Tree != nil {
		e.renderElement(&b, sn.Tree, 0)
	}
	out := b.String()
	if len(out) > maxChars {
		// 截断保留头部与交互摘要（uid 上下文）。按行边界截（不劈行/不劈 UTF-8
		// 多字节字符——旧实现 out[:maxChars] 字节硬切会把中文劈出非法字节）。
		cut := maxChars
		for cut > 0 && out[cut-1] != '\n' {
			cut--
		}
		if cut == 0 {
			// 预算小到连第一行都放不下：退化为按行取第一行
			if nl := strings.IndexByte(out, '\n'); nl > 0 {
				cut = nl + 1
			} else {
				cut = len(out)
			}
		}
		out = out[:cut] + "\n… (tree truncated; actionable targets are in the summary above; use computer_find for incremental search)"
	}
	return out
}

// collectInteractive 收集树中所有可交互元素并排序：窗口内容控件优先、菜单栏
// 子树（AXMenuBar/AXMenu/AXMenuItem——含系统「最近使用的项目」等静态历史列表）
// 降权排后。稳定排序：同级内保持树序（摘要展示顺序稳定，模型看到的 uid 顺序
// 与树一致可预期）。真机教训 2026-09：WPS 首页 >200 交互项里菜单历史占多数，
// 不降权则摘要前 60 全被菜单项占据，窗口控件不可见。
func collectInteractive(root *Element) []*Element {
	type ranked struct {
		el   *Element
		menu bool // 处于菜单栏/菜单子树
	}
	var out []ranked
	var walk func(*Element, bool)
	walk = func(el *Element, inMenu bool) {
		if el == nil {
			return
		}
		menu := inMenu || el.Role == "AXMenuBar" || el.Role == "AXMenu" ||
			el.Role == "AXMenuItem" || el.Role == "AXMenuBarItem"
		if len(el.Actions) > 0 || el.Focusable || el.Role == "AXButton" ||
			el.Role == "AXCheckBox" || el.Role == "AXTextField" || el.Role == "AXMenuItem" ||
			el.Role == "AXPopUpButton" || el.Role == "AXComboBox" || el.Role == "AXLink" ||
			el.Role == "AXRadioButton" || el.Role == "AXDisclosureTriangle" || el.Role == "AXRow" ||
			el.Description != "" {
			out = append(out, ranked{el: el, menu: menu})
		}
		for _, c := range el.Children {
			walk(c, menu)
		}
	}
	walk(root, false)
	// 稳定排序：非菜单项在前（保持树序），菜单项在后
	stable := make([]*Element, 0, len(out))
	for _, r := range out {
		if !r.menu {
			stable = append(stable, r.el)
		}
	}
	for _, r := range out {
		if r.menu {
			stable = append(stable, r.el)
		}
	}
	return stable
}

func (e *Executor) renderElement(b *strings.Builder, el *Element, depth int) {
	if el == nil {
		return
	}
	indent := strings.Repeat("  ", depth)
	// 行：uid 角色 标题 值 动作
	line := fmt.Sprintf("%s%s [%s", indent, el.UID, el.Role)
	if el.Title != "" {
		line += " \"" + el.Title + "\""
	}
	if el.Value != "" {
		line += " =" + el.Value
	}
	if el.Description != "" && el.Description != el.Title && el.Description != el.Value {
		line += " ‹" + el.Description + "›"
	}
	if len(el.Actions) > 0 {
		line += " ⚡" + strings.Join(el.Actions, ",")
	}
	if el.Selected {
		line += " [selected]"
	}
	if el.Focused && (el.Role == "AXTextField" || el.Role == "AXTextArea" ||
		el.Role == "AXButton" || el.Role == "AXCheckBox" || el.Role == "AXMenuItem" ||
		el.Role == "AXRadioButton" || el.Role == "AXComboBox" || el.Role == "AXSearchField") {
		line += " [focused]"
	}
	line += "]"
	if el.Frame.W > 0 || el.Frame.H > 0 {
		line += fmt.Sprintf(" (x:%.0f y:%.0f w:%.0f h:%.0f)", el.Frame.X, el.Frame.Y, el.Frame.W, el.Frame.H)
	}
	b.WriteString(line + "\n")
	for _, c := range el.Children {
		e.renderElement(b, c, depth+1)
	}
}

// Find 增量目标检索：先查缓存树，未命中再走 backend.Find。
func (e *Executor) Find(ctx context.Context, text, role, app string) (string, error) {
	e.mu.Lock()
	tree := e.lastTree
	e.mu.Unlock()
	var hits []*Element
	if tree != nil {
		hits = collectMatches(tree, text, role)
	}
	if len(hits) == 0 {
		els, err := e.backend.Find(FindQuery{Text: text, Role: role, App: app})
		if err != nil {
			return e.textErr("find", err)
		}
		for i := range els {
			hits = append(hits, &els[i])
		}
	}
	if len(hits) == 0 {
		return "No matching element found (text=" + text + "). Run computer_snapshot first to view the current UI.", nil
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Found %d matches:\n", len(hits)))
	for i, el := range hits {
		if i >= 20 {
			break
		}
		b.WriteString(fmt.Sprintf("%s [%s", el.UID, el.Role))
		if el.Title != "" {
			b.WriteString(" \"" + el.Title + "\"")
		}
		if el.Value != "" {
			b.WriteString(" =" + el.Value)
		}
		b.WriteString("]\n")
	}
	return b.String(), nil
}

func collectMatches(root *Element, text, role string) []*Element {
	var out []*Element
	var walk func(*Element)
	walk = func(el *Element) {
		if el == nil {
			return
		}
		if (text == "" || strings.Contains(el.Title, text) || strings.Contains(el.Value, text) ||
			strings.Contains(el.Description, text)) &&
			(role == "" || strings.EqualFold(el.Role, role)) {
			out = append(out, el)
		}
		for _, c := range el.Children {
			walk(c)
		}
	}
	walk(root)
	return out
}

// ---- 动作：语义按压 ----

// Press uid 语义按压：命中后执行 + 动作后验证。
// 若 uid 失稳（not_found），返回带「重新 snapshot」引导的结构化错误文本。
func (e *Executor) Press(ctx context.Context, uid string) (string, error) {
	if uid == "" {
		return "", fmt.Errorf("press requires a uid parameter")
	}
	// 前置校验：uid 在最近快照树中存在（防止动作打在过期窗口）
	el := e.lookupUID(uid)
	if el == nil {
		return "uid " + uid + " no longer exists (tree changed?). Run computer_snapshot to get fresh uids.", nil
	}
	// 动作前锚：记录目标当前 value/窗口标题/已有输入框（供新增输入框检测）
	beforeVal := el.Value
	beforeWindows := e.windowTitles()
	beforeInputs := e.inputFieldUIDs()
	err := e.backend.Press(uid)
	if err != nil {
		return e.textErr("press "+uid, err)
	}
	// 短稳定等待
	if e.stabilizeWait > 0 {
		time.Sleep(e.stabilizeWait)
	}
	// 动作后验证（§6.3）：目标 value 变化 / 消失（界面切换）/ 窗口标题变化
	verified, detail := e.verifyAfterAction(ctx, el, beforeVal, beforeWindows)
	if !verified {
		// 引导：是否出现了新输入框（如点了「添加提醒事项」→ 出现标题输入行）？
		if hint := e.newInputHint(beforeInputs); hint != "" {
			return "✅ Pressed uid " + uid + " (" + el.Title + ")." + hint, nil
		}
		return fmt.Sprintf("⚠ Pressed uid %s (%s) but no expected change observed (%s). Possible: target missed / popup overlaying / animation unfinished. Re-run computer_snapshot to confirm.", uid, el.Title, detail), nil
	}
	// 成功也附带新输入框引导（若出现）
	hint := e.newInputHint(beforeInputs)
	if hint != "" {
		return fmt.Sprintf("✅ Pressed uid %s (%s). Verified: %s%s", uid, el.Title, detail, hint), nil
	}
	return fmt.Sprintf("✅ Pressed uid %s (%s). Verified: %s", uid, el.Title, detail), nil
}

// inputFieldUIDs 当前缓存树中所有输入框（AXTextField/AXTextArea）uid 集合。
func (e *Executor) inputFieldUIDs() map[string]bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]bool{}
	if e.lastTree == nil {
		return out
	}
	var walk func(*Element)
	walk = func(el *Element) {
		if el == nil {
			return
		}
		if el.Role == "AXTextField" || el.Role == "AXTextArea" {
			out[el.UID] = true
		}
		for _, c := range el.Children {
			walk(c)
		}
	}
	walk(e.lastTree)
	return out
}

// newInputHint 对比动作后树与 before 集合，返回「检测到新输入框」引导文本。
// 目的（真机 LLM 教训 2026-09）：press 添加类按钮后模型常不知下一步该键入——
// 检测到新输入框时显式引导 computer_type，把隐含 UI 状态变显式。
func (e *Executor) newInputHint(before map[string]bool) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastTree == nil {
		return ""
	}
	var fresh []string
	var walk func(*Element)
	walk = func(el *Element) {
		if el == nil {
			return
		}
		if (el.Role == "AXTextField" || el.Role == "AXTextArea") && !before[el.UID] {
			// 跳过搜索框误报：新输入框应有可写 frame（非 0 宽）且不在角落搜索位
			if el.Frame.W > 20 && el.Frame.H > 10 {
				fresh = append(fresh, el.UID)
			}
		}
		for _, c := range el.Children {
			walk(c)
		}
	}
	walk(e.lastTree)
	if len(fresh) > 0 {
		return " New input field detected: " + fresh[0] + " (type into it with computer_type)"
	}
	return ""
}

func (e *Executor) lookupUID(uid string) *Element {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastTree == nil {
		return nil
	}
	return findUID(e.lastTree, uid)
}

func findUID(root *Element, uid string) *Element {
	if root == nil {
		return nil
	}
	if root.UID == uid {
		return root
	}
	for _, c := range root.Children {
		if hit := findUID(c, uid); hit != nil {
			return hit
		}
	}
	return nil
}

// verifyAfterAction 动作后验证：目标 value 变化 / 窗口标题变化。
// 返回 (是否验证通过, 细节文本)。宽松策略：任一信号变化即视为通过。
func (e *Executor) verifyAfterAction(ctx context.Context, el *Element, beforeVal string, beforeWindows []string) (bool, string) {
	if e.verifyWaitMs <= 0 {
		return true, "(verification disabled)"
	}
	// 简单轮询：重新 snapshot 观察目标状态变化
	deadline := time.Now().Add(time.Duration(e.verifyWaitMs) * time.Millisecond)
	for {
		sn, err := e.backend.Snapshot(SnapshotOpts{MaxDepth: 9})
		if err == nil && sn.Tree != nil {
			e.mu.Lock()
			e.lastTree = sn.Tree
			e.lastApp = sn.FrontmostApp
			e.lastPid = sn.Pid
			e.lastWindows = sn.Windows
			e.mu.Unlock()
			cur := findUID(sn.Tree, el.UID)
			if cur != nil {
				if cur.Value != "" && cur.Value != beforeVal {
					return true, "element value changed: " + beforeVal + " → " + cur.Value
				}
			} else if el.UID != "" && el.UID != "a:0" {
				// 目标从树消失 = 界面切换（popover 内按钮点击后面板切换/关闭），
				// 视为动作生效——仅应用根 a:0 永不消失
				return true, "target element gone (UI switched)"
			}
			// 窗口标题集合变化（验证成功信号；与动作前对比）
			if !sameTitles(beforeWindows, sn.Windows) {
				return true, "window title changed"
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Duration(e.verifyPollMs) * time.Millisecond)
	}
	return false, "no visible change in target state or windows"
}

// windowTitles 当前窗口标题列表（动作前锚定）。
func (e *Executor) windowTitles() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.lastWindows))
	for _, w := range e.lastWindows {
		if w.Title != "" {
			out = append(out, w.Title)
		}
	}
	return out
}

// sameTitles 两窗口列表标题是否相同（忽略顺序）。
func sameTitles(a []string, ws []Window) bool {
	if len(a) != len(ws) {
		return false
	}
	set := map[string]bool{}
	for _, t := range a {
		set[t] = true
	}
	for _, w := range ws {
		if w.Title != "" && !set[w.Title] {
			return false
		}
	}
	return true
}

// ---- 文本化错误 ----

// textErr 把 Backend/HelperError 转成模型可见的纠错文本（§6.3 结构化错误）。
func (e *Executor) textErr(op string, err error) (string, error) {
	var he *HelperError
	if errors.As(err, &he) {
		switch he.Code {
		case ErrPermDenied:
			return "permission denied: " + he.Msg + ". Grant it in System Settings → Privacy & Security → Accessibility and retry.", nil
		case ErrNotFound:
			return "target not found (" + he.Msg + "). Re-run computer_snapshot to get fresh uids.", nil
		case ErrTimeout:
			return "operation timed out (" + he.Msg + "). The system may be busy or the target app unresponsive — retry or snapshot again.", nil
		case ErrUnsupported:
			return "operation not supported (" + he.Msg + ").", nil
		default:
			return "operation failed: " + he.Msg, nil
		}
	}
	return "", err
}

// ---- 动作：type/key/open/activate 等直通封装 ----// Type 键入当前焦点（先聚焦 target_uid 可选）。
func (e *Executor) Type(ctx context.Context, text, targetUID string) (string, error) {
	if targetUID != "" {
		el := e.lookupUID(targetUID)
		if el == nil {
			return "uid " + targetUID + " does not exist. Run computer_snapshot first.", nil
		}
		// 聚焦目标（点击/按压）；文本框 press 语义 = 聚焦
		if err := e.backend.Press(targetUID); err != nil {
			return e.textErr("focus "+targetUID, err)
		}
		if e.stabilizeWait > 0 {
			time.Sleep(e.stabilizeWait)
		}
	}
	if err := e.backend.TypeText(text); err != nil {
		return e.textErr("type", err)
	}
	return fmt.Sprintf("✅ Typed %d characters.", len([]rune(text))), nil
}

// Key 键组合/单键。
func (e *Executor) Key(ctx context.Context, keys string) (string, error) {
	if err := e.backend.KeyChord(keys); err != nil {
		return e.textErr("key "+keys, err)
	}
	return "✅ Key pressed: " + keys, nil
}

// SetApprovedApps 热更新已授权 App 集合（settings.json 变更后 bridge 调用，
// 无需重启插件；nil = 恢复全放行库默认）。线程安全（门禁判定持锁）。
func (e *Executor) SetApprovedApps(approved map[string]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.appApproved = approved
}

// checkAppAuthorized 统一授权门：app 未在已授权集合 → 返回拒绝提示（nil = 放行）。
// 集合为 nil（库默认/未装配）→ 全部放行；空集合 → 全部拒绝（安全默认）。
// 判定用大小写不敏感子串匹配（app 名可能来自快照前台名/用户输入/菜单归属，
// 允许「提醒事项」匹配「提醒事项 (123)」这类后缀）。
func (e *Executor) checkAppAuthorized(app string) string {
	e.mu.Lock()
	approved := e.appApproved
	e.mu.Unlock()
	if approved == nil {
		return "" // 未装配门禁：放行（单测/直跑）
	}
	if app == "" {
		return "" // 空目标不判（无 app 的动作如按键/键入）
	}
	if e.approvedContains(approved, app) {
		return ""
	}
	// 懒刷新：快照未命中 → 若有 reload 回调，重读一次最新集合再判（用户刚在
	// 设置页加了授权、进程内快照旧——bridge 长跑不重启也能立即生效）。
	e.mu.Lock()
	reload := e.reloadApproved
	e.mu.Unlock()
	if reload != nil {
		if fresh := reload(); fresh != nil {
			e.SetApprovedApps(fresh)
			if e.approvedContains(fresh, app) {
				return ""
			}
		}
	}
	return fmt.Sprintf("App \"%s\" is not approved for desktop control yet. Add it under Settings → Plugins → Computer Use → Approved Apps, then retry (approval allows all computer_* actions on that app).", app)
}

// approvedContains 集合匹配（大小写不敏感子串双向）。
func (e *Executor) approvedContains(approved map[string]bool, app string) bool {
	for name := range approved {
		if strings.EqualFold(name, app) || strings.Contains(app, name) || strings.Contains(name, app) {
			return true
		}
	}
	return false
}

// OpenApp 启动/激活应用（统一授权门：未授权 App 拒绝打开）。
// 真实前台确认：轮询直到前台应用匹配目标（open 是慢动作——WPS 冷启动可达数秒；
// 若仅凭「前台非空」就返回，旧前台（宿主 Electron）会让调用假成功——模型拿 ✅
// 继续 snapshot 仍看到宿主窗口 → 误判「WPS 窗口不可见」→ bash 自救。教训 2026-09）。
func (e *Executor) OpenApp(ctx context.Context, name string) (string, error) {
	if denied := e.checkAppAuthorized(name); denied != "" {
		return "⛔ " + denied, nil
	}
	if err := e.backend.OpenApp(name); err != nil {
		return e.textErr("open "+name, err)
	}
	// 等待目标应用成为前台（匹配显示名/进程名/子串；3s 超时）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		sn, err := e.backend.Snapshot(SnapshotOpts{MaxDepth: 9})
		if err == nil && sn.FrontmostApp != "" {
			if appsMatch(sn.FrontmostApp, name) {
				e.mu.Lock()
				e.lastApp = sn.FrontmostApp
				e.lastPid = sn.Pid
				e.mu.Unlock()
				return fmt.Sprintf("✅ Opened/activated %s (frontmost now: %s)", name, sn.FrontmostApp), nil
			}
			// 前台还是别的 app（如宿主）——继续等，不假成功
		}
		time.Sleep(200 * time.Millisecond)
	}
	// 超时未确认前台：诚实回报（⚠ 而非 ✅），引导 snapshot 确认
	if sn, err := e.backend.Snapshot(SnapshotOpts{MaxDepth: 9}); err == nil && sn.FrontmostApp != "" {
		return fmt.Sprintf("⚠ Open command sent for %s, but it did not become frontmost within 3s (frontmost: %s). It may be slow to launch or another app took focus. Run computer_snapshot to confirm.", name, sn.FrontmostApp), nil
	}
	return fmt.Sprintf("⚠ Open command sent for %s, but frontmost state could not be read within 3s. Run computer_snapshot to confirm.", name), nil
}

// appsMatch 前台应用名与目标名匹配。归一化后比较（小写 + 去空格/标点）：
// 「WPS Office」→「wpsoffice」，「提醒事项 (2)」→「提醒事项2」都能对上。
// normAppName 应用名归一化（小写 + 去空格/标点，保留 CJK）：
// 「WPS Office」→「wpsoffice」与 bundle 文件名一致——授权集合里存的是文件名
// （wpsoffice）还是显示名（WPS Office），归一化后都能互相匹配（2026-09 真机教训：
// app:pick 返回 .app 文件名，显示名经本地化不同，门禁须归一化匹配）。
func normAppName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r >= 0x80 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func appsMatch(front, target string) bool {
	fl, tl := normAppName(front), normAppName(target)
	return fl == tl || strings.Contains(fl, tl) || strings.Contains(tl, fl)
}

// Activate 激活窗口/应用前台。backend.Activate(pid, windowTitle) 契约里 pid 由
// backend 按 app 名解析（跨 App 激活），executor 侧传 app 名（P0：OpenApp 已按名
// 激活，Activate 主要用于窗口标题级激活，pid=0 让 backend 查前台/按名解析）。
// 激活后短轮询确认前台切换（防假成功——activate 返回 ✅ 但前台未变，模型接着
// snapshot 看到旧 app → 误判；教训 2026-09）。
func (e *Executor) Activate(ctx context.Context, app, windowTitle string) (string, error) {
	if denied := e.checkAppAuthorized(app); denied != "" {
		return "⛔ " + denied, nil
	}
	if err := e.backend.Activate(e.lastPidFor(app), app, windowTitle); err != nil {
		return e.textErr("activate "+app, err)
	}
	if app != "" {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			sn, err := e.backend.Snapshot(SnapshotOpts{MaxDepth: 9})
			if err == nil && sn.FrontmostApp != "" && appsMatch(sn.FrontmostApp, app) {
				e.mu.Lock()
				e.lastApp = sn.FrontmostApp
				e.lastPid = sn.Pid
				e.mu.Unlock()
				return fmt.Sprintf("✅ Activated: %s %s (frontmost confirmed: %s)", app, windowTitle, sn.FrontmostApp), nil
			}
			time.Sleep(150 * time.Millisecond)
		}
	}
	return fmt.Sprintf("⚠ Activate command sent for %s %s, but the foreground switch was not confirmed. Run computer_snapshot to confirm.", app, windowTitle), nil
}

// lastPidFor 若 app 匹配最近前台应用则用其 pid，否则 0（backend 按名解析）。
func (e *Executor) lastPidFor(app string) int {
	if app == "" {
		return e.lastPid
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastApp != "" && (strings.EqualFold(e.lastApp, app) || strings.Contains(e.lastApp, app)) {
		return e.lastPid
	}
	return 0
}

// ---- 动作：菜单/文本按压 + 等待验证（P0-5 工具支撑） ----

// PressMenu 菜单路径按压（app 菜单栏逐项匹配；走 backend.Press menuPath）。
func (e *Executor) PressMenu(ctx context.Context, app string, path []string) (string, error) {
	if len(path) == 0 {
		return "Menu path is empty.", nil
	}
	// 先确保目标应用在前台（菜单属于具体应用；过统一授权门）
	if denied := e.checkAppAuthorized(app); denied != "" {
		return "⛔ " + denied, nil
	}
	if err := e.backend.OpenApp(app); err != nil {
		return e.textErr("open "+app, err)
	}
	if e.stabilizeWait > 0 {
		time.Sleep(e.stabilizeWait)
	}
	err := e.backend.PressMenu(app, path)
	if err != nil {
		return e.textErr("menu "+app+"/"+strings.Join(path, "/"), err)
	}
	return "✅ Menu executed: " + app + " → " + strings.Join(path, " → "), nil
}

// PressTitle 按标题文本匹配当前快照中的可交互元素并按压。
func (e *Executor) PressTitle(ctx context.Context, title string) (string, error) {
	el := e.findTitle(title)
	if el == nil {
		return "No element with title containing \"" + title + "\" found. Run computer_snapshot to view the current UI.", nil
	}
	return e.Press(ctx, el.UID)
}

func (e *Executor) findTitle(title string) *Element {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastTree == nil {
		return nil
	}
	var found *Element
	var walk func(*Element)
	walk = func(el *Element) {
		if found != nil || el == nil {
			return
		}
		if (strings.Contains(el.Title, title) || strings.Contains(el.Value, title)) && isPressableControl(el) {
			found = el
			return
		}
		for _, c := range el.Children {
			walk(c)
		}
	}
	walk(e.lastTree)
	return found
}

// WaitVerify 等待界面文本出现/消失（轮询 snapshot 树标题）。
func (e *Executor) WaitVerify(ctx context.Context, text string, gone bool, timeoutMS int) (string, error) {
	if text == "" {
		return "computer_wait_verify needs an expect_text parameter.", nil
	}
	if timeoutMS <= 0 {
		timeoutMS = 5000
	}
	deadline := time.Now().Add(time.Duration(timeoutMS) * time.Millisecond)
	var lastDetail string
	for {
		found := false
		sn, err := e.backend.Snapshot(SnapshotOpts{MaxDepth: 9})
		if err == nil && sn.Tree != nil {
			e.mu.Lock()
			e.lastTree = sn.Tree
			e.lastApp = sn.FrontmostApp
			e.lastPid = sn.Pid
			e.lastWindows = sn.Windows
			e.mu.Unlock()
			found = treeContainsText(sn.Tree, text)
			lastDetail = "expected text \"" + text + "\" not present"
		}
		if found != gone { // 出现 且 expect 出现 → 成功；消失 且 expect 消失 → 成功
			if gone {
				return "✅ Confirmed \"" + text + "\" disappeared.", nil
			}
			return "✅ Confirmed \"" + text + "\" appeared.", nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Duration(e.verifyPollMs) * time.Millisecond)
	}
	if gone {
		return "⏰ Timeout: \"" + text + "\" is still present. The action may not have succeeded — re-run snapshot to confirm.", nil
	}
	return "⏰ Timeout: " + lastDetail + ". The action may not have succeeded — re-run snapshot to confirm.", nil
}

func treeContainsText(root *Element, text string) bool {
	if root == nil {
		return false
	}
	if strings.Contains(root.Title, text) || strings.Contains(root.Value, text) ||
		strings.Contains(root.Description, text) {
		return true
	}
	for _, c := range root.Children {
		if treeContainsText(c, text) {
			return true
		}
	}
	return false
}

// ---- 供工具/e2e 用的 uid 直查（缓存树） ----

// FindUID 在最近快照缓存树中找首个 title/value/description 含关键字的
// 可交互元素 uid（返回 "" = 未找到）。供 press/type 前定位目标。
func (e *Executor) FindUID(substr string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastTree == nil {
		return ""
	}
	return findUIDByText(e.lastTree, substr)
}

// containerRoles 文本命中时要跳过的纯容器角色（无按压语义的壳/承载）：
// AXWindow/AXMenuBar/AXMenu 等只有 AXRaise/AXCancel 类动作，press 它们
// 无业务效果——命中其标题（如窗口「新建文档」含「新建」）是误命中。
// 真机教训 2026-09：FindUID("新建") 命中窗口 a:0:1「新建文档」而非「新建」按钮。
var containerRoles = map[string]bool{
	"AXWindow": true, "AXMenuBar": true, "AXMenu": true, "AXGroup": true,
	"AXScrollArea": true, "AXSplitter": true, "AXToolbar": true, "AXApplication": true,
}

// isPressableControl 是否可按压业务控件（文本命中的合格目标）。
func isPressableControl(el *Element) bool {
	if el == nil || containerRoles[el.Role] {
		return false
	}
	return len(el.Actions) > 0 || el.Focusable || el.Role == "AXTextField" ||
		el.Role == "AXButton" || el.Role == "AXCheckBox" || el.Role == "AXMenuItem" ||
		el.Role == "AXRadioButton" || el.Role == "AXLink" || el.Role == "AXDisclosureTriangle"
}

func findUIDByText(el *Element, substr string) string {
	if el == nil {
		return ""
	}
	if strings.Contains(el.Title, substr) || strings.Contains(el.Value, substr) ||
		strings.Contains(el.Description, substr) {
		if isPressableControl(el) {
			return el.UID
		}
	}
	for _, c := range el.Children {
		if uid := findUIDByText(c, substr); uid != "" {
			return uid
		}
	}
	return ""
}

// Screenshot 捕获当前屏幕（视觉档看图通道：工具把图注入模型下一轮）。
// 返回 Image（PNG 字节）；权限缺失时 helper 返回 perm_denied（错误文本引导）。
func (e *Executor) Screenshot(ctx context.Context) (*Image, error) {
	img, err := e.backend.Screenshot(Rect{}, 0)
	if err != nil {
		he, ok := err.(*HelperError)
		if ok && he.Code == ErrPermDenied {
			return nil, fmt.Errorf("%s (screen-recording permission: System Settings → Privacy & Security → Screen Recording)", he.Msg)
		}
		return nil, err
	}
	return img, nil
}
