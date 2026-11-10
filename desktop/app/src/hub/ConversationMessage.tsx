import type { CSSProperties, ReactNode } from 'react'
import { useT } from '../i18n'
import AgentSigil from './AgentSigil'
import { accentOfAgent } from './agentAccent'
import { useAgentState } from './useAgentState'
import type { MentionAgent } from './mentions'

// 会话区的两种消息版式（群聊 / DM 共用，2026-10 会话区复刻）：
//
//   agent  左侧 32px sigil + 作者头（名字/职责/标签/时间）+ **无气泡正文**；
//   human  右侧实心暖色气泡 + 气泡下方右对齐的时间。
//
// 之前 DM 只有正文、群聊另写一套带头气泡；两边节奏不同，切会话像换产品。
// 这里只做版式：身份色仍走 accentOfAgent，状态仍走 useAgentState（不重做头像），
// 正文由调用方给（Markdown / 流式文本 / 任务卡），不在这里判断消息类型。

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
  const accent = accentOfAgent(agent ?? { id: '', name })
  const style = { '--agent-accent': accent } as CSSProperties
  const sigil = (
    <AgentSigil agentId={agent?.id} responsibility={agent?.role} state={state} size={32} />
  )
  return (
    <div
      className={`hub-msg agent${className ? ` ${className}` : ''}`}
      data-seq={dataSeq}
      data-agent-id={agent?.id || undefined}
      style={style}
    >
      <div className="hub-msg-rail">
        {onOpenAgent ? (
          <button
            type="button"
            className="hub-msg-avatar"
            onClick={onOpenAgent}
            title={t('hub.openAgentSession').replace('{name}', name)}
            aria-label={t('hub.openAgentSession').replace('{name}', name)}
          >
            {sigil}
          </button>
        ) : (
          <span className="hub-msg-avatar static">{sigil}</span>
        )}
      </div>
      <div className="hub-msg-main">
        <div className="hub-msg-head">
          {onOpenAgent ? (
            <button type="button" className="hub-msg-name" onClick={onOpenAgent}>
              {name}
            </button>
          ) : (
            <span className="hub-msg-name">{name}</span>
          )}
          {role ? <span className="hub-msg-role">{role}</span> : null}
          {tags}
          {time ? <time className="hub-msg-time" dateTime={dateTime}>{time}</time> : null}
        </div>
        <div className="hub-msg-body">{children}</div>
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
      className={`hub-msg human${pending ? ' pending' : ''}${failed ? ' failed' : ''}${className ? ` ${className}` : ''}`}
      data-seq={dataSeq}
    >
      <div className="hub-msg-main">
        <div className="hub-user-bubble">{children}</div>
        {ownerOnly || pending || failed || time ? (
          <div className="hub-user-meta">
            {ownerOnly ? <span className="hub-owner-only">{t('hub.ownerOnly')}</span> : null}
            {pending ? <span>{t('hub.sending')}</span> : null}
            {failed ? <span className="hub-user-failed">{t('hub.sendFailed')}</span> : null}
            {time ? <time dateTime={dateTime}>{time}</time> : null}
          </div>
        ) : null}
      </div>
    </div>
  )
}

/** 日期分隔线：细线 + 居中日期（今天 / 昨天 / 9 月 21 日）。 */
export function DayDivider({ label }: { label: string }) {
  return (
    <div className="hub-day-divider" role="separator" aria-label={label}>
      <span>{label}</span>
    </div>
  )
}
