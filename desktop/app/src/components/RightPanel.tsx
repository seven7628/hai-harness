import { Fragment, useEffect, useMemo, useRef, useState, useCallback } from 'react'
import type { ReactElement, ReactNode } from 'react'
import { useAppStore, isPendingWebTab, rebuildWebTab } from '../store/useAppStore'
import type { RightTab, TabKind, TabInstance, TabSpec } from '../store/useAppStore'
import type { AnyEvent } from '../store/events'
import { projectAllTraces, liveRootRunId } from '../store/trace'
import type { TraceDetail } from '../store/trace'
import TraceWaterfall, { buildRows } from './TraceWaterfall'
import SpanSheet, { lookupPayload } from './SpanSheet'
import { Activity, GitBranch, FileIcon, CheckList, Globe, PanelRight, ChevronDown, Plus } from './icons'
import { fmtTokens, fmtUsd, fmtDuration } from './ui'
import { ChangesTab } from './ChangesTab'
import { WebTab } from './WebTab'
import { FilePreviewTab } from './FilePreview'
import { EditorConfirm } from './EditorConfirm'
import { fileUrlToPath } from '../lib/filePaths'
import { fmtBytesShort, ratio } from '../lib/format'
import { getSurface } from '../surfaces/registry'
import type {} from '../lib/fileStatus'
import { shortName } from '../lib/shortPath'
import { copyText } from '../lib/clipboard'
import { useT } from '../i18n'
import TodoPanel from './TodoPanel'
import { useFocusTodos } from './useFocusTodos'

// Tab 分两段：主段 = 会话工作流核心（链路 · 变更）；辅段 = 预览工具（文件 · 网页）。
// 这里**没有**「任务」tab（2026-10 用户拍板退役）：子智能体/异步 Bash 就是另一个会话，
// 住在左栏会话树里，点开即子会话视图；它的待审批/压缩/待办与主会话**同一套**面板，
// 只是按「你在看谁」分域渲染（见 sessionScope.ts）。
// 跨会话用量看板在设置「使用统计」页（原「指标」tab 已下线，见 docs/DESKTOP.md §5.4）。
// 分组让「主次」从视觉层级表达出来，而非 6 个平级小字挤一行。
function primaryTabs(t: (k: string) => string): { key: RightTab; label: string; icon: ReactElement }[] {
  return [
    { key: 'activity', label: t('rp.tab.activity'), icon: <Activity /> },
    { key: 'git', label: t('rp.tab.git'), icon: <GitBranch /> },
  ]
}
function secondaryTabs(t: (k: string) => string): { key: RightTab; label: string; icon: ReactElement }[] {
  return [
    { key: 'file', label: t('rp.tab.file'), icon: <FileIcon /> },
    { key: 'web', label: t('rp.tab.web'), icon: <Globe /> },
  ]
}

// 链路 tab = 执行链路（trace）瀑布图：树 + 时间轴泳道，点击 Span 升起详情抽屉。
// 数据源（docs/TRACE_CHAIN_REDESIGN.md）：
//   - 当前 trace：前端由事件流实时投影（store.traceEvents → projectAllTraces），零往返
//   - 历史 trace：bridge `trace` 命令按需拉取（读事件日志投影，零新增存储）
// 顶部 = trace 选择器（一个会话 = 多条 trace；Alt+←/→ 切换）+ 汇总；正文按锚点从 blocks 回查。
function ActivityTab() {
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

// GitStatus 空值（工作区未探测/非仓库时的兜底展示）
// 标签标题/类型图标本体已随 R06b 迁进 surfaces/builtin/webSurface.tsx（那里是 web 分支的
// 唯一消费者；若将来第二种 kind 也要用，再提升到 lib —— 本轮不预先抽象）。

// 标签条最多平铺几个（超出收进「+N」）。取 5：与原型一致，且 360px 宽的右栏里
// 5 个 × 最窄 86px 已经是「还能读出文件名」的下限。

// 主 Agent 压缩镜像（右栏任务 tab）：与主会话同一数据源 —— 进行中显示活动条
// （顶层 compressing/compressBefore 镜像），完成后渲染主会话同一个 compression block
// （blocks 尾部最后一条 kind==='compression'）。不新建第二套活动状态。
// 「+」菜单里**没有** file：文件标签需要路径，由点文件产生（阶段 2 起是**多实例**：
// 一个文件一个标签，同一文件的 diff / source 各一个 —— 见 store.openTab 的幂等键）。
const ADDABLE_KINDS: TabKind[] = ['activity', 'git', 'web']
function kindIcon(kind: TabKind): ReactElement {
  switch (kind) {
    case 'git': return <GitBranch size={12} />
    case 'file': return <FileIcon size={12} />
    case 'web': return <Globe size={12} />
    default: return <Activity size={12} />
  }
}

export default function RightPanel() {
  const t = useT()
  const allTabs = primaryTabs(t).concat(secondaryTabs(t))
  const rightExpanded = useAppStore((s) => s.rightExpanded)
  const rightTab = useAppStore((s) => s.rightTab)
  const setRightTab = useAppStore((s) => s.setRightTab)
  const setRightExpanded = useAppStore((s) => s.setRightExpanded)
  const toggleRight = useAppStore((s) => s.toggleRight)
  const rightLayout = useAppStore((s) => s.rightLayout)
  const applyBias = useAppStore((s) => s.applyBias)
  const browserConfig = useAppStore((s) => s.browserConfig)
  // —— 标签模型（阶段 1）——
  const tabsByScope = useAppStore((s) => s.tabsByScope)
  const activeSessionId = useAppStore((s) => s.activeSessionId)
  const workspace = useAppStore((s) => s.workspace)
  const openTab = useAppStore((s) => s.openTab)
  const activateTab = useAppStore((s) => s.activateTab)
  const closeTab = useAppStore((s) => s.closeTab)
  const closeTabs = useAppStore((s) => s.closeTabs)
  const reopenTab = useAppStore((s) => s.reopenTab)
  const syncTabs = useAppStore((s) => s.syncTabsToRightTab)
  const embedNav = useAppStore((s) => s.embedNav)
  const unreadViews = useAppStore((s) => s.unreadViews)
  const fetchBrowserShot = useAppStore((s) => s.fetchBrowserShot)
  const scope = activeSessionId || workspace || ''
  const tabSet = tabsByScope[scope]
  const tabs = tabSet?.tabs ?? []
  const closedTabs = tabSet?.closed ?? []
  const activeTab = tabs.find((x) => x.id === tabSet?.activeId) ?? null
  // 编辑标签的脏标记（PR-05b）：真相在 lib/editorDocs，store 只镜像布尔值（标签圆点读它）。
  const dirtyTabs = useAppStore((s) => s.dirtyTabs)
  // 面板级菜单（⌄ 重开 / ＋ 新建 / 标签右键）：定位与内容仍是组件局部 state（菜单要锚坐标、
  // 要读标签），**开合**另镜像一份到 store.panelMenuOpen —— R06c 起 web 面板由 webSurface 渲染，
  // WebTab 不在本组件的 state 树里，读不到这两个局部 state，而它必须知道「有菜单开着」才能
  // 隐藏原生视图（原生层级 > DOM，否则菜单被预览画面盖掉）。
  // 两个 setter 调用点（下面 setMenu / setCtx）各自同步镜像；ref 只是为了让两个 wrapper 保持
  // **稳定身份** —— 否则 ctx 的全局 click/Escape effect 会因为依赖变化每次渲染都重订阅。
  const [menu, setMenuState] = useState<'add' | 'more' | null>(null)
  // 标签右键菜单（R4 的批量关闭 + 文件动作；内层标签条退役后搬到这里）。
  const [ctx, setCtxState] = useState<{ x: number; y: number; id: string } | null>(null)
  const setPanelMenuOpen = useAppStore((s) => s.setPanelMenuOpen)
  const menuRef = useRef<'add' | 'more' | null>(null)
  const ctxRef = useRef<{ x: number; y: number; id: string } | null>(null)
  const syncPanelMenuOpen = useCallback(() => {
    setPanelMenuOpen(menuRef.current !== null || ctxRef.current !== null)
  }, [setPanelMenuOpen])
  const setMenu = useCallback((next: 'add' | 'more' | null | ((cur: 'add' | 'more' | null) => 'add' | 'more' | null)) => {
    const v = typeof next === 'function' ? next(menuRef.current) : next
    menuRef.current = v
    setMenuState(v)
    syncPanelMenuOpen()
  }, [syncPanelMenuOpen])
  const setCtx = useCallback((next: { x: number; y: number; id: string } | null) => {
    ctxRef.current = next
    setCtxState(next)
    syncPanelMenuOpen()
  }, [syncPanelMenuOpen])
  // 卸载时归零：panelMenuOpen 描述的是**本组件子树里**那个菜单，状态不该比它的所有者活得久。
  // RightPanel 真的会卸载（App 的 <main> 由 mainReady 控制，启动/重载期间不渲染子树）——
  // 若那一次卸载恰好带着开着的菜单，残留的 true 会让下一次挂载的 WebTab **永远**隐藏原生视图
  //（面板一片空白，且没有任何报错）。这是唯一一个不属于「镜像」的写点，故意留在这里而不是
  // 藏在 setter 里：它是生命周期约束，不是菜单状态的来源。
  useEffect(() => () => setPanelMenuOpen(false), [setPanelMenuOpen])
  const ctxTab = ctx ? tabs.find((x) => x.id === ctx.id) ?? null : null
  const ctxIndex = ctxTab ? tabs.findIndex((x) => x.id === ctxTab.id) : -1
  // 菜单里的文件动作需要**绝对路径**：web 标签从 file:// URL 还原（阶段 5：待重建的恢复标签
  // 不在池里，退化用它自己的 url 快照 —— 「复制路径 / 在访达中显示」对它们仍然可用）；
  // file 标签用工作区相对路径补全。
  const ctxPath = ctxTab?.kind === 'web'
    ? fileUrlToPath(embedNav?.targets?.find((x) => x.id === ctxTab.viewId)?.url ?? ctxTab.url ?? '')
    : ctxTab?.kind === 'file' && ctxTab.path
      ? (ctxTab.path.startsWith('/') ? ctxTab.path : `${workspace ?? ''}/${ctxTab.path}`)
      : ''
  const ctxClose = (id: string) => { closeTab(id); setCtx(null) }
  // 预览没有工具条（见 WebTab 的注释）→ 刷新收进右键菜单；用 embedReloadTarget 只刷**这个**
  // 标签的页面（不是激活视图：右键的标签可能不是当前激活的那个）。
  const ctxRefresh = () => {
    if (ctxTab?.kind === 'web') {
      if (ctxTab.viewId) void window.desktop?.embedReloadTarget?.(ctxTab.viewId)
      else if (ctxTab.pageId != null) fetchBrowserShot(ctxTab.pageId)
    }
    setCtx(null)
  }
  // 批量关闭走 closeTabs：干净的一次关掉，脏的进确认队列（逐个问）。
  // 早先这里是 `for (...) closeTab(x)` —— 守卫虽然兜住了内容，但多个脏标签只有一个能被问到，
  // 用户点完"丢弃"还得再点一次关其它（评审 #4）。
  const ctxCloseOthers = () => { closeTabs(tabs.filter((x) => x.id !== ctx?.id).map((x) => x.id)); setCtx(null) }
  const ctxCloseRight = () => { closeTabs(tabs.slice(ctxIndex + 1).map((x) => x.id)); setCtx(null) }
  const ctxCopyPath = async () => { if (ctxPath) await copyText(ctxPath); setCtx(null) }
  const ctxReveal = async () => { if (ctxPath) await window.desktop?.showItemInFolder?.(ctxPath); setCtx(null) }
  const ctxOpenDefault = async () => { if (ctxPath) await window.desktop?.openPath?.(ctxPath); setCtx(null) }
  useEffect(() => {
    if (!ctx) return
    const onDown = (e: MouseEvent) => {
      if (!(e.target as HTMLElement | null)?.closest?.('.ctx-menu')) setCtx(null)
    }
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setCtx(null) }
    window.addEventListener('mousedown', onDown)
    window.addEventListener('keydown', onKey)
    return () => {
      window.removeEventListener('mousedown', onDown)
      window.removeEventListener('keydown', onKey)
    }
    // setCtx 是 useCallback（身份稳定）：加进依赖只是把「它稳定」写成可检查的事实，
    // 不会让 effect 重订阅 —— 但漏了它，panelMenuOpen 的镜像就会在闭包里悄悄失联。
  }, [ctx, setCtx])
  // 可见性仲裁的口径与 WebTab 主体一致：**只看呈现方式**，不看插件/引擎状态 ——
  // 「打开文件/网页」不依赖浏览器插件（见 §8.7），把这道兜底闸门挂在插件上会让它在
  // **默认配置**（插件未启用）下整体失效，而默认配置恰恰是面板现在会挂载的情形。
  // 历史：这里曾写 `!embeddedPresentation && !egoEngine` + `!egoEngine && !browserEnabled`
  // —— 同样的「能力耦合工具面」模式，且 embeddedPresentation 缺「无配置→默认内嵌」的 fallback。
  const embeddedPresentation = browserConfig?.presentation === 'embedded' || !browserConfig

  // 全局可见性仲裁（本组件常驻挂载，折叠态也渲染）：面板未展开或不在 web tab →
  // 原生视图必须隐藏。可见性判定权收归渲染层——此前 embed.ts 创建视图时强制显示，
  // 后台会话触发 Browser Use 时白屏浮在无关界面上方。web tab 内的细粒度控制
  // （hasPages/全屏弹窗）由 WebTab 的 effect 接管。
  // 只 hide 不 show：show 由 WebTab 的可见性 effect（hasPages/弹窗）负责——
  // 本层只管「右栏收起/非 web tab 时强制隐藏」（刷新竞态：建连 effect 已改为建连即
  // hide，不会在右栏收起时浮出；此处兜底任何状态下右栏收起都隐藏）。
  useEffect(() => {
    if (!embeddedPresentation) return
    if (!(rightExpanded && activeTab?.kind === 'web')) void window.desktop?.embedHide()
  }, [embeddedPresentation, rightExpanded, activeTab?.kind])

  // 标签集单向收敛到 rightTab：rightTab 有多条改写路径（动作 / loadViewToTop 会话恢复 /
  // e2e 直接 setState 复位），在渲染层收敛一处覆盖全部；已一致时 store 侧不 set（防自激）。
  useEffect(() => { syncTabs() }, [syncTabs, rightTab, scope])

  // 阶段 5：**视图按需重建** —— 重启恢复出来的 web 标签没有视图（恢复刻意不 eager 建，
  // 见 store 的 loadPersistedTabs / rebuildWebTab），只有它真的成了「当前正在显示的内容」
  // 才在主进程里建出来。判据三条缺一不可：面板展开 + 当前显示 web + 激活标签就是待重建的那个。
  // 为什么放在这里（而不是点击处）：待重建标签成为激活标签有两条路径 —— 用户点它、以及
  // 「重启时它就是激活标签，用户只是把面板展开」；后者不在渲染层兜住就是一个永远空的网页面板。
  // 依赖用**标签 id**（不是对象）：重建期间同步链会重排标签数组，用对象做依赖会让同一次
  // 重建被触发两次（双开视图）。
  const pendingWebTabId = activeTab && isPendingWebTab(activeTab) ? activeTab.id : ''
  useEffect(() => {
    if (!rightExpanded || activeTab?.kind !== 'web' || !pendingWebTabId) return
    const tab = useAppStore.getState().tabsByScope[scope]?.tabs.find((x) => x.id === pendingWebTabId)
    if (tab && isPendingWebTab(tab)) rebuildWebTab(tab)
  }, [rightExpanded, activeTab?.kind, pendingWebTabId, scope])

  const todos = useAppStore((s) => s.todos)
  // M6 的「网页沉浸态」随 R06c 迁进 components/WebTab.tsx（它只有一个消费者，且 provider 的
  // render 拿不到 props）：RightPanel 不再持有 immersive / toolbarHidden。
  // 看的是哪个子智能体 → 待办面板/角标跟随它（判定在 useFocusTodos，与面板同一份）
  const focusTodos = useFocusTodos()
  // 活跃待办数（未完成）→ 折叠图标条角标（跟随焦点，与面板同源）
  const activeTodos = (focusTodos?.todos ?? todos).filter((todo) => !todo.done).length

  // 标签显示名：file → 文件名；web → 当前页标题（退化到主机名）；其余 → kind 的 i18n 文案。
  const kindLabel = (k: TabKind) => t(`rp.tab.${k}`)
  // 图标：web 标签按页面 URL 判类型（pdf/图片/xlsx/网页…，与产出卡/文件栏共用 visualKindOf），
  // 其余按 kind。未读时**用未读点替代**图标（R4 口径：未读时用户最需要知道的是「有新东西」）。
  // 阶段 5：待重建的恢复标签不在视图池里（viewId 为空）→ 退化用它的 **url 快照**，
  // 否则重启后这些标签会丢掉图标与名字（用户看到一排「网页」）。
  // R06b 起 web 的「名字 / 类型图标」由 webSurface 提供（visualKindOf 那套判定随之搬走，
  // 仍是单一份）；RP 只保留 webTabUrl —— 标签的 title 属性还要用它。fallback 只在
  // provider 未注册时可达：main.tsx 的副作用 import 保证不会发生。
  const webTabUrl = (tab: TabInstance): string =>
    tab.kind === 'web' ? (embedNav?.targets?.find((x) => x.id === tab.viewId)?.url ?? tab.url ?? '') : ''
  // 返回 ReactNode（而非 ReactElement）：provider 的 icon 签名是 ReactNode，见 types.ts。
  const tabIconEl = (tab: TabInstance): ReactNode =>
    tab.kind === 'web' ? (getSurface('web')?.icon?.(tab) ?? kindIcon('web')) : kindIcon(tab.kind)
  const tabTitle = (tab: TabInstance): string => {
    if (tab.kind === 'file') {
      const base = (tab.path ?? '').split('/').filter(Boolean).pop()
      // 长文件名中间截断（保留 `_test.go` 这类尾巴）：标签最宽 168px，从尾巴截断时
      // `conversation_router.go` 与 `conversation_router_test.go` 会长得一模一样。
      // 完整路径在 title 里（见渲染点的 title 属性）。
      return (base && shortName(base)) || kindLabel('file')
    }
    if (tab.kind === 'web') return getSurface('web')?.title(tab, t) ?? kindLabel('web')
    return kindLabel(tab.kind)
  }

  if (!rightExpanded) {
    return (
      <aside className="right collapsed" aria-label={t('rp.aria')}>
        <div className="rail right-rail" style={{ flex: 1 }}>
          {/* 常驻展开钮（与左侧 rail 的 PanelLeft 对称） */}
          <button
            className="rbtn"
            data-tip={t('topbar.toggleRight')}
            aria-label={t('topbar.toggleRight')}
            onClick={() => setRightExpanded(true)}
          >
            <PanelRight size={15} />
          </button>
          {todos.length > 0 && (
            <button
              className="rbtn"
              data-tip={t('todo.title')}
              aria-label={t('todo.title')}
              onClick={() => setRightExpanded(true)}
            >
              <CheckList />
              {activeTodos > 0 && <span className="todo-badge mono">{activeTodos}</span>}
            </button>
          )}
          {allTabs.map((tab) => {
            // 折叠态未读角标（阶段 4 的可见性兜底）：标签条只在展开态存在，而「AI/Agent 后台
            // 产出不抢视线」之后，用户唯一能知道「有东西在等我」的地方就是这条 rail。
            // 只数**本 scope 标签集里真实存在**的未读视图（unreadViews 是全局数组，可能留着
            // 别的会话的 id；在别的会话点了「已读」也不该影响这里的数字）。
            const unread = tab.key === 'web'
              ? tabs.filter((x) => x.kind === 'web' && !!x.viewId && unreadViews.includes(x.viewId)).length
              : 0
            return (
              <button key={tab.key} className={`rbtn ${rightTab === tab.key ? 'on' : ''}`}
                data-tip={tab.label}
                aria-label={tab.label}
                onClick={() => {
                  setRightTab(tab.key)
                  setRightExpanded(true) // 点击图标 → 展开 + 切到该 tab（此前只切换不展开 = 点不动）
                }}>
                {unread > 0 && <span className="todo-badge unread-badge mono">{unread}</span>}
                {tab.icon}
              </button>
            )
          })}
        </div>
      </aside>
    )
  }

  return (
    <aside className="right expanded" aria-label={t('rp.aria')}>
      <div style={{ flex: 1, display: 'flex', flexDirection: 'column', minHeight: 0 }}>
        {/* 待办面板跟随「你在看谁」（作用域判定在面板内部，见 TodoPanel）：看主会话 =
            会话清单；看某个子智能体 = 它自己的清单（绝不回退到主会话清单）。 */}
        <TodoPanel />
        <div className="rtabs" role="tablist" aria-label={t('rp.aria')}>
          <button
            className="rtabs-more"
            data-tip={t('rp.tabs.reopen')}
            aria-label={t('rp.tabs.reopen')}
            onClick={() => setMenu((m) => (m === 'more' ? null : 'more'))}
          >
            <ChevronDown size={13} />
          </button>
          <div className="rtabs-list">
            {tabs.map((tab) => {
              const on = tab.id === tabSet?.activeId
              // 阶段 3：休眠/恢复态的标签标记（灰态 + 「已休眠」/「正在恢复…」）。
              // 休眠条目仍在 embedNav.targets 里（sleeping: true）—— 标签身份不丢，点一下就恢复。
              // 阶段 5：**重启恢复的待重建标签**用同一套视觉（它同样是「有标签、没视图，点一下
              // 就有」的形态 —— 视图留给用户点击那一刻才建，见 store 的 rebuildWebTab）。
              const tp = tab.kind === 'web' ? embedNav?.targets?.find((x) => x.id === tab.viewId) : undefined
              const pending = isPendingWebTab(tab)
              const sleeping = !!tp?.sleeping || pending
              const restoring = !!tp?.restoring
              return (
                <div
                  key={tab.id}
                  className={`rtab ${on ? 'on' : ''} ${sleeping ? 'sleeping' : ''}`}
                  data-kind={tab.kind}
                  data-view={tab.kind === 'web' ? (tab.viewId ?? (tab.pageId != null ? `p${tab.pageId}` : '')) : ''}
                  data-sleeping={sleeping ? '1' : undefined}
                  data-pending={pending ? '1' : undefined}
                  data-restoring={restoring ? '1' : undefined}
                  role="tab"
                  aria-selected={on}
                  tabIndex={on ? 0 : -1}
                  title={restoring ? t('browser.tabRestoring') : sleeping ? t('browser.tabSleeping') : (webTabUrl(tab) || (tab.kind === 'file' ? (tab.path || tabTitle(tab)) : tabTitle(tab)))}
                  onClick={() => activateTab(tab.id)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); activateTab(tab.id) }
                  }}
                  onContextMenu={(e) => { e.preventDefault(); setCtx({ x: e.clientX, y: e.clientY, id: tab.id }) }}
                >
                  {tab.kind === 'web' && tab.viewId && unreadViews.includes(tab.viewId)
                    ? <span className="browser-page-dot" title={t('browser.tabUnread')} />
                    : <span className="rtab-ico">{tabIconEl(tab)}</span>}
                  <span className="rtab-t">{restoring ? t('browser.tabRestoring') : tabTitle(tab)}</span>
                  {/* 未保存圆点：位置在标题后、✕ 前（✕ 的命中区不受影响），title 给读屏与 hover 文案 */}
                  {dirtyTabs[tab.id] && <span className="rtab-dirty" title={t('editor.dirty')} aria-label={t('editor.dirty')} />}
                  <button
                    className="rtab-close"
                    title={t('rp.tabs.close')}
                    aria-label={t('rp.tabs.close')}
                    onClick={(e) => { e.stopPropagation(); closeTab(tab.id) }}
                  >
                    ✕
                  </button>
                </div>
              )
            })}
          </div>
          {/* ＋ 的定位容器：新建菜单锚在**按钮**下方（贴按钮右缘），而不是标签条左侧 ——
              用户口径「菜单应该锚在「＋」按钮下面（右侧），不是左侧」。⌄ 的重开菜单保持锚在左侧。 */}
          <div className="rtabs-add-wrap">
            <button
              className="rtabs-add"
              data-tip={t('rp.tabs.add')}
              aria-label={t('rp.tabs.add')}
              onClick={() => setMenu((m) => (m === 'add' ? null : 'add'))}
            >
              <Plus size={13} />
            </button>
            {menu === 'add' && <div className="rtabs-menu rtabs-menu-right" role="menu">
              {ADDABLE_KINDS.map((k) => (
                <button
                  key={k}
                  className="rtabs-mi"
                  role="menuitem"
                  onClick={() => { openTab({ kind: k } as TabSpec); setMenu(null) }}
                >
                  {kindIcon(k)} {kindLabel(k)}
                </button>
              ))}
            </div>}
          </div>
          <div className="rp-bias" role="group" aria-label={t('rp.bias.label')}>
            {(['even', 'center', 'right'] as const).map((b) => (
              <button
                key={b}
                className={'rp-bias-b' + (rightLayout.open && rightLayout.bias === b ? ' on' : '')}
                onClick={() => applyBias(b)}
                title={t('rp.bias.' + b)}
                aria-label={t('rp.bias.' + b)}
                aria-pressed={rightLayout.open && rightLayout.bias === b}
              >
                {t('rp.bias.' + b)}
              </button>
            ))}
          </div>
          <button className="tab-collapse" onClick={toggleRight} title={t('topbar.toggleRight')} aria-label={t('topbar.toggleRight')}>
            <PanelRight size={15} />
          </button>
          {ctx && ctxTab && (
            <div className="ctx-menu" style={{ left: ctx.x, top: ctx.y }} role="menu">
              <button className="ctx-item" role="menuitem" onClick={() => ctxClose(ctxTab.id)}>{t('browser.menu.close')}</button>
              {ctxTab.kind === 'web' && (
                <button className="ctx-item" role="menuitem" onClick={ctxRefresh}>{t('rp.git.refresh')}</button>
              )}
              <button className="ctx-item" role="menuitem" disabled={tabs.length < 2} onClick={ctxCloseOthers}>{t('browser.menu.closeOthers')}</button>
              <button
                className="ctx-item"
                role="menuitem"
                disabled={ctxIndex < 0 || ctxIndex >= tabs.length - 1}
                onClick={ctxCloseRight}
              >
                {t('browser.menu.closeRight')}
              </button>
              {ctxPath && (
                <>
                  <div className="ctx-sep" />
                  <button className="ctx-item" role="menuitem" onClick={() => void ctxCopyPath()}>{t('browser.menu.copyPath')}</button>
                  <button className="ctx-item" role="menuitem" onClick={() => void ctxReveal()}>{t('browser.menu.reveal')}</button>
                  <button className="ctx-item" role="menuitem" onClick={() => void ctxOpenDefault()}>{t('browser.menu.openDefault')}</button>
                </>
              )}
            </div>
          )}
          {menu && (
            <>
              <div className="rtabs-backdrop" onClick={() => setMenu(null)} />
              {/* 重开（⌄）菜单：仍锚在标签条左侧（它本来就该在 ⌄ 下面）。＋ 的新建菜单在
                  上面的 .rtabs-add-wrap 里，用 .rtabs-menu-right 锚到按钮右缘下方。 */}
              {menu === 'more' && <div className="rtabs-menu" role="menu">
                {closedTabs.length === 0
                  ? <div className="rtabs-mi rtabs-mi-empty">{t('rp.tabs.closedEmpty')}</div>
                  : closedTabs.map((tab) => (
                    <button
                      key={tab.id}
                      className="rtabs-mi"
                      role="menuitem"
                      onClick={() => { reopenTab(tab.id); setMenu(null) }}
                    >
                      {kindIcon(tab.kind)} {tabTitle(tab)}
                    </button>
                  ))}
              </div>}
            </>
          )}
        </div>
        <div className={`right-body ${activeTab?.kind === 'file' ? 'file-mode' : ''}`}>
          {!activeTab && (
            <div className="empty rtabs-empty">
              <div className="big">{t('rp.tabs.empty')}</div>
              {t('rp.tabs.emptyHint')}
            </div>
          )}
          {activeTab?.kind === 'activity' && <ActivityTab />}
          {activeTab?.kind === 'git' && (getSurface('git')?.render({ tab: activeTab, scope }) ?? <ChangesTab />)}
          {/* key={activeTab.id}：**每个 file 标签一份组件实例**（阶段 2 的隔离手段）。
              为什么不共用一个实例再按 props 复位：file 标签之间切换时元素类型/位置不变，
              React 会复用实例 → 组件 state（mdMode/csvMode/detailMode/列表展开…）会从上一个
              标签**泄漏**到下一个标签（用户在 A 切了「纯文本」，切到 B 却看到纯文本）。
              加 key 后每个标签的状态天然各归各的；滚动位置另存（见 FilePreviewTab 的
              fileTabScroll —— 重挂载会丢 DOM 的 scrollTop）。 */}
          {activeTab?.kind === 'file' && (
            // key 挂在 Fragment 上：切换文件标签要整体重挂（原实现直接在组件上写 key）
            <Fragment key={activeTab.id}>
              {getSurface('file')?.render({ tab: activeTab, scope }) ?? <FilePreviewTab tab={activeTab} />}
            </Fragment>
          )}
          {/* R06c：web 面板走 provider 渲染，`??` 回退到本地 WebTab —— 与上面 git/file 同法。
              为什么必须留着回退：provider 是**副作用注册**（只有 main.tsx 导入 builtin 时才存在），
              而若干探针页只 import RightPanel。没有回退时，那些探针会渲染出空白面板，
              且「面板空白」不会让任何断言变红（最坏的一种失败）。
              注意**不要**给这个元素加 key：切 web 标签时组件实例必须原地复用（DOM 容器不重挂载，
              原生视图与页面状态才保得住）—— 加 key = 每次切标签都销毁重建 WebContentsView。 */}
          {activeTab?.kind === 'web' && (getSurface('web')?.render({ tab: activeTab, scope }) ?? <WebTab tab={activeTab} scope={scope} />)}
        </div>
      </div>
      {/* 脏内容确认框：关标签与退出应用共用一个（reason 区分）。挂在 aside 末尾而不是 body 里
          —— 标签被关掉后 body 可能已经是空态，框还得在。 */}
      <EditorConfirm />
    </aside>
  )
}
