import type { HubMessage, HubSearchCursor, HubSearchHit, HubSearchPage, HubSearchScope } from './types'

// Hub 搜索的纯逻辑（P4-04b-1）。
//
// 为什么单独一层：搜索结果映射、游标、点击命中后「加载 seq 附近一页」的合并
// 都有边界（跨会话/乐观气泡/重复命中/空页），放组件里就只能靠手点验证。
// 这里全部是纯函数，scripts/test-hub-search.mjs 直接在 Node 里跑。

/** 输入 debounce：设计定稿 250ms（桌面与手机共用口径）。 */
export const HUB_SEARCH_DEBOUNCE_MS = 250
/** 一次全局搜索的默认条数（bridge 默认值一致）。 */
export const HUB_SEARCH_LIMIT = 50
/** 点在历史里的命中时，加载「命中 seq 附近」的一页。 */
export const HUB_SEARCH_AROUND_LIMIT = 50

const SCOPES: HubSearchScope[] = ['all', 'group', 'dm']

export function normalizeSearchQuery(raw: unknown): string {
  return typeof raw === 'string' ? raw.trim() : ''
}

export function normalizeSearchScope(raw: unknown): HubSearchScope {
  const v = typeof raw === 'string' ? raw.trim() : ''
  return (SCOPES as string[]).includes(v) ? (v as HubSearchScope) : 'all'
}

export function searchScopeLabel(scope: HubSearchScope): string {
  if (scope === 'group') return '群聊'
  if (scope === 'dm') return '私聊'
  return '全部'
}

/**
 * 原始命中 → 领域命中。非法/缺关键字段的条目返回 null（不让一条坏数据
 * 把整页渲染打崩）。scope 决定 group_id/agent_id 哪个是目标。
 */
export function mapSearchHit(raw: Partial<HubSearchHit> | null | undefined): HubSearchHit | null {
  if (!raw || typeof raw !== 'object') return null
  const scope: HubSearchHit['scope'] = raw.scope === 'dm' ? 'dm' : 'group'
  const groupId = typeof raw.group_id === 'string' ? raw.group_id : ''
  const agentId = typeof raw.agent_id === 'string' ? raw.agent_id : ''
  if (scope === 'group' ? !groupId : !agentId) return null
  const seq = Math.trunc(Number(raw.seq))
  if (!Number.isSafeInteger(seq) || seq <= 0) return null
  const snippet = typeof raw.snippet === 'string' ? raw.snippet : ''
  const messageId = typeof raw.message_id === 'string' ? raw.message_id : ''
  return {
    scope,
    ...(groupId ? { group_id: groupId } : {}),
    ...(agentId ? { agent_id: agentId } : {}),
    name: typeof raw.name === 'string' ? raw.name : '',
    seq,
    message_id: messageId || `msg_${seq}`,
    snippet,
    sender: typeof raw.sender === 'string' ? raw.sender : '',
    sender_kind: typeof raw.sender_kind === 'string' ? raw.sender_kind : '',
    sender_agent_id: typeof raw.sender_agent_id === 'string' ? raw.sender_agent_id : '',
    kind: typeof raw.kind === 'string' ? raw.kind : '',
    created_at: typeof raw.created_at === 'string' ? raw.created_at : '',
    owner_only: raw.owner_only === true,
  }
}

/** 命中的会话键：同一条消息在不同 scope 下不会撞键。 */
export function searchHitKey(hit: Pick<HubSearchHit, 'scope' | 'group_id' | 'agent_id' | 'message_id' | 'seq'>): string {
  const conv = hit.scope === 'group' ? (hit.group_id ?? '') : (hit.agent_id ?? '')
  return `${hit.scope}:${conv}:${hit.message_id || `msg_${hit.seq}`}`
}

/** 翻页去重：同一轮重复返回（游标边界）的命中只留第一条。 */
export function mergeSearchHits(existing: HubSearchHit[], incoming: HubSearchHit[]): HubSearchHit[] {
  const out: HubSearchHit[] = []
  const seen = new Set<string>()
  for (const hit of [...(existing ?? []), ...(incoming ?? [])]) {
    const key = searchHitKey(hit)
    if (seen.has(key)) continue
    seen.add(key)
    out.push(hit)
  }
  return out
}

/** hub_search 响应 → 页面状态（非法条目丢掉，游标只在 has_more 时保留）。 */
export function mapSearchPage(raw: HubSearchPage | null | undefined): {
  hits: HubSearchHit[]
  hasMore: boolean
  next: HubSearchCursor | null
} {
  const hits = (raw?.hits ?? [])
    .map((h) => mapSearchHit(h))
    .filter((h): h is HubSearchHit => h !== null)
  const hasMore = raw?.has_more === true && hits.length > 0
  const cursor = raw?.next_before
  const next =
    hasMore && cursor && typeof cursor.id === 'string' && cursor.id && typeof cursor.created_at === 'string' && cursor.created_at
      ? { created_at: cursor.created_at, id: cursor.id, seq: Number(cursor.seq) || 0 }
      : null
  return { hits, hasMore: hasMore && next !== null, next }
}

/** hub_search payload。cursor 为 null = 第一页。 */
export function searchPagePayload(
  query: string,
  scope: HubSearchScope,
  opts: { cursor?: HubSearchCursor | null; limit?: number; groupId?: string; agentId?: string } = {},
): Record<string, unknown> {
  const q = normalizeSearchQuery(query)
  const payload: Record<string, unknown> = { query: q, scope: normalizeSearchScope(scope) }
  if (opts.groupId) payload.group_id = opts.groupId
  if (opts.agentId) payload.agent_id = opts.agentId
  payload.limit = Math.max(1, Math.min(200, Math.trunc(opts.limit ?? HUB_SEARCH_LIMIT)))
  const cursor = opts.cursor
  if (cursor && cursor.id && cursor.created_at) {
    payload.before = { created_at: cursor.created_at, id: cursor.id, seq: cursor.seq ?? 0 }
  }
  return payload
}

export interface HubSearchAroundPlan {
  /** 复用已加载历史的两条命令之一（群 / DM）。 */
  command: 'hub_group_history' | 'hub_history'
  payload: Record<string, unknown>
  limit: number
}

/**
 * 点击命中后「加载命中附近一页」的请求计划：
 * before_seq = seq + 1 ⇒ 返回以命中 seq 结尾的一页（含命中本身）。
 * 不用 seq 当 before_seq，否则会把命中自己排除掉。
 */
export function searchAroundPlan(
  scope: HubSearchHit['scope'],
  targetId: string,
  seq: number,
  limit = HUB_SEARCH_AROUND_LIMIT,
): HubSearchAroundPlan | null {
  const n = Math.trunc(Number(seq))
  if (!targetId || !Number.isSafeInteger(n) || n <= 0) return null
  const size = Math.max(1, Math.min(200, Math.trunc(limit)))
  return scope === 'group'
    ? { command: 'hub_group_history', payload: { group_id: targetId, before_seq: n + 1, limit: size }, limit: size }
    : { command: 'hub_history', payload: { agent_id: targetId, before_seq: n + 1, limit: size }, limit: size }
}

/** 消息里是否已经有这个 seq（乐观气泡 seq<=0 不算）。 */
export function hasSeq(messages: Array<Pick<HubMessage, 'seq'>> | null | undefined, seq: number): boolean {
  const n = Math.trunc(Number(seq))
  if (!(n > 0)) return false
  return (messages ?? []).some((m) => Math.trunc(Number(m?.seq)) === n)
}

/**
 * 把「命中附近一页」合进当前时间线：
 *   - 按 seq 去重（事件可能已经先到）；
 *   - seq<=0 的乐观/失败气泡保留；
 *   - 逆序或乱序页都按 seq 升序重排。
 * 不做「整段替换」：切走再切回/直播新消息都靠同一份列表，替换会丢新消息。
 */
export function mergeAroundPage(
  messages: HubMessage[] | null | undefined,
  page: HubMessage[] | null | undefined,
): HubMessage[] {
  const byKey = new Map<string, HubMessage>()
  const order: string[] = []
  const add = (m: HubMessage | null | undefined) => {
    if (!m) return
    const seq = Math.trunc(Number(m.seq))
    const key = typeof m.id === 'string' && m.id ? `id:${m.id}` : `seq:${seq}`
    if (!byKey.has(key)) order.push(key)
    byKey.set(key, m)
  }
  for (const m of page ?? []) add(m)
  for (const m of messages ?? []) {
    if (byKey.has(typeof m.id === 'string' && m.id ? `id:${m.id}` : `seq:${Math.trunc(Number(m.seq))}`)) {
      // 已有（可能是先到的直播消息）：保留先到的版本，只补页里没有的。
      continue
    }
    add(m)
  }
  return order
    .map((k) => byKey.get(k))
    .filter((m): m is HubMessage => !!m)
    .sort((a, b) => Number(a.seq) - Number(b.seq))
}

/** 合并后是否还有更早的历史（最小真实 seq > 1）。 */
export function aroundHasMore(messages: Array<Pick<HubMessage, 'seq'>> | null | undefined): boolean {
  const seqs = (messages ?? []).map((m) => Math.trunc(Number(m?.seq))).filter((n) => n > 0)
  if (seqs.length === 0) return false
  return Math.min(...seqs) > 1
}
