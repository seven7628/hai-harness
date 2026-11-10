import type { ReactNode } from 'react'
import { CheckList } from '../components/icons'
import type { MentionAgent } from './mentions'
import TaskArtifacts from './TaskArtifacts'
import { taskDurationText, taskUsageText, type TaskCardView } from './taskCard'

// 任务结果卡（P3b-07 → 2026-10 会话区复刻的 event-card 解剖）。
//
// 只渲染 task_result 消息携带的快照：不查账本、不 @ 路由、不唤醒任何人。
// 「打开会话」= 选中执行者的 DM（由父级 select() 完成），卡片本身不跳走。
//
// 版式与交接卡/审批卡/工具行同一套：29px 图标块 + mono kicker + 12px 标题 +
// meta + 右侧状态；正文与操作缩进到标题列（38px）。派发者只认账本 origin（B5）。

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
    <article
      className={`hub-event hub-event--task tone-${view.tone}`}
      aria-label={`任务卡：${view.title}（${view.statusLabel}）`}
    >
      <span className="hub-event-icon" aria-hidden="true"><CheckList size={14} /></span>
      <div className="hub-event-main">
        <div className="hub-event-top">
          <div className="hub-event-copy">
            <span className="hub-event-kicker">Task</span>
            <span className="hub-event-title" title={view.title}>{view.title}</span>
            <span className="hub-event-meta">
              {execName ? <span className="hub-event-who">{execName}</span> : null}
              {fromName && fromName !== execName
                ? <span>{fromName === '你' ? '你派发' : `${fromName} 派发`}</span>
                : null}
              {duration ? <span title="任务耗时">{duration}</span> : null}
              {usage ? <span title="用量与成本">{usage}</span> : null}
            </span>
          </div>
          <span className={`hub-event-status ${view.tone}`}>{view.statusLabel}</span>
          {context}
        </div>
        {view.summary ? <p className="hub-event-body">{view.summary}</p> : null}
        {view.error ? <p className="hub-event-error" role="alert">{view.error}</p> : null}
        <TaskArtifacts artifacts={view.artifacts} />
        {view.taskId || (view.agentId && onOpenAgent) ? (
          <footer className="hub-event-foot">
            {view.taskId ? <span className="hub-event-id" title={view.taskId}>{view.taskId}</span> : null}
            {view.agentId && onOpenAgent ? (
              <button type="button" className="hub-event-action hub-task-open" onClick={onOpenAgent}>
                打开会话
              </button>
            ) : null}
          </footer>
        ) : null}
      </div>
    </article>
  )
}
