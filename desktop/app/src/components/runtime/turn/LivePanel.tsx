import { useEffect, useRef, useState, type ReactNode } from 'react'
import { Face } from '../Face'
import { toolArgsSummary, toolMetric, toolVerb } from '../../../lib/toolSummary'
import { fmtRun } from '../../../lib/duration'
import { activeElapsed, settlePause } from '../../../lib/activeElapsed'
import { useNow } from '../../../lib/useNow'
import type { MsgBlock } from '../../../store/useAppStore'
import './live-panel.css'

/* The working block: one header line for the whole run (AgentStart → AgentEnd)
   and, inside it, the run's actual content — thoughts, the narration between
   calls, the tool rows, subagent rows. Concise does not *change* what a turn
   contains; it puts the same thing in a box and folds the box at AgentEnd.
   Detailed mode renders the very same children without the box.

   Design: `.cy-live` head (ui.components.css:2101-2133) with the phase label on
   the right; the window underneath is capped so a long run cannot push the
   reply off the screen — the cap is ours, the header is the design's.

   规格（2026-10）补的两件事，都在这个头上：
     · 等待输入/授权时要**说出来**（Waiting for input / approval），而不是继续报
       "thinking" —— 那一刻模型什么都没做；
     · 那个钟必须是活跃执行时长：等待期间停住（见 lib/activeElapsed.ts），
       否则「等了 10 分钟才批准」会被读成「模型跑了 10 分钟」。 */

const actOf = (items: MsgBlock[]): string => {
  for (let i = items.length - 1; i >= 0; i--) {
    const b = items[i]
    if (b.kind === 'tool' && b.status === 'running') return 'running a command'
    if (b.kind === 'agent' && (b.status === 'running' || b.status === 'interrupting')) return 'thinking'
  }
  return 'thinking'
}

export type Waiting = 'approval' | 'input' | null

export function LivePanel({
  items,
  startedAt,
  waiting = null,
  children,
}: {
  items: MsgBlock[]
  startedAt?: number
  /** 正在等用户：审批 / 提问。等待期间时钟停住，阶段标签改成 Waiting for … */
  waiting?: Waiting
  children?: ReactNode
}) {
  // 设计里时钟 100ms 一跳（ui.js: setInterval(..., 100)）
  const now = useNow(true, 100)
  // 等待段的记账：结束的并进 pausedMs，正在等的那一段由 activeElapsed 扣掉
  const [pausedMs, setPausedMs] = useState(0)
  const waitStartRef = useRef<number | null>(null)
  useEffect(() => {
    if (waiting) {
      if (waitStartRef.current == null) waitStartRef.current = Date.now()
    } else if (waitStartRef.current != null) {
      setPausedMs((p) => settlePause(p, waitStartRef.current ?? undefined, Date.now()))
      waitStartRef.current = null
    }
  }, [waiting])
  const elapsed = activeElapsed({ now, startedAt, pausedMs, ...(waitStartRef.current != null ? { waitingSince: waitStartRef.current } : {}) })
  const calls = items.filter((b) => b.kind === 'tool').length

  // 内容在底部滚动：读的是最新几行，不是开头
  const bodyRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const el = bodyRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [items])

  return (
    <div className="cy-live" data-waiting={waiting ?? undefined}>
      <div className="cy-live-head">
        {/* 等待时不转：状态脸给 'work'（在等一个外部回应），跑起来才是 'run' */}
        <Face state={waiting ? 'work' : 'run'} />
        <span className="lv-lbl">{waiting ? 'Waiting' : 'Working'}</span>
        <span className="lv-clock">{fmtRun(elapsed)}</span>
        <span className="lv-sep">·</span>
        <span className="lv-n">{calls} {calls === 1 ? 'call' : 'calls'}</span>
        <span className="lv-act" data-waiting={waiting ?? undefined}>
          {waiting === 'approval' ? 'waiting for approval' : waiting === 'input' ? 'waiting for input' : actOf(items)}
        </span>
      </div>
      <div className="rt-work-body" ref={bodyRef}>
        {children}
      </div>
    </div>
  )
}
