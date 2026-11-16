// 群/DM 流式草稿的纯归约（P3a-09，方案 §6.5.8）。
//
// 只做「同一个 message_id 的草稿合并/最终化」这一件事，方便 Node 直接跑回归：
//   - index 单调：迟到/乱序 delta 丢弃；
//   - final 之后到达的 delta 丢弃（finalized 最近 200 个 LRU）；
//   - 并行 agent / 并行轮次各自一条草稿，不合并；
//   - 单条草稿最多 256KB（UTF-8 字节），超出丢后续 delta；final 仍会补齐。
//
// 作用域（场景2b）：群 delta 带 group_id，DM delta 的 group_id 是**空串**（不是缺字段），
// 两种都收 —— 但都必须有 agent_id，否则不知道草稿属于谁。DM 的多轮正文靠 message_id
// （每轮一个 msg_ id）+ 草稿数组的各一条气泡表达，不合并成一条。

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
  // groupId 允许为空（DM，场景2b）；agentId 必填 —— 没有它就不知道草稿归谁，
  // 群与 DM 的过滤都靠它（DM 靠 agent_id 分桶，群靠 group_id + agent_id）。
  if (!messageId || !agentId || !delta || index < 0) return drafts
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
  // 没有对应草稿时保持原数组：DM 每轮 final 都会走这里，无条件 filter 会让
  // groupDrafts 的引用每轮都变，白白重渲染群视图与消息列表。
  const nextDrafts = drafts.some((d) => d.messageId === id)
    ? drafts.filter((d) => d.messageId !== id)
    : drafts
  return { drafts: nextDrafts, finalized: nextFinalized }
}

/**
 * 哪几条草稿是「此刻真的还在流」的那条（UI 只给它们画光标 / 「正在输入」）。
 *
 * 一次输入里的持续回复会被切成多段草稿（bridge 每个 LLM 轮次开一段、新 message_id），
 * 而 final 要等本轮 AgentEnd 才逐条发下来 —— 所以工作过程中会同时挂着 2~4 条草稿，
 * 前面几条其实已经在 LLMEnd 收口了，只有最后开的那条还在收 delta。全部画光标就是
 * 「一句话还没说完，屏幕上三根光标一起闪」（用户 2026-11 报障的截图）。
 *
 * 判据：同一个 agent 的 LLM 调用严格串行，所以该 agent 的**最后一条**草稿就是活的；
 * 并行 agent 各留各的活草稿（群里两个 agent 同时回复 = 两根光标，这是对的）。
 * 按 groupId + agentId 分桶：群与 DM 的草稿在同一个数组里，互不越界。
 */
export function liveDraftIds(drafts: readonly HubStreamDraft[]): Set<string> {
  const newest = new Map<string, string>()
  for (const d of drafts) {
    // 数组按首块到达顺序追加（finalize 用 filter 保序），后来的覆盖前面的 = 最新段。
    newest.set(`${d.groupId}\u0000${d.agentId}`, d.messageId)
  }
  return new Set(newest.values())
}

/**
 * agent_end 兜底：空回复/中断没有 final，按 agent 清掉本轮所有草稿。
 * groupId 给出时只清该作用域（DM 传空串），否则清该 agent 的全部草稿 ——
 * 同一个 agent 在 DM 与群里可以同时流式，两边的事件互不越界。
 */
export function dropDraftsForAgent(
  drafts: HubStreamDraft[],
  agentId: string,
  groupId?: string,
): HubStreamDraft[] {
  if (!agentId) return drafts
  const next = drafts.filter(
    (d) => d.agentId !== agentId || (groupId !== undefined && d.groupId !== groupId),
  )
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

/**
 * 从 session_id 解析 DM 作用域：hub.<agent>.dm（场景2b）。
 * 群会话（hub.<agent>.g.<group>）与项目/非 Hub 会话都返回 null —— DM 的 agent_end
 * 只能是这个形状，否则会误清群里同名 agent 的草稿。
 */
export function dmScopeFromSession(sessionId: string | undefined): { agentId: string } | null {
  if (!sessionId) return null
  const m = /^hub\.([^.]+)\.dm$/.exec(sessionId)
  return m ? { agentId: m[1] } : null
}
