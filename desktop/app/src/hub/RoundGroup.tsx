import { GroupBubble } from './MessageBubble'
import type { HubMessage } from './types'

// 同轮合并：只有 ≥2 条、且属于同一轮（round_id 或同一组 trigger_seqs）才折叠。
// 单条/无轮次信息保持平铺，避免把普通聊天硬凑成组。
export function RoundGroup({
  messages,
  names,
  grouped,
}: {
  messages: HubMessage[]
  names: Record<string, string>
  grouped: boolean
}) {
  if (!grouped || messages.length < 2) {
    return (
      <>
        {messages.map((m) => <GroupBubble key={`${m.seq}-${m.clientMsgId ?? ''}`} msg={m} names={names} />)}
      </>
    )
  }
  const authors = uniqueAuthors(messages, names)
  return (
    <section className="hub-round-group" aria-label="同轮回复">
      <header className="hub-round-group-head">
        <span className="hub-chip">同轮</span>
        <span>{authors.join('、')} 的回复</span>
      </header>
      <div className="hub-round-group-body">
        {messages.map((m) => <GroupBubble key={`${m.seq}-${m.clientMsgId ?? ''}`} msg={m} names={names} />)}
      </div>
    </section>
  )
}


function uniqueAuthors(messages: HubMessage[], names: Record<string, string>): string[] {
  const seen: string[] = []
  for (const m of messages) {
    const id = m.sender?.agent_id ?? ''
    const name = id ? (names[id] ?? id) : '系统'
    if (!seen.includes(name)) seen.push(name)
  }
  return seen
}
