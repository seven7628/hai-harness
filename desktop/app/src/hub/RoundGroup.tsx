import { GroupBubble } from './MessageBubble'
import type { HubMessage } from './types'
import type { MentionAgent } from './mentions'

// 同轮消息只做「间距更紧」的排版分组，不再画外框/表头。
// 每条消息仍由 GroupBubble 独立渲染：不同智能体必须是各自的气泡，
// 「合并」只影响同一轮的间距，不能把多个作者塞进一个框里。
export function RoundGroup({
  messages,
  names,
  grouped,
  roster,
  onSelectAgent,
  triggerSeqBySender,
  onJumpToSeq,
  highlightSeq,
}: {
  messages: HubMessage[]
  names: Record<string, string>
  grouped: boolean
  roster?: MentionAgent[]
  onSelectAgent?: (agentID: string) => void
  triggerSeqBySender?: Record<string, number>
  onJumpToSeq?: (seq: number) => void
  highlightSeq?: number | null
}) {
  const bubbles = messages.map((m) => (
    <GroupBubble
      key={`${m.seq}-${m.clientMsgId ?? ''}`}
      msg={m}
      names={names}
      roster={roster}
      onSelectAgent={onSelectAgent}
      triggerSeqBySender={triggerSeqBySender}
      onJumpToSeq={onJumpToSeq}
      highlightSeq={highlightSeq}
    />
  ))
  if (!grouped || messages.length < 2) return <>{bubbles}</>
  return <div className="hub-round-stack">{bubbles}</div>
}
