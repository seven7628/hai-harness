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

const isText = (b: MsgBlock) => b.kind === 'assistant' && b.text.trim().length > 0

function emptyTurn(index: number, key: string): Turn {
  return { key, index, items: [], tools: [], agents: [], compressions: [], running: false }
}

function push(t: Turn, b: MsgBlock) {
  t.items.push(b)
  switch (b.kind) {
    case 'assistant':
      if (isText(b)) t.answer = t.answer ? (b.text.trim() ? b : t.answer) : b
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
   about the record: a signal, the run's artifacts, or a background task's
   receipt are things that happened, not things that were done to get here, and
   folding them away would hide a record the reader has no other way to find
   (the red line: never silently drop data). The answer is included wherever it
   sits in the turn, so the order of what is left is the order it happened. */
export const ALWAYS_SHOWN = new Set<MsgBlock['kind']>([
  'system',
  'error',
  'divider',
  'artifacts',
  'task_result',
  'task_delivery',
])

export function foldedBody(turn: Turn): MsgBlock[] {
  return turn.items.filter((b) => b === turn.answer || ALWAYS_SHOWN.has(b.kind))
}

/* Running, folded density: the only thing allowed outside the working block is
   the sentence being written right now. Every earlier llm_content, every
   thought and every tool call lives inside it — that is what "working" means
   (AgentStart → AgentEnd is one block). Records always stay visible. */
export function runningBody(turn: Turn): MsgBlock[] {
  const last = turn.items[turn.items.length - 1]
  return turn.items.filter(
    (b) => ALWAYS_SHOWN.has(b.kind) || (b === last && b.kind === 'assistant' && b.streaming),
  )
}
