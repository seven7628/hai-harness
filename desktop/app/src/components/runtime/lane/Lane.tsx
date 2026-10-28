import { useState } from 'react'
import type { MsgBlock, AgentItem } from '../../../store/useAppStore'
import { AgentMark } from '../AgentMark'
import { FoldIcon } from '../FoldIcon'
import { toolArgsSummary, toolVerb } from '../../../lib/toolSummary'
import './lane.css'

type AgentMsg = Extract<MsgBlock, { kind: 'agent' }>

/* 注意 mark 的 kind：设计按语义角色（map/audit/read…）选图形，我们的运行没有这个
   字段（PORT-PLAN §11.11 第 7 条）。这里固定 'scout' 而不是从 label 关键词去猜 ——
   猜错比统一更糟，且形状在这里只承担「这是个子 agent」。 */

/* 车道：一个子 agent 一行，四个事实 —— 谁去了、走到哪、最后一句是什么、多久了。
   设计（parts/lane.css）明确说不能画进度条或剩余时间：父 run 只能知道这四件事，
   多画一个百分比就是编。这里唯一自己动的是那个 1px 呼吸条。 */

const WORD: Record<string, string> = {
  run: 'running',
  stop: 'killed',
  done: 'done',
}

/* 十位秒 → 整秒：读的时候不跳，结束时与最后一帧同形 */
function clock(ms: number): string {
  const s = Math.max(0, ms) / 1000
  if (s < 60) return s.toFixed(1) + ' s'
  const m = Math.floor(s / 60)
  return m + 'm ' + String(Math.floor(s % 60)).padStart(2, '0') + 's'
}

/* 「killed 必须带原因」（design parts/lane.js）：只说它停了不是信息。
   原因来自 AgentEnd.ErrorKind（中断/截断时桥侧补上 aborted/max_iterations），
   这里只把它翻成一句人话；认不出来的原样显示，不硬编一个「未知」。 */
const END_WHY: Record<string, string> = {
  aborted: 'stopped by you',
  canceled: 'canceled',
  context_exceeded: 'context limit',
  rate_limit: 'rate limit',
  output_truncated: 'output limit',
  upstream_unavailable: 'upstream down',
  permanent: 'request rejected',
  max_iterations: 'iteration limit',
  generic: 'error',
  error: 'error',
}

function endWhy(b: AgentMsg): string {
  if (b.endKind) return END_WHY[b.endKind] ?? b.endKind
  return b.status === 'abandoned' ? 'abandoned' : ''
}

function stateOf(b: AgentMsg): 'run' | 'stop' | 'done' {
  if (b.status === 'running' || b.status === 'interrupting') return 'run'
  if (b.status === 'completed') return 'done'
  return 'stop'
}

/* 最后一条动作：设计里这一列是「该子 agent 最后一条动作」，不是它的全部历史。
   没有 items 时留空 —— 画上一轮的旧动作等于谎报「它现在在做这个」。 */
function lastAction(items: AgentItem[] | undefined): string {
  const it = items && items.length ? items[items.length - 1] : undefined
  if (!it) return ''
  if (it.kind === 'tool' && it.name) {
    const o = toolArgsSummary(it.name, it.args || '')
    return o ? toolVerb(it.name) + ' ' + o : toolVerb(it.name)
  }
  if (it.kind === 'thinking') return 'thinking'
  if (it.kind === 'compression') return 'compacting'
  if (it.kind === 'text') return (it.text || '').split('\n')[0].slice(0, 72)
  return ''
}

function eventRows(items: AgentItem[] | undefined) {
  return (items || []).slice(-6).map((it, i) => {
    const isTool = it.kind === 'tool' && it.name
    return {
      key: i,
      t: isTool ? toolVerb(it.name!) : it.kind,
      a: isTool
        ? toolArgsSummary(it.name!, it.args || '')
        : it.kind === 'text'
          ? (it.text || '').split('\n')[0].slice(0, 90)
          : '',
      s: isTool ? (it.status === 'error' ? 'fail' : it.durMs != null ? clock(it.durMs) : '') : '',
    }
  })
}

export function Lane({ agents }: { agents: AgentMsg[] }) {
  const [open, setOpen] = useState<Record<string, boolean>>({})
  // 「同时派发」= 起始时刻彼此相差不到 1.2s。设计强调两个同时开跑的子 agent 不能被
  // 读成一前一后，而时间轴要么造假偏移、要么加一条刻度 —— 用一句话说清最省。
  const same = agents
    .filter((a) => agents.some((o) => o !== a && Math.abs((o.spawnedAt || 0) - (a.spawnedAt || 0)) < 1200))
    .map((a) => a.taskId || a.runId)
  const ids = agents.map((a) => a.taskId || a.runId)

  return (
    <div className="la">
      <div className="la-grp">
        <b>
          {agents.length} subagent{agents.length === 1 ? '' : 's'}
        </b>
        {same.length > 1 ? (
          <>
            <span className="sep">·</span>
            <span>{[...new Set(same)].join(' + ')} dispatched together</span>
          </>
        ) : null}
      </div>
      {agents.map((b, i) => {
        const s = stateOf(b)
        const isOpen = Boolean(open[ids[i]])
        const live = s === 'run'
        const started = b.spawnedAt || 0
        const dur = b.durMs != null ? b.durMs : live ? Date.now() - started : 0
        const markState = s === 'run' ? 'run' : s === 'stop' ? 'stop' : 'done'
        const rows = eventRows(b.items)
        return (
          <div className="la-row" data-s={s} data-id={ids[i]} title={ids[i]} data-open={isOpen ? '1' : undefined} key={ids[i] + i}>
            <button className="la-head" type="button" aria-expanded={isOpen} onClick={() => setOpen((m) => ({ ...m, [ids[i]]: !isOpen }))}>
              <span className="la-ico">
                <AgentMark kind="scout" state={markState as 'run' | 'stop' | 'done'} />
              </span>
              <span className="la-id">
                <span className="la-name">{b.label || ids[i]}</span>
              </span>
              <span className="la-state">
                {WORD[s]}
                {/* 只有停下来的那一档需要解释；running 的「为什么」还没发生，
                    done 的原因是它交回的结果本身 */}
                {s === 'stop' && endWhy(b) ? (
                  <>
                    <span className="sep"> · </span>
                    <span className="why">{endWhy(b)}</span>
                  </>
                ) : null}
              </span>
              <span className="la-act">
                <span className="la-act-t">{lastAction(b.items)}</span>
                {live ? <span className="la-live" aria-hidden="true" /> : null}
              </span>
              <span className="la-dur">{clock(dur)}</span>
              <FoldIcon open={isOpen} />
            </button>
            <div className="la-open">
              <div className="la-evs-wrap">
                <div className="la-evs">
                  <div className="la-ev la-ev-h">
                    <span className="t">{ids[i]}</span>
                    <span className="a">{(b.items || []).length} calls</span>
                    <span className="s" />
                  </div>
                  {rows.map((r) => (
                    <div className="la-ev" key={r.key}>
                      <span className="t">{r.t}</span>
                      <span className="a">{r.a}</span>
                      <span className="s">{r.s}</span>
                    </div>
                  ))}
                  {(b.items || []).length > rows.length ? (
                    <div className="la-foot">
                      showing the last {rows.length} of {(b.items || []).length} calls
                    </div>
                  ) : null}
                </div>
              </div>
            </div>
          </div>
        )
      })}
    </div>
  )
}
