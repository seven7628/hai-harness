import type { ReactNode } from 'react'
import { useT } from '../i18n'
import AgentSigil from './AgentSigil'
import { useAgentState } from './useAgentState'
import type { MentionAgent } from './mentions'

// 会话区版式：直接采用 demo 的 DOM 结构（msg-row / agent-head / agent-text /
// user-bubble / owner-avatar），类名和间距不另造。
//
// 只把数据接进来：头像由 Luna 引擎画，正文由调用方给 Markdown，
// 状态/角色/时间仍来自 Hub 的真实字段。

export function AgentMessage({
  agent,
  name,
  role,
  tags,
  time,
  dateTime,
  onOpenAgent,
  dataSeq,
  className = '',
  children,
  footer,
}: {
  agent?: MentionAgent | null
  name: string
  role?: string
  /** 作者头里的真实标签（观察 / 仅你可看 / 正在输入）；没有数据就别传。 */
  tags?: ReactNode
  time?: string
  dateTime?: string
  onOpenAgent?: () => void
  dataSeq?: string
  className?: string
  children: ReactNode
  footer?: ReactNode
}) {
  const t = useT()
  const state = useAgentState(agent?.id, agent?.deleted)
  return (
    <div
      className={`msg-row agent${className ? ` ${className}` : ''}`}
      data-seq={dataSeq}
      data-agent-id={agent?.id || undefined}
    >
      {onOpenAgent ? (
        <button
          type="button"
          className="avatar-btn"
          onClick={onOpenAgent}
          title={t('hub.openAgentSession').replace('{name}', name)}
          aria-label={t('hub.openAgentSession').replace('{name}', name)}
        >
          <AgentSigil agentId={agent?.id} name={name} responsibility={agent?.role ?? role} accent={agent?.accent} state={state} size={52} label={null} />
        </button>
      ) : (
        <AgentSigil agentId={agent?.id} name={name} responsibility={agent?.role ?? role} accent={agent?.accent} state={state} size={52} label={null} />
      )}
      <div className="msg-main">
        <div className="agent-head">
          {onOpenAgent ? (
            <button type="button" className="agent-name agent-name-btn" onClick={onOpenAgent} title={role || undefined}>
              {name}
            </button>
          ) : (
            <span className="agent-name" title={role || undefined}>{name}</span>
          )}
          {tags}
          {time ? <time className="msg-time" dateTime={dateTime}>{time}</time> : null}
        </div>
        <div className="agent-text">{children}</div>
        {footer}
      </div>
    </div>
  )
}

export function UserMessage({
  time,
  dateTime,
  ownerOnly,
  pending,
  failed,
  dataSeq,
  className = '',
  children,
}: {
  time?: string
  dateTime?: string
  ownerOnly?: boolean
  pending?: boolean
  failed?: boolean
  dataSeq?: string
  className?: string
  children: ReactNode
}) {
  const t = useT()
  return (
    <div
      className={`msg-row user${pending ? ' pending' : ''}${failed ? ' failed' : ''}${className ? ` ${className}` : ''}`}
      data-seq={dataSeq}
    >
      <div className="msg-main">
        <div className="user-bubble">{children}</div>
        {ownerOnly || pending || failed || time ? (
          <div className="user-meta">
            {ownerOnly ? <span className="user-owner-only">{t('hub.ownerOnly')}</span> : null}
            {pending ? <span>{t('hub.sending')}</span> : null}
            {failed ? <span className="user-failed">{t('hub.sendFailed')}</span> : null}
            {time ? <time dateTime={dateTime}>{time}</time> : null}
          </div>
        ) : null}
      </div>
      <span className="owner-avatar" aria-hidden="true">你</span>
    </div>
  )
}

/** 日期分隔线：细线 + 居中日期（今天 / 昨天 / 9 月 21 日）。 */
export function DayDivider({ label }: { label: string }) {
  return (
    <div className="date-divider" role="separator" aria-label={label}>
      <span>{label}</span>
    </div>
  )
}
