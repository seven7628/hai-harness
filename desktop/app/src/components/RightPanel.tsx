import { Fragment, useEffect, useMemo, useRef, useState, useCallback } from 'react'
import type { ReactElement } from 'react'
import { useAppStore, isPendingWebTab, rebuildWebTab } from '../store/useAppStore'
import type { RightTab, TabKind, TabInstance, TabSpec } from '../store/useAppStore'
import type { AnyEvent } from '../store/events'
import { projectAllTraces, liveRootRunId } from '../store/trace'
import type { TraceDetail } from '../store/trace'
import TraceWaterfall, { buildRows } from './TraceWaterfall'
import SpanSheet, { lookupPayload } from './SpanSheet'
import { Activity, GitBranch, FileIcon, BarChart, CheckList, Globe, PanelRight, ImageIcon, ChevronDown, Plus } from './icons'
import { fmtTokens, fmtUsd, fmtDuration } from './ui'
import { ChangesTab } from './ChangesTab'
import { FilePreviewTab } from './FilePreview'
import { fileUrlToPath } from '../lib/filePaths'
import { fmtBytesShort, ratio } from '../lib/format'
import { getSurface } from '../surfaces/registry'
import type {} from '../lib/fileStatus'
import { visualKindOf } from '../lib/visualFile'
import { centerOf, overlayCoversRect, rectsIntersect } from '../lib/overlayHit'
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
/** 标签标题：file:// → 文件名；否则 title → 主机名 → 原 URL。 */
function tabLabel(url: string, title: string): string {
  if (url.startsWith('file://')) {
    const base = fileUrlToPath(url).split('/').filter(Boolean).pop()
    if (base) return base
  }
  if (title) return title
  try {
    return new URL(url).host || url
  } catch {
    return url
  }
}

/** 标签类型图标（与产出卡/文件栏共用 visualKindOf 这一事实源，避免两处漂移）。 */
function tabIcon(url: string): ReactElement {
  switch (visualKindOf(fileUrlToPath(url) || url)) {
    case 'xlsx': return <BarChart size={12} />
    case 'image': return <ImageIcon size={12} />
    case 'pdf':
    case 'docx':
    case 'pptx': return <FileIcon size={12} />
    case 'browser': return <Globe size={12} />
    default: return /^https?:/.test(url) ? <Globe size={12} /> : <FileIcon size={12} />
  }
}

// 标签条最多平铺几个（超出收进「+N」）。取 5：与原型一致，且 360px 宽的右栏里
// 5 个 × 最窄 86px 已经是「还能读出文件名」的下限。

// embedRectOf：把一个 DOM 容器量成「给原生视图用的矩形」；**量不出来返回 undefined**。
//
// 为什么不返回一个钳过的最小矩形（2026-09-21 用户报障的根因）：
//   收起右栏时 BrowserTab 卸载，而 ResizeObserver 对**已脱离文档**的元素还会补发一次
//   「尺寸 0」的通知 → 量到 0×0。旧实现在这里钳成 `Math.max(80, …)`，于是这次「量不到」被
//   当成「容器只有 80×80」下发：`embed:resize` 把**所有**原生视图缩到 80×80，而
//   `Emulation.setDeviceMetricsOverride` 更狠 —— 它把面板里**正显示的那一页**的排版视口钉死在
//   80×80（页面按 80px 宽排版：内容只剩左上角一小块、页面底色铺满整块面板）。而且它**不会自愈**：
//   同尺寸下切换标签不重挂载组件、ResizeObserver 也不再触发 → 那一页的视口留着错的（用户原话
//   「图片、网页等无法正常加载」，刷新也救不回来）。
//   正确的处置是「量不到就什么都不下发」：隐藏路径本就有 embedHide 兜底；主进程对 show 收到
//   非法/缺失 rect 时也有右侧栏默认区域兜底（见 electron/embed.ts 的 isValidEmbedRect）。
function embedRectOf(el: Element | null): { x: number; y: number; width: number; height: number } | undefined {
  if (!el || !el.isConnected) return undefined
  const r = el.getBoundingClientRect()
  const width = Math.round(r.width)
  const height = Math.round(r.height)
  if (width < 1 || height < 1) return undefined
  return { x: Math.round(r.x), y: Math.round(r.y), width, height }
}

function BrowserTab({ toolbarHidden, onToggleTabs, panelVisible, activePageId, menuOpen }: { toolbarHidden?: boolean; onToggleTabs?: () => void; panelVisible?: boolean; activePageId?: number; menuOpen?: boolean }) {
  const t = useT()
  const plugins = useAppStore((s) => s.plugins)
  const browserPages = useAppStore((s) => s.browserPages)
  const browserShot = useAppStore((s) => s.browserShot)
  const browserBusy = useAppStore((s) => s.browserBusy)
  const fetchPlugins = useAppStore((s) => s.fetchPlugins)
  const fetchBrowserPages = useAppStore((s) => s.fetchBrowserPages)
  const fetchBrowserShot = useAppStore((s) => s.fetchBrowserShot)
  const shotRef = useRef<HTMLDivElement>(null)
  const browserConfig = useAppStore((s) => s.browserConfig)
  const fetchBrowserConfig = useAppStore((s) => s.fetchBrowserConfig)
  const browserEngine = useAppStore((s) => s.browserEngine) // 引擎=ego：浏览器任务走 ego lite，本面板仅 file:// 预览
  const setRightSize = useAppStore((s) => s.setRightSize)
  const startEmbedEndpoint = useAppStore((s) => s.startEmbedEndpoint)
  const attachEmbedView = useAppStore((s) => s.attachEmbedView)
  // 全屏 DOM 弹窗状态：打开期间必须隐藏原生内嵌视图——WebContentsView 恒合成在窗口
  // DOM 之上（原生层级 > DOM），不隐藏会盖住设置/提问等弹窗（CSS z-index 压不住）。
  const inSettings = useAppStore((s) => s.inSettings)
  const inSkills = useAppStore((s) => s.inSkills)
  const inCron = useAppStore((s) => s.inCron)
  const showNewWsModal = useAppStore((s) => s.showNewWsModal)
  const askQuestion = useAppStore((s) => s.questions.length > 0)
  const askRelease = useAppStore((s) => s.release)
  const uiModalOpen = inSettings || inSkills || inCron || showNewWsModal || askQuestion || !!askRelease

  // —— M5/M6：工具条 / AI 动作可视化 / 回退可见化 状态（embed 事件由 store 层全局订阅）——
  const nav = useAppStore((s) => s.embedNav)
  // R0：当前会话（页列表只展示本会话的视图；Electron 侧视图池按 sid 归属）
  const activeSessionId = useAppStore((s) => s.activeSessionId)
  const aiActive = useAppStore((s) => s.embedAiActive)
  const lastAction = useAppStore((s) => s.embedLastAction)
  const [urlInput, setUrlInput] = useState('')
  const [embedError, setEmbedError] = useState('') // 非空 = 建连失败，回退截图模式（可见化，不再静默）
  const [retryNonce, setRetryNonce] = useState(0) // 手动重试建连
  // R4：后台打开的标签未读点 + 右键菜单（原型 .pv-tab .dot / .ctx-menu）
  const urlRef = useRef<HTMLInputElement>(null)

  const browser = plugins.find((p) => p.id === 'browser')
  const enabled = browser?.status === 'enabled'
  const egoMode = browserEngine === 'ego'
  // 本地预览能力**不依赖浏览器插件**（2026-09 定论）：
  //   · 面板里「打开文件/网页」是 Electron 原生内嵌视图的能力，属于应用本身；
  //   · Browser Use 插件只提供**工具面**（MCP 页列表 / 截图 / AI 驱动的动作）；
  //     引擎 ego 与 mcp 的区别只是「用哪个浏览器」，不是能力分级。
  // 历史：原生视图路径曾整体挂在插件 enabled 上 → 插件没开（或 ego 与插件互斥被停用）时
  // 右栏只剩「未启用」占位、file:// 打不开（用户两次报障）。两次都靠加条件绕（`|| egoMode`），
  // 但根因是**能力与工具面耦合**。这里按上面的口径一次解开。
  // 呈现方式：显式 embedded，或**没有配置时默认 embedded**（与 store 的 embedded 默认值同口径）。
  // 插件未启用 → browserConfig 不存在 → 旧代码据此判成「非内嵌」→ 面板整块不渲染。
  const embeddedMode = browserConfig?.presentation === 'embedded' || !browserConfig
  const nativeViewMode = embeddedMode
  // 实时视图是否可用：原生视图模式且建连未失败。此时原生视图就是画面本身，
  // 不再叠加 <img> 截图（截图仅 window 模式 / 回退态使用）。
  const embedLive = nativeViewMode && !embedError
  // P0 观测契约（§3.5）：分层状态——未安装 / 已安装未启用 / 启用连接中 / 已就绪 / 错误。
  // 不再把所有非 enabled 状态都显示成「浏览器未启用」。
  const browserNotInstalled = browser && browser.status !== 'enabled' && !browser.enabled && browser.status !== 'ready' && browser.status !== 'installing' && browser.status !== 'update-available'
  const browserInstalledNotEnabled = browser && browser.status !== 'enabled' && !browser.enabled && (browser.status === 'ready' || browser.status === 'update-available')
  const browserConnecting = browser && browser.enabled && browser.status !== 'enabled' // 已启用但状态机仍非 enabled（update-available 掩盖）
  const browserError = browser && browser.status === 'error'
  // 已启用：再看 MCP 连接阶段（在线 / 连接中 / 错误）
  const browserOnline = enabled && (!browser?.mcp || browser.mcp.state === 'connected')
  const browserMCPConnecting = enabled && browser?.mcp && browser.mcp.state === 'idle'
  const browserMCPError = enabled && browser?.mcp && browser.mcp.state === 'error'
  const browserStateText = browserError ? t('browser.state.error')
    // 引擎=ego 优先判定：插件被停用（互斥）是预期状态，不是「未安装/未启用」故障
    // ——否则头部显示「已安装，未启用」，用户以为面板坏了（2026-09 报障）。
    : egoMode ? t('browser.state.ego')
    : browserNotInstalled ? t('browser.state.notInstalled')
    // 装了但工具面没启用**不是故障**：面板仍能用（本地预览）→ 如实说「本地预览」。
    // 此前这里显示「已安装，未启用」，用户以为面板坏了（ego 那次报障就是这么来的）。
    : browserInstalledNotEnabled ? t('browser.state.localPreview')
    : browserConnecting ? t('browser.state.connecting')
    : browserMCPConnecting ? t('browser.state.connecting')
    : browserMCPError ? t('browser.state.error')
    : browserOnline ? t('browser.state.on') : t('browser.state.off')
  // M3：插件 presentation=embedded 时，web tab 呈现「内嵌浏览器」而非静态截图。
  // 注意：只依赖 presentation，不依赖 enabled——embedded 模式下需先起内嵌转发器注入端点，
  // 插件才能 Enable 成功（否则死锁：Enable 要端点、端点靠 web tab、web tab 又看 enabled）。
  // 是否「启用内嵌浏览器并连接」：只要 web tab 打开且插件启用就建立（创建 native 视图 +
  // attach 调试代理 + 注入转发器），让 browser_* 工具驱动内嵌视图。**不能**以「有页面」为前提
  // ——attach 依赖页面、页面又依赖 attach，会死锁，导致插件回退启动系统 Chrome（双开）且
  // 内嵌视图永远白屏。页面数据经 browser_pages 事件按需拉取。
  // 视觉可见性单独由 hasPages 控制：空闲（无页面）隐藏 native 视图（露出占位，避免空白白框）。
  // M6：页面清单双数据源——实时视图可用 = Electron 真实视图池（nav.targets，真开真关的
  // 多页面）；回退/window 模式 = MCP 页列表（截图语义）。两套 id 空间不同：前者 'tN' 字符串，
  // 后者 MCP 数字 pageId，切换/关闭动作分别路由到 embedActivate/embedCloseTarget 或 bridge 命令。
  // 注意过滤纯 about:blank 占位视图（池里常驻一个默认空视图，不算「有页面」），
  // 否则空闲态永远判为有页 → 白屏盖住占位引导。
  // R0：只展示**本会话**的视图（无归属的自举视图不展示）。保留全量状态在 Electron 侧，
  // 便于 embedStatus 诊断，且切会话时无需往返 IPC 重新拉取。
  const navTargets = (nav?.targets ?? []).filter(
    (tp) => tp.url && tp.url !== 'about:blank' && (!tp.sid || tp.sid === activeSessionId),
  )
  const hasPages = embedLive ? navTargets.length > 0 : browserPages.length > 0
  // 目标页（回退/MCP 模式）：由**激活的 web 标签**派生（阶段 1.5 起标签是唯一的选择真相源；
  // 组件本地的 shotPage 已删 —— 否则标签条与截图页会各说各话）。
  // page-id 路由下截图必须带 pageId（无则服务器报错），故保留「选中页/首页」兜底。
  const targetPage = activePageId ?? browserPages.find((p) => p.selected)?.id ?? browserPages[0]?.id
  // 供建连 effect 读取当前页面数（不放进依赖，避免 setup 重跑导致重建/闪烁）
  const hasPagesRef = useRef(hasPages)
  hasPagesRef.current = hasPages

  useEffect(() => {
    fetchPlugins()
  }, [fetchPlugins])
  // M3：拉取浏览器配置（presentation 决定呈现方式；embedded 需先注入端点，故不依赖 enabled）
  useEffect(() => {
    fetchBrowserConfig()
     
  }, [fetchBrowserConfig])

  // M3：embedded 模式 → 按 Browser Use 设置里的 viewport 调宽右侧面板（钳制到面板上限 900）
  useEffect(() => {
    if (!embeddedMode) return
    const vpW = /^(\d+)x(\d+)$/.exec(browserConfig?.viewport ?? '')
    if (!vpW) return
    const w = parseInt(vpW[1], 10)
    const max = Math.min(900, Math.max(320, window.innerWidth - 320)) // 留中栏余量，钳制面板上限
    setRightSize(Math.max(320, Math.min(w, max)))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [embeddedMode, browserConfig?.viewport])

  // M3/M5：embedded 模式 → 建连内嵌浏览器（创建 native 视图 + debugger 代理 + attach 转发器），
  // 并用 ResizeObserver 持续把 native 视图同步到 .browser-shot 容器尺寸（面板展开/拖动跟随）。
  // 建连不依赖页面数（见上死锁说明）；空闲时隐藏 native 视图，露出占位提示。
  // M5：失败不再静默回退——错误写入 embedError 在面板明示（含重试按钮）。
  useEffect(() => {
    if (!nativeViewMode) return
    const el = shotRef.current
    if (!el) return
    // 量一个**真实**矩形；量不出来返回 undefined（不造数）。见 embedRectOf 的注释
    //（把「量不到」钳成 80×80 正是 2026-09-21 用户报障「预览只剩左上角一小块、刷新也不恢复」的根因）。
    const rect = () => embedRectOf(el)
    // 量到「布局位置」而不是「动画中的位置」：rect() 用 getBoundingClientRect()，它含 transform，
    // 而面板内容区带 180ms 入场动画（.right-body 的 slideRight：translateX(18px)→0）。动画期间
    // 量到的是「动画中的位置」——实测同一个容器随动画相位差 1~4px（e2e:pdf 的 T3 容差 ±2px，
    // 就是被它打红打绿的）。ResizeObserver 只报尺寸变化、不报位移，所以必须补量。
    // 判据用 document.getAnimations() 找「作用于 el 或其子树」的运行中动画：比只监听
    // animationend 稳 —— ResizeObserver 的回调可能落在动画中间，而动画早已开始（事件错过）。
    const applyRect = (r0: { x: number; y: number; width: number; height: number }) => {
      if (window.desktop?.embedResize) void window.desktop.embedResize(r0)
      // M6：视口对齐（渲染层兜底，免重启）——上游可能按默认 1280x720 仿真导致截断，
      // 强制对齐真实容器尺寸；主进程侧同名劫持（embed.ts）重启后接管。
      void window.desktop
        ?.embedCommand({ method: 'Emulation.setDeviceMetricsOverride', params: { width: r0.width, height: r0.height, deviceScaleFactor: 1, mobile: false } })
        .catch(() => {})
    }
    // 注意方向：动画在**祖先**上（.right-body），它的 transform 会平移 el —— 所以判据必须是
    // 「作用于 el 或它的**祖先**」，而 animationend 也只会从祖先向**上**冒泡、永远到不了 el
    //（第一版两处都写反了，实测仍差 4px：那是动画剩 4px 时量到的位置）。
    const animatedAncestor = () => {
      if (typeof document.getAnimations !== 'function') return undefined
      return document.getAnimations().find((a) => {
        if (a.playState !== 'running') return false
        const t = (a.effect as KeyframeEffect | null)?.target
        if (!(t instanceof Element)) return false
        return t === el || t.contains(el)
      })
    }
    const sync = () => {
      // 量不到（组件卸载中 / DOM 还没布局）→ **什么都不下发**：这时候下发就是把
      // 「量不到」当成「容器只有 80×80」写进原生视图与页面排版视口（见 embedRectOf）。
      // 收起/卸载路径的隐藏由本 effect 的 cleanup 与 RightPanel 的可见性 effect 负责。
      const r0 = rect()
      if (!r0) return
      applyRect(r0)
      const running = animatedAncestor()
      // 动画结束再补一枪：ResizeObserver 只报尺寸变化，动画结束本身不会再触发它。
      if (running) running.finished.then(() => {
        const r1 = rect()
        if (r1) applyRect(r1)
      }).catch(() => { /* 动画被取消：忽略 */ })
    }
    // 事件路径同样补一次（挂在真正被动画的那个祖先上）
    const animRoot = el.closest('.right-body') ?? el
    animRoot.addEventListener('animationend', sync)
    animRoot.addEventListener('animationstart', sync)
    let ro: ResizeObserver | null = null
    ;(async () => {
      try {
        // 确保内嵌端点已启动（enable/启动恢复时已起，幂等）
        startEmbedEndpoint()
        // 建连（创建 native 视图 + debugger 代理 + attach）；显示与否由 panelVisible
        // （右栏展开）决定——右栏收起时建连即 hide，杜绝「刷新后右栏收起、视图仍浮在
        // 最上层挡住主页面」的竞态（此前建连无条件 show，可见性 effect 兜不住异步时序）。
        const shown = await window.desktop?.embedShow({ rect: rect() })
        if (!shown?.ok) throw new Error(shown?.error || t('rp.browser.embedCreateFailed'))
        if (!panelVisible) void window.desktop?.embedHide() // 右栏收起：建连不显示
        const proxy = await window.desktop?.embedDebuggerProxy()
        if (!proxy?.ok || !proxy.endpoint) throw new Error(proxy?.error || t('rp.browser.proxyFailed'))
        attachEmbedView(proxy.endpoint)
        setEmbedError('')
        // 面板/容器尺寸变化 → 同步 native 视图 bounds
        if ('ResizeObserver' in window) {
          ro = new ResizeObserver(sync)
          ro.observe(el)
        }
        // 空闲（当前无页面）→ 隐藏 native 视图露出占位；有页面交给 hasPages effect 显示
        if (!hasPagesRef.current) await window.desktop?.embedHide()
      } catch (e) {
        setEmbedError(e instanceof Error ? e.message : String(e))
        void window.desktop?.embedHide() // 回退态不残留半就绪视图
      }
    })()
    return () => {
      animRoot.removeEventListener('animationend', sync)
      animRoot.removeEventListener('animationstart', sync)
      ro?.disconnect()
      if (window.desktop?.embedHide) void window.desktop.embedHide()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nativeViewMode, retryNonce, panelVisible])

  // 通用「覆盖层」检测（2026-09-20 修用户报的缺陷）：上面那份 uiModalOpen 名单是**手维护**的，
  // 新增弹窗必漏 —— Composer 的「压缩上下文？」确认弹窗（CompactConfirm，`.modal-hint`）就不在
  // 名单里。而 `.modal-hint` 是 `position:fixed; inset:0` + 居中 → 弹窗按**整个窗口**居中（卡片
  // 360px / compact-card 420px），窗口 1440 宽时右栏从 x=552 起 → 弹窗横跨 510~930，**右半整段
  // 压在右栏上**，被原生预览画面盖住（用户截图：文字被切、按钮被压）。
  // 故这里改成按 DOM 判定：任何高层级覆盖层只要与右栏矩形**相交**就隐藏视图。名单保留作兜底
  //（覆盖层是 fixed 定位、右栏尚未挂载等边界情况仍靠它）。
  const [domOverlayOpen, setDomOverlayOpen] = useState(false)
  useEffect(() => {
    if (!nativeViewMode) return
    // 全屏弹窗（.modal-hint / .settings-overlay / .img-zoom）+ 光标处浮层（.ctx-menu / .sel-popup /
    // .settings-pop）：后者只有真的压在右栏上时才隐藏 —— 在中栏弹出的右键菜单不该让右栏黑一下。
    const SEL = '.modal-hint.open, .settings-overlay, .img-zoom, .rt-lb, .cy-lb, .ctx-menu, .sel-popup, .settings-pop'
    // 名单之外再问一句 DOM：右栏矩形**中点**上最顶层的元素是不是还在右栏里（判据与测试
    // 见 lib/overlayHit.ts）。原生视图不参与 DOM 命中测试，所以「中点被别的元素占住」
    // 只可能是 DOM 覆盖层压在它上面 —— 包括以后新加、名字还没进名单的那些。
    // 用户 2026-10 报的正是漏掉的那一类：先开右栏的 HTML 预览、再点开聊天里的大图，
    // 大图（`.rt-lb`，fixed inset:0）整个躲在原生预览画面后面。
    const compute = () => {
      const hostRect = shotRef.current?.getBoundingClientRect()
      const host = shotRef.current
      const pt = centerOf(hostRect ?? undefined, { width: window.innerWidth, height: window.innerHeight })
      let hit = !!host && overlayCoversRect(
        hostRect ?? undefined,
        pt ? document.elementsFromPoint(pt.x, pt.y) : [],
        { contains: (el) => host.contains(el as Node), positionOf: (el) => getComputedStyle(el as Element).position, viewport: { width: window.innerWidth, height: window.innerHeight } },
      )
      for (const el of document.querySelectorAll(SEL)) {
        const r = el.getBoundingClientRect()
        if (r.width < 1 || r.height < 1) continue
        if (rectsIntersect(hostRect ?? undefined, r)) { hit = true; break }
      }
      // 同值不 set（MutationObserver 每次 class 变动都会走到这里，避免无谓重渲染）
      setDomOverlayOpen((prev) => (prev === hit ? prev : hit))
    }
    compute()
    // 节流 120ms（**不是防抖**）：聊天流式输出时 DOM 每秒变化几十次，防抖会让定时器永远被重置、
    // 检测永不触发；节流保证「一定会跑」，同时把最坏开销从 60 次/秒压到 ~8 次/秒。
    // 120ms 的延迟不可感知（弹窗自身入场动画 180ms）。无覆盖层时循环体不执行 → 不做布局读取。
    let timer = 0
    const mo = new MutationObserver(() => {
      if (!timer) timer = window.setTimeout(() => { timer = 0; compute() }, 120)
    })
    mo.observe(document.body, { subtree: true, attributes: true, attributeFilter: ['class', 'style'], childList: true })
    return () => { mo.disconnect(); if (timer) window.clearTimeout(timer) }
  }, [nativeViewMode])

  // 内嵌视图可见性：随页面数切换——有页面（正在浏览）显示，空闲隐藏（避免空白 about:blank 白框）；
  // 全屏弹窗（设置/提问等）与**面板级下拉菜单**打开期间强制隐藏：原生视图恒合成在窗口 DOM 之上
  // （原生层级 > DOM，CSS z-index 压不住），不隐藏就会盖住菜单（用户实测：点「＋」后菜单被预览画面挡掉）。
  useEffect(() => {
    if (!nativeViewMode || embedError) return
    const el = shotRef.current
    const rect = embedRectOf(el)
    // 量不到 → 只有两条合法归宿：有覆盖层/无页面时本就该 hide；否则**不下发 show**
    //（把视图交给上一次的 bounds，绝不拿假的 80×80 去缩视图 —— 见 embedRectOf）。
    if (uiModalOpen || menuOpen || domOverlayOpen || !hasPages || !rect) void window.desktop?.embedHide()
    else void window.desktop?.embedShow({ rect })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nativeViewMode, hasPages, uiModalOpen, menuOpen, domOverlayOpen])

  // 预览 vs 真网页：预览（file:// / data:）用精简工具条（见下），真网页保留完整导航。
  const isPreviewUrl = /^(file|data):/.test(nav?.url ?? '')
  // —— M6：地址栏跟随激活视图 URL（store 层已全局订阅 embed-nav；用户输入中不打断）——
  const navUrl = nav?.url ?? ''
  useEffect(() => {
    if (document.activeElement !== urlRef.current) setUrlInput(navUrl)
  }, [navUrl])

  // 插件启用（或初始已启用）→ 拉页列表
  useEffect(() => {
    if (enabled && browserPages.length === 0 && !browserBusy) fetchBrowserPages()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabled])
  // 目标页就绪 → 自动截它（仅截图模式：window 呈现或实时视图回退态）
  useEffect(() => {
    if (!embedLive && enabled && targetPage != null && !browserBusy) fetchBrowserShot(targetPage)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [embedLive, enabled, targetPage])

  // M5：地址栏回车 → 直接驱动内嵌视图（不经 AI/插件，用户自由导航）
  const go = () => {
    const u = urlInput.trim()
    if (u) void window.desktop?.embedNavigate(u)
  }

  return (
    <div className="file-preview">
      {/* 沉浸态出口：收起工具条后**必须**还留一个 ☰（否则再也调不回来）。
          预览（file://）整层都不要 —— 面板级标签上已经写着同一个文件名，再来一行是重复
          （用户口径：「红线这一层完全没必要了」）；预览的刷新走标签右键菜单。
          面板级标签条在 .right-body 之外、永远可见，所以这里收起的只是浏览器自己的 chrome。 */}
      <div className="fp-hd" style={!isPreviewUrl && toolbarHidden ? undefined : { display: 'none' }}>
        <button className="bt-nav" onClick={() => onToggleTabs?.()} aria-label={t('rp.browser.showTabs')} title={t('rp.browser.showTabs')}>☰</button>
        <span className={`mcp-dot ${enabled ? 'on' : ''}`} title={browserStateText} />
      </div>
      {/* 占位只在「既没有原生视图、又没有工具面」时出现 —— 否则面板总该有东西可看：
          内嵌视图（nativeViewMode）或 MCP 页列表/截图（enabled）。此前条件是 `!enabled && !egoMode`，
          于是「插件没开」会把**能用的**内嵌预览也一起挡掉（两次报障的根因）。 */}
      {!nativeViewMode && !enabled ? (
        <div className="empty">
          <div className="big">{browserNotInstalled ? t('browser.state.notInstalled') : browserInstalledNotEnabled ? t('browser.state.ready') : browserConnecting ? t('browser.state.connecting') : browserError ? (browser.reason || t('browser.state.error')) : t('browser.notEnabled')}</div>
          {browserInstalledNotEnabled ? t('browser.enableHint') : browserNotInstalled ? t('browser.installHint') : t('browser.enableHint')}
        </div>
      ) : (
        <>
          {/* 浏览器标签的工具条（阶段 1.5：与 .fp-hd 合并成一行）。**只给真网页**：
                · 预览（file:// / data:）→ 整层不渲染：文件名在面板标签上、内容就是页面本身，
                  再来一行（哪怕是「图标+文件名+刷新」）都是重复（用户两次口径一致：
                  「如果是浏览器Tab，是不是就不需要这个头部了」「红线这一层完全没必要了」）。
                  预览的刷新收进标签右键菜单。
                · 真网页（http/https，或还没开页）→ 保留完整导航（用户可自主输入地址）。 */}
          {nativeViewMode && !toolbarHidden && !isPreviewUrl && (
            <div className="browser-toolbar">
              <button className="bt-nav" onClick={() => onToggleTabs?.()} aria-label={t('rp.browser.hideTabs')} title={t('rp.browser.hideTabs')}>▴</button>
              <button className="bt-nav" disabled={!nav?.canGoBack} onClick={() => void window.desktop?.embedBack()} aria-label={t('browser.nav.back')} title={t('browser.nav.back')}>←</button>
              <button className="bt-nav" disabled={!nav?.canGoForward} onClick={() => void window.desktop?.embedForward()} aria-label={t('browser.nav.forward')} title={t('browser.nav.forward')}>→</button>
              <button className="bt-nav" onClick={() => void window.desktop?.embedReload()} aria-label={t('browser.nav.reload')} title={t('browser.nav.reload')}>⟳</button>
              <input
                ref={urlRef}
                className="bt-url mono"
                value={urlInput}
                placeholder={t('browser.nav.ph')}
                spellCheck={false}
                onChange={(e) => setUrlInput(e.target.value)}
                onKeyDown={(e) => {
                  // 输入法组合态（中文等用 Enter 选字/上屏）不得触发跳转
                  if (e.nativeEvent.isComposing || e.keyCode === 229) return
                  if (e.key === 'Enter') go()
                }}
              />
              <span className={`btab-ai ${aiActive ? 'on' : ''}`} title={t('browser.ai.controlled')}>{t('browser.ai.badge')}</span>
            </div>
          )}
          {embeddedMode && aiActive && lastAction && <div className="btab-action mono">{lastAction}</div>}
          {/* M5：建连失败可见化（此前静默回退截图，用户只看到“静态页”） */}
          {embedError && (
            <div className="btab-fallback">
              ⚠ {t('browser.embed.fallback')}（{embedError}）
              <button onClick={() => { setEmbedError(''); setRetryNonce((n) => n + 1) }}>{t('browser.embed.retry')}</button>
            </div>
          )}
          <div className="browser-shot" ref={shotRef}>
            {embedLive ? (
              // M5：实时视图 = 原生 WebContentsView 本体（覆盖本容器），DOM 只留占位/空态。
              // 有页面时视图可见盖住容器；空闲时视图隐藏，露出引导文案。
              hasPages ? null : (
                <div className="empty">
                  <div className="big">{t('browser.idle.title')}</div>
                  {t('browser.idle.hint')}
                </div>
              )
            ) : browserBusy ? (
              <div className="empty">{t('browser.loading')}</div>
            ) : browserShot ? (
              <img src={browserShot} alt={t('rp.browser.screenshotAlt')} />
            ) : (
              <div className="empty">
                <div className="big">{t('browser.noShot')}</div>
                {t('browser.shotHint')}
              </div>
            )}
          </div>
        </>
      )}
    </div>
  )
}

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
  const [menu, setMenu] = useState<'add' | 'more' | null>(null)
  // 标签右键菜单（R4 的批量关闭 + 文件动作；内层标签条退役后搬到这里）。
  const [ctx, setCtx] = useState<{ x: number; y: number; id: string } | null>(null)
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
  // 预览没有工具条（见 BrowserTab 的注释）→ 刷新收进右键菜单；用 embedReloadTarget 只刷**这个**
  // 标签的页面（不是激活视图：右键的标签可能不是当前激活的那个）。
  const ctxRefresh = () => {
    if (ctxTab?.kind === 'web') {
      if (ctxTab.viewId) void window.desktop?.embedReloadTarget?.(ctxTab.viewId)
      else if (ctxTab.pageId != null) fetchBrowserShot(ctxTab.pageId)
    }
    setCtx(null)
  }
  const ctxCloseOthers = () => { for (const x of tabs) if (x.id !== ctx?.id) closeTab(x.id); setCtx(null) }
  const ctxCloseRight = () => { for (const x of tabs.slice(ctxIndex + 1)) closeTab(x.id); setCtx(null) }
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
  }, [ctx])
  // 可见性仲裁的口径与 BrowserTab 主体一致：**只看呈现方式**，不看插件/引擎状态 ——
  // 「打开文件/网页」不依赖浏览器插件（见 §8.7），把这道兜底闸门挂在插件上会让它在
  // **默认配置**（插件未启用）下整体失效，而默认配置恰恰是面板现在会挂载的情形。
  // 历史：这里曾写 `!embeddedPresentation && !egoEngine` + `!egoEngine && !browserEnabled`
  // —— 同样的「能力耦合工具面」模式，且 embeddedPresentation 缺「无配置→默认内嵌」的 fallback。
  const embeddedPresentation = browserConfig?.presentation === 'embedded' || !browserConfig

  // 全局可见性仲裁（本组件常驻挂载，折叠态也渲染）：面板未展开或不在 web tab →
  // 原生视图必须隐藏。可见性判定权收归渲染层——此前 embed.ts 创建视图时强制显示，
  // 后台会话触发 Browser Use 时白屏浮在无关界面上方。web tab 内的细粒度控制
  // （hasPages/全屏弹窗）由 BrowserTab 的 effect 接管。
  // 只 hide 不 show：show 由 BrowserTab 的可见性 effect（hasPages/弹窗）负责——
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
  // M6：网页模式的沉浸态——收起**浏览器自己的工具条**（.fp-hd）把垂直空间让给内嵌页面；
  // ☰ 一键唤回。注意：**面板级标签条不再随之隐藏**（阶段 1 起它就是唯一的标签入口，
  // 隐藏它等于切不走标签）。
  // 默认**非沉浸**：阶段 1 之前 web tab 默认隐藏面板 tab 行（M6 的「让垂直空间给页面」），
  // 现在面板标签条常驻、浏览器工具条又被合并成一行 → 默认收起它只会让用户看不到地址栏/文件名。
  const [immersive, setImmersive] = useState(false)
  const toolbarHidden = activeTab?.kind === 'web' && immersive
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
  const webTabUrl = (tab: TabInstance): string =>
    tab.kind === 'web' ? (embedNav?.targets?.find((x) => x.id === tab.viewId)?.url ?? tab.url ?? '') : ''
  const tabIconEl = (tab: TabInstance): ReactElement =>
    tab.kind === 'web' ? tabIcon(webTabUrl(tab)) : kindIcon(tab.kind)
  const tabTitle = (tab: TabInstance): string => {
    if (tab.kind === 'file') {
      const base = (tab.path ?? '').split('/').filter(Boolean).pop()
      // 长文件名中间截断（保留 `_test.go` 这类尾巴）：标签最宽 168px，从尾巴截断时
      // `conversation_router.go` 与 `conversation_router_test.go` 会长得一模一样。
      // 完整路径在 title 里（见渲染点的 title 属性）。
      return (base && shortName(base)) || kindLabel('file')
    }
    if (tab.kind === 'web') {
      const tp = embedNav?.targets?.find((x) => x.id === tab.viewId)
      return tabLabel(tp?.url ?? tab.url ?? '', tp?.title ?? '') || kindLabel('web')
    }
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
          {activeTab?.kind === 'web' && <BrowserTab toolbarHidden={toolbarHidden} onToggleTabs={() => setImmersive((v) => !v)} menuOpen={!!menu || !!ctx} panelVisible={rightExpanded} activePageId={activeTab?.kind === 'web' ? activeTab.pageId : undefined} />}
        </div>
      </div>
    </aside>
  )
}
