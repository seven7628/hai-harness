import type { CSSProperties } from 'react'
import { useT } from '../i18n'
import type { HubMessage } from './types'
import type { MentionAgent } from './mentions'
import { uniqueContextSenders } from './groupProjection'

// 回复的触发上下文（P3b-07，方案 §8.3.5 TurnContextLine）。
// 只有 ≥2 个**不同**触发者时才显示——单人回复没什么可解释的。
// 群消息头用 compact 只画「↳ N」，不再单独占一行；trigger_seqs 放进 title/tooltip，
// 点击只跳到触发消息并高亮（B6），不打开 DM。

export default function TurnContextLine({
  msg,
  roster,
  triggerSeqBySender,
  onJumpToSeq,
  compact = false,
}: {
  msg: HubMessage
  roster?: MentionAgent[]
  /** agent_id → 触发消息 seq（由已加载消息 + context.trigger_seqs 反推）。 */
  triggerSeqBySender?: Record<string, number>
  onJumpToSeq?: (seq: number) => void
  /** 群消息头里的紧凑入口：只显示「↳ N」，不占一行。 */
  compact?: boolean
}) {
  const t = useT()
  const senders = uniqueContextSenders(msg.context?.senders)
  if (senders.length < 2) return null
  const byID = new Map((roster ?? []).map((a) => [a.id, a]))
  const seqs = msg.context?.trigger_seqs ?? []
  const tip = seqs.length > 0
    ? t('hub.contextTriggerSeqs').replace('{seqs}', seqs.join(', #'))
    : t('hub.contextTriggerMulti')
  if (compact) {
    const names = senders.map((s) => s.name || byID.get(s.agent_id ?? '')?.name || s.agent_id || '成员')
    const first = senders[0]
    const seq = (first.agent_id ? triggerSeqBySender?.[first.agent_id] : 0) || seqs[0] || 0
    const label = t('hub.contextJump').replace('{names}', names.join('、'))
    const body = <span aria-hidden="true">↳ {senders.length}</span>
    if (onJumpToSeq && seq > 0) {
      return (
        <button
          type="button"
          className="hub-context-jump"
          title={tip}
          aria-label={label}
          onClick={() => onJumpToSeq(seq)}
        >
          {body}
        </button>
      )
    }
    return <span className="hub-context-jump static" title={tip} role="note" aria-label={label}>{body}</span>
  }
  return (
    <div className="hub-context-line" title={tip} aria-label={t('hub.contextJump').replace('{names}', senders.map((s) => s.name || byID.get(s.agent_id ?? '')?.name || s.agent_id).join('、'))}>
      <span className="hub-context-label">{t('hub.contextFrom')}</span>
      {senders.map((s) => {
        const agent = s.agent_id ? byID.get(s.agent_id) : undefined
        const name = s.name || agent?.name || s.agent_id || '成员'
        const accent = agent?.accent || 'var(--text-3)'
        const style = { '--context-accent': accent } as CSSProperties
        const seq = (s.agent_id ? triggerSeqBySender?.[s.agent_id] : 0) || seqs[0] || 0
        const content = (
          <>
            <span className="hub-context-dot" aria-hidden="true" />
            {name}
          </>
        )
        return onJumpToSeq && seq > 0 ? (
          <button
            key={s.agent_id || name}
            type="button"
            className="hub-context-chip"
            style={style}
            title={tip}
            aria-label={t('hub.contextJumpTo').replace('{name}', name).replace('{seq}', String(seq))}
            onClick={() => onJumpToSeq(seq)}
          >
            {content}
          </button>
        ) : (
          <span key={s.agent_id || name} className="hub-context-chip" style={style} title={tip}>
            {content}
          </span>
        )
      })}
      <span className="hub-context-label">{t('hub.contextOfMessage')}</span>
    </div>
  )
}
