import { Markdown } from '../components/markdown'
import type { HubMessage } from './types'
import { parseRoutingText } from './groupThread'

// 群消息气泡：handoff 卡、审批占位、路由回执都只在人类 UI 出现；
// routing_receipt 的落盘文本是 targets=ag_x，需要按名册还原成可读动作行。
export function GroupBubble({ msg, names }: { msg: HubMessage; names: Record<string, string> }) {
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
  return (
    <div className={`hub-msg ${kind === 'human' ? 'human' : 'agent'} ${msg.pending ? 'pending' : ''} ${msg.failed ? 'failed' : ''}`}>
      <div className="hub-group-author">
        {who}
        {msg.visibility === 'owner' ? <span className="hub-chip owner-only">仅你可看</span> : null}
      </div>
      <div className="hub-body">
        <Markdown text={msg.text ?? ''} />
      </div>
    </div>
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

