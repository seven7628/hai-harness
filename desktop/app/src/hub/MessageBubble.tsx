import { useCallback, useMemo, type CSSProperties } from 'react'
import { Markdown } from '../components/markdown'
import type { HubMessage } from './types'
import { parseRoutingText } from './groupThread'
import AgentBadge from './AgentBadge'
import AgentMentionChip from './AgentMentionChip'
import TaskResultCard from './TaskResultCard'
import TurnContextLine from './TurnContextLine'
import { formatRelativeTime } from './conversation'
import { accentOfAgent } from './agentAccent'
import { ALL_MENTION_ID, linkifyMentions, type MentionAgent } from './mentions'
import { parseTaskResult } from './taskCard'
import { useHubStore } from './useHubStore'
import { highlightClass, seqDataAttr } from './seqJump'

// 群消息气泡：handoff 卡、审批占位、路由回执都只在人类 UI 出现；
// routing_receipt 的落盘文本是 targets=ag_x，需要按名册还原成可读动作行。
// P3b-07 起：task_result → TaskResultCard（派发者取账本 origin，不拿 sender 猜）；
// 正文里的 @Name → AgentMentionChip；context.senders ≥2 → TurnContextLine
// （点击跳到触发消息并高亮；系统/交接/回执路径保持原样）。

export interface GroupBubbleProps {
  msg: HubMessage
  names: Record<string, string>
  roster?: MentionAgent[]
  onSelectAgent?: (agentID: string) => void
  triggerSeqBySender?: Record<string, number>
  onJumpToSeq?: (seq: number) => void
  highlightSeq?: number | null
}

export function GroupBubble({
  msg,
  names,
  roster = [],
  onSelectAgent,
  triggerSeqBySender,
  onJumpToSeq,
  highlightSeq,
}: GroupBubbleProps) {
  const taskId = msg.kind === 'task_result' ? String(msg.refs?.task_id ?? '') : ''
  // 任务卡派发者必须来自账本（origin.kind/agent_id）：bridge 写的结果卡 sender
  // 就是执行者本人，按 sender 推会得到「Writer → Writer」（B5）。
  const task = useHubStore((s) => (taskId ? s.tasks.find((t) => t.id === taskId) : undefined))
  const renderMention = useCallback(
    (id: string) => {
      if (id === ALL_MENTION_ID) {
        return <AgentMentionChip agent={{ id, name: '所有人', groupRole: 'all' }} />
      }
      const agent = roster.find((a) => a.id === id)
      if (!agent) return <span className="hub-mention unknown">@{id}</span>
      return (
        <AgentMentionChip
          agent={agent}
          onClick={onSelectAgent && !agent.deleted ? () => onSelectAgent(agent.id) : undefined}
        />
      )
    },
    [roster, onSelectAgent],
  )

  // 输入框每敲一个字 GroupView 都会重渲染：linkify 必须按「正文 + 名册」记忆，
  // 否则整页消息每条都要重扫一遍（Markdown 的 memo 也救不了这层）。
  const linkedText = useMemo(
    () => (roster.length > 0 ? linkifyMentions(msg.text ?? '', roster) : (msg.text ?? '')),
    [msg.text, roster],
  )

  // data-seq / 高亮 class 与 DM、深链共用同一口径（seqJump.ts），别再各写一份。
  const seq = seqDataAttr(msg.seq)
  const hl = highlightClass(msg.seq, highlightSeq)

  if (msg.kind === 'approval_stub') {
    return (
      <div className={`hub-msg system hub-approval-stub${hl}`} data-seq={seq} role="status">
        <span className="hub-chip">审批占位</span>
        <span>{msg.text || '有人发起审批，详情在 Needs you'}</span>
        <span className="hub-muted">仅你可看</span>
      </div>
    )
  }
  if (msg.kind === 'routing_receipt') {
    return <RoutingAction text={msg.text ?? ''} names={names} seq={seq} highlight={!!hl} />
  }
  if (msg.sender?.kind === 'system') {
    return (
      <div className={`hub-system-note${hl}`} data-seq={seq} role="status">
        {msg.text}
      </div>
    )
  }
  if (msg.kind === 'handoff') {
    const status = String(msg.refs?.status ?? '')
    const label = status === 'pending_ack' ? '等待确认' : status === 'accepted' ? '已接受' : status === 'declined' ? '已拒绝' : '交接'
    return (
      <div className={`hub-msg system hub-handoff-card${hl}`} data-seq={seq} role="status">
        <span className="hub-chip">交接</span>
        <span className="hub-handoff-text">{msg.text}</span>
        <span className={`hub-handoff-status ${status || 'pending_ack'}`}>{label}</span>
      </div>
    )
  }
  const kind = msg.sender?.kind ?? 'system'
  const senderId = kind === 'agent' ? (msg.sender?.agent_id ?? '') : ''
  const agent = senderId ? roster.find((a) => a.id === senderId) : undefined
  const who = kind === 'human' ? '你' : ((names[senderId] ?? senderId) || '智能体')
  const roleParts = [agent?.role, agent?.groupRole === 'owner' ? '群主' : ''].filter(Boolean)
  const role = roleParts.join(' · ')
  const time = formatRelativeTime(msg.created_at)
  const accent = accentOfAgent(agent ?? { id: senderId, name: who })
  const style = { '--agent-accent': accent } as CSSProperties
  const card = parseTaskResult(msg, task)

  if (card) {
    const executor = card.agentId ? roster.find((a) => a.id === card.agentId) : undefined
    const initiator = card.initiatorAgentId ? roster.find((a) => a.id === card.initiatorAgentId) : undefined
    return (
      <div className={`hub-msg task${hl} ${msg.pending ? 'pending' : ''}`} data-seq={seq}>
        <TaskResultCard
          view={card}
          executor={executor}
          initiator={initiator}
          onOpenAgent={card.agentId && onSelectAgent ? () => onSelectAgent(card.agentId) : undefined}
          context={(
            <TurnContextLine
              compact
              msg={msg}
              roster={roster}
              triggerSeqBySender={triggerSeqBySender}
              onJumpToSeq={onJumpToSeq}
            />
          )}
        />
      </div>
    )
  }

  return (
    <div
      className={`hub-msg ${kind === 'human' ? 'human' : 'agent'} hub-group-msg${hl} ${msg.pending ? 'pending' : ''} ${msg.failed ? 'failed' : ''}`}
      data-seq={seq}
      data-agent-id={senderId || undefined}
      style={kind === 'agent' ? style : undefined}
    >
      <div className={`hub-msg-head${kind === 'human' ? ' human' : ''}`}>
        {kind === 'human' ? (
          <>
            <span className="hub-agent-name">你</span>
            {msg.visibility === 'owner' ? <span className="hub-chip owner-only">仅你可看</span> : null}
            {time ? <time className="hub-msg-time" dateTime={msg.created_at}>{time}</time> : null}
          </>
        ) : (
          <>
            <AgentBadge
              agent={agent}
              name={who}
              role={role || undefined}
              onClick={agent && onSelectAgent && !agent.deleted ? () => onSelectAgent(agent.id) : undefined}
            />
            <TurnContextLine
              compact
              msg={msg}
              roster={roster}
              triggerSeqBySender={triggerSeqBySender}
              onJumpToSeq={onJumpToSeq}
            />
            {msg.visibility === 'owner' ? <span className="hub-chip owner-only">仅你可看</span> : null}
            {time ? <time className="hub-msg-time" dateTime={msg.created_at}>{time}</time> : null}
          </>
        )}
      </div>
      <div className="hub-body">
        <Markdown
          text={linkedText}
          renderMention={roster.length > 0 ? renderMention : undefined}
        />
      </div>
    </div>
  )
}

export function RoutingAction({
  text,
  names,
  seq,
  highlight,
}: {
  text: string
  names: Record<string, string>
  seq?: string
  highlight?: boolean
}) {
  const parsed = parseRoutingText(text)
  if (!parsed) {
    return (
      <div className={`hub-system-note${highlight ? ' hub-msg-highlight' : ''}`} data-seq={seq} role="status">
        {text}
      </div>
    )
  }
  const targets = parsed.targets.map((id) => names[id] ?? id)
  return (
    <div className={`hub-system-note hub-system-routing${highlight ? ' hub-msg-highlight' : ''}`} data-seq={seq} role="status">
      <span className="hub-system-kind">路由</span>
      <span>
        {parsed.all ? '@all ' : ''}
        {targets.length === 0 ? '无人被唤醒' : `唤醒 ${targets.join('、')}`}
        {parsed.dropped ? `（丢弃 ${parsed.dropped} 个）` : ''}
      </span>
    </div>
  )
}
