import { useClock } from '../lib/useNow'
import { fmtRun, fmtDuration } from './ui'

// 实时计时展示（时钟方案 A，FIX_OPTIMIZATION 问题3）：唯一订阅 1s 全局时钟的小组件。
// 运行中 → 每秒刷新的「已运行时长」（· 12s…）；结束 → 结算总耗时 durMs；无数据 → null。
// 因为只有这个小组件订阅 useClock，每秒 tick 不会重渲正文/工具体（配合块级 memo）。
export function ElapsedTimer({ startAt, running, durMs, className }: {
  startAt?: number
  running: boolean
  durMs?: number
  className?: string
}) {
  const live = useClock(startAt, running && startAt != null)
  if (running && startAt != null) {
    return <span className={className}>{` · ${fmtRun(live)}…`}</span>
  }
  if (durMs != null) return <span className={className}>{` · ${fmtDuration(durMs)}`}</span>
  return null
}
