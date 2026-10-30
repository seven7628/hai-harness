// 活跃执行时长 —— Working 标题栏那个钟显示的必须是「真的在跑」的时间。
//
// 规格（2026-10）：「运行时长从 Agent Start 开始累计，到 Agent End 结束；等待 Ask Question
// 或权限确认时保留已累计时长，如果实际执行已暂停，不能把等待时间伪装成活跃执行时间。」
//
// 所以钟 = 墙钟 − 累计等待。两段等待要能接起来（同一次运行里可能等好几次），
// 已经结束的等待累加到 pausedMs，正在等的那一段等到结束时再结算 —— 这样钟在等待期间是**停住的**，
// 而不是继续往上走。

export interface ActiveElapsedInput {
  now: number
  /** Agent Start 的时刻（事件时间戳，不是客户端点发送的时刻） */
  startedAt?: number
  /** 已经结束的等待段累计（毫秒） */
  pausedMs?: number
  /** 正在等待的起点；不在等待时为 undefined */
  waitingSince?: number
}

export function activeElapsed({ now, startedAt, pausedMs = 0, waitingSince }: ActiveElapsedInput): number {
  if (startedAt == null) return 0
  const waiting = waitingSince != null ? Math.max(0, now - waitingSince) : 0
  return Math.max(0, now - startedAt - Math.max(0, pausedMs) - waiting)
}

/** 等待段结束时结算：把它并入 pausedMs。 */
export function settlePause(pausedMs: number, waitingSince: number | undefined, now: number): number {
  if (waitingSince == null) return pausedMs
  return Math.max(0, pausedMs) + Math.max(0, now - waitingSince)
}
