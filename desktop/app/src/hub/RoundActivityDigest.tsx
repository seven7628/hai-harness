import type { HubGroupActivity } from './types'

// 动作清单：把 Run 活动流压成一行摘要（路由 / 工具成功失败 / 总耗时）。
// 不依赖后端额外事件：activity 已带 kind/status/name/ms/after_seq。
export function RoundActivityDigest({
  activity,
  names,
}: {
  activity: HubGroupActivity[]
  names: Record<string, string>
}) {
  if (activity.length === 0) return null
  const routing = activity.filter((a) => a.kind === 'routing').length
  const tools = activity.filter((a) => a.kind !== 'routing')
  const failed = tools.filter((a) => a.status === 'failed' || a.status === 'error').length
  const totalMs = activity.reduce((sum, a) => sum + (a.ms ?? 0), 0)
  const toolNames = [...new Set(tools.map((a) => a.name).filter(Boolean))].slice(0, 6)
  return (
    <div className="hub-digest" role="status">
      <span className="hub-chip">动作</span>
      <span>{routing} 次路由 · {tools.length} 个工具{failed ? ` · ${failed} 失败` : ''}</span>
      {totalMs > 0 ? <span className="hub-muted">{totalMs}ms</span> : null}
      {toolNames.length > 0 ? <span className="hub-muted">{toolNames.join('、')}</span> : null}
      <span className="hub-muted">{activity.map((a) => names[a.agent_id ?? ''] ?? a.agent_id).filter(Boolean).slice(0, 4).join('、')}</span>
    </div>
  )
}
