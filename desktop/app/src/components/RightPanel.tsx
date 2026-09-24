import { useEffect, useMemo, useRef, useState, useCallback } from 'react'
import type { ReactElement } from 'react'
import { useAppStore, runningTaskCount, asyncTaskBlocks, fileKeyOf, isPendingWebTab, rebuildWebTab } from '../store/useAppStore'
import type { RightTab, TabKind, TabInstance, TabSpec, FileTabInstance, MsgBlock, Diff, GitFile, GitStatus } from '../store/useAppStore'
import type { AnyEvent } from '../store/events'
import { projectAllTraces, liveRootRunId } from '../store/trace'
import type { TraceDetail } from '../store/trace'
import TraceWaterfall, { buildRows } from './TraceWaterfall'
import SpanSheet, { lookupPayload } from './SpanSheet'
import { Activity, GitBranch, FileIcon, Bot, BarChart, CheckList, Globe, PanelRight, ImageIcon, ChevronDown, Plus } from './icons'
import { fmtTokens, fmtUsd, fmtDuration, UnifiedDiff } from './ui'
import ApprovalCard from './ApprovalCard'
import ConfirmModal from './ConfirmModal'
import { CompressionBlock } from './NarrativeBlocks'
// 任务卡（子 agent / 后台任务 / 工具任务）：与主对话的子 agent 卡**同一份外壳**，见 TaskCard.tsx
import { AgentTaskCard, AsyncTaskCard, ToolTaskCard } from './TaskCard'
import { Markdown } from './markdown'
import CsvPreview from './CsvPreview'
import { codeToHtml } from './highlight'
import { useResolvedTheme } from '../lib/useTheme'
import { useAppearance } from '../lib/appearance'
import { isCsvPath } from '../lib/csv'
import { baseDirFor } from '../lib/localAsset'
import { visualKindOf } from '../lib/visualFile'
import { copyText } from '../lib/clipboard'
import { useT } from '../i18n'
import TodoPanel from './TodoPanel'

// Tab 分两段：主段 = 会话工作流核心（链路 · 变更）；辅段 = 监视/工具（Agent · 文件 · 网页）。
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
    { key: 'agent', label: t('rp.tab.agent'), icon: <Bot /> },
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

function ratio(n: number, total: number): number {
  return total > 0 ? Math.max(3, (n / total) * 100) : 0
}

/** 体积简写（列表右列：trace 只存元数据的体积，一眼可见存储账）。 */
function fmtBytesShort(n: number): string {
  if (!n) return '—'
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(0)} KB`
  return `${(n / 1024 / 1024).toFixed(1)} MB`
}
// GitStatus 空值（工作区未探测/非仓库时的兜底展示）
const EMPTY_GIT: GitStatus = { is_repo: false, ahead: 0, behind: 0, staged: 0, modified: 0, untracked: 0, conflicted: 0, clean: true }
const GIT_STALE_MS = 30_000 // 变更 tab 打开时工作区级快照的 stale 阈值
const GIT_POLL_MS = 10_000 // 变更 tab 打开时的快照轮询周期（确认是仓库后才启动）
const GIT_GRAPH_COLORS = ['var(--running)', 'var(--success)', 'var(--warning)', 'var(--danger)', 'var(--text-2)']

// git log --graph 图示前缀按列着色（泳道感）；空格原样占位
function GitGraph({ g }: { g?: string }) {
  if (!g) return null
  return (
    <span className="git-graph mono">
      {g.split('').map((ch, i) => (ch === ' '
        ? ' '
        : <span key={i} style={{ color: GIT_GRAPH_COLORS[i % GIT_GRAPH_COLORS.length] }} className={ch === '*' ? 'dot' : undefined}>{ch}</span>
      ))}
    </span>
  )
}

// iso-strict 时间 → 「YYYY-MM-DD HH:mm」（当年省略年份）
function fmtGitDate(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const pad = (n: number) => String(n).padStart(2, '0')
  const ymd = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
  const hm = `${pad(d.getHours())}:${pad(d.getMinutes())}`
  return d.getFullYear() === new Date().getFullYear() ? `${ymd.slice(5)} ${hm}` : `${ymd} ${hm}`
}

// 变更 tab：Git 面板（设计见 docs/DESKTOP_GIT.md）。
// 双层数据源：有活跃会话读会话级快照（随工具调用自动刷新）；
// 无会话读工作区级探测结果（loadWsGit/loadWsGitHistory，添加工作区即拉取）。
function ChangesTab() {
  const t = useT()
  const hasSession = useAppStore((s) => !!s.activeSessionId)
  const git = useAppStore((s) => (hasSession ? s.git : s.wsGit ?? EMPTY_GIT))
  const gitFiles = useAppStore((s) => (hasSession ? s.gitFiles : s.wsGitFiles))
  const loading = useAppStore((s) => (hasSession ? s.gitLoading : s.wsGitLoading))
  const error = useAppStore((s) => (hasSession ? s.gitError : s.wsGitError))
  const commits = useAppStore((s) => (hasSession ? s.gitHistory : s.wsGitHistory))
  const hasMore = useAppStore((s) => (hasSession ? s.gitHasMore === true : s.wsGitHasMore === true))
  const histLoading = useAppStore((s) => (hasSession ? s.gitHistLoading === true : s.wsGitHistLoading === true))
  const fileDiffs = useAppStore((s) => s.wsFileDiffs)
  const refresh = useAppStore((s) => s.refreshGit)
  const loadHistory = useAppStore((s) => s.loadGitHistory)
  const loadCommitDiff = useAppStore((s) => s.loadCommitDiff)
  const loadGitDiff = useAppStore((s) => s.loadGitDiff)
  const loadWsGit = useAppStore((s) => s.loadWsGit)
  const loadWsGitHistory = useAppStore((s) => s.loadWsGitHistory)
  const loadWsFileDiff = useAppStore((s) => s.loadWsFileDiff)
  const loadWsCommitDiff = useAppStore((s) => s.loadWsCommitDiff)
  // —— 写操作（P1）——
  const gitOpBusy = useAppStore((s) => s.gitOpBusy)
  const gitOpError = useAppStore((s) => s.gitOpError)
  const gitStage = useAppStore((s) => s.gitStage)
  const gitUnstage = useAppStore((s) => s.gitUnstage)
  const gitDiscard = useAppStore((s) => s.gitDiscard)
  const gitCommitOp = useAppStore((s) => s.gitCommit)
  const clearGitOpError = useAppStore((s) => s.clearGitOpError)
  const openFilePreview = useAppStore((s) => s.openFile)
  // —— 分支与仓库初始化（P2）——
  const gitBranches = useAppStore((s) => s.gitBranches)
  const loadGitBranches = useAppStore((s) => s.loadGitBranches)
  const gitCheckout = useAppStore((s) => s.gitCheckout)
  const gitInitRepo = useAppStore((s) => s.gitInitRepo)
  const [openPath, setOpenPath] = useState<string | null>(null) // 展开的文件（staged 前缀 s: 区分同路径双态）
  const [openCommit, setOpenCommit] = useState<string | null>(null)
  const [confirmDiscard, setConfirmDiscard] = useState<GitFile | null>(null) // 丢弃/删除确认（破坏性）
  const [commitMsg, setCommitMsg] = useState('')
  const [stageAll, setStageAll] = useState(false) // 提交时是否包含未暂存变更（git add -A）
  const [filter, setFilter] = useState('') // 变更文件过滤（大仓库快速定位）
  const [branchOpen, setBranchOpen] = useState(false) // 分支切换弹层（点开才拉清单；不占常驻空间）
  const [secOpen, setSecOpen] = useState({ files: true, history: true }) // 分区折叠（空间有限时按需收起）

  // 挂载/stale 重拉：工作区级快照缺失或超过 30s → 重拉 snapshot + history
  useEffect(() => {
    if (hasSession) { refresh(); loadHistory(); return }
    const st = useAppStore.getState()
    if (st.wsGitAt == null || Date.now() - st.wsGitAt > GIT_STALE_MS) { loadWsGit(); loadWsGitHistory() }
  }, [hasSession, refresh, loadHistory, loadWsGit, loadWsGitHistory])

  // 定时刷新（仅确认是仓库后启动；no-git / 未识别时不轮询）：
  // 10s 快照轮询；切 session / 切 workspace 由既有触发链（setActiveSession / setActiveWorkspace）覆盖，
  // tick 内经 getState() 取当前数据源，切换后的下一次 tick 自然路由到新目标。
  useEffect(() => {
    if (!git.is_repo) return
    const timer = setInterval(() => {
      if (document.hidden) return // 窗口不可见时跳过，省电省 git 调用
      const st = useAppStore.getState()
      if (st.activeSessionId) st.refreshGit()
      else st.loadWsGit()
    }, GIT_POLL_MS)
    return () => clearInterval(timer)
  }, [git.is_repo])

  // HEAD 变化（新提交/切换分支/checkout）→ 重拉提交历史；unborn HEAD（head 为空）不触发
  const lastHeadRef = useRef<string | undefined>(undefined)
  useEffect(() => {
    const head = git.head
    if (head && lastHeadRef.current != null && lastHeadRef.current !== head) {
      if (hasSession) loadHistory()
      else loadWsGitHistory()
    }
    if (head) lastHeadRef.current = head
  }, [git.head, hasSession, loadHistory, loadWsGitHistory])

  // 窗口重新可见/聚焦 → 立即刷新快照（覆盖「切到终端操作 git 后切回」场景；
  // 与 10s 轮询互补，P2 记录了不引入 fs watcher 的取舍：.git watch 感知不到工作区编辑）
  useEffect(() => {
    if (!git.is_repo) return
    const refreshNow = () => {
      if (document.hidden) return
      const st = useAppStore.getState()
      if (st.activeSessionId) st.refreshGit()
      else st.loadWsGit()
    }
    window.addEventListener('focus', refreshNow)
    document.addEventListener('visibilitychange', refreshNow)
    return () => {
      window.removeEventListener('focus', refreshNow)
      document.removeEventListener('visibilitychange', refreshNow)
    }
  }, [git.is_repo])

  const refreshAll = () => { if (hasSession) { refresh(); loadHistory() } else { loadWsGit(); loadWsGitHistory() } }

  // 提交成功（busy 从 git_commit 归零且无错误）→ 清空输入框；失败保留供修改重试
  const prevBusyRef = useRef<string | undefined>(undefined)
  useEffect(() => {
    if (prevBusyRef.current === 'git_commit' && !gitOpBusy && !gitOpError) setCommitMsg('')
    prevBusyRef.current = gitOpBusy
  }, [gitOpBusy, gitOpError])

  const doCommit = () => {
    const msg = commitMsg.trim()
    if (!msg || gitOpBusy) return
    if (!stagedFiles.length && !stageAll) return // 没有可提交内容
    gitCommitOp(msg, stageAll)
  }

  if (loading) return <div className="empty"><div className="big">{t('rp.git.loading')}</div></div>
  if (error) {
    return (
      <div className="empty">
        <div className="big">{t('rp.git.loadFailed')}</div>
        <div className="hint">{error}</div>
        <button className="git-more" onClick={refreshAll}>{t('rp.git.retry')}</button>
      </div>
    )
  }
  if (!git.is_repo) {
    return (
      <div className="empty">
        <div className="big">{t('rp.git.notRepo')}</div>
        <div className="hint">{t('rp.git.notRepoHint')}</div>
        <button className="git-more" disabled={!!gitOpBusy} onClick={() => gitInitRepo()}>
          {gitOpBusy === 'git_init' ? t('rp.git.initBusy') : t('rp.git.init')}
        </button>
      </div>
    )
  }

  const branchLabel = git.branch || (git.head ? `detached@${git.head}` : 'HEAD')

  const stagedFiles = gitFiles.filter((f) => f.staged)
  const conflictFiles = gitFiles.filter((f) => f.status === 'conflicted')
  const untrackedFiles = gitFiles.filter((f) => !f.staged && f.status === 'untracked')
  const workFiles = gitFiles.filter((f) => !f.staged && f.status !== 'conflicted' && f.status !== 'untracked')
  const restageable = [...workFiles, ...untrackedFiles]
  const ft = filter.trim().toLowerCase()
  const fileKey = (f: GitFile) => (f.staged ? `s:${f.path}` : f.path)
  const fileDiff = (f: GitFile) => (hasSession ? gitFiles.find((x) => x.path === f.path)?.diff : fileDiffs[f.path])

  const toggleFile = (f: GitFile) => {
    if (f.status === 'conflicted') {
      // 冲突文件：跳转文件预览直接查看冲突标记（<<<<<<< / ======= / >>>>>>>）
      openFilePreview(f.path)
      return
    }
    const key = fileKey(f)
    if (openPath === key) { setOpenPath(null); return }
    setOpenPath(key)
    if (hasSession) loadGitDiff(f.path, f.staged)
    else loadWsFileDiff(f.path, f.staged)
  }
  const toggleCommit = (hash: string) => {
    if (openCommit === hash) { setOpenCommit(null); return }
    setOpenCommit(hash)
    const c = commits.find((x) => x.hash === hash)
    if (!c?.diff) { if (hasSession) loadCommitDiff(hash); else loadWsCommitDiff(hash) }
  }

  const renderGroup = (title: string, list: GitFile[]) => {
    const shown = ft ? list.filter((f) => f.path.toLowerCase().includes(ft)) : list
    if (shown.length === 0) return null
    return (
      <>
        <div className="git-sub-title">{title} · {shown.length}</div>
        {shown.map((f) => {
          const key = fileKey(f)
          const open = openPath === key
          const diff = open ? fileDiff(f) : undefined
          const isUntracked = f.status === 'untracked'
          return (
            <div key={key} className="git-file-wrap">
              <div
                className={`git-file${open ? ' open' : ''}${f.status === 'conflicted' ? ' conflict' : ''}`}
                onClick={() => toggleFile(f)}
                title={f.status === 'conflicted' ? t('rp.git.conflicted') : f.path}
              >
                <span className={`git-file-status st-${f.status || 'modified'}${f.staged ? ' staged' : ''}`}>{fileStatusLabel((f.status || 'modified') as FileActivityStatus)}</span>
                <span className="git-file-path">{f.path}</span>
                <span className="git-file-stats mono">{f.additions > 0 ? `+${f.additions}` : ''}{f.deletions > 0 ? ` −${f.deletions}` : ''}{!f.additions && !f.deletions ? '·' : ''}</span>
                <span className="git-file-acts">
                  {f.staged && (
                    <button className="git-act" title={t('rp.git.unstage')} disabled={!!gitOpBusy} onClick={(e) => { e.stopPropagation(); gitUnstage([f.path]) }}>−</button>
                  )}
                  {!f.staged && f.status !== 'conflicted' && (
                    <button className="git-act" title={t('rp.git.stage')} disabled={!!gitOpBusy} onClick={(e) => { e.stopPropagation(); gitStage([f.path]) }}>+</button>
                  )}
                  {!f.staged && f.status !== 'conflicted' && (
                    <button className="git-act danger" title={isUntracked ? t('rp.git.deleteUntracked') : t('rp.git.discard')} disabled={!!gitOpBusy} onClick={(e) => { e.stopPropagation(); setConfirmDiscard(f) }}>↩</button>
                  )}
                  <button className="git-act" title={t('rp.git.openPreview')} onClick={(e) => { e.stopPropagation(); openFilePreview(f.path) }}>⤢</button>
                </span>
              </div>
              {open && <div className="git-diff">{diff ? <UnifiedDiff unified={diff} /> : <div className="git-diff-loading">{t('rp.git.loadingDiff')}</div>}</div>}
            </div>
          )
        })}
      </>
    )
  }

  return (
    <div className="git-panel">
      <div className="git-header">
        <GitBranch />
        <button
          className="git-branch-btn"
          title={t('rp.git.switchBranch')}
          onClick={() => { setBranchOpen((v) => { if (!v) loadGitBranches(); return !v }) }}
        >
          <strong className="git-branch-name">{branchLabel}</strong>
          <span className="git-branch-caret">▾</span>
        </button>
        {(git.ahead > 0 || git.behind > 0) && (
          <span className="git-ab mono">{git.ahead > 0 ? `↑${git.ahead}` : ''}{git.behind > 0 ? ` ↓${git.behind}` : ''}</span>
        )}
        <span className="grow" />
        <button className="git-refresh" onClick={refreshAll} title={t('rp.git.refresh')}>↻</button>
      </div>
      <div className="git-summary mono">{git.root || ''}</div>

      {branchOpen && (
        <>
          <div className="git-pop-backdrop" onClick={() => setBranchOpen(false)} />
          <div className="git-branch-pop">
            <div className="git-branch-pop-title">{t('rp.git.localBranches')}</div>
            {(gitBranches ?? []).length === 0 && <div className="git-empty">{gitOpBusy === 'git_branch_list' ? t('rp.git.loadingBranches') : t('rp.git.noBranches')}</div>}
            {(gitBranches ?? []).map((b) => (
              <button
                key={b.name}
                className={`git-branch-item${b.current ? ' current' : ''}`}
                disabled={b.current || !!gitOpBusy}
                onClick={() => { setBranchOpen(false); gitCheckout(b.name) }}
              >
                <span className="git-branch-item-name">{b.name}</span>
                {b.current && <span className="git-branch-item-check">✓</span>}
              </button>
            ))}
          </div>
        </>
      )}

      {gitOpError && (
        <div className="git-op-error">
          <span>{gitOpError}</span>
          <button className="git-op-error-x" onClick={clearGitOpError} title={t('rp.git.close')}>✕</button>
        </div>
      )}

      <div className="git-section">
        <div
          className="git-section-title as-toggle"
          role="button"
          aria-expanded={secOpen.files}
          onClick={() => setSecOpen((v) => ({ ...v, files: !v.files }))}
        >
          <span className={`git-sec-caret${secOpen.files ? ' open' : ''}`}>▾</span>
          <span>{t('rp.git.changedFiles').replace('{n}', String(gitFiles.length))}</span>
          <span className="grow" />
          {secOpen.files && restageable.length > 0 && (
            <button
              className="git-title-act"
              disabled={!!gitOpBusy}
              onClick={(e) => { e.stopPropagation(); gitStage(restageable.map((f) => f.path)) }}
            >
              {t('rp.git.stageAll')}
            </button>
          )}
        </div>
        {secOpen.files && (
          <>
            {gitFiles.length === 0 && <div className="git-empty">{t('rp.git.clean')}</div>}
            {gitFiles.length >= 20 && (
              <input className="git-filter" placeholder={t('rp.git.filterPh')} value={filter} onChange={(e) => setFilter(e.target.value)} spellCheck={false} />
            )}
            {renderGroup(t('rp.git.staged'), stagedFiles)}
            {renderGroup(t('rp.git.unstaged'), workFiles)}
            {renderGroup(t('rp.git.untracked'), untrackedFiles)}
            {renderGroup(t('rp.git.conflicts'), conflictFiles)}
          </>
        )}
      </div>

      {(stagedFiles.length > 0 || stageAll) && secOpen.files && (
        <div className="git-commit-box">
          <textarea
            className="git-commit-input"
            placeholder={t('rp.git.commitPh')}
            value={commitMsg}
            onChange={(e) => setCommitMsg(e.target.value)}
            onKeyDown={(e) => { if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') doCommit() }}
            rows={2}
            spellCheck={false}
          />
          <div className="git-commit-acts">
            <label className="git-stage-all-toggle">
              <input type="checkbox" checked={stageAll} onChange={(e) => setStageAll(e.target.checked)} />
              {t('rp.git.includeUnstaged').replace('{n}', String(gitFiles.length - stagedFiles.length))}
            </label>
            <span className="grow" />
            <button
              className="btn primary git-commit-btn"
              disabled={(!stagedFiles.length && !stageAll) || !!gitOpBusy || !commitMsg.trim()}
              onClick={doCommit}
            >
              {gitOpBusy === 'git_commit' ? t('rp.git.commitBusy') : t('rp.git.commit')}
            </button>
          </div>
        </div>
      )}

      <div className="git-section">
        <div
          className="git-section-title as-toggle"
          role="button"
          aria-expanded={secOpen.history}
          onClick={() => setSecOpen((v) => ({ ...v, history: !v.history }))}
        >
          <span className={`git-sec-caret${secOpen.history ? ' open' : ''}`}>▾</span>
          <span>{t('rp.git.history').replace('{n}', String(commits.length))}</span>
        </div>
        {secOpen.history && (
          <>
            {commits.length === 0 && <div className="git-empty">{t('rp.git.noCommits')}</div>}
            {commits.map((c) => (
              <div key={c.hash} className="git-commit-wrap">
                <div className={`git-commit${openCommit === c.hash ? ' open' : ''}`} onClick={() => toggleCommit(c.hash)}>
                  <div className="git-commit-line"><GitGraph g={c.graph} /><span className="mono git-commit-hash">{c.short_hash}</span><span className="git-commit-subject">{c.subject}</span></div>
                  {c.graph_after && <div className="git-graph-after mono">{c.graph_after}</div>}
                  <div className="git-commit-meta">{c.author} · {fmtGitDate(c.date)}</div>
                </div>
                {openCommit === c.hash && (
                  <div className="git-diff">{c.diff ? <UnifiedDiff unified={c.diff} /> : <div className="git-diff-loading">{t('rp.git.loadingCommitDiff')}</div>}</div>
                )}
              </div>
            ))}
            {hasMore && (
              <button className="git-more" disabled={histLoading} onClick={() => (hasSession ? loadHistory(true) : loadWsGitHistory(true))}>
                {histLoading ? t('rp.git.loadingMore') : t('rp.git.loadMore')}
              </button>
            )}
          </>
        )}
      </div>

      {confirmDiscard && (
        <ConfirmModal
          title={confirmDiscard.status === 'untracked' ? t('rp.git.deleteUntracked') : t('rp.git.discardTitle')}
          message={
            confirmDiscard.status === 'untracked'
              ? t('rp.git.discardMsgUntracked').replace('{path}', confirmDiscard.path)
              : t('rp.git.discardMsg').replace('{path}', confirmDiscard.path)
          }
          confirmLabel={confirmDiscard.status === 'untracked' ? t('rp.git.delete') : t('rp.git.discard2')}
          cancelLabel={t('rp.git.cancel')}
          danger
          onConfirm={() => {
            gitDiscard([confirmDiscard.path], confirmDiscard.status === 'untracked')
            if (openPath === fileKey(confirmDiscard)) setOpenPath(null)
            setConfirmDiscard(null)
          }}
          onCancel={() => setConfirmDiscard(null)}
        />
      )}
    </div>
  )
}


// 文件扩展名 → shiki 语言（未知回退 text）
function langFromPath(p: string): string {
  const ext = (p.split('.').pop() || '').toLowerCase()
  const map: Record<string, string> = {
    go: 'go', ts: 'typescript', tsx: 'tsx', js: 'javascript', jsx: 'jsx',
    json: 'json', yaml: 'yaml', yml: 'yaml', sql: 'sql', py: 'python',
    rs: 'rust', html: 'html', htm: 'html', css: 'css', md: 'markdown',
    vue: 'vue', // .vue 单文件组件（template + script + style 三块高亮）
  }
  return map[ext] ?? 'text'
}

// Markdown 文件判定（「最终文件」视图据此提供预览/纯文本双模式）：
// .md / .markdown / .mdx（MDX 也走同一套渲染 —— 它的 JSX 部分会按 HTML 处理，
// 比纯源码视图更接近作者意图；真要看源码切「纯文本」即可）。
function isMarkdownPath(p: string): boolean {
  const ext = (p.split('.').pop() || '').toLowerCase()
  return ext === 'md' || ext === 'markdown' || ext === 'mdx'
}

type FileFilter = 'all' | 'changed' | 'read'
type FileActivityStatus = 'added' | 'modified' | 'deleted' | 'conflicted' | 'untracked' | 'read'

type FileOperation = {
  toolId: string
  tool: string
  result: string
  diff?: Diff
  time?: number
  durMs?: number
  isError?: boolean
}

type FileActivity = {
  path: string
  status: FileActivityStatus
  additions: number
  deletions: number
  changed: boolean
  lastAt: number
  operations: FileOperation[]
  git?: GitFile
}

function normalizeFilePath(path: string): string {
  return path.replaceAll('\\', '/').replace(/^\.\//, '')
}

// sameFilePath 路径同一性比较（容忍「绝对 vs 工作区相对」两种写法）。
//
// 背景（2026-09-15 实测缺陷）：文件活动列表（aggregateFileActivity）的 path 来自工具
// args、是工作区相对路径；而产出卡片（Artifacts）等外部入口可能给出工作区绝对路径。
// 直接用 `===` 比较会让同一文件被判为不同 → 右侧详情永远停在「正在读取文件…」。
// 归一化：去掉 ./、统一分隔符、绝对路径在工作区内时折叠为相对，再比较。
function sameFilePath(a: string | undefined, b: string | undefined, ws: string | undefined): boolean {
  if (!a || !b) return false
  const na = toRelIfInside(a, ws)
  const nb = toRelIfInside(b, ws)
  return na === nb
}

// toRelIfInside 工作区内的绝对路径 → 相对；其余原样（仅归一化分隔符与 ./ 前缀）。
function toRelIfInside(p: string, ws: string | undefined): string {
  const n = normalizeFilePath(p)
  if (!ws || !n.startsWith('/')) return n
  const w = normalizeFilePath(ws).replace(/\/+$/, '')
  if (n === w) return ''
  if (n.startsWith(w + '/')) return n.slice(w.length + 1)
  return n // 工作区外：保持绝对（与 activities 中工作区外条目的写法一致）
}

function fileStatusLabel(status: FileActivityStatus): string {
  if (status === 'added' || status === 'untracked') return 'A'
  if (status === 'deleted') return 'D'
  if (status === 'conflicted') return '!'
  if (status === 'modified') return 'M'
  return '○'
}

function formatToolTimestamp(timestamp: number | undefined, t: (k: string) => string): string {
  if (timestamp == null || !Number.isFinite(timestamp)) return t('rp.file.timeUnknown')
  const date = new Date(timestamp)
  const pad = (value: number) => String(value).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`
}

function operationLocation(operation: FileOperation): string {
  const unified = operation.diff?.unified || ''
  const hunk = unified.match(/^@@\s+[^+]*\+(\d+)(?:,(\d+))?\s+@@(?:\s*(.*))?/m)
  if (!hunk) return ''
  const context = (hunk[3] || '').trim()
    .replace(/^(?:func|function|class|type|const|let|var)\s+/, '')
    .replace(/\s*[{(:].*$/, '')
    .trim()
  if (context) return context.slice(0, 30)
  const start = Number(hunk[1])
  const count = Number(hunk[2] || 1)
  return count > 1 ? `L${start}–${start + count - 1}` : `L${start}`
}

export function aggregateFileActivity(blocks: MsgBlock[], gitFiles: GitFile[]): FileActivity[] {
  const byPath = new Map<string, FileActivity>()
  const ensure = (path: string) => {
    const normalized = normalizeFilePath(path)
    let item = byPath.get(normalized)
    if (!item) {
      item = { path: normalized, status: 'read', additions: 0, deletions: 0, changed: false, lastAt: 0, operations: [] }
      byPath.set(normalized, item)
    }
    return item
  }

  for (const block of blocks) {
    if (block.kind !== 'tool') continue
    // run_python 的 args 是**脚本源码**（没有 path 字段），产物路径只能取 Go 侧在产出点写的
    // Diff.path —— 有它才说明这次脚本动了文件，才与 write/edit 同等待遇；没有就说明这个调用
    // 只是算了点东西，不进文件活动列表（否则每跑一次脚本都会多出一条无路径条目）。
    const pyWrote = block.name === 'run_python' && !!block.diff?.path
    if (!pyWrote && !['read_file', 'write_file', 'edit_file'].includes(block.name)) continue
    let path = pyWrote ? String(block.diff?.path ?? '') : ''
    if (!path) {
      try { path = String(JSON.parse(block.args || '{}').path || '') } catch { /* ignore malformed tool args */ }
    }
    if (!path) continue
    const item = ensure(path)
    item.operations.push({ toolId: block.toolId, tool: block.name, result: block.result || '', diff: block.diff, time: block.startedAt, durMs: block.durMs, isError: block.isError })
    item.lastAt = Math.max(item.lastAt, block.startedAt ?? 0)
    // pyWrote 与 write/edit 同等待遇：Go 侧既然认了「脚本改了这个文件」，它在文件栏里就是变更态
    //（而不是只读列表里的一条记录）。
    if (!block.isError && block.status === 'done' && (pyWrote || block.name === 'write_file' || block.name === 'edit_file')) {
      item.changed = true
      item.status = 'modified'
    }
    if (block.diff) {
      item.additions += block.diff.added
      item.deletions += block.diff.removed
    }
  }

  for (const file of gitFiles) {
    const item = ensure(file.path)
    item.git = file
    item.changed = true
    item.status = (['added', 'modified', 'deleted', 'conflicted', 'untracked'].includes(file.status)
      ? file.status
      : 'modified') as FileActivityStatus
    if (item.additions === 0 && item.deletions === 0) {
      item.additions = file.additions || 0
      item.deletions = file.deletions || 0
    }
    item.lastAt = Math.max(item.lastAt, file.changedAt ?? 0)
  }

  return [...byPath.values()].sort((a, b) => Number(b.changed) - Number(a.changed) || b.lastAt - a.lastAt || a.path.localeCompare(b.path))
}

type FileDetailMode = 'operation' | 'file' | 'source'

// 文件双栏分栏条：master（列表）最小/默认宽度与 detail（全文/变更）最小宽度（px）。
// 默认 44% 由 CSS grid 承担（minmax(200px, 44%)），拖拽后以 px 覆盖。
const FILE_MASTER_MIN = 200
const FILE_DETAIL_MIN = 320

function OperationDiff({ operation }: { operation?: FileOperation }) {
  const t = useT()
  if (!operation) return <div className="file-activity-empty">{t('rp.file.noOperation')}</div>
  if (operation.diff?.unified) return <UnifiedDiff unified={operation.diff.unified} />
  return <div className="file-activity-empty">{t('rp.file.noDiff')}</div>
}

// —— 每个 file 标签自己的滚动位置（阶段 2「浏览器式标签」）——
//
// 为什么需要：切标签时本组件因 key={tab.id} 重挂载（见 RightPanel 的渲染点），DOM 是新的
// → 浏览器不会替我们记住滚动位置。而「关掉一个标签不影响另一个的滚动位置」是本阶段的
// 验收口径之一，所以按**标签 id** 存一份（模块级：组件实例会随标签切换而重建）。
//
// 存「容器角色 → scrollTop」而不是元素引用：重挂载后元素是新的；同一标签的 path/mode 不变，
// 容器角色也就稳定（.fp-code / .md-preview-doc / .file-inspector-body / 列表）。
// 记录方式是**滚动时写**（capture 监听），不是卸载时读 DOM —— 卸载时机在 React 里不保证
// ref 还在（commitDeletion 会先 detach ref），读 DOM 会拿到 null 而静默丢掉位置。
const FILE_TAB_SCROLL_MAX = 32
const fileTabScroll = new Map<string, Record<string, number>>()
const SCROLL_ROLES: Array<[string, string]> = [
  ['master', '.file-activity-master'],
  ['body', '.file-inspector-body'],
  ['code', '.file-source .fp-code'],
  ['md', '.file-source .md-preview-doc'],
]
const scrollRoleOf = (el: HTMLElement): string | undefined =>
  SCROLL_ROLES.find(([, sel]) => el.matches(sel))?.[0]

// —— 每个 file 标签自己的「视图模式」（模块级，理由同上）——
// 用户在某个标签里选过「纯文本 / 表格」后切走再切回，期望的是**还在原来那个视图**（浏览器式
// 标签）。只记用户显式选过的两个模式开关；列表选中/展开/分隔条宽度这类**派生态**不记 ——
// 它们由标签 payload 派生（见下面的定位 effect），记了反而多一份真相源。
// 语义边界：**重新打开**同一个文件（openSeq 自增）仍然回默认视图（用户决策「不记住」），
// 只有「切走再切回」才恢复 —— 见下面 openSeq 的 effect。
type FileViewModes = { mdMode: 'preview' | 'plain'; csvMode: 'table' | 'plain' }
const FILE_TAB_VIEW_MAX = 32
const fileTabView = new Map<string, FileViewModes>()
const recallViewModes = (tabId: string): FileViewModes => fileTabView.get(tabId) ?? { mdMode: 'preview', csvMode: 'table' }
function rememberViewModes(tabId: string, patch: Partial<FileViewModes>): void {
  fileTabView.delete(tabId) // 先删再插 → 最近用过的排末尾，超出上限淘汰最久没动过的
  fileTabView.set(tabId, { ...recallViewModes(tabId), ...patch })
  while (fileTabView.size > FILE_TAB_VIEW_MAX) {
    const oldest = fileTabView.keys().next().value
    if (oldest === undefined) break
    fileTabView.delete(oldest)
  }
}

// 文件活动：按路径聚合工具调用与 Git 状态；点击对话中的文件可精确定位并高亮。
//
// 阶段 2（内容按标签隔离）：本组件的**数据源是激活的 file 标签**，不再是全局单例
// `filePreview` / `fileFocus`。三处口径随之改变：
//   · 内容 → `filePreviews[fileKeyOf(tab.path)]`（按归一化路径索引的缓存；同一文件的
//     diff / source 两个标签共享同一份内容，互不覆盖）；
//   · 「每次打开回默认视图」的触发器 → `tab.openSeq`（阶段 1 是全局 fileFocus.seq，
//     那会让重开 A 把 B 的视图一起复位）；
//   · 视图状态 → 由 `key={tab.id}`（见 RightPanel 的渲染点）保证每个标签一份实例。
// 为什么必须换：两个 file 标签若都读单例，打开第二个就是**覆盖**第一个的内容。
function FilePreviewTab({ tab }: { tab: FileTabInstance }) {
  const t = useT()
  const workspace = useAppStore((s) => s.workspace) // fileKeyOf / sameFilePath 判工作区内外用
  const filePreviews = useAppStore((s) => s.filePreviews)
  const ensureFilePreview = useAppStore((s) => s.ensureFilePreview)
  const tabPath = tab.path
  // 内容 = 本标签路径对应的缓存条目。缓存缺失时由下面的 effect 补拉（切工作区清空过 /
  // 被上限淘汰 / 从「最近关闭」重开都会走到这条）。
  const filePreview = useMemo(
    () => (tabPath ? filePreviews[fileKeyOf(tabPath, workspace)] : undefined),
    [filePreviews, tabPath, workspace],
  )
  const files = useAppStore((s) => s.gitFiles) ?? []
  const blocks = useAppStore((s) => s.blocks) ?? []
  const openFile = useAppStore((s) => s.openFile)
  const closeFile = useAppStore((s) => s.closeFile)
  // 二进制预览入口（见 renderBinaryActions）：与产出卡走**同一条**通道 ——
  // pdf/图片/html/svg 用内嵌浏览器，xlsx/docx/pptx 先经 bridge 转 HTML 再进同一条浏览器。
  // 复用 store 动作而非在文件栏新写渲染，是为了「卡片能看 → 文件栏也能看」不会各自演化。
  const openHtmlInBrowser = useAppStore((s) => s.openHtmlInBrowser)
  const previewOfficeFile = useAppStore((s) => s.previewOfficeFile)
  // 转换在途的文件路径（xlsx/docx/pptx 任一）——驱动按钮「转换中…」与禁用，见 renderBinaryActions。
  const officeBusy = useAppStore((s) => s.officeBusy)
  const showToast = useAppStore((s) => s.showToast)
  const resolved = useResolvedTheme()
  // 代码主题也在这里订阅：字号/主题之外，「外观 → 代码主题」改了必须**重渲 HTML 内嵌的那段
  // shiki 产物** —— 高亮结果是字符串，不像 CSS 变量会自动跟着走。此前依赖数组里只有
  // resolved（亮/暗），于是切同侧的另一个代码主题时对话代码块会变、右侧栏预览纹丝不动。
  const ap = useAppearance()
  const codeTheme = resolved === 'paper' ? ap.codeThemeLight : ap.codeThemeDark
  const [html, setHtml] = useState('')
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  const [filter, setFilter] = useState<FileFilter>('all')
  const [highlightPath, setHighlightPath] = useState('')
  const [highlightTool, setHighlightTool] = useState('')
  const [selectedPath, setSelectedPath] = useState('')
  const [selectedTool, setSelectedTool] = useState('')
  const [detailMode, setDetailMode] = useState<FileDetailMode>('operation')
  // Markdown「最终文件」双模式（仅独立大视图 standalone 生效）：'preview' = 渲染后的
  // 文档（复用对话区 Markdown 管线：GFM/表格/mermaid/KaTeX/代码高亮），'plain' = shiki
  // 高亮源码（含行号）。用户决策：默认预览、不持久化（每次打开回到默认）、只做大视图。
  // 按标签记忆（阶段 2）：切走再切回恢复用户选过的模式（见 fileTabView）；**重新打开**同一文件
  // （openSeq 自增）仍回默认 —— 用户决策「不记住」的口径没变，变的是「谁算一次打开」。
  const [mdMode, setMdMode] = useState<'preview' | 'plain'>(() => recallViewModes(tab.id).mdMode)
  // CSV/TSV 双模式：'table' = 表格（默认，走 CsvPreview → GFM 表格），'plain' = shiki 高亮源码。
  // 与 mdMode 分开存，而不是合并成一个「显示模式」枚举：两者的默认值与可用取值不同
  // （Markdown 默认预览、CSV 默认表格），合并后判定「当前该显示什么」要多绕一层条件。
  const [csvMode, setCsvMode] = useState<'table' | 'plain'>(() => recallViewModes(tab.id).csvMode)
  // 双栏分隔条拖拽（master 像素宽度；不持久化，默认 44%，与 grid 初始一致）
  const [masterW, setMasterW] = useState<number | null>(null)
  const masterWStart = useRef<{ startX: number; startW: number; maxW: number } | null>(null)
  const panelRef = useRef<HTMLDivElement>(null)
  const rowRefs = useRef(new Map<string, HTMLDivElement>())
  const highlightTimer = useRef<number | undefined>(undefined)
  const activities = useMemo(() => aggregateFileActivity(blocks, files), [blocks, files])
  const changed = useMemo(() => activities.filter((f) => f.changed), [activities])
  const readOnly = useMemo(() => activities.filter((f) => !f.changed), [activities])
  const visibleGroups = filter === 'changed'
    ? [{ label: t('rp.file.changedFiles'), items: changed }]
    : filter === 'read'
      ? [{ label: t('rp.file.readOnly'), items: readOnly }]
      : [{ label: t('rp.file.changedFiles'), items: changed }, { label: t('rp.file.readOnly'), items: readOnly }]
  const additions = activities.reduce((sum, f) => sum + f.additions, 0)
  const deletions = activities.reduce((sum, f) => sum + f.deletions, 0)

  useEffect(() => {
    let alive = true
    if (!filePreview) { setHtml(''); return }
    setHtml('')
    void codeToHtml(filePreview.content, langFromPath(filePreview.path), resolved, true).then((h) => { if (alive) setHtml(h) })
    return () => { alive = false }
  }, [filePreview?.path, filePreview?.content, resolved, codeTheme])

  // 缓存缺失 → 按需补拉（阶段 2）。什么时候会缺：切工作区把缓存清了、被 FILE_PREVIEWS_MAX
  // 淘汰、标签从「最近关闭」重开。没有这条兜底，用户会永久停在「正在读取文件…」（阶段 1
  // 靠「打开即清空 + 立即请求」顺带保证了这一点，阶段 2 内容可以跨标签复用就必须显式补）。
  // 依赖里带 filePreviews：响应写回缓存后 effect 重跑一次（此时已有内容 → 直接返回）。
  useEffect(() => { if (tabPath && !filePreview) ensureFilePreview(tabPath) }, [tabPath, filePreview, filePreviews, ensureFilePreview])

  // 每次打开文件 → Markdown 模式回到默认（预览）。用户决策「不记住」：语义是
  // **每次打开**都回默认，不只是「换到另一个文件」时。（openSeq 每次命中该标签自增，
  // 故重开同一文件也会重置。）
  //
  // 为什么要 lastOpenSeq 这层判断：切标签会让组件重挂载（key={tab.id}），挂载时 effect 同样
  // 会跑一遍 —— 没有它，每次切回来都被复位成默认，「切走再切回还在原视图」就不成立（那是
  // 浏览器式标签的基本预期）。判据是「openSeq 变了」而不是「跑了几次」，故与 StrictMode 的
  // 双调用无关。
  const lastOpenSeq = useRef(tab.openSeq)
  useEffect(() => {
    if (lastOpenSeq.current === tab.openSeq) return
    lastOpenSeq.current = tab.openSeq
    setMdMode('preview')
    setCsvMode('table')
  }, [tab.openSeq])

  // 用户选过的模式按标签记下来（切走再切回时由上面的 lazy init 取回）。写回放在 effect 里，
  // 调用点（两个切换按钮）不必各写一遍。
  useEffect(() => { rememberViewModes(tab.id, { mdMode }) }, [tab.id, mdMode])
  useEffect(() => { rememberViewModes(tab.id, { csvMode }) }, [tab.id, csvMode])


  // isMarkdownFile：path 指向的文件当前是否已加载且为 Markdown（决定是否给切换按钮、
  // 是否走预览渲染）。**必须按「正在渲染的那个路径」逐处判定**，不能用某个全局布尔 ——
  // 「最终文件」有两条渲染路径（活动列表内嵌 item.path / 独立大视图 standalonePath），
  // 用全局布尔正是 2026-09 的漏改原因：只有独立大视图满足条件，用户在活动列表点开
  // 完全没有切换按钮。
  const isMarkdownFile = (p: string) => isMarkdownPath(p) && Boolean(filePreview && sameFilePath(filePreview.path, p, workspace))

  // 同上，CSV 版：判定也按「正在渲染的那个路径」，供两条渲染路径共用。
  const isCsvFile = (p: string) => isCsvPath(p) && Boolean(filePreview && sameFilePath(filePreview.path, p, workspace))

  // 二进制文件的类型化预览入口（见 renderFinalFile ⓪）。
  // 为什么需要它：bridge 对二进制只回 binary=true + 空 content（xlsx/docx/pdf/图片），
  // 文件栏此前只给一句「不适用文本预览」，**没有任何出口** —— 用户点开 .pdf/.xlsx 只能
  // 看到一句话，而产出卡那条路早就有预览按钮。同一个文件，从卡片进能看、从文件栏进看不了。
  // 这里按类型分派到**既有**通道（不新增读文件/渲染机制），与产出卡主按钮同一口径：
  //   pdf/图片/html/svg → openHtmlInBrowser（内嵌浏览器 file://，Chromium 原生渲染）
  //   xlsx/xls/xlsm     → previewOfficeFile(p,'xlsx')（受管 Python + openpyxl 转 HTML）
  //   docx/docm/doc     → previewOfficeFile(p,'docx')（受管 Python + mammoth 转 HTML）
  //   pptx/pptm/ppt     → previewOfficeFile(p,'pptx')（受管 Python + python-pptx 近似版式）
  // 后三条拿到产物路径后走的都是 openHtmlInBrowser —— 与 PDF 同一条内嵌浏览器。
  // 其余二进制（zip/音视频等）**确实没有**应用内预览：只给「用默认程序打开」这条真实出口。
  const renderBinaryActions = (p: string) => {
    const kind = visualKindOf(p)
    // 需要经 bridge 转换的三种格式 → 主按钮文案与动作。表在这里（而不是嵌套三元）：
    // 下面的按钮文案/动作/次按钮判定都要问「这是不是转换类」，一份表比三处条件可靠。
    const convertLabel = kind === 'xlsx' ? 'art.previewXlsx'
      : kind === 'docx' ? 'art.previewDocx'
        : kind === 'pptx' ? 'art.previewPptx' : ''
    // 转换在途反馈（与 ArtifactCard 同一口径）：首次转换可能先 bootstrap 受管运行时，
    // 分钟级 —— 按钮没有可见反馈时，用户会以为没点到而反复点。只标**这一个文件**在途，
    // 同栏其他文件的按钮照常可用（store 侧 busy 是单路径槽，见 previewOfficeFile 注释）。
    const thisBusy = !!kind && officeBusy === p
    return <div className="file-binary-actions">
      {kind && <button className="btn primary" disabled={thisBusy} onClick={() => {
        if (kind === 'xlsx' || kind === 'docx' || kind === 'pptx') void previewOfficeFile(p, kind)
        else void openHtmlInBrowser(p)
      }}>
        {thisBusy ? t('art.converting') : convertLabel ? t(convertLabel) : kind === 'pdf' ? t('art.previewPdf') : t('art.openBrowser')}
      </button>}
      {/* 次按钮保留「用默认程序打开」：内嵌预览看不了批注/图表/条件格式/修订/动画，系统应用是
          必要出口；转换失败（.xls/.doc/.ppt 老格式不支持 / 运行时未就绪）也靠它兜底。
          与产出卡（ArtifactCard.openDefault）同一实现口径 —— openPath 由 electron 侧做路径校验。 */}
      <button className="btn" onClick={() => void openDefault(p)}>{t('art.openDefault')}</button>
    </div>
  }
  // 「用默认程序打开」：与 ArtifactCard.openDefault 同实现（绝对化由 electron 侧兜底校验）。
  const openDefault = async (p: string) => {
    const d = window.desktop
    if (!d?.openPath) { showToast(t('art.unsupported'), 'error'); return }
    const abs = p.startsWith('/') ? p : workspace ? `${workspace}/${p}` : p
    const r = await d.openPath(abs)
    if (!r?.ok) showToast(t('art.openFail').replace('{err}', r?.error || ''), 'error')
  }

  // read_file start_line → 「最终文件」渲染完成后定位滚动到对应行：
  // html 刚更新时 DOM 未提交，用 rAF 双帧等 shiki 结构稳定后再滚。
  // 只对 source 模式（无 toolId，统一 open-file）生效；operation 模式不动。
  // 兼容两种渲染路径：独立文件视图（standalone，file-side-detail）与
  // 文件活动列表内嵌 source（detailMode==='source'，file-inline-detail）。
  // 阶段 2：定位锚来自**本标签**的 payload（tab.startLine / tab.toolId），不再读 fileFocus ——
  // 否则打开 B 时 A 标签（已挂载在别处？）会跟着滚。openSeq 参与依赖：重开同一文件要重新定位。
  useEffect(() => {
    if (!html || tab.startLine == null || tab.toolId) return
    // 可视容器：standalone 优先（file-side-detail 显示中），否则找 detailMode==='source'
    // 的内嵌容器（窄屏 file-inline-detail 显示中）。列表容器 display:none 时
    // querySelector 也会命中 → 用 closest 判断是否可见（offsetParent!==null）。
    // 范围限定在本面板内（panelRef）：同一页面上只挂一个 FilePreviewTab，但把查询收窄
    // 到自己的容器能防住将来「多面板并存」时的误定位。
    const containers = Array.from((panelRef.current ?? document).querySelectorAll<HTMLElement>('.fp-code'))
    const visible = containers.find((c) => c.offsetParent !== null) ?? containers[0]
    if (!visible) return
    const target = visible.querySelector<HTMLElement>(`.fp-ln[data-line="${tab.startLine}"]`)
    if (!target) return
    const lineEl = target.closest('.line')
    if (!lineEl) return
    // 用 scrollIntoView 定位该行到容器中部（block:'center'）；smooth 与整体 UX 一致。
    let raf2 = 0
    const frame1 = requestAnimationFrame(() => {
      raf2 = requestAnimationFrame(() => {
        lineEl.scrollIntoView({ block: 'center', behavior: 'smooth' })
      })
    })
    return () => { cancelAnimationFrame(frame1); cancelAnimationFrame(raf2) }
  }, [html, tab.openSeq, tab.startLine, tab.toolId])

  // 定位本标签的文件：把文件活动列表选中/展开/高亮到 tab 指向的那一项，并按 mode 落视图模式
  //（diff → 该次操作；source → 最终文件）。触发器 = tab.openSeq（每次打开该标签自增）+
  // payload 三元组；组件因 key={tab.id} 在切标签时重挂载，故激活一个标签也会跑到这里
  //（= 每个标签的列表选中态由**它自己的 payload** 派生，不继承上一个标签）。
  useEffect(() => {
    if (!tab.path) return
    const path = normalizeFilePath(tab.path)
    // source 模式（统一 open-file，无 toolId）：打开最终文件全量视图。
    // 文件若在活动列表 → 选中它 + 切 source 模式（不滚动不高亮，独立文件由 standalone 接管）
    if (tab.mode !== 'diff' || !tab.toolId) {
      setSelectedPath(path)
      setDetailMode('source')
      return
    }
    setFilter('all')
    setExpanded((prev) => new Set(prev).add(path))
    setHighlightPath(path)
    setHighlightTool(tab.toolId)
    setSelectedPath(path)
    setSelectedTool(tab.toolId)
    setDetailMode('operation')
    if (highlightTimer.current) window.clearTimeout(highlightTimer.current)
    let frame2 = 0
    const frame1 = requestAnimationFrame(() => {
      frame2 = requestAnimationFrame(() => rowRefs.current.get(path)?.scrollIntoView({ block: 'center', behavior: 'smooth' }))
    })
    highlightTimer.current = window.setTimeout(() => { setHighlightPath(''); setHighlightTool('') }, 1400)
    return () => { cancelAnimationFrame(frame1); cancelAnimationFrame(frame2) }
  }, [tab.openSeq, tabPath, tab.mode, tab.toolId])

  useEffect(() => () => {
    if (highlightTimer.current) window.clearTimeout(highlightTimer.current)
  }, [])

  // 滚动位置：① 监听本面板内所有滚动容器的 scroll（capture，因为 scroll 不冒泡）→ 记到
  // fileTabScroll[tab.id]；② 内容就绪后把上次的位置放回去（重挂载后 DOM 是新的）。
  // 为什么放在内容之后恢复：html 为空时容器还很矮，直接写 scrollTop 会被浏览器夹到 0。
  useEffect(() => {
    const root = panelRef.current
    if (!root) return
    const onScroll = (e: Event) => {
      const el = e.target as HTMLElement | null
      if (!el || el === root) return
      const role = scrollRoleOf(el)
      if (!role) return
      const snap = { ...(fileTabScroll.get(tab.id) ?? {}) }
      if (el.scrollTop > 0) snap[role] = el.scrollTop
      else delete snap[role]
      // 先删再插 → 最近用过的标签排在末尾；超出上限淘汰最久没动过的（防无界增长）
      fileTabScroll.delete(tab.id)
      fileTabScroll.set(tab.id, snap)
      while (fileTabScroll.size > FILE_TAB_SCROLL_MAX) {
        const oldest = fileTabScroll.keys().next().value
        if (oldest === undefined) break
        fileTabScroll.delete(oldest)
      }
    }
    root.addEventListener('scroll', onScroll, true)
    return () => root.removeEventListener('scroll', onScroll, true)
  }, [tab.id])

  useEffect(() => {
    const root = panelRef.current
    const snap = fileTabScroll.get(tab.id)
    if (!root || !snap) return
    // 有 read_file 定位锚时不恢复：那一次打开的意图是「滚到那一行」（见上面 startLine 的 effect）
    if (tab.startLine != null && tab.mode !== 'diff') return
    for (const [role, sel] of SCROLL_ROLES) {
      const el = root.querySelector<HTMLElement>(sel)
      if (el && snap[role] != null) el.scrollTop = snap[role]
    }
  }, [tab.id, tab.startLine, tab.mode, html, filePreview?.content])

  const toggleFile = (item: FileActivity) => {
    const opening = !expanded.has(item.path)
    setExpanded((prev) => {
      const next = new Set(prev)
      opening ? next.add(item.path) : next.delete(item.path)
      return next
    })
    setSelectedPath(item.path)
    if (opening) {
      const latestChange = [...item.operations].reverse().find((op) => op.tool !== 'read_file' && op.diff?.unified)
      setSelectedTool(latestChange?.toolId || '')
      setDetailMode(latestChange ? 'operation' : 'source')
      openFile(item.path)
    }
  }

  const selectOperation = (item: FileActivity, operation: FileOperation) => {
    setSelectedPath(item.path)
    setSelectedTool(operation.toolId)
    setDetailMode('operation')
  }

  // 文件活动列表内嵌的「最终文件」（detailMode==='source'）：与独立大视图共用同一
  // 份双模式渲染（Markdown 预览/纯文本、CSV 表格/纯文本）—— 两处若各写一遍，正是
  // 「改了一处漏另一处」的温床（2026-09 实测踩过：只在独立大视图加了切换按钮，
  // 用户在活动列表点开完全没有）。
  // matchPath 为目标文件的路径（列表内嵌传 item.path，独立大视图传 standalonePath）。
  const renderFinalFile = (matchPath: string) => {
    if (!(filePreview && sameFilePath(filePreview.path, matchPath, workspace))) {
      return <div className="file-activity-empty">{t('rp.file.reading')}</div>
    }
    return <div className="file-source">
      <div className="file-source-meta mono">
        {filePreview.binary
          ? '' // 二进制没有行数概念（bridge 返回 lines=0），显示 "0 行" 只会让人困惑
          : t('rp.file.lines').replace('{n}', String(filePreview.lines)) + (filePreview.truncated ? t('rp.file.truncated') : '')}
      </div>
      {/* 四条渲染路径（择一），都复用既有能力，不新增读文件通道：
          ⓪ binary（bridge 判定：xlsx/docx/pdf/图片等，content 为空）→ 类型化预览入口。
             为什么必须单独挡：这条入口是**文件树/工具行**的 open-file，产出卡那条路有 xlsx/pdf
             专门分支，而这里此前没有——二进制会被当字符串喂给 shiki，结果是大段乱码 + 白耗
             渲染（渲染成本随内容线性增长）。
             binary=true 时**没有文本内容可展示**，故此处给的是「用哪条既有通道能真的看到内容」
             （pdf/图片/html/svg → 内嵌浏览器；xlsx/docx/pptx → bridge 转 HTML 后同一条浏览器；
             见 renderBinaryActions），而不是一句指不到按钮的提示。zip 等确实无应用内预览的
             格式 → 只留「用默认程序打开」这条真实出口，文案如实说明（rp.file.binaryNoPreview）。
          ① CSV/TSV 且表格模式 → CsvPreview（内容 → lib/csv.ts 转 GFM 表格 → 同一套
             Markdown 管线）；内容来自上面这条 file_preview 流程，**不在前端另读文件**
             （读文件的 workspace 越界校验与 1 MiB 上限在 bridge 侧，绕过它等于开洞）。
          ② Markdown 且预览模式 → 对话区 Markdown 组件（GFM/表格/mermaid/KaTeX）。
          ③ 其余（含 ①② 的纯文本模式）→ shiki 高亮源码（既有行为不变）。
          html（shiki 结果）对 CSV 也照常算着——切「纯文本」时立刻可用，省一次切换等待。 */}
      {filePreview.binary
        ? (visualKindOf(matchPath)
          // 可视类：内容已由上面的自动打开送进预览通道 —— 这里只留一行状态，**不再有提示段落
          // 与主按钮**（用户口径：那是多余的中间态）。文件动作（用默认程序打开／复制路径／
          // 在访达中显示）移到预览标签的右键菜单里（见 BrowserTab），需要时才出现。
          ? <div className="file-binary-status">
            <span className="file-binary-ok" aria-hidden="true">✓</span>
            {officeBusy === matchPath ? t('art.converting') : t('rp.file.previewOpened')}
          </div>
          // 无应用内预览（zip/音视频等）：保留「用默认程序打开」这条真实出口，文案如实说明
          //（不把「预览」写进没有预览的格式里 —— 那正是当初要修的旧文案缺陷）。
          : <div className="file-binary-hint">
            <div className="file-activity-empty">{t('rp.file.binaryNoPreview')}</div>
            {renderBinaryActions(matchPath)}
          </div>)
        : isCsvFile(matchPath) && csvMode === 'table'
          ? <CsvPreview content={filePreview.content} path={filePreview.path} />
          : isMarkdownFile(matchPath) && mdMode === 'preview'
            ? <div className="md-preview-doc"><Markdown text={filePreview.content} baseDir={baseDirFor(filePreview.path, workspace)} /></div>
            : <div className="fp-code" dangerouslySetInnerHTML={{ __html: html }} />}
    </div>
  }

  const renderDetail = (item: FileActivity, compact = false) => {
    const operation = item.operations.find((op) => op.toolId === selectedTool)
    const fileDiff = item.git?.diff || [...item.operations].reverse().find((op) => op.diff?.unified)?.diff?.unified
    return <div className={`file-inspector ${compact ? 'compact' : ''}`}>
      <div className="file-inspector-head">
        <span className="file-inspector-path mono" title={item.path}>{item.path}</span>
        {operation && detailMode === 'operation' && <span className="file-inspector-operation mono">
          {operation.tool} · {formatToolTimestamp(operation.time, t)}
          {operation.diff
            ? <span className="op-stats"> · <b className="add">+{operation.diff.added}</b> <b className="del">−{operation.diff.removed}</b></span>
            : ' · +0 −0'}
        </span>}
        <div className="file-detail-tabs">
          {/* Markdown 双模式切换：见 renderFinalFile —— 「最终文件」在两个分支
              （本处内嵌 / 独立大视图）都要能切，故两处共用同一份状态与判定。 */}
          {isMarkdownFile(item.path) && <span className="md-view-switch" role="group" aria-label={t('rp.file.mdTabsAria')}>
            <button className={mdMode === 'preview' ? 'on' : ''} onClick={() => setMdMode('preview')} aria-pressed={mdMode === 'preview'}>{t('rp.file.mdPreview')}</button>
            <button className={mdMode === 'plain' ? 'on' : ''} onClick={() => setMdMode('plain')} aria-pressed={mdMode === 'plain'}>{t('rp.file.mdSource')}</button>
          </span>}
          {/* CSV/TSV 同款双模式（表格 / 纯文本）：与 Markdown 那条并列、互斥出现
              （一个文件不可能既是 .md 又是 .csv）。复用 .md-view-switch 的样式 ——
              它是「内容形态切换器」的既有样式，改名会牵连 e2e 选择器且毫无收益。 */}
          {isCsvFile(item.path) && <span className="md-view-switch" role="group" aria-label={t('rp.file.csvTabsAria')}>
            <button className={csvMode === 'table' ? 'on' : ''} onClick={() => setCsvMode('table')} aria-pressed={csvMode === 'table'}>{t('rp.file.csvPreview')}</button>
            <button className={csvMode === 'plain' ? 'on' : ''} onClick={() => setCsvMode('plain')} aria-pressed={csvMode === 'plain'}>{t('rp.file.csvSource')}</button>
          </span>}
          <button className={detailMode === 'operation' ? 'on' : ''} onClick={() => setDetailMode('operation')}>{t('rp.file.thisChange')}</button>
          <button className={detailMode === 'file' ? 'on' : ''} onClick={() => setDetailMode('file')}>{t('rp.file.fileChange')}</button>
          <button className={detailMode === 'source' ? 'on' : ''} onClick={() => { setDetailMode('source'); openFile(item.path) }}>{t('rp.file.finalFile')}</button>
        </div>
      </div>
      <div className="file-inspector-body">
        {detailMode === 'operation' && <OperationDiff operation={operation} />}
        {detailMode === 'file' && (fileDiff ? <UnifiedDiff unified={fileDiff} /> : <div className="file-activity-empty">{t('rp.file.noFileDiff')}</div>)}
        {detailMode === 'source' && renderFinalFile(item.path)}
      </div>
    </div>
  }

  const renderItem = (item: FileActivity) => {
    const isOpen = expanded.has(item.path)
    const changeOps = item.operations.filter((op) => op.tool !== 'read_file')
    const failedOps = changeOps.filter((op) => op.isError)
    const shownOps = changeOps.filter((op) => !op.isError)
    const readCount = item.operations.filter((op) => op.tool === 'read_file').length
    return <div
      className={`file-activity-item status-${item.status} ${isOpen ? 'is-open' : ''} ${highlightPath === item.path ? 'is-target' : ''}`}
      key={item.path}
      ref={(node) => { if (node) rowRefs.current.set(item.path, node); else rowRefs.current.delete(item.path) }}
    >
      <button className="file-activity-row" onClick={() => toggleFile(item)} aria-expanded={isOpen} aria-current={highlightPath === item.path ? 'true' : undefined}>
        <span className="file-activity-chevron">{isOpen ? '⌄' : '›'}</span>
        <span className="file-activity-status mono" title={item.status}>{fileStatusLabel(item.status)}</span>
        <span className="file-activity-path mono" title={item.path}>{item.path}</span>
        {item.changed
          ? <span className="file-activity-count mono">{t('rp.file.changedCount').replace('{n}', String(shownOps.length))}{failedOps.length > 0 ? t('rp.file.failedCount').replace('{n}', String(failedOps.length)) : ''}</span>
          : readCount > 1 && <span className="file-activity-count mono">{t('rp.file.readCount').replace('{n}', String(readCount))}</span>}
        <span className="grow" />
        <span className="file-activity-stat mono">
          {item.additions > 0 && <b className="add">+{item.additions}</b>}
          {item.deletions > 0 && <b className="del">−{item.deletions}</b>}
          {!item.changed && <span>{t('rp.file.read')}</span>}
        </span>
      </button>
      {isOpen && <div className="file-activity-detail">
        <div className="file-operation-list">
          {shownOps.map((op) => {
            const location = operationLocation(op)
            const noChange = !op.diff || (op.diff.added === 0 && op.diff.removed === 0)
            return <button
              className={`file-operation is-change ${noChange ? 'no-change' : ''} ${selectedTool === op.toolId ? 'is-selected' : ''} ${highlightTool === op.toolId ? 'is-target' : ''}`}
              key={op.toolId}
              onClick={() => selectOperation(item, op)}
              aria-pressed={selectedTool === op.toolId}
            >
              <span className="operation-status">✓</span>
              <span className="operation-chevron">{selectedTool === op.toolId && detailMode === 'operation' ? '⌄' : '›'}</span>
              <span className="operation-name mono">{op.tool}</span>
              <span className="operation-location mono" title={location || (noChange ? t('rp.file.noNetChange') : undefined)}>{location || (noChange ? t('rp.file.noNetChange') : '')}</span>
              <span className="operation-called-at mono" title={formatToolTimestamp(op.time, t)}>{formatToolTimestamp(op.time, t)}</span>
              {op.diff && !noChange && <span className="operation-stat mono"><b className="add">+{op.diff.added}</b><b className="del">−{op.diff.removed}</b></span>}
              {op.durMs != null && <span className="operation-time mono">{fmtDuration(op.durMs)}</span>}
            </button>
          })}
          {failedOps.length > 0 && <details className="failed-operation-group">
            <summary><span className="operation-status error">✗</span><span>{t('rp.file.failedCalls').replace('{n}', String(failedOps.length))}</span></summary>
            {failedOps.map((op) => <div className="file-operation failed-operation" key={op.toolId}>
              <span className="operation-name mono">{op.tool}</span>
              <span className="operation-called-at mono" title={formatToolTimestamp(op.time, t)}>{formatToolTimestamp(op.time, t)}</span>
              {op.durMs != null && <span className="operation-time mono">{fmtDuration(op.durMs)}</span>}
            </div>)}
          </details>}
          {changeOps.length === 0 && <div className="file-activity-empty">{t('rp.file.gitChanges')}</div>}
        </div>
        {selectedPath === item.path && <div className="file-inline-detail">{renderDetail(item, true)}</div>}
      </div>}
    </div>
  }

  const selectedActivity = activities.find((item) => item.path === selectedPath)

  // 独立文件完整视图（统一 open-file 能力）：**本标签**指向的文件不在活动列表
  //（工作区外绝对路径 / 尚未在会话里操作过的文件）→ 直接渲染全量内容（source 模式）。
  // 阶段 2：判据从全局 fileFocus 换成标签 payload —— mode='source' 且带 path 才是完整预览
  //（mode 缺省 = closeFile 清过视图，回落文件活动列表；此时保留 path 是为了标签标题不变）。
  const standalonePath = tab.mode === 'source' && tabPath ? normalizeFilePath(tabPath) : undefined
  // 同一性比较用 sameFilePath（容忍绝对 vs 工作区相对）——见其注释的实测缺陷说明
  const standalone = standalonePath && (!selectedActivity || !sameFilePath(selectedActivity.path, standalonePath, workspace))
    ? { path: standalonePath }
    : undefined

  // 注：「打开即见内容」（可视类文件点开即自动进预览）挂在 **store.openFile**（用户点击入口），
  // 不在这里做 —— 组件会在切走时卸载，去重状态随之丢失，会导致「切回文件栏又被弹回预览」的拉锯
  //（第一版实现实测：4ms 内 rightTab 从 file 被顶回 web）。本组件只负责把结果显示出来。

  // 整文件（standalone 全文，或列表里选中文件的「最终文件」）→ **独占整宽**：不渲染左侧活动列表
  // 与分隔条。用户口径（2026-09）：「完整视图就是整个文件，就不需要左侧的变更记录了，而查看变更
  // 的时候才会展示变更列表，否则会导致文件预览不全」。展示**变更**（operation / file）时保持两栏。
  // 两条渲染路径都算整文件：文件不在活动列表 → standalone；在列表里但切到「最终文件」→ detailMode。
  const wholeFile = Boolean(standalone || (selectedActivity && detailMode === 'source'))
  // 双列 = 列表 + 右侧详情/全文（选中文件或 standalone 全文都算，缺一不可否则 detail 无列可放）
  const splitVisible = Boolean(selectedActivity || standalone) && !wholeFile
  // 用户拖过分隔条 → px 宽度覆盖默认 44%；双击分隔条复位（masterW=null）
  const panelStyle = masterW != null && splitVisible ? { gridTemplateColumns: `${masterW}px 9px minmax(0, 1fr)` } : undefined
  const onSplitterDown = (e: React.PointerEvent<HTMLDivElement>) => {
    if (e.button !== 0 || !panelRef.current) return
    e.preventDefault()
    const master = panelRef.current.querySelector<HTMLElement>('.file-activity-master')
    if (!master) return
    e.currentTarget.setPointerCapture(e.pointerId)
    masterWStart.current = {
      startX: e.clientX,
      startW: master.getBoundingClientRect().width,
      maxW: panelRef.current.getBoundingClientRect().width - FILE_DETAIL_MIN,
    }
  }
  const onSplitterMove = (e: React.PointerEvent<HTMLDivElement>) => {
    const d = masterWStart.current
    if (!d) return
    const w = Math.max(FILE_MASTER_MIN, Math.min(d.maxW, d.startW + e.clientX - d.startX))
    setMasterW(Math.round(w))
  }
  const endSplit = () => { masterWStart.current = null }

  return <div
    className={`file-activity-panel ${splitVisible ? 'has-selection' : ''} ${wholeFile ? 'whole-file' : ''}`}
    style={panelStyle}
    ref={panelRef}
  >
    {/* 整文件态整块不渲染（不只是隐藏）：工具条/筛选/空态都在里面，留着只会白占宽度 */}
    {!wholeFile && <div className="file-activity-master">
      <div className="file-activity-toolbar">
        <div className="file-activity-summary">
          <div className="file-summary-main"><strong>{activities.length}</strong><span>{t('rp.file.files')}</span><span className="file-summary-split">·</span><span>{changed.length} {t('rp.file.changed')}</span><span>{readOnly.length} {t('rp.file.readOnly2')}</span></div>
          <div className="file-summary-stat mono"><b className="add">+{additions}</b><b className="del">−{deletions}</b></div>
        </div>
        {activities.length > 0 && <div className="file-filter" role="group" aria-label={t('rp.file.filterAria')}>
          {([['all', t('rp.file.all')], ['changed', t('rp.file.changed')], ['read', t('rp.file.readOnly2')]] as const).map(([key, label]) => <button key={key} className={filter === key ? 'on' : ''} onClick={() => setFilter(key)} aria-pressed={filter === key}>{label}</button>)}
        </div>}
      </div>
      {visibleGroups.map((group) => group.items.length > 0 && <section className="file-activity-group" key={group.label}>
        <div className="file-activity-group-title">{group.label}<span className="mono">{group.items.length}</span></div>
        <div className="file-activity-list">{group.items.map(renderItem)}</div>
      </section>)}
      {activities.length === 0 && !standalone && <div className="empty"><div className="big">{t('rp.file.noActivity')}</div>{t('rp.file.noActivityHint')}</div>}
    </div>}
    {splitVisible && (
      <div
        className="file-splitter"
        role="separator"
        aria-orientation="vertical"
        title={t('rp.file.dragSplit')}
        onPointerDown={onSplitterDown}
        onPointerMove={onSplitterMove}
        onPointerUp={endSplit}
        onPointerCancel={endSplit}
        onDoubleClick={() => setMasterW(null)}
      />
    )}
    {standalone && (
      <div className="file-side-detail">
        <div className="file-inspector">
          <div className="file-inspector-head">
            <span className="file-inspector-path mono" title={standalone.path}>{standalone.path}</span>
            <div className="file-detail-tabs">
              {isMarkdownFile(standalone.path) && <span className="md-view-switch" role="group" aria-label={t('rp.file.mdTabsAria')}>
                <button className={mdMode === 'preview' ? 'on' : ''} onClick={() => setMdMode('preview')} aria-pressed={mdMode === 'preview'}>{t('rp.file.mdPreview')}</button>
                <button className={mdMode === 'plain' ? 'on' : ''} onClick={() => setMdMode('plain')} aria-pressed={mdMode === 'plain'}>{t('rp.file.mdSource')}</button>
              </span>}
              {/* CSV/TSV 表格切换：与上面 Markdown 那条并列（见 renderDetail 处的说明）。
                  独立大视图这一支必须**独立存在** —— 2026-09 的漏改正是「只在其中一条
                  渲染路径加了切换按钮」，用户从产出卡片进来（走本分支）永远切不了。 */}
              {isCsvFile(standalone.path) && <span className="md-view-switch" role="group" aria-label={t('rp.file.csvTabsAria')}>
                <button className={csvMode === 'table' ? 'on' : ''} onClick={() => setCsvMode('table')} aria-pressed={csvMode === 'table'}>{t('rp.file.csvPreview')}</button>
                <button className={csvMode === 'plain' ? 'on' : ''} onClick={() => setCsvMode('plain')} aria-pressed={csvMode === 'plain'}>{t('rp.file.csvSource')}</button>
              </span>}
              <button className="on">{t('rp.file.finalFile')}</button>
              <button className="file-detail-close" onClick={() => closeFile()} title={t('rp.file.closeView')} aria-label={t('rp.file.closeView')}>✕</button>
            </div>
          </div>
          <div className="file-inspector-body">
            {/* 与文件活动列表内嵌视图共用 renderFinalFile（含 Markdown 双模式 +
                加载中占位），避免两处实现漂移。 */}
            {standalone ? renderFinalFile(standalone.path) : null}
          </div>
        </div>
      </div>
    )}
    {selectedActivity && !standalone && <div className="file-side-detail">{renderDetail(selectedActivity)}</div>}
  </div>
}
// M5/M6：内嵌视图状态类型与 AI 动作描述已上移至 store（全局订阅，tab 未开也更新）
// 浏览器面板（右栏 web tab）：插件私有浏览器连接的状态 + 页列表 + 当前页截图。// 浏览器面板（右栏 web tab）：插件私有浏览器连接的状态 + 页列表 + 当前页截图。
// 数据经 bridge browser_* 命令按需拉取（截图重内容不推送事件通道）；页面切换调截图。
// 沉浸态（immersive）：收起**浏览器自己的工具条**，把垂直空间让给内容；☰ 唤回。
// 注意与 M6 原语义的差别：M6 收的是**面板级 tab 行**，而阶段 1 起面板级标签条是常驻的
//（它是唯一的标签入口，收起它就等于切不走标签）→ 这里只收浏览器自己的 chrome。
// —— R4：预览标签条的展示口径（原型 docs/demo/right-panel-preview.html 的 .pv-tab）——
//
// 「标签上该写什么」此前是 `title || url`，PDF 恰好暴露了它的缺陷：Chromium 的 PDF 页
// title 常常是空的 → 退化成整条百分号编码的长 URL（`file:///Users/…/%E4%B8%AD%E6%96%87…`），
// 用户完全认不出来是哪个文件。改为按 URL 形态分派（见 tabLabel）。
function fileUrlToPath(url: string): string {
  if (!url.startsWith('file://')) return ''
  try {
    return decodeURIComponent(url.slice('file://'.length))
  } catch {
    return '' // 非法编码：交给调用方走「非 file://」分支（宁可少显示，不要抛错）
  }
}

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
  const askQuestion = useAppStore((s) => s.question)
  const askRelease = useAppStore((s) => s.release)
  const uiModalOpen = inSettings || inSkills || inCron || showNewWsModal || !!askQuestion || !!askRelease

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
    const SEL = '.modal-hint.open, .settings-overlay, .img-zoom, .ctx-menu, .sel-popup, .settings-pop'
    const compute = () => {
      const panel = shotRef.current?.getBoundingClientRect()
      let hit = false
      for (const el of document.querySelectorAll(SEL)) {
        const r = el.getBoundingClientRect()
        if (r.width < 1 || r.height < 1) continue
        const overlaps = !panel || (r.left < panel.right && r.right > panel.left && r.top < panel.bottom && r.bottom > panel.top)
        if (overlaps) { hit = true; break }
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
function MainCompressionMirror() {
  const t = useT()
  const blocks = useAppStore((s) => s.blocks)
  const compressing = useAppStore((s) => s.compressing)
  const compressBefore = useAppStore((s) => s.compressBefore)
  const last = useMemo(() => [...blocks].reverse().find((b) => b.kind === 'compression'), [blocks])
  const showActive = compressing || Boolean(last)
  if (!showActive) return null
  return (
    <div className="rail-compression">
      {compressing ? (
        <div className="rail-compressing" role="status">
          {t('rp.task.mainCompressing').replace('{n}', String(compressBefore ?? '—'))}
        </div>
      ) : last?.kind === 'compression' ? (
        <CompressionBlock b={last} />
      ) : null}
    </div>
  )
}

// 异步任务监视器（右侧栏「任务」tab）：并发子 agent + 工具任务统一展示。
// 数据 = store 里 kind==='agent'（run_id 归属，输出按 SDK 事件 run_id 累计到卡片）
// 与 kind==='tool' 且带 taskId（含 promoted 后台任务）的块。子 agent 详细执行内容
// 能力保留在 AgentView（items 转录）。
// 顶部另有两块「提示」区（与本任务监视并列，不是任务）：
//   - 待审批：Session 统一 FIFO 队首（主/子共用，标注运行归属；与 Modal 共享 approve）
//   - 主 Agent 压缩镜像：主会话 CompressionBlock 同源展示
function TaskMonitor() {
  const t = useT()
  const blocks = useAppStore((s) => s.blocks)
  const tasksRunning = useAppStore((s) => runningTaskCount(s.blocks))
  const approvals = useAppStore((s) => s.approvals)
  const compressing = useAppStore((s) => s.compressing)
  const [open, setOpen] = useState(true)
  const tasks = asyncTaskBlocks(blocks)
  const head = approvals[0]
  const lastComp = useMemo(() => [...blocks].reverse().find((b) => b.kind === 'compression'), [blocks])
  const headOwner = (runId?: string): string => {
    if (!runId) return t('rp.task.mainAgent')
    const b = blocks.find((x) => x.kind === 'agent' && x.runId === runId)
    return b?.kind === 'agent' ? b.label : t('rp.task.subAgent')
  }
  if (tasks.length === 0 && !head && !compressing && !lastComp) {
    return <div className="empty"><div className="big">{t('rp.task.noTasks')}</div>{t('rp.task.noTasksHint')}</div>
  }
  // 运行中置顶（稳定排序），其余按出现顺序
  const sorted = [...tasks].sort((a, b) => (a.status === 'running' ? 0 : 1) - (b.status === 'running' ? 0 : 1))
  return (
    <div className="agent-monitor">
      {head && (
        <div className="rail-approval">
          <div className="rail-approval-hd">
            <span>{t('rp.task.pendingApproval').replace('{name}', head.name)}</span>
            <span className="mono rail-approval-owner">{headOwner(head.runId)}</span>
          </div>
          <ApprovalCard a={head} ownerLabel={headOwner(head.runId)} />
        </div>
      )}
      <MainCompressionMirror />
      <div
        className="am-hd"
        role="button"
        aria-expanded={open}
        tabIndex={0}
        onClick={() => setOpen((v) => !v)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault()
            setOpen((v) => !v)
          }
        }}
      >
        <span className="am-title">{t('rp.task.asyncTasks')}</span>
        <span className="am-count mono">{t('rp.task.runningCount').replace('{n}', String(tasksRunning)).replace('{m}', String(tasks.length))}</span>
        <span className="am-chev">{open ? '▾' : '▸'}</span>
      </div>
      {open && (
        <div className="am-list">
          {sorted.map((a) =>
            a.kind === 'agent'
              ? <AgentTaskCard key={a.id} b={a} />
              : a.kind === 'async_task'
                ? <AsyncTaskCard key={a.id} b={a} />
                : <ToolTaskCard key={a.id} b={a} />,
          )}
        </div>
      )}
    </div>
  )
}

// 面板级标签条（阶段 1，docs/RIGHT_PANEL_TABS_PLAN.md）：
// 把原来的「5 个固定 tab 按钮」换成**已打开的标签**列表（可切 / 可关 / 可重开）。
// 与折叠态 rail 的分工：rail = 启动器（5 类固定入口，收起态也能直接开标签，也是 e2e 的
// 点击入口 `.rbtn`）；标签条 = 已打开的标签。两者共用下面这份 kind → 图标注册表。
//
// 「+」菜单里**没有** file：文件标签需要路径，由点文件产生（阶段 2 起是**多实例**：
// 一个文件一个标签，同一文件的 diff / source 各一个 —— 见 store.openTab 的幂等键）。
const ADDABLE_KINDS: TabKind[] = ['activity', 'git', 'agent', 'web']
function kindIcon(kind: TabKind): ReactElement {
  switch (kind) {
    case 'git': return <GitBranch size={12} />
    case 'agent': return <Bot size={12} />
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
    if (!(rightExpanded && rightTab === 'web')) void window.desktop?.embedHide()
  }, [embeddedPresentation, rightExpanded, rightTab])

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
    if (!rightExpanded || rightTab !== 'web' || !pendingWebTabId) return
    const tab = useAppStore.getState().tabsByScope[scope]?.tabs.find((x) => x.id === pendingWebTabId)
    if (tab && isPendingWebTab(tab)) rebuildWebTab(tab)
  }, [rightExpanded, rightTab, pendingWebTabId, scope])

  const todos = useAppStore((s) => s.todos)
  // M6：网页模式的沉浸态——收起**浏览器自己的工具条**（.fp-hd）把垂直空间让给内嵌页面；
  // ☰ 一键唤回。注意：**面板级标签条不再随之隐藏**（阶段 1 起它就是唯一的标签入口，
  // 隐藏它等于切不走标签）。
  // 默认**非沉浸**：阶段 1 之前 web tab 默认隐藏面板 tab 行（M6 的「让垂直空间给页面」），
  // 现在面板标签条常驻、浏览器工具条又被合并成一行 → 默认收起它只会让用户看不到地址栏/文件名。
  const [immersive, setImmersive] = useState(false)
  const toolbarHidden = rightTab === 'web' && immersive
  // 活跃待办数（未完成）→ 折叠图标条角标
  const activeTodos = todos.filter((todo) => !todo.done).length
  // 运行中的异步任务数（子 agent + 工具任务，随当前会话镜像）→ 折叠 rail / tab 角标
  const runningTasks = runningTaskCount(useAppStore((s) => s.blocks))

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
      return base || kindLabel('file')
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
                data-tip={tab.key === 'agent' && runningTasks > 0 ? t('rp.agent.asyncTip').replace('{n}', String(runningTasks)) : tab.label}
                aria-label={tab.label}
                onClick={() => {
                  setRightTab(tab.key)
                  setRightExpanded(true) // 点击图标 → 展开 + 切到该 tab（此前只切换不展开 = 点不动）
                }}>
                {unread > 0 && <span className="todo-badge unread-badge mono">{unread}</span>}
                {tab.icon}
                {tab.key === 'agent' && runningTasks > 0 && (
                  <span className="todo-badge agent-badge mono">{runningTasks}</span>
                )}
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
                  title={restoring ? t('browser.tabRestoring') : sleeping ? t('browser.tabSleeping') : (webTabUrl(tab) || tabTitle(tab))}
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
                  {tab.kind === 'agent' && runningTasks > 0 && <span className="tab-count mono">{runningTasks}</span>}
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
          {activeTab?.kind === 'git' && <ChangesTab />}
          {activeTab?.kind === 'agent' && <TaskMonitor />}
          {/* key={activeTab.id}：**每个 file 标签一份组件实例**（阶段 2 的隔离手段）。
              为什么不共用一个实例再按 props 复位：file 标签之间切换时元素类型/位置不变，
              React 会复用实例 → 组件 state（mdMode/csvMode/detailMode/列表展开…）会从上一个
              标签**泄漏**到下一个标签（用户在 A 切了「纯文本」，切到 B 却看到纯文本）。
              加 key 后每个标签的状态天然各归各的；滚动位置另存（见 FilePreviewTab 的
              fileTabScroll —— 重挂载会丢 DOM 的 scrollTop）。 */}
          {activeTab?.kind === 'file' && <FilePreviewTab key={activeTab.id} tab={activeTab} />}
          {activeTab?.kind === 'web' && <BrowserTab toolbarHidden={toolbarHidden} onToggleTabs={() => setImmersive((v) => !v)} menuOpen={!!menu || !!ctx} panelVisible={rightExpanded} activePageId={activeTab?.kind === 'web' ? activeTab.pageId : undefined} />}
        </div>
      </div>
    </aside>
  )
}
