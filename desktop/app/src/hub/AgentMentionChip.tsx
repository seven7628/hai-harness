import type { CSSProperties } from 'react'
import AgentSigil from './AgentSigil'
import { lunaColor } from './lunaAvatar'
import type { MentionAgent } from './mentions'

// 正文里的 @ 徽章（P3b-07，方案 §7.5.2 / §8.3.5）。
// 头像用 sigil v2（与列表/消息/详情同图同色：role palette 职责色），文字用同一个身份色混色，
// 头像 16px inline、压在基线上，不改变正文行高；正文里的提及不画状态层
// （一段话里不需要第二套状态表）。
// 可点击时是 role=link 的按钮（键盘 Tab + Enter 可达），不可点击时退化为普通 span。

export default function AgentMentionChip({
  agent,
  onClick,
  title,
}: {
  agent: MentionAgent
  onClick?: () => void
  title?: string
}) {
  const name = agent.name || agent.id
  const identity = lunaColor(agent.id, agent.name, agent.role ?? '')
  const isAll = agent.groupRole === 'all'
  const role = agent.role || ''
  const roleLabel = agent.deleted
    ? '已移除'
    : isAll ? '群内所有人' : [agent.groupRole, role].filter(Boolean).join(' · ')
  const label = isAll
    ? '提及所有人'
    : `提及智能体 ${name}${role ? `（${role}）` : ''}${agent.deleted ? '（已移除）' : ''}`
  const style = { '--mention-accent': identity } as CSSProperties
  const body = (
    <>
      <AgentSigil agentId={agent.id} responsibility={role} status={false} size={16} />
      <span className="hub-mention-name">{name}</span>
      {agent.deleted ? <span className="hub-mention-gone">（已移除）</span> : null}
    </>
  )
  if (onClick && !agent.deleted) {
    return (
      <button
        type="button"
        role="link"
        className="hub-mention"
        style={style}
        title={title ?? (roleLabel ? `${name} · ${roleLabel}` : name)}
        aria-label={label}
        onClick={onClick}
      >
        {body}
      </button>
    )
  }
  return (
    <span
      className={`hub-mention ${agent.deleted ? 'gone' : ''}`}
      style={style}
      title={title ?? (roleLabel ? `${name} · ${roleLabel}` : name)}
      aria-label={label}
    >
      {body}
    </span>
  )
}
