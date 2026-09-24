// TraceWaterfall —— 链路 tab 的「树 + 时间轴泳道」瀑布图（docs/TRACE_CHAIN_REDESIGN.md P3）。
//
// 关键工程约束（原型 docs/trace_waterfall_lab.html 已验证）：
//   ① 时间轴不能靠 overflow-x：实测有 36 分钟与 91 分钟的 trace，铺成实宽会到百万像素，
//      浏览器布局崩。改为**时间视窗虚拟化** —— 只渲染视窗内的条，平移改 t0、缩放改 pxPerMs。
//   ② 名称列冻结（不参与时间映射），两列同网格保证行对齐。
//   ③ 并行用横向重叠表达（同级 span 区间重叠）；父子在左边用缩进 + 导引线表达。
//
// 行结构一旦变化（展开/折叠/切 trace）才重建 DOM；平移/缩放只走 transform 式的轻量重绘。

import { useEffect, useMemo, useRef, useState, useCallback } from 'react'
import type { ReactElement } from 'react'
import { fmtDuration } from './ui'
import { useT } from '../i18n'
import type { TraceSpan, TraceDetail } from '../store/trace'

const MAX_PX_PER_MS = 0.4 // 缩放下限之外的放大上限（0.4px/ms：1s = 400px）
const MIN_BAR_PX = 2 // 极短 span 的最小可见宽度
const TICK_STEPS = [10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000, 30000, 60000, 120000, 300000, 600000, 1800000, 3600000]
const TICK_MIN_PX = 70 // 刻度最小间距

const KIND_ORDER: Record<string, number> = { run: 0, llm: 1, tool: 2, compress: 3, wait: 4 }

/** 展平为行（含树导引线所需的祖先信息）。 */
interface Row {
  span: TraceSpan
  depth: number
  ancHas: boolean[]
  isLast: boolean
  kids: number
  key: string
}

/** span 唯一键：kind + id/run + 起始偏移。 */
function spanKey(s: TraceSpan, i: number): string {
  return `${s.kind}:${s.id ?? s.run_id ?? ''}:${s.start_at ?? 0}:${i}`
}

/**
 * buildRows —— 组装树形行序：
 *   run 是树节点（parent_id 建父子）；llm/tool/compress/wait 挂到所属 run 下。
 * 同级按开始时间排序（时间序 = 阅读序）。
 */
export function buildRows(spans: TraceSpan[], collapsed: Set<string>): Row[] {
  const keyed = spans.map((s, i) => ({ s, key: spanKey(s, i) }))
  const runs = keyed.filter((x) => x.s.kind === 'run')
  const byRun = new Map<string, { s: TraceSpan; key: string }>()
  for (const r of runs) if (r.s.run_id) byRun.set(r.s.run_id, r)

  const kidsOfParent = new Map<string, { s: TraceSpan; key: string }[]>()
  for (const r of runs) {
    const p = r.s.parent_id ?? ''
    if (!kidsOfParent.has(p)) kidsOfParent.set(p, [])
    kidsOfParent.get(p)!.push(r)
  }
  // run 的所有后代 span（llm/tool/compress/wait）按 run_id 分组
  const leavesOfRun = new Map<string, { s: TraceSpan; key: string }[]>()
  for (const x of keyed) {
    if (x.s.kind === 'run') continue
    const rid = x.s.run_id ?? ''
    if (!leavesOfRun.has(rid)) leavesOfRun.set(rid, [])
    leavesOfRun.get(rid)!.push(x)
  }

  const out: Row[] = []
  // 排序键：span 的开始时间（同级按时间序 = 阅读序）
  const cmpSpan = (a: { s: TraceSpan }, b: { s: TraceSpan }): number =>
    (a.s.start_at ?? 0) - (b.s.start_at ?? 0) || KIND_ORDER[a.s.kind] - KIND_ORDER[b.s.kind]

  // 主 run = 无 parent 的那个（历史 trace 的 root）；找不到时退化为第一个 run
  const rootEntry = runs.find((r) => !r.s.parent_id) ?? runs[0]
  if (!rootEntry) return out

  const walk = (entry: { s: TraceSpan; key: string }, depth: number, ancHas: boolean[], isLast: boolean): void => {
    const kidRuns = (kidsOfParent.get(entry.s.run_id ?? '') ?? []).slice().sort(cmpSpan)
    const leaves = (leavesOfRun.get(entry.s.run_id ?? '') ?? []).slice().sort(cmpSpan)
    const kids = kidRuns.length + leaves.length
    out.push({ span: entry.s, depth, ancHas, isLast, kids, key: entry.key })
    if (collapsed.has(entry.key)) return
    // 子 run 与叶子混合按时间排序渲染（子 agent 与工具在时间轴上是并列的）
    const merged = [...kidRuns.map((k) => ({ e: k, run: true })), ...leaves.map((k) => ({ e: k, run: false }))]
      .sort((a, b) => cmpSpan(a.e, b.e))
    merged.forEach((m, i) => {
      const last = i === merged.length - 1
      if (m.run) {
        walk(m.e, depth + 1, [...ancHas, !isLast], last)
      } else {
        out.push({
          span: m.e.s, depth: depth + 1,
          ancHas: [...ancHas, !isLast], isLast: last, kids: 0, key: m.e.key,
        })
      }
    })
  }
  walk(rootEntry, 0, [], true)
  return out
}

/** 并行窗口：顶层分支（root 的直接子 span）之间时间重叠的并集。 */
export function parallelWindows(spans: TraceSpan[], total: number): { start: number; end: number; max: number }[] {
  const root = spans.find((s) => s.kind === 'run' && !s.parent_id)
  if (!root) return []
  const branches = spans
    .filter((s) => s !== root && (s.kind === 'run' ? s.parent_id === root.run_id : s.run_id === root.run_id))
    .map((s) => [s.start_at ?? 0, (s.start_at ?? 0) + (s.dur_ms ?? 0)] as [number, number])
    .filter(([a, b]) => b > a)
  if (branches.length < 2) return []
  const pts = [...new Set(branches.flat())].sort((a, b) => a - b)
  const raw: { start: number; end: number; max: number }[] = []
  for (let i = 0; i < pts.length - 1; i++) {
    const t0 = pts[i]
    const t1 = pts[i + 1]
    const c = branches.filter(([a, b]) => a < t1 && b > t0).length
    if (c >= 2) raw.push({ start: t0, end: t1, max: c })
  }
  // 相邻窗口间隔 < 0.5% 总时长时合并（并发期间的工具切换属同一段并行期）
  const out: typeof raw = []
  for (const w of raw) {
    const last = out[out.length - 1]
    if (last && w.start - last.end < total * 0.005) {
      last.end = w.end
      last.max = Math.max(last.max, w.max)
    } else out.push({ ...w })
  }
  return out
}

interface Props {
  detail: TraceDetail
  /** 运行中：时长由父级时钟推进（每秒重算），并自动跟随进度 */
  running?: boolean
  selected?: string | null
  onSelect?: (key: string | null) => void
  tick?: number // 父级时钟（running 时变化 → 重绘）
}

export default function TraceWaterfall({ detail, running, selected, onSelect, tick }: Props): ReactElement {
  const t = useT()
  const laneRef = useRef<HTMLDivElement | null>(null)
  const [laneW, setLaneW] = useState(0)
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())
  const [pxPerMs, setPxPerMs] = useState(0) // 0 = 适应宽度（未初始化）
  const [t0, setT0] = useState(0)
  const [follow, setFollow] = useState(true)
  const [hover, setHover] = useState<string | null>(null)

  const spans = detail.spans
  const rows = useMemo(() => buildRows(spans, collapsed), [spans, collapsed])

  // trace 总时长（含运行中已过时间）
  const total = useMemo(() => {
    let end = 0
    for (const s of spans) end = Math.max(end, (s.start_at ?? 0) + (s.dur_ms ?? 0))
    return Math.max(1, end)
  }, [spans])

  // 泳道宽度测量（右栏可拖宽 → 适应宽度要跟着重算）
  useEffect(() => {
    const el = laneRef.current
    if (!el) return
    const measure = (): void => {
      const w = el.getBoundingClientRect().width
      if (w > 0) setLaneW((prev) => (Math.abs(prev - w) >= 0.5 ? w : prev))
    }
    measure()
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  // 切 trace / 泳道宽度变化：重置为「适应宽度」
  useEffect(() => {
    setPxPerMs(0)
    setT0(0)
    setFollow(true)
    setCollapsed(new Set())
  }, [detail.summary.id])

  const fitPxPerMs = laneW > 0 ? laneW / total : 0
  const effPxPerMs = pxPerMs > 0 ? pxPerMs : fitPxPerMs

  // 边界夹紧
  const clamp = useCallback((next0: number, nextPx: number) => {
    const minPx = laneW > 0 ? laneW / total : 0.0001
    const p = Math.min(MAX_PX_PER_MS, Math.max(minPx, nextPx))
    const win = laneW > 0 ? laneW / p : total
    const maxT0 = Math.max(0, total - win)
    return { t0: Math.min(maxT0, Math.max(0, next0)), px: p }
  }, [laneW, total])

  // 运行中自动跟随：把视窗推到尾部
  useEffect(() => {
    if (!running || !follow || laneW <= 0) return
    const win = laneW / (effPxPerMs || 1)
    const next = Math.max(0, total - win)
    setT0((prev) => (Math.abs(prev - next) > win * 0.02 ? next : prev))
  }, [running, follow, laneW, effPxPerMs, total, tick])

  const timeToX = useCallback((ms: number) => (ms - t0) * effPxPerMs, [t0, effPxPerMs])

  const ticks = useMemo(() => {
    if (effPxPerMs <= 0 || laneW <= 0) return []
    const step = TICK_STEPS.find((v) => v * effPxPerMs >= TICK_MIN_PX) ?? TICK_STEPS[TICK_STEPS.length - 1]
    const out: number[] = []
    const t1 = t0 + laneW / effPxPerMs
    for (let v = Math.ceil(t0 / step) * step; v <= t1; v += step) out.push(v)
    return out
  }, [effPxPerMs, laneW, t0])

  const pars = useMemo(() => parallelWindows(spans, total), [spans, total])

  // 缩放手势：⌘/Ctrl + 滚轮（以光标为锚点）；Shift + 滚轮 / 拖动 = 平移
  const onWheel = useCallback((e: React.WheelEvent) => {
    if (laneW <= 0) return
    const el = laneRef.current
    if (!el) return
    if (e.ctrlKey || e.metaKey) {
      e.preventDefault()
      const rect = el.getBoundingClientRect()
      const anchorX = Math.min(Math.max(0, e.clientX - rect.left), laneW)
      const anchorT = t0 + anchorX / effPxPerMs
      const factor = e.deltaY < 0 ? 1.2 : 1 / 1.2
      const c = clamp(0, effPxPerMs * factor)
      // 锚点不动：t0 使得 anchorT 仍映射到 anchorX
      const nextT0 = anchorT - anchorX / c.px
      const c2 = clamp(nextT0, c.px)
      setPxPerMs(c2.px)
      setT0(c2.t0)
      setFollow(false)
      return
    }
    if (e.shiftKey || Math.abs(e.deltaX) > Math.abs(e.deltaY)) {
      e.preventDefault()
      const dx = (e.deltaX || e.deltaY) / effPxPerMs
      const c = clamp(t0 + dx, effPxPerMs)
      setT0(c.t0)
      setFollow(false)
    }
  }, [laneW, t0, effPxPerMs, clamp])

  // 拖动平移
  const dragRef = useRef<{ x: number; t0: number } | null>(null)
  const onMouseDown = useCallback((e: React.MouseEvent) => {
    if (e.button !== 0 || laneW <= 0) return
    dragRef.current = { x: e.clientX, t0 }
    setFollow(false)
  }, [laneW, t0])
  useEffect(() => {
    const onMove = (e: MouseEvent): void => {
      const d = dragRef.current
      if (!d) return
      const c = clamp(d.t0 - (e.clientX - d.x) / effPxPerMs, effPxPerMs)
      setT0(c.t0)
    }
    const onUp = (): void => { dragRef.current = null }
    window.addEventListener('mousemove', onMove)
    window.addEventListener('mouseup', onUp)
    return () => {
      window.removeEventListener('mousemove', onMove)
      window.removeEventListener('mouseup', onUp)
    }
  }, [clamp, effPxPerMs])

  const zoom = (factor: number): void => {
    const cx = t0 + (laneW / effPxPerMs) / 2
    const c = clamp(0, effPxPerMs * factor)
    const c2 = clamp(cx - (laneW / c.px) / 2, c.px)
    setPxPerMs(c2.px)
    setT0(c2.t0)
    setFollow(false)
  }
  const fit = (): void => { setPxPerMs(0); setT0(0) }
  const focusSel = (): void => {
    const row = rows.find((r) => r.key === selected)
    if (!row || laneW <= 0) return
    const dur = Math.max(row.span.dur_ms ?? 0, total * 0.02)
    const pad = dur * 0.3
    const c = clamp(0, laneW / (dur + pad * 2))
    const c2 = clamp((row.span.start_at ?? 0) - pad, c.px)
    setPxPerMs(c2.px)
    setT0(c2.t0)
    setFollow(false)
  }

  const toggle = (key: string): void =>
    setCollapsed((prev) => {
      const n = new Set(prev)
      if (n.has(key)) n.delete(key)
      else n.add(key)
      return n
    })

  const zLabel = effPxPerMs > 0 ? `${(1 / effPxPerMs).toFixed(1)}ms/px` : '—'
  const winLabel = laneW > 0 ? `${fmtDurShort(t0)} – ${fmtDurShort(t0 + laneW / effPxPerMs)}` : '—'

  return (
    <div className="wf" onWheel={onWheel}>
      {/* 工具条 */}
      <div className="wf-tools">
        <button className="tb" onClick={() => setCollapsed(new Set())}>{t('rp.trace.expandAll')}</button>
        <button className="tb" onClick={() => setCollapsed(new Set(rows.filter((r) => r.kids > 0).map((r) => r.key)))}>{t('rp.trace.collapseAll')}</button>
        <span className="sp" />
        <span className="zr mono" title={t('rp.trace.zoomHint')}>{zLabel}</span>
      </div>

      {/* 刻度尺 + 行 */}
      <div className="wf-ruler">
        <div className="rl-l">
          <span>{t('rp.trace.colSpan')}</span>
          <span className="sp" />
          <span>{t('rp.trace.colDur')}</span>
        </div>
        <div className="rl-lane" ref={laneRef}>
          {ticks.map((v) => {
            const x = timeToX(v)
            return (
              <span key={v} className="tick mono" style={{ left: x }}>
                {fmtTick(v)}
              </span>
            )
          })}
        </div>
      </div>

      <div className="wf-body" onMouseDown={onMouseDown}>
        {/* 并行窗口底纹（横跨所有行；用绝对定位层，避免逐行写背景） */}
        <div className="wf-ovl" aria-hidden="true">
          {pars.map((w, i) => {
            const x0 = timeToX(w.start)
            const x1 = timeToX(w.end)
            if (x1 < 0 || x0 > laneW) return null
            return <i key={i} className="parw" style={{ left: Math.max(0, x0), width: Math.max(1, Math.min(laneW, x1) - Math.max(0, x0)) }} />
          })}
        </div>

        {rows.map((r) => {
          const s = r.span
          const closed = collapsed.has(r.key)
          const x = timeToX(s.start_at ?? 0)
          const w = Math.max(MIN_BAR_PX, (s.dur_ms ?? 0) * effPxPerMs)
          const vis = s.dur_ms == null && s.status === 'running'
          // 视窗外的条只在贴边处留提示（不静默消失）
          const offLeft = x + w < 0
          const offRight = x > laneW
          const barCls = `bar k-${s.kind}${s.status === 'error' ? ' err' : s.status === 'running' ? ' busy' : s.status === 'interrupted' ? ' warn' : ''}`
          const durText = w >= 46 ? fmtDuration(s.dur_ms) : ''
          return (
            <div
              key={r.key}
              className={`wf-row${r.depth === 0 ? ' is-main' : ''}${s.kind === 'tool' ? ' is-tool' : ''}${selected === r.key ? ' sel' : ''}${hover === r.key ? ' hov' : ''}`}
              onMouseEnter={() => setHover(r.key)}
              onMouseLeave={() => setHover((h) => (h === r.key ? null : h))}
              onClick={() => onSelect?.(selected === r.key ? null : r.key)}
              title={`${s.kind}｜${s.label ?? s.name ?? ''}\n+${fmtDurShort(s.start_at ?? 0)} · ${fmtDuration(s.dur_ms)}`}
            >
              <div className="wf-name">
                {r.ancHas.map((on, i) => <i key={i} className={`tg${on ? ' on' : ''}`} />)}
                {r.depth > 0 && <i className={`tg ${r.isLast ? 'el' : 'on'}`} />}
                <button
                  className={`chev${closed ? ' closed' : ''}${r.kids > 0 ? '' : ' hide'}`}
                  onClick={(e) => { e.stopPropagation(); toggle(r.key) }}
                  aria-label={closed ? t('rp.trace.expand') : t('rp.trace.collapse')}
                >
                  <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="m6 9 6 6 6-6" /></svg>
                </button>
                <span className={`glyph ${s.status === 'error' ? 'bad' : s.status === 'running' ? 'busy' : s.status === 'interrupted' ? 'warn' : 'ok'}`}>{spanGlyph(s)}</span>
                <span className="lbl">{s.kind === 'tool' ? (s.name ?? s.label ?? '') : (s.label ?? s.label)}</span>
                {s.retries ? <span className="badge" title={t('rp.trace.retries').replace('{n}', String(s.retries))}>↻{s.retries}</span> : null}
                {s.promoted ? <span className="badge bg">{t('tool.promoted')}</span> : null}
                {closed && r.kids > 0 ? <span className="badge">+{r.kids}</span> : null}
                <span className="dur mono">{durText}</span>
              </div>
              <div className="wf-lane">
                {!offLeft && !offRight && (
                  <span className={barCls} style={{ left: x, width: w }}>
                    {vis && <i className="pulse" />}
                  </span>
                )}
                {(offLeft || offRight) && <i className={`offmark ${offLeft ? 'l' : 'r'}`} />}
              </div>
            </div>
          )
        })}
        {rows.length === 0 && <div className="wf-empty">{t('rp.trace.noRunHint')}</div>}
      </div>

      {/* 底部：缩放 + 视窗 + 跟随 */}
      <div className="wf-foot">
        <div className="zc">
          <button data-z="out" onClick={() => zoom(1 / 1.5)} title={t('rp.trace.zoomOut')}>−</button>
          <button data-z="in" onClick={() => zoom(1.5)} title={t('rp.trace.zoomIn')}>＋</button>
          <button data-z="fit" onClick={fit} title={t('rp.trace.zoomFit')}>{t('rp.trace.fit')}</button>
          <button data-z="focus" onClick={focusSel} disabled={!selected} title={t('rp.trace.zoomFocus')}>{t('rp.trace.focus')}</button>
          <button data-z="follow" className={follow ? 'on' : ''} onClick={() => setFollow((f) => !f)} title={t('rp.trace.followHint')}>{t('rp.trace.follow')}</button>
          <span className="sp" />
          <span className="val dim mono">{winLabel}</span>
        </div>
      </div>
    </div>
  )
}

function spanGlyph(s: TraceSpan): string {
  if (s.status === 'error') return '✗'
  if (s.status === 'running') return '▶'
  if (s.status === 'interrupted') return '⏸'
  // 成功统一 ✓（工具曾用 ◇，与 run/llm/compress/wait 不同形；状态字形只表达状态）
  return '✓'
}

/** 短时长文本（刻度/提示用）。 */
export function fmtDurShort(ms: number): string {
  if (ms < 1000) return `${Math.round(ms)}ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`
  const m = Math.floor(ms / 60_000)
  const s = Math.round((ms % 60_000) / 1000)
  return s >= 1 ? `${m}m${s}s` : `${m}m`
}

/** 刻度文本：按量级给 m/s 形式。 */
function fmtTick(ms: number): string {
  if (ms < 1000) return `${Math.round(ms)}ms`
  if (ms < 60_000) return `${Math.round(ms / 1000)}s`
  const m = Math.floor(ms / 60_000)
  const s = Math.round((ms % 60_000) / 1000)
  return s ? `${m}m${s}s` : `${m}m`
}
