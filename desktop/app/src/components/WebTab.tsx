// web（内嵌浏览器）面板的组件本体 —— R06c 自 RightPanel 逐字平移。
//
// 为什么搬：PR-06 要让 web 面板走 SurfaceProvider（surfaces/builtin/webSurface.tsx），
// provider 的 render 只拿得到 { tab, scope }。组件里原本有四个 RP props 不是标签数据，
// 各自换了来源（都是**取值不变**的等价替换）：
//   · toolbarHidden / onToggleTabs ← RP 的 immersive。它只有一个消费者（本组件），
//     所以变成内部 state；切标签不会重置 —— 组件实例在切标签时不重挂载，与平移前一致。
//   · panelVisible ← store.rightExpanded。挂载期间**恒为 true**（见下文），取值与平移前恒等。
//   · menuOpen ← store.panelMenuOpen（R06c 上移，口径仍是 `menu || ctx`）。
//   · activePageId ← tab.pageId。
//
// **挂载/卸载时机必须与平移前逐字一致** —— 这是两个历史 bug 的根因（见 embedRectOf 的长注释）：
//   ①「页面排版视口被钉死 80×80」：量不到矩形时下发 0×0（旧实现钳成 80），页面按 80px 排版
//      且**不自愈**（同尺寸切标签不重挂载、ResizeObserver 也不再触发）；
//   ②「右栏收起但原生视图仍浮在最上层」：建连成功后不 hide，可见性 effect 兜不住异步时序。
// 两条都取决于「DOM 容器什么时候存在」。所以这里**不加**任何新的条件渲染、不改根节点结构
// （`div.file-preview > div.browser-shot` 逐字保留）：组件仍由 RightPanel 在
// `rightExpanded && activeTab.kind === 'web'` 时挂载，收起右栏即卸载。
import { useCallback, useEffect, useRef, useState } from 'react'
import { useT } from '../i18n'
import { centerOf, overlayCoversRect, rectsIntersect } from '../lib/overlayHit'
import { useAppStore } from '../store/useAppStore'
import type { WebTabInstance } from '../store/useAppStore'
import type { SurfaceRenderContext } from '../surfaces/types'

// embedRectOf：把一个 DOM 容器量成「给原生视图用的矩形」；**量不出来返回 undefined**。
//
// 为什么不返回一个钳过的最小矩形（2026-09-21 用户报障的根因）：
//   收起右栏时 WebTab 卸载，而 ResizeObserver 对**已脱离文档**的元素还会补发一次
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

export function WebTab({ tab }: SurfaceRenderContext<WebTabInstance>) {
  const t = useT()
  // —— R06c：原先由 RightPanel 传入的四样东西，各归其位（取值不变）——
  // 沉浸（隐藏标签条）：RP 里只有这一个消费者 → 内部 state。切标签不重置（组件不重挂载）。
  const [immersive, setImmersive] = useState(false)
  const toolbarHidden = immersive
  const onToggleTabs = useCallback(() => setImmersive((v) => !v), [])
  // 右栏是否展开：可见性仲裁的输入。**挂载期间恒为 true** —— RightPanel 在 !rightExpanded 时
  // 走折叠分支（整块内容不渲染），本组件只在展开态存在。保留它是为了让建连 effect 的依赖表
  // 与「建连即 hide」那行和平移前逐字一致（不是新语义，也不是新增的渲染条件）。
  const panelVisible = useAppStore((s) => s.rightExpanded)
  // 面板级菜单（⌄ / ＋ / 标签右键）：RP 的 `menu !== null || ctx !== null` 镜像（R06c 上移）。
  const menuOpen = useAppStore((s) => s.panelMenuOpen)
  // 回退（MCP 页列表）模式的目标页：由**激活标签**派生（平移前是 RP 传的 activePageId prop）。
  const activePageId = tab.pageId
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