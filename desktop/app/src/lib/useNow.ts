import { useEffect, useRef, useState } from 'react'
import { create } from 'zustand'

// 实时时钟：active 期间每 interval ms 刷新一次当前时间戳（用于非时钟场景的瞬时刷新）。
export function useNow(active: boolean, interval = 300): number {
  const [now, setNow] = useState(() => Date.now())
  const prev = useRef(active)
  useEffect(() => {
    if (active && !prev.current) setNow(Date.now())
    prev.current = active
    if (!active) return
    const t = setInterval(() => setNow(Date.now()), interval)
    return () => clearInterval(t)
  }, [active, interval])
  return now
}

// —— 单一时钟（用户方案）：全应用只有一个 setInterval，每秒 tick 一次 ——
// tick 放 zustand store，组件经 selector 订阅（store 变更必然触发重渲染，可靠）。
// 订阅计数驱动时钟启停：无时钟消费时空闲自动停拍，不占后台、不堆积定时器。
interface ClockStore {
  tick: number // 每秒更新的当前时刻（ms）
  subs: number // 活跃时钟订阅数
}
const useClockStore = create<ClockStore>(() => ({ tick: Date.now(), subs: 0 }))

let clockTimer: ReturnType<typeof setInterval> | null = null
function startClock() {
  if (clockTimer) return
  clockTimer = setInterval(() => useClockStore.setState({ tick: Date.now() }), 1000)
}
function stopClockIfIdle() {
  if (useClockStore.getState().subs === 0 && clockTimer) {
    clearInterval(clockTimer)
    clockTimer = null
  }
}

// useClock：订阅单一时钟。running && startAt 时返回「已运行时长」= tick - startAt（每秒走）；
// 否则返回 undefined（调用方显示结算总耗时 thinkMs/durMs）。running=false→true 自动归零。
// 仅「思考中」与「异步 Task」需要时钟——其余场景不调用此 hook，避免无谓订阅/重渲染。
export function useClock(startAt: number | undefined, running: boolean): number | undefined {
  const active = Boolean(running && startAt != null)
  const tick = useClockStore((s) => s.tick)
  useEffect(() => {
    if (!active) return
    useClockStore.setState({ tick: Date.now(), subs: useClockStore.getState().subs + 1 })
    startClock()
    return () => {
      useClockStore.setState((s) => ({ subs: Math.max(0, s.subs - 1) }))
      stopClockIfIdle()
    }
  }, [active])
  if (!active) return undefined
  return tick - (startAt ?? tick)
}
