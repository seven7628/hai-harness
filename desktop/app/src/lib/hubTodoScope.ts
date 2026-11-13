// Project 会话待办在 Hub 单聊/群里的**纯投影**（可单测；store 与内联卡共用同一份规则）。
//
// 背景（场景2）：Project 任务派出去之后，Hub 里只能看到「已派发/运行中」，看不到它
// 正在做什么。线上事件里 `tool_run_end` 自带 `todos` 快照（属主 = **发出该调用的 run**，
// 见 events.ToolResponse.Todos），但 Hub store 原先在 `tool_run_start/tool_run_end`
// 第一行就按当前 DM 的 sid 过滤，Project 会话的事件整条被丢掉。这里把「怎么收、认领给
// 哪条任务、怎么显示」拆成纯函数，store 只负责把事件喂进来：
//
//   收（mergeTodoSnapshot）   键 = (session, run)；后到覆盖先到；`todos` 字段**缺失**
//                            = 这条工具没有快照（不改），`[]`/`null` = 清单被清空（必须生效）。
//   认领（resolveTaskForTool）run 的事件归属于哪条 hub_task（只看 target.kind=workspace）。
//   显示（activeTodos）       只显示作用域内**在跑**的任务（dispatched/running）；终态立刻
//                            收卡；同一任务只出一张卡（最新 run），最多 3 张。
//   命名（resolveRunName）    主 run = 「主会话」，子 run = agent_start.name，
//                            未知 run = `run_<前 8 位>`（**绝不**回退「主会话」）。
//
// 三条硬性约束（写在这里而不是散在调用点，因为它们最容易被"顺手统一"掉）：
//   1. Hub 自己的会话（`hub.<agent>.dm` / `hub.<agent>.g.<group>`）一个字节都不记 ——
//      那里的清单已经在转录/右栏待办面板里，内联卡再显示一次是重复。
//   2. 未知 run 不回退「主会话」：把「不知道是谁的计划」显示成「主会话的计划」是更难
//      发现的错（同 lib/todoScope.ts 的第 2 条约束）。
//   3. 只读：本模块不产出任何 hub_message / refs —— 待办卡绝不进群快照与智能体上下文。
// 只做**类型**导入：编译期擦除，不产生 lib → store/hub 的运行时依赖（todoScope.ts 同一约定）。
import type { HubTask } from '../hub/types'
import type { TodoItem } from '../store/useAppStore'

/** 内联卡的作用域（DM 与群同一套口径，与 HubInlineNeeds.needScopeMatch 对齐）。 */
export type HubInlineScope =
  | { kind: 'dm'; agentId: string | null }
  | { kind: 'group'; groupId: string }

/** `tool_run_end` 里与本模块相关的字段（事件是 JSON，全部可选）。 */
export interface HubTodoEvent {
  session_id?: string
  workspace?: string
  run_id?: string
  /** 事件自带时间戳（ISO）：迟到的旧事件不能覆盖新快照。 */
  timestamp?: string
  /** 事件序（Hub 消息事件才有；有则同样按「旧不覆盖新」处理）。 */
  seq?: number
  /** 缺字段 = 无快照（不改）；null/[] = 清单被清空（必须生效）。 */
  todos?: unknown
  /** 到达序（store 自增）。纯函数不读时钟：缺省时用 prev.at + 1。
   *  刻意不叫 `at`：Hub 的告警事件顶层已有 `at`（ISO 时间串），同名会让
   *  `applyTodoSnapshot(ev)` 的类型不兼容（也容易把两者搞混）。 */
  arrival?: number
}

/** 一条 run 的待办快照（table 的 value；key 见 hubRunKey）。 */
export interface HubTodoSnapshot {
  key: string
  sessionId: string
  runId: string
  workspace: string
  todos: TodoItem[]
  /** 事件自带时间戳（ISO）；空 = 事件没带。 */
  ts: string
  /** 事件序；0 = 没有。 */
  seq: number
  /** 到达序：同键后到胜出、LRU 淘汰都看它。 */
  at: number
}

/** run → 展示名（agent_start 维护；root = 这条 run 是会话的主 run）。 */
export interface HubRunName {
  name?: string
  root?: boolean
}
export type HubRunNames = Record<string, HubRunName>

/** agent_start 里 run 名表需要的字段。 */
export interface HubRunNameEvent {
  session_id?: string
  run_id?: string
  name?: string
  parent_run_id?: string
  depth?: number
}

/** 每张卡最多列几条（其余折成 +N）。 */
export const HUB_TODO_MAX_ITEMS = 3
/** 最多几张卡（同一任务一张）。 */
export const HUB_TODO_MAX_CARDS = 3
/** 快照表按 run 计的上限（LRU，至少 200）。 */
export const HUB_TODO_MAX_RUNS = 200
/** run 名表上限（agent_start 可能很多：子智能体、后台任务、压缩 run）。 */
export const HUB_RUN_NAME_MAX = 400

const LIVE_STATUS: Record<string, true> = { dispatched: true, running: true }
const TERMINAL_STATUS: Record<string, true> = {
  completed: true, failed: true, interrupted: true, delivered: true,
  canceled: true, blocked_budget: true, declined: true,
}

function str(v: unknown): string {
  return typeof v === 'string' ? v.trim() : ''
}

/** 任务是否在跑（只有这两种状态会出内联卡；queued 还没派发、终态已收口）。 */
export function isLiveTaskStatus(status: string | undefined | null): boolean {
  return !!status && LIVE_STATUS[status] === true
}

/** 任务是否终态（与 agenthub.TaskStatus.Terminal 同口径）。 */
export function isTerminalTaskStatus(status: string | undefined | null): boolean {
  return !!status && TERMINAL_STATUS[status] === true
}

/**
 * 是不是 Project 会话（能带待办的会话）。
 *
 * 只排除 Hub 自己的会话前缀：Hub DM/群会话的清单已经在 Hub 自己的 UI 里，再收一遍会
 * 让内联卡与转录同源重复；其余（`s_...` 及旧/测试里的普通 id）都按 Project 处理。
 */
export function isProjectSession(sessionId: string | undefined | null): boolean {
  const sid = str(sessionId)
  return sid !== '' && !sid.startsWith('hub.')
}

/** 快照键：session 与 run 都必须参与（不同会话的 run id 理论上也可能撞）。 */
export function hubRunKey(sessionId: string | undefined | null, runId: string | undefined | null): string {
  return `${str(sessionId)}\u0000${str(runId)}`
}

/**
 * 作用域判定：这条任务该出现在哪个 DM / 群里。
 *
 * 群：`origin.group_id === activeGroupId`（与 workspace 目标的 hub_task_update 顶层
 *     group_id 为空无关 —— 调用方必须回退到 task.origin.group_id）。
 * DM：`!origin.group_id && origin.agent_id === activeId`（发起者是该智能体；群任务
 *     即使发起者相同也不进 DM）。
 */
export function scopeMatch(task: Pick<HubTask, 'origin'>, scope: HubInlineScope): boolean {
  const origin = task.origin ?? {}
  if (scope.kind === 'group') {
    return !!scope.groupId && origin.group_id === scope.groupId
  }
  if (!scope.agentId) return false
  if (origin.group_id) return false
  return origin.agent_id === scope.agentId
}

function taskTime(task: HubTask): number {
  const t = Date.parse(task.updated_at || task.created_at || '')
  return Number.isFinite(t) ? t : 0
}

/**
 * 这条 run 事件归属于哪条任务（同一 target 多条时取最新非终态）。
 *
 * 匹配口径：任务的 `target.kind === 'workspace'`；
 *   · 任务有 `target.session_id` → 必须与事件的会话相同；
 *   · 任务还没有 session（刚 queued/dispatched）→ 用 `target.workspace` 兜底。
 * 优先返回最新的一条**非终态**任务；没有非终态时可返回最新的终态任务 —— 调用方据
 * 它的 status 决定收卡（activeTodos 会过滤终态）。
 */
export function resolveTaskForTool(
  sessionId: string,
  workspace: string,
  tasks: readonly HubTask[],
): HubTask | null {
  const sid = str(sessionId)
  const ws = str(workspace)
  let live: HubTask | null = null
  let liveAt = -1
  let dead: HubTask | null = null
  let deadAt = -1
  for (const task of tasks) {
    const target = task.target
    if (!target || target.kind !== 'workspace') continue
    const targetSid = str(target.session_id)
    const matched = targetSid ? !!sid && targetSid === sid : !!ws && str(target.workspace) === ws
    if (!matched) continue
    const at = taskTime(task)
    if (isTerminalTaskStatus(task.status)) {
      // 同时间戳时先出现的那条更「新」（store 里新任务总在前面），故用 > 比较。
      if (at > deadAt) { dead = task; deadAt = at }
      continue
    }
    if (at > liveAt) { live = task; liveAt = at }
  }
  return live ?? dead
}

/** 未知/未命名 run 的展示名：`run_` + 前 8 位（绝不回退主会话）。 */
export function shortRunName(runId: string): string {
  const id = str(runId)
  if (!id) return ''
  const head = id.startsWith('run_') ? id.slice(4, 12) : id.slice(0, 8)
  return `run_${head}`
}

/**
 * run 的展示名（内联卡的「谁的清单」）。
 *
 * 只有 agent_start 明确标了 `root` 的 run 才显示「主会话」：数不到的 run 一律显示
 * `run_<前 8 位>`。不回退主会话是硬约束 —— 「不知道是谁」和「主会话」在 UI 上必须
 * 区分，否则子智能体的清单会被误读成主会话的计划（todoScope.ts 第 2 条约束同源）。
 */
export function resolveRunName(
  runId: string,
  sessionId: string,
  runNames: HubRunNames,
): string {
  const id = str(runId)
  if (!id) return ''
  const entry = runNames[hubRunKey(sessionId, id)]
  if (!entry) return shortRunName(id)
  if (entry.root) return '主会话'
  const name = str(entry.name)
  return name || shortRunName(id)
}

function normalizeBlocked(raw: unknown): string[] {
  const list = Array.isArray(raw) ? raw : []
  const out: string[] = []
  for (const v of list) {
    const id = str(v)
    if (id) out.push(id)
  }
  return out
}

/** 事件里的 todos（`{id,title,done,blocked_by}`）→ 前端 TodoItem（`blockedBy`）。 */
export function normalizeTodos(raw: unknown): TodoItem[] {
  if (raw == null) return []
  if (!Array.isArray(raw)) return []
  const out: TodoItem[] = []
  for (const item of raw) {
    if (!item || typeof item !== 'object') continue
    const rec = item as Record<string, unknown>
    const id = str(rec.id)
    const title = str(rec.title)
    if (!id && !title) continue
    const blockedBy = normalizeBlocked(rec.blocked_by ?? rec.blockedBy)
    out.push({
      id: id || title,
      title: title || id,
      done: rec.done === true,
      ...(blockedBy.length > 0 ? { blockedBy } : {}),
    })
  }
  return out
}

function sameTodos(a: TodoItem[], b: TodoItem[]): boolean {
  if (a === b) return true
  if (a.length !== b.length) return false
  for (let i = 0; i < a.length; i++) {
    const x = a[i]
    const y = b[i]
    if (x.id !== y.id || x.title !== y.title || x.done !== y.done) return false
    const xb = x.blockedBy ?? []
    const yb = y.blockedBy ?? []
    if (xb.length !== yb.length || xb.some((v, j) => v !== yb[j])) return false
  }
  return true
}

/**
 * 合并一条 `tool_run_end` 的待办快照。
 *
 * 返回 null = 不改（字段缺失 / 会话不是 Project / 迟到）；返回 prev = 内容没变
 * （调用方据此跳过 setState）。三条语义都钉在测试里：
 *   · `todos` 字段缺失   → 不改（这条工具根本没带快照）；
 *   · `todos: []`/`null` → 清空生效（清单被清空是事实，不是"没数据"）；
 *   · 旧 timestamp/seq   → 不覆盖新（重连补包、乱序到达的常态）。
 */
export function mergeTodoSnapshot(
  prev: HubTodoSnapshot | undefined,
  event: HubTodoEvent,
): HubTodoSnapshot | null {
  const sessionId = str(event.session_id)
  const runId = str(event.run_id)
  if (!isProjectSession(sessionId) || !runId) return null
  if (!('todos' in event)) return null
  const ts = str(event.timestamp)
  const seq = Number.isFinite(event.seq) ? Number(event.seq) : 0
  if (prev) {
    if (seq > 0 && prev.seq > 0 && seq < prev.seq) return null
    if (ts && prev.ts) {
      const nextT = Date.parse(ts)
      const prevT = Date.parse(prev.ts)
      if (Number.isFinite(nextT) && Number.isFinite(prevT) && nextT < prevT) return null
    }
  }
  const at = Number.isFinite(event.arrival) ? Number(event.arrival) : (prev ? prev.at + 1 : 1)
  if (prev && at < prev.at) return null
  const todos = normalizeTodos(event.todos)
  if (prev && at === prev.at && ts === prev.ts && seq === prev.seq && sameTodos(prev.todos, todos)) {
    return prev // 重放：内容与序都没变，别换引用
  }
  return {
    key: hubRunKey(sessionId, runId),
    sessionId,
    runId,
    workspace: str(event.workspace) || (prev?.workspace ?? ''),
    todos,
    ts,
    seq,
    at,
  }
}

/**
 * 把快照放进表并按到达序裁剪（保留最新 `limit` 个 run，默认 200）。
 * 返回同一个引用 = 表没变（store 据此跳过 setState）。
 */
export function putTodoSnapshot(
  all: Record<string, HubTodoSnapshot>,
  next: HubTodoSnapshot,
  limit = HUB_TODO_MAX_RUNS,
): Record<string, HubTodoSnapshot> {
  if (all[next.key] === next) return all
  const out: Record<string, HubTodoSnapshot> = { ...all, [next.key]: next }
  const keys = Object.keys(out)
  const over = keys.length - Math.max(1, limit)
  if (over <= 0) return out
  keys.sort((a, b) => out[a].at - out[b].at)
  for (const k of keys.slice(0, over)) delete out[k]
  return out
}

/**
 * 任务终态时清掉它的快照（卡片立即消失，不必等 LRU）。
 *
 * 清的口径是**整个会话**：同一 Project 会话里的主 run/子 run 都属于这条任务。
 * 只有 workspace 而没有 session 的任务不清（同一 workspace 可能还有别的在跑任务，
 * 清掉会把别人的卡一起收走 —— 显示层已经按终态过滤，这里只是回收内存）。
 */
export function forgetTodoSnapshots(
  all: Record<string, HubTodoSnapshot>,
  opts: { sessionId?: string; runIds?: readonly string[] },
): Record<string, HubTodoSnapshot> {
  const sid = str(opts.sessionId)
  const runIds = new Set((opts.runIds ?? []).map((v) => str(v)).filter(Boolean))
  if (!sid && runIds.size === 0) return all
  let out: Record<string, HubTodoSnapshot> | null = null
  for (const [key, snap] of Object.entries(all)) {
    const hit = (!!sid && snap.sessionId === sid) || runIds.has(snap.runId)
    if (!hit) continue
    if (!out) out = { ...all }
    delete out[key]
  }
  return out ?? all
}

/**
 * 记一条 run → 展示名（agent_start）。有界（默认 400）：插入序即 LRU，
 * 超出时丢最旧的。返回同一引用 = 没变。
 */
export function putRunName(
  all: HubRunNames,
  key: string,
  entry: HubRunName,
  limit = HUB_RUN_NAME_MAX,
): HubRunNames {
  const prev = all[key]
  const name = str(entry.name)
  const root = entry.root === true
  if (prev && prev.root === root && str(prev.name) === name) return all
  const out: HubRunNames = { ...all, [key]: { root, name } }
  const keys = Object.keys(out)
  const over = keys.length - Math.max(1, limit)
  if (over <= 0) return out
  for (const k of keys.slice(0, over)) delete out[k]
  return out
}

/** 内联卡（activeTodos 的产物；组件只渲染，不再判断）。 */
export interface HubTodoCard {
  /** React key：任务 + run（同一任务换 run 就是另一张卡）。 */
  key: string
  taskId: string
  taskTitle: string
  /** 发起者展示名（origin.agent_name，缺省回退 agent_id）。 */
  agentName: string
  workspace: string
  sessionId: string
  runId: string
  /** 归属 run 的展示名：'主会话' / 子智能体名 / `run_xxxx`。 */
  runName: string
  /** 这张卡是主 run 的清单（归属行据此显示「主会话」）。 */
  mainRun: boolean
  /**
   * 这条 run 的归属是**已知**的（见过 agent_start，或它就是任务派发的主 run）。
   * false = 只知道 run id：归属行只写 `run_xxxx`，不冒充「子智能体」也绝不写「主会话」。
   */
  runKnown: boolean
  /** 实际展示的条目（最多 HUB_TODO_MAX_ITEMS）。 */
  todos: TodoItem[]
  done: number
  total: number
  /** 未展示的条数（>0 时显示 +N）。 */
  more: number
}

function snapshotList(
  snapshots: readonly HubTodoSnapshot[] | Record<string, HubTodoSnapshot>,
): HubTodoSnapshot[] {
  return Array.isArray(snapshots) ? snapshots : Object.values(snapshots)
}

/**
 * 当前作用域该显示的待办卡。
 *
 * 顺序：最新 run 在前；同一任务只出一张卡（最新 run 的那份清单）—— 主 run 与子 run
 * 各有清单时，用户要看的是「此刻在动的那份」，两份并列会把消息区挤没。归属行写明
 * 是主会话还是哪个子智能体。
 */
export function activeTodos(
  tasks: readonly HubTask[],
  snapshots: readonly HubTodoSnapshot[] | Record<string, HubTodoSnapshot>,
  scope: HubInlineScope,
  runNames: HubRunNames = {},
): HubTodoCard[] {
  const live = tasks.filter((task) =>
    task.target?.kind === 'workspace' &&
    isLiveTaskStatus(task.status) &&
    scopeMatch(task, scope))
  if (live.length === 0) return []
  const newest = new Map<string, { task: HubTask; snap: HubTodoSnapshot }>()
  for (const snap of snapshotList(snapshots)) {
    // `todos: []` = 清单被清空：表里留着这条（挡迟到的新快照），但不该再出卡。
    if (snap.todos.length === 0) continue
    const task = resolveTaskForTool(snap.sessionId, snap.workspace, live)
    if (!task) continue
    const id = str(task.id)
    if (!id) continue
    const cur = newest.get(id)
    if (!cur || snap.at >= cur.snap.at) newest.set(id, { task, snap })
  }
  const cards: HubTodoCard[] = []
  const picked = [...newest.values()].sort((a, b) => b.snap.at - a.snap.at)
  for (const { task, snap } of picked.slice(0, HUB_TODO_MAX_CARDS)) {
    const entry = runNames[hubRunKey(snap.sessionId, snap.runId)]
    // 主 run 判定：agent_start 的 root 标志优先；没见过 agent_start 时用任务的
    // dispatch.run_id（派发出去的那个主 run）兜底。都不知道 → 按未知 run 处理。
    const mainRun = entry
      ? entry.root === true
      : !!str(task.dispatch?.run_id) && str(task.dispatch?.run_id) === snap.runId
    const todoList = snap.todos.slice(0, HUB_TODO_MAX_ITEMS)
    const done = snap.todos.reduce((n, it) => (it.done ? n + 1 : n), 0)
    cards.push({
      key: `${str(task.id)}\u0000${snap.key}`,
      taskId: str(task.id),
      taskTitle: str(task.title),
      agentName: str(task.origin?.agent_name) || str(task.origin?.agent_id),
      workspace: snap.workspace,
      sessionId: snap.sessionId,
      runId: snap.runId,
      runName: mainRun ? '主会话' : resolveRunName(snap.runId, snap.sessionId, runNames),
      mainRun,
      runKnown: !!entry || mainRun,
      todos: todoList,
      done,
      total: snap.todos.length,
      more: snap.todos.length - todoList.length,
    })
  }
  return cards
}
