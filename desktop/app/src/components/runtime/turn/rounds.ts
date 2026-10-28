import type { MsgBlock } from '../../../store/useAppStore'

/* A turn is not a flat list of blocks: it is rounds. A round opens on an
   assistant block (its reasoning and its text) and swallows the tool calls and
   subagents that answer it; a run of tool calls with no assistant before them is
   a round of its own. The design draws that boundary as 6px inside a round and
   12px between them — the numbers are meaningless without the grouping.
   (design/agent-runtime/v2/ui.js:2356-2380, PORT-PLAN §11.6.3) */

export interface Round {
  key: string
  /** the assistant block that opened this round, if any */
  say?: MsgBlock
  items: MsgBlock[]
}

const WORK = new Set<MsgBlock['kind']>(['tool', 'agent', 'compression'])

export function roundsOf(items: MsgBlock[]): Round[] {
  const out: Round[] = []
  let cur: Round | null = null
  const open = (b?: MsgBlock) => {
    cur = { key: 'r' + out.length + '-' + (b ? b.id : 'x'), ...(b ? { say: b } : {}), items: [] }
    out.push(cur)
  }
  for (const b of items) {
    if (b.kind === 'assistant') {
      // the assistant's own words open a round; a previous round of bare tool
      // calls stays closed on its own
      open(b)
      continue
    }
    if (WORK.has(b.kind)) {
      if (!cur) open()
      cur!.items.push(b)
      continue
    }
    // anything else (user, artifacts, system, results) is not part of a round
    cur = null
  }
  return out
}
