// P4-01 基准的无浏览器回退（lab 专用）。
//
// 没有 Playwright/Chromium 时仍然给出可重复的「纯数据层」数字：
//   - applyHubDelta：每条 delta 的归约成本（真实生产函数）；
//   - buildThreadItems：220 条消息的全量重建成本（真实生产函数）；
// 渲染/键入/滚动指标必须真浏览器，回退模式下显式标记为 unavailable。
import { applyHubDelta } from '../hub/groupStream'
import { buildThreadItems } from '../hub/groupThread'
import { buildHubLongFixture } from './hubLongFixture'

export interface HubLongPureResult {
  messages: number
  rounds: number
  items: number
  drafts: number
  textChars: number
  applyDeltaUs: { n: number; mean: number; p95: number; max: number }
  buildItemsMs: { n: number; mean: number; p95: number; max: number }
}

function stats(values: number[]): { n: number; mean: number; p95: number; max: number } {
  if (values.length === 0) return { n: 0, mean: 0, p95: 0, max: 0 }
  const sorted = [...values].sort((a, b) => a - b)
  let sum = 0
  for (const v of sorted) sum += v
  return {
    n: sorted.length,
    mean: Math.round((sum / sorted.length) * 1000) / 1000,
    p95: Math.round(sorted[Math.floor((sorted.length - 1) * 0.95)] * 1000) / 1000,
    max: Math.round(sorted[sorted.length - 1] * 1000) / 1000,
  }
}

export function runHubLongPureBench(): HubLongPureResult {
  const fx = buildHubLongFixture()
  let drafts = fx.initialDrafts.map((d) => ({ ...d }))
  const finalized: string[] = []
  const deltaSamples: number[] = []

  for (let n = 0; n < 3000; n++) {
    const w = n % drafts.length
    const draft = drafts[w]
    const start = performance.now()
    drafts = applyHubDelta(drafts, {
      group_id: fx.group.id,
      agent_id: draft.agentId,
      round_id: draft.roundId,
      message_id: draft.messageId,
      index: draft.index + 1,
      delta: `第${n}段：弱网下 delta 只丢草稿，final 到达后整条覆盖。`,
    }, finalized)
    deltaSamples.push((performance.now() - start) * 1000)
  }

  const buildSamples: number[] = []
  const items = buildThreadItems(fx.messages, fx.rounds)
  for (let i = 0; i < 300; i++) {
    const start = performance.now()
    buildThreadItems(fx.messages, fx.rounds)
    buildSamples.push(performance.now() - start)
  }

  const textChars = fx.messages.reduce((n, m) => n + (m.text?.length ?? 0), 0)
  return {
    messages: fx.messages.length,
    rounds: fx.rounds.length,
    items: items.length,
    drafts: fx.initialDrafts.length,
    textChars,
    applyDeltaUs: stats(deltaSamples),
    buildItemsMs: stats(buildSamples),
  }
}
