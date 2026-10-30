// 批量执行的「真实进度」—— 只允许用后端真的给了的数据算，一个百分点都不许猜。
//
// 数据契约（2026-10 核对 events/tool.go + core/nested.go + tools/nested.go）：
//   · nested_calls 只随 **tool_run_end** 一次性到达（子调用不发事件，tools/nested.go:227-230）；
//   · 没有 batch_id / total / completed / failed_count 任何进度字段；
//   · codemode **完全不发 tool_output**（只有 bash 发，tools/builtin/bash_tail.go:75）。
// 所以：
//   运行中 —— 没有可用的分母，**只能**给不确定态（跑马灯）+ 已用时长，绝不画百分比；
//   结束后 —— nested_calls 就是真账：条数=总数、is_error=失败、duration=各自耗时。
// 这正是规格 §3「总任务数量未知时使用不确定进度动画，不得伪造百分比」的那一档。
//
// 想让运行中也能画真实进度，需要后端补一份契约（详见交付说明），前端不做推测。

import type { NestedCallView } from './codemodeNested'

export interface BatchProgress {
  /** 子调用总数（结束后才知道；运行中为 0） */
  total: number
  done: number
  failed: number
  /** 子调用耗时之和（只加有值的） */
  durMs: number
  /** 完成比例 0–1；没有总数时为 undefined（调用方据此画不确定态） */
  pct?: number
}

export function batchProgressOf(calls: NestedCallView[] | undefined): BatchProgress | undefined {
  if (!calls || calls.length === 0) return undefined
  let failed = 0
  let durMs = 0
  for (const c of calls) {
    if (c.isError) failed++
    if (typeof c.durMs === 'number' && c.durMs > 0) durMs += c.durMs
  }
  const total = calls.length
  const done = total - failed
  return { total, done, failed, durMs, pct: total ? done / total : undefined }
}

/** 结束后的那一行数字：`Execute · 2/2 completed · 1 failed · 5.1s`（规格 §3 的语法）。 */
export function batchLineOf(p: BatchProgress, tier: 'search' | 'execute', wall?: string): string {
  const parts = [`${tier === 'search' ? 'Search' : 'Execute'}`, `${p.done + p.failed === p.total ? p.total : p.done}/${p.total} completed`]
  if (p.failed > 0) parts.push(`${p.failed} failed`)
  if (wall) parts.push(wall)
  return parts.join(' \u00b7 ')
}
