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
import { useAppStore } from './store/useAppStore'
import { applyTheme } from './lib/theme'
import { applyNarrativeSkin } from './lib/narrativeSkin'
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
    applyAppearance(appearance) // 必须在 applyTheme 之后：它读的是解析后的 data-theme
    if (window.desktop) void window.desktop.applyTheme(theme)
  }, [theme, narrativeSkin, appearance])

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

  return (
    <div className={`app-root ${inSkills ? 'in-skills' : ''}${inCron ? 'in-cron' : ''}${leaving ? ' leaving' : ''}`}>
      {mainReady && (
        <main className="main">
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
