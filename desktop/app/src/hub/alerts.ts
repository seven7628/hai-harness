import type { HubAlert } from './types'

// P2.5-05：hub_alert 的纯逻辑（去重/封顶），与 zustand store 分开便于 node 回归。
// 幂等键 = code + task_id + at/message：同一任务重复告警只保留一条，全局失败
// （task_id 为空）按时间/文案区分。

export function alertKey(code: string, taskID?: string, at?: string, message?: string): string {
  return `${code}:${taskID ?? ''}:${at ?? message ?? ''}`
}

export function addHubAlert(list: HubAlert[], alert: HubAlert, max = 20): HubAlert[] {
  if (list.some((a) => a.id === alert.id)) return list
  return [alert, ...list].slice(0, max)
}

export function alertSeverity(value?: string): 'info' | 'warning' | 'error' {
  return value === 'info' || value === 'error' ? value : 'warning'
}
