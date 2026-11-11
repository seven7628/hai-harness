import type { CSSProperties } from 'react'
import { useT } from '../i18n'
import { accentOfAgent } from './agentAccent'
import AgentSigil from './AgentSigil'
import { useAgentState } from './useAgentState'
import type { MentionAgent } from './mentions'

// 群消息/任务卡的「谁说的」标识：头像 + 名字 + 职责。
// 只负责身份，不负责气泡；accent 只给名字上色，正文保持 --ink 的可读性。
// 头像用 demo 的 Luna 引擎（职责色回答「我是谁」，表情回答「我现在怎样」）；
// 名字就在同一行，头像对读屏是装饰（aria-hidden）。
export default function AgentBadge({
  agent,
  name,
  role,
  onClick,
}: {
  agent?: MentionAgent | null
  name: string
  role?: string
  onClick?: () => void
}) {
  const t = useT()
  const accent = accentOfAgent(agent ?? { id: '', name })
  const state = useAgentState(agent?.id, agent?.deleted)
  const style = { '--agent-accent': accent } as CSSProperties
  const body = (
    <>
      <AgentSigil agentId={agent?.id} accent={agent?.accent} responsibility={agent?.role} state={state} size={32} />
      <span className="hub-agent-name">{name}</span>
      {role ? <span className="hub-agent-role">{role}</span> : null}
    </>
  )
  if (onClick) {
    return (
      <button
        type="button"
        className="hub-agent-badge clickable"
        style={style}
        onClick={onClick}
        title={t('hub.openAgentSession').replace('{name}', name)}
      >
        {body}
      </button>
    )
  }
  return <span className="hub-agent-badge" style={style}>{body}</span>
}
