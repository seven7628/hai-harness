import { useCallback, useMemo } from 'react'
import { Markdown } from '../components/markdown'
import { EyeIcon, GitBranch } from '../components/icons'
import { useT } from '../i18n'
import type { HubMessage } from './types'
import { parseRoutingText } from './groupThread'
import AgentMentionChip from './AgentMentionChip'
import { AgentMessage, UserMessage } from './ConversationMessage'
import TaskResultCard from './TaskResultCard'
import TurnContextLine from './TurnContextLine'
import { formatClockTime } from './conversation'
import { ALL_MENTION_ID, linkifyMentions, type MentionAgent } from './mentions'
import { parseTaskResult } from './taskCard'
import { useHubStore } from './useHubStore'
import { highlightClass, seqDataAttr } from './seqJump'

// 群消息（handoff 卡、审批占位、路由回执只在人类 UI 出现；
// routing_receipt 的落盘文本是 targets=ag_x，按名册还原成可读动作行）。
// P3b-07 起：task_result → TaskResultCard（派发者取账本 origin，不拿 sender 猜）；
// 正文里的 @Name → AgentMentionChip；context.senders ≥2 → TurnContextLine。
//
// 2026-10 会话区复刻：agent 消息与 DM 共用「32px sigil + 作者头 + 无气泡正文」，
// human 消息与 DM 共用「右侧暖色气泡 + 时间」。群特有标签只在数据能表达时出现：
//   · 观察  ← groupRole=observer（名册真实字段）
//   · ↳ N   ← context.senders ≥2（触发上下文）
// 没有 reply_to 字段，所以不画 demo 的「回复 @」。

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
  const t = useT()
  const taskId = msg.kind === 'task_result' ? String(msg.refs?.task_id ?? '') : ''
  // 任务卡派发者必须来自账本（origin.kind/agent_id）：bridge 写的结果卡 sender
  // 就是执行者本人，按 sender 推会得到「Writer → Writer」（B5）。
  const task = useHubStore((s) => (taskId ? s.tasks.find((x) => x.id === taskId) : undefined))
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
      <article className={`hub-event hub-event--approval${hl}`} data-seq={seq} aria-label={`审批占位：${msg.text || '有人发起审批，详情在 Needs you'}`}>
        <span className="hub-event-icon" aria-hidden="true"><EyeIcon size={14} /></span>
        <div className="hub-event-main">
          <div className="hub-event-top">
            <div className="hub-event-copy">
              <span className="hub-event-kicker">Private approval</span>
              <span className="hub-event-title">{msg.text || '有人发起审批，详情在 Needs you'}</span>
            </div>
            <span className="hub-event-status muted">仅你可看</span>
          </div>
          <p className="hub-event-body">审批正文不进入群上下文；处理入口在右栏 Needs you。</p>
        </div>
      </article>
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
      <article className={`hub-event hub-event--handoff${hl}`} data-seq={seq} aria-label={`交接：${msg.text}（${label}）`}>
        <span className="hub-event-icon" aria-hidden="true"><GitBranch size={14} /></span>
        <div className="hub-event-main">
          <div className="hub-event-top">
            <div className="hub-event-copy">
              <span className="hub-event-kicker">Handoff</span>
              <span className="hub-event-title">{msg.text}</span>
            </div>
            <span className={`hub-event-status ${status || 'pending_ack'}`}>{label}</span>
          </div>
          <p className="hub-event-body">完整交接指令在 Needs you；群内只保留摘要。</p>
        </div>
      </article>
    )
  }
  const kind = msg.sender?.kind ?? 'system'
  const senderId = kind === 'agent' ? (msg.sender?.agent_id ?? '') : ''
  const agent = senderId ? roster.find((a) => a.id === senderId) : undefined
  const who = kind === 'human' ? '你' : ((names[senderId] ?? senderId) || '智能体')
  const roleParts = [agent?.role, agent?.groupRole === 'owner' ? '群主' : ''].filter(Boolean)
  const role = roleParts.join(' · ')
  const time = formatClockTime(msg.created_at)
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

  const tags = (
    <>
      {agent?.groupRole === 'observer' ? <span className="hub-msg-tag observe">{t('hub.observer')}</span> : null}
      <TurnContextLine
        compact
        msg={msg}
        roster={roster}
        triggerSeqBySender={triggerSeqBySender}
        onJumpToSeq={onJumpToSeq}
      />
      {msg.visibility === 'owner' ? <span className="hub-msg-tag owner">{t('hub.ownerOnly')}</span> : null}
    </>
  )

  if (kind === 'human') {
    return (
      <UserMessage
        dataSeq={seq}
        className={`hub-group-msg${hl}`}
        ownerOnly={msg.visibility === 'owner'}
        pending={msg.pending}
        failed={msg.failed}
        time={time}
        dateTime={msg.created_at}
      >
        <Markdown text={linkedText} renderMention={roster.length > 0 ? renderMention : undefined} />
      </UserMessage>
    )
  }

  return (
    <AgentMessage
      agent={agent}
      name={who}
      role={role || undefined}
      tags={tags}
      time={time}
      dateTime={msg.created_at}
      dataSeq={seq}
      className={`hub-group-msg${hl}`}
      onOpenAgent={agent && onSelectAgent && !agent.deleted ? () => onSelectAgent(agent.id) : undefined}
    >
      <Markdown text={linkedText} renderMention={roster.length > 0 ? renderMention : undefined} />
    </AgentMessage>
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
