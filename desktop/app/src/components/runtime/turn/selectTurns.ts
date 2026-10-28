import type { MsgBlock } from '../../../store/useAppStore'

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
      cur = emptyTurn(turns.length, 't0-' + i)
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

/** "31 s" / "4.2 m" — the same rounding the canvas uses. */
export function durText(ms: number): string {
  if (!ms) return ''
  if (ms < 1000) return Math.round(ms) + ' ms'
  const s = ms / 1000
  if (s < 60) return (s < 10 ? s.toFixed(1) : Math.round(s)) + ' s'
  const m = Math.floor(s / 60)
  return m + ' m ' + Math.round(s - m * 60) + ' s'
}
