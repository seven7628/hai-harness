import type { MsgBlock, SessionView } from './useAppStore'

// 视图内存预算（2026-11-12，P1）。
//
// ## 为什么
//
// 渲染进程的 footprint 实测 **2.08 GB（峰值 3.13 GB）**，而 V8 的堆上限是
// **崩溃线**不是降级线（v8.dev/blog/heap-size-limit：到顶后 V8 做 "a series of
// last resort garbage collections"，再不行就 "stops execution and reports an
// out-of-memory failure" ⇒ Electron 里就是 Aw, Snap）。
// 我们**没有**设 `--max-old-space-size` ⇒ 用的是 Chromium 推算的默认值。
//
// ⇒ 渲染进程的内存必须**我们自己管**，不能让 V8 兜底。
//
// ## 抄谁
//
// **Redis 的 eviction 模型**（redis.io/docs/latest/develop/reference/eviction/）：
//
//   · `maxmemory` = **字节**预算（不是个数）
//   · 淘汰发生在**写入时**（Redis: "Whenever a client runs a new command that
//     adds more data to the cache, Redis checks the memory usage… until the
//     total memory used is back below the limit"）
//   · **分级**：`volatile-lru` 只丢带 TTL 的，`allkeys-lru` 丢任何
//   · `noeviction` = **拒绝写入**，而不是丢已有数据
//
// **Chromium 的 tab discarding**（chromiumos-design-docs）：
// 内存压力下丢弃标签页的**可重取状态**，保留用户正在交互的东西，回来时 reload。
//
// ## 三类数据
//
//   · **A 可重取**：blocks / trace / diffs / turns / git —— bridge 能再给
//   · **B 可重放**：正在流式输出的正文 —— 服务端事件流能重放
//   · **C 不可丢**：未发送的输入草稿、**审批中的 diff**、模型在等回答的问题
//
// ⇒ 超预算时**只动 A 类**；C 类永不丢（Redis 的 `noeviction` 语义：
// 宁可拒绝新的缓存写入，也不丢用户数据）。

// —— 预算常量 ——

/** 常态预算（字节）。对齐 Redis `maxmemory` 的角色：固定值，不跟机器内存挂钩
 *  （应用在 16 GB 和 64 GB 机器上手感应该一样）。 */
export const VIEW_BUDGET_BYTES = 256 * 1024 * 1024

/** 机器在换页颠簸时的收紧预算：降到一半，逼出更多空间。 */
export const VIEW_BUDGET_PRESSURE_BYTES = 128 * 1024 * 1024

/** 降级后保留的**尾部**块数（保留最近的对话 + 正在流式输出的内容）。 */
export const VIEW_KEEP_BLOCKS = 200

/** 降级后保留的尾部「窗口外卡片」数（左栏树用的子智能体/后台任务卡）。
 *
 *  这些卡片是**渲染进程内存的大头**：bridge 的 `detachAgents` 对「有 task_id 的」
 *  只发骨架，但对「没有 task_id 的同步子智能体」**整份带上 items**
 *  （实测单卡 100–185KB，见 desktop/bridge/view_reducer.go 的 detachAgents 注释）
 *  ⇒ 一个 1600 张卡的会话，new_session 一次就能带回上百 MB。 */
export const VIEW_KEEP_AGENT_CARDS = 50

/** 降级后保留的尾部轮次数。 */
export const VIEW_KEEP_TURNS = 50

/** 预算检查的最小间隔（毫秒）。视图有 4 万块时，一次全量估价要几毫秒；
 *  每个事件都查太浪费（Redis 的检查是 O(1) 的，我们的不是）。 */
export const BUDGET_CHECK_INTERVAL_MS = 1000

// —— 成本估算 ——

/** 每块的估算字节缓存。
 *
 *  依赖一条不变量：**块对象不被原地修改**（更新一律走 `v.blocks.map(b => ({...b, ...}))`
 *  生成新对象）。核实过 useAppStore.ts 里没有 `.text +=` / `.result +=` 这类原地写。
 *  一旦这条被打破，缓存会读到旧值 —— 只影响降级时机，不影响正确性。 */
const blockSizeCache = new WeakMap<object, number>()

/** roughSize 的深度上限：再深的结构按固定值算（避免环与深树）。 */
const MAX_DEPTH = 8

/** roughSize 单次遍历的节点上限：超过就按剩余量粗估（防病态结构拖慢估算本身）。 */
const MAX_NODES = 20000

/** 粗略估算一个值的字节数。
 *
 *  JS 字符串是 UTF-16 ⇒ 按 2 字节/字符算（base64 图片尤其吃亏：它们是字符串）。
 *  `String.length` 是 O(1) 的属性读 ⇒ 即使是 2 MB 的 base64，估算也只是一次读。
 */
export function roughSize(v: unknown, depth = 0, budget = { left: MAX_NODES }): number {
  if (v === null || v === undefined) return 0
  const t = typeof v
  if (t === 'string') return (v as string).length * 2
  if (t === 'number') return 8
  if (t === 'boolean') return 4
  if (t !== 'object') return 0
  if (depth >= MAX_DEPTH) return 64
  if (budget.left <= 0) return 64
  const o = v as Record<string, unknown>
  if (Array.isArray(o)) {
    let n = 32
    for (const x of o) {
      budget.left--
      n += 16 + roughSize(x, depth + 1, budget)
      if (budget.left <= 0) return n + 64 * Math.max(0, o.length - 1)
    }
    return n
  }
  let n = 48
  for (const k in o) {
    budget.left--
    n += k.length * 2 + 16 + roughSize(o[k], depth + 1, budget)
    if (budget.left <= 0) break
  }
  return n
}

/** 一个块的估算字节（带缓存）。 */
export function blockBytes(b: MsgBlock): number {
  let n = blockSizeCache.get(b)
  if (n === undefined) {
    n = roughSize(b)
    blockSizeCache.set(b, n)
  }
  return n
}

/** 一个视图的估算字节。 */
export function viewBytes(v: SessionView): number {
  let n = 0
  for (const b of v.blocks) n += blockBytes(b)
  for (const b of v.agentDetached ?? []) n += blockBytes(b)
  // 其余字段一起估：trace/diff/turn/git 各自有上限，量级可控。
  n += roughSize({
    traceEvents: v.traceEvents,
    traceList: v.traceList,
    traceDetail: v.traceDetail,
    traceFallback: v.traceFallback,
    diffs: v.diffs,
    turns: v.turns,
    gitFiles: v.gitFiles,
    gitHistory: v.gitHistory,
    todos: v.todos,
    approvals: v.approvals,
    questions: v.questions,
    pendingQueue: v.pendingQueue,
  })
  return n
}

/** 降级后这个视图还剩多少字节。 */
export function degradedBytes(v: SessionView): number {
  const blocks = v.blocks.length > VIEW_KEEP_BLOCKS ? v.blocks.slice(-VIEW_KEEP_BLOCKS) : v.blocks
  const cards = (v.agentDetached ?? []).slice(-VIEW_KEEP_AGENT_CARDS)
  let n = 0
  for (const b of blocks) n += blockBytes(b)
  for (const b of cards) n += blockBytes(b)
  n += roughSize({
    turns: v.turns.slice(-VIEW_KEEP_TURNS),
    todos: v.todos,
    approvals: v.approvals,
    questions: v.questions,
    pendingQueue: v.pendingQueue,
    runNodes: v.runNodes,
  })
  return n
}

// —— C 类保护 ——

/** 这个视图里有没有「用户正在交互、丢了就是丢数据」的东西。
 *
 *  · `approvals`   —— 审批中的 diff（用户正要决定）
 *  · `questions`   —— 模型在等用户回答（ask_user）
 *  · `release`     —— 沙箱放行确认
 *  · `pendingQueue`—— 用户输入的暂存队列（还没推送出去）
 *
 *  这些视图**既不降级也不淘汰**：降级会丢用户数据，淘汰会让正在等回答的会话
 *  永远等不到（agent 挂死）。
 */
export function viewHasPendingInteraction(v: SessionView): boolean {
  return (
    (v.approvals?.length ?? 0) > 0 ||
    (v.questions?.length ?? 0) > 0 ||
    !!v.release ||
    (v.pendingQueue?.length ?? 0) > 0
  )
}

// —— 降级 ——

/** 把一个视图降级成「轻量形态」：丢掉 A 类，保留 C 类与界面状态。
 *
 *  **降级不是清空**：blocks 裁到**尾部** VIEW_KEEP_BLOCKS 个
 *  —— 保留最近的对话，也保住正在流式输出的内容（B 类）。
 *
 *  **关键**：`hydrated` 置 false —— 切回这个会话时前端会重新向 bridge 要
 *  `new_session`（isHydrated 判据见 useAppStore.ts）。不置 false 的话，
 *  切回去会直接渲染一个空壳 ⇒ 用户看到「历史凭空消失」。
 */
export function degradeView(v: SessionView): SessionView {
  const trimmed = v.blocks.length > VIEW_KEEP_BLOCKS
  const blocks = trimmed ? v.blocks.slice(-VIEW_KEEP_BLOCKS) : v.blocks
  const cards = (v.agentDetached ?? []).slice(-VIEW_KEEP_AGENT_CARDS)
  return {
    ...v,
    blocks,
    agentDetached: cards,
    // 裁掉的是**更早**的块 ⇒ 游标跟着新的最早块走，「加载更早」仍能取回它们。
    blocksCursor: trimmed ? blocks[0]?.id : v.blocksCursor,
    blocksHasMore: trimmed ? true : v.blocksHasMore,
    blocksTotal: v.blocksTotal ?? v.blocks.length,
    blocksOlderLoading: false,
    blocksOlderError: undefined,
    // —— A 类：可重取 ——
    traceEvents: [],
    traceList: undefined,
    traceListTotal: undefined,
    traceHasMore: undefined,
    traceDetail: undefined,
    traceFallback: undefined,
    diffs: [],
    turns: v.turns.length > VIEW_KEEP_TURNS ? v.turns.slice(-VIEW_KEEP_TURNS) : v.turns,
    gitFiles: [],
    gitHistory: [],
    gitHasMore: undefined,
    gitHistLoading: false,
    // 降级后不再是「已恢复」⇒ 切回去重新向 bridge 要数据（否则渲染空壳）。
    hydrated: false,
    // —— 保留：approvals / questions / release / pendingQueue / todos /
    //         usage / costUsd / ctxTokens / runNodes / streaming / runEnded /
    //         focusedAgent / 右侧栏状态 / 压缩状态 …—
  }
}

// —— 内存压力信号 ——

export interface MemPressure {
  /** 物理内存空闲（KB）。 */
  free: number
  /** 物理内存总量（KB）。 */
  total: number
  /** swap 空闲（KB）。 */
  swapFree: number
  /** swap 总量（KB）。 */
  swapTotal: number
  /** 采样时刻（epoch ms）。 */
  at: number
}

let lastPressure: MemPressure | undefined

/** 记录一次系统内存采样（主进程轮询 getSystemMemoryInfo 推过来）。 */
export function setMemPressure(p: MemPressure): void {
  lastPressure = p
}

/** 最近一次采样（测试用）。 */
export function getMemPressure(): MemPressure | undefined {
  return lastPressure
}

/** 机器是不是在换页颠簸。
 *
 *  判据（两个都满足才算）：swap 用掉一半以上 **且** 物理内存空闲不足 15%。
 *  swap 满但物理内存还够 = 系统在慢慢换页，不算颠簸（这时降级属于过度反应）。
 */
export function memPressureActive(): boolean {
  const p = lastPressure
  if (!p || !p.total || !p.swapTotal) return false
  const swapUsed = p.swapTotal - p.swapFree
  const freeRatio = p.free / p.total
  return swapUsed / p.swapTotal > 0.5 && freeRatio < 0.15
}

/** 当前生效的预算（颠簸时收紧）。 */
export function currentBudgetBytes(): number {
  return memPressureActive() ? VIEW_BUDGET_PRESSURE_BYTES : VIEW_BUDGET_BYTES
}

// —— 预算处置计划 ——

export interface BudgetPlan {
  /** 要降级（裁块尾）的会话 id。 */
  degrade: string[]
  /** 要整体淘汰的会话 id。 */
  evict: string[]
  /** 处置后估算还剩多少字节。 */
  remaining: number
}

/**
 * 算出超预算时该怎么处置。
 *
 * 分级（Redis 的 volatile→allkeys 思路 + Chromium 的降级思路）：
 *
 *  1. **降级**可降级的视图（LRU 优先）—— 裁块尾，保住最近内容
 *  2. 还超 → **整体淘汰**可淘汰的视图（LRU 优先）
 *  3. 还超 → 降级**其余**视图（含 active 与运行中的；C 类仍然不动）
 *
 * 两级保护，不一样：
 *   · **不可淘汰**：active + 运行中的（`protect` 回调）+ C 类
 *     —— 淘汰运行中的会话 = 它的实时 UI 直接坏掉
 *   · **不可降级**：C 类（有待办交互）—— 降级会丢用户数据
 */
export function planBudget(opts: {
  views: Record<string, SessionView>
  /** LRU 顺序：最久未看的在前，最近的在后。 */
  order: string[]
  active?: string
  budget: number
  /** 额外的「不可淘汰」判据（调用方传 sessionBusy）。 */
  protect?: (sid: string, v: SessionView) => boolean
}): BudgetPlan {
  const plan: BudgetPlan = { degrade: [], evict: [], remaining: 0 }
  const { views, order, budget } = opts
  let total = 0
  for (const v of Object.values(views)) total += viewBytes(v)
  plan.remaining = total
  if (total <= budget) return plan

  const noEvict = new Set<string>()
  if (opts.active) noEvict.add(opts.active)
  for (const [sid, v] of Object.entries(views)) {
    if (viewHasPendingInteraction(v)) noEvict.add(sid)
    if (opts.protect?.(sid, v)) noEvict.add(sid)
  }

  const cands = order.filter((sid) => views[sid] && !noEvict.has(sid))
  const rest = Object.keys(views).filter((sid) => !cands.includes(sid))

  // ① 降级（LRU 优先）
  for (const sid of cands) {
    if (total <= budget) break
    const v = views[sid]
    const after = total - viewBytes(v) + degradedBytes(v)
    if (after < total) {
      plan.degrade.push(sid)
      total = after
    }
  }
  // ② 整体淘汰（LRU 优先；已降级的先淘汰，因为它们剩下的价值最小）
  const evictOrder = [...plan.degrade, ...cands.filter((s) => !plan.degrade.includes(s))]
  for (const sid of evictOrder) {
    if (total <= budget) break
    if (plan.evict.includes(sid)) continue
    const v = views[sid]
    const after = total - degradedBytes(v)
    if (after < total) {
      plan.evict.push(sid)
      total = after
    }
  }
  // ③ 还超 → 降级其余（active / 运行中的也降级；但 C 类不动）
  for (const sid of rest) {
    if (total <= budget) break
    if (plan.degrade.includes(sid) || plan.evict.includes(sid)) continue
    const v = views[sid]
    if (viewHasPendingInteraction(v)) continue
    const after = total - viewBytes(v) + degradedBytes(v)
    if (after < total) {
      plan.degrade.push(sid)
      total = after
    }
  }
  plan.remaining = total
  return plan
}
