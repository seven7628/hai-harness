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

/* 兼容旧调用：只给 items 时退化成「四行窗口」（别的宿主还在用）。
   行里带与工具行同口径的度量，子 agent 派发也占一行。 */
export function LiveRows({ items }: { items: MsgBlock[] }) {
  const WINDOW = 4
  const work = items.filter(
    (b): b is Extract<MsgBlock, { kind: 'tool' | 'agent' | 'compression' }> =>
      b.kind === 'tool' || b.kind === 'agent' || b.kind === 'compression',
  )
  const recent = work.slice(-WINDOW)
  return (
    <div className="cy-live-rows">
      {recent.map((t) => {
        if (t.kind === 'agent') {
          const busy = t.status === 'running' || t.status === 'interrupting'
          return (
            <div className="lv-row" data-s={busy ? 'run' : 'done'} key={`a${t.id}`}>
              <i className="lv-dot" />
              <b className="v">Agent</b>
              <span className="o">{t.label}</span>
              <span className="m" />
              <span className="dur">{busy ? '' : fmtMs(t.durMs ?? undefined)}</span>
            </div>
          )
        }
        if (t.kind === 'compression') {
          return (
            <div className="lv-row" data-s="run" key={`c${t.id}`}>
              <i className="lv-dot" />
              <b className="v">Compact</b>
              <span className="o" />
              <span className="m" />
              <span className="dur" />
            </div>
          )
        }
        const metric = toolMetric(t.name, t.args || '', t.result, t.diff)
        const m = metric ? (metric.kind === 'diff' ? `+${metric.added} \u2212${metric.removed}` : metric.text) : ''
        return (
          <div className="lv-row" data-s={t.status === 'running' ? 'run' : 'done'} key={t.id}>
            <i className="lv-dot" />
            <b className="v">{toolVerb(t.name)}</b>
            <span className="o">{toolArgsSummary(t.name, t.args || '')}</span>
            <span className="m">{m}</span>
            <span className="dur">{t.status === 'running' ? '' : fmtMs(t.durMs ?? undefined)}</span>
          </div>
        )
      })}
    </div>
  )
}
