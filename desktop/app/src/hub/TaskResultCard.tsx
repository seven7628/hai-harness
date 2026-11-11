import type { ReactNode } from 'react'
import { CheckList } from '../components/icons'
import AgentSigil from './AgentSigil'
import type { MentionAgent } from './mentions'
import TaskArtifacts from './TaskArtifacts'
import { taskDurationText, taskUsageText, type TaskCardView } from './taskCard'

// 任务结果卡：demo 的 event-card 解剖。
//
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
  /** 多触发者时的「N 人触发」入口；由群消息层传入，保持 B6 跳转。 */
  context?: ReactNode
}) {
  const usage = taskUsageText(view.usage)
  const duration = taskDurationText(view.durationMs)
  const execName = executor?.name || view.agentId
  const fromName = view.originKind === 'human'
    ? '你'
    : (initiator?.name || view.initiatorName || view.initiatorAgentId || '')
  const status = view.tone === 'success' ? 'ok' : view.tone === 'warn' ? 'wait' : ''
  // 16px 身份头像跟名字同行：执行者与派发者都能看出来源；没有智能体对象时只留文字。
  const face = (agent: MentionAgent) => (
    <AgentSigil agentId={agent.id} responsibility={agent.role} status={false} size={16} />
  )
  return (
    <article
      className={`event-card event-card--task tone-${view.tone}`}
      aria-label={`任务卡：${view.title}（${view.statusLabel}）`}
    >
      <div className="event-head">
        <span className="event-icon" aria-hidden="true"><CheckList size={14} /></span>
        <div className="event-main">
          <div className="event-kicker">Task</div>
          <div className="event-title">
            <span className="name" title={view.title}>{view.title}</span>
            <span className={`event-status ${status}`}>{view.statusLabel}</span>
          </div>
          <div className="event-meta">
            {execName ? (
              <span className="event-who">
                {executor ? face(executor) : null}
                {execName}
              </span>
            ) : null}
            {fromName && fromName !== execName ? (
              <span className="event-who">
                {initiator ? face(initiator) : null}
                {fromName === '你' ? '你派发' : `${fromName} 派发`}
              </span>
            ) : null}
            {duration ? <span title="任务耗时">{duration}</span> : null}
            {usage ? <span title="用量与成本">{usage}</span> : null}
          </div>
        </div>
      </div>
      {view.summary ? <p className="event-body">{view.summary}</p> : null}
      {view.error ? <p className="event-error" role="alert">{view.error}</p> : null}
      <TaskArtifacts artifacts={view.artifacts} />
      {/* 「N 人触发」是卡的元信息，不是正文：跟 task id / 打开会话同一行（demo 的 event-actions）。
          条件带上 context —— 否则一张既无 task_id 又无入口的卡会留下空 footer。 */}
      {view.taskId || context || (view.agentId && onOpenAgent) ? (
        <footer className="event-actions">
          {context}
          {view.taskId ? <span className="event-id" title={view.taskId}>{view.taskId}</span> : null}
          {view.agentId && onOpenAgent ? (
            <button type="button" className="event-action hub-task-open" onClick={onOpenAgent}>
              打开会话
            </button>
          ) : null}
        </footer>
      ) : null}
    </article>
  )
}
