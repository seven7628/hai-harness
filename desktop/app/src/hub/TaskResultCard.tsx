import type { MentionAgent } from './mentions'
import { taskArtifactText, taskDurationText, taskUsageText, type TaskCardView } from './taskCard'

// 任务结果卡（P3b-07，方案 §7.5.5 / §8.3.5）。
// 只渲染 task_result 消息携带的快照：不查账本、不 @ 路由、不唤醒任何人。
// 「打开会话」= 选中执行者的 DM（由父级 select() 完成），卡片本身不跳走。

export default function TaskResultCard({
  view,
  executor,
  initiator,
  onOpenAgent,
}: {
  view: TaskCardView
  executor?: MentionAgent | null
  /** 派发者（智能体派的任务）；人派的任务传 null → 文案「你 → 执行者」。 */
  initiator?: MentionAgent | null
  onOpenAgent?: () => void
}) {
  const usage = taskUsageText(view.usage)
  const duration = taskDurationText(view.durationMs)
  const execName = executor?.name || view.agentId
  const fromName = initiator?.name || view.initiatorAgentId || (view.initiatorAgentId ? '' : '你')
  return (
    <article className={`hub-task-card tone-${view.tone}`} aria-label={`任务卡：${view.title}（${view.statusLabel}）`}>
      <header className="hub-task-card-head">
        <span className={`hub-task-status ${view.tone}`}>{view.statusLabel}</span>
        <span className="hub-task-title" title={view.title}>{view.title}</span>
      </header>
      {view.summary ? (
        <p className="hub-task-summary">{view.summary}</p>
      ) : null}
      {view.error ? (
        <p className="hub-task-error" role="alert">{view.error}</p>
      ) : null}
      {view.artifacts.length > 0 ? (
        <ul className="hub-task-artifacts" aria-label="产物">
          {view.artifacts.map((a, i) => (
            <li key={`${a.path || a.name}-${i}`} title={a.path || a.name}>
              <span className="hub-task-artifact-kind">{a.kind || 'artifact'}</span>
              <span className="hub-task-artifact-name">{a.name || a.path}</span>
              {a.path && a.name ? <span className="hub-task-artifact-path">{a.path}</span> : null}
            </li>
          ))}
        </ul>
      ) : null}
      <footer className="hub-task-card-foot">
        {execName ? (
          <span className="hub-task-exec" title={view.agentId ? `执行者 ${view.agentId}` : undefined}>
            {fromName && fromName !== execName ? `${fromName} → ` : ''}{execName}
          </span>
        ) : null}
        {duration ? <span title="任务耗时">{duration}</span> : null}
        {usage ? <span title="用量与成本">{usage}</span> : null}
        {view.taskId ? <span className="hub-muted" title={view.taskId}>{view.taskId}</span> : null}
        {view.agentId && onOpenAgent ? (
          <button type="button" className="hub-link hub-task-open" onClick={onOpenAgent}>
            打开会话
          </button>
        ) : null}
      </footer>
      {/* 产物可读文本也放进 title：小屏/深色下不再只靠布局传达（无颜色依赖）。 */}
      {view.artifacts.length > 0 ? (
        <span className="hub-sr-only">
          产物：{view.artifacts.map((a) => taskArtifactText(a)).join('；')}
        </span>
      ) : null}
    </article>
  )
}
