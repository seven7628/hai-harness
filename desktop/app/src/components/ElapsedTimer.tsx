import { useClock } from '../lib/useNow'
import { fmtRun, fmtDuration } from './ui'

// 实时计时展示（时钟方案 A，FIX_OPTIMIZATION 问题3）：唯一订阅 1s 全局时钟的小组件。
// 运行中 → 每秒刷新的「已运行时长」（· 12s…）；结束 → 结算总耗时 durMs；无数据 → null。
// 因为只有这个小组件订阅 useClock，每秒 tick 不会重渲正文/工具体（配合块级 memo）。
// 时间统一带一个 `.elapsed` 标记：运行时皮肤据此上 --tone-time 身份色
//（用户 2026-10：所有计时信息要有一个与主题搭配的颜色）。调用点自己的 className 保留。
export function ElapsedTimer({ startAt, running, durMs, className }: {
  startAt?: number
  running: boolean
  durMs?: number
  className?: string
}) {
  const live = useClock(startAt, running && startAt != null)
  const cls = className ? `elapsed ${className}` : 'elapsed'
  if (running && startAt != null) {
    return <span className={cls}>{` · ${fmtRun(live)}…`}</span>
  }
  if (durMs != null) return <span className={cls}>{` · ${fmtDuration(durMs)}`}</span>
  return null
}
