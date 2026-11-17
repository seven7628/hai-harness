// 右栏「现在是哪个面板」的**派生选择器**（纯函数模块，零 store 运行时依赖）—— R11b。
// 模块名 rightPanel = 它回答的问题（右栏在显示什么），不是被删字段的名字。
//
// 背景：PR-02 把 `activeId` 定成唯一权威，同时留了一个派生字段 `AppState.rightTab`（由
// `rightTabPatch` 从激活标签写穿）。留下的理由当时是「旧读点 / 会话恢复 / e2e 兼容一个版本」，
// 代价是**两份真相**：`tabsByScope[scope].activeId` 与 `rightTab` 会在三条路径上短暂不一致
// （会话恢复的 view 镜像、e2e 直写、组件 effect 的收敛时机），而「哪一份是真相」只有注释在说。
// R11b 删掉字段，读点从「state 里存的值」变成「算出来的值」—— 漂移在结构上不再可能。
//
// 依赖纪律：与 lib/webTab.ts 同一口径 —— 只允许 **type-only** 依赖 store
//（`import type`，esbuild/tsc 整句擦除），否则 store → lib → store 的运行时环立刻回来。
import type { ScopeKey, TabInstance, TabSet, TabSpec } from '../store/useAppStore'
import type { SurfaceKind } from '../surfaces/types'

/** 选择器需要的最小状态形状（AppState 结构上满足它；单测因此不必造整个 store）。 */
export interface TabsState {
  tabsByScope: Record<ScopeKey, TabSet>
  activeSessionId?: string
  workspace?: string
}

/**
 * tabScopeKey：标签集的归属键。无会话（加了工作区但还没建会话）时退化为工作区路径 ——
 * 否则右栏在「已添加工作区、尚未建会话」这个窗口期没有归属（D7）。
 *
 * 这里是**唯一实现**：store 里所有 `tabsByScope[...]` 的取键、选择器、UI 快照的 scope 键
 * 都走它。两份实现分叉的症状很隐蔽 —— 一个 scope 的标签在另一处被当成「没有标签集」，
 * 于是跟着补一个默认标签、看起来像「标签被吃掉了」。
 */
export function tabScopeKey(s: { activeSessionId?: string; workspace?: string }): ScopeKey {
  return s.activeSessionId || s.workspace || ''
}

/** 插件入口 → openTab 的 spec：`params.entry` 必填（D-2：同一 kind 的多个标签靠它区分）。 */
export function pluginEntrySpec(entry: { kind: SurfaceKind; entryId: string }): TabSpec {
  // TabInstance 联合里没有 plugin 成员（口径见 store 的 toPersistedTab），形状 cast 只此一处。
  return { kind: entry.kind, params: { entry: entry.entryId } } as unknown as TabSpec
}

/**
 * activeTabOf：当前 scope 的激活标签实例。
 *
 * `undefined` 的三种来源都是有意义的**状态**，不要把它们并成一个兜底值：
 *   · 该 scope 还没有标签集（新建会话、`tabsByScope` 被 deleteSession 回收）；
 *   · `activeId === null`（用户把标签关光了 → 内容区渲染空态）；
 *   · `activeId` 悬空（指向一个已不存在的实例 —— 只可能来自外部直接 setState 的脏数据）。
 */
export function activeTabOf(s: TabsState): TabInstance | undefined {
  const set0 = s.tabsByScope[tabScopeKey(s)]
  if (!set0?.activeId) return undefined
  return set0.tabs.find((t) => t.id === set0.activeId)
}

/**
 * selectRightTab：当前 scope 激活标签的 kind —— 右栏「现在是哪个面板」的唯一读点。
 *
 * 返回 `SurfaceKind`（内置四类 | `plugin:<id>`）而不是 `BuiltinKind`：plugin 是**第五种标签**
 * （PR-14c），它运行时就在同一个 `TabSet.tabs` 数组里（静态类型 TabInstance 只列了四类，
 * 见 toPersistedTab 的注释）。把返回值收窄成四类会让「插件标签当选时面板是什么」这个问题
 * 在类型上消失 —— 而复用 `rightTab` 的那次改造里，它正是被这么一个错误断言盖住的。
 *
 * 无值（标签集缺失 / 关光 / 悬空）→ `undefined`。**不要**加兜底：旧字段在「关光标签」时
 * 会保留最后的值，于是折叠态 rail 会一直高亮一个已经不在的面板 —— 那是缺陷，不是行为。
 */
export function selectRightTab(s: TabsState): SurfaceKind | undefined {
  const t = activeTabOf(s)
  return t ? (t.kind as SurfaceKind) : undefined
}
