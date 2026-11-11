import AgentSigil from './AgentSigil'
import { planStack, type AvatarState } from './avatar'

// Group 头像叠放：完全按 demo 的 28px xs + 4 个 + `+N` 的规则。
// 引擎负责每颗头的脸，这里只负责顺序/重叠/状态语义。

export interface AgentStackMember {
  id: string
  name?: string
  responsibility?: string
  state: AvatarState
  deleted?: boolean
}

export default function AgentStack({
  members,
  size = 28,
  groupRunning = false,
  label,
  className = '',
}: {
  members: readonly AgentStackMember[]
  size?: number
  groupRunning?: boolean
  label?: string | null
  className?: string
}) {
  if (members.length === 0) return null
  const plan = planStack(members, { groupRunning })
  return (
    <span
      className={`avatar-stack luna-avatar-stack${className ? ` ${className}` : ''}`}
      data-state={plan.state}
      role={label ? 'img' : undefined}
      aria-label={label || undefined}
      aria-hidden={label ? undefined : true}
      style={{ display: 'inline-flex', alignItems: 'center' }}
    >
      {plan.members.map((m, i) => (
        <AgentSigil
          key={m.id}
          agentId={m.id}
          name={members.find((x) => x.id === m.id)?.name}
          responsibility={m.responsibility}
          state={m.state}
          size={size}
          status={m.withStatus}
          style={{ zIndex: 20 - i, marginLeft: i === 0 ? 0 : -1 }}
        />
      ))}
      {plan.more > 0 ? (
        <span className="avatar-more" aria-hidden="true">+{plan.more}</span>
      ) : null}
    </span>
  )
}
