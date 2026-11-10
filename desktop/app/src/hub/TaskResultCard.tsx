import type { ReactNode } from 'react'
import AgentBadge from './AgentBadge'
import type { MentionAgent } from './mentions'
import TaskArtifacts from './TaskArtifacts'
import { taskDurationText, taskUsageText, type TaskCardView } from './taskCard'

// 任务结果卡（P3b-07，方案 §7.5.5 / §8.3.5）。
// 只渲染 task_result 消息携带的快照：不查账本、不 @ 路由、不唤醒任何人。
// 「打开会话」= 选中执行者的 DM（由父级 select() 完成），卡片本身不跳走。

export default function TaskResultCard({
  view,
  executor,
  initiator,
  onOpenAgent,
  context,
}: {
  view: TaskCardView
  executor?: MentionAgent | null
  /** 派发者（账本 origin 解析出的智能体）；人派任务 originKind=human → 文案「你 → 执行者」。 */
  initiator?: MentionAgent | null
  onOpenAgent?: () => void
  /** 多触发者时的「↳ N」入口；由群消息层传入，保持 B6 跳转。 */
  context?: ReactNode
}) {
  const usage = taskUsageText(view.usage)
  const duration = taskDurationText(view.durationMs)
  const execName = executor?.name || view.agentId
  // 派发者只认账本 origin（B5）：human → 你；智能体 → 名册/origin 快照/ID；
  // 未知（任务未加载且无 refs 快照）→ 只显示执行者。
  const fromName = view.originKind === 'human'
    ? '你'
    : (initiator?.name || view.initiatorName || view.initiatorAgentId || '')
  return (
    <article className={`hub-task-card tone-${view.tone}`} aria-label={`任务卡：${view.title}（${view.statusLabel}）`}>
      <div className="hub-task-card-who">
        <AgentBadge
          agent={executor}
          name={execName || '执行者'}
          role={executor?.role}
          onClick={execName && onOpenAgent ? onOpenAgent : undefined}
        />
        <span className={`hub-task-status ${view.tone}`}>{view.statusLabel}</span>
        {context}
        {fromName && fromName !== execName ? (
          <span className="hub-task-from">
            {fromName === '你' ? '你派发' : `${fromName} 派发`}
          </span>
        ) : null}
      </div>
      <header className="hub-task-card-head">
        <span className="hub-task-title" title={view.title}>{view.title}</span>
      </header>
      {view.summary ? (
        <p className="hub-task-summary">{view.summary}</p>
      ) : null}
      {view.error ? (
        <p className="hub-task-error" role="alert">{view.error}</p>
      ) : null}
      <TaskArtifacts artifacts={view.artifacts} />
      <footer className="hub-task-card-foot">
        {duration ? <span title="任务耗时">{duration}</span> : null}
        {usage ? <span title="用量与成本">{usage}</span> : null}
        {view.taskId ? <span className="hub-muted" title={view.taskId}>{view.taskId}</span> : null}
        {view.agentId && onOpenAgent ? (
          <button type="button" className="hub-link hub-task-open" onClick={onOpenAgent}>
            打开会话
          </button>
        ) : null}
      </footer>
    </article>
  )
}
