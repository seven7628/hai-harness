import { Fragment, useEffect, useReducer, useRef, useState, useCallback } from 'react'
import type { KeyboardEvent as ReactKeyboardEvent, ReactElement, ReactNode } from 'react'
import { useAppStore, isPendingWebTab, rebuildWebTab, selectRightTab, TABS_MAX_VISIBLE } from '../store/useAppStore'
import type { FileTabInstance, RightTab, TabKind, TabInstance, TabSpec } from '../store/useAppStore'
import { Activity, GitBranch, FileIcon, CheckList, Globe, PanelRight, ChevronDown, Plus } from './icons'
import { ChangesTab } from './ChangesTab'
import { WebTab } from './WebTab'
import { ActivityTab } from './ActivityTab'
import { FilePreviewTab } from './FilePreview'
import { EditorConfirm } from './EditorConfirm'
import { fileUrlToPath } from '../lib/filePaths'
import { getSurface, subscribeSurfaces } from '../surfaces/registry'
import { SurfaceBoundary, SurfaceTombstone } from '../surfaces/SurfaceBoundary'
import type { UnavailableReason } from '../surfaces/SurfaceBoundary'
import { getSurfaceHealth, isSurfaceDisabled } from '../surfaces/health'
import { declaredEntryTitle, entryTitleText, groupEntryItems, listPluginEntries } from '../surfaces/plugin/entries'
import type { PluginEntryItem } from '../surfaces/plugin/entries'
import PluginEntryIcon from '../surfaces/PluginEntryIcon'
import type { SurfaceKind } from '../surfaces/types'
import type {} from '../lib/fileStatus'
import { shortName } from '../lib/shortPath'
import { copyText } from '../lib/clipboard'
import { useLang, useT } from '../i18n'
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
// 标签标题与类型图标**都在 provider 里**：web 的随 R06b 迁进 surfaces/builtin/webSurface.tsx，
// 其余三个随 PR-11 迁进各自的 provider（file 的 preview→眼睛也在那里）。RP 侧只剩
// `kindIcon(kind, tab?)` 一个取值点，不再认识任何一个 kind 的字形。
// 若将来要在 provider 之外复用「页面类型 → 图标」那套判定，提升到 lib —— 不要在这里再抄一份。

// 标签条最多平铺几个（超出收进「+N」）：常量在 store（TABS_MAX_VISIBLE = 5，R4 起就已定义，
// PR-09 之前没有消费者）。取 5：与原型一致，且 360px 宽的右栏里 5 个已是「还能读出文件名」的下限。

// 主 Agent 压缩镜像（右栏任务 tab）：与主会话同一数据源 —— 进行中显示活动条
// （顶层 compressing/compressBefore 镜像），完成后渲染主会话同一个 compression block
// （blocks 尾部最后一条 kind==='compression'）。不新建第二套活动状态。
// 「+」菜单里**没有** file：文件标签需要路径，由点文件产生（阶段 2 起是**多实例**：
// 一个文件一个标签，同一文件的 diff / source 各一个 —— 见 store.openTab 的幂等键）。
const ADDABLE_KINDS: TabKind[] = ['activity', 'git', 'web']
/**
 * 标签 / 菜单 / 空态面板共用的**一个**图标取值点：provider 是唯一真相。
 *
 * PR-11 之前这里是 `switch (kind)`（git/file/web/activity 四个字形 + default），而 web 的
 * 标签图标早已搬进 webSurface —— 同一种信息两处判，第二个 kind 细化（file 的 preview→眼睛）
 * 就得改两个地方。现在四个内置 provider 各自给字形与尺寸，RP 不再认识任何一个 kind 的名字。
 *
 * tab 可省：＋ 菜单与空态能力面板列的是「现在能开什么」，那里没有标签实例（provider 的
 * icon 签名已把入参改成可选，见 surfaces/types.ts —— 不要在这里造假 tab）。
 *
 * 兜底 `Activity` 只在 **provider 未注册**时可达（只 import RightPanel 的探针页；生产里
 * main.tsx 的副作用 import 保证注册在先）：与 `.right-body` 的 panelFallback 同一口径 ——
 * 给一个中性图标，而不是在 RP 里再抄一份 kind→图标表（那正是本轮删掉的东西）。
 */
function kindIcon(kind: SurfaceKind, tab?: TabInstance): ReactNode {
  return getSurface(kind)?.icon?.(tab) ?? <Activity size={12} />
}

// 「现在能新建什么」= 静态清单减去**被禁用**的 kind（health 的禁用集）。
//
// 为什么**不**和 listSurfaces() 取交集（PR-10 复评改过一次）：菜单是「这个应用能开什么」的
// 静态能力清单，不是「注册表此刻装了什么」的镜像。取交集会让「注册表空 / 某个 provider 漏了
// 副作用注册」变成**菜单静默少一项甚至全空** —— 而那正是 panelFallback 兜得住的场景
//（04 F45）：点下去还能渲染，用户只是少看见一个入口。少一项没人会报障，比渲染出错更难查。
// 「禁用」是另一回事：那是用户/系统的显式决定，菜单必须跟着变。
// PR-11 删掉静态清单时，等价的写法是「listSurfaces() ∪ 允许兜底的 kind」，别退回交集。
function addableKinds(): TabKind[] {
  return ADDABLE_KINDS.filter((k) => !isSurfaceDisabled(k))
}

/**
 * 标签的「不可用」判定（PR-10 墓碑）：
 *   · 被禁用（连续 3 次崩溃自动禁用 / 手动禁用）→ 'disabled'
 *   · 插件 provider 没注册（插件缺失/卸载）→ 'missing'
 * 内置 kind 的 provider 缺失**不算**墓碑：若干探针页只 import RightPanel，注册表是空的，
 * 那里靠 panelFallback 渲染（04 F45 的兜底）—— 把那种情况改成墓碑会把老探针整片打红，
 * 而生产里内置 provider 由 main.tsx 的副作用 import 保证一定在。
 */
/**
 * provider 的渲染宿主 —— 必须是**一个组件**，不能写成
 * `{getSurface(kind)?.render({ tab, scope })}` 这样在 RightPanel 自己的 render 里直接调用。
 *
 * 为什么（PR-10 最容易漏的一条，实测踩过）：`provider.render` 是普通函数，直接在 RP 的
 * render 期间调用 = 它的异常发生在 **RP 自己的渲染栈**里，SurfaceBoundary 根本接不到 ——
 * React 看到的是「RightPanel 抛了」，于是整棵树被卸载（探针表现为「整页空白」，而
 * CrashCard 一条都不出现）。放进独立组件后，异常落在边界**子树**里，才归 boundary 管。
 * 返回值是 Fragment：这一层不产生 DOM 节点，`.right-body > .file-activity-panel` 之类的
 * 直接子元素关系不变。
 */
function SurfaceHost({ kind, tab, scope, fallback }: {
  kind: SurfaceKind
  tab: TabInstance
  scope: string
  /** provider 未注册时的本地兜底（file 用 FilePreviewTab，其余见 panelFallback） */
  fallback?: ReactNode
}) {
  const p = getSurface(kind)
  if (p) return <>{p.render({ tab, scope })}</>
  return <>{fallback ?? panelFallback(tab, scope)}</>
}

function unavailableReason(tab: TabInstance): UnavailableReason | null {
  const kind: SurfaceKind = tab.kind
  if (isSurfaceDisabled(kind)) return 'disabled'
  if (kind.startsWith('plugin:') && !getSurface(kind)) return 'missing'
  return null
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

// 标签条上的三个面板菜单（＋ 新建 / ⌄ 重开 / PR-09 的 +N 溢出）。开合走同一套
// ref + state + panelMenuOpen 镜像，backdrop 与 Escape 也只写一份。
type PanelMenu = 'add' | 'more' | 'overflow' | null

export default function RightPanel() {
  const t = useT()
  // R16b：入口标题可能是 `{ zh, en }`，语言在**渲染点**选（快照里留声明本身 ——
  // 见 surfaces/plugin/entries.ts 的 entryTitleText）。切语言时这里跟着重渲染。
  const lang = useLang()
  const allTabs = primaryTabs(t).concat(secondaryTabs(t))
  const rightExpanded = useAppStore((s) => s.rightExpanded)
  // R11b：rail 高亮读**派生选择器**（激活标签的 kind），不再读 state 里那个写穿副本。
  // 差异只有一处，且是修缺陷：标签关光（activeId=null）时旧字段保留最后的值，rail 会一直
  // 高亮一个已经不在的面板；选择器此时返回 undefined → 没有一个图标亮着（与内容区空态一致）。
  const activeKind = useAppStore(selectRightTab)
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
  // PR-10：内容区要不要渲染 provider（墓碑判定，见 unavailableReason）。非 null = 渲染占位卡，
  // **不渲染 provider** —— 被禁用的面板连挂载都不该发生（否则又是一次崩溃/白屏）。
  const activeUnavailable = activeTab ? unavailableReason(activeTab) : null
  // —— R16：插件入口（D-2 的三处宿主渲染点，这里是其中两处）——
  // 快照由加载器在每轮 sync 里更新（禁用 → 立刻为空），订阅走下面的 subscribeSurfaces，
  // 不另开 IPC。分组口径与设置页共用 groupEntryItems：一个插件一条「这几个入口」。
  const pluginEntryGroups = groupEntryItems(listPluginEntries())
  // 只有一个插件时不画插件名小标：那时「面板插件」这四个字已经说清来源，
  // 再加一行名字只是把 3 行菜单变成 4 行（多插件时才需要区分「谁的课表」）。
  const showPluginGroupNames = pluginEntryGroups.length > 1
  // —— PR-09：+N 溢出（让 TABS_MAX_VISIBLE 真正生效）——
  // 可见窗口 = 数组前 TABS_MAX_VISIBLE 个；**激活标签必须始终可见**：它落在窗口外时占据
  // 最后一槽（只换渲染，不动 tabs 数组 —— 数组顺序既是用户打开的顺序，也是 ⌃⇥ 的遍历顺序）。
  // 隐藏项 = 没进可见窗口的那些，N 与之一一对应（下拉里数得出来几个，按钮上就写几）。
  const overflowed = tabs.length > TABS_MAX_VISIBLE
  const visibleTabs = (() => {
    if (!overflowed) return tabs
    const head = tabs.slice(0, TABS_MAX_VISIBLE)
    const act = tabs.find((x) => x.id === tabSet?.activeId)
    if (!act || head.includes(act)) return head
    return [...head.slice(0, TABS_MAX_VISIBLE - 1), act]
  })()
  const visibleIds = new Set(visibleTabs.map((x) => x.id))
  const hiddenTabs = overflowed ? tabs.filter((x) => !visibleIds.has(x.id)) : []
  // 编辑标签的脏标记（PR-05b）：真相在 lib/editorDocs，store 只镜像布尔值（标签圆点读它）。
  const dirtyTabs = useAppStore((s) => s.dirtyTabs)
  // 面板级菜单（⌄ 重开 / ＋ 新建 / 标签右键）：定位与内容仍是组件局部 state（菜单要锚坐标、
  // 要读标签），**开合**另镜像一份到 store.panelMenuOpen —— R06c 起 web 面板由 webSurface 渲染，
  // WebTab 不在本组件的 state 树里，读不到这两个局部 state，而它必须知道「有菜单开着」才能
  // 隐藏原生视图（原生层级 > DOM，否则菜单被预览画面盖掉）。
  // 两个 setter 调用点（下面 setMenu / setCtx）各自同步镜像；ref 只是为了让两个 wrapper 保持
  // **稳定身份** —— 否则 ctx 的全局 click/Escape effect 会因为依赖变化每次渲染都重订阅。
  // PR-09：'overflow' = 标签条的 +N 下拉（与 ＋ / ⌄ 共用 backdrop、Escape、panelMenuOpen 镜像）。
  const [menu, setMenuState] = useState<PanelMenu>(null)
  // 标签右键菜单（R4 的批量关闭 + 文件动作；内层标签条退役后搬到这里）。
  const [ctx, setCtxState] = useState<{ x: number; y: number; id: string } | null>(null)
  const setPanelMenuOpen = useAppStore((s) => s.setPanelMenuOpen)
  const menuRef = useRef<PanelMenu>(null)
  const ctxRef = useRef<{ x: number; y: number; id: string } | null>(null)
  const syncPanelMenuOpen = useCallback(() => {
    setPanelMenuOpen(menuRef.current !== null || ctxRef.current !== null)
  }, [setPanelMenuOpen])
  const setMenu = useCallback((next: PanelMenu | ((cur: PanelMenu) => PanelMenu)) => {
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
  // 打开一个插件入口：`params.entry` 是**必填**的（D-2：一个 kind 的多个标签实例靠它区分）。
  // 不传它 = 三个入口共用一个标签，点第二个会「没反应」（store.tabKeyOf 按 (kind, entry) 认身份）。
  // 形状 cast 只此一处：TabInstance 联合里没有 plugin 成员（口径见 store 的 toPersistedTab）。
  const openPluginEntry = useCallback((entry: PluginEntryItem) => {
    openTab({ kind: entry.kind, params: { entry: entry.entryId } } as unknown as TabSpec)
    setMenu(null)
  }, [openTab, setMenu])
  // 卸载时归零：panelMenuOpen 描述的是**本组件子树里**那个菜单，状态不该比它的所有者活得久。
  // RightPanel 真的会卸载（App 的 <main> 由 mainReady 控制，启动/重载期间不渲染子树）——
  // 若那一次卸载恰好带着开着的菜单，残留的 true 会让下一次挂载的 WebTab **永远**隐藏原生视图
  //（面板一片空白，且没有任何报错）。这是唯一一个不属于「镜像」的写点，故意留在这里而不是
  // 藏在 setter 里：它是生命周期约束，不是菜单状态的来源。
  useEffect(() => () => setPanelMenuOpen(false), [setPanelMenuOpen])
  // —— PR-09：+N 下拉的键盘可达（roving focus）——
  // 焦点由 state 驱动（overflowFocus 变 → 下面的 effect 把 DOM 焦点搬过去）；菜单项是
  // tabIndex=-1 的**真 <button>**：Enter/Space 走浏览器原生激活（= 该项的 onClick），
  // keydown 里只处理 ↑↓，不代劳 click（否则会和原生激活重复触发一次）。
  const overflowBtnRef = useRef<HTMLButtonElement | null>(null)
  const overflowMenuRef = useRef<HTMLDivElement | null>(null)
  const [overflowFocus, setOverflowFocus] = useState(0)
  useEffect(() => {
    if (menu !== 'overflow') return
    const items = overflowMenuRef.current?.querySelectorAll<HTMLButtonElement>('.rtabs-overflow-item')
    if (!items?.length) return
    items[Math.min(overflowFocus, items.length - 1)]?.focus()
  }, [menu, overflowFocus])
  // —— R16b：＋ 新建菜单的键盘可达（与上面 +N 同一套 roving focus）——
  // 打开时把焦点放到第一项、↑↓ 在**真 <button>** 之间搬焦点、Enter/Space 走浏览器原生
  // 激活（= 该项的 onClick；keydown 里不代劳，否则一次按键会开两个标签）。
  // 与 +N 的唯一差别是 items 的范围：这里用 `.rtabs-mi`（含「面板插件」那几项 ——
  // 插件的入口也是菜单项，方向键不该在分组边界上停住）。
  const addBtnRef = useRef<HTMLButtonElement | null>(null)
  const addMenuRef = useRef<HTMLDivElement | null>(null)
  const [addFocus, setAddFocus] = useState(0)
  const addMenuItemCount = () => addMenuRef.current?.querySelectorAll<HTMLButtonElement>('.rtabs-mi').length ?? 0
  useEffect(() => {
    if (menu !== 'add') return
    const items = addMenuRef.current?.querySelectorAll<HTMLButtonElement>('.rtabs-mi')
    if (!items?.length) return
    items[Math.min(Math.max(addFocus, 0), items.length - 1)]?.focus()
  }, [menu, addFocus])
  // ↑↓ 的焦点移动：回绕（末项 ↓ = 首项），与 +N 的方向语义相反没关系 —— 两个菜单都只
  // 需要「一直按 ↓ 能转一圈回到起点」。
  const onAddMenuKeyDown = (e: ReactKeyboardEvent) => {
    if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return
    const n = addMenuItemCount()
    if (!n) return
    e.preventDefault()
    setAddFocus((i) => (i + (e.key === 'ArrowDown' ? 1 : -1) + n) % n)
  }

  // ＋ / ⌄ / +N 三个面板菜单的 Escape：backdrop 只覆盖「点别处」，键盘也得能关。
  // +N 是键盘入口，关闭后焦点必须**回到按钮**：菜单项在关闭时被卸载，焦点会掉到 body，
  // 键盘用户得从头 Tab 一整轮才能再打开（这就是「菜单键盘可达」的反面）。
  useEffect(() => {
    if (!menu) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      setMenu(null)
      // 关闭后焦点必须回到**打开它的按钮**：菜单项被卸载时焦点掉到 body，键盘用户得
      // 从头 Tab 一整轮才能再打开（"菜单键盘可达"的反面）。
      if (menu === 'overflow') overflowBtnRef.current?.focus()
      else if (menu === 'add') addBtnRef.current?.focus()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [menu, setMenu])
  // —— PR-09：⌃⇥ / ⌃⇧⇥ 在 tabs 数组里前后移动 activeId（末尾/开头回绕）——
  // 与 ⌘1..9（App.tsx：切最近会话，不是切标签）是两回事，所以显式排除 metaKey。
  // 监听挂在 window 而不是标签条上：这是**面板级**的全局切换，输入框/编辑器聚焦时也要生效
  //（它不是一个文本编辑键）。卸载/折叠时拆掉监听。
  useEffect(() => {
    if (!rightExpanded) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Tab' || !e.ctrlKey || e.metaKey) return
      e.preventDefault() // 别让 Tab 的默认行为（移动焦点）和切换同时发生
      const set0 = useAppStore.getState().tabsByScope[scope]
      const list = set0?.tabs ?? []
      const n = list.length
      if (!n) return
      const i = list.findIndex((x) => x.id === set0?.activeId)
      const step = e.shiftKey ? -1 : 1
      // activeId 悬空（i<0）时：前进落到第一个、后退落到最后一个
      const j = i < 0 ? (step > 0 ? 0 : n - 1) : (i + step + n) % n
      const next = list[j]
      if (next && next.id !== set0?.activeId) activateTab(next.id)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [rightExpanded, scope, activateTab])
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
  // 关闭全部 = 同一条批量路径（干净的直接 closeTabNow、脏的进确认队列逐个问，见 store.closeTabs）。
  // 不写成 `for (...) closeTab(x)`：那样多个脏标签只能被问到最后一个，用户丢完一个还得再关一次。
  const ctxCloseAll = () => { closeTabs(tabs.map((x) => x.id)); setCtx(null) }
  // 重开 = 重开栈栈顶（store.reopenTab 不传 id 时就是这个语义）；栈空时菜单项 disabled。
  const ctxReopen = () => { reopenTab(); setCtx(null) }
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

  // 标签集收敛：这条 effect 现在只做两件事 —— 「该 scope 还没有标签集时建默认集」和「清恢复
  // 标记」，已一致时 store 侧不 set（防自激）。R11b 之前它还要把派生字段 rightTab 拉回激活
  // 标签，所以依赖里有 rightTab；字段删掉后没有要收敛的第二份状态，依赖只剩 scope。
  useEffect(() => { syncTabs() }, [syncTabs, scope])

  // PR-10：注册表 / 禁用集变了要推动重渲染 —— 墓碑替换崩溃卡、标签条灰态、＋ 菜单剔除
  // 被禁用的 kind，全都靠这一次重渲染。订阅复用 registry.subscribeSurfaces（health 的每次
  // 改动也走那一条广播），不另造第二套订阅。
  const [, bumpSurfaces] = useReducer((x: number) => x + 1, 0)
  useEffect(() => subscribeSurfaces(bumpSurfaces), [])

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
  // 图标：所有 kind 都由 provider 的 icon 给（PR-11 起 RP 里没有 switch，见 kindIcon）。
  // web 按页面 URL 判类型（pdf/图片/xlsx/网页…，与产出卡/文件栏共用 visualKindOf，仍在
  // webSurface 里单一份）；待重建的恢复标签不在视图池里（viewId 为空）→ provider 退化用它的
  // **url 快照**，否则重启后这些标签会丢图标与名字（用户看到一排「网页」）。
  // 未读时**用未读点替代**图标（R4 口径：未读时用户最需要知道的是「有新东西」）—— 这一条
  // 留在 RP：它是标签条的交互状态，不是 provider 的元数据。
  // RP 只保留 webTabUrl：标签的 title 属性还要用它。
  const webTabUrl = (tab: TabInstance): string =>
    tab.kind === 'web' ? (embedNav?.targets?.find((x) => x.id === tab.viewId)?.url ?? tab.url ?? '') : ''
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
    // 插件 kind（PR-12 起进 store）没有 i18n 文案。
    // R16 起**已声明的入口**由 manifest 定标题：菜单项叫「课表」而标签叫别的，用户会以为点错。
    // 默认入口（零 entries）与插件自己 openTab 的标签仍归 provider.title —— 那两种情况下主人
    // 是插件自己，宿主没有声明可以依据（见 surfaces/plugin/entries.ts 的 declaredEntryTitle）。
    // provider 缺失（加载失败/已卸载）时退回 kind 本身，但**已声明入口**仍显示它的 manifest 标题
    //（墓碑也不该叫 plugin:course-manager）。
    const declared = declaredEntryTitle(tab.kind, (tab as { params?: unknown }).params, lang)
    if (declared) return declared
    const prov = getSurface(tab.kind)
    if (prov) return prov.title(tab, t)
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
              <button key={tab.key} className={`rbtn ${activeKind === tab.key ? 'on' : ''}`}
                // data-kind：折叠态下「现在是哪个面板」唯一的机器可读出口（展开态看标签条的
                // data-kind）。R11b 删掉 state 里的面板字段后，rail 高亮是选择器唯一的 DOM 证人。
                data-kind={tab.key}
                data-tip={tab.label}
                aria-label={tab.label}
                onClick={() => {
                  // rail 按钮是「只有 kind、没有 payload」的标签 spec（payload 由 provider 的
                  // defaultSpec 给：file → fileFocus，web → 池里激活的真实页面）。`as TabSpec`
                  // 收敛在这一处：TabSpec 是联合类型，`{ kind: tab.key }`（key 是四类的并集）
                  // 不能自动分配到某个成员上，而拆成四个 if 只会把同一件事写四遍。
                  openTab({ kind: tab.key } as TabSpec)
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
            {visibleTabs.map((tab) => {
              const on = tab.id === tabSet?.activeId
              // 阶段 3：休眠/恢复态的标签标记（灰态 + 「已休眠」/「正在恢复…」）。
              // 休眠条目仍在 embedNav.targets 里（sleeping: true）—— 标签身份不丢，点一下就恢复。
              // 阶段 5：**重启恢复的待重建标签**用同一套视觉（它同样是「有标签、没视图，点一下
              // 就有」的形态 —— 视图留给用户点击那一刻才建，见 store 的 rebuildWebTab）。
              const tp = tab.kind === 'web' ? embedNav?.targets?.find((x) => x.id === tab.viewId) : undefined
              const pending = isPendingWebTab(tab)
              const sleeping = !!tp?.sleeping || pending
              const restoring = !!tp?.restoring
              // PR-10：这个标签的面板现在不可用（被禁用 / 插件缺失）。标签**仍然在**（身份不丢，
              // 点它能看到原因与「重新启用」）—— 视觉复用休眠那套灰态（CSS 里 .rtab.unavailable
              // 与 .rtab.sleeping 同一组规则），但不写 data-sleeping：休眠是视图池 LRU 的语义，
              // 两个状态混用一个属性，以后就分不出「为什么会灰」。
              const un = unavailableReason(tab)
              return (
                <div
                  key={tab.id}
                  className={`rtab ${on ? 'on' : ''} ${sleeping ? 'sleeping' : ''} ${un ? 'unavailable' : ''}`}
                  data-kind={tab.kind}
                  data-mode={tab.kind === 'file' ? ((tab as FileTabInstance).mode ?? 'source') : undefined}
                  data-view={tab.kind === 'web' ? (tab.viewId ?? (tab.pageId != null ? `p${tab.pageId}` : '')) : ''}
                  /* R16：同一个插件的多个入口是**多个标签**（kind 相同、entry 不同），
                     标签条上必须能分辨它们 —— 定位、诊断、e2e 都读这一个属性（与菜单同源）。 */
                  data-entry={tab.kind.startsWith('plugin:')
                    ? String((tab as { params?: Record<string, unknown> }).params?.entry ?? '')
                    : undefined}
                  data-sleeping={sleeping ? '1' : undefined}
                  data-pending={pending ? '1' : undefined}
                  data-restoring={restoring ? '1' : undefined}
                  data-unavailable={un ? '1' : undefined}
                  data-unavailable-reason={un ?? undefined}
                  role="tab"
                  aria-selected={on}
                  tabIndex={on ? 0 : -1}
                  title={un
                    ? t(un === 'disabled' ? 'rp.surface.tabDisabled' : 'rp.surface.tabMissing')
                    : restoring ? t('browser.tabRestoring') : sleeping ? t('browser.tabSleeping') : (webTabUrl(tab) || (tab.kind === 'file' ? (tab.path || tabTitle(tab)) : tabTitle(tab)))}
                  onClick={() => activateTab(tab.id)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); activateTab(tab.id) }
                  }}
                  onContextMenu={(e) => { e.preventDefault(); setCtx({ x: e.clientX, y: e.clientY, id: tab.id }) }}
                  onMouseDown={(e) => {
                    // 只挡默认行为（Chromium 的中键自动滚屏）；关闭放在 auxclick ——
                    // 「按下后拖走取消」的手势不该关标签。
                    if (e.button === 1) e.preventDefault()
                  }}
                  onAuxClick={(e) => {
                    // PR-09：中键关闭（浏览器式）。走 closeTab —— 与 ✕ 完全同一条路径
                    //（dirty 标签会进确认框，不会静默丢内容）。
                    if (e.button !== 1) return
                    e.preventDefault()
                    closeTab(tab.id)
                  }}
                >
                  {tab.kind === 'web' && tab.viewId && unreadViews.includes(tab.viewId)
                    ? <span className="browser-page-dot" title={t('browser.tabUnread')} />
                    : <span className="rtab-ico">{kindIcon(tab.kind, tab)}</span>}
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
          {/* PR-09：+N 溢出。只渲染 TABS_MAX_VISIBLE 个槽位，其余收在这里；按钮是 <button>，
              所以天然在 Tab 序里（Enter/Space 打开，↑↓ 在项间移焦点，Escape 关闭并归还焦点）。
              菜单锚在自己的按钮下方（与 ＋ 同一套 .rtabs-menu-right 口径）。 */}
          {overflowed && (
            <div className="rtabs-overflow-wrap">
              <button
                ref={overflowBtnRef}
                className="rtabs-overflow"
                data-tip={t('rp.tabs.overflow').replace('{n}', String(hiddenTabs.length))}
                aria-label={t('rp.tabs.overflow').replace('{n}', String(hiddenTabs.length))}
                aria-haspopup="menu"
                aria-expanded={menu === 'overflow'}
                onClick={() => { setOverflowFocus(0); setMenu((m) => (m === 'overflow' ? null : 'overflow')) }}
              >
                +<span className="rtabs-overflow-n">{hiddenTabs.length}</span>
              </button>
              {menu === 'overflow' && (
                <div
                  ref={overflowMenuRef}
                  className="rtabs-menu rtabs-overflow-menu"
                  role="menu"
                  onKeyDown={(e) => {
                    if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return
                    e.preventDefault()
                    const n = hiddenTabs.length
                    if (!n) return
                    const step = e.key === 'ArrowDown' ? 1 : -1
                    setOverflowFocus((i) => (Math.min(i, n - 1) + step + n) % n)
                  }}
                >
                  {hiddenTabs.map((tab) => (
                    <button
                      key={tab.id}
                      className="rtabs-mi rtabs-overflow-item"
                      role="menuitem"
                      tabIndex={-1}
                      onClick={() => { activateTab(tab.id); setMenu(null) }}
                    >
                      {kindIcon(tab.kind, tab)} {tabTitle(tab)}
                    </button>
                  ))}
                </div>
              )}
            </div>
          )}
          {/* ＋ 的定位容器：新建菜单锚在**按钮**下方（贴按钮右缘），而不是标签条左侧 ——
              用户口径「菜单应该锚在「＋」按钮下面（右侧），不是左侧」。⌄ 的重开菜单保持锚在左侧。 */}
          <div className="rtabs-add-wrap">
            <button
              ref={addBtnRef}
              className="rtabs-add"
              data-tip={t('rp.tabs.add')}
              aria-label={t('rp.tabs.add')}
              aria-haspopup="menu"
              aria-expanded={menu === 'add'}
              onClick={() => { setAddFocus(0); setMenu((m) => (m === 'add' ? null : 'add')) }}
            >
              <Plus size={13} />
            </button>
            {menu === 'add' && <div
              ref={addMenuRef}
              className="rtabs-menu rtabs-menu-right"
              role="menu"
              aria-label={t('rp.tabs.add')}
              onKeyDown={onAddMenuKeyDown}
            >
              {addableKinds().map((k) => (
                <button
                  key={k}
                  className="rtabs-mi"
                  data-kind={k}
                  role="menuitem"
                  onClick={() => { openTab({ kind: k } as TabSpec); setMenu(null) }}
                >
                  {kindIcon(k)} {kindLabel(k)}
                </button>
              ))}
              {/* R16：分组「面板插件」（D-2 的第 1 处宿主渲染点）。
                  条目来自 surface:index 的 enabled 插件（快照见 surfaces/plugin/entries.ts）：
                  禁用/卸载 → 下一轮 surfaces_changed 之后这里整组消失，不是灰掉 ——
                  一个点不动的入口比没有入口更让人以为是坏了。 */}
              {pluginEntryGroups.length > 0 && (
                <>
                  <div className="rtabs-menu-sec" data-plugin-entries role="presentation">
                    {t('rp.tabs.plugins')}
                  </div>
                  {pluginEntryGroups.map((g) => (
                    <Fragment key={g.pluginId}>
                      {showPluginGroupNames && (
                        <div className="rtabs-menu-sub" data-plugin-group={g.pluginId} role="presentation">
                          {g.pluginName}
                        </div>
                      )}
                      {g.items.map((e) => (
                        <button
                          key={`${g.pluginId}:${e.entryId}`}
                          className="rtabs-mi rtabs-mi-plugin"
                          data-kind={e.kind}
                          data-entry={e.entryId}
                          data-plugin-entry={`${g.pluginId}/${e.entryId}`}
                          role="menuitem"
                          onClick={() => openPluginEntry(e)}
                        >
                          <PluginEntryIcon entry={e} fallback={kindIcon(e.kind)} /> {entryTitleText(e.title, lang)}
                        </button>
                      ))}
                    </Fragment>
                  ))}
                </>
              )}
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
              <button className="ctx-item" role="menuitem" onClick={ctxCloseAll}>{t('browser.menu.closeAll')}</button>
              <button className="ctx-item" role="menuitem" disabled={closedTabs.length === 0} onClick={ctxReopen}>{t('browser.menu.reopen')}</button>
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
              {/* PR-09：空态不是终点 —— 把「能开什么」直接列出来（与 ＋ 菜单同一份 addableKinds()，
                  图标/文案同源），点一下就能开工，用户不必先猜 ＋ 里有什么。
                  PR-10：这份清单已经按注册表的可用性过滤（被禁用的 kind 不再出现）。 */}
              {tabs.length === 0 && (
                <>
                  <div className="rtabs-empty-adds" role="group" aria-label={t('rp.tabs.add')}>
                    {addableKinds().map((k) => (
                      <button key={k} className="rtabs-empty-add" data-kind={k} onClick={() => openTab({ kind: k } as TabSpec)}>
                        {kindIcon(k)}
                        <span>{kindLabel(k)}</span>
                      </button>
                    ))}
                  </div>
                  {/* R16：D-2 的第 2 处渲染点 —— **空态能力面板**。这是新用户第一次看到右栏
                      时的全部内容，插件入口不在这里出现 = 「装了插件但界面上什么都看不见」
                      （D-2 的硬约束正是冲着这个状态写的）。分组口径与 ＋ 菜单同一份。 */}
                  {pluginEntryGroups.length > 0 && (
                    <div className="rtabs-empty-plugin" data-plugin-entries>
                      <div className="rtabs-empty-sec">{t('rp.tabs.plugins')}</div>
                      {pluginEntryGroups.map((g) => (
                        <Fragment key={g.pluginId}>
                          {showPluginGroupNames && (
                            <div className="rtabs-empty-sub" data-plugin-group={g.pluginId}>{g.pluginName}</div>
                          )}
                          <div className="rtabs-empty-adds" role="group" aria-label={g.pluginName}>
                            {g.items.map((e) => (
                              <button
                                key={`${g.pluginId}:${e.entryId}`}
                                className="rtabs-empty-add"
                                data-kind={e.kind}
                                data-entry={e.entryId}
                                data-plugin-entry={`${g.pluginId}/${e.entryId}`}
                                onClick={() => openPluginEntry(e)}
                              >
                                <PluginEntryIcon entry={e} fallback={kindIcon(e.kind)} />
                                <span>{entryTitleText(e.title, lang)}</span>
                              </button>
                            ))}
                          </div>
                        </Fragment>
                      ))}
                    </div>
                  )}
                </>
              )}
            </div>
          )}
          {/* R07c：非 file 面板（activity / git / web）统一走 provider 委派 —— 一条表达式覆盖三种
              kind，本地兜底见 panelFallback。为什么不按 kind 各写一行：三行的内容都是
              「getSurface(kind)?.render(...) ?? 本地组件」，逐行写等于把 kind 判别留在 body 里，
              而 PR-11 的终态判据是 body 里再没有 kind 等值分支（`grep -c "kind ==="` = 0）。
              顺序也重要：provider 先、兜底后。provider 是**副作用注册**（只有 main.tsx 导入
              builtin 时才存在），而若干探针页只 import RightPanel —— 没有兜底时它们渲染空白面板，
              且空白不会让任何断言变红（04-review F45/F46）。

              PR-10：被禁用/插件缺失时不渲染 provider，换成墓碑（原因 + 重新启用 + 移除标签）。
              正常路径外面多了一层 SurfaceBoundary —— 错误边界只包内容区（标签条/收起钮/＋ 菜单
              都在它外面：崩一个面板不能让人没法切走）。
              **隔离靠 tabId prop，不靠 key**：A 标签崩了切到 B，boundary 在 componentDidUpdate
              里看到 tabId 变了就把错误态复位，B 立刻渲染自己的内容。为什么不挂 key={tab.id}：
              key 一变 React 会重挂整棵 children —— 切 web 标签时 DOM 容器重建，R06c 的
              「原地复用」（原生视图与页面状态靠容器不重挂）当场破功。这一条由探针 T2b 钉住
              （同一 provider 组件的实例号在 a→b→a 之后必须不变）。 */}
          {activeTab && activeTab.kind !== 'file' && (
            activeUnavailable ? (
              <SurfaceTombstone
                kind={activeTab.kind}
                title={tabTitle(activeTab)}
                reason={activeUnavailable}
                auto={getSurfaceHealth(activeTab.kind)?.auto === true}
                onRemove={() => closeTabs([activeTab.id])}
              />
            ) : (
              <SurfaceBoundary tabId={activeTab.id} kind={activeTab.kind} title={tabTitle(activeTab)}>
                <SurfaceHost kind={activeTab.kind} tab={activeTab} scope={scope} />
              </SurfaceBoundary>
            )
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
              {activeUnavailable ? (
                <SurfaceTombstone
                  kind="file"
                  title={tabTitle(activeTab)}
                  reason={activeUnavailable}
                  auto={getSurfaceHealth('file')?.auto === true}
                  onRemove={() => closeTabs([activeTab.id])}
                />
              ) : (
                <SurfaceBoundary tabId={activeTab.id} kind="file" title={tabTitle(activeTab)}>
                  <SurfaceHost kind="file" tab={activeTab} scope={scope} fallback={<FilePreviewTab tab={activeTab} />} />
                </SurfaceBoundary>
              )}
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
