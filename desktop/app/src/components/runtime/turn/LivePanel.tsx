import type { MsgBlock } from '../../../store/useAppStore'
import { Face } from '../Face'
import { fmtMs, toolArgsSummary, toolMetric, toolVerb } from '../../../lib/toolSummary'
import { useNow } from '../../../lib/useNow'
import './live-panel.css'

type Tool = Extract<MsgBlock, { kind: 'tool' }>
type Agent = Extract<MsgBlock, { kind: 'agent' }>
type Compact = Extract<MsgBlock, { kind: 'compression' }>

/* 正在写的转录是被夹住的：你看它的最后几行，而不是一堵把整轮往下推的墙。
   设计把这一块叫 `.cy-live`（ui.components.css:2101-2133）：头部一行（状态 · 时钟 ·
   调用数 · 当前动作），下面是**四行窗口**，结束 240ms 淡出。 */

const WINDOW = 4

export function LivePanel({ items, startedAt }: { items: MsgBlock[]; startedAt?: number }) {
  // 设计里时钟 100ms 一跳（ui.js: `setInterval(..., 100)`）
  const now = useNow(true, 100)
  // 窗口里是**这一轮做过的事**，不只是工具：设计 图1 的行里有 Edit / Todo /
  // Agent / Agent —— 子 agent 派发与压缩也是这一轮的工作，漏掉它们，面板就只
  // 说了半句话。
  const work = items.filter(
    (b): b is Tool | Agent | Compact =>
      b.kind === 'tool' || b.kind === 'agent' || b.kind === 'compression',
  )
  const tools = work.filter((b): b is Tool => b.kind === 'tool')
  const recent = work.slice(-WINDOW)
  const running = tools.find((t) => t.status === 'running')
  const elapsed = startedAt != null ? Math.max(0, now - startedAt) : 0
  // 右侧那一格说的是**这一轮处在哪个阶段**，不是具体在跑哪条命令（命令就在下面
  // 第一行）。设计的三档：thinking → running a command → settling（ui.js:2763-2781）。
  const act = running ? 'running a command' : 'thinking'

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
          const o = toolArgsSummary(t.name, t.args || '')
          // 度量与工具行同一口径（lib/toolSummary.toolMetric）：143 lines / 5 hits / +8 −5
          const metric = toolMetric(t.name, t.args || '', t.result, t.diff)
          const m = metric ? (metric.kind === 'diff' ? `+${metric.added} \u2212${metric.removed}` : metric.text) : ''
          return (
            <div className="lv-row" data-s={t.status === 'running' ? 'run' : 'done'} key={t.id}>
              <i className="lv-dot" />
              <b className="v">{toolVerb(t.name)}</b>
              <span className="o">{o}</span>
              <span className="m">{m}</span>
              <span className="dur">{t.status === 'running' ? '' : fmtMs(t.durMs ?? undefined)}</span>
            </div>
          )
        })}
      </div>
    </div>
  )
}
