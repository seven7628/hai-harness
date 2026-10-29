import type { MsgBlock } from '../../../store/useAppStore'

/* A turn is not a flat list of blocks: it is rounds. A round opens on an
   assistant block (its reasoning and its text) and swallows the tool calls and
   subagents that answer it; a run of tool calls with no assistant before them is
   a round of its own. The design draws that boundary as 6px inside a round and
   12px between them — the numbers are meaningless without the grouping.
   (design/agent-runtime/v2/ui.js:2356-2380, PORT-PLAN §11.6.3) */

export interface Round {
  key: string
  /** 这一段属于哪一轮 LLM 调用（tool/agent 块上的 request_id）。
      有了它，「同一次运行里的第几轮」不用靠相邻性猜；旧数据没有 → undefined，
      行为退回相邻性。 */
  req?: string
  /** the assistant block that opened this round, if any */
  say?: MsgBlock
  /** 不是一轮 LLM 调用，而是一个「记录」块（artifacts / system / 回执），单独成段 */
  bare?: boolean
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
  // open() 会写 cur，但 TS 看不到闭包里的赋值 —— 用它拿到「当前段」的非空视图
  const ensure = (): Round => {
    if (!cur) open()
    return cur as unknown as Round
  }
  const reqOf = (b: MsgBlock): string | undefined => {
    const r = (b as { requestId?: string }).requestId
    return typeof r === 'string' && r ? r : undefined
  }
  for (const b of items) {
    if (b.kind === 'assistant') {
      // the assistant's own words open a round; a previous round of bare tool
      // calls stays closed on its own
      open(b)
      continue
    }
    if (WORK.has(b.kind)) {
      const req = reqOf(b)
      let c = cur as unknown as Round | null
      if (!c) c = ensure()
      else if (req && c.req && req !== c.req) {
        // 新的一轮 LLM 调用发起的工具（哪怕没有 assistant 夹在中间）——
        // 之前只按相邻性归段，会把两轮的同名工具并成一段
        open()
        c = ensure()
      }
      // 段还没有归属就先认领（assistant 块目前不带 request_id）
      if (!c.req && req) c.req = req
      c.items.push(b)
      continue
    }
    /* 其余块（artifacts / system / task_result / divider）不属于任何一轮，但它们
       **必须留在原位**而不是被丢掉：产出卡、系统信号、回执都是在两轮之间发生的
       记录，丢了就是「agent_end 之后没有产出块」——用户报的就是这个。
       各自成为一段（bare：没有 assistant 开口，groupRounds 因此不画批处理行）。 */
    out.push({ key: 's' + out.length + '-' + b.id, items: [b], bare: true })
    cur = null
  }
  return out
}
