import type { CSSProperties } from 'react'
import type { HubMessage } from './types'
import type { MentionAgent } from './mentions'
import { uniqueContextSenders } from './groupProjection'

// 回复的触发上下文（P3b-07，方案 §8.3.5 TurnContextLine）。
// 只有 ≥2 个**不同**触发者时才显示——单人回复没什么可解释的，多出来一行噪音。
// trigger_seqs 放进 title/tooltip；chip 点击只跳到触发消息并高亮（B6），不打开 DM。

export default function TurnContextLine({
  msg,
  roster,
  triggerSeqBySender,
  onJumpToSeq,
}: {
  msg: HubMessage
  roster?: MentionAgent[]
  /** agent_id → 触发消息 seq（由已加载消息 + context.trigger_seqs 反推）。 */
  triggerSeqBySender?: Record<string, number>
  onJumpToSeq?: (seq: number) => void
}) {
  const senders = uniqueContextSenders(msg.context?.senders)
  if (senders.length < 2) return null
  const byID = new Map((roster ?? []).map((a) => [a.id, a]))
  const seqs = msg.context?.trigger_seqs ?? []
  const tip = seqs.length > 0 ? `触发消息 #${seqs.join(', #')}` : '由多条消息触发'
  return (
    <div className="hub-context-line" title={tip} aria-label={`来自 ${senders.map((s) => s.name || byID.get(s.agent_id ?? '')?.name || s.agent_id).join('、')} 的消息`}>
      <span className="hub-context-label">来自</span>
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
            title={`${tip} · 点击定位`}
            aria-label={`跳到 ${name} 的触发消息 #${seq}`}
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
      <span className="hub-context-label">的消息</span>
    </div>
  )
}
