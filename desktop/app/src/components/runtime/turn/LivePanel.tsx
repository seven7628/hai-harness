import type { MsgBlock } from '../../../store/useAppStore'
import { Face } from '../Face'
import { fmtMs, toolArgsSummary, toolVerb } from '../../../lib/toolSummary'
import { useNow } from '../../../lib/useNow'
import './live-panel.css'

type Tool = Extract<MsgBlock, { kind: 'tool' }>

/* 正在写的转录是被夹住的：你看它的最后几行，而不是一堵把整轮往下推的墙。
   设计把这一块叫 `.cy-live`（ui.components.css:2101-2133）：头部一行（状态 · 时钟 ·
   调用数 · 当前动作），下面是**四行窗口**，结束 240ms 淡出。 */

const WINDOW = 4

export function LivePanel({ items, startedAt }: { items: MsgBlock[]; startedAt?: number }) {
  // 设计里时钟 100ms 一跳（ui.js: `setInterval(..., 100)`）
  const now = useNow(true, 100)
  const tools = items.filter((b): b is Tool => b.kind === 'tool')
  const recent = tools.slice(-WINDOW)
  const running = tools.find((t) => t.status === 'running')
  const elapsed = startedAt != null ? Math.max(0, now - startedAt) : 0
  const act = running
    ? toolVerb(running.name) + (toolArgsSummary(running.name, running.args || '') ? ' ' + toolArgsSummary(running.name, running.args || '') : '')
    : tools.length
      ? 'thinking'
      : 'thinking'

  return (
    <div className="cy-live">
      <div className="cy-live-head">
        <Face state="run" />
        <span className="lv-lbl">Working</span>
        <span className="lv-clock">{fmtMs(elapsed)}</span>
        <span className="lv-sep">·</span>
        <span className="lv-n">{tools.length} calls</span>
        <span className="lv-act">{act}</span>
      </div>
      <div className="cy-live-rows">
        {recent.map((t) => {
          const o = toolArgsSummary(t.name, t.args || '')
          return (
            <div className="lv-row" data-s={t.status === 'running' ? 'run' : 'done'} key={t.id}>
              <i className="lv-dot" />
              <b className="v">{toolVerb(t.name)}</b>
              <span className="o">{o}</span>
              <span className="m" />
              <span className="dur">{t.status === 'running' ? '' : fmtMs(t.durMs ?? undefined)}</span>
            </div>
          )
        })}
      </div>
    </div>
  )
}
