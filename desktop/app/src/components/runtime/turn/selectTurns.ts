import type { MsgBlock, TurnMetric } from '../../../store/useAppStore'

/* The transcript is a flat list of blocks — there is no "turn" object anywhere
   in the store (see PORT-PLAN §5: `turns[]` holds metrics only, the content
   lives in `blocks[]`). A turn is therefore *derived*: it opens on a user
   message and runs until the next one. Everything in between — thinking, tools,
   subagents, compressions, artifacts — belongs to it. */
type M<K extends MsgBlock['kind']> = Extract<MsgBlock, { kind: K }>

export interface Turn {
  key: string
  index: number
  user?: M<'user'>
  /* 这一轮属于哪一次运行（AgentStart 的 run_id，来自块上的 runId）。
     轮是**派生**的（user 块到下一个 user 块），运行却是显式的 —— 两者不是一一对应：
     后台子 agent / promoted 工具会把一个 run 的余温带进下一次运行。判「这一轮还在不在
     跑」必须同时看 run（见 lib/turnSettle.ts 的 sameRun 与用户 2026-10 的报障：
     上一轮 AgentEnd 之后又发一条消息，上一轮的 Working 被新 run 重新点亮）。
     agent 块的 runId 是**子** run，不能拿来当这一轮的 run → 只看正文与工具块。 */
  run?: string
  items: MsgBlock[]
  answer?: M<'assistant'>
  tools: M<'tool'>[]
  agents: M<'agent'>[]
  compressions: M<'compression'>[]
  artifacts?: M<'artifacts'>
  running: boolean
}

/* A turn's cost is not on any block: usage lives in `SessionView.turns[]`, one
   entry per LLM call, tagged with the `runId` it belongs to. So the join key is
   the runId the blocks already carry — no timestamp arithmetic, no guessing. */
export interface TurnUsage {
  input: number
  output: number
  cacheRead: number
  reasoning: number
  total: number
  llmMs: number
  ttftMs?: number
  matched: boolean
}

export interface TurnStats {
  calls: number
  /** measured segments only: Σ assistant.durMs + Σ tool.durMs. Not wall time —
      the gaps between them are not attributable to anything we can name. */
  workedMs: number
  files: number
  added: number
  removed: number
  agents: number
  compressions: number
}

// 文本判据必须容忍**缺字段**：Go 那边 Block.Text 是 `json:"text,omitempty"`，
// 一个「只有思考、没有正文」的 assistant 块序列化后根本没有 text 字段。历史分页
// （blocks_page）把这种块原样交给 UI，之前的 `b.text.trim()` 直接把整个转录打成
// 空白页 —— 用户只是往上滚了一屏（2026-10 报障：滚一下就 Cannot read 'trim'）。
// 一条畸形块不该让整页崩：这里按空串处理，缺什么当没有。
const textOf = (b: MsgBlock): string => (typeof (b as { text?: string }).text === 'string' ? (b as { text: string }).text : '')
const isText = (b: MsgBlock) => b.kind === 'assistant' && textOf(b).trim().length > 0

function emptyTurn(index: number, key: string): Turn {
  return { key, index, items: [], tools: [], agents: [], compressions: [], running: false }
}

function push(t: Turn, b: MsgBlock) {
  t.items.push(b)
  // 取**最后一个**带 runId 的正文/工具块：agent_resume 会在同一轮里续跑第二次 run，
  // 那一轮属于**新** run（取第一个会把续跑判成「上一轮」而永远不亮 Working）。
  if (b.kind === 'assistant' || b.kind === 'tool') {
    const r = (b as { runId?: string }).runId
    if (typeof r === 'string' && r) t.run = r
  }
  switch (b.kind) {
    case 'assistant':
      if (isText(b)) t.answer = t.answer ? (textOf(b).trim() ? b : t.answer) : b
      if (b.streaming) t.running = true
      break
    case 'tool':
      t.tools.push(b)
      if (b.status === 'running') t.running = true
      break
    case 'agent':
      t.agents.push(b)
      if (b.status === 'running') t.running = true
      break
    case 'compression':
      t.compressions.push(b)
      break
    case 'artifacts':
      t.artifacts = b
      break
    default:
      break
  }
}

export function selectTurns(blocks: MsgBlock[]): Turn[] {
  const turns: Turn[] = []
  let cur: Turn | null = null
  blocks.forEach((b, i) => {
    if (b.kind === 'user') {
      cur = emptyTurn(turns.length, 't' + b.id)
      cur.user = b
      turns.push(cur)
      return
    }
    if (!cur) {
      // a session can start mid-run (restored transcript): the first blocks have
      // no user message of their own, and they still need a home
      // key 用块 id 而不是下标：分页「加载更早」会改变可见切片，下标一变轮盒
      // 就要重挂，已展开的代码块/工具详情会丢状态。
      cur = emptyTurn(turns.length, 't0-' + b.id)
      turns.push(cur)
    }
    push(cur, b)
  })
  return turns
}

export function turnStats(turn: Turn): TurnStats {
  const files = new Map<string, { add: number; del: number }>()
  let workedMs = 0
  turn.items.forEach((b) => {
    if (b.kind === 'assistant' && b.durMs) workedMs += b.durMs
    if (b.kind === 'tool' && b.durMs) workedMs += b.durMs
    if (b.kind === 'tool' && b.diff && b.diff.path) {
      const row = files.get(b.diff.path) || { add: 0, del: 0 }
      row.add += b.diff.added ?? 0
      row.del += b.diff.removed ?? 0
      files.set(b.diff.path, row)
    }
  })
  let added = 0
  let removed = 0
  files.forEach((f) => { added += f.add; removed += f.del })
  return {
    calls: turn.tools.length,
    workedMs,
    files: files.size,
    added,
    removed,
    agents: turn.agents.length,
    compressions: turn.compressions.length,
  }
}

/* The turn head is one dim line: which model answered it, and when it opened.
   The model is taken from the answer when there is one — a turn can switch
   models halfway, and the head should name the one that produced the text, not
   whichever call happened to come first. */
export interface TurnHead {
  model?: string
  t0?: number
}

export function turnHead(turn: Turn): TurnHead {
  const fromAnswer = turn.answer?.model
  const fromItems = turn.items.find((b) => b.kind === 'assistant' && b.model)
  const model = fromAnswer || (fromItems && fromItems.kind === 'assistant' ? fromItems.model : undefined)
  // 轮的开口时刻 = 用户那句话的时刻；没有 user 块（会话截断）才退到首块
  const first = turn.user ?? turn.items.find((b) => 'ts' in b && b.ts)
  const t0 = first && 'ts' in first ? first.ts : undefined
  return { model, t0 }
}

export function turnUsage(turn: Turn, metrics: TurnMetric[] | undefined): TurnUsage {
  const runs = new Set<string>()
  turn.items.forEach((b) => {
    if (b.kind === 'assistant' && b.runId) runs.add(b.runId)
    if (b.kind === 'agent' && b.runId) runs.add(b.runId)
  })
  const u: TurnUsage = { input: 0, output: 0, cacheRead: 0, reasoning: 0, total: 0, llmMs: 0, matched: false }
  if (!metrics || !runs.size) return u
  metrics.forEach((m) => {
    if (!m.runId || !runs.has(m.runId)) return
    u.matched = true
    u.input += m.input || 0
    u.output += m.output || 0
    u.cacheRead += m.cacheRead || 0
    u.reasoning += m.reasoning || 0
    u.llmMs += m.durMs || 0
  })
  u.total = u.input + u.output
  if (!u.ttftMs) {
    const a = turn.items.find((b) => b.kind === 'assistant' && b.ttftMs)
    if (a && a.kind === 'assistant') u.ttftMs = a.ttftMs
  }
  return u
}

/** "31 s" / "4.2 m" — the same rounding the canvas uses. */
export function durText(ms: number): string {
  if (!ms) return ''
  if (ms < 1000) return Math.round(ms) + ' ms'
  const s = ms / 1000
  if (s < 60) return (s < 10 ? s.toFixed(1) : Math.round(s)) + ' s'
  const m = Math.floor(s / 60)
  return m + ' m ' + Math.round(s - m * 60) + ' s'
}

/* What a folded turn still shows. Folding is about the working phase, never
   about the record: the answer, the error that explains why there is no
   answer, the context divider and the produced files stay outside; everything
   else is what it took to get here.

   Records (signal / task_delivery) are the interesting case — their place
   depends on **when they arrived**, not on what kind they are (2026-10，
   用户口径：简洁档 = 时间线 + 一个 Working 块):
   · 运行中到达 → 属于那次运行的过程，进盒子、按事件顺序夹在工作行之间。
     九类 Small signals（memory_activity / tool_hook_warning / run_silent_end /
     goal_alignment_reminder / task_promoted / user_inputs_consumed /
     session_budget_exceeded / session_persist_error / session_closed）与
     后台任务回执 task_delivery 同一判据：它们是「正在发生的事」的一行说明。
   · AgentEnd 之后到达 → 盒子已经折叠，折进去等于「跑完之后什么都没发生」。
     它们留在时间线上（store 按所属轮落位，见 insertAtTurnEnd）。
   判据见 recordAfterRun：块上的 afterRun 位（store 在落块时按 runEnded 盖戳），
   旧快照没有这一位时退回位置 —— 排在轮内最后一段正文之后即交卷后到达。 */
export const ALWAYS_SHOWN = new Set<MsgBlock['kind']>([
  // 失败/重试进度：这一轮为什么没有答案。折进盒子 = 让失败看起来像「什么都没发生」。
  'error',
  // 上下文隔离横线（shutdown / abort / clear）：结构边界，不是这一轮的内容。
  'divider',
  // 本轮产出（AgentEnd.Artifacts）：这一轮的**结果**，轮尾固定落位（见 Narrative）。
  'artifacts',
])

/** 记录类块：一行信号 / 后台任务回执。它们的位置按到达时刻决定，见上方注释。 */
const isRecord = (b: MsgBlock): b is M<'signal'> | M<'task_delivery'> =>
  b.kind === 'signal' || b.kind === 'task_delivery'

/* 这条记录是不是 AgentEnd **之后**才到的。afterRun 位优先（store 的 afterRunNow 在落块
   那一刻盖戳，之后还可能被 insertAtTurnEnd 挪位置，位置本身已经不是证据）；旧快照没有
   这一位时按位置判：轮内最后一段正文（turn.answer）之后到达的即交卷后。 */
const recordAfterRun = (b: MsgBlock, i: number, answerAt: number): boolean => {
  if (!isRecord(b)) return false
  if (b.afterRun != null) return b.afterRun
  return answerAt >= 0 && i > answerAt
}

/* 位置兜底的分界点 = 这一轮的结论（最后一段正文）。正在流式输出的那一段还不是结论：
   此刻到达的记录仍属于这次运行，否则「边跑边来的信号」会在打字期间被甩到盒子外。 */
const answerIndex = (turn: Turn): number => {
  const a = turn.answer
  return a && !a.streaming ? turn.items.indexOf(a) : -1
}

/** 盒子外面（时间线上）的块：结构性/结论性块 + 交卷后到达的记录。 */
function outsideOf(turn: Turn): (b: MsgBlock, i: number) => boolean {
  const answerAt = answerIndex(turn)
  return (b, i) => ALWAYS_SHOWN.has(b.kind) || recordAfterRun(b, i, answerAt)
}

export function foldedBody(turn: Turn): MsgBlock[] {
  const outside = outsideOf(turn)
  return turn.items.filter((b, i) => b === turn.answer || outside(b, i))
}

/* Running, folded density: while the box exists the box holds the run — work
   rows *and* the records that arrived while it was live (in the order they
   happened). Outside stays only what is not the run's progress: the answer,
   the failure that explains its absence, the divider, the produced files, and
   the records that arrive after AgentEnd (the box is already folded then —
   folding those in would read as "nothing happened once it finished").

   includeAnswer 覆盖「这一轮根本没有 Working 盒子」的形态（纯对话轮：没有工具、
   没有子 agent，workBody 为空 ⇒ 不画盒子）。此时正文若仍被排除在外面，它就既不在
   盒子里（没有盒子）也不在外面 —— 整轮在 AgentEnd 之前一个字都渲染不出来。 */
export function runningBody(turn: Turn, includeAnswer = false): MsgBlock[] {
  const outside = outsideOf(turn)
  return turn.items.filter((b, i) => outside(b, i) || (includeAnswer && b === turn.answer))
}

/* What the working box holds (concise). Everything that is *work*: tools, subagents,
   background tasks, the run's own rows — plus the mid-run narration and thoughts.

   Why assistant blocks belong here too (2026-10, from the spec «历史轮次的 Reasoning
   默认收缩…中间轮次的 Content 保留在 Working 内»): a run with five LLM rounds used to
   drop rounds 1–4 entirely — they were neither outside (only the last round's reply is)
   nor inside the box (this list had no `assistant`). The reader could expand the box
   and still not find what the model said in round 2. They belong inside, and the
   Thinking component already folds itself when it is not live, so they arrive
   collapsed — which is exactly the "history rounds stay shut" rule.

   The one assistant left out is the turn's answer: it is handed over to the Worked
   area at AgentEnd and must not appear twice. Compression blocks come along for the
   same reason — they are part of what happened during the run. */
export function workBody(turn: Turn): MsgBlock[] {
  const answerAt = answerIndex(turn)
  return turn.items.filter(
    (b, i) =>
      FOLDED_KINDS.has(b.kind)
      // 运行中到达的记录：这一次运行的过程说明，按事件顺序夹在工作行之间。
      || (isRecord(b) && !recordAfterRun(b, i, answerAt))
      || ((b.kind === 'assistant' || b.kind === 'compression') && b !== turn.answer),
  )
}

/* 这一轮有没有东西要装进 Working 盒子？盒子存在当且仅当 workBody 非空 ——
   不是「有没有工具」。按工具数判会把 async_task-only / system-only 的轮次整轮丢掉：
   workBody 里明明有行，却没有任何地方渲染它（盒子是按工具数决定画不画的）。 */
export function hasWork(turn: Turn): boolean {
  return workBody(turn).length > 0
}

/** Blocks that belong to the working phase even though they are not "a tool". */
export const FOLDED_KINDS: ReadonlySet<MsgBlock['kind']> = new Set<MsgBlock['kind']>([
  'tool', 'agent', 'async_task', 'system', 'task_result',
])
