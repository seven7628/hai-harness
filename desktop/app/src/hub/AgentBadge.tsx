import type { CSSProperties } from 'react'
import { useT } from '../i18n'
import { accentOfAgent } from './agentAccent'
import type { MentionAgent } from './mentions'

// 群消息/任务卡的「谁说的」标识：头像 + 名字 + 职责。
// 只负责身份，不负责气泡；颜色只给头像环与名字，正文保持 --ink 的可读性。
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
  const emoji = agent?.avatar?.kind === 'emoji' ? agent.avatar.value : ''
  const avatar = emoji || name.slice(0, 1)
  const style = { '--agent-accent': accent } as CSSProperties
  const body = (
    <>
      <span className="hub-agent-avatar" aria-hidden="true">{avatar}</span>
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
