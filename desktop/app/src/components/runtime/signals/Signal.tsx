import { Bot, CheckCircleIcon, Clock, GitBranch, HookIcon, Sparkles } from '../../icons'
import './signal.css'

export type SignalKind =
  | 'memory'
  | 'hook'
  | 'silent'
  | 'goal'
  | 'promote'
  | 'inputs'
  | 'budget'
  | 'persist'
  | 'closed'

const ICONS: Record<SignalKind, (p: { size?: number }) => JSX.Element> = {
  memory: Sparkles,
  hook: HookIcon,
  silent: Sparkles,
  goal: GitBranch,
  promote: Bot,
  inputs: Clock,
  budget: Clock,
  persist: AccessIconFallback,
  closed: CheckCircleIcon,
}

/* 图标按设计用 14px / stroke 1.8，颜色由 data-kind 决定（ui.components.css:970-990）。 */
function AccessIconFallback({ size = 14 }: { size?: number }) {
  return <Clock size={size} />
}

/* 一行信号：这些事件存在，是为了让转录不会在没有说明的情况下突然安静。
   它们不带任何交互 —— 一行字、一个图标、一个事件名（设计 §「Small signals」）。 */
export function Signal({
  kind,
  title,
  detail,
  type,
}: {
  kind: SignalKind
  title: string
  detail?: string
  type?: string
}) {
  const Icon = ICONS[kind] || Sparkles
  return (
    <div className="sig" data-kind={kind}>
      <Icon size={14} />
      <div style={{ minWidth: 0 }}>
        <b>{title}</b>
        {detail ? <span>{detail}</span> : null}
      </div>
      {type ? <span className="sig-t">{type}</span> : null}
    </div>
  )
}
