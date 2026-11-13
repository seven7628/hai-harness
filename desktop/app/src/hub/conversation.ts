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
    // 未知状态码不能显示成一个裸单词（读起来像文案 bug，也分不清是状态还是原因）：
    // 带上「未知状态」前缀，同时保留原始码给排障。
    default: return status ? `未知状态（${status}）` : '未知'
  }
}

/**
 * 轮次/深度/队列一行：
 * - 有轮次上限：`第 3/8 轮 · 深度 2/4 · 队列 1`
 * - 无限（turn_budget<=0，生产默认）：`第 3 轮 · 深度 2/4 · 队列 1`
 */
export function conversationTurnText(conv?: HubConversation | null): string {
  if (!conv) return ''
  const { depth, max } = depthPair(conv)
  return [conversationRoundText(conv), `深度 ${depth}/${max}`, conversationQueueText(conv)].join(' · ')
}

/** 深度计数：`depth/max`。本链与「深度」两种叫法共用同一个分母口径（缺省 = HUB_MAX_DEPTH）。 */
function depthPair(conv?: HubConversation | null): { depth: number; max: number } {
  const max = conv?.max_depth && conv.max_depth > 0 ? conv.max_depth : HUB_MAX_DEPTH
  return { depth: Math.max(0, conv?.depth ?? 0), max }
}

/** 本轮（人类一次发言后自动接龙的轮数）：`第 3 轮` / 有上限时 `第 3/8 轮`。 */
export function conversationRoundText(conv?: HubConversation | null): string {
  if (!conv) return ''
  const budget = conv.turn_budget ?? HUB_TURN_BUDGET
  const turns = Math.max(0, conv.turns_used ?? 0)
  return budget > 0 ? `第 ${turns}/${budget} 轮` : `第 ${turns} 轮`
}

/**
 * 本链（当前接龙链的深度，P6-05）：`本链 2/4`。
 * 与 conversationTurnText 里的「深度 2/4」是同一个数 —— 状态条用「本链」把
 * 「这是**这一条链**的深度，不是整个群的轮次」说清楚。
 */
export function conversationChainText(conv?: HubConversation | null): string {
  if (!conv) return ''
  const { depth, max } = depthPair(conv)
  return `本链 ${depth}/${max}`
}

/** 待投递队列（排队等目标空闲的 @）：`队列 2`。 */
export function conversationQueueText(conv?: HubConversation | null): string {
  if (!conv) return ''
  return `队列 ${Math.max(0, conv.queue_len ?? 0)}`
}

/**
 * 最热的一对（方案 §6.5.6 的踢皮球护栏，P6-04 起是**无序**滚动窗口）：
 * `A ↔ B 10/10 · 约 8 分钟后自动恢复`；没有则空串。
 *
 * 键是 Go 的 ConversationPairKey（`a↔b`，字典序）。旧数据里的 `a→b` 也兼容解析，
 * 否则升级后老会话的计数会整片消失。
 */
export function conversationPairText(
  conv?: HubConversation | null,
  names?: Record<string, string>,
  now: number = Date.now(),
): string {
  if (!conv?.pair_count) return ''
  const limit = conv.pair_limit && conv.pair_limit > 0 ? conv.pair_limit : 10
  let best: { key: string; count: number } | null = null
  for (const [key, count] of Object.entries(conv.pair_count)) {
    if (!Number.isFinite(count) || count <= 0) continue
    if (!best || count > best.count) best = { key, count }
  }
  if (!best) return ''
  const parts = best.key.split(/\s*(?:↔|→|->)\s*/)
  if (parts.length !== 2) return ''
  const [from, to] = parts
  const label = (id: string): string => names?.[id] ?? id
  const base = `${label(from)} ↔ ${label(to)} ${best.count}/${limit}`
  const left = pairRecoverText(conv, now)
  return left ? `${base} · ${left}` : base
}

/** 到限的 pair 还有多久自动恢复：`约 8 分钟后自动恢复`；没到限/没有窗口则空串。 */
export function pairRecoverText(conv?: HubConversation | null, now: number = Date.now()): string {
  if (!conv?.pair_reset_at) return ''
  const limit = conv.pair_limit && conv.pair_limit > 0 ? conv.pair_limit : 10
  // 只在「确实到限」时才提示恢复时间（没到限时那行字是噪音）。
  const counts = Object.values(conv.pair_count ?? {})
  if (counts.length === 0 || Math.max(...counts) < limit) return ''
  const t = Date.parse(conv.pair_reset_at)
  if (!Number.isFinite(t)) return ''
  const ms = Math.max(0, t - now)
  if (ms <= 0) return '即将自动恢复'
  const min = Math.ceil(ms / 60_000)
  if (min < 1) return '不到 1 分钟后自动恢复'
  if (min < 60) return `约 ${min} 分钟后自动恢复`
  const hours = Math.ceil(min / 60)
  return `约 ${hours} 小时后自动恢复`
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
    case 'run_timeout': return '单轮超时，已中断'
    case 'shutdown': return 'Hub 正在关停'
    case 'invalid': return '群或成员已不可用'
    case 'queue_full': return '排队过多，已跳过'
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
 * 两条投递是否属于**同一批**（同一次 @ 产生的 N 条投递）。
 *
 * I2：按 conversation_id + round_id 聚合 —— 被护栏跳过的目标从来没有 turn（恒 0），
 * 只按 turn 比对会把「A running(turn=1) + B skipped(turn=0)」拆成两批，状态条只显示
 * 其中一条，人看到「A 在跑」却看不到「B 被跳过了」。
 *
 * round_id 为空（旧后端/人类首轮）才回退到 turn：两侧都空时比 turn，任一侧有
 * round_id 就必须相等 —— 不同 round 绝不能混进同一批。
 */
function sameDeliveryBatch(a: HubDelivery, b: HubDelivery): boolean {
  if ((a.conversation_id ?? '') !== (b.conversation_id ?? '')) return false
  const ar = (a.round_id ?? '').trim()
  const br = (b.round_id ?? '').trim()
  if (ar || br) return ar === br
  return (a.turn ?? 0) === (b.turn ?? 0)
}

/** 最近一批投递（同一 conversation_id + round_id 的最后一段）；历史记录不能淹没当前这轮。 */
function lastDeliveryBatch(list: HubDelivery[]): HubDelivery[] {
  if (list.length === 0) return []
  const last = list[list.length - 1]
  return list.filter((d) => sameDeliveryBatch(last, d))
}

/**
 * 「只停这个成员」的目标（P6-08）：账本里还有进行中/排队投递的 target，按出现顺序
 * 去重。群头/成员行的停止入口用它决定给谁显示 —— 群级停止仍然是另一颗按钮。
 */
export function stoppableAgentIds(conv?: HubConversation | null): string[] {
  const out: string[] = []
  const seen = new Set<string>()
  for (const d of conv?.deliveries ?? []) {
    const id = (d.target ?? '').trim()
    if (!id || seen.has(id)) continue
    if (d.status !== 'running' && d.status !== 'queued') continue
    seen.add(id)
    out.push(id)
  }
  return out
}

/**
 * 投递原因码 → 状态条里的一句话原因。
 *
 * 与 deliveryReasonText 的分工：那个是「码 → 人话」，未知码原样返回（给单条投递的
 * 明细用，排障要能 grep 到原始码）；这里是「状态条上的一句话」，未知码必须**说明它
 * 是未知码**，否则 `失败 1（boom）` 会被读成一句没翻译完的文案。空原因返回空串 ——
 * 调用方不要拼出 `失败 1（）` 这种空括号。
 */
export function deliveryReasonInline(reason?: string | null): string {
  const raw = (reason ?? '').trim()
  if (!raw) return ''
  const known = deliveryReasonText(raw)
  return known === raw ? `未知原因：${raw}` : known
}

/**
 * 一轮 @ 的投递分类（P6-10）。语义（与 Go 的 agenthub.Delivery 对齐）：
 * - enqueued（已入队）= queued + running + done —— 「@all 成功 = 已入队」的口径；
 * - pending（未回复）= queued + running —— 还没拿到回复的（排队/进行中）；
 * - skipped 是护栏**有意**不跑（pair/depth/budget/queue_full…），与 failed 分开：
 *   前者是设计，后者是出错，混在一起会让人以为护栏坏了。
 */
export interface DeliveryTally {
  /** 已入队 = queued + running + done（未知状态不计入，避免把「已入队」算多）。 */
  enqueued: number
  queued: number
  running: number
  done: number
  skipped: number
  failed: number
  /** 未回复 = queued + running。 */
  pending: number
  /** 本批投递条数（含未知状态）。 */
  total: number
  /** 第一条 failed 的原因码（状态条只放一句话；没有失败则空）。 */
  failureReason: string
  /** 每条投递的一句话，换行分隔 —— 状态条 hover 展开「谁在跑、谁跳过、为什么」。 */
  detail: string
}

/** 把最近一轮投递归到几档里；没有账本时全 0（调用方据此决定显示哪些段）。 */
export function deliveryTally(conv?: HubConversation | null, names?: Record<string, string>): DeliveryTally {
  const batch = lastDeliveryBatch(conv?.deliveries ?? [])
  const t: DeliveryTally = {
    enqueued: 0, queued: 0, running: 0, done: 0, skipped: 0, failed: 0,
    pending: 0, total: batch.length, failureReason: '', detail: '',
  }
  for (const d of batch) {
    switch (d.status) {
      case 'queued': t.queued++; break
      case 'running': t.running++; break
      case 'done': t.done++; break
      case 'skipped': t.skipped++; break
      case 'failed': t.failed++; break
      default: break
    }
    if (!t.failureReason && d.status === 'failed' && (d.reason ?? '').trim()) t.failureReason = d.reason as string
  }
  t.pending = t.queued + t.running
  t.enqueued = t.queued + t.running + t.done
  t.detail = batch.map((d) => deliveryLabel(d, names)).filter(Boolean).join('\n')
  return t
}

/**
 * 投递一档的文案：`已入队 5 · 已回复 2 · 未回复 2 · 失败 1`。
 * 只写非零的档（`已入队 0` 是噪音），全 0 返回空串 —— 空态不占一行。
 */
export function deliveryTallyText(t: DeliveryTally): string {
  const parts: string[] = []
  if (t.enqueued > 0) parts.push(`已入队 ${t.enqueued}`)
  if (t.done > 0) parts.push(`已回复 ${t.done}`)
  if (t.pending > 0) parts.push(`未回复 ${t.pending}`)
  if (t.failed > 0) parts.push(`失败 ${t.failed}`)
  if (t.skipped > 0) parts.push(`跳过 ${t.skipped}`)
  return parts.join(' · ')
}

// ── 状态条模型（P6-10）────────────────────────────────────────────────────────
// 一条状态条说清楚：本轮 / 本链 / 队列 / pair（含恢复倒计时）/ 投递（已入队·已回复·
// 未回复·失败·跳过）/ 终态，外加「停止」与「重置本群限制」两个门禁。
// 组件只负责把 model 画出来 —— 文案、空态、门禁全在这里，Node 直测。

export type ConversationSegmentKey = 'round' | 'chain' | 'queue' | 'pair' | 'delivery' | 'terminal'

export interface ConversationBarSegment {
  key: ConversationSegmentKey
  text: string
  /** hover 展开的口径/原因（一段投递明细可能是多行）。 */
  title: string
  tone: 'warn' | 'danger' | ''
}

export interface ConversationBarModel {
  /** 没有任何可说的（空闲且无 run）→ 整条不渲染。 */
  visible: boolean
  tone: ConversationTone
  /** 左侧状态标签（空闲/自动对话中/等你继续/未知状态（x）…）。 */
  status: string
  /** 状态标签的 hover 说明（未知状态码/无对话等空态在这里给可读文案）。 */
  statusTitle: string
  segments: ConversationBarSegment[]
  canStop: boolean
  /** 到限/等你继续/护栏命中 → 显示「重置本群限制」。 */
  canReset: boolean
  resetTitle: string
}

export const CONVERSATION_RESET_TITLE = '重置本群对话限制（清零互踢计数与深度，不唤醒任何人）'

/**
 * 组装状态条。所有判断都在这里，组件保持无逻辑：
 * - visible：有 run 在跑 / 对话不是 idle / 有未回复或失败的投递 / 有可重置的护栏；
 * - 未知状态码显示成「未知状态（x）」而不是裸码；
 * - segments 里不会出现空文案（空态不占位，也不留悬空的分隔符）。
 */
export function conversationBarModel(opts: {
  conversation?: HubConversation | null
  pairNames?: Record<string, string>
  active?: boolean
  now?: number
}): ConversationBarModel {
  const conv = opts.conversation ?? null
  const now = opts.now ?? Date.now()
  const active = opts.active === true
  const rawStatus = (conv?.status ?? '').trim()
  const synthetic = rawStatus === '' || rawStatus === 'idle'
  const tally = deliveryTally(conv, opts.pairNames)
  const limit = conv?.pair_limit && conv.pair_limit > 0 ? conv.pair_limit : 10
  const counts = Object.values(conv?.pair_count ?? {})
    .map((n) => Number(n))
    .filter((n) => Number.isFinite(n))
  const pairAtLimit = counts.some((n) => n >= limit)
  const { depth, max } = depthPair(conv)
  const depthAtLimit = depth > 0 && depth >= max
  const canReset = rawStatus === 'waiting_human' || rawStatus === 'exhausted' || pairAtLimit || depthAtLimit
  const visible = active || !synthetic || canReset || tally.pending > 0 || tally.failed > 0
  const tone: ConversationTone = synthetic ? (active ? 'running' : 'idle') : conversationTone(rawStatus)
  const status = synthetic ? (active ? '任务运行中' : '空闲') : conversationStatusLabel(rawStatus)
  const statusTitle = synthetic
    ? (active ? '当前有 run 在跑，可随时停止' : '本群当前没有自动对话')
    : tone === 'unknown'
      ? `后端返回了未知的对话状态码（${rawStatus}），状态条只显示原始值`
      : ''

  const segments: ConversationBarSegment[] = []
  if (!synthetic) {
    // 未知状态码时**不画**本轮/本链/队列：这几个数在未知状态下没有已知含义，摆一排
    // 0 只会让人以为「什么都没发生」。状态标签 + hover 里的原始码已经够排障。
    if (tone !== 'unknown') {
      segments.push({ key: 'round', text: conversationRoundText(conv), title: '本轮：人类一次发言后自动接龙的轮数', tone: '' })
      segments.push({
        key: 'chain',
        text: conversationChainText(conv),
        title: '本链：当前接龙链的深度（防无限交接）',
        tone: depthAtLimit ? 'warn' : '',
      })
      segments.push({
        key: 'queue',
        text: conversationQueueText(conv),
        title: '队列：排队等目标空闲的 @（目标忙/限流退避）',
        tone: (conv?.queue_len ?? 0) > 0 ? 'warn' : '',
      })
    }
    const pair = conversationPairText(conv, opts.pairNames, now)
    if (pair) {
      segments.push({
        key: 'pair',
        text: pair,
        title: '同一对智能体在滚动窗口内的往返次数（防踢皮球）；到限后窗口滚过即自动恢复',
        tone: pairAtLimit ? 'warn' : '',
      })
    }
    const delivery = deliveryTallyText(tally)
    if (delivery) {
      const reason = deliveryReasonInline(tally.failureReason)
      segments.push({
        key: 'delivery',
        text: reason ? `${delivery}（${reason}）` : delivery,
        title: tally.detail || '本轮 @ 的投递账本（每个目标一条）',
        tone: tally.failed > 0 ? 'danger' : tally.skipped > 0 ? 'warn' : '',
      })
    }
    const terminal = conversationTerminalText(conv)
    if (terminal) {
      segments.push({
        key: 'terminal',
        text: terminal,
        title: '终态：新的人类消息或「重置本群限制」可继续',
        tone: rawStatus === 'exhausted' ? 'warn' : '',
      })
    }
  }

  return {
    visible,
    tone,
    status,
    statusTitle,
    segments,
    canStop: conversationCanStop(conv, active),
    canReset,
    resetTitle: CONVERSATION_RESET_TITLE,
  }
}
