import type { HubMessage, HubRound } from './types'

// 群线程的纯逻辑：动作行/轮次聚合只依赖数据，不依赖 React，便于 Node 直测。
export type ThreadItem =
  | { kind: 'message'; msg: HubMessage }
  | { kind: 'round'; round: HubRound }

export function roundHasAction(r: HubRound | null | undefined): boolean {
  return !!r && (!!r.mention_all || (r.targets ?? []).length > 0 || (r.dropped ?? 0) > 0)
}

export function upsertRound(list: HubRound[], round: HubRound | null | undefined): HubRound[] {
  if (!roundHasAction(round) || !round) return list
  const next = list.filter((r) => r.id !== round.id)
  next.push(round)
  next.sort((a, b) => (a.trigger_seq ?? 0) - (b.trigger_seq ?? 0))
  return next
}

export function buildThreadItems(messages: HubMessage[], rounds: HubRound[]): ThreadItem[] {
  const items: ThreadItem[] = []
  const bySeq = new Map<number, HubRound[]>()
  for (const r of rounds) {
    if (!roundHasAction(r)) continue
    const seq = r.trigger_seq ?? 0
    if (!seq) continue
    bySeq.set(seq, [...(bySeq.get(seq) ?? []), r])
  }
  // N3：同一轮已经有「路由」动作行时，紧跟其后的 routing_receipt 不再单独渲染，
  // 否则群里会出现两条「唤醒 Writer」。receipt 由 AppendGroupMessageRouted 紧跟在
  // 触发消息后面写（seq = trigger+1），历史数据也守这个不变式。
  const actionSeqs = new Set<number>()
  for (const r of rounds) {
    if (roundHasAction(r) && (r.trigger_seq ?? 0) > 0) actionSeqs.add(r.trigger_seq as number)
  }
  for (const msg of messages) {
    if (msg.kind === 'routing_receipt' && actionSeqs.has(msg.seq - 1)) continue
    items.push({ kind: 'message', msg })
    for (const r of bySeq.get(msg.seq) ?? []) items.push({ kind: 'round', round: r })
  }
  return items
}

export function roundKey(msg: HubMessage): string {
  if (msg.round_id) return `r:${msg.round_id}`
  const triggers = msg.context?.trigger_seqs ?? []
  if (triggers.length > 0) return `t:${triggers.join(',')}`
  return ''
}

export function parseRoutingText(text: string): { all: boolean; targets: string[]; dropped: number } | null {
  const m = /^(@all\s+)?targets=([^ ]*)(?:\s+dropped=(\d+))?$/.exec(text.trim())
  if (!m) return null
  return {
    all: !!m[1],
    targets: m[2] ? m[2].split(',').filter(Boolean) : [],
    dropped: Number(m[3] ?? 0),
  }
}

export function routingSummary(text: string, names: Record<string, string>): string {
  const parsed = parseRoutingText(text)
  if (!parsed) return text
  const targets = parsed.targets.map((id) => names[id] ?? id)
  return `${parsed.all ? '@all ' : ''}${targets.length ? `唤醒 ${targets.join('、')}` : '无人被唤醒'}${parsed.dropped ? `（丢弃 ${parsed.dropped} 个）` : ''}`
}
