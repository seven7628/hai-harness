import type { MsgBlock } from '../../../store/useAppStore'
import { fmtMs, toolArgsSummary } from '../../../lib/toolSummary'

/* 一个 round 里的调用折成一行，语法与轮自己的折叠行相同：动词、对象、这个动词
   数的那个量、再是耗时。逐字搬自 design/agent-runtime/v2/ui.js:2143-2202
   （BATCH_VERB 表序就是并列时的裁决顺序）。 */

type Tool = Extract<MsgBlock, { kind: 'tool' }>

const BATCH_VERB: [RegExp, string, string][] = [
  [/edit_file|write_file/, 'Edited', 'file'],
  [/read_file/, 'Read', 'file'],
  [/grep/, 'Searched', 'query'],
  [/glob/, 'Globbed', 'pattern'],
  [/bash/, 'Ran', 'command'],
  [/todo/, 'Planned', 'todo'],
]

function linesOf(s: string | undefined): number {
  return s ? s.split('\n').filter((l) => l.trim().length > 0).length : 0
}

export function batchLine(calls: Tool[]): string {
  if (!calls.length) return ''
  const groups = BATCH_VERB.map(([k, v, u]) => ({ v, u, list: calls.filter((c) => k.test(c.name)) }))
    .filter((g) => g.list.length)
    .sort((a, b) => b.list.length - a.list.length)
  if (!groups.length) return ''
  const top = groups[0]
  const obj = toolArgsSummary(top.list[0].name, top.list[0].args || '').trim()
  const rest = calls.length - top.list.length
  const n0 = top.list.length
  const count = n0 + ' ' + (n0 === 1 ? top.u : top.u === 'query' ? 'queries' : top.u + 's')
  const title = top.v + ' ' + (obj || count) + (rest ? ' +' + rest : '')

  let num = ''
  if (top.v === 'Edited') {
    let a = 0
    let d = 0
    for (const c of calls) {
      if (!/edit_file|write_file/.test(c.name) || !c.diff) continue
      a += c.diff.added ?? 0
      d += c.diff.removed ?? 0
    }
    num = '+' + a + ' \u2212' + d
  } else if (top.v === 'Searched') {
    // 设计从结果的 foot 里取 "N match"；我们的结果就是命中行清单，按行数算同义
    const hits = calls.filter((c) => /grep/.test(c.name)).reduce((n, c) => n + linesOf(c.result), 0)
    num = hits ? hits + ' hits' : ''
  } else if (top.v === 'Read') {
    const lines = calls.filter((c) => /read_file/.test(c.name)).reduce((n, c) => n + linesOf(c.result), 0)
    num = lines ? lines.toLocaleString('en-US') + ' lines' : ''
  } else if (top.v === 'Globbed') {
    const files = calls.filter((c) => /glob/.test(c.name)).reduce((n, c) => n + linesOf(c.result), 0)
    num = files ? files.toLocaleString('en-US') + ' files' : ''
  } else if (top.v === 'Ran') {
    const kb = calls
      .filter((c) => /bash/.test(c.name))
      .reduce((n, c) => n + (c.result ? c.result.length / 1024 : 0), 0)
    num = kb ? kb.toFixed(1) + ' KB' : ''
  }
  const dur = calls.reduce((n, c) => n + (c.durMs ?? 0), 0)
  return [title, num, dur ? fmtMs(dur) : ''].filter(Boolean).join(' \u00b7 ')
}

/* 子 agent 的**派发调用**不自己占一行：agent_spawn / agent_send 是「让某个子 agent
   跑起来」这个动作的实现细节，而子 agent 自己由车道（Lane / .la-row）用一行讲完
   ——「subagent · go test ./auth · done · 结果 · 2.2 s」。两处都画就是同一个子 agent
   出现两次（用户截图里上面两行「Agent」+ 下面两行「subagent」正是这个）。
   别的皮肤不动：判据只在运行时皮肤的分组里用（见 Narrative 的 groupRounds）。 */
export const isDispatchTool = (name: string): boolean => /^agent_(spawn|send|resume|stop)$/.test(name)

/* 一组调用「进行到哪了」—— 运行中要说清 完成几 / 还在跑几 / 失败几，
   而不是只报一句静态的「Read 202 files」。规格（2026-10）：
     「工具组执行期间保持展开，展示总数、已完成数、运行中数量、失败数量和执行时长」
   结算之后才回到设计那句话（动词 · 对象 · 量 · 耗时）。 */
export interface BatchStatus {
  total: number
  done: number
  running: number
  failed: number
  /** 已结算调用的耗时之和（只加有值的：跑着的那条还没有 durMs） */
  durMs: number
  /** 全部结束（没有一条还在跑）—— 组可以自动收缩了 */
  settled: boolean
}

export function batchStatus(calls: Tool[]): BatchStatus {
  let done = 0
  let running = 0
  let failed = 0
  let durMs = 0
  for (const c of calls) {
    if (c.status === 'running') running++
    else if (c.isError) failed++
    else done++
    if (typeof c.durMs === 'number') durMs += c.durMs
  }
  return { total: calls.length, done, running, failed, durMs, settled: running === 0 }
}

/** 运行中那一版的组头：数字全部来自真实状态，不猜也不四舍五入。 */
export function batchRunningLine(calls: Tool[]): string {
  const s = batchStatus(calls)
  if (!s.total) return ''
  const parts = [`${s.total} tool ${s.total === 1 ? 'call' : 'calls'}`]
  if (s.done) parts.push(`${s.done} completed`)
  if (s.running) parts.push(`${s.running} running`)
  if (s.failed) parts.push(`${s.failed} failed`)
  if (s.durMs) parts.push(fmtMs(s.durMs))
  return parts.join(' \u00b7 ')
}

/** 失败调用的一行摘要（收缩态也要看得见）：名字 + 结果/错误的首行。 */

