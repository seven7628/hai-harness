import { useCallback, useEffect, useRef, useState } from 'react'
import { Group, Panel, Separator, usePanelCallbackRef } from 'react-resizable-panels'
import LeftPanel from './components/LeftPanel'
import CenterPanel from './components/CenterPanel'
import RightPanel from './components/RightPanel'
import Statusbar from './components/Statusbar'
import SkillsPage from './components/SkillsPage'
import CronPage from './components/CronPage'
import CronRunsPage from './components/CronRunsPage'
import CronRunDetailPage from './components/CronRunDetailPage'
import IMBindConfirmModal from './components/IMBindConfirmModal'
import SandboxReleaseModal from './components/SandboxReleaseModal'
import NewWorkspaceModal from './components/NewWorkspaceModal'
import SettingsModal from './components/SettingsModal'
import ToastView from './components/ToastView'
import BootScreen from './components/BootScreen'
import HubSurface from './hub/HubSurface'
import ArtifactPreview from './hub/ArtifactPreview'
import HubInspector from './hub/HubInspector'
import AgentEditor from './hub/AgentEditor'
import './hub/hub.css'
import { attachHubEvents, useHubStore } from './hub/useHubStore'
import { setHubOwnerContext } from './lib/activeOwner'
import BridgeBanner from './components/BridgeBanner'
import { useAppStore } from './store/useAppStore'
import type { RightLayout } from './lib/layout'
import { getTransport } from './transport'
import { applyTheme } from './lib/theme'
import { applyNarrativeDensity, applyNarrativeSkin } from './lib/narrativeSkin'
import { applyAppearance, useAppearance } from './lib/appearance'

// 三栏面板的稳定 id：Group.onLayoutChanged 的 layout 以 id 为键（`{[id]: percentage}`）——
// 不显式给 id 就只能拿到库自动生成的键，读不到「右栏占多少」。拖拽时用它换算成
// 右栏占比（见 onLayoutChanged → setRightLayoutFromDrag）。
const LEFT_PANEL_ID = 'left'
const RIGHT_PANEL_ID = 'right'
// Hub 的「窄窗」门槛：<1000 时左栏是 Hub 的群/DM 导航，进 Hub 要把它顶开（否则只剩
// 44px 图标条，选不了会话 —— P4 审计 HUB-N-01）。
// ⚠️ 它**不再**决定 inspector 是否自动展开：Hub 里右栏默认收起（用户报「智能体模式右侧
// 占比太大」），宽窗窄窗一致，要看详情点 44px 图标条（.hub-detail-rail）。
const HUB_INSPECTOR_MIN_W = 1000
// Hub 内展开 inspector 的目标宽度（与右栏 Panel 的 defaultSize 一致）。
const HUB_INSPECTOR_W = 360
// Hub 内右栏的宽度上限（px）：1280 下 420 = 32.8%，不到一半。库的 maxSize 只认 px/%/rem
// 这类单值（没有 calc/min()），所以「420 与 40% 取更小」由下面的 hubRightMaxSize 自己算。
const HUB_INSPECTOR_MAX_W = 420
// 右栏 Panel 的 minSize：上限不能算得比它还小，否则库会把展开宽度夹到 max（反而更窄）。
const RIGHT_PANEL_MIN_W = 200
// 右栏在一个窗口里的物理上限 ≈ 窗口 - 左栏图标条 44 - 中心列下限 300 - 两条 4px 分隔条。
const rightRoomPx = (winW: number): number => winW - 44 - 300 - 8
// Hub 内右栏的 maxSize：≥1000 取「420px 与窗口 40% 的较小者」（1280 下 420 = 32.8%，不到
// 一半）；<1000 只留 420px 硬上限。窗口窄到连 420 都够不着（room ≤ 420，如 640 下只有
// 288）就返回 '80%' 不设限 —— 库在「imperative resize 的目标 > maxSize」时会算出非法布局
// 然后整条拒绝，实测 640 下把 maxSize 设成 420 会让右栏连手动展开都做不到。够不着上限时
// 不设它，420 的承诺依然成立（能到的最宽就是 room）。
const hubRightMaxSize = (): number | '80%' => {
  const winW = window.innerWidth
  if (rightRoomPx(winW) <= HUB_INSPECTOR_MAX_W) return '80%'
  const cap = winW >= HUB_INSPECTOR_MIN_W ? Math.min(HUB_INSPECTOR_MAX_W, Math.round(winW * 0.4)) : HUB_INSPECTOR_MAX_W
  return Math.max(RIGHT_PANEL_MIN_W, cap)
}
import { useRailTips } from './lib/railTips'

export default function App() {
  const inSkills = useAppStore((s) => s.inSkills)
  const inCron = useAppStore((s) => s.inCron)
  const cronDetailOpen = useAppStore((s) => s.cronDetailOpen)
  const cronRunsJob = useAppStore((s) => s.cronRunsJob)
  const leftExpanded = useAppStore((s) => s.leftExpanded)
  const rightExpanded = useAppStore((s) => s.rightExpanded)
  const setLeftExpanded = useAppStore((s) => s.setLeftExpanded)
  const setRightExpanded = useAppStore((s) => s.setRightExpanded)
  const leftRef = usePanelCallbackRef()
  const rightRef = usePanelCallbackRef()
  // 窄窗进 Hub：左栏的展开要排在 inspector 收起之后（见下面 Hub 打开 effect）。
  const pendingNarrowLeftExpand = useRef(false)
  // —— 面板「实际」折叠态（渲染依据；store 只是用户意图）——
  // v4 的 Panel 没有 controlled collapsed prop：窗口太窄时库会拒绝 expand，此时 store
  // 仍是 true 而面板只有 44px。过去 LeftPanel 直接按 store 渲染，于是 44px 容器里塞
  // 展开内容、群/DM 列表被挤成竖排单字（P4 审计 HUB-N-01）。两栏都从 onResize 回写真实
  // 状态：class/内容按真实宽度渲染，store 同步跟随（供快捷键、持久化等其它消费者用）。
  const [leftCollapsed, setLeftCollapsed] = useState(() => !useAppStore.getState().leftExpanded)
  const [rightCollapsed, setRightCollapsed] = useState(() => !useAppStore.getState().rightExpanded)
  const applyLeftPanelState = useCallback((collapsed: boolean): void => {
    setLeftCollapsed(collapsed)
    setLeftExpanded(!collapsed)
  }, [setLeftExpanded])
  // 启动 splash：每次启动先播品牌时刻动画，播完由 BootScreen 决定去向
  //（有持久工作区 → 自动恢复进入；无 → 显示选目录）。只启动一次，之后不再回启动屏。
  // 进入主界面 = 交叉淡化：BootScreen 内容淡出+轻微放大，同时 main 挂载并淡入上浮
  //（mainReady 在 leaving 时再挂——避免启动动画期间渲染重面板卡掉逐笔描画；
  //  LEAVE_MS 与 index.css `.app-root.leaving` 的过渡时长保持一致）。
  const LEAVE_MS = 360
  const [splash, setSplash] = useState(true) // BootScreen 是否仍挂载
  const [leaving, setLeaving] = useState(false) // 离开淡化已开始
  const [mainReady, setMainReady] = useState(false) // main 已挂载（leaving 起挂）

  // 主题：store settings.theme → <html data-theme> + Electron 窗口底色（mount 即应用，切换实时）
  // 会话页样式（data-skin）与主题正交，共用这条 effect（切换实时；缺省 current）
  const theme = useAppStore((s) => s.settings.theme)
  const narrativeSkin = useAppStore((s) => s.settings.narrative_skin ?? 'current')
  const narrativeDensity = useAppStore((s) => s.settings.narrative_density ?? 'concise')
  // TEMP-STARTUP 打点单独一次（挂载时）：它与"主题/皮肤"无关，
  // 混在同一条 effect 里会让用户切一次皮肤就往启动时间轴里多写一条 app-mounted。
  useEffect(() => {
    if (window.desktop) void window.desktop.startupMark('app-mounted')
  }, [])
  // 办公组件状态（R08d）：预览面板横幅与设置页共用这一份；启动查一次，之后只在用户
  // 点「重新探测」时刷新（安装/卸载发生在 app 之外，自动轮询没有意义）。
  const fetchOfficeKit = useAppStore((s) => s.fetchOfficeKit)
  useEffect(() => {
    void fetchOfficeKit()
  }, [fetchOfficeKit])
  // 外观偏好（代码主题 / 字体大小 / 亮色底色）在这里订阅：改设置即时落 <html> +
  // 通知 Electron 缩放；顺带覆盖「切换主题」这一路径 —— 底色覆盖只能在 paper 下挂，
  // 切到 ink 必须由同一次 effect 清掉（inline 变量优先级高于 [data-theme] 规则）。
  const appearance = useAppearance()
  useEffect(() => {
    applyTheme(theme)
    applyNarrativeSkin(narrativeSkin)
    applyNarrativeDensity(narrativeDensity)
    applyAppearance(appearance) // 必须在 applyTheme 之后：它读的是解析后的 data-theme
    if (window.desktop) void window.desktop.applyTheme(theme)
  }, [theme, narrativeSkin, narrativeDensity, appearance])

  // 折叠图标条 tooltip 定位（fixed，避开图标条 overflow 裁剪；左右 rail 共用一次委托）
  useRailTips()

  // R0：把当前会话同步给 Electron 侧视图池（可见性按会话过滤）。放在 App 常驻层是因为
  // 会话切换有多条路径（setActiveSession 两分支 / 切工作区 / 删会话后顺延），
  // 在 store 里逐处调用会漏；订阅 activeSessionId 一处覆盖全部。
  const activeSessionId = useAppStore((s) => s.activeSessionId)
  useEffect(() => {
    void window.desktop?.embedSetSession?.(activeSessionId ?? '')
  }, [activeSessionId])

  // 全局快捷键：⌘B 左栏 / ⌘\ 右栏（对齐设置面板的宣告）；⌘1..5 切换最近打开的会话
  const toggleLeft = useAppStore((s) => s.toggleLeft)
  const toggleRight = useAppStore((s) => s.toggleRight)
  const recentSessions = useAppStore((s) => s.recentSessions)
  const setActiveSession = useAppStore((s) => s.setActiveSession)
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (!(e.metaKey || e.ctrlKey)) return
      const k = e.key.toLowerCase()
      if (k === 'b') {
        e.preventDefault()
        toggleLeft()
      } else if (k === '\\') {
        e.preventDefault()
        toggleRight()
      } else if (k >= '1' && k <= '9' || k === '0') {
        // ⌘1..9 / ⌘0：切换到最近会话队列第 N 个（1-9、0=第10；无则忽略）
        const idx = k === '0' ? 9 : Number(k) - 1
        const r = useAppStore.getState().recentSessions[idx]
        if (r) {
          e.preventDefault()
          setActiveSession(r.id, r.path)
        }
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [toggleLeft, toggleRight, setActiveSession, recentSessions])

  // 展开/折叠：store 状态 → 命令式 collapse/expand（v4 无 controlled collapsed prop）。
  // 先取 handle 再进 effect：依赖数组里放变量而不是 leftRef[0] 这种复杂表达式，
  // 否则 exhaustive-deps 既报「复杂表达式」又漏检（handle 变了 effect 不会重跑）。
  const leftHandle = leftRef[0]
  const rightHandle = rightRef[0]
  useEffect(() => {
    const h = leftRef[0]
    if (!h) return
    if (leftExpanded && h.isCollapsed()) {
      h.expand()
      // expand 会被空间约束拒绝（库直接保持 collapsedSize）：下一帧核对真实状态并回写，
      // 别让 class/内容与面板分叉。窄窗进 Hub 时正在等 inspector 让位，交给重试那一步。
      const raf = requestAnimationFrame(() => {
        if (pendingNarrowLeftExpand.current) return
        applyLeftPanelState(h.isCollapsed())
      })
      return () => cancelAnimationFrame(raf)
    }
    if (!leftExpanded && !h.isCollapsed()) h.collapse()
  }, [leftExpanded, leftRef[0], applyLeftPanelState])

  useEffect(() => {
    const h = rightRef[0]
    if (!h) return
    if (rightExpanded && h.isCollapsed()) {
      h.expand()
      // 同理：窄窗（<1000）展开右栏会被中心列 300px 下限拒绝，回到 44px 图标条而不是
      // 在 44px 里画详情。
      const raf = requestAnimationFrame(() => {
        const collapsed = h.isCollapsed()
        setRightCollapsed(collapsed)
        if (collapsed) setRightExpanded(false)
      })
      return () => cancelAnimationFrame(raf)
    }
    if (!rightExpanded && !h.isCollapsed()) h.collapse()
  }, [rightExpanded, rightRef[0], setRightExpanded])
  const hubOpen = useHubStore((s) => s.open)
  // Hub 期间右栏的宽度上限（见 hubRightMaxSize）。窗口宽窄变了要重算：否则 1024 下还按
  // 1280 算，420 就成了 41%。退出 Hub 后回到 '80%'，不影响工作区的拖拽范围。
  const [hubRightMax, setHubRightMax] = useState(hubRightMaxSize)
  // 三态布局（PR-01）：预设 bias 时把右栏 resize 到目标比例；custom 由用户拖拽决定，不回写。
  const rightLayout = useAppStore((s) => s.rightLayout)
  const rightSize = useAppStore((s) => s.rightSize)
  const setRightLayout = useAppStore((s) => s.setRightLayout)
  const setRightLayoutFromDrag = useAppStore((s) => s.setRightLayoutFromDrag)
  useEffect(() => {
    // Hub 期间 inspector 的宽度由 Hub 自己管（宽窗 HUB_INSPECTOR_W）。若让 even 预设
    // 在这里生效，1280 下右栏会从 360 撑到可用宽度的 50%（~500），会话列被压窄。
    if (hubOpen) return
    // rightSize（内嵌浏览器 viewport）优先级更高：它在时本轮不动宽度，避免两个 effect 抢写
    if (!rightHandle || !leftHandle || rightSize != null) return
    if (!rightLayout.open || rightLayout.bias === 'custom' || rightHandle.isCollapsed()) return
    const availPct = Math.max(1, 100 - leftHandle.getSize().asPercentage)
    rightHandle.resize(((availPct * rightLayout.ratio) / 100).toFixed(2) + '%')
  }, [hubOpen, rightLayout, rightSize, rightHandle, leftHandle])

  // M3：rightSize 非空时把右侧面板 resize 到期望宽度（浏览器内嵌按 viewport 设）。
  useEffect(() => {
    if (!rightHandle || rightSize == null || rightHandle.isCollapsed()) return
    rightHandle.resize(rightSize)
  }, [rightSize, rightHandle])

  // hubRightMax 的重算：只跟 Hub 开关与窗口宽度有关，挂 listener 比每帧读 innerWidth 稳。
  useEffect(() => {
    if (!hubOpen) return
    const onResize = (): void => setHubRightMax(hubRightMaxSize())
    onResize()
    window.addEventListener('resize', onResize)
    return () => window.removeEventListener('resize', onResize)
  }, [hubOpen])

  // Hub 内展开 inspector：宽度落回设计值 360（用户报「展开后仍占太大」）。折叠→展开、
  // 点 rail 的概览/任务/Needs/Context 图标都走这里 —— expand() 只会恢复「上次尺寸」，
  // 工作区里拖宽过的右栏会被原样带进 Hub。
  // 依赖只有这三项：把 rightSize/尺寸也放进来会在拖拽过程中每帧把宽度拉回 360。
  useEffect(() => {
    if (!hubOpen || !rightExpanded || !rightHandle || rightHandle.isCollapsed()) return
    const raf = requestAnimationFrame(() => {
      // 窄窗下 expand() 可能被中心列 300px 下限拒绝（上面那条 effect 会把 store 回写成
      // false）：仍是图标条就别 resize —— resize 会把 44px 的折叠面板重新撑开。
      if (rightHandle.isCollapsed()) return
      rightHandle.resize(HUB_INSPECTOR_W)
    })
    return () => cancelAnimationFrame(raf)
  }, [hubOpen, rightExpanded, rightHandle])

  const hubAlerts = useHubStore((s) => s.alerts)
  // 归属（owner）上卷：Hub 的 DM session_id / 群 id 只在 useHubStore 里，而
  // useAppStore.send() 是非 React 路径 —— 这里单点写入中立模块（lib/activeOwner），
  // 让「设置页显示给谁」与「命令实际写给谁」用同一份派生（B1）。
  const hubOwnerSid = useHubStore((s) => s.sid)
  const hubOwnerGroup = useHubStore((s) => s.activeGroupId)
  useEffect(() => {
    setHubOwnerContext({ open: hubOpen, sid: hubOwnerSid, groupId: hubOwnerGroup })
  }, [hubOpen, hubOwnerSid, hubOwnerGroup])
  const openHub = useHubStore((s) => s.openHub)
  const hubEditor = useHubStore((s) => s.editor)
  const closeEditor = useHubStore((s) => s.closeEditor)
  const openAgent = useHubStore((s) => s.openAgent)
  const clearHubSelection = useHubStore((s) => s.clearSelection)
  // 进入 Hub 时右栏（inspector）**一律收起**成 44px 图标条：Hub 的主角是会话列，宽窗下
  // 强开 360px 详情会把会话列压到 ~680px（用户报「右侧占比太大、本该默认收起」）。
  // 宽窄窗只差左栏：<1000 时左栏是群/DM 导航，要等右栏让出宽度后再顶开（两步，见下）。
  // 只在打开的那一次强制，用户随后手动展开不被反复收起（折叠态由 HubInspector 自己提供
  // 图标条）。退出 Hub 时恢复进入前的折叠状态：Hub 是临时视图，不应改变工作区的布局偏好。
  const wasHubOpen = useRef(false)
  const rightExpandedBeforeHub = useRef<boolean | null>(null)
  // 进入 Hub 前的三态布局与进入后自动写成的布局：退出时只还原前者，且仅在用户没在
  // Hub 里自己拖过比例时（cur === auto）—— Hub 是临时视图，不该改写工作区布局偏好。
  const rightLayoutBeforeHub = useRef<RightLayout | null>(null)
  const rightLayoutAutoHub = useRef<RightLayout | null>(null)
  const leftExpandedBeforeHub = useRef<boolean | null>(null)
  const [narrowHubEntry, setNarrowHubEntry] = useState(false)
  // 退出 Hub 的第二步（P0 复审 #1）：640 从 rail 进 Hub → 点 inspector 图标（右栏撑开、
  // 左栏被挤成 44px）→ 退出 Hub。进 Hub 前左栏是 expanded，但「右栏收起」与「左栏展开」
  // 同一帧下发时，库按当时的空间结算，expand 被拒绝，左栏就永久停在 44px。
  // 这里先让右栏让位，等 onResize 回写出 rightCollapsed 再恢复左栏 —— 与窄窗进 Hub
  // 复用同一条「两步 + 回写核对」办法。
  const pendingExitLeftExpand = useRef<boolean | null>(null)
  const [exitLeftRestore, setExitLeftRestore] = useState(false)
  // Hub 打开期间跨过 1000px 阈值（640↔1280 拖窗口）：默认收起之后只保留「收窄让位」。
  // 变宽不自动展开（Hub 默认图标条）；用户手动动过右栏就把控制权交还给人
  // （autoRight 置空），本次 Hub 会话不再自动收起。
  const autoRight = useRef<{ wide: boolean; expanded: boolean } | null>(null)
  // P0 复审 #2：窄窗下右栏展开会把左栏挤成 44px，Hub 的群/DM 导航跟着消失。
  // 实测关掉 inspector 之后左栏仍停在 44px（库只在窗口变宽时才给回宽度），
  // 用户只能退出 Hub 才找得回来。这里记下「左栏是被右栏挤掉的」，等右栏让位再恢复。
  const leftSqueezedInHub = useRef(false)
  const prevLeftCollapsed = useRef(leftCollapsed)
  // 恢复左栏的「真展开」：store 可能已经是 true（库把面板收成 44px 时回写过），
  // 只 setLeftExpanded 不会触发左侧 effect，必须自己 expand 并回写核对真实宽度。
  const restoreLeftPanel = useCallback((expanded: boolean): void => {
    setLeftExpanded(expanded)
    const h = leftRef[0]
    if (!h) return
    if (expanded && h.isCollapsed()) h.expand()
    if (!expanded && !h.isCollapsed()) h.collapse()
    requestAnimationFrame(() => applyLeftPanelState(h.isCollapsed()))
  }, [leftRef[0], setLeftExpanded, applyLeftPanelState])
  useEffect(() => {
    if (hubOpen && !wasHubOpen.current) {
      const st = useAppStore.getState()
      rightExpandedBeforeHub.current = st.rightExpanded
      rightLayoutBeforeHub.current = st.rightLayout
      leftExpandedBeforeHub.current = null
      if (window.innerWidth < HUB_INSPECTOR_MIN_W) {
        // 窄窗：先收 inspector。左栏是 Hub 的群/DM 导航，必须给它真实宽度（否则只剩
        // 44px 图标条，选不了会话）。展开要等右栏真的收起来 —— 同一帧内 expand 会因
        // 空间不足被库拒绝，这也是 HUB-N-01 的成因。
        leftExpandedBeforeHub.current = st.leftExpanded
        pendingNarrowLeftExpand.current = true
        setNarrowHubEntry(true)
      }
      setRightExpanded(false)
      autoRight.current = { wide: window.innerWidth >= HUB_INSPECTOR_MIN_W, expanded: false }
      rightLayoutAutoHub.current = useAppStore.getState().rightLayout
    } else if (!hubOpen && wasHubOpen.current) {
      const beforeRight = rightExpandedBeforeHub.current
      const beforeLeft = leftExpandedBeforeHub.current
      const beforeLayout = rightLayoutBeforeHub.current
      const autoLayout = rightLayoutAutoHub.current
      // 用户没在 Hub 里拖过分隔条时才还原比例；拖过就尊重这次显式操作。
      const layoutUntouched = beforeLayout != null && autoLayout != null
        && JSON.stringify(useAppStore.getState().rightLayout) === JSON.stringify(autoLayout)
      rightExpandedBeforeHub.current = null
      rightLayoutBeforeHub.current = null
      rightLayoutAutoHub.current = null
      leftExpandedBeforeHub.current = null
      pendingNarrowLeftExpand.current = false
      setNarrowHubEntry(false)
      autoRight.current = null
      if (beforeRight != null) setRightExpanded(beforeRight)
      if (layoutUntouched && beforeLayout) setRightLayout(beforeLayout)
      if (beforeLeft != null) {
        // 右栏此刻还占着宽度（Hub 里点开过 inspector）→ 先让它收起来，下一帧再恢复左栏。
        const rightNowCollapsed = rightRef[0]?.isCollapsed() ?? rightCollapsed
        if (beforeRight === false && !rightNowCollapsed) {
          pendingExitLeftExpand.current = beforeLeft
          setExitLeftRestore(true)
        } else {
          restoreLeftPanel(beforeLeft)
        }
      }
    }
    wasHubOpen.current = hubOpen
  }, [hubOpen, setRightExpanded, setRightLayout, restoreLeftPanel, rightCollapsed])

  // 窄窗进 Hub 的第二步：听右栏「真实」折叠态（onResize 回写），让出宽度后再展开左栏。
  useEffect(() => {
    if (!narrowHubEntry || !rightCollapsed) return
    setNarrowHubEntry(false)
    pendingNarrowLeftExpand.current = false
    setLeftExpanded(true)
    const h = leftRef[0]
    if (!h) return
    if (h.isCollapsed()) h.expand()
    // store 本来就是 true 时上面不会触发左侧 effect，这里自己核对一次：extreme 窄窗下
    // 左栏仍可能被拒绝展开，那就回写成图标条，别留下「store 说展开、实际 44px」。
    // 本 effect 会随即改 narrowHubEntry 触发重跑，因此不返回 cleanup（cleanup 会取消这一帧）。
    requestAnimationFrame(() => applyLeftPanelState(h.isCollapsed()))
  }, [narrowHubEntry, rightCollapsed, leftRef[0], setLeftExpanded, applyLeftPanelState])
  // 退出 Hub 的第二步：等右栏真的收成图标条（onResize 回写 rightCollapsed）再恢复左栏。
  useEffect(() => {
    if (!exitLeftRestore || !rightCollapsed) return
    setExitLeftRestore(false)
    const want = pendingExitLeftExpand.current
    pendingExitLeftExpand.current = null
    if (want != null) restoreLeftPanel(want)
  }, [exitLeftRestore, rightCollapsed, restoreLeftPanel])
  // Hub 里「左栏被右栏挤成 44px」的识别：只在左栏由展开→收起、且此刻右栏是展开的时候
  // 记账（用户自己按 ⌘B 收左栏时右栏通常不在展开态）。Hub 关闭即作废。
  useEffect(() => {
    const was = prevLeftCollapsed.current
    prevLeftCollapsed.current = leftCollapsed
    if (!hubOpen) {
      leftSqueezedInHub.current = false
      return
    }
    if (!was && leftCollapsed && rightExpanded) leftSqueezedInHub.current = true
  }, [leftCollapsed, rightExpanded, hubOpen])

  // 右栏让位（收起 / 窗口变宽）后把被挤掉的左栏恢复：Hub 打开期间不许把群/DM 导航弄丢
  // （P0 复审 #2）。恢复走 restoreLeftPanel：会回写核对真实宽度，不会再留「store 说展开、实际 44px」。
  useEffect(() => {
    if (!hubOpen || !rightCollapsed || !leftCollapsed) return
    if (!leftSqueezedInHub.current) return
    leftSqueezedInHub.current = false
    restoreLeftPanel(true)
  }, [hubOpen, rightCollapsed, leftCollapsed, restoreLeftPanel])

  // 兜底：右栏没在 400ms 内回写（异常路径）也要恢复，别把左栏永久留在 44px 图标条。
  useEffect(() => {
    if (!exitLeftRestore) return
    const timer = window.setTimeout(() => {
      setExitLeftRestore(false)
      const want = pendingExitLeftExpand.current
      pendingExitLeftExpand.current = null
      if (want != null) restoreLeftPanel(want)
    }, 400)
    return () => window.clearTimeout(timer)
  }, [exitLeftRestore, restoreLeftPanel])

  // Hub 打开期间窗口跨过 1000px 阈值：默认收起之后，这里只剩「交还控制权」与「收窄让位」。
  //   · 变宽不自动展开：Hub 默认就是图标条，用户要看详情自己点（原来 640→1280 会自动
  //     顶开 360px inspector，正是用户报的「右侧占比太大」）。
  //   · 变窄仍可自动收起：给会话列让位。
  //   · 用户手动动过右栏（store 与 autoRight 记录不一致）就置空 autoRight，本次 Hub 会话
  //     不再自动收 —— 否则刚点开的 inspector 会在拖窗口时被莫名收掉。
  useEffect(() => {
    if (!hubOpen) return
    const auto = (): { wide: boolean; expanded: boolean } | null => autoRight.current
    const apply = (): void => {
      const a = auto()
      if (!a) return
      const wide = window.innerWidth >= HUB_INSPECTOR_MIN_W
      if (wide === a.wide) return
      if (useAppStore.getState().rightExpanded !== a.expanded) {
        autoRight.current = null
        return
      }
      // 记下新的默认态（永远是收起）。变宽到这里就结束 —— 不再 expand。
      autoRight.current = { wide, expanded: false }
      if (wide) return
      // 收窄方向：右栏让位之后，被挤掉的左栏由上面的 squeezed effect 恢复。
      setRightExpanded(false)
    }
    // 收窄要立刻让位（延迟会让库按旧宽度把左栏挤掉）；变宽方向 apply 是空操作，不必等排版。
    const onResize = (): void => {
      const a = auto()
      if (a && a.wide && window.innerWidth < HUB_INSPECTOR_MIN_W) apply()
    }
    apply()
    window.addEventListener('resize', onResize)
    return () => window.removeEventListener('resize', onResize)
  }, [hubOpen, setRightExpanded])

  // App 级订阅：离开 Hub 页期间产生的结算/告警也要收进告警中心（P2.5-05 复审 B3）。
  useEffect(() => attachHubEvents(), [])
  // hub:// 深链（P4-02）：主进程 open-url / second-instance 转发过来 → 打开 Hub 并定位。
  // 订阅放在 App 级：Hub 关着时点链接同样要能进去（mock/browser 下是空实现）。
  useEffect(() => {
    const transport = getTransport()
    const handle = (link: string): void => void useHubStore.getState().openDeepLink(link)
    // 先订阅再取队列：两步之间到达的链接由主进程排队，不会漏（见 electron/main.ts）。
    const off = transport.onHubDeepLink(handle)
    void transport.takeHubDeepLinks().then((links) => {
      for (const link of links) handle(link)
    })
    return off
  }, [])
  return (
    <div className={`app-root ${inSkills ? 'in-skills' : ''}${inCron ? 'in-cron' : ''}${leaving ? ' leaving' : ''}`}>
      {mainReady && <BridgeBanner />}
      {mainReady && (
        <main className="main">
          {/* Hub 打开时**不卸载**工作区中栏/右栏：composer 草稿、附件、右栏标签与
              滚动位置都是组件本地 state，卸载即丢（P1-08 复核 B1）。
              只把对应 host 设 display:none（见 .hub-host-stack），面板尺寸保持不变。 */}
          <Group
            orientation="horizontal"
            className="main-group"
            // 用户拖分隔条 → 记成布局比例（custom 或吸附到预设）；**不再自动收起左栏**
            //（三态布局里右栏 >50% 是合法状态，D-A 的自动展开只属于 openTab，不参与拖拽）
            onLayoutChanged={(layout, meta) => {
              if (!meta.isUserInteraction) return
              if (!rightLayout.open) return // 收起态拖左/中分隔条不该把右栏弹开（D-A）
              // 只传 layout 百分比：它与 getSize().inPixels 的分母不同（后者不含分隔条），
              // 混用会把窄拖拽（右栏 < 窗口 25%）当成抖动丢掉。去重在 store 里按 ratio 做。
              setRightLayoutFromDrag(layout[RIGHT_PANEL_ID] ?? 0, layout[LEFT_PANEL_ID] ?? 0)
            }}
          >
            <Panel
              id={LEFT_PANEL_ID}
              panelRef={leftRef[1]}
              collapsible
              collapsedSize={44}
              defaultSize={236}
              minSize={140}
              maxSize={360}
              onResize={(_size, _id, prev) => {
                if (!prev) return // 挂载首报不覆盖 store 初始态
                const h = leftRef[0]
                if (!h) return
                applyLeftPanelState(h.isCollapsed())
              }}
            >
              <LeftPanel collapsed={leftCollapsed} />
            </Panel>
            <Separator className="resize-handle" />
            <Panel minSize={300}>
              <div className={`hub-host-stack ${hubOpen ? 'hub-on' : ''}`}>
                <div className="hub-host-cell workspace-cell">
                  <CenterPanel />
                </div>
                {hubOpen ? (
                  <div className="hub-host-cell hub-cell">
                    <HubSurface />
                  </div>
                ) : null}
              </div>
            </Panel>
            <Separator className="resize-handle" />
            <Panel
              id={RIGHT_PANEL_ID}
              panelRef={rightRef[1]}
              collapsible
              collapsedSize={44}
              defaultSize={360}
              minSize={RIGHT_PANEL_MIN_W}
              // Hub 期间右栏只是「详情」：上限 420px（且 ≤40%，见 hubRightMaxSize），
              // 不许再吃到半屏；工作区仍是原来的 80%，拖拽范围不变。
              maxSize={hubOpen ? hubRightMax : '80%'}
              // 只做「展开/收起」的镜像；「右栏 >50% 收左栏」的规则在 Group.onLayoutChanged
              //（那里才有 meta.isUserInteraction，能区分用户拖拽与程序化 resize）。
              onResize={(_size, _id, prev) => {
                if (!prev) return // 挂载首报不覆盖 store 初始态
                const collapsed = rightRef[0]?.isCollapsed() ?? false
                setRightCollapsed(collapsed)
                setRightExpanded(!collapsed)
              }}
            >
              <div className={`hub-host-stack ${hubOpen ? 'hub-on' : ''}`}>
                <div className="hub-host-cell workspace-cell">
                  <RightPanel />
                </div>
                {hubOpen ? (
                  <div className="hub-host-cell hub-cell">
                    <HubInspector />
                  </div>
                ) : null}
              </div>
            </Panel>
          </Group>
        </main>
      )}
      {splash && (
        <BootScreen
          onEnter={() => {
            setLeaving(true)
            setMainReady(true)
            window.setTimeout(() => setSplash(false), LEAVE_MS)
          }}
        />
      )}
      <SkillsPage />
      {inCron && (cronDetailOpen ? <CronRunDetailPage /> : cronRunsJob ? <CronRunsPage /> : <CronPage />)}
      {!hubOpen && hubAlerts.length > 0 ? (
        <button className="hub-global-alert" onClick={openHub} title={hubAlerts[0].message || hubAlerts[0].code}>
          <span className={`hub-alert-dot ${hubAlerts[0].severity ?? 'warning'}`} />
          <span>Hub 告警 {hubAlerts.length > 1 ? `(${hubAlerts.length})` : ''}</span>
          <span className="hub-global-alert-text">{hubAlerts[0].message || hubAlerts[0].code}</span>
        </button>
      ) : null}
      {/* Hub 现在是 App shell 的一部分，状态栏常驻（工作区与 Hub 共用同一条底栏）。 */}
      <Statusbar />
      {hubEditor ? (
        <AgentEditor
          agent={hubEditor === 'new' ? null : hubEditor}
          onClose={closeEditor}
          onSaved={(id) => {
            closeEditor()
            void useHubStore.getState().refresh()
            // 新建/删除返回 id / ''：新建后直接进它的会话；删除后清空右栏概览。
            if (id) void openAgent(id)
            else clearHubSelection()
          }}
        />
      ) : null}
      <ArtifactPreview />
      <SandboxReleaseModal />
      <NewWorkspaceModal />
      <IMBindConfirmModal />
      <SettingsModal />
      <ToastView />
    </div>
  )
}
