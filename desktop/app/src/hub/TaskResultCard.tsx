import type { ReactNode } from 'react'
import { CheckList } from '../components/icons'
import AgentSigil from './AgentSigil'
import type { MentionAgent } from './mentions'
import TaskArtifacts from './TaskArtifacts'
import { stripProjectHeader, taskDurationText, taskUsageText, type TaskCardView } from './taskCard'

// 任务结果卡：demo 的 event-card 解剖。
//
// 只渲染 task_result 消息携带的快照：不查账本、不 @ 路由、不唤醒任何人。
// 两种目标画法不同（B1）：
//   · agent 目标：执行者是谁 →「<执行者>」，后面跟「<派发者> 派发」；
//   · workspace 目标：没有执行智能体，只有一个 Project 会话 →
//     「<发起者> 发起 · <ws> / <sid>」，正文用项目会话的摘要。
// 「打开会话」= agent 目标选中执行者 DM、workspace 目标切到那个 Project 会话
//（由父级回调完成），卡片本身不跳走。

export default function TaskResultCard({
  view,
  executor,
  initiator,
  onOpenAgent,
  onOpenSession,
  context,
}: {
  view: TaskCardView
  /** 执行者（agent 目标）；workspace 目标没有执行者，传了也不画。 */
  executor?: MentionAgent | null
  /** 派发者/发起者（账本 origin 解析出的智能体）；人派任务 originKind=human → 文案「你 → 执行者」。 */
  initiator?: MentionAgent | null
  onOpenAgent?: () => void
  /** workspace 目标：打开结果所在的 Project 会话（不是发起者的 DM）。 */
  onOpenSession?: () => void
  /** 多触发者时的「N 人触发」入口；由群消息层传入，保持 B6 跳转。 */
  context?: ReactNode
}) {
  const usage = taskUsageText(view.usage)
  const duration = taskDurationText(view.durationMs)
  // workspace 目标没有执行智能体：画执行者会把发起者顶上去，出现「群派发A 派发」（B1）。
  const workspace = view.targetKind === 'workspace'
  const execName = workspace ? '' : (executor?.name || view.agentId)
  const fromName = view.originKind === 'human'
    ? '你'
    : (initiator?.name || view.originName || view.initiatorName || view.initiatorAgentId || '')
  const where = workspace ? [view.targetWorkspace, view.targetSessionId].filter(Boolean) : []
  const openSession = workspace
    ? (view.targetSessionId && onOpenSession ? onOpenSession : null)
    : (view.agentId && onOpenAgent ? onOpenAgent : null)
  const body = workspace ? stripProjectHeader(view.summary) : view.summary
  const status = view.tone === 'success' ? 'ok' : view.tone === 'warn' ? 'wait' : ''
  // 16px 身份头像跟名字同行：执行者与派发者都能看出来源；没有智能体对象时只留文字。
  const face = (agent: MentionAgent) => (
    <AgentSigil agentId={agent.id} name={agent.name} responsibility={agent.role} status={false} size={16} />
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
            {workspace ? (
              <span className="event-who">
                {initiator ? face(initiator) : null}
                {fromName ? `${fromName} 发起` : '项目任务'}
              </span>
            ) : execName ? (
              <span className="event-who">
                {executor ? face(executor) : null}
                {execName}
              </span>
            ) : null}
            {where.length > 0 ? (
              <span className="event-where" title={where.join(' / ')}>
                <code>{where.join(' / ')}</code>
              </span>
            ) : fromName && fromName !== execName ? (
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
      {body ? <p className="event-body">{body}</p> : null}
      {view.error ? <p className="event-error" role="alert">{view.error}</p> : null}
      <TaskArtifacts artifacts={view.artifacts} taskId={view.taskId} />
      {/* 「N 人触发」是卡的元信息，不是正文：跟 task id / 打开会话同一行（demo 的 event-actions）。
          条件带上 context —— 否则一张既无 task_id 又无入口的卡会留下空 footer。 */}
      {view.taskId || context || openSession ? (
        <footer className="event-actions">
          {context}
          {view.taskId ? <span className="event-id" title={view.taskId}>{view.taskId}</span> : null}
          {openSession ? (
            <button type="button" className="event-action hub-task-open" onClick={openSession}>
              打开会话
            </button>
          ) : null}
        </footer>
      ) : null}
    </article>
  )
}
