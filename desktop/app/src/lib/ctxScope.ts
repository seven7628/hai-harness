// 「这个读数属于谁」—— composer 的 ctx 环、状态栏、压缩确认都显示「当前上下文」，
// 而「当前」有两层：**会话**（主 run）与**正在查看的那个 run**（左栏树选中子智能体时）。
//
// 用户 2026-10 报障：进到子智能体的视角里，上下文窗口大小（分母）还是主智能体的。
// 原因有两处，缺一不可修：
//   ① 分母：窗口只在 agent_start 上报，旧实现把它写进**会话级**那一个字段 —— 主 run 与
//      子 run 抢同一个格子；子 run 若跑在别的模型上（窗口不同），子视角的分母就是错的。
//      现在每个 run 自己带一份（RunNode/agent 块上的 ctxWindow，见 store 的映射）。
//   ② 分子与读数：composer 直接读顶层镜像 s.ctxTokens（= 会话的），聚焦子 agent 时
//      不知道要看谁。这里按 focusedAgent 选源。
//
// 数据都已在客户端：turns 带 runId，agent 块带 usage。没有新协议字段。

export interface CtxScopeState {
  focusedAgent?: { runId: string; nodeId?: string; taskId?: string }
  ctxTokens?: number
  ctxTokensEstimated?: boolean
  contextWindow?: number
  turns: { runId?: string; input?: number; durMs?: number }[]
  runNodes: { id: string; taskId?: string; ctxWindow?: number }[]
  blocks: { kind: string; runId?: string; taskId?: string; spawnedAt?: number; ctxWindow?: number; usage?: { input?: number } }[]
}

export interface RunCtx {
  /** 该 run 当前上下文占用（最近一轮 LLM 调用的输入 token） */
  tokens?: number
  /** 该 run 自己的窗口；它没报过就退回会话级（宁可少一个事实，也不要报 0 分母） */
  window?: number
  /** 该 run 最近一轮的耗时（面板里「本轮」那一行的来源） */
  turnDurMs?: number
}

/** 聚焦中的子 run 的上下文读数；未聚焦（看主会话）时返回 undefined。 */
export function runCtxOf(s: CtxScopeState): RunCtx | undefined {
  const f = s.focusedAgent
  if (!f) return undefined
  // 焦点是点选时的快照；agent_resume 后同一 task 换 run_id → 按 taskId 找最新卡，
  // 否则环里的分子/窗口仍停在上一轮（用户 2026-11 报障的「续跑后状态不对」同源）。
  const taskKey = f.taskId || f.nodeId
  let currentRunId = f.runId
  let latestAt = Number.NEGATIVE_INFINITY
  for (const b of s.blocks) {
    if (b.kind !== 'agent') continue
    if (!(b.taskId && b.taskId === taskKey) && b.runId !== f.runId && b.runId !== f.nodeId) continue
    const at = b.spawnedAt ?? Number.NEGATIVE_INFINITY
    if (at >= latestAt) {
      latestAt = at
      if (b.runId) currentRunId = b.runId
    }
  }
  if (currentRunId === f.runId) {
    const node = s.runNodes.find((n) => (taskKey && n.taskId === taskKey) || n.id === f.runId)
    if (node) currentRunId = node.id
  }
  const out: RunCtx = {}
  for (let i = s.turns.length - 1; i >= 0; i--) {
    const t = s.turns[i]
    if (t.runId !== currentRunId) continue
    if (typeof t.input === 'number' && t.input > 0) out.tokens = t.input
    if (t.durMs != null) out.turnDurMs = t.durMs
    break
  }
  const node = s.runNodes.find((n) => n.id === currentRunId)
  const block = s.blocks.find((b) => b.kind === 'agent' && b.runId === currentRunId)
  const w = node?.ctxWindow ?? (block as { ctxWindow?: number } | undefined)?.ctxWindow
  if (w != null && w > 0) out.window = w
  // 一个 turn 都还没落地（子 agent 刚起）：退回它这一路已累计的输入
  if (out.tokens == null && block?.usage && typeof block.usage.input === 'number' && block.usage.input > 0) out.tokens = block.usage.input
  return out
}

/** 环/状态栏的分子：聚焦时用那个 run 的，否则会话的。 */
export function ctxTokensOf(s: CtxScopeState): number | undefined {
  const r = runCtxOf(s)
  return r ? r.tokens : s.ctxTokens
}

/** 分母：优先该 run 自己上报的窗口，其次会话级（再往下是模型表，见 modelWindowOf）。 */
export function ctxWindowOf(s: CtxScopeState): number | undefined {
  const r = runCtxOf(s)
  return r?.window ?? s.contextWindow
}

/** 读数口径：会话级才可能是「压后估算」（子 run 的 token 来自真实 usage）。 */
export function ctxEstimatedOf(s: CtxScopeState): boolean {
  return s.focusedAgent ? false : s.ctxTokensEstimated === true
}
