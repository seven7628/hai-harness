import type { CSSProperties } from 'react'
import type { HubMessage } from './types'
import type { MentionAgent } from './mentions'

// 回复的触发上下文（P3b-07，方案 §8.3.5 TurnContextLine）。
// 只有 ≥2 个**不同**触发者时才显示——单人回复没什么可解释的，多出来一行噪音。
// trigger_seqs 放进 title/tooltip：可追溯到触发消息，不在正文里塞 #42。

export interface ContextSender {
  agent_id?: string
  name?: string
}

function uniqueContextSenders(senders?: ContextSender[] | null): ContextSender[] {
  const seen = new Set<string>()
  const out: ContextSender[] = []
  for (const s of senders ?? []) {
    const key = s.agent_id || s.name || ''
    if (!key || seen.has(key)) continue
    seen.add(key)
    out.push(s)
  }
  return out
}

export default function TurnContextLine({
  msg,
  roster,
  onSelectAgent,
}: {
  msg: HubMessage
  roster?: MentionAgent[]
  onSelectAgent?: (agentID: string) => void
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
        const content = (
          <>
            <span className="hub-context-dot" aria-hidden="true" />
            {name}
          </>
        )
        return s.agent_id && onSelectAgent && !agent?.deleted ? (
          <button
            key={s.agent_id}
            type="button"
            className="hub-context-chip"
            style={style}
            title={tip}
            onClick={() => onSelectAgent(s.agent_id!)}
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
