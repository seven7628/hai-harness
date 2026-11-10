import AgentSigil from './AgentSigil'
import { planStack, type AvatarState } from './avatar'

// Group 头像叠放（方案 §3.3.5）。
//
// 规则：
//   - 2–4 个：全部直接叠放（重叠 25%，左侧在前）；
//   - ≥5 个：前 3 个 + `+N`（N = 总数 - 3，同尺寸、同叠放偏移）；
//   - 只给最左 lead 画一次组状态（成员最高优先级），其余成员只画身份；
//   - 叠放用 paper 色 drop-shadow 分隔（CSS），不额外套边框层。
//
// 语义：整组是一个 role="img"（侧栏父按钮已有 aria-label 时传 label=null → aria-hidden，
// 避免同一行读两遍）。

export interface AgentStackMember {
  id: string
  name?: string
  /** 职责短句，只影响内侧 trait。 */
  responsibility?: string
  state: AvatarState
  deleted?: boolean
}

export default function AgentStack({
  members,
  size = 24,
  groupRunning = false,
  label,
  className = '',
}: {
  members: readonly AgentStackMember[]
  size?: 24 | 32 | 48
  /** 群会话正在跑：没有更高优先级时 lead 显示 working。 */
  groupRunning?: boolean
  label?: string | null
  className?: string
}) {
  // 空群没有头像可画：返回 null，而不是一个 aria-label 说「0 个智能体」的空容器。
  if (members.length === 0) return null
  const plan = planStack(members, { groupRunning })
  return (
    <span
      className={`agent-stack s${size}${className ? ` ${className}` : ''}`}
      data-state={plan.state}
      role={label ? 'img' : undefined}
      aria-label={label || undefined}
      aria-hidden={label ? undefined : true}
    >
      {plan.members.map((m, i) => (
        <AgentSigil
          key={m.id}
          agentId={m.id}
          responsibility={m.responsibility}
          state={m.state}
          size={size}
          status={m.withStatus}
          style={{ zIndex: 20 - i }}
          className={m.lead ? 'lead' : 'peer'}
        />
      ))}
      {plan.more > 0 ? (
        <span className={`agent-stack-more s${size}`} aria-hidden="true">+{plan.more}</span>
      ) : null}
    </span>
  )
}
