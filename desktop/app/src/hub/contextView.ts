import type { HubMessage } from './types'
import { uniqueContextSenders } from './groupProjection'

// Context 只读面板的纯数据（P4-02）：消息级触发上下文 + L1–L4 提示词分层。
// Node 直测（scripts/test-hub-context.mjs）；组件只负责画。
//
// 口径：
//   - 只有「有上下文」的消息才进选择器（trigger_seqs / senders / reason / round_id 任一非空）；
//   - 人类自己发的消息不是被触发的一方，不进候选；
//   - 触发 seq 只保留正整数并去重（后端的 trigger_seqs 可能有 0/重复）；
//   - 发送者名字缺省时用名册 id→name 兜底，再退 agent_id，最后「成员」。

export interface ContextSenderView {
  id: string
  name: string
}

export interface MessageContextView {
  seq: number
  roundId: string
  reason: string
  senders: ContextSenderView[]
  triggerSeqs: number[]
  /** 这四条里任意一条非空：面板据此显示「这条没有触发上下文」。 */
  hasContext: boolean
}

function positiveSeqs(seqs: number[] | null | undefined): number[] {
  const out: number[] = []
  const seen = new Set<number>()
  for (const raw of seqs ?? []) {
    const n = Math.trunc(Number(raw))
    if (!Number.isSafeInteger(n) || n <= 0 || seen.has(n)) continue
    seen.add(n)
    out.push(n)
  }
  return out
}

export function messageContextView(
  msg: HubMessage | null | undefined,
  names?: Record<string, string> | null,
): MessageContextView {
  const triggerSeqs = positiveSeqs(msg?.context?.trigger_seqs)
  const senders = uniqueContextSenders(msg?.context?.senders).map((s) => {
    const id = String(s.agent_id ?? '').trim()
    const explicit = String(s.name ?? '').trim()
    const name = explicit || (id ? names?.[id] : '') || id || '成员'
    return { id, name }
  })
  const reason = String(msg?.context?.reason ?? '').trim()
  const roundId = String(msg?.round_id ?? '').trim()
  return {
    seq: Number(msg?.seq) > 0 ? Number(msg?.seq) : 0,
    roundId,
    reason,
    senders,
    triggerSeqs,
    hasContext: triggerSeqs.length > 0 || senders.length > 0 || reason !== '' || roundId !== '',
  }
}

export function messageHasContext(msg: HubMessage | null | undefined): boolean {
  return messageContextView(msg).hasContext
}

/**
 * 可选择的消息（最新在前）。limit 只截最近 N 条，避免把整段历史塞进下拉。
 * pending/failed（乐观气泡）与 seq<=0 一律排除：它们还没有可定位的真实位置。
 */
export function contextMessageOptions(messages: HubMessage[] | null | undefined, limit = 20): HubMessage[] {
  const pool = (messages ?? []).filter(
    (m) =>
      !!m &&
      !m.pending &&
      !m.failed &&
      Number(m.seq) > 0 &&
      m.sender?.kind !== 'human' &&
      messageHasContext(m),
  )
  const cap = Math.max(1, Math.trunc(limit) || 20)
  return pool.slice(-cap).reverse()
}

/** 当前选中项：选了就尊重选择，否则默认最新一条（仍是 options[0]）。 */
export function pickContextMessage(
  messages: HubMessage[] | null | undefined,
  selectedSeq?: number | null,
  limit = 20,
): HubMessage | null {
  const options = contextMessageOptions(messages, limit)
  if (options.length === 0) return null
  const want = Math.trunc(Number(selectedSeq))
  if (want > 0) {
    const hit = options.find((m) => Number(m.seq) === want)
    if (hit) return hit
  }
  return options[0]
}

/**
 * 触发 seq 的定位落点：优先「已在内存里」的第一个，否则退回第一个合法 seq
 *（消息不在当前页时仍显示 chip，由调用方提示/尝试加载）。
 */
export function resolveTriggerJump(
  seqs: number[] | null | undefined,
  messages: Array<Pick<HubMessage, 'seq'>> | null | undefined,
): number {
  const wanted = positiveSeqs(seqs)
  if (wanted.length === 0) return 0
  const loaded = new Set((messages ?? []).map((m) => Number(m?.seq)))
  for (const seq of wanted) if (loaded.has(seq)) return seq
  return wanted[0] ?? 0
}

// —— 提示词分层（hub_agent_prompt_preview.layers）——

export interface PromptLayerInput {
  key?: string
  title?: string
  text?: string
}

export interface PromptLayerView {
  key: string
  /** L1–L4（从标题里的 `L2 ·` 提取；标题没有时按 key 兜底，仍没有就是空串）。 */
  label: string
  title: string
  text: string
  chars: number
}

/** 后端 PromptLayer.Key → 层号的兜底映射（标题正常时优先用标题里的 L 号）。 */
const LAYER_KEY_LABELS: Record<string, string> = {
  protocol: 'L1',
  roster: 'L2',
  identity: 'L2',
  environment: 'L3',
  memory: 'L4',
}

export function promptLayerLabel(key?: string | null, title?: string | null): string {
  const m = /(^|[\s·•:：(（])(L[0-9])(?=$|[\s·•)）:：])/.exec(String(title ?? ''))
  if (m) return m[2]
  return LAYER_KEY_LABELS[String(key ?? '').trim()] ?? ''
}

/** 归一化预览层：丢掉空层、保留后端顺序、给出层号与字符数。 */
export function promptLayerViews(layers?: PromptLayerInput[] | null): PromptLayerView[] {
  const out: PromptLayerView[] = []
  for (const layer of layers ?? []) {
    const key = String(layer?.key ?? '').trim()
    const title = String(layer?.title ?? '').trim()
    const text = String(layer?.text ?? '')
    if (!title && !text) continue
    out.push({ key, label: promptLayerLabel(key, title), title: title || key || '提示词层', text, chars: text.length })
  }
  return out
}

/** 面板底部恒显示的那句：L5 不在预览里（后端 note 有就复用，缺 L5 说明时补上）。 */
export function promptRuntimeNote(serverNote?: string | null): string {
  const base = 'L5 运行时工具 schema 与技能清单由内核在请求期装配，不在预览内。'
  const note = String(serverNote ?? '').trim()
  if (!note) return base
  return note.includes('L5') ? note : `${note} ${base}`
}

/** 预览总字节：优先后端的 total_bytes，缺失时按文本长度估（只是展示）。 */
export function promptPreviewBytes(totalBytes?: number | null, layers?: PromptLayerView[] | null): number {
  const n = Math.trunc(Number(totalBytes ?? 0))
  if (Number.isFinite(n) && n > 0) return n
  return (layers ?? []).reduce((sum, l) => sum + l.chars, 0)
}
