import { useEffect, useRef, useState } from 'react'
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
import HubInspector from './hub/HubInspector'
import AgentEditor from './hub/AgentEditor'
import './hub/hub.css'
import { attachHubEvents, useHubStore } from './hub/useHubStore'
import { setHubOwnerContext } from './lib/activeOwner'
import BridgeBanner from './components/BridgeBanner'
import { useAppStore } from './store/useAppStore'
import { getTransport } from './transport'
import { applyTheme } from './lib/theme'
import { applyNarrativeDensity, applyNarrativeSkin } from './lib/narrativeSkin'
import { applyAppearance, useAppearance } from './lib/appearance'

// 三栏面板的稳定 id：Group.onLayoutChanged 的 layout 以 id 为键（`{[id]: percentage}`）——
// 不显式给 id 就只能拿到库自动生成的键，读不到「右栏占多少」。显式 id 同时让 layout 的
// 含义自解释（见 App 里那条「右栏 >50% 收左栏」的规则）。
const LEFT_PANEL_ID = 'left'
const RIGHT_PANEL_ID = 'right'
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
  const rightOverHalf = useRef(false)
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
  // 依赖 handle：挂载即应用初始状态（首轮 rightRef.current 为 null 拿不到 handle）。
  useEffect(() => {
    const h = leftRef[0]
    if (!h) return
    if (leftExpanded && h.isCollapsed()) h.expand()
    else if (!leftExpanded && !h.isCollapsed()) h.collapse()
  }, [leftExpanded, leftRef[0]])

  useEffect(() => {
    const h = rightRef[0]
    if (!h) return
    if (rightExpanded && h.isCollapsed()) h.expand()
    else if (!rightExpanded && !h.isCollapsed()) h.collapse()
  }, [rightExpanded, rightRef[0]])

  // M3：rightSize 非空时把右侧面板 resize 到期望宽度（浏览器内嵌按 viewport 设）。
  const rightSize = useAppStore((s) => s.rightSize)
  useEffect(() => {
    const h = rightRef[0]
    if (!h || rightSize == null || h.isCollapsed()) return
    h.resize(rightSize)
  }, [rightSize, rightRef[0]])

  const hubOpen = useHubStore((s) => s.open)
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
  // 进入 Hub 时确保右栏展开（概览是详情的主入口）；只在打开的那一次强制，
  // 用户随后手动收起右栏不被反复顶开（折叠态由 HubInspector 自己提供图标条）。
  // 退出 Hub 时恢复进入前的折叠状态：Hub 是临时视图，不应改变工作区的布局偏好。
  const wasHubOpen = useRef(false)
  const rightExpandedBeforeHub = useRef<boolean | null>(null)
  useEffect(() => {
    if (hubOpen && !wasHubOpen.current) {
      rightExpandedBeforeHub.current = rightExpanded
      setRightExpanded(true)
    } else if (!hubOpen && wasHubOpen.current) {
      const before = rightExpandedBeforeHub.current
      rightExpandedBeforeHub.current = null
      if (before != null) setRightExpanded(before)
    }
    wasHubOpen.current = hubOpen
  }, [hubOpen, rightExpanded, setRightExpanded])
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
            // 右栏被**用户拖**到 ≥50% 时自动收起左栏（把视界让给右栏）。
            //
            // 为什么必须看 `meta.isUserInteraction`（2026-09-18 实测缺陷）：右栏宽度也会被
            // **程序**改 —— 内嵌浏览器按 viewport 适配走 `setRightSize` → `h.resize(900)`，
            // 在 1440 宽的窗口里就是 62% > 50%。旧写法把这条规则放在 Panel.onResize 里，
            // 于是「预览一个 PDF」会顺手把用户的左栏收掉（同一份 e2e 里实测：左栏 false）——
            // 那正是 M4 要修掉的「预览有 Browser Use 接管视界的副作用」的另一种化身。
            // 库只在**用户直接操作分隔条**（拖拽释放 / 方向键）时置 isUserInteraction=true，
            // 程序化 setLayout/约束重算/初始挂载都是 false —— 正好是我们想要的判据。
            onLayoutChanged={(layout, meta) => {
              const pct = layout[RIGHT_PANEL_ID]
              if (pct == null) return
              const overHalf = pct >= 50
              // 只在「从阈值下方跨到上方」时折叠一次；保持在 50% 以上期间不重复触发。
              if (meta.isUserInteraction && overHalf && !rightOverHalf.current && !(leftRef[0]?.isCollapsed() ?? true)) {
                leftRef[0]?.collapse()
              }
              rightOverHalf.current = overHalf
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
                setLeftExpanded(!(leftRef[0]?.isCollapsed() ?? false))
              }}
            >
              <LeftPanel />
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
              minSize={200}
              maxSize="80%"
              // 只做「展开/收起」的镜像；「右栏 >50% 收左栏」的规则在 Group.onLayoutChanged
              //（那里才有 meta.isUserInteraction，能区分用户拖拽与程序化 resize）。
              onResize={(_size, _id, prev) => {
                if (!prev) return // 挂载首报不覆盖 store 初始态
                setRightExpanded(!(rightRef[0]?.isCollapsed() ?? false))
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
      <SandboxReleaseModal />
      <NewWorkspaceModal />
      <IMBindConfirmModal />
      <SettingsModal />
      <ToastView />
    </div>
  )
}
