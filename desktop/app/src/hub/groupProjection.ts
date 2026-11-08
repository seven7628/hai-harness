import type { HubDMInfo, HubGroupProgress, HubMessage, HubTaskUpdate } from './types'
import { parseTaskResult, taskOutcomeLine } from './taskCard'

// 群左栏投影 + 「当前是否正在看」的纯归约（P3b-07 B4/N1/N2/N10）。
//
// store 只负责把命令/事件拼进来；「未读算不算、预览取哪条、迟到任务更新能不能
// 复活 last_outcome」全部在这里判定，Node 直测（scripts/test-hub-p3b.mjs）。
// 口径与 Go agenthub.GroupPerception 对齐：
//   - 可用消息 = 非空正文 + 非 UI-only（routing_receipt/round_summary/approval_stub）；
//   - 未读 = seq > 已读的可用消息，不数人自己发的；
//   - last_text = 最后一条可用消息；last_outcome 只有「最新可用消息就是任务卡」才非空；
//   - last_task_* 跟随最近一张任务卡，不因后续普通消息而消失。

const UI_ONLY_KINDS = new Set(['routing_receipt', 'round_summary', 'approval_stub'])

/** Hub 打开且当前 target 就是这个 id 才算「正在看」；关闭 Hub 后一律算未读。 */
export function isHubTargetActive(
  open: boolean,
  activeId: string | null | undefined,
  id: string | null | undefined,
): boolean {
  return open === true && !!id && activeId === id
}

/** 是否计入左栏预览/未读：UI-only 回执和空正文不算。 */
export function isUsableGroupMessage(msg: Pick<HubMessage, 'kind' | 'text'> | null | undefined): boolean {
  if (!msg) return false
  if (UI_ONLY_KINDS.has(msg.kind ?? '')) return false
  return (msg.text ?? '').trim() !== ''
}

/** 单行预览：空白折叠 + 按 rune 截断。 */
export function previewText(text: string | undefined | null, max = 80): string {
  const flat = (text ?? '').replace(/\s+/g, ' ').trim()
  const runes = Array.from(flat)
  return runes.length > max ? runes.slice(0, Math.max(0, max - 1)).join('') + '…' : flat
}

/**
 * 一条群消息落到左栏投影。
 * - active=true：正在看，未读保持 0（不因自己的浏览再累加）；
 * - 重复/乱序 seq（<= last_seq）直接返回原对象（调用方可按引用判等）；
 * - 普通消息会清空 last_outcome（回退到 last_text），任务卡才重新给出结论。
 */
export function projectGroupMessage(
  info: HubGroupProgress,
  msg: HubMessage,
  active: boolean,
): HubGroupProgress {
  if (!isUsableGroupMessage(msg)) return info
  const seq = Number(msg.seq ?? 0)
  if (!(seq > 0) || seq <= (info.last_seq ?? 0)) return info
  const next: HubGroupProgress = {
    ...info,
    last_seq: seq,
    last_sender: msg.sender?.kind,
    last_at: msg.created_at ?? new Date().toISOString(),
    last_text: previewText(msg.text),
  }
  const card = msg.kind === 'task_result' ? parseTaskResult(msg) : null
  next.last_outcome = card ? taskOutcomeLine(card.status, card.summary, card.error) : ''
  if (card) {
    next.last_task_id = card.taskId || info.last_task_id
    next.last_task_status = card.status
    next.last_task_summary = card.summary || card.title
    next.last_task_seq = seq
  } else {
    // 最新可用消息不是任务卡：结论清空，但 last_task_* 保留（后端同口径）。
    next.last_task_seq = 0
  }
  if (active) next.unread = 0
  else if (msg.sender?.kind !== 'human') next.unread = (info.unread ?? 0) + 1
  return next
}

/**
 * hub_task_update 对左栏的影响：只更新「最近一张任务卡」的字段。
 * 迟到更新绝不能把 last_outcome 写回来 —— 只有那张卡仍是最新可用消息时才重算。
 */
export function projectTaskUpdate(info: HubGroupProgress, update: HubTaskUpdate): HubGroupProgress {
  const taskID = update.task_id || update.task?.id || ''
  if (!taskID || info.last_task_id !== taskID) return info
  const status = update.status || update.task?.status || ''
  const summary = update.task?.result?.summary ?? ''
  const error = update.error || update.task?.error || update.task?.result?.error || ''
  const next: HubGroupProgress = { ...info }
  if (status) next.last_task_status = status
  if (summary) next.last_task_summary = summary
  // 刷新得到的 progress 没有 last_task_seq：last_outcome 非空就说明卡是最新。
  const cardIsLatest = info.last_task_seq
    ? info.last_task_seq === info.last_seq
    : !!info.last_outcome
  if (cardIsLatest) {
    next.last_outcome = taskOutcomeLine(status, summary || info.last_task_summary || '', error)
  }
  return next
}

/** hub_read：seq<=0 一律忽略；只前进不清退（seq < last_seq 时仍有更新的未读）。 */
export function projectGroupRead(info: HubGroupProgress, seq: number): HubGroupProgress {
  const read = Number(seq)
  if (!(read > 0)) return info
  if (read < (info.last_seq ?? 0)) return info
  if ((info.unread ?? 0) === 0) return info
  return { ...info, unread: 0 }
}

/**
 * loadGroups 的竞态守卫：事件可能比列表响应先到（§7.5.7）。
 * - last_seq 更旧 → 丢弃；
 * - last_seq 更新 → 采纳；
 * - 相同 last_seq：只有本地在请求开始后没再更新过，才用服务端快照覆盖
 *   （否则会把刚到的任务结论/未读覆盖回旧值）。这与「完成时间/修订」等价，
 *   因为 progress 本身没有 revision 字段。
 */
export function shouldApplyGroupProgress(
  local: HubGroupProgress | undefined,
  incoming: HubGroupProgress | undefined,
  localUpdatedAt: number,
  requestStartedAt: number,
): boolean {
  if (!incoming) return false
  if (!local) return true
  const incomingSeq = incoming.last_seq ?? 0
  const localSeq = local.last_seq ?? 0
  if (incomingSeq !== localSeq) return incomingSeq > localSeq
  return (localUpdatedAt ?? 0) <= (requestStartedAt ?? 0)
}

/**
 * hub_agent_list 的 DM 投影竞态守卫（P3b-08，口径同 shouldApplyGroupProgress）：
 * 刷新请求在飞时到达的 hub_message 已经把未读/最后一条推进了，旧响应不能把它覆盖回去。
 * - 服务端 last_seq 更旧 → 丢弃；
 * - 更新的 last_seq（别端刚发过消息）→ 采纳整条；
 * - 相同 last_seq：只有本地在这份请求开始后没再更新过，才用服务端快照覆盖
 *   （同 seq 也能有不同 unread：本地读过/加过未读）。
 */
export function shouldApplyDMInfo(
  local: HubDMInfo | undefined,
  incoming: HubDMInfo | undefined,
  localUpdatedAt: number,
  requestStartedAt: number,
): boolean {
  if (!incoming) return false
  if (!local) return true
  const incomingSeq = incoming.last_seq ?? 0
  const localSeq = local.last_seq ?? 0
  if (incomingSeq !== localSeq) return incomingSeq > localSeq
  return (localUpdatedAt ?? 0) <= (requestStartedAt ?? 0)
}

/** 触发上下文里的发送者（TurnContextLine 只画 ≥2 个不同的人）。 */
export interface ContextSenderRef {
  agent_id?: string
  name?: string
}

/** 去重并保持后端顺序；空 id/name 的幽灵项直接丢掉。 */
export function uniqueContextSenders(senders?: ContextSenderRef[] | null): ContextSenderRef[] {
  const seen = new Set<string>()
  const out: ContextSenderRef[] = []
  for (const s of senders ?? []) {
    const key = s.agent_id || s.name || ''
    if (!key || seen.has(key)) continue
    seen.add(key)
    out.push(s)
  }
  return out
}

/**
 * 触发消息 seq → 发送者 agent_id（B6）。
 * TurnContext 只给 senders + trigger_seqs，不给逐人 seq；从已加载消息里找
 * 「seq 落在 trigger_seqs 内、发送者就是该 agent」的第一条，chip 才能精确跳转。
 * 没有命中（例如触发者是人类）时由调用方回退到 trigger_seqs[0]。
 */
export function triggerSeqBySender(messages: HubMessage[]): Record<string, number> {
  const wanted = new Set<number>()
  for (const m of messages) for (const seq of m.context?.trigger_seqs ?? []) wanted.add(seq)
  const map: Record<string, number> = {}
  for (const m of messages) {
    const id = m.sender?.agent_id
    if (!id || !(m.seq > 0) || !wanted.has(m.seq)) continue
    if (!(id in map)) map[id] = m.seq
  }
  return map
}
