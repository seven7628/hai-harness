import { useEffect, useState } from 'react'
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
import BridgeBanner from './components/BridgeBanner'
import { useAppStore } from './store/useAppStore'
import { applyTheme } from './lib/theme'
import { applyNarrativeDensity, applyNarrativeSkin } from './lib/narrativeSkin'
import { applyAppearance, useAppearance } from './lib/appearance'

// 三栏面板的稳定 id：Group.onLayoutChanged 的 layout 以 id 为键（`{[id]: percentage}`）——
// 不显式给 id 就只能拿到库自动生成的键，读不到「右栏占多少」。拖拽时用它换算成
// 右栏占比（见 onLayoutChanged → setRightLayoutFromDrag）。
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
    if (!leftHandle) return
    if (leftExpanded && leftHandle.isCollapsed()) leftHandle.expand()
    else if (!leftExpanded && !leftHandle.isCollapsed()) leftHandle.collapse()
  }, [leftExpanded, leftHandle])

  useEffect(() => {
    if (!rightHandle) return
    if (rightExpanded && rightHandle.isCollapsed()) rightHandle.expand()
    else if (!rightExpanded && !rightHandle.isCollapsed()) rightHandle.collapse()
  }, [rightExpanded, rightHandle])

  // 三态布局（PR-01）：预设 bias 时把右栏 resize 到目标比例；custom 由用户拖拽决定，不回写。
  const rightLayout = useAppStore((s) => s.rightLayout)
  const rightSize = useAppStore((s) => s.rightSize)
  const setRightLayoutFromDrag = useAppStore((s) => s.setRightLayoutFromDrag)
  useEffect(() => {
    // rightSize（内嵌浏览器 viewport）优先级更高：它在时本轮不动宽度，避免两个 effect 抢写
    if (!rightHandle || !leftHandle || rightSize != null) return
    if (!rightLayout.open || rightLayout.bias === 'custom' || rightHandle.isCollapsed()) return
    const availPct = Math.max(1, 100 - leftHandle.getSize().asPercentage)
    rightHandle.resize(((availPct * rightLayout.ratio) / 100).toFixed(2) + '%')
  }, [rightLayout, rightSize, rightHandle, leftHandle])

  // M3：rightSize 非空时把右侧面板 resize 到期望宽度（浏览器内嵌按 viewport 设）。
  useEffect(() => {
    if (!rightHandle || rightSize == null || rightHandle.isCollapsed()) return
    rightHandle.resize(rightSize)
  }, [rightSize, rightHandle])

  return (
    <div className={`app-root ${inSkills ? 'in-skills' : ''}${inCron ? 'in-cron' : ''}${leaving ? ' leaving' : ''}`}>
      {mainReady && <BridgeBanner />}
      {mainReady && (
        <main className="main">
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
                setLeftExpanded(!(leftRef[0]?.isCollapsed() ?? false))
              }}
            >
              <LeftPanel />
            </Panel>
            <Separator className="resize-handle" />
            <Panel minSize={300}>
              <CenterPanel />
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
              <RightPanel />
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
      <Statusbar />
      <SandboxReleaseModal />
      <NewWorkspaceModal />
      <IMBindConfirmModal />
      <SettingsModal />
      <ToastView />
    </div>
  )
}
