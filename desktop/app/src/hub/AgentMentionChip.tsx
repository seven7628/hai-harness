import type { CSSProperties } from 'react'
import type { MentionAgent } from './mentions'

// 正文里的 @ 徽章（P3b-07，方案 §7.5.2 / §8.3.5）。
// accent 只给圆点与名字上色（整块染色会让群里五颜六色），整体版式与正文同高；
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
  const accent = agent.accent || 'var(--accent-solid)'
  const isAll = agent.groupRole === 'all'
  const role = agent.role || ''
  const roleLabel = agent.deleted
    ? '已移除'
    : isAll ? '群内所有人' : [agent.groupRole, role].filter(Boolean).join(' · ')
  const label = isAll
    ? '提及所有人'
    : `提及智能体 ${name}${role ? `（${role}）` : ''}${agent.deleted ? '（已移除）' : ''}`
  const style = { '--mention-accent': accent } as CSSProperties
  const body = (
    <>
      <span className="hub-mention-dot" aria-hidden="true" />
      {agent.avatar?.kind === 'emoji' && agent.avatar.value ? (
        <span className="hub-mention-avatar" aria-hidden="true">{agent.avatar.value}</span>
      ) : null}
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
