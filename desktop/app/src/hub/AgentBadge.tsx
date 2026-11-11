import type { CSSProperties } from 'react'
import { useT } from '../i18n'
import { lunaColor } from './lunaAvatar'
import AgentSigil from './AgentSigil'
import { useAgentState } from './useAgentState'
import type { MentionAgent } from './mentions'

// 群消息/任务卡的「谁说的」标识：头像 + 名字 + 职责。
// 只负责身份，不负责气泡；名字用 role palette 的身份色，正文保持 --ink 的可读性。
// 严格 demo：accent 不参与头像/名字，只留给侧栏环/未读/状态点。
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
  const identity = lunaColor(agent?.id ?? '', agent?.name ?? name, agent?.role ?? role ?? '')
  const state = useAgentState(agent?.id, agent?.deleted)
  const style = { '--agent-accent': identity } as CSSProperties
  const body = (
    <>
      <AgentSigil agentId={agent?.id} name={agent?.name ?? name} responsibility={agent?.role ?? role} state={state} size={32} />
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
