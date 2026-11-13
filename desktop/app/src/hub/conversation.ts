import type { HubConversation, HubDelivery, HubGroupProgress } from './types'

// 群内自动对话的状态文案（P3b-07，方案 §7.5.4 / §8.3.5）。
// 纯函数：ConversationBar 与左栏都从这里取同一套口径，Node 直测。

/**
 * Go 侧 UnlimitedTurnBudget / DefaultConversationMaxDepth 的同值镜像。
 * HUB_TURN_BUDGET 是「没有限额」：生产默认不设自动接龙轮次上限，缺省不能又显示 8。
 */
export const HUB_TURN_BUDGET = -1
export const HUB_MAX_DEPTH = 4

export type ConversationTone = 'idle' | 'running' | 'waiting' | 'stopped' | 'exhausted' | 'unknown'

export function conversationTone(status?: string): ConversationTone {
  switch (status) {
    case 'idle': return 'idle'
    case 'running': return 'running'
    case 'waiting_human': return 'waiting'
    case 'stopped': return 'stopped'
    case 'exhausted': return 'exhausted'
    default: return 'unknown'
  }
}

export function conversationStatusLabel(status?: string): string {
  switch (status) {
    case 'idle': return '空闲'
    case 'running': return '自动对话中'
    case 'waiting_human': return '等你继续'
    case 'stopped': return '已停止'
    case 'exhausted': return '已达上限'
    default: return status || '未知'
  }
}

/**
 * 轮次/深度/队列一行：
 * - 有轮次上限：`第 3/8 轮 · 深度 2/4 · 队列 1`
 * - 无限（turn_budget<=0，生产默认）：`第 3 轮 · 深度 2/4 · 队列 1`
 */
export function conversationTurnText(conv?: HubConversation | null): string {
  if (!conv) return ''
  const budget = conv.turn_budget ?? HUB_TURN_BUDGET
  const maxDepth = conv.max_depth && conv.max_depth > 0 ? conv.max_depth : HUB_MAX_DEPTH
  const turns = Math.max(0, conv.turns_used ?? 0)
  const depth = Math.max(0, conv.depth ?? 0)
  const queue = Math.max(0, conv.queue_len ?? 0)
  const turn = budget > 0 ? `第 ${turns}/${budget} 轮` : `第 ${turns} 轮`
  return `${turn} · 深度 ${depth}/${maxDepth} · 队列 ${queue}`
}

/** 最热的一对 A→B（方案 §6.5.6 的踢皮球护栏）：`A → B 2/2`；没有则空串。 */
export function conversationPairText(
  conv?: HubConversation | null,
  names?: Record<string, string>,
): string {
  if (!conv?.pair_count) return ''
  const limit = conv.pair_limit && conv.pair_limit > 0 ? conv.pair_limit : 2
  let best: { key: string; count: number } | null = null
  for (const [key, count] of Object.entries(conv.pair_count)) {
    if (!Number.isFinite(count) || count <= 0) continue
    if (!best || count > best.count) best = { key, count }
  }
  if (!best) return ''
  const parts = best.key.split(/\s*(?:→|->)\s*/)
  if (parts.length !== 2) return ''
  const [from, to] = parts
  const label = (id: string): string => names?.[id] ?? id
  return `${label(from)} → ${label(to)} ${best.count}/${limit}`
}

/**
 * 终态/停止文案。stop_reason 是后端写的人话原因；默认那句不重复。
 */
export function conversationTerminalText(conv?: HubConversation | null): string {
  if (!conv) return ''
  const reason = (conv.stop_reason ?? '').trim()
  if (conv.status === 'stopped') {
    if (!reason || reason === '人类停止了自动对话，等你继续') return '你停止了自动对话'
    return `你停止了自动对话：${reason}`
  }
  if (conv.status === 'exhausted') {
    if (reason) return `自动对话已达上限：${reason}`
    // 兜底不提轮数：生产不设轮次上限，命中的只会是深度/同对/同内容等护栏。
    return '自动对话已达上限，等你继续'
  }
  if (conv.status === 'waiting_human') return '队列已排空，等你继续'
  return ''
}

/**
 * 停止按钮是否可用：**确实有东西可停**才允许。
 * - active（流式草稿/成员在跑/已派发或运行中的群任务）优先：工具型长任务
 *   可能没有草稿、也没有 running 的对话视图，任务状态是唯一信号（B7）；
 * - 否则只有 running/exhausted 且队列里还有 wake 才可停；
 * - stopped 一律禁用；idle 没有 active 时也禁用。
 */
export function conversationCanStop(conv: HubConversation | null | undefined, active: boolean): boolean {
  if (conv?.status === 'stopped') return false
  if (active === true) return true
  if (!conv) return false
  if (conv.status !== 'running' && conv.status !== 'exhausted') return false
  return (conv.queue_len ?? 0) > 0
}

/** 左栏预览：任务结论优先，其次最后一条可用消息（与 Go GroupPerception 同口径）。 */
export function groupPreview(info?: HubGroupProgress | null): string {
  if (!info) return ''
  return (info.last_outcome || info.last_text || '').replace(/\s+/g, ' ').trim()
}

/** 未读角标：>99 显示 99+（方案 §8.2）。 */
export function unreadBadge(count?: number): string {
  const n = Math.max(0, Math.floor(count ?? 0))
  return n > 99 ? '99+' : String(n)
}

/** 相对时间：刚刚 / N 分钟前 / N 小时前 / 昨天 / N 天前 / 月日。 */
export function formatRelativeTime(iso?: string | null, now: number = Date.now()): string {
  if (!iso) return ''
  const t = Date.parse(iso)
  if (!Number.isFinite(t)) return ''
  const diff = Math.max(0, now - t)
  const min = Math.floor(diff / 60_000)
  if (min < 1) return '刚刚'
  if (min < 60) return `${min} 分钟前`
  const hours = Math.floor(min / 60)
  if (hours < 24) return `${hours} 小时前`
  const days = Math.floor(hours / 24)
  if (days === 1) return '昨天'
  if (days < 7) return `${days} 天前`
  const d = new Date(t)
  const sameYear = d.getFullYear() === new Date(now).getFullYear()
  return sameYear
    ? `${d.getMonth() + 1} 月 ${d.getDate()} 日`
    : `${d.getFullYear()}/${d.getMonth() + 1}/${d.getDate()}`
}

/** 消息时钟（HH:MM，本地时区）：消息头与用户气泡下的小字。 */
export function formatClockTime(iso?: string | null): string {
  if (!iso) return ''
  const t = Date.parse(iso)
  if (!Number.isFinite(t)) return ''
  const d = new Date(t)
  const hh = d.getHours() < 10 ? `0${d.getHours()}` : String(d.getHours())
  const mm = d.getMinutes() < 10 ? `0${d.getMinutes()}` : String(d.getMinutes())
  return `${hh}:${mm}`
}

// ── 投递账本（P6-01）──────────────────────────────────────────────────────────
// 一条 @ 会变成 N 条投递：谁在跑、谁排队、谁被护栏跳过。状态条与 @all 摘要都从
// 这里取同一套口径（Go 的 agenthub.Delivery 是权威，这里只做文案映射）。

/** 投递原因码 → 人话。未知码原样返回（后端新增原因时前端不会显示空白）。 */
export function deliveryReasonText(reason?: string | null): string {
  switch (reason) {
    case 'target_busy': return '目标忙，排队中'
    case 'rate_limit': return '上游限流，退避重试'
    case 'pair_limit': return '互踢到上限，已跳过'
    case 'depth_limit': return '链路太深，已跳过'
    case 'budget': return '预算不足，未启动'
    case 'stopped': return '已停止'
    case 'stale_conversation': return '上一轮对话已作废'
    case 'duplicate': return '重复投递已忽略'
    case 'interrupted': return '运行被中断'
    case 'shutdown': return 'Hub 正在关停'
    case 'invalid': return '群或成员已不可用'
    default: return (reason ?? '').trim()
  }
}

/** 单条投递的一句话：`B 排队中（目标忙）` / `C 失败：…`。 */
export function deliveryLabel(d?: HubDelivery | null, names?: Record<string, string>): string {
  if (!d) return ''
  const who = names?.[d.target ?? ''] ?? d.target ?? ''
  const why = deliveryReasonText(d.reason)
  switch (d.status) {
    case 'queued': return why ? `${who} 排队中（${why}）` : `${who} 排队中`
    case 'running': return `${who} 进行中`
    case 'done': return `${who} 已回复`
    case 'skipped': return why ? `${who} 已跳过：${why}` : `${who} 已跳过`
    case 'failed': return why ? `${who} 失败：${why}` : `${who} 失败`
    default: return who
  }
}

/**
 * 一轮 @ 的投递摘要：`9 进行中 · 2 排队 · 1 跳过`。
 * 只统计最近一批（同一 conversation_id + 同一 turn 的最后连续一段），
 * 否则历史记录会把当前这轮淹没。
 */
export function deliverySummary(conv?: HubConversation | null): string {
  const list = conv?.deliveries ?? []
  if (list.length === 0) return ''
  const last = list[list.length - 1]
  const batch = list.filter((d) => (d.turn ?? 0) === (last.turn ?? 0) &&
    (d.conversation_id ?? '') === (last.conversation_id ?? ''))
  const count = (s: string): number => batch.filter((d) => d.status === s).length
  const parts: string[] = []
  const running = count('running')
  const queued = count('queued')
  const done = count('done')
  const skipped = count('skipped')
  const failed = count('failed')
  if (running > 0) parts.push(`${running} 进行中`)
  if (queued > 0) parts.push(`${queued} 排队`)
  if (done > 0) parts.push(`${done} 已回复`)
  if (skipped > 0) parts.push(`${skipped} 跳过`)
  if (failed > 0) parts.push(`${failed} 失败`)
  return parts.join(' · ')
}
