// 「Small signals」的文案构造：9 类事件各一行（标题 + 副题 + 右侧 event_type）。
// 设计见 design/agent-runtime/v2 的「Small signals」那一节（9 events · 1 row each）。
//
// 为什么文案在这里而不是在 store 里拼：store 只能拿到 i18nT（无参模板），把句子写死在
// store 里等于把中英文散文埋进状态层；这里只接受**事实**（条数/文件名/轮次/金额…），
// 由调用方给进 t()，于是同一份逻辑既能被单测（假的 t 直接回 key）也能被两种语言复用。
//
// 一条硬规则：**没有的字段不编**。设计里每行都有一个具体副题，但我们手里有的字段并不
// 一一对应（例如 session_closed 的「18 runs · 17 consumed · 1 abandoned mid-tool」这套
// 计数根本没下发）。缺就是缺 —— 副题留空，不用猜的数字把它填满。

import type { SignalKind } from '../components/runtime/signals/Signal'

/** 一次信号事件能提供的全部事实（严格对应后端事件字段，缺的字段不出现）。 */
export interface SignalFact {
  sig: SignalKind
  /** 事件名（设计右侧那一列），直接写事件类型，不翻译 */
  etype: string
  /** 条数（memory 的 count / inputs 的条数） */
  n?: number
  /** 文件名列表（memory_activity 的 file） */
  files?: string[]
  /** 上下文名（hook 名 / 工具名 / 后台任务名） */
  name?: string
  /** 事件的原始 message / error（hook 警告、persist 失败） */
  message?: string
  /** run_silent_end 的轮数 */
  rounds?: number
  /** run_silent_end 的工具调用数 */
  toolCalls?: number
  /** run_silent_end：本 run 是否曾产出 assistant 文本 */
  sawText?: boolean
  /** session_budget_exceeded 的已用/上限（美元） */
  cost?: number
  maxCost?: number
  /** goal_alignment_reminder 的轮次 */
  round?: number
  /** 合并输入的原文（只取前两条做副题） */
  texts?: string[]
  /** session_closed 的运行账（Go 的 SessionClosed.Runs/Consumed/AbandonedMidTool） */
  runs?: number
  consumed?: number
  abandoned?: number
  /** compress_end 的压缩前后消息数（上下文压缩完成的那一行） */
  before?: number
  after?: number
}

export interface SignalText {
  title: string
  detail?: string
}

type T = (key: string) => string

const fmtUsd = (n: number): string => (n >= 1 ? '$' + n.toFixed(2) : '$' + n.toFixed(4))

/** 事实 → 一行文案。后端没给的字段一律不出现（宁缺不编）。 */
export function signalText(f: SignalFact, t: T): SignalText {
  const rep = (key: string, pairs: Record<string, string | number>): string =>
    Object.entries(pairs).reduce((s, [k, v]) => s.split(`{${k}}`).join(String(v)), t(key))
  switch (f.sig) {
    case 'memory': {
      const saved = f.etype === 'memory_activity' && f.n != null
      const title = rep(saved ? 'sig.memory.saved' : 'sig.memory.recalled', { n: f.n ?? 0 })
      const detail = f.files?.length ? f.files.slice(0, 4).join(' · ') : undefined
      return { title, ...(detail ? { detail } : {}) }
    }
    case 'hook': {
      const detail = [f.name, f.message].filter(Boolean).join(' · ')
      return { title: t('sig.hook'), ...(detail ? { detail } : {}) }
    }
    case 'silent': {
      const facts: string[] = []
      if (f.rounds != null) facts.push(rep('sig.silent.rounds', { n: f.rounds }))
      if (f.toolCalls != null) facts.push(rep('sig.silent.calls', { n: f.toolCalls }))
      if (f.sawText === false) facts.push(t('sig.silent.neverSpoke'))
      return { title: t('sig.silent'), ...(facts.length ? { detail: facts.join(' · ') } : {}) }
    }
    case 'goal': {
      const detail = f.round != null ? rep('sig.goal.round', { n: f.round }) : undefined
      return { title: t('sig.goal'), ...(detail ? { detail } : {}) }
    }
    case 'promote': {
      const detail = [f.name, f.message].filter(Boolean).join(' · ')
      return { title: t('sig.promote'), ...(detail ? { detail } : {}) }
    }
    case 'inputs': {
      const title = rep('sig.inputs', { n: f.n ?? 0 })
      const detail = f.texts?.length
        ? f.texts.slice(0, 2).map((s) => s.replace(/\s+/g, ' ').slice(0, 40)).join(' · ')
        : undefined
      return { title, ...(detail ? { detail } : {}) }
    }
    case 'budget': {
      const detail = f.cost != null && f.maxCost != null ? `${fmtUsd(f.cost)} / ${fmtUsd(f.maxCost)}` : undefined
      return { title: t('sig.budget'), ...(detail ? { detail } : {}) }
    }
    case 'persist': {
      return { title: t('sig.persist'), ...(f.message ? { detail: f.message } : {}) }
    }
    case 'compact': {
      // 只报手里有的数：compress_end 的 before/after 是消息数，缺任一端就不写副题
      const detail = f.before != null && f.after != null
        ? rep('sig.compact.msgs', { before: f.before, after: f.after })
        : undefined
      return { title: t('sig.compact'), ...(detail ? { detail } : {}) }
    }
    default: {
      // session_closed：设计的副题是「18 runs · 17 consumed · 1 abandoned mid-tool」。
      // 三个数都是后端的事实，缺哪个就不写哪个（老 bridge 一个都不发 → 只有标题）。
      // 不做算术：runs 不一定等于 consumed + abandoned（思考阶段被打断的运行两类都不算）。
      const facts: string[] = []
      if (f.runs != null) facts.push(rep('sig.closed.runs', { n: f.runs }))
      if (f.consumed != null) facts.push(rep('sig.closed.consumed', { n: f.consumed }))
      if (f.abandoned) facts.push(rep('sig.closed.abandoned', { n: f.abandoned }))
      return { title: t('sig.closed'), ...(facts.length ? { detail: facts.join(' · ') } : {}) }
    }
  }
}
