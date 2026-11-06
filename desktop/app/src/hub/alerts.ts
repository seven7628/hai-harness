import type { HubAlert } from './types'

// P2.5-05：hub_alert 的纯逻辑（去重/封顶），与 zustand store 分开便于 node 回归。
// 幂等键 = code + task_id + message（**不含 at**）：后端每次告警都带新的 at，
// 把它放进 key 会让「反复 notify_failed」永不合并（P2.5-05 复审 N5）。

export function alertKey(code: string, taskID?: string, message?: string): string {
  return `${code}:${taskID ?? ''}:${message ?? ''}`
}

export function addHubAlert(list: HubAlert[], alert: HubAlert, max = 20): HubAlert[] {
  if (list.some((a) => a.id === alert.id)) return list
  return [alert, ...list].slice(0, max)
}

export function alertSeverity(value?: string): 'info' | 'warning' | 'error' {
  return value === 'info' || value === 'error' ? value : 'warning'
}
