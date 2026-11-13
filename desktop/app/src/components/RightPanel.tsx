import { Fragment, useEffect, useRef, useState, useCallback } from 'react'
import type { ReactElement, ReactNode } from 'react'
import { useAppStore, isPendingWebTab, rebuildWebTab } from '../store/useAppStore'
import type { FileTabInstance, RightTab, TabKind, TabInstance, TabSpec } from '../store/useAppStore'
import { Activity, GitBranch, FileIcon, CheckList, EyeIcon, Globe, PanelRight, ChevronDown, Plus } from './icons'
import { ChangesTab } from './ChangesTab'
import { WebTab } from './WebTab'
import { ActivityTab } from './ActivityTab'
import { FilePreviewTab } from './FilePreview'
import { EditorConfirm } from './EditorConfirm'
import { fileUrlToPath } from '../lib/filePaths'
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

/**
 * `.right-body` 的**本地兜底**：provider 未注册时用它渲染非 file 面板。
 *
 * 为什么存在：provider 靠 `surfaces/builtin` 的**副作用 import** 注册（只有 main.tsx 那样做），
 * 而若干 e2e 探针页只 import RightPanel —— 那时注册表是空的。没有兜底，那些探针看到的是空白
 * 面板，且空白不会让任何断言变红（04-review F45/F46 两次踩过）。
 *
 * 为什么没有 file：文件面板要一条「按标签 id 重挂载」的 key（见 body 里 file 那段的注释），
 * 与统一委派槽的「不许加 key」相冲突 —— file 留独立槽，收口留给 PR-11。
 * 表里没有的 kind（例如未注册的 plugin:*）返回 null：与「没有分支可渲染」的旧行为一致。
 */
function panelFallback(tab: TabInstance, scope: string): ReactNode {
  switch (tab.kind) {
    case 'activity': return <ActivityTab />
    case 'git': return <ChangesTab />
    case 'web': return <WebTab tab={tab} scope={scope} />
    default: return null
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
  // R08b：pdf 预览标签（file kind）也在这块区域里显示原生视图 —— 它由面板自己建/摆位，
  // 并把「我正在显示」写进 store.previewEmbedActive。缺这一条时，闸门会在面板刚显示完就
  // 把视图藏掉（父级 effect 晚于子级 effect 跑），表现为「pdf 预览一片空白、但视图池里有它」。
  const previewEmbedActive = useAppStore((s) => s.previewEmbedActive)
  useEffect(() => {
    if (!embeddedPresentation) return
    const nativeHost = activeTab?.kind === 'web' || (activeTab?.kind === 'file' && previewEmbedActive)
    if (!(rightExpanded && nativeHost)) void window.desktop?.embedHide()
  }, [embeddedPresentation, rightExpanded, activeTab?.kind, previewEmbedActive])

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
  const tabIconEl = (tab: TabInstance): ReactNode => {
    if (tab.kind === 'web') return getSurface('web')?.icon?.(tab) ?? kindIcon('web')
    // 预览标签用**眼睛**图标（R08b）：标签窄到只剩两个汉字时，标题里的「· 预览」会被
    // shortName 截掉 —— 图标是那时唯一还看得见的区分（同一个文件的 source 标签仍是文件图标）。
    if (tab.kind === 'file' && (tab as FileTabInstance).mode === 'preview') return <EyeIcon size={12} />
    return kindIcon(tab.kind)
  }
  const tabTitle = (tab: TabInstance): string => {
    if (tab.kind === 'file') {
      const base = (tab.path ?? '').split('/').filter(Boolean).pop()
      // 长文件名中间截断（保留 `_test.go` 这类尾巴）：标签最宽 168px，从尾巴截断时
      // `conversation_router.go` 与 `conversation_router_test.go` 会长得一模一样。
      // 完整路径在 title 里（见渲染点的 title 属性）。
      const name = (base && shortName(base)) || kindLabel('file')
      // mode 后缀（R08b / R08a 偏差 5）：同一个文件可以同时有 preview 与 source 两个标签
      //（tabKeyOf 的幂等键带 mode），而标题此前只写文件名 —— 两个标签一模一样，用户分不清
      // 点哪个。只给**非缺省**的那一侧加后缀：source 是缺省（也是绝大多数标签），给它加词
      // 等于把全应用的标签都改宽一寸，而「预览」这种新入口才是需要被认出来的那个。
      // diff 也有同样的问题，但那是 PR-06 之前就有的口径，本轮不动（不在验收范围内）。
      const mode = (tab as FileTabInstance).mode
      return mode === 'preview' ? `${name} · ${t('rp.tab.previewTag')}` : name
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
                  data-mode={tab.kind === 'file' ? ((tab as FileTabInstance).mode ?? 'source') : undefined}
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
          {/* R07c：非 file 面板（activity / git / web）统一走 provider 委派 —— 一条表达式覆盖三种
              kind，本地兜底见 panelFallback。为什么不按 kind 各写一行：三行的内容都是
              「getSurface(kind)?.render(...) ?? 本地组件」，逐行写等于把 kind 判别留在 body 里，
              而 PR-11 的终态判据是 body 里再没有 kind 等值分支（`grep -c "kind ==="` = 0）。
              顺序也重要：provider 先、兜底后。provider 是**副作用注册**（只有 main.tsx 导入
              builtin 时才存在），而若干探针页只 import RightPanel —— 没有兜底时它们渲染空白面板，
              且空白不会让任何断言变红（04-review F45/F46）。
              **不要**给这个元素加 key：切 web 标签时组件实例必须原地复用（DOM 容器不重挂载，
              原生视图与页面状态才保得住）—— 加 key = 每次切标签销毁重建 WebContentsView（R06c）。
              file 不走这条：它要一条「按标签 id 重挂载」的 key，与「不许加 key」相冲突，
              重挂载语义的收口留给 PR-11。 */}
          {activeTab && activeTab.kind !== 'file' && (
            getSurface(activeTab.kind)?.render({ tab: activeTab, scope }) ?? panelFallback(activeTab, scope)
          )}
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
        </div>
      </div>
      {/* 脏内容确认框：关标签与退出应用共用一个（reason 区分）。挂在 aside 末尾而不是 body 里
          —— 标签被关掉后 body 可能已经是空态，框还得在。 */}
      <EditorConfirm />
    </aside>
  )
}
