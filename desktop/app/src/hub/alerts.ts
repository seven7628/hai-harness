import type { HubAlert } from './types'

// P2.5-05：hub_alert 的纯逻辑（去重/封顶），与 zustand store 分开便于 node 回归。
// 幂等键 = code + task_id + message（**不含 at**）：后端每次告警都带新的 at，
// 把它放进 key 会让「反复 notify_failed」永不合并（P2.5-05 复审 N5）。

export function alertKey(code: string, taskID?: string, message?: string): string {
  return `${code}:${taskID ?? ''}:${message ?? ''}`
}

/** hub_alert 事件的最小形状（store 只消费这些字段）。 */
export interface HubAlertEventLike {
  scope?: string
  code?: string
  kind?: string
  severity?: string
  task_id?: string
  agent_id?: string
  group_id?: string
  message?: unknown
  at?: string
}

/** 事件 → 告警对象；id 刻意不含 at，保证重放/重复告警可合并。 */
export function alertFromEvent(ev: HubAlertEventLike): HubAlert {
  const code = ev.code || ev.kind || 'hub_alert'
  const message = typeof ev.message === 'string' ? ev.message : undefined
  return {
    id: alertKey(code, ev.task_id, message),
    scope: ev.scope,
    code,
    kind: ev.kind,
    severity: ev.severity,
    task_id: ev.task_id,
    agent_id: ev.agent_id,
    group_id: ev.group_id,
    message,
    at: ev.at,
  }
}

export function addHubAlert(list: HubAlert[], alert: HubAlert, max = 20): HubAlert[] {
  if (list.some((a) => a.id === alert.id)) return list
  return [alert, ...list].slice(0, max)
}

export function alertSeverity(value?: string): 'info' | 'warning' | 'error' {
  return value === 'info' || value === 'error' ? value : 'warning'
}
