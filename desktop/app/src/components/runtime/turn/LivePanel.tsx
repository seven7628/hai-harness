import { useEffect, useRef, type ReactNode } from 'react'
import { Face } from '../Face'
import { fmtMs, toolArgsSummary, toolMetric, toolVerb } from '../../../lib/toolSummary'
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
   reply off the screen — the cap is ours, the header is the design's. */

const actOf = (items: MsgBlock[]): string => {
  for (let i = items.length - 1; i >= 0; i--) {
    const b = items[i]
    if (b.kind === 'tool' && b.status === 'running') return 'running a command'
    if (b.kind === 'agent' && (b.status === 'running' || b.status === 'interrupting')) return 'thinking'
  }
  return 'thinking'
}

export function LivePanel({
  items,
  startedAt,
  children,
}: {
  items: MsgBlock[]
  startedAt?: number
  children?: ReactNode
}) {
  // 设计里时钟 100ms 一跳（ui.js: setInterval(..., 100)）
  const now = useNow(true, 100)
  const elapsed = startedAt != null ? Math.max(0, now - startedAt) : 0
  const calls = items.filter((b) => b.kind === 'tool').length

  // 内容在底部滚动：读的是最新几行，不是开头
  const bodyRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const el = bodyRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [items])

  return (
    <div className="cy-live">
      <div className="cy-live-head">
        <Face state="run" />
        <span className="lv-lbl">Working</span>
        <span className="lv-clock">{fmtMs(elapsed)}</span>
        <span className="lv-sep">·</span>
        <span className="lv-n">{calls} {calls === 1 ? 'call' : 'calls'}</span>
        <span className="lv-act">{actOf(items)}</span>
      </div>
      <div className="rt-work-body" ref={bodyRef}>
        {children}
      </div>
    </div>
  )
}
