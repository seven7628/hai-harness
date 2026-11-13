// 链路（activity）面板：执行链路瀑布图 —— 树 + 时间轴泳道，点击 Span 升起详情抽屉。
//
// PR-07 把它从 RightPanel 逐字平移到这里（R06c 的 WebTab 同法）：组件本体不动一行，
// RightPanel 只保留「provider 优先、`??` 回退」的一行委派，注册与元数据在
// surfaces/builtin/activitySurface.tsx。判据：`git show <R06d>:…/RightPanel.tsx` 的
// 45–293 行与本文件 30 行之后逐字相同（diff 只允许出现在文件头与 import）。
//
// 为什么能整体搬走：它只依赖 store（useAppStore）与三个纯函数（projectAllTraces /
// liveRootRunId / buildRows），**没有** RightPanel 的 props 或局部 state —— R07 的前置检查，
// 见 PR-07 报告。反过来，任何想让它吃 RP 局部 state 的改动都该先问一句：这层是不是该留在
// store（R06c 的 panelMenuOpen 就是这么上移的）。
//
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import type { AnyEvent } from '../store/events'
import { projectAllTraces, liveRootRunId } from '../store/trace'
import type { TraceDetail } from '../store/trace'
import TraceWaterfall, { buildRows } from './TraceWaterfall'
import SpanSheet, { lookupPayload } from './SpanSheet'
import { fmtTokens, fmtUsd, fmtDuration } from './ui'
import { fmtBytesShort, ratio } from '../lib/format'
import { useT } from '../i18n'

// 链路 tab = 执行链路（trace）瀑布图：树 + 时间轴泳道，点击 Span 升起详情抽屉。
// 数据源（docs/TRACE_CHAIN_REDESIGN.md）：
//   - 当前 trace：前端由事件流实时投影（store.traceEvents → projectAllTraces），零往返
//   - 历史 trace：bridge `trace` 命令按需拉取（读事件日志投影，零新增存储）
// 顶部 = trace 选择器（一个会话 = 多条 trace；Alt+←/→ 切换）+ 汇总；正文按锚点从 blocks 回查。
export function ActivityTab() {
  const traceEvents = useAppStore((s) => s.traceEvents)
  const traceList = useAppStore((s) => s.traceList)
  const traceListTotal = useAppStore((s) => s.traceListTotal)
  const traceListLoading = useAppStore((s) => s.traceListLoading)
  const traceHasMore = useAppStore((s) => s.traceHasMore)
  const activeTraceId = useAppStore((s) => s.activeTraceId)
  const traceDetail = useAppStore((s) => s.traceDetail)
  const traceFallback = useAppStore((s) => s.traceFallback)
  const blocks = useAppStore((s) => s.blocks)
  const runNodes = useAppStore((s) => s.runNodes)
  // reqBlock（request_id → assistant 块 id）是 view 内部字段，不镜像顶层：
  // 正文回查需要它把 llm span 的 request_id 落到具体块上。
  const reqBlock = useAppStore((s) => s.views[s.activeSessionId ?? '']?.reqBlock)
  const runningAgentCount = useAppStore((s) => s.runningAgentCount)
  const fetchTraces = useAppStore((s) => s.fetchTraces)
  const selectTrace = useAppStore((s) => s.selectTrace)
  const t = useT()

  // 详情抽屉选中项（span key）
  const [selKey, setSelKey] = useState<string | null>(null)
  const [pop, setPop] = useState(false)
  const [loadedAll, setLoadedAll] = useState(false)

  // ① 实时投影：当前 trace（最近一条）+ 全部 trace（供选择器与切回实时）
  // liveRoot = 仍在运行的顶层 run（来自 runNodes：实时维护 + 恢复时归一化）：
  // 让「无 agent_end 的 root run」显示为 running 而不是 interrupted。
  const liveRoot = liveRootRunId(runNodes as { id: string; kind: string; status: string; parentId?: string }[])
  const liveTraces = useMemo(
    () => projectAllTraces(traceEvents as AnyEvent[], liveRoot),
    [traceEvents, liveRoot],
  )
  const liveLatest = liveTraces[0]

  // ② 选中态：activeTraceId 为空 = 跟随最新（实时）；否则用命令拉回的历史详情。
  // 刷新/切回会话后 traceEvents 为空（checkpoint 不恢复它）→ 实时投影无内容，
  // 此时回退到命令兜底拉回的 traceFallback，避免误报「尚无运行」。
  // 优先级：实时投影 > 兜底（一旦有新事件，实时投影立即接管）。
  const showingHistory = !!activeTraceId
  const liveAvailable = !!liveLatest
  const detail = showingHistory ? traceDetail : (liveLatest ?? traceFallback)
  // 「正在运行」：实时可用时看投影状态；兜底数据来自磁盘，其 running 由后端判定
  const running = !showingHistory && detail?.summary.status === 'running'

  // 汇总列表：历史列表优先（含更早的 trace），无则用实时投影
  const list = useMemo(() => {
    if (traceList?.length) return traceList
    return liveTraces.map((d) => d.summary)
  }, [traceList, liveTraces])

  // 进入 tab 拉一次历史列表（每个会话一次）
  const askedRef = useRef<string | null>(null)
  const sid = useAppStore((s) => s.activeSessionId) || ''
  useEffect(() => {
    if (!sid) return
    if (askedRef.current === sid) return
    askedRef.current = sid
    setLoadedAll(false)
    setSelKey(null)
    fetchTraces(sid)
  }, [sid, fetchTraces])

  // 切换选中 trace 时清抽屉
  useEffect(() => { setSelKey(null) }, [detail?.summary.id])

  // Alt + ←/→ 切换 trace（列表按最近在上：← 更早，→ 更新）
  // idx = 当前在列表中的位置；跟随实时（activeTraceId 空）时视为「最新位」。
  // 实时投影的 id 与历史列表可能对不上（未落盘的最新 run / mock 工具），故 idx<0 时按 0 处理。
  const idxRaw = useMemo(() => {
    if (!detail) return -1
    return list.findIndex((x) => x.id === detail.summary.id)
  }, [list, detail])
  const idx = idxRaw >= 0 ? idxRaw : 0
  const go = useCallback((delta: number) => {
    const next = idx + delta
    if (next < 0 || next >= list.length) return
    // 回到最新位 → 复位为「跟随实时」（用实时投影，零往返）；否则拉该条历史详情
    if (next === 0) selectTrace(undefined, sid)
    else selectTrace(list[next].id, sid)
  }, [idx, list, selectTrace, sid])
  useEffect(() => {
    const onKey = (e: KeyboardEvent): void => {
      if (!e.altKey || e.metaKey || e.ctrlKey) return
      if (e.key === 'ArrowLeft') { e.preventDefault(); go(-1) }
      else if (e.key === 'ArrowRight') { e.preventDefault(); go(1) }
      else if (e.key === 'Escape') setPop(false)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [go])

  // 选中 span 的正文回查（tool_id / request_id → blocks）
  const rows = useMemo(() => (detail ? buildRows(detail.spans, new Set()) : []), [detail])
  const selSpan = useMemo(() => {
    if (!selKey || !detail) return null
    const hit = detail.spans.find((s, i) => `${s.kind}:${s.id ?? s.run_id ?? ''}:${s.start_at ?? 0}:${i}` === selKey)
    return hit ?? null
  }, [selKey, detail])
  const payload = useMemo(
    () => lookupPayload(blocks, selSpan, reqBlock),
    [blocks, selSpan, reqBlock],
  )
  void rows

  if (!detail) {
    return (
      <div className="empty">
        <div className="big">{t('rp.trace.noRun')}</div>
        {traceListLoading ? t('rp.trace.listLoading') : t('rp.trace.noRunHint')}
      </div>
    )
  }

  const sum = detail.summary
  const st = sum.status
  const statusLabel = st === 'running' ? t('rp.trace.status.running') : st === 'error' ? t('rp.trace.status.error') : st === 'interrupted' ? t('rp.trace.status.interrupted') : t('rp.trace.status.done')
  const glyph = st === 'error' ? '✗' : st === 'running' ? '▶' : st === 'interrupted' ? '⏸' : '✓'
  const gcls = st === 'error' ? 'bad' : st === 'running' ? 'busy' : st === 'interrupted' ? 'warn' : 'ok'
  const total = Math.max(1, sum.wall_ms)
  const ks = kindShare(detail)
  const more = traceListTotal != null && list.length < traceListTotal && traceHasMore && !loadedAll

  return (
    <>
      {/* trace 选择器：一个会话 = 多条 trace（最近在上） */}
      <div className="tr-rail" style={{ position: 'relative' }}>
        <button className="tr-nav" onClick={() => go(-1)} disabled={idx <= 0} title={t('rp.trace.prev')}>‹</button>
        <button className={`tr-pick${pop ? ' on' : ''}`} onClick={() => setPop((p) => !p)}
          title={t('rp.trace.pick').replace('{n}', String(traceListTotal ?? list.length))}>
          <span className="tr-idx mono">{`${idx + 1}/${traceListTotal ?? list.length}`}</span>
          <span className={`g ${gcls}`}>{glyph}</span>
          <span className="tr-lab">{sum.label}</span>
          <span className="cv">{pop ? '▴' : '▾'}</span>
        </button>
        <button className="tr-nav" onClick={() => go(1)} disabled={idx >= list.length - 1} title={t('rp.trace.next')}>›</button>

        {pop && (
          <div className="tr-pop">
            {/* 列表体独立滚动：弹层自身限高，行数一多也不会把上半截顶出可视区。
                此前 .tr-pop 为绝对定位且缺 top，静态位置落在 .tr-rail（flex 居中）的
                中线 → 行数 ≥4 时上半越出 .right-body 顶边；块级 start 方向溢出按规范
                不可见且不可滚 → 只能看到 3 条（共 5 条）。 */}
            <div className="tr-pop-body">
              {list.map((x, i) => (
                <button key={x.id} className={`tr-row${i === idx ? ' on' : ''}`}
                  onClick={() => { setPop(false); if (i === 0) selectTrace(undefined, sid); else selectTrace(x.id, sid) }}>
                  <span className={`g ${x.status === 'error' ? 'bad' : x.status === 'running' ? 'busy' : x.status === 'interrupted' ? 'warn' : 'ok'}`}>
                    {x.status === 'error' ? '✗' : x.status === 'running' ? '▶' : x.status === 'interrupted' ? '⏸' : '✓'}
                  </span>
                  <span className="mid">
                    <span className="l1">{i === 0 ? <span className="cur">{t('rp.trace.pickLabel')}</span> : null}{x.label}</span>
                    <span className="l2 mono">
                      {x.spans} spans · {x.tools} tools · {x.turns} {t('rp.trace.turns').replace('{n}', '').trim()}
                      {x.subs ? ` · ${x.subs} 子` : ''}
                      {x.errors ? ` · ${x.errors} ✗` : ''}
                    </span>
                    <span className="mix">
                      {x.turns > 0 && <i className="k-llm" style={{ width: `${ratio(x.turns, x.spans)}%` }} />}
                      {x.tools > 0 && <i className="k-tool" style={{ width: `${ratio(x.tools, x.spans)}%` }} />}
                    </span>
                  </span>
                  <span className="r">
                    <span className="dur mono">{fmtDuration(x.wall_ms)}</span>
                    <span className="ago mono">{fmtBytesShort(x.meta_bytes)}</span>
                  </span>
                </button>
              ))}
              {more && (
                // 加载更多：显式请求「全部」—— 后端 limit<=0 才回落默认上限，
                // 不传会重复返回同一批（长会话 112 条 trace 时按钮点了没反应）。
                <button className="more" onClick={() => { setLoadedAll(true); fetchTraces(sid, traceListTotal ?? 0) }}>
                  {t('rp.trace.more').replace('{n}', String((traceListTotal ?? 0) - list.length))}
                </button>
              )}
            </div>
          </div>
        )}
      </div>

      {/* trace 汇总 header */}
      <div className="trace-hd">
        <div className="th-line1">
          <span className={`th-st ${gcls === 'ok' ? 'ok' : gcls === 'busy' ? 'busy' : ''}`}>{glyph}</span>
          <span className="th-label" title={sum.label}>{sum.label}</span>
          <span className="th-status mono">{statusLabel}</span>
        </div>
        <div className="th-meta mono">
          {/* 时间对比：墙钟 vs 各类累计 —— 并发时累计可超墙钟，差额就是「并行省下的时间」 */}
          <span>{t('rp.trace.wall')} <b>{fmtDuration(sum.wall_ms)}</b></span>
          <span>
            {t('rp.trace.total')} <b>{fmtDuration(ks.sum)}</b>
            {!running && ks.sum > sum.wall_ms ? ` · ${t('rp.trace.saved').replace('{dur}', fmtDuration(ks.sum - sum.wall_ms))}` : ''}
          </span>
          <span>{sum.spans} spans</span>
          {sum.tools > 0 && <span>{sum.tools} tools</span>}
          {sum.turns > 0 && <span>{t('rp.trace.turns').replace('{n}', String(sum.turns))}</span>}
          {sum.subs > 0 && <span>{t('rp.trace.subsTitle').replace('{n}', String(sum.subs))}</span>}
          {sum.in_tok + sum.out_tok > 0 && <span>{fmtTokens(sum.in_tok + sum.out_tok)}</span>}
          {sum.cost_usd > 0 && <span>{fmtUsd(sum.cost_usd)}</span>}
          {sum.errors > 0 && <span style={{ color: 'var(--error)' }}>{t('rp.trace.errors').replace('{n}', String(sum.errors))}</span>}
        </div>
        {/* 时间占比条 + 图例：一眼看出这条 trace 主要花在哪 */}
        <div className="th-bar">
          {ks.segs.map((k) => (
            <i key={k.kind} className={`k-${k.kind}`} style={{ width: `${(k.ms / total) * 100}%` }}
              title={`${t(`rp.trace.kind.${k.kind}`)} ${fmtDuration(k.ms)}`} />
          ))}
        </div>
        <div className="th-legend mono">
          {ks.segs.map((k) => (
            <span key={k.kind}><i className={`k-${k.kind}`} />{t(`rp.trace.kind.${k.kind}`)} {fmtDuration(k.ms)}</span>
          ))}
        </div>
      </div>

      {/* 瀑布图（树 + 时间轴泳道） */}
      <TraceWaterfall
        detail={detail}
        running={running}
        selected={selKey}
        onSelect={setSelKey}
        tick={runningAgentCount}
      />

      {/* Span 详情抽屉（正文按锚点回查，不在 trace 里） */}
      {selSpan && <SpanSheet span={selSpan} payload={payload} onClose={() => setSelKey(null)} />}
    </>
  )
}

/**
 * 各类 span 的累计时长（占比条 + 图例 + 时间对比的数据源）。
 * 并发时各类之和可超墙钟 —— 不归一化：差额正是「并行省下的时间」，
 * 归一会把这个信息抹掉（原型 trace_waterfall_lab.html:1539 同口径）。
 */
function kindShare(d: TraceDetail): { segs: { kind: string; ms: number }[]; sum: number } {
  const acc = new Map<string, number>()
  for (const s of d.spans) {
    if (s.kind === 'run') continue
    acc.set(s.kind, (acc.get(s.kind) ?? 0) + (s.dur_ms ?? 0))
  }
  const segs = [...acc.entries()].filter(([, ms]) => ms > 0).map(([kind, ms]) => ({ kind, ms }))
  return { segs, sum: segs.reduce((a, x) => a + x.ms, 0) }
}
