import { useCallback, useMemo } from 'react'
import { Markdown } from '../components/markdown'
import { EyeIcon, GitBranch } from '../components/icons'
import { useT } from '../i18n'
import type { HubMessage } from './types'
import { parseRoutingText } from './groupThread'
import { uniqueContextSenders } from './groupProjection'
import AgentMentionChip from './AgentMentionChip'
import AgentSigil from './AgentSigil'
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
// 2026-11 demo 源码移植：agent 消息与 DM 共用「52px Luna + 作者头 + 无气泡正文」，
// human 消息与 DM 共用「右侧暖色气泡 + 时间」。群特有标签只在数据能表达时出现：
//   · 观察  ← groupRole=observer（名册真实字段）
//   · N 人触发 ← context.senders ≥2（触发上下文）
// 没有 reply_to 字段，所以不画 demo 的「回复 @」。

// 事件卡的归属智能体：refs 里是 bridge 落的真实 id（hub_handoff.go 写
// from_agent_id / to_agent_id；审批写 agent_id），名册只补名字与职责。
// refs 缺 id 时，handoff/审批退回 sender —— 那是发起人本人。
// 两条路都拿不到就不画：绝不拿「当前打开的会话」兜底，归属不明的卡只画事件。
type EventActor = { id: string; name: string; role?: string }

function eventActor(id: unknown, roster: MentionAgent[]): EventActor | null {
  const key = typeof id === 'string' ? id.trim() : ''
  if (!key) return null
  const agent = roster.find((a) => a.id === key)
  return { id: key, name: agent?.name || key, role: agent?.role }
}

function senderActor(msg: HubMessage, roster: MentionAgent[]): EventActor | null {
  return msg.sender?.kind === 'agent' ? eventActor(msg.sender.agent_id, roster) : null
}

/** demo 的 .event-flow：`<span class="agent">{头像}{名字}</span>`（组内 gap 4），
 *  组与组之间才是 .event-flow 的 6px 与 `→`。头像与名字分开当兄弟会变成 6px。 */
function EventFlow({ from, to }: { from?: EventActor | null; to?: EventActor | null }) {
  if (!from && !to) return null
  const face = (a: EventActor) => (
    <span className="agent">
      <AgentSigil agentId={a.id} name={a.name} responsibility={a.role} status={false} size={18} />
      {a.name}
    </span>
  )
  return (
    <div className="event-flow">
      {from ? face(from) : null}
      {from && to ? <span className="arrow" aria-hidden="true">→</span> : null}
      {to ? face(to) : null}
    </div>
  )
}

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
  // 失败气泡的行内「重试」：只有「就是我这条 cid 的那次失败」才可点（沿用首发 cid，
  // 见 store.retry(cid)）。按 cid 查失败表而不是看「最新一条」，多条失败时才不会点 A 发 B，
  // 中间成功过别的消息也不会把这条的 cid 弄丢（复审 B1/B3）。按布尔订阅，别的消息不重渲染。
  const canRetryThis = useHubStore((s) => !!msg.failed && !!s.failedByCid[msg.clientMsgId ?? ''])
  const retryFailed = useHubStore((s) => s.retry)
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
    // 发起人：refs.agent_id（bridge 写卡时落的）→ sender.agent_id；都没有就不画。
    const initiator = eventActor(msg.refs?.agent_id, roster) ?? senderActor(msg, roster)
    return (
      <div className={`msg-row event-row${hl}`} data-seq={seq}>
        <span className="event-spacer" aria-hidden="true" />
        <div className="msg-main">
          <article className="event-card event-card--approval" aria-label={`审批占位：${msg.text || '有人发起审批，详情在 Needs you'}`}>
            <div className="event-head">
              <span className="event-icon" aria-hidden="true"><EyeIcon size={14} /></span>
              <div className="event-main">
                <div className="event-kicker">Private approval</div>
                <div className="event-title">
                  <span className="name">{msg.text || '有人发起审批，详情在 Needs you'}</span>
                  <span className="event-status">仅你可看</span>
                </div>
                <EventFlow from={initiator} />
              </div>
            </div>
            <p className="event-body">审批正文不进入群上下文；处理入口在右栏 Needs you。</p>
          </article>
        </div>
      </div>
    )
  }
  if (msg.kind === 'routing_receipt') {
    return <RoutingAction text={msg.text ?? ''} names={names} seq={seq} highlight={!!hl} />
  }
  if (msg.sender?.kind === 'system') {
    return (
      <div className={`system-line${hl}`} data-seq={seq} role="status">
        <span>{msg.text}</span>
      </div>
    )
  }
  if (msg.kind === 'handoff') {
    const status = String(msg.refs?.status ?? '')
    const label = status === 'pending_ack' ? '等待确认' : status === 'accepted' ? '已接受' : status === 'declined' ? '已拒绝' : '交接'
    // 发起人/接收人取 refs（bridge 在 hub_handoff.go 里写死这两个 id）；发起人缺失退回 sender。
    const from = eventActor(msg.refs?.from_agent_id, roster) ?? senderActor(msg, roster)
    const to = eventActor(msg.refs?.to_agent_id, roster)
    return (
      <div className={`msg-row event-row${hl}`} data-seq={seq}>
        <span className="event-spacer" aria-hidden="true" />
        <div className="msg-main">
          <article className="event-card event-card--handoff" aria-label={`交接：${msg.text}（${label}）`}>
            <div className="event-head">
              <span className="event-icon" aria-hidden="true"><GitBranch size={14} /></span>
              <div className="event-main">
                <div className="event-kicker">Handoff</div>
                <div className="event-title">
                  <span className="name">{msg.text}</span>
                  <span className={`event-status ${status === 'pending_ack' || !status ? 'wait' : status === 'accepted' ? 'ok' : ''}`}>{label}</span>
                </div>
                <EventFlow from={from} to={to} />
              </div>
            </div>
            <p className="event-body">完整交接指令在 Needs you；群内只保留摘要。</p>
          </article>
        </div>
      </div>
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
  // TurnContextLine 自己会在 <2 人时 render null；但父层先判掉，任务卡的 footer
  // 条件才能把「有 context」当真实内容，不至于补出一行空 footer。
  const hasContext = uniqueContextSenders(msg.context?.senders).length >= 2

  if (card) {
    const executor = card.agentId ? roster.find((a) => a.id === card.agentId) : undefined
    const initiator = card.initiatorAgentId ? roster.find((a) => a.id === card.initiatorAgentId) : undefined
    return (
      <div className={`msg-row event-row task-row${hl} ${msg.pending ? 'pending' : ''}`} data-seq={seq}>
        <span className="event-spacer" aria-hidden="true" />
        <div className="msg-main">
          <TaskResultCard
            view={card}
            executor={executor}
            initiator={initiator}
            onOpenAgent={card.agentId && onSelectAgent ? () => onSelectAgent(card.agentId) : undefined}
            context={hasContext ? (
              <TurnContextLine
                compact
                msg={msg}
                roster={roster}
                triggerSeqBySender={triggerSeqBySender}
                onJumpToSeq={onJumpToSeq}
              />
            ) : undefined}
          />
        </div>
      </div>
    )
  }

  const tags = (
    <>
      {agent?.groupRole === 'observer' ? <span className="agent-tag observe">{t('hub.observer')}</span> : null}
      {msg.visibility === 'owner' ? <span className="agent-tag owner">{t('hub.ownerOnly')}</span> : null}
    </>
  )
  const contextLine = hasContext ? (
    <TurnContextLine
      compact
      msg={msg}
      roster={roster}
      triggerSeqBySender={triggerSeqBySender}
      onJumpToSeq={onJumpToSeq}
    />
  ) : undefined

  if (kind === 'human') {
    return (
      <UserMessage
        dataSeq={seq}
        className={`hub-group-msg${hl}`}
        ownerOnly={msg.visibility === 'owner'}
        pending={msg.pending}
        failed={msg.failed}
        onRetry={canRetryThis ? () => void retryFailed(msg.clientMsgId) : undefined}
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
      footer={contextLine}
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
      <div className={`system-line${highlight ? ' hub-msg-highlight' : ''}`} data-seq={seq} role="status">
        {text}
      </div>
    )
  }
  const targets = parsed.targets.map((id) => names[id] ?? id)
  return (
    <div className={`route-line${highlight ? ' hub-msg-highlight' : ''}`} data-seq={seq} role="status">
      <span className="route-title"><span className="routing-pill">路由</span></span>
      <span>
        {parsed.all ? '@all ' : ''}
        {targets.length === 0 ? '无人被唤醒' : `唤醒 ${targets.join('、')}`}
        {parsed.dropped ? `（丢弃 ${parsed.dropped} 个）` : ''}
      </span>
    </div>
  )
}
