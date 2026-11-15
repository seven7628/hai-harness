package main

// surface_agent.go —— 插件 ↔ Agent 双向互通的宿主侧（09-agent-bridge §5.1 / §6.2–6.6 / §8，P1c）。
//
// 一条命令四个 name（插件 host.command('agent.run', …) → main 按 token 注入 id →
// 桥收到 {type:"surface_agent", payload:{id, name, args}}）：
//
//	agent.session  {title?}                            → {session_id}
//	agent.run      {session_id?, input, instructions?}  → {run_id, session_id}（先回执，再流事件）
//	agent.cancel   {run_id}                            → {}
//	tool.result    {call_id, ok, result?, error?}      → {}（P1 只做形状）
//
// 三条不变式，每条都有测试钉子：
//
//  1. **入口校验顺序固定**：id 形状 → index/enabled → manifest 声明 → 宿主 allowlist →
//     参数/配额。顺序即语义：没声明不能因为「顺便也停用了」而报成 denied，否则插件作者
//     永远不知道该改 manifest 还是找用户开权限。
//  2. **插件只能碰自己的东西**：会话按 pluginID 反查（内存 → 磁盘），跨插件/项目会话一律
//     denied。装配只走 P1a 的 runtimePlugin 入口（pluginRuntimeForHome / createIn），
//     绝不因为这条命令造出项目 runtime（09 §4.4 的红线）。
//  3. **回执先行、事件后到、终态后停投**：agent.run 的 command_response 一定排在第一条
//     agent.* 事件之前；seq 每 run 单调；done/error 之后不再投任何事件。写 stdout 复用
//     emit 的 dispatchMu + out.writeLine（不自己开 writer），插件事件与同一会话的 SDK
//     事件行同序。
//
// P1 明确不做的：`schema` / `tools` 参数（P2 才启用，出现也不报错）、插件工具
// （plugin__*，P2）、真实 tool.call/waiter（P1 的 tool.result 只回 ok + 审计）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

const (
	// 四个宿主放行的 name（§6.6）。不在表里的一律 denied —— 声明不等于授权。
	surfaceAgentSessionName = "agent.session"
	surfaceAgentRunName     = "agent.run"
	surfaceAgentCancelName  = "agent.cancel"
	surfaceAgentToolResult  = "tool.result"

	// surfaceAgentEventType bridge stdout 的信封名（main 原样转发 data 到渲染层）。
	surfaceAgentEventType = "surface_agent_event"

	// 事件 type（§6.6 事件 data 表）。
	surfaceAgentEvDelta = "agent.delta"
	surfaceAgentEvTool  = "agent.tool"
	surfaceAgentEvUsage = "agent.usage"
	surfaceAgentEvDone  = "agent.done"
	surfaceAgentEvError = "agent.error"
	// surfaceToolCallEvent 工具调用下发（§5.2 / §6.6，P2 启用）。
	// 名字以 surfaceTool* 而非 surfaceAgentEv* 开头是刻意的：它不是「run 的投影」，
	// 而是一次**双向**调用的上半场（下半场是插件的 tool.result 回执）。
	surfaceToolCallEvent = "tool.call"
)

// 拒绝 reason 封闭集合（§6.5 + §6.6 的 agent.error reason 表）。
//
// 为什么 code 与 reason 取同一个值：§6.6 的拒绝形状是
// `{ok:false, code, data:{ok:false, reason}}`，两处写两个不同的枚举只会让插件作者
// 对不上号（前端要同时读 code 与 data.reason）。这里刻意同值。
const (
	surfaceReasonNotDeclared  = "not_declared"  // manifest.commands 里没有
	surfaceReasonDenied       = "denied"        // 停用/卸载/越权/跨插件/跨项目
	surfaceReasonQuota        = "quota"         // 并发 / 每小时 / 体积
	surfaceReasonInvalidInput = "invalid_input" // 参数形状或空 input
	surfaceReasonNoSession    = "no_session"    // 会话不存在（或建不出来）
	surfaceReasonCancelled    = "cancelled"     // 取消/超时的终态事件 reason
	surfaceReasonBridgeError  = "bridge_error"  // 桥内部失败
	// 以下三个是 P2 工具调用引入的（§6.5 同一张表，之前没有消费者）：
	surfaceReasonTimeout        = "timeout"         // 插件在 deadline 内没回 tool.result
	surfaceReasonPluginUnloaded = "plugin_unloaded" // 等待中插件被卸载/停用
	// surfaceReasonToolError 插件回 ok:false（面板侧错误）。错误文本原样回给模型，
	// 审计只记这个封闭值 —— 插件给的文本是内容，不是可统计的类别。
	surfaceReasonToolError = "tool_error"
	// surfaceToolReasonNoSuchCall tool.result 没有对应的等待者（幂等回执，无副作用）。
	surfaceToolReasonNoSuchCall = "no_such_call"
)

// 额度（§8.3，V1 默认值）。测试通过 surfaceAgentHub 的字段注入小值。
const (
	surfaceAgentMaxInputBytes  = 32 << 10 // input ≤ 32 KiB
	surfaceAgentMaxInstrBytes  = 8 << 10  // instructions ≤ 8 KiB
	surfaceAgentMaxRunsPerHour = 20       // 20 run/小时/surface
	surfaceAgentRunTimeout     = 10 * time.Minute
	// surfaceAgentDeltaWindow 文本增量的合并窗口（§6.6「Go 侧 50ms 合并」）。
	surfaceAgentDeltaWindow = 50 * time.Millisecond
	// surfaceAgentPersistTimeout 新建会话首次落盘（`<storeDir>/<sid>.jsonl`）的上限。
	// 用独立超时而不是请求 ctx：dispatch 的 ctx 在回执后就结束，拿它写盘会留下半行
	// state（jsonl 是 append-only，半行 = 坏记录），那比「回执晚 5s」糟得多。
	surfaceAgentPersistTimeout = 5 * time.Second
)

// surfaceAgentHostCommands 宿主放行的 name（§5.1）。声明（manifest）∩ 这张表 = 放行。
var surfaceAgentHostCommands = map[string]bool{
	surfaceAgentSessionName: true,
	surfaceAgentRunName:     true,
	surfaceAgentCancelName:  true,
	surfaceAgentToolResult:  true,
}

// surfaceAgentAssembleHook 仅测试用：run 装配完成、Ask 之前的确定性停点，
// 让测试把会话 loop 换成假 provider（生产恒 nil，与 hub 系列 hook 同款）。
var surfaceAgentAssembleHook func(*bridgeSession)

// ---- 命令入口 ----

// handleSurfaceAgent `surface_agent` 的唯一入口：校验链 → 分派到四个 name。
func (m *manager) handleSurfaceAgent(c command) {
	id := strings.TrimSpace(str(c.Payload, "id"))
	name := strings.TrimSpace(str(c.Payload, "name"))
	args, _ := c.Payload["args"].(map[string]any)

	// ① id 形状：非法 id 连审计都只能用 <empty>/截断形式写（它可能就是恶意输入）。
	if !surfaceIDPattern.MatchString(id) {
		m.denySurfaceAgent(c, id, name, surfaceReasonDenied, "surface id 不合法", nil)
		return
	}
	// ② index/enabled：停用/卸载 = 能力一起收走（09 §5.3「撤销后命令 denied」）。
	// 判据只认 surfaceStore（安装期与启用状态的唯一真相源），不看内存 runtime。
	if !m.surfaces.Enabled(id) {
		m.denySurfaceAgent(c, id, name, surfaceReasonDenied, "插件未安装或已停用", nil)
		return
	}
	// ③ manifest 声明：声明是白名单的一半（§6.2 的第一步），读不出来一律 fail closed。
	mf, err := m.surfaces.Manifest(id)
	if err != nil {
		m.denySurfaceAgent(c, id, name, surfaceReasonDenied, "读不到插件 manifest", nil)
		return
	}
	if !surfaceDeclares(mf.Commands, name) {
		m.denySurfaceAgent(c, id, name, surfaceReasonNotDeclared, "manifest 未声明该命令", nil)
		return
	}
	// ④ 宿主 allowlist：声明了但宿主不放行（含 P2 才启用的名字）也是 denied。
	if !surfaceAgentHostCommands[name] {
		m.denySurfaceAgent(c, id, name, surfaceReasonDenied, "宿主不允许该命令", nil)
		return
	}
	// ⑤ 参数/配额：落到各 name 自己的 handler（拒绝一律走 auditSurfaceAgent）。
	switch name {
	case surfaceAgentSessionName:
		m.handleSurfaceAgentSession(c, id, args)
	case surfaceAgentRunName:
		m.handleSurfaceAgentRun(c, id, args)
	case surfaceAgentCancelName:
		m.handleSurfaceAgentCancel(c, id, args)
	default:
		m.handleSurfaceAgentToolResult(c, id, args)
	}
}

// denySurfaceAgent 拒绝回执 + 审计：
// `{ok:false, code:<reason>, data:{ok:false, reason}}`（§6.6 的形状，逐字）。
//
// extra 只带排查用的上下文（session/run/bytes），不带用户内容。
func (m *manager) denySurfaceAgent(c command, id, name, reason, msg string, extra map[string]any) {
	m.respCode(c, false, msg, reason, map[string]any{"ok": false, "reason": reason})
	m.auditSurfaceAgent(id, surfaceAgentAction(name), false, extra, reason)
}

// auditSurfaceAgent 写 `surfaces/audit.jsonl`（§8.5）。
//
// 行形状：{id, action, ts, ok, session?, run?, bytes?, reason?}；写失败不影响主流程
// （surfaceStore.writeAudit 自己吞错只记日志）。审计 action 按 name 归并：
// agent.session/cancel 记 agent_run —— 它们是同一次 Agent 交互的准备与收尾，
// 分开记只会让「这个插件跑过几次」统计不出来。
func (m *manager) auditSurfaceAgent(id, action string, ok bool, extra map[string]any, reason string) {
	rec := map[string]any{"id": auditID(id), "action": action, "ok": ok}
	for k, v := range extra {
		switch vv := v.(type) {
		case string:
			if vv == "" {
				continue
			}
		case int:
			if vv == 0 {
				continue
			}
		case int64:
			if vv == 0 {
				continue
			}
		default:
			continue
		}
		rec[k] = v
	}
	m.surfaces.writeAudit(rec, reason)
}

// surfaceAgentAction 审计 action 名（§8.5 的封闭集合）。
func surfaceAgentAction(name string) string {
	switch name {
	case surfaceAgentToolResult:
		// P2 起并入 tool_call：插件回执与模型侧调用是**同一次**工具调用，
		// 两个 action 名会让「这个插件调过几次工具」统计不出来（§8.5）。
		return "tool_call"
	default:
		return "agent_run"
	}
}

// ---- agent.session ----

// handleSurfaceAgentSession 建一个插件虚拟 Project 的会话（随机 id），**不跑模型**。
//
// 装配走 P1a 同一个入口（pluginRuntimeForHome → createIn）：这条命令存在的意义是
// 「插件自己记 session_id 后再 agent.run 复用」（D4），所以它必须造出与用户手动
// 新建会话**同构**的东西 —— 工具面 denylist、record 键 plugin-<id>、落盘在
// plugin-sessions/<id>/sessions，一处都不能走旁路。
//
// **建完立刻落一条 state**（D4 的边缘路径）：空会话没有消息，Session 只在有输入/轮次
// 检查点时 persist，于是「建会话但还没发消息」这条路径在磁盘上不产生任何东西 ——
// 而 listSessions 只从内存 + `<storeDir>/*.jsonl` 取，进程重启后这个 session_id 就
// 凭空消失了（插件手里那个 id 变成 no_session）。所以这里显式 Persist 一次，
// 失败就不把 id 交出去（否则插件拿到的是「看起来成功、重启就丢」的 id）。
func (m *manager) handleSurfaceAgentSession(c command, id string, args map[string]any) {
	home := pluginHomePath(id)
	ws, err := m.pluginRuntimeForHome(home)
	if err != nil || ws == nil {
		m.denySurfaceAgent(c, id, surfaceAgentSessionName, surfaceReasonDenied, "插件运行时装配失败", nil)
		return
	}
	sid := m.genID()
	bs, err := m.createIn(ws, sid, "", sessionOpts{})
	if err != nil {
		// 内部 error（磁盘/装配细节）只进审计：回给插件的是固定文案 + 固定 reason。
		m.respCode(c, false, "plugin session 创建失败", surfaceReasonBridgeError,
			map[string]any{"ok": false, "reason": surfaceReasonBridgeError})
		m.auditSurfaceAgent(id, "agent_run", false, map[string]any{"session": sid},
			surfaceReasonBridgeError+": "+err.Error())
		return
	}
	// D4：空会话也要有磁盘身份，否则回执里的 session_id 活不过重启。
	if perr := persistSurfaceAgentSession(bs); perr != nil {
		// 撤回内存会话：留下它就是「内存里看得见、重启就没」的幽灵，
		// 而插件这次没拿到 id（不会去用），撤掉比留着更诚实。
		dropSurfaceAgentSession(ws, sid, bs)
		m.respCode(c, false, "plugin session 落盘失败", surfaceReasonBridgeError,
			map[string]any{"ok": false, "reason": surfaceReasonBridgeError})
		m.auditSurfaceAgent(id, "agent_run", false, map[string]any{"session": sid},
			surfaceReasonBridgeError+" persist: "+perr.Error())
		return
	}
	// title 可选（§6.6）：只写手动标题覆盖（会话列表读同一份），不触发 AI 标题生成 ——
	// 插件建会话不该顺手花一次 token。
	if title := strings.TrimSpace(str(args, "title")); title != "" {
		sessionManualTitles.set(workspaceKey(home), sid, clipText(title, 80))
	}
	m.resp(c, true, "", map[string]any{"session_id": sid})
	m.auditSurfaceAgent(id, "agent_run", true, map[string]any{"session": sid}, "")
}

// persistSurfaceAgentSession 让刚建出来的会话在磁盘上**立刻**有身份：
// `<storeDir>/<sid>.jsonl` + 一条 state 行（Session.Persist → SaveState，无消息也写）。
//
// 不伪造文件：写盘走 Session 自己的路径，恢复端读到的就是 SDK 协议里的那条 state，
// 与「发过消息的会话」同一种记录形状（空历史）。
func persistSurfaceAgentSession(bs *bridgeSession) error {
	if bs == nil || bs.s == nil {
		return fmt.Errorf("session 未装配")
	}
	ctx, cancel := context.WithTimeout(context.Background(), surfaceAgentPersistTimeout)
	defer cancel()
	return bs.s.Persist(ctx)
}

// dropSurfaceAgentSession 撤回一个刚建出来、没能落盘的会话。
//
// 只在「建完立刻失败」的窗口里调用：此刻会话没有任何输入、没有 run、也没进 Hub mesh
// 注册表（插件 runtime 不走那条注册），所以摘表 + 停 goroutine 不会影响别人。
//
// 收尾走 stopSession（完整关停：Session.Shutdown + checkpoint 协调器 drain + 事件文件），
// 代价是它会顺带发出 session_persist_error / session_closed 两条会话级事件。这是**要的**：
// 插件侧会一直等在 host.command 的 Promise 上、看不到会话事件；而桌面端那边这两条
// 恰恰是「刚才那次落盘失败了」的唯一可观察线索（未知 sid 只会多一个不可见的视图
// 条目，不会出现在会话列表里 —— 我们这里不发 session 列表事件）。
func dropSurfaceAgentSession(ws *workspaceRuntime, sid string, bs *bridgeSession) {
	if ws != nil {
		ws.mu.Lock()
		if cur, ok := ws.sessions[sid]; ok && cur == bs {
			delete(ws.sessions, sid)
		}
		ws.mu.Unlock()
	}
	if bs != nil {
		stopSession(bs) // 停 run goroutine，别把半成品留在进程里
	}
}

// ---- agent.run ----

// handleSurfaceAgentRun 起一次插件 run：校验 → 配额 → **先回执** → 异步跑。
//
// 回执先行的理由（§6.6）：插件在 host.command(...) 的 Promise 上等回执，事件走另一条
// 通道。若先跑再回执，第一段 delta 会早于 ack 到达，插件侧「还没拿到 run_id 就收到
// 事件」只能丢弃或猜归属。所以这里先写 command_response，再 Ask。
func (m *manager) handleSurfaceAgentRun(c command, id string, args map[string]any) {
	input := str(args, "input")
	instructions := str(args, "instructions")
	// 空 input（含纯空白）= 参数错误，不是配额。
	if strings.TrimSpace(input) == "" {
		m.denySurfaceAgent(c, id, surfaceAgentRunName, surfaceReasonInvalidInput, "input 为空", nil)
		return
	}
	// 体积闸门（§8.3 input ≤32 KiB / instructions ≤8 KiB，按字节计）。
	// 与 image 预算同一条纪律：在入口按字节判，让插件拿到可行动的 reason，
	// 而不是让大 payload 在别处炸成一行看不懂的错误。
	if len(input) > surfaceAgentMaxInputBytes {
		m.denySurfaceAgent(c, id, surfaceAgentRunName, surfaceReasonQuota,
			fmt.Sprintf("input 超过 %d 字节", surfaceAgentMaxInputBytes), map[string]any{"bytes": len(input)})
		return
	}
	if len(instructions) > surfaceAgentMaxInstrBytes {
		m.denySurfaceAgent(c, id, surfaceAgentRunName, surfaceReasonQuota,
			fmt.Sprintf("instructions 超过 %d 字节", surfaceAgentMaxInstrBytes), map[string]any{"bytes": len(instructions)})
		return
	}
	// P1 忽略 schema / tools（P2 再启用）：出现也不报错，只是不生效 —— 报错会让
	// 「先写前端、后等宿主」的插件在 P1 阶段直接不可用。
	home := pluginHomePath(id)
	ws, err := m.pluginRuntimeForHome(home)
	if err != nil || ws == nil {
		m.denySurfaceAgent(c, id, surfaceAgentRunName, surfaceReasonDenied, "插件运行时装配失败", nil)
		return
	}
	// 会话归属：不给 session_id 就新建（D4）；给了必须属于**同一个** surface。
	sid := strings.TrimSpace(str(args, "session_id"))
	newSession := false
	if sid == "" {
		sid = m.genID()
		newSession = true
	} else {
		owner, found := m.surfaceAgentSessionOwner(sid)
		if !found {
			m.denySurfaceAgent(c, id, surfaceAgentRunName, surfaceReasonNoSession, "会话不存在", map[string]any{"session": sid})
			return
		}
		if owner != id {
			// 跨插件 / 项目会话：不告诉对方它属于谁（那是别人的元数据）。
			m.denySurfaceAgent(c, id, surfaceAgentRunName, surfaceReasonDenied, "会话不属于该插件", map[string]any{"session": sid})
			return
		}
	}
	// 配额（并发 1/surface、20 run/小时），超限**不排队**（§8.3「超限 quota，不排队」）。
	// 配额先于建会话：被 quota 挡下的调用不该顺手留下一个空会话（插件重试就会攒一堆）。
	hub := m.surfaceAgentHub()
	run, reason, msg := hub.reserve(id, sid)
	if run == nil {
		m.denySurfaceAgent(c, id, surfaceAgentRunName, reason, msg, map[string]any{"session": sid})
		return
	}
	bs, err := m.createIn(ws, sid, "", sessionOpts{})
	if err != nil {
		hub.release(run)
		// 不带 session_id 时"建不出来"就是 no_session；带了（复用）时是桥内部失败。
		reason := surfaceReasonBridgeError
		msg := "session 装配失败"
		if newSession {
			reason = surfaceReasonNoSession
			msg = "plugin session 创建失败"
		}
		m.respCode(c, false, msg, reason, map[string]any{"ok": false, "reason": reason})
		m.auditSurfaceAgent(id, "agent_run", false, map[string]any{"session": sid}, reason+": "+err.Error())
		return
	}
	if surfaceAgentAssembleHook != nil {
		surfaceAgentAssembleHook(bs)
	}
	// 不带 session_id 新建的会话：**先落盘再 ack/Ask**（D4）。
	// 回执里的 session_id 必须是「重启后还在」的那个 id；Ask 之后 run loop 虽然会
	// 在跑模型前 persist 一次，但那是「ack 之后」的事 —— 这中间进程被杀，插件手里
	// 就留下一个磁盘上不存在的 id。复用已有会话不重复写：它本来就有 jsonl。
	if newSession {
		if perr := persistSurfaceAgentSession(bs); perr != nil {
			hub.release(run)
			dropSurfaceAgentSession(ws, sid, bs)
			m.respCode(c, false, "plugin session 落盘失败", surfaceReasonBridgeError,
				map[string]any{"ok": false, "reason": surfaceReasonBridgeError})
			m.auditSurfaceAgent(id, "agent_run", false, map[string]any{"session": sid},
				surfaceReasonBridgeError+" persist: "+perr.Error())
			return
		}
	}
	run.bs = bs
	hub.armTimeout(run)
	// 用户消息：input 是正文（标题/气泡来源），instructions 作为**同一消息的第二个
	// 文本块**跟在后面 —— P1 没有 per-run system prompt 通道，而另造一个隐藏块类型
	// 等于在 transcript 协议里加一节；两个文本块是最小改动且模型两段都看得到。
	blocks := []core.Content{{Type: core.ContentTypeText, Content: input}}
	if instructions != "" {
		blocks = append(blocks, core.Content{Type: core.ContentTypeText, Content: instructions})
	}
	if bs.surfaceAgentRun.Swap(run) != nil {
		// 同一会话上一条 run 还没收尾（并发闸门只看 surface，用户手动发起的 ask 不算）。
		// 覆盖指针会让旧 run 的终态丢失，宁可拒绝也不静默串台。
		m.surfaceAgentHub().release(run)
		m.denySurfaceAgent(c, id, surfaceAgentRunName, surfaceReasonQuota, "该会话已有 run 在跑", map[string]any{"session": sid})
		return
	}
	// 先回执（含 run_id + session_id），再入队：事件的第一条 delta 只能出现在这行之后。
	m.resp(c, true, "", map[string]any{"run_id": run.id, "session_id": sid})
	m.auditSurfaceAgent(id, "agent_run", true,
		map[string]any{"session": sid, "run": run.id, "bytes": len(input) + len(instructions)}, "")

	if err := bs.s.Ask(bs.ctx, core.NewUserMessage(blocks...)); err != nil {
		// 入队失败（会话已关 / 队列满）：ack 已经发出、改不了，只能补一条终态事件，
		// 并把配额立刻归还。内部 error 只进审计，事件里给固定文案。
		m.auditSurfaceAgent(id, "agent_run", false, map[string]any{"session": sid, "run": run.id},
			surfaceReasonNoSession+": "+err.Error())
		m.abortSurfaceAgentRun(run, surfaceReasonNoSession, "会话不可用（已关闭或队列已满）")
		return
	}
	bs.lastActive.Store(time.Now().UnixMilli())
	bs.wakeRun() // 与 CmdAsk 同一条路径：run loop 消费队列，dispatch 立即返回
}

// ---- agent.cancel ----

// handleSurfaceAgentCancel 按 run_id + surface 取消（§6.6）。
//
// 语义：幂等。run 已终态或从未存在（上一个进程的 run_id）都回 ok —— 插件取消是
// 「我不想要了」，把「已经没有这个 run」报成错误只会让插件卡在错误分支里。
// 但**跨插件**的 run_id 是 denied：那是一次越权探测，不能回 ok。
func (m *manager) handleSurfaceAgentCancel(c command, id string, args map[string]any) {
	runID := strings.TrimSpace(str(args, "run_id"))
	if runID == "" {
		m.denySurfaceAgent(c, id, surfaceAgentCancelName, surfaceReasonInvalidInput, "run_id 为空", nil)
		return
	}
	hub := m.surfaceAgentHub()
	run, owned := hub.lookup(runID)
	if owned && run.surfaceID != id {
		m.denySurfaceAgent(c, id, surfaceAgentCancelName, surfaceReasonDenied, "run 不属于该插件",
			map[string]any{"run": runID})
		return
	}
	m.resp(c, true, "", map[string]any{})
	m.auditSurfaceAgent(id, "agent_run", true, map[string]any{"run": runID}, "")
	if !owned {
		return // 未知 run_id（旧进程/已清表）：回执已给，无事可做
	}
	m.abortSurfaceAgentRun(run, surfaceReasonCancelled, "")
}

// ---- tool.result ----

// handleSurfaceAgentToolResult 插件回工具结果（§5.1 / §6.4，P2 接真实 waiter）。
//
// 三条分支的处置不同，改之前先读：
//  1. **没有这个 call_id** → 回 ok（幂等）：插件可能重发、也可能是上一个进程的残余。
//     把它报成错误会让插件的错误处理分支永远在重试，而审计里留一行 no_such_call
//     就足以回答「它到底回了没有」（这正是联调时唯一能看的材料）。
//  2. **call_id 存在但 surface 不匹配** → denied 且**不动**那个 waiter：这是一次
//     越权探测（A 插件想替 B 插件回结果），拒掉回执即可，B 的正常结果必须还能到。
//  3. **匹配** → resolve 等待者：摘表 + 投递在 hub 锁内一次完成（见 resolveToolResult），
//     结果文本超 64 KiB 截断并带明确 marker（不静默）。
func (m *manager) handleSurfaceAgentToolResult(c command, id string, args map[string]any) {
	callID := strings.TrimSpace(str(args, "call_id"))
	if callID == "" {
		m.denySurfaceAgent(c, id, surfaceAgentToolResult, surfaceReasonInvalidInput, "call_id 为空", nil)
		return
	}
	ok, hasOK := args["ok"].(bool)
	if !hasOK {
		// 形状不合法才是错误：ok 缺失时我们既不能当成功也不能当失败（后者会把
		// 一条可能正确的插件结果变成错误回执）。
		m.denySurfaceAgent(c, id, surfaceAgentToolResult, surfaceReasonInvalidInput, "ok 必须是布尔值",
			map[string]any{"tool": clipText(callID, 120)})
		return
	}
	out := surfaceToolCallOutcome{ok: ok}
	var text string
	if ok {
		text = pluginToolResultText(args["result"])
		out.text = text
	} else {
		out.err = strings.TrimSpace(str(args, "error"))
		out.reason = surfaceReasonToolError
	}
	status, call := m.surfaceAgentHub().resolveToolResult(callID, id, out)
	bytes := len(text)
	switch status {
	case toolResultNoCall:
		m.resp(c, true, "", map[string]any{})
		// tool 记 call_id：没有 waiter 就问不出工具名，而「哪条回执没接住」只能靠它。
		m.auditSurfaceAgent(id, surfaceAgentAction(surfaceAgentToolResult), false,
			map[string]any{"tool": clipText(callID, 120), "bytes": bytes}, surfaceToolReasonNoSuchCall)
	case toolResultMismatch:
		m.denySurfaceAgent(c, id, surfaceAgentToolResult, surfaceReasonDenied, "该工具调用不属于本插件",
			map[string]any{"tool": clipText(callID, 120)})
	default:
		// 匹配成功**不另写审计行**：模型侧那一行（proxy 工具在调用收尾时写）已经带齐
		// {session, run, tool, bytes, ok}，而且它是「调用结束」的唯一权威时刻。两边都写
		// 等于每次调用记两条 —— 既让「调过几次工具」翻倍，也让审计文件的 flock 多一次
		// 无谓争用（写失败只是丢记录，审计是观测面不是事务，见 surfaces.go 文件头）。
		// 只有**异常**（no_such_call / 跨插件 / 形状不合法）才在这里留痕。
		_ = call
		m.resp(c, true, "", map[string]any{})
	}
}

// ---- 会话归属 ----

// surfaceAgentSessionOwner 该 session_id 属于哪个插件（""=找到但不属于任何插件 = 项目/Hub）。
//
// 查法刻意是「内存 → 磁盘」两段：
//   - 内存：本进程装配过的插件会话（绝大多数场景，含刚 agent.session 建的）；
//   - 磁盘：进程重启后插件会话还没装配 —— 此时 plugin-sessions/<id>/sessions/<sid>.jsonl
//     就是归属证据（与 listSessions 在「已卸载插件」路径上用的是同一份目录约定）。
//
// found=false 才是「不存在」；项目/Hub 会话返回 found=true + owner=""（→ denied，
// 因为插件不许碰真实 Project）。
func (m *manager) surfaceAgentSessionOwner(sid string) (pluginID string, found bool) {
	if sid == "" {
		return "", false
	}
	// 先在 m.mu 下把运行态指针抄出来，**出了锁再查会话**：ws.mu 与 m.mu 是两把锁，
	// 反向嵌套（m.mu → ws.mu）会和装配路径（ws.mu 内回调 m.xxx）撞死锁。
	// 也不走 m.hubRuntime(id) —— 那会在「只是查归属」的路径上装配 Hub 运行时（副作用）。
	m.mu.Lock()
	pluginIDs := make([]string, 0, len(m.pluginRuntimes))
	pluginWS := make([]*workspaceRuntime, 0, len(m.pluginRuntimes))
	for id, ws := range m.pluginRuntimes {
		pluginIDs = append(pluginIDs, id)
		pluginWS = append(pluginWS, ws)
	}
	projectWS := make([]*workspaceRuntime, 0, len(m.workspaces))
	for _, ws := range m.workspaces {
		projectWS = append(projectWS, ws)
	}
	hubWS := make([]*workspaceRuntime, 0, len(m.hubRuntimes))
	for _, ws := range m.hubRuntimes {
		hubWS = append(hubWS, ws)
	}
	m.mu.Unlock()

	hasSession := func(ws *workspaceRuntime) bool {
		if ws == nil {
			return false
		}
		ws.mu.Lock()
		defer ws.mu.Unlock()
		_, ok := ws.sessions[sid]
		return ok
	}
	for i, ws := range pluginWS {
		if hasSession(ws) {
			return pluginIDs[i], true
		}
	}
	for _, ws := range projectWS {
		if hasSession(ws) {
			return "", true // 项目会话：找到但不是插件的（→ denied）
		}
	}
	for _, ws := range hubWS {
		if hasSession(ws) {
			return "", true // Hub 智能体会话：同上
		}
	}
	// 磁盘：plugin-sessions/<id>/sessions/<sid>.jsonl 存在即归属该插件。
	// 进程重启后插件会话还没装配，这条路是唯一的归属证据（目录约定与 listSessions 同源）。
	entries, err := os.ReadDir(pluginSessionsRoot())
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() || !surfaceIDPattern.MatchString(e.Name()) {
			continue
		}
		if _, serr := os.Stat(filepath.Join(pluginSessionsDir(e.Name()), sid+".jsonl")); serr == nil {
			return e.Name(), true
		}
	}
	return "", false
}

// ---- 配额 / run 表（进程级） ----

// surfaceAgentHub 插件 Agent 通道的进程级状态。
//
// 三张表各管一件事：runs（run_id → run，供 cancel 与归属判定）、active（每 surface
// 至多一条在跑 = 并发 1）、recent（每 surface 最近一小时的开始时间 = 20 run/小时）。
// 时间窗用「切片 + 懒裁剪」而不是环形桶：每 surface 每小时 ≤20 条，切片足够，
// 而桶需要额外维护边界。
type surfaceAgentHub struct {
	owner  *manager // 超时回调要回到 manager 的收尾路径
	mu     sync.Mutex
	runs   map[string]*surfaceAgentRun
	active map[string]string
	recent map[string][]time.Time
	// toolCalls 等待中的插件工具调用（call_id → waiter，P2）。懒建：见 addToolCall。
	toolCalls map[string]*surfaceToolCall
	// 以下四个字段供测试注入（零值 = 生产默认）。
	now      func() time.Time
	timeout  time.Duration
	perHour  int
	deltaWin time.Duration
}

// surfaceAgentHub 懒建一次（manager 是字面量构造的，不能依赖构造函数）。
func (m *manager) surfaceAgentHub() *surfaceAgentHub {
	m.surfaceAgentMu.Lock()
	defer m.surfaceAgentMu.Unlock()
	if m.surfaceAgents == nil {
		m.surfaceAgents = &surfaceAgentHub{
			owner:     m,
			runs:      map[string]*surfaceAgentRun{},
			active:    map[string]string{},
			recent:    map[string][]time.Time{},
			toolCalls: map[string]*surfaceToolCall{},
		}
	}
	return m.surfaceAgents
}

func (h *surfaceAgentHub) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

func (h *surfaceAgentHub) runTimeout() time.Duration {
	if h.timeout > 0 {
		return h.timeout
	}
	return surfaceAgentRunTimeout
}

func (h *surfaceAgentHub) maxPerHour() int {
	if h.perHour > 0 {
		return h.perHour
	}
	return surfaceAgentMaxRunsPerHour
}

func (h *surfaceAgentHub) deltaWindow() time.Duration {
	if h.deltaWin > 0 {
		return h.deltaWin
	}
	return surfaceAgentDeltaWindow
}

// reserve 占一次 run：并发 1 + 每小时上限，返回 (nil, reason, msg) 表示超限。
//
// 两个闸门都在**同一个临界区**里判：先判并发再判频率 —— 并发是「现在能不能跑」，
// 频率是「今天还能不能跑」，先报并发对插件更可行动（退避 vs 明天再来）。
func (h *surfaceAgentHub) reserve(surfaceID, sessionID string) (*surfaceAgentRun, string, string) {
	id := surfaceAgentRunID()
	h.mu.Lock()
	now := h.clock()
	if _, busy := h.active[surfaceID]; busy {
		h.mu.Unlock()
		return nil, surfaceReasonQuota, "该插件已有 run 在跑（并发上限 1）"
	}
	cut := now.Add(-time.Hour)
	kept := h.recent[surfaceID][:0]
	for _, t := range h.recent[surfaceID] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	limit := h.maxPerHour()
	if len(kept) >= limit {
		h.recent[surfaceID] = kept
		h.mu.Unlock()
		return nil, surfaceReasonQuota, fmt.Sprintf("一小时内的 run 已达上限（%d）", limit)
	}
	h.recent[surfaceID] = append(kept, now)
	run := &surfaceAgentRun{id: id, surfaceID: surfaceID, sessionID: sessionID, hub: h, startedAt: now}
	h.active[surfaceID] = id
	h.runs[id] = run
	h.pruneLocked(now)
	h.mu.Unlock()
	return run, "", ""
}

// armTimeout 装上单 run 时长上限（§8.3：≤10 min，到点中断并发终态 error{quota}）。
//
// 刻意不在 reserve 里起定时器：那一刻 run 还没有 bs（会话是配额通过后才装配的），
// 定时器提前触发就会 abort 到一个没有会话的 run 上。这里 run 已绑定 bs、但还没挂到
// 会话指针上（下面 Swap），仍然只有一个 writer —— 不需要额外加锁。
func (h *surfaceAgentHub) armTimeout(run *surfaceAgentRun) {
	if run == nil || run.bs == nil {
		return
	}
	timeout := h.runTimeout()
	run.timeout = time.AfterFunc(timeout, func() {
		h.m().abortSurfaceAgentRun(run, surfaceReasonQuota,
			fmt.Sprintf("run 超过时长上限（%s）", timeout))
	})
}

// release 归还并发槽（run 终态：done / error / cancel 都走这里）。
func (h *surfaceAgentHub) release(run *surfaceAgentRun) {
	if run == nil {
		return
	}
	h.mu.Lock()
	if h.active[run.surfaceID] == run.id {
		delete(h.active, run.surfaceID)
	}
	h.mu.Unlock()
	if run.timeout != nil {
		run.timeout.Stop()
	}
}

// lookup 按 run_id 取 run（cancel 用）。已终态的 run 仍留在表里（见 pruneLocked），
// 所以「取消一条刚跑完的 run」能判出归属而不是当成未知 id。
func (h *surfaceAgentHub) lookup(runID string) (*surfaceAgentRun, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	run, ok := h.runs[runID]
	return run, ok
}

// pruneLocked 清掉一小时前终态的 run（保留在表里的唯一目的是 cancel 的归属判定）。
// 上限 512：20 run/小时 × 多插件也不会把这条表撑起来，超了就按最早开始时间淘汰。
func (h *surfaceAgentHub) pruneLocked(now time.Time) {
	if len(h.runs) <= 512 {
		return
	}
	cut := now.Add(-time.Hour)
	for id, run := range h.runs {
		if run.terminal() && run.startedAt.Before(cut) {
			delete(h.runs, id)
		}
	}
}

// m 取回 manager（run 的 timeout 回调与 emit 侧都要用）。
func (h *surfaceAgentHub) m() *manager { return h.owner }

// ---- 一次 run ----

// surfaceAgentRun 一次插件 run 的投递状态。
//
// 并发的两个来源：emit（run goroutine，持 bs.dispatchMu）与 timer/cancel
// （dispatch goroutine，先拿 bs.dispatchMu 再进来）。字段用 mu 保护，seq 的自增
// 与 closed 的置位必须原子完成 —— 终态之后到达的任何事件都不许再投。
type surfaceAgentRun struct {
	id        string
	surfaceID string
	sessionID string
	bs        *bridgeSession
	hub       *surfaceAgentHub

	mu        sync.Mutex
	seq       int
	closed    bool
	rootRunID string
	pending   strings.Builder
	deltaTmr  *time.Timer
	inTok     int64
	outTok    int64
	startedAt time.Time

	timeout *time.Timer
}

// surfaceAgentRunID 生成 run_id。前缀 sr- 让它与 session id（s-）/任务 id（task-）
// 一眼可分 —— 日志里三种 id 混在一起时，前缀是唯一的符号线索。
// 纳秒 + 进程内单调序号：同一纳秒内两次 reserve（并发闸门放行后的极端情况）也不会撞号。
func surfaceAgentRunID() string {
	n := surfaceAgentRunSeq.Add(1)
	return fmt.Sprintf("sr-%d-%d", time.Now().UnixNano(), n)
}

var surfaceAgentRunSeq atomic.Int64

func (r *surfaceAgentRun) terminal() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// observe 是 emit 的插件侧钩子：只认**根 run** 的事件（子 agent 有自己的 RunId，
// 插件看不到别人的流水，子 agent 也不例外）。
//
// 调用者必须已持 bs.dispatchMu（emit 内），所以这里的 send 可以直接写 stdout。
func (r *surfaceAgentRun) observe(e events.Event) {
	switch ev := e.(type) {
	case *events.AgentStart:
		if ev.ParentRunId != "" || ev.Depth != 0 {
			return
		}
		r.mu.Lock()
		if r.rootRunID == "" {
			r.rootRunID = ev.RunId
		}
		r.mu.Unlock()
	case *events.ContentChunk:
		if !r.isRoot(ev.RunId) {
			return
		}
		r.feedDelta(ev.Content)
	case *events.LLMEnd:
		if !r.isRoot(ev.RunId) || ev.Usage == nil {
			return
		}
		r.addUsage(ev.Usage.Input, ev.Usage.Output)
	case *events.ToolStart:
		if !r.isRoot(ev.RunId) {
			return
		}
		// 只给名字与状态（§6.6）：参数/结果属于会话正文，不给插件。
		r.send(surfaceAgentEvTool, map[string]any{"name": ev.Name, "status": "start"})
	case *events.ToolResponse:
		if !r.isRoot(ev.RunId) {
			return
		}
		r.send(surfaceAgentEvTool, map[string]any{"name": ev.Name, "status": "done", "ok": !ev.IsError})
	case *events.AgentEnd:
		if ev.ParentRunId != "" || ev.Depth != 0 {
			return
		}
		r.finish(ev)
	}
}

// isRoot 事件是否属于本次 run 的根运行（AgentStart 未到时按根处理：ContentChunk
// 只会在 AgentStart 之后出现，这条分支只是防御）。
func (r *surfaceAgentRun) isRoot(runID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rootRunID == "" {
		return true
	}
	return runID == r.rootRunID
}

// feedDelta 累积增量并启动 50ms 合并窗口（不立即写 stdout）。
func (r *surfaceAgentRun) feedDelta(text string) {
	if text == "" {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.pending.WriteString(text)
	if r.deltaTmr == nil {
		r.deltaTmr = time.AfterFunc(r.hub.deltaWindow(), r.flushDelta)
	}
	r.mu.Unlock()
}

// flushDelta 合并窗口到点：拿 dispatchMu（与 emit 同一把锁 = 同序）后写出。
func (r *surfaceAgentRun) flushDelta() {
	bs := r.bs
	bs.dispatchMu.Lock()
	defer bs.dispatchMu.Unlock()
	r.mu.Lock()
	r.deltaTmr = nil
	text := r.takePendingLocked()
	r.mu.Unlock()
	if text == "" {
		return
	}
	r.send(surfaceAgentEvDelta, map[string]any{"text": text})
}

// takePendingLocked 取走合并缓冲（调用方持 r.mu）。
func (r *surfaceAgentRun) takePendingLocked() string {
	if r.pending.Len() == 0 {
		return ""
	}
	text := r.pending.String()
	r.pending.Reset()
	return text
}

func (r *surfaceAgentRun) addUsage(in, out int64) {
	r.mu.Lock()
	r.inTok += in
	r.outTok += out
	r.mu.Unlock()
}

// finish 根 AgentEnd → 终态：先补投最后一窗 delta，再 usage，再 done/error。
//
// 顺序是协议的一部分（前端按「delta … usage … done」重建气泡）：done 之后停投，
// 所以最后一段 delta 必须在这里补出去，否则末尾几个字永远看不到。
func (r *surfaceAgentRun) finish(ae *events.AgentEnd) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	text := r.takePendingLocked()
	if r.deltaTmr != nil {
		r.deltaTmr.Stop()
		r.deltaTmr = nil
	}
	in, out := r.inTok, r.outTok
	r.mu.Unlock()
	// 终态之后不会再有工具结果回来：一次性唤醒这条 run 的等待者，让工具 Call 立刻
	// 返回（reason=cancelled，语义同「取消即停」）而不是等满 manifest 的 deadline。
	r.failToolCalls(surfaceReasonCancelled, "run 已结束，工具结果不会再回来")
	if ae.Usage != nil {
		in, out = ae.Usage.Input, ae.Usage.Output
	}
	if text != "" {
		r.sendTerminal(surfaceAgentEvDelta, map[string]any{"text": text})
	}
	if ae.FinishReason == core.FinishReasonError || ae.Error != "" {
		msg := ae.Error
		if msg == "" {
			msg = "run failed"
		}
		r.sendTerminal(surfaceAgentEvError, map[string]any{"reason": surfaceReasonBridgeError, "message": clipText(msg, 300)})
	} else {
		r.sendTerminal(surfaceAgentEvUsage, map[string]any{"input_tokens": in, "output_tokens": out})
		r.sendTerminal(surfaceAgentEvDone, map[string]any{"text": ae.Content})
	}
	r.hub.release(r)
	r.bs.surfaceAgentRun.CompareAndSwap(r, nil)
}

// send 投一条非终态事件（closed 后静默丢弃 = 终态停投）。
// 调用方必须已持 bs.dispatchMu。
func (r *surfaceAgentRun) send(typ string, data map[string]any) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.seq++
	seq := r.seq
	r.mu.Unlock()
	r.writeEvent(typ, seq, data)
}

// sendTerminal 投一条终态事件（closed 已置位，seq 照常单调）。
func (r *surfaceAgentRun) sendTerminal(typ string, data map[string]any) {
	r.mu.Lock()
	r.seq++
	seq := r.seq
	r.mu.Unlock()
	r.writeEvent(typ, seq, data)
}

// writeEvent 统一信封（§6.6 逐字形状）：
//
//	{"event_type":"surface_agent_event","data":{"surface_id":"…","event":{type,seq,run_id,session_id,data}}}
func (r *surfaceAgentRun) writeEvent(typ string, seq int, data map[string]any) {
	if r.bs == nil || r.bs.out == nil {
		return
	}
	r.bs.out.writeLine(map[string]any{
		"event_type": surfaceAgentEventType,
		"data": map[string]any{
			"surface_id": r.surfaceID,
			"event": map[string]any{
				"type":       typ,
				"seq":        seq,
				"run_id":     r.id,
				"session_id": r.sessionID,
				"data":       data,
			},
		},
	})
}

// failToolCalls 让本条 run 的等待者一次性失败（终态清理的唯一入口）。
func (r *surfaceAgentRun) failToolCalls(reason, message string) int {
	if r == nil || r.hub == nil {
		return 0
	}
	return r.hub.failToolCalls(
		func(c *surfaceToolCall) bool { return c.runID == r.id },
		surfaceToolCallOutcome{err: message, reason: reason})
}

// failSurfaceAgentToolCalls 让某个插件的全部等待者失败（卸载 / 停用路径）。
//
// 与 run 终态失败的区别只在判据（surface 而非 run）：插件没了，它名下所有 run 的
// 等待调用都不可能再有结果。**不**懒建 hub：没有 hub 就等于没有等待者。
func (m *manager) failSurfaceAgentToolCalls(surfaceID, reason, message string) int {
	if m == nil || surfaceID == "" {
		return 0
	}
	m.surfaceAgentMu.Lock()
	hub := m.surfaceAgents
	m.surfaceAgentMu.Unlock()
	if hub == nil {
		return 0
	}
	return hub.failToolCalls(
		func(c *surfaceToolCall) bool { return c.surfaceID == surfaceID },
		surfaceToolCallOutcome{err: message, reason: reason})
}

// abortSurfaceAgentRun 取消/超时的统一收尾：投终态 error（**不发 done**）→ 归还配额 →
// 中断会话当前 run。
//
// 为什么在取消时立刻发终态而不是等 AgentEnd：AgentEnd 依赖 SDK 真的把 abort 走出来
// （工具阻塞、provider 不响应时可能很久），而插件侧「点了取消还在收 delta」是最难解释
// 的行为。这里先钉住终态（closed 之后 emit 来的事件一律丢弃），再让 abort 慢慢收尾。
func (m *manager) abortSurfaceAgentRun(r *surfaceAgentRun, reason, message string) {
	bs := r.bs
	bs.dispatchMu.Lock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		bs.dispatchMu.Unlock()
		return
	}
	r.closed = true
	text := r.takePendingLocked()
	if r.deltaTmr != nil {
		r.deltaTmr.Stop()
		r.deltaTmr = nil
	}
	r.mu.Unlock()
	// 取消/超时：等待中的工具调用立即失败（§8.4「取消即停」）。放在发终态事件之前 ——
	// 模型侧的工具 Call 越早返回，run 的收尾越早开始。
	r.failToolCalls(surfaceReasonCancelled, "run 已取消，工具结果不会再回来")
	if text != "" {
		r.sendTerminal(surfaceAgentEvDelta, map[string]any{"text": text})
	}
	data := map[string]any{"reason": reason}
	if message != "" {
		data["message"] = clipText(message, 300)
	}
	r.sendTerminal(surfaceAgentEvError, data)
	r.hub.release(r)
	bs.surfaceAgentRun.CompareAndSwap(r, nil)
	bs.dispatchMu.Unlock()
	// 中断放在锁外：Interrupt 只 abort 运行中的 ctx，但 emit 侧可能正拿 dispatchMu 写
	// 最后几行事件，锁内等待只会把取消路径拖成死等（事件已经在这之前停投了）。
	bs.s.Interrupt()
}

// observeSurfaceAgentEvent emit 的钩子（见 main.go emit 内调用点）。
func (bs *bridgeSession) observeSurfaceAgentEvent(e events.Event) {
	run := bs.surfaceAgentRun.Load()
	if run == nil {
		return
	}
	run.observe(e)
}
