import { useCallback, useMemo } from 'react'
import { Markdown } from '../components/markdown'
import type { HubMessage } from './types'
import { parseRoutingText } from './groupThread'
import AgentMentionChip from './AgentMentionChip'
import TaskResultCard from './TaskResultCard'
import TurnContextLine from './TurnContextLine'
import { ALL_MENTION_ID, linkifyMentions, type MentionAgent } from './mentions'
import { parseTaskResult } from './taskCard'

// 群消息气泡：handoff 卡、审批占位、路由回执都只在人类 UI 出现；
// routing_receipt 的落盘文本是 targets=ag_x，需要按名册还原成可读动作行。
// P3b-07 起：task_result → TaskResultCard；正文里的 @Name → AgentMentionChip；
// context.senders ≥2 → TurnContextLine（系统/交接/回执路径保持原样）。

export function GroupBubble({
  msg,
  names,
  roster = [],
  onSelectAgent,
}: {
  msg: HubMessage
  names: Record<string, string>
  roster?: MentionAgent[]
  onSelectAgent?: (agentID: string) => void
}) {
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

  if (msg.kind === 'approval_stub') {
    return (
      <div className="hub-msg system hub-approval-stub" role="status">
        <span className="hub-chip">审批占位</span>
        <span>{msg.text || '有人发起审批，详情在 Needs you'}</span>
        <span className="hub-muted">仅你可看</span>
      </div>
    )
  }
  if (msg.kind === 'routing_receipt') {
    return <RoutingAction text={msg.text ?? ''} names={names} />
  }
  if (msg.sender?.kind === 'system') {
    return <div className="hub-msg system">{msg.text}</div>
  }
  if (msg.kind === 'handoff') {
    const status = String(msg.refs?.status ?? '')
    const label = status === 'pending_ack' ? '等待确认' : status === 'accepted' ? '已接受' : status === 'declined' ? '已拒绝' : '交接'
    return (
      <div className="hub-msg system hub-handoff-card" role="status">
        <span className="hub-chip">交接</span>
        <span className="hub-handoff-text">{msg.text}</span>
        <span className={`hub-handoff-status ${status || 'pending_ack'}`}>{label}</span>
      </div>
    )
  }
  const kind = msg.sender?.kind ?? 'system'
  const who = kind === 'human' ? '你' : (names[msg.sender?.agent_id ?? ''] ?? msg.sender?.agent_id ?? '智能体')
  const card = parseTaskResult(msg)
  const context = <TurnContextLine msg={msg} roster={roster} onSelectAgent={onSelectAgent} />

  if (card) {
    const executor = card.agentId ? roster.find((a) => a.id === card.agentId) : undefined
    const initiator = card.initiatorAgentId ? roster.find((a) => a.id === card.initiatorAgentId) : undefined
    return (
      <>
        {context}
        <div className={`hub-msg task ${msg.pending ? 'pending' : ''}`}>
          <TaskResultCard
            view={card}
            executor={executor}
            initiator={initiator}
            onOpenAgent={card.agentId && onSelectAgent ? () => onSelectAgent(card.agentId) : undefined}
          />
        </div>
      </>
    )
  }

  return (
    <>
      {context}
      <div className={`hub-msg ${kind === 'human' ? 'human' : 'agent'} ${msg.pending ? 'pending' : ''} ${msg.failed ? 'failed' : ''}`}>
        <div className="hub-group-author">
          {who}
          {msg.visibility === 'owner' ? <span className="hub-chip owner-only">仅你可看</span> : null}
        </div>
        <div className="hub-body">
          <Markdown
            text={linkedText}
            renderMention={roster.length > 0 ? renderMention : undefined}
          />
        </div>
      </div>
    </>
  )
}

export function RoutingAction({ text, names }: { text: string; names: Record<string, string> }) {
  const parsed = parseRoutingText(text)
  if (!parsed) return <div className="hub-msg system">{text}</div>
  const targets = parsed.targets.map((id) => names[id] ?? id)
  return (
    <div className="hub-action routing" role="status">
      <span className="hub-action-kind">路由</span>
      <span>
        {parsed.all ? '@all ' : ''}
        {targets.length === 0 ? '无人被唤醒' : `唤醒 ${targets.join('、')}`}
        {parsed.dropped ? `（丢弃 ${parsed.dropped} 个）` : ''}
      </span>
    </div>
  )
}
