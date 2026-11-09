// 群内流式草稿的纯归约（P3a-09，方案 §6.5.8）。
//
// 只做「同一个 message_id 的草稿合并/最终化」这一件事，方便 Node 直接跑回归：
//   - index 单调：迟到/乱序 delta 丢弃；
//   - final 之后到达的 delta 丢弃（finalized 最近 200 个 LRU）；
//   - 并行 agent 各自一条草稿，不合并；
//   - 单条草稿最多 256KB（UTF-8 字节），超出丢后续 delta；final 仍会补齐。

export const HUB_DRAFT_MAX_BYTES = 256 * 1024
export const HUB_FINALIZED_LIMIT = 200

export interface HubMessageDelta {
  group_id?: string
  agent_id?: string
  round_id?: string
  message_id?: string
  index?: number
  delta?: string
}

export interface HubStreamDraft {
  messageId: string
  groupId: string
  agentId: string
  roundId?: string
  text: string
  index: number
  bytes: number
  updatedAt: number
}

function utf8Bytes(s: string): number {
  // 流式正文通常 <4KB；只有接近 256KB 上限时才走 TextEncoder。
  // 无 TextEncoder 的测试环境用 UTF-16 长度近似（上限只用于安全截断）。
  if (typeof TextEncoder !== 'undefined') return new TextEncoder().encode(s).length
  return s.length
}

export function applyHubDelta(
  drafts: HubStreamDraft[],
  ev: HubMessageDelta,
  finalized: readonly string[],
  now = Date.now(),
): HubStreamDraft[] {
  const messageId = typeof ev.message_id === 'string' ? ev.message_id : ''
  const groupId = typeof ev.group_id === 'string' ? ev.group_id : ''
  const agentId = typeof ev.agent_id === 'string' ? ev.agent_id : ''
  const delta = typeof ev.delta === 'string' ? ev.delta : ''
  const index = typeof ev.index === 'number' && Number.isInteger(ev.index) ? ev.index : -1
  if (!messageId || !groupId || !agentId || !delta || index < 0) return drafts
  if (finalized.includes(messageId)) return drafts

  const at = drafts.findIndex((d) => d.messageId === messageId)
  if (at < 0) {
    // 第一条必须 index=0；首块丢失时不凭空造半截草稿，等 final 完整渲染。
    if (index !== 0) return drafts
    const bytes = utf8Bytes(delta)
    if (bytes > HUB_DRAFT_MAX_BYTES) return drafts
    return [...drafts, {
      messageId, groupId, agentId,
      roundId: typeof ev.round_id === 'string' ? ev.round_id : undefined,
      text: delta, index, bytes, updatedAt: now,
    }]
  }
  const cur = drafts[at]
  if (index <= cur.index) return drafts
  const add = utf8Bytes(delta)
  if (cur.bytes + add > HUB_DRAFT_MAX_BYTES) return drafts
  const next = drafts.slice()
  next[at] = { ...cur, text: cur.text + delta, index, bytes: cur.bytes + add, updatedAt: now }
  return next
}

/** final 到达：删草稿，并把 message_id 放进最近 200 的 finalized LRU。 */
export function finalizeHubDraft(
  drafts: HubStreamDraft[],
  finalized: readonly string[],
  messageId: string | undefined,
): { drafts: HubStreamDraft[]; finalized: string[] } {
  const id = typeof messageId === 'string' ? messageId : ''
  if (!id) return { drafts, finalized: [...finalized] }
  const nextFinalized = [id, ...finalized.filter((x) => x !== id)].slice(0, HUB_FINALIZED_LIMIT)
  return { drafts: drafts.filter((d) => d.messageId !== id), finalized: nextFinalized }
}

/** agent_end 兜底：空回复/中断没有 final，按 agent 清掉本轮所有草稿。 */
export function dropDraftsForAgent(drafts: HubStreamDraft[], agentId: string): HubStreamDraft[] {
  if (!agentId) return drafts
  const next = drafts.filter((d) => d.agentId !== agentId)
  return next.length === drafts.length ? drafts : next
}

/**
 * 重连/重进群后的对账（P4-01）：断线期间可能只收到 delta、错过 final（甚至错过
 * agent_end）。重拉历史后，历史里已有的 message_id 说明这条消息已经落地 ——
 * 对应草稿必须删掉并记入 finalized，否则迟到的 delta 还能把它复活成幽灵气泡。
 * 只处理「历史里真的有」的 id：历史缺页时不能误杀仍在流式的草稿。
 */
export function reconcileDraftsWithMessages(
  drafts: HubStreamDraft[],
  finalized: readonly string[],
  messageIds: readonly string[],
): { drafts: HubStreamDraft[]; finalized: string[] } {
  if (drafts.length === 0 || messageIds.length === 0) return { drafts, finalized: [...finalized] }
  const landed = new Set(messageIds)
  const stale = drafts.filter((d) => landed.has(d.messageId)).map((d) => d.messageId)
  if (stale.length === 0) return { drafts, finalized: [...finalized] }
  let nextDrafts = drafts
  let nextFinalized: readonly string[] = finalized
  for (const id of stale) {
    const r = finalizeHubDraft(nextDrafts, nextFinalized, id)
    nextDrafts = r.drafts
    nextFinalized = r.finalized
  }
  return { drafts: nextDrafts, finalized: [...nextFinalized] }
}

/** 从 session_id 解析群作用域：hub.<agent>.g.<group>。 */
export function groupScopeFromSession(sessionId: string | undefined): { agentId: string; groupId: string } | null {
  if (!sessionId) return null
  const m = /^hub\.([^.]+)\.g\.([^.]+)$/.exec(sessionId)
  return m ? { agentId: m[1], groupId: m[2] } : null
}
