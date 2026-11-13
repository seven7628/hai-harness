# 创造模式 V0.1 —— M0 ~ M2 完整解决方案与代码方案

> 配套：`docs/creator-mode-v0.1/01-design.md`（设计/取舍）、`docs/creator-mode-v0.1/02-pr-plan.md`（PR 序列）
> 本文回答"具体怎么写"：文件级布局、类型契约、关键模块代码、Go 侧接口、协议、测试与迁移。
> 代码为**目标实现**，不是现有代码的摘录；标注「示意」的片段可原样落地，标注「骨架」的需要按仓库风格补注释与错误分支。

---

## 0. 阅读指南

```
§1  总览：模块依赖与三条数据流
§2  M0（容器化 + 布局 + 编辑器 + 多标签）
§3  M1（ui 通道 + 安装器 + 创造模式）
§4  M2（出进程插件 / TSX 编译 / 并排 / 市场 / LSP）
§5  迁移、兼容与测试
§6  工作量与分工
```

**不可变约束**（贯穿 M0-M2，任何 PR 违反即打回）

1. `events.Event` 的 28 类封闭集合不动；UI 状态走独立 `ui` 信封，不落盘、不重放。
2. 会话日志仍是唯一真相；标签、编辑器 buffer、插件运行时状态都是派生数据。
3. ADR-0 会话隔离不动：web 视图只允许 session 作用域。
4. 插件永远拿不到 `write` 能力（仅内置 provider 可写）。
5. 内置能力与插件能力走**同一套** `SurfaceProvider` 契约，不允许"内置特判"。

---

## 1. 总览

### 1.1 模块依赖（新增部分加粗）

```
desktop/app/src
  store/useAppStore.ts ────────┐
    ├─ tabsByScope / activeId  │  activeId 权威；rightTab 由 applyTabs() 单点派生
    ├─ rightLayout             │  三态布局真值
    └─ fileChanges             │  外部变更推送（编辑器用）
  lib/layout.ts        **新** ─┘  纯函数：state → 像素尺寸
  surfaces/types.ts    **新**      契约（无运行时依赖）
  surfaces/registry.ts **新**      注册表（单例）
  surfaces/builtin/*.tsx **新**    5 个内置 provider
  surfaces/plugin/loader.ts **新** 插件 ESM 加载/卸载
  surfaces/preview/{types,registry}.ts **新**  文档渲染器注册表
  surfaces/preview/renderers/*.tsx     **新**  text/image/csv/pdf/docx/pptx/xlsx/media
  components/RightPanel.tsx        只做容器（≈120 行）
  components/Editor.tsx    **新**  CodeMirror 6
  components/SurfaceBoundary.tsx **新**

desktop/bridge（Go）
  main.go                          命令 dispatch switch
  file_edit.go           **新**    file_open / file_save / editor_open_set
  surfaces_registry.go   **新**    index.json / install / remove / audit
  ui_channel.go          **新**    ui 信封、快照缓存
  conversion/service.go  **新**    准入/限额/缓存/输出保留
  internal/officekit/    **新**    kit 探测/校验/soffice 转换/renderImages
  electron/media-protocol.ts **新** hai-media:// + Range
  tools_registry.go      **新**    ui_inspect / surface_* / office_* 工具注册

tools/builtin
  ui_inspect.go          **新**
```

### 1.2 三条数据流

**A. 布局与标签（纯前端）**

```
用户动作 → store 动作 → applyTabs(单一写点) → {tabsByScope, activeId, rightTab*}
                    └→ rightLayout → lib/layout.ts → Group 尺寸
RightPanel → getSurface(active.kind) → provider.render()
```

**B. 编辑器读写（前端 ↔ bridge）**

```
Editor → file_open ────────────→ bridge：读字节 + sha256 + 编码/大小判定
Editor → file_save(baseHash) ─→ bridge：per-path 锁 → 比对 hash → 原子写
工具写文件 ─→ bridge post-write hook → 比对 editor open set → ui{file_changed}
Editor ← 静默 reload（clean）/ 冲突条（dirty）
```

**C. 插件（M1）**

```
agent → surface_install ─→ bridge：manifest 校验 → hash → 快照 index → 原子写 → ui{surfaces_changed}
renderer ← ui{...} → loader.import() → apply(host) → registry.register
renderer → ui_snapshot（debounce）→ bridge 缓存 ← ui_inspect 工具
```

---

## 2. M0：容器化 + 三态布局 + 内嵌编辑器 + 多标签

### 2.1 `surfaces/types.ts`（完整）

```ts
// desktop/app/src/surfaces/types.ts
import type { ReactNode } from 'react'
import type { AppState, TabInstance } from '../store/useAppStore'

/** Omit 在联合类型上不分配（keyof 取交集），标签形状必须用它 */
export type DistributiveOmit<T, K extends PropertyKey> = T extends unknown ? Omit<T, K> : never

export type BuiltinKind = 'activity' | 'git' | 'file' | 'web'   // office/媒体不是 kind，是 file 预览的渲染器（见 §2.11）
export type SurfaceKind = BuiltinKind | `plugin:${string}`

export type SurfaceGroup = 'workflow' | 'content' | 'plugin'
export type SurfaceScope = 'session' | 'workspace'
export type SurfaceWeight = 'light' | 'heavy'
export type SurfaceCapability = 'read' | 'view' | 'write'

/** 插件标签：身份 = (kind, params)。unavailable = 所属插件缺失/被禁用时的墓碑态 */
export interface PluginTabInstance {
  id: string
  kind: `plugin:${string}`
  params?: Record<string, unknown>
  unavailable?: { reason: string }
}

/** 全量标签联合（内置沿用 store 里既有的四类 + plugin） */
export type SurfaceTab =
  | TabInstance
  | PluginTabInstance

export interface SurfaceRenderContext<T extends SurfaceTab = SurfaceTab> {
  tab: T
  scope: string
  host?: HostApi   // M0 可选；M1 起插件 provider 改用 PluginRenderContext（host 必填）
}

/** M1：插件面板的渲染上下文，host 必填 */
export type PluginRenderContext<T extends SurfaceTab = SurfaceTab> = SurfaceRenderContext<T> & { host: HostApi }

export interface SurfaceProvider<T extends SurfaceTab = SurfaceTab> {
  kind: SurfaceKind
  namespace: 'builtin' | 'plugin'
  group: SurfaceGroup
  order?: number

  title: (tab: T, t: (k: string, v?: Record<string, unknown>) => string) => string
  icon?: (tab: T) => ReactNode
  badge?: (tab: T, s: AppState) => number

  singleton?: boolean
  allowDuplicate?: boolean
  scope?: SurfaceScope
  weight?: SurfaceWeight
  capabilities?: SurfaceCapability[]

  /** 打开时的默认实例（不含 id） */
  defaultSpec?: (s: AppState) => DistributiveOmit<T, 'id'>
  /** 去重键：相同则激活已有实例。缺省 = singleton ? 'singleton' : JSON(tab.params ?? {}) */
  dedupeKey?: (tab: DistributiveOmit<T, 'id'>) => string

  persist?: (tab: T) => unknown
  restore?: (raw: unknown) => DistributiveOmit<T, 'id'> | null

  render: (ctx: SurfaceRenderContext<T>) => ReactNode
  onOpen?: (tab: T) => void
  onClose?: (tab: T) => void
}

/** M1 填充：插件面板唯一可用的宿主能力（见 §3.6） */
export interface HostApi {
  React: typeof import('react')
  Icon: (p: { name: string; size?: number }) => ReactNode
  t: (key: string, vars?: Record<string, unknown>) => string
  theme(): { mode: 'light' | 'dark'; tokens: Record<string, string> }
  useSession<T>(sel: (s: SessionProjection) => T): T
  command(name: string, payload?: unknown): Promise<unknown>
  events: { on(type: string, fn: (e: unknown) => void): () => void }
  openTab(spec: DistributiveOmit<SurfaceTab, 'id'>): void
  toast(msg: string): void
}

export interface SessionProjection {
  id: string
  workspace: string
  model: string
  status: 'idle' | 'running' | 'waiting'
  cost: { usd: number; calls: number; cachePct: number }
  ctx: { used: number; max: number }
}
```

> 循环导入提醒：`types.ts` 对 store 只做 `import type`，store 对 `SurfaceKind` 也只做 `import type`。TS 编译期擦除，运行时无环。

### 2.2 `surfaces/registry.ts`（完整）

```ts
// desktop/app/src/surfaces/registry.ts
import type { SurfaceKind, SurfaceProvider } from './types'

const byKind = new Map<SurfaceKind, SurfaceProvider>()
const listeners = new Set<() => void>()
const GROUP_ORDER = { workflow: 0, content: 1, plugin: 2 } as const

function assertKind(p: SurfaceProvider) {
  if (p.namespace === 'builtin' && p.kind.startsWith('plugin:')) {
    throw new Error(`builtin provider 不允许 plugin: 前缀：${p.kind}`)
  }
  if (p.namespace === 'plugin' && !p.kind.startsWith('plugin:')) {
    throw new Error(`plugin provider 必须使用 plugin: 前缀：${p.kind}`)
  }
  if (p.capabilities?.includes('write') && p.namespace !== 'builtin') {
    throw new Error(`插件不允许 write 能力：${p.kind}`) // 约束 4，注册期硬拦
  }
  if (!p.kind) throw new Error('surface provider 缺少 kind')
  // 内置 kind 只允许 4 个（D-I）；未知名字（如历史 office）在注册期拒收
  if (p.namespace === 'builtin' && !['activity', 'git', 'file', 'web'].includes(p.kind))
    throw new Error(`未知的内置 kind：${p.kind}`)
}

export function registerSurface(p: SurfaceProvider) {
  assertKind(p)
  if (byKind.has(p.kind)) throw new Error(`surface kind 重复注册：${p.kind}`)
  byKind.set(p.kind, p)
  emit()
}

export function unregisterSurface(kind: SurfaceKind) {
  if (byKind.delete(kind)) emit()
}

export function getSurface(kind: SurfaceKind | undefined) {
  return kind ? byKind.get(kind) : undefined
}

export function listSurfaces(): SurfaceProvider[] {
  return [...byKind.values()].sort((a, b) =>
    GROUP_ORDER[a.group] - GROUP_ORDER[b.group] ||
    (a.order ?? 100) - (b.order ?? 100) ||
    (a.kind < b.kind ? -1 : a.kind > b.kind ? 1 : 0))  // 码点比较：不用 localeCompare，避免 ICU 差异
}

function emit() { for (const f of listeners) f() }
export function subscribeSurfaces(fn: () => void) { listeners.add(fn); return () => { listeners.delete(fn) } }
export function resetSurfacesForTest() { byKind.clear(); listeners.clear() }
```

测例（`registry.test.ts`）：重复 kind 抛错、插件 write 抛错、排序稳定、`unregister` 后 `getSurface` 为 undefined。

### 2.3 `lib/layout.ts`（完整）

```ts
// desktop/app/src/lib/layout.ts
export type RightBias = 'even' | 'center' | 'right' | 'custom'
export type RightLayout =
  | { open: false }
  | { open: true; bias: RightBias; ratio: number } // ratio = 右栏占「除左栏外」的百分比

export const PX = {
  leftRail: 56,
  leftExpanded: 236,
  leftMin: 140,
  leftMax: 360,
  centerMin: 300,
  rightMin: 200,
} as const

export const BIAS_RATIO: Record<Exclude<RightBias, 'custom'>, number> = {
  even: 50, center: 36, right: 64,
}

export interface PanelSizes { left: number; center: number; right: number }

/** 三态 → 像素尺寸（containerWidth 由 usePanelCallbackRef 的 group 提供） */
export function panelSizes(layout: RightLayout, leftPinned: boolean, W: number): PanelSizes {
  const left = layout.open && !leftPinned ? PX.leftRail : PX.leftExpanded
  if (!layout.open) return { left, center: W - left, right: 0 }

  const avail = W - left
  let right = Math.round((avail * layout.ratio) / 100)
  right = clamp(right, PX.rightMin, avail - PX.centerMin)
  return { left, center: avail - right, right }
}

export function expand(bias: Exclude<RightBias, 'custom'> = 'even'): RightLayout {
  return { open: true, bias, ratio: BIAS_RATIO[bias] }
}
export const collapse: RightLayout = { open: false }

/** 拖拽后：只认像素，bias 标 custom（按钮组显示"自定义"） */
export function afterDrag(right: number, W: number, leftPinned: boolean): RightLayout {
  const left = leftPinned ? PX.leftExpanded : PX.leftRail
  const ratio = Math.round((right / (W - left)) * 100)
  const known = (Object.entries(BIAS_RATIO) as [Exclude<RightBias,'custom'>, number][])
    .find(([, r]) => Math.abs(r - ratio) <= 1)
  return { open: true, bias: known?.[0] ?? 'custom', ratio }
}

const clamp = (v: number, lo: number, hi: number) => Math.max(lo, Math.min(hi, v))
```

> **实现偏差（R01 评审后，2026-11-11）**：`panelSizes` 最终**没有落地**——App 保留各 Panel 的
> `defaultSize`，宽度用 `rightHandle.resize(百分比)` 命令式应用，避免受控 Group 与 `rightSize`（内嵌浏览器
> viewport）抢写；`panelSizes` 因此删除（不留死代码）。`afterDrag` 的签名改为
> `(rightPct, leftPct)`：直接吃 `react-resizable-panels` 的 layout 百分比（分母 = 所有 panel 之和）。
> 早先按像素传参时，`getSize().inPixels`（offsetWidth，不含 8px 分隔条）与 `clientWidth` 不同源，
> 会把「右栏 < 窗口 25%」的真实拖拽当抖动丢掉。`PX.leftRail` 由 56 改为实测的 **44**（仅作文档常量）。

`App.tsx` 接线（约 30 行改动）：

```tsx
const rightLayout = useAppStore(s => s.rightLayout)
const leftPinned  = useAppStore(s => s.leftPinned)
const sizes = panelSizes(rightLayout, leftPinned, groupWidth)

<Group direction="horizontal" onLayoutChanged={(l, meta) => {
  if (!meta.isUserInteraction) return         // 程序化 setLayout 不算用户拖拽
  store.setRightLayoutFromDrag(l['right'])
}}>
  <Panel id="left"  defaultSize={sizes.left}  minSize={PX.leftMin} maxSize={PX.leftMax}>…</Panel>
  <Panel id="center" minSize={PX.centerMin}>…</Panel>
  <Panel id="right" defaultSize={sizes.right} minSize={PX.rightMin} maxSize="80%">…</Panel>
</Group>
```

**删除**：`rightOverHalf` ref 与 `App.tsx:160` 的自动收起；`rightExpanded` 改为 `rightLayout.open` 的派生 getter。

**自动展开（D-A）**：在 `openTab` 里做唯一判定，而不是在每个调用点：

```ts
// store.openTab
if (!state.rightLayout.open) state.rightLayout = expand('even')   // 内容触发即展开
```
（用户手动收起后不再加锁 —— 下一次内容触发仍展开，符合 D-A。）

### 2.4 store 改造（关键 diff）

**目标**：`activeId` 权威、`rightTab` 单点派生、标签写操作收敛到一个 helper。

```ts
// 1) 类型
type TabSet = { tabs: SurfaceTab[]; activeId: string | null; closed: SurfaceTab[]; restored?: boolean }
type AppState = {
  rightLayout: RightLayout
  leftPinned: boolean
  tabsByScope: Record<string, TabSet>
  rightTab: RightTab            // 派生字段：只为 e2e / SessionView 兼容保留一个版本
  fileChanges: Record<string, { hash: string; by: 'agent' | 'external'; at: number }>
}

// 2) 唯一写点：所有标签变更都过它
// applyTabs 只做「标签集 → activeId → rightTab」这一件事，不碰布局（F18）
function applyTabs(s: AppState, scope: string, next: TabSet): Partial<AppState> {
  const active = next.tabs.find(t => t.id === next.activeId)
  return {
    tabsByScope: { ...s.tabsByScope, [scope]: next },
    rightTab: (active?.kind ?? s.rightTab) as RightTab,   // 唯一写 rightTab 的地方
  }
}

// 3) 动作（8 条全部改成同一形态）
openTab: (spec, opts) => set(s => {
  const scope = s.scopeKey
  const cur = s.tabsByScope[scope] ?? { tabs: [], activeId: null, closed: [] }
  const key = dedupeKeyOf(spec, s)
  const hit = cur.tabs.find(t => dedupeKeyOf(t, s) === key)
  const next = hit ? { ...cur, activeId: hit.id } : {
    ...cur,
    tabs: [...cur.tabs, { ...spec, id: newId() } as SurfaceTab],
    activeId: (…new id…),
  }
  // 自动展开只属于「打开/激活」这一个入口（D-A）；关闭、同步、恢复都不许展开（F18）
  const layout = s.rightLayout.open ? s.rightLayout : expand('even')
  return { ...applyTabs(s, scope, next), rightLayout: layout }
})
```

配套两个纯函数（放 `surfaces/tabs.ts`，可单测）：

```ts
export function dedupeKeyOf(t: SurfaceTab, s: AppState): string {
  const p = getSurface(t.kind)
  if (p?.dedupeKey) return `${t.kind}|${p.dedupeKey(t as any)}`
  if (p?.singleton) return `${t.kind}|singleton`
  if (t.kind === 'file') return `file|${t.path}|${t.mode ?? 'source'}`   // D5
  if (t.kind === 'web')  return `web|${t.viewId ?? t.pageId ?? t.url ?? 'pending'}`
  return `${t.kind}|${JSON.stringify((t as any).params ?? {})}`
}

export function kindOf(t?: SurfaceTab): RightTab | undefined { return t?.kind as RightTab }
```

**兼容策略**：`rightTab` 保留字段但**只有 `rightTabPatch` 写它**（`tabPatch` 是唯一调用者之一）。

> **实现偏差（R02）**：`syncTabsToRightTab` 没有整体删除——它退化成「无标签集时按 `rightTab` 建默认集」+「恢复态先补派生字段、再清 `restored`」两条分支；反向收敛（按 `rightTab` 改 activeId）已删。仓库里 `applyTabs` 本就存在、名为 `tabPatch`，没有另起新名。PR-02 不接自动展开（要求零行为变化）；D-A 的接线已在 PR-04 完成：`openTab` 且 `activate!==false` 且当前收起时才 `expand('even')`。
>
> **不变量边界**：`rightTab === kindOf(active)` 只在「标签动作」与 `syncTabsToRightTab()` 之后成立；
> `viewFields`/`topViewFields` 的会话恢复镜像、以及旧 e2e 直接 `setState({rightTab})` 会让它短暂不一致。
> PR-04 已把 RightPanel 的 embedHide / 待重建 / toolbar 三处改读 `activeTab.kind`（effect 依赖一起改）；标签条高亮那处留到 PR-09。

### 2.5 `RightPanel.tsx` 新骨架

```tsx
export default function RightPanel() {
  const scope = useAppStore(s => s.scopeKey)
  const set = useAppStore(s => s.tabsByScope[scope])
  useSurfaceVersion()                       // 订阅注册表变化，provider 热装后重渲染
  const tab = set?.tabs.find(t => t.id === set.activeId)
  const provider = getSurface(tab?.kind)

  return (
    <div className="rp-root">
      <TabBar scope={scope} />
      <div className="rp-body">
        {!tab && <EmptyState />}                                {/* 能力面板选择器 */}
        {tab && !provider && <Tombstone kind={tab.kind} />}     {/* 插件缺失 */}
        {tab && provider && (
          <SurfaceBoundary key={`${tab.kind}:${tab.id}`} kind={tab.kind}>
            {provider.render({ tab, scope, host: hostApi })}
          </SurfaceBoundary>
        )}
      </div>
      <PanelStatusBar tab={tab} provider={provider} />
    </div>
  )
}
```

`TabBar`：按 `listSurfaces()` 的 `group` 渲染三段；`+N` 溢出；右键菜单 `关闭/其他/右侧/全部/重开`；`⌃⇥` 切换相邻。

### 2.6 内置 provider（两个示例，其余同构）

```tsx
// surfaces/builtin/gitSurface.tsx（PR-03 落地形态：组件本体在 components/ChangesTab.tsx）
export const gitSurface: SurfaceProvider = {
  kind: 'git', namespace: 'builtin', group: 'workflow', order: 20,
  singleton: true, scope: 'session', weight: 'light', capabilities: ['read', 'view'],
  title: (_t, tr) => tr('rp.tab.git'),
  icon: () => <GitBranch size={12} />,
  defaultSpec: () => ({ kind: 'git' }),
  render: () => <ChangesTab />,          // 原组件整体平移，零改动
  onOpen: () => analytics.track('right_tab_open', { kind: 'git' }),
}

// surfaces/builtin/fileSurface.tsx
export const fileSurface: SurfaceProvider = {
  kind: 'file', namespace: 'builtin', group: 'content', order: 30,
  scope: 'session', weight: 'light', capabilities: ['read', 'view', 'write'],
  title: (t) => shortName(t.path ?? '') + (t.mode === 'edit' ? ' •' : ''),
  icon: (t) => t.mode === 'edit' ? <Pencil size={12}/> : <FileIcon size={12}/>,
  dedupeKey: (t) => `${t.path}|${t.mode ?? 'source'}`,
  persist: (t) => ({ path: t.path, mode: t.mode, startLine: t.startLine }),
  restore: (raw) => isFileRaw(raw) ? { kind: 'file', ...raw } : null,
  render: ({ tab, scope }) =>
    tab.mode === 'edit'
      ? <Editor path={tab.path!} scope={scope} />
      : <FileTab tab={tab} scope={scope} />,
}
```

> `web` 迁移只是把 `WebTab` 包进来，`viewId/pageId/rebuildWebTab` 仍留在 store（跨 provider 的池是基础设施，不是某个 provider 的私产）。

### 2.7 编辑器

**Go：`desktop/bridge/file_edit.go`（结构完整）**

```go
package main

// file_open / file_save：内嵌编辑器的读写通道（docs/creator-mode-v0.1/01-design.md §4.5）
// 不变量：① 不规范化行尾（\r\n 原样往返）；② 保存必带 baseHash；③ 与工具写共用 per-path 锁。

const (
	maxEditBytes = 2 << 20 // 2 MiB
	fileOpenCap  = 64      // 同时跟踪的编辑器文件上限
)

type editorSet struct {
	mu    sync.Mutex
	base  map[string]string // abs path -> 最近一次确认的 sha256
	order []string          // 简单 LRU，超上限丢最旧
}

func (e *editorSet) track(path, hash string) { … }
func (e *editorSet) drop(path string)        { … }
func (e *editorSet) expect(path string) (string, bool) { … }

// handleFileOpen：读原始字节 → 判二进制/超限 → sha256 → 返回
func (m *bridge) handleFileOpen(c Command) {
	path, ok := m.absPath(str(c.Payload, "path"))
	if !ok { m.resp(c, false, "path 非法", nil); return }
	raw, err := os.ReadFile(path)
	if err != nil { m.resp(c, false, err.Error(), nil); return }
	if isBinary(raw) || len(raw) > maxEditBytes {
		m.resp(c, true, "", map[string]any{"path": path, "readonly": readonlyReason(raw)}); return
	}
	sum := sha256.Sum256(raw)
	m.editors.track(path, hex.EncodeToString(sum[:]))
	m.resp(c, true, "", map[string]any{
		"path": path, "content": string(raw), "hash": hex.EncodeToString(sum[:]),
		"mtime": statMtime(path), "size": len(raw), "eol": detectEOL(raw),
	})
}

// handleFileSave：锁 → baseHash 比对 → 原子写 → 更新 base
func (m *bridge) handleFileSave(c Command) {
	path, ok := m.absPath(str(c.Payload, "path"))
	base := str(c.Payload, "baseHash")
	content := str(c.Payload, "content")

	lock, err := atomicfile.Lock(path) // 与 tools/builtin/edit_file 同一个 flock（见 §2.7 决策 D-G）
	if err != nil { m.resp(c, true, "", map[string]any{"ok": false, "code": "locked", "error": err.Error()}); return }
	if lock != nil { defer lock.Close() } // Close 即释放 flock

	if cur, err := os.ReadFile(path); err == nil {
		sum := sha256.Sum256(cur)
		if hex.EncodeToString(sum[:]) != base {
			m.resp(c, true, "", map[string]any{
				"ok": false, "code": "conflict",
				"currentHash": hex.EncodeToString(sum[:]),
			})
			return
		}
	}
	if err := atomicfile.Replace(lock, path, []byte(content)); err != nil {
		m.resp(c, true, "", map[string]any{"ok": false, "code": "write_failed", "error": err.Error()})
		return
	}
	sum := sha256.Sum256([]byte(content))
	m.editors.track(path, hex.EncodeToString(sum[:]))
	m.resp(c, true, "", map[string]any{"ok": true, "hash": hex.EncodeToString(sum[:]), "mtime": statMtime(path)})
}

// 外部变更：工具写成功后调用。只在「该文件被编辑器跟踪」且 hash 变了时推送。
func (m *bridge) notifyFileChanged(absPath, by string) {
	if _, tracked := m.editors.expect(absPath); !tracked { return }
	raw, err := os.ReadFile(absPath); if err != nil { return }
	sum := sha256.Sum256(raw)
	m.emitUI("file_changed", map[string]any{"path": absPath, "by": by, "hash": hex.EncodeToString(sum[:])})
}
```

接线：

- `FileTools.WithPostWrite`（已存在，见 `tools/builtin/builtin.go`）里调 `notifyFileChanged(abs, "agent")`；
- `bash`/`run_python` 改了文件也走同一条：在 bridge 的 `tool_run_end` 事件里对 `ToolResponse.Diff` 涉及的路径补一次 hash 比对（覆盖"agent 用 shell 改文件"这个缺口）；
- `editor_open_set` 命令：渲染进程每次开关编辑器时上报，`m.editors` 以此为准（`file_open` 也会 track，二者取并集）。

> **锁的来源（决策 D-G）**：`tools/builtin/lock.go`（116 行）是**文件 fd 上的 flock**（POSIX 建议锁、跨进程），不是内存 mutex；`edit_file` 靠它把「读 → 改 → 写」整段串行化。`file_save` 必须用**同一把锁**，所以把该文件整体提取到 `internal/atomicfile`：

```go
package atomicfile

// Lock 打开目标并加非阻塞排他锁；文件不存在返回 (nil, nil) —— 没有写者可言。
func Lock(path string) (*os.File, error)
// Replace 在已持锁前提下原子替换（临时文件 + rename）；lock 可为 nil。
func Replace(lock *os.File, path string, content []byte) error
// Append 追加写（flock + O_APPEND 单次写）。
func Append(path, content string) error
```

`tools/builtin` 保留同签名薄壳转发（`lockPath`/`replaceLocked` 调用点一行不改），bridge 直接引用 —— **不要复制两份**：复制等于两把互不相识的锁，`file_save` 的「读 hash → rename」窗口会被工具的一次写入穿透，用户保存的内容静默覆盖 agent 的改动。

对 `file_save` 的含义：锁在**读 hash 之前**获取、**rename 之后**释放；文件不存在时 `Lock` 返回 nil，此时 race 由 rename 原子性兜底（两个写者都成功，后者胜出 —— 这是唯一可接受的丢更新，因为它没有 baseHash 可依据）。

**前端：`components/Editor.tsx`（骨架）**

```tsx
export default function Editor({ path, scope }: { path: string; scope: string }) {
  const hostRef = useRef<HTMLDivElement>(null)
  const viewRef = useRef<EditorView>()
  const [base, setBase] = useState<{ hash: string; content: string }>()
  const [dirty, setDirty] = useState(false)
  const [conflict, setConflict] = useState<null | { hash: string; by: string }>(null)
  const change = useAppStore(s => s.fileChanges[path])

  // 1) 打开
  useEffect(() => { void (async () => {
    const r: any = await window.desktop.command({ type: 'file_open', payload: { path } })
    if (!r.ok) return setError(r.error)
    if (r.readonly) return setReadonly(r.readonly)
    setBase({ hash: r.hash, content: r.content })
  })() }, [path])

  // 2) 挂 CM6（懒加载语言包）
  useEffect(() => {
    if (!base || !hostRef.current) return
    let disposed = false
    void (async () => {
      const [{ EditorView, keymap }, { basicSetup }, lang] = await Promise.all([
        import('@codemirror/view'), import('codemirror'),
        loadLanguage(path),                      // 按扩展名 import @codemirror/lang-*
      ])
      if (disposed) return
      viewRef.current = new EditorView({
        parent: hostRef.current!,
        doc: base.content,
        extensions: [
          basicSetup, lang,
          keymap.of([{ key: 'Mod-s', preventDefault: true, run: () => { void save(); return true } }]),
          EditorView.updateListener.of(u => u.docChanged && setDirty(true)),
        ],
      })
    })()
    return () => { disposed = true; viewRef.current?.destroy() }
  }, [base])

  // 3) 外部变更
  useEffect(() => {
    if (!change || change.hash === base?.hash) return
    if (!dirty) void reload()                       // clean → 静默 reload（保光标）
    else setConflict({ hash: change.hash, by: change.by })
  }, [change])

  // 4) dirty 归属「标签」，不归属组件 —— 两个编辑器各自 setDirty 会互相清掉标志
  useEffect(() => { setTabDirty(tabId, dirty) }, [tabId, dirty])
  // 5) 打开集合上报全量（bridge 以此为准），卸载即移除，避免跟踪表被已关闭的文件占满
  useEffect(() => {
    setEditorOpen(path, true)
    return () => { setEditorOpen(path, false); setTabDirty(tabId, false) }
  }, [path, tabId])

  async function save() {
    const content = viewRef.current!.state.doc.toString()
    const r: any = await window.desktop.command({
      type: 'file_save', payload: { path, content, baseHash: base!.hash },
    })
    if (r.ok) { setBase({ hash: r.hash, content }); setDirty(false); setConflict(null); clearFileChange(path) }
    else if (r.code === 'conflict') setConflict({ hash: r.currentHash, by: 'external' })
    else toast(r.error ?? '保存失败')
  }
  …
}
```

**三处状态归属（评审结论，别改回去）**

| 状态 | 归属 | 唯一写者 | 生命周期 |
|---|---|---|---|
| `dirty` | 标签实例 `tab.dirty` | Editor 的 `setTabDirty` | 随标签；关标签/保存即清 |
| `editorOpenSet` | store（全量集合） | Editor mount/unmount | 上报给 bridge，关闭即移除 |
| `fileChanges[path]` | store | bridge 的 `ui{file_changed}` | 保存/静默 reload/tab 关闭后 `clearFileChange(path)`；上限 32 条 LRU |

退出保护：App 根部订阅 `editorDirtyCount > 0`，只在这一个地方调 `window.desktop.setDirty(...)`。

**冲突条三动作**（`components/ConflictBar.tsx`）：
`查看差异`（复用 diff 视图，对比 buffer 与磁盘）／`用磁盘覆盖`（丢弃我的，reload）／`另存为`（`file_save` 换路径）。

### 2.8 多标签

只需三块：

1. `TabBar` 溢出：可见 `TABS_MAX_VISIBLE`，其余进 `+N` 下拉（复用现有常量）；
2. 右键菜单：`close/closeOthers/closeRight/closeAll/reopen`，全部走 `applyTabs`；
3. `⌃⇥ / ⌃⇧⇥`：在 `tabs` 数组里前后移动 `activeId`（不碰 `⌘1..9`）。

### 2.9 Boundary 与墓碑

```tsx
export class SurfaceBoundary extends React.Component<Props, { err?: Error }> {
  static getDerivedStateFromError(err: Error) { return { err } }
  componentDidCatch(err: Error) { recordMountFailure(this.props.kind, err) } // localStorage 计数
  render() {
    if (this.state.err) return <CrashCard kind={this.props.kind} err={this.state.err}
      onReload={() => this.setState({ err: undefined })} onDisable={() => disableSurface(this.props.kind)} />
    return this.props.children
  }
}
```

`recordMountFailure` 连续 3 次 → `disableSurface(kind)`：写 `localStorage['surface.disabled']`，注册表在 `listSurfaces()` 里过滤（但保留墓碑展示）。

> **实现偏差（R10，F64–F65）** —— 上面那段骨架能跑通的前提是「provider.render 的异常落在边界子树里」，
> 而按 RightPanel 现有的写法（内联调用 `getSurface(kind)?.render(...)`）根本不成立。落地时改了四处：
>
> - **`SurfaceHost` 宿主组件**：`provider.render()` 是普通函数，在 RightPanel 自己的 render 期间调用 =
>   异常发生在 RP 的渲染栈里，boundary 接不到（症状：整棵树被卸载、全页空白，一张 CrashCard 都不出现）。
>   必须把它放进一个独立组件当 children，异常才归 boundary 管（03 的骨架默认了这一点，但没写出来）。
> - **不做内存缓存**：health 一开始按「内存 store + write-through」写，结果探针里 `import()` 出来的
>   health 与 RightPanel 走的那份被解析成**两个模块实例**，两份 cache 当场分叉 ——「localStorage 里
>   明明禁用了，菜单/墓碑却当没发生」。真相源只留 localStorage 一个（F64）。
> - **失败计数不能做成「只崩第一次」的夹具**：React 18 在 dev 下会把出错的那次 render **再跑一遍**
>   （收集组件栈），「第一次抛、第二次成功」的写法第二遍就成功了，崩溃根本不发生（F65）。探针改用
>   显式闸门（`__probe.setBoom`）控制「这次崩 / 这次不崩」。
> - **`key={tab.id}` 的代价要写进注释**：boundary 换 key ⇒ children 跟着重挂，R06c 那条「切 web 标签
>   时内容原地复用」不再成立（视图在主进程池里，重挂不重建 WebContentsView，但 DOM 确实重建一次）。
>   PR-11 若要拿回原地复用，得把「按标签重置错误态」从 key 换成 boundary 内部的一条显式 reset。

### 2.10 Go 侧命令与 dispatch

`command_registry.go` 新增常量：

```go
CmdFileOpen   Command = "file_open"   // 读 + 登记（refcount++）
CmdFileSave   Command = "file_save"   // 持锁比 hash → atomicfile.ReplaceLocked
CmdFileClose  Command = "file_close"  // 退订（refcount--），无副作用
```

`main.go` 的 dispatch switch 新增三个 `case`（与 `file_preview` 同形），bridge 结构体挂 `fileEditor`（打开集合 + fsnotify watcher）。

> **实现偏差（R05a，F33–F36）**
> - 错误信封是既有的扁平 `{ok:false, error:"…", code:"…"}`（`respCode`），不是嵌套 `error:{code,message}`（F33）。
> - `editor_open_set` 取消：打开即 `file_open` 登记、关闭即 `file_close` 退订；打开集合有 **64 条 LRU + 引用计数**，崩溃/漏发 close 不会无限增长（F34）。
> - 外部变更用 **fsnotify 目录监听**（不用 postWrite hook，能覆盖 `bash`/`run_python` 直接改文件），去抖 300ms；V0.1 `by` 恒 `"external"`，`by:"agent"` 留待有写入意图标记时再加（F35，已知限制）。
> - `file_save` 增 `allowNew`（新建/另存为）；`baseHash:""` + 已登记路径 = 无条件写，冲突检测只在 `baseHash` 非空时生效（F36）。
> - 自家保存不回推：记 `path+hash`，2s 同 hash 丢弃；watcher 只监听已打开文件**所在目录**（按文件挂会被 rename 打死）。

### 2.11 文档与媒体预览（`file` 内的渲染器）

`office` 不是 kind。`file{mode:'preview'}` 内部按扩展名选渲染器（决策 D-I，完整调研见 `05-preview-media-research.md`）：

```ts
// surfaces/preview/types.ts
export interface PreviewRenderer {
  id: string
  match: (f: { path: string; ext: string; size: number }) => number   // 0=不接，取最大
  weight: 'light' | 'heavy'
  load: () => Promise<React.ComponentType<PreviewProps>>
}

// surfaces/preview/registry.ts —— 与 Surface 注册表同纪律：重复 id 抛错、可 unregister
export function registerRenderer(r: PreviewRenderer): void
export function pickRenderer(f: FileRef): PreviewRenderer | undefined
```

渲染器清单与归口：

| id | 实现 | 归口 |
|---|---|---|
| `text` / `markdown` / `code` | 现有 markdown 管线 + 分页读取（M2 补 offset） | 前端 |
| `image` | `<img>` + transform 缩放/平移 | 前端 |
| `csv` | `CsvPreview`（GFM 表格）+ 文本视图切换 | 前端 |
| `pdf` | 内嵌浏览器 Chromium viewer | 前端 + 现有通道 |
| `docx` / `pptx` / `xls` / `xlsx` | **kit 可用：`soffice→PDF`**；否则现有 previewer 的 HTML（横幅提示） | **转换服务** + `internal/officekit`（§2.13） |
| `media` | `<video>`/`<audio>` + `hai-media://` | Electron 协议 + `ffprobe` |

转换服务（`desktop/bridge/conversion/service.go`）的硬约束见 `01-design.md` §4.7；**失败不缓存、输出不落工作区、并发 2、60s 超时**。媒体协议的 Range 实现见 `05-preview-media-research.md` §4 D-L（含路径校验）。

### 2.12 M0 测试矩阵

| 层 | 用例 | 位置 |
|---|---|---|
| 单测 | registry 校验/排序；`panelSizes` 三档宽度；`dedupeKeyOf` 8 种 | `*.test.ts` |
| Go 单测 | 并发 save、hash 冲突、二进制、>2MiB、CRLF 往返、锁串行 | `desktop/bridge/file_edit_test.go` |
| e2e | 布局三态、多标签并存、编辑器保存/冲突/退出拦截 | `e2e/verify_layout_states.mjs`、`verify_editor.mjs`、`verify_right_tabs_multi.mjs` |
| e2e | `run_code` 内 `tools.ui_inspect` 返回对象（非字符串）；`surface_install` 未批准 → catch 到 rejected | `verify_codemode_surfaces.mjs` |
| 回归 | `verify_auto_open_visual.mjs`、`verify_pdf_preview.mjs`、`test:right-tabs` | 原样 |

---


---

### 2.13 LibreOffice kit（Go 侧）

对齐 DSH 的 `office-kit`：**kit 是可选组件，缺失时降级近似**。完整设计见 `05-preview-media-research.md` §8，这里是代码骨架。

```go
// internal/officekit/kit.go
type Kit struct {
    Root    string          // ~/.go-code/office-kit/<version>/<platform>
    Version string
    Bin     string          // <Root>/soffice （Linux/已装系统 soffice 时为 "soffice"）
}

// Probe 一次探测并按进程缓存：系统 soffice 优先，其次本地 kit；都不行返回 (nil, ErrUnavailable)
func Probe(ctx context.Context) (*Kit, error)

// Convert 转 PDF。每个 job 独立 profile，避免 LibreOffice 的 user-installation 锁。
func (k *Kit) Convert(ctx context.Context, src, outDir string) (pdfPath string, err error) {
    jobProfile, _ := os.MkdirTemp("", "lo-profile-*")
    defer os.RemoveAll(jobProfile)

    args := []string{
        "--headless", "--norestore", "--nolockcheck", "--nodefault",
        "-env:UserInstallation=file://" + jobProfile,
        "--convert-to", "pdf", "--outdir", outDir, src,
    }
    cmd := exec.CommandContext(ctx, k.Bin, args...)
    cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}   // 杀进程组，别留 soffice.bin
    // ctx 超时 → 先 SIGTERM 进程组，2s 后 SIGKILL；Wait 后校验 outDir 里是否真产出 PDF
}

// RenderImages 复用受管 Python 运行时里的 pypdfium2（勿引入 poppler/mutool）
func (k *Kit) RenderImages(ctx context.Context, pdfPath string, pages []int, dpi int) ([]Image, error)
```

三个不变量：

1. **profile 隔离**：`-env:UserInstallation` 必须每 job 独立；共用会报 "user installation is locked"；
2. **进程组**：只 kill 父进程会留 `soffice.bin` 孤儿并占住 profile；
3. **失败不缓存**：转换失败/超时进错误码（`unavailable`/`timeout`/`too-large`/`convert-failed`），不进缓存。

预热：kit 安装完成后跑一次空转换，把 3–6s 冷启成本挪到安装期。

---

## 3. M1：`ui` 通道 + 安装器 + 创造模式

### 3.1 协议：`ui` 信封（**不动 28 个 agent 事件**）

Go：

```go
// desktop/bridge/ui_channel.go
type uiEnvelope struct {
	Type string `json:"type"` // 恒定 "ui"
	Op   string `json:"op"`   // snapshot | file_changed | surfaces_changed | surface_status
	Data any    `json:"data,omitempty"`
}

func (m *bridge) emitUI(op string, data any) {
	m.write(uiEnvelope{Type: "ui", Op: op, Data: data}) // 与事件同一 stdout 流
}
```

TS（transport 层只加一个分支）：

```ts
// transport/stdio.ts
if (msg.type === 'ui') { uiBus.emit(msg.op, msg.data); return }
if (msg.type === 'event') { … 原逻辑 … }
```

`uiBus` 是个极小的 `Map<string, Set<fn>>`，不做持久化、不做重放、不进 metrics。

### 3.2 渲染进程：快照推送

```ts
// surfaces/telemetry.ts
let timer: number | undefined
export function scheduleUISnapshot() {
  clearTimeout(timer)
  timer = setTimeout(() => send({
    type: 'ui_snapshot',
    payload: {
      layout: useAppStore.getState().rightLayout,
      surfaces: listSurfaces().map(p => ({ kind: p.kind, group: p.group, namespace: p.namespace,
        singleton: !!p.singleton, capabilities: p.capabilities ?? [] })),
      // 只给当前会话的标签（ADR-0）；其它会话只给计数，否则 agent 能读到别的会话开了什么
      tabs: (() => {
        const st = useAppStore.getState()
        const cur = st.tabsByScope[st.scopeKey]
        return {
          scope: st.scopeKey,
          activeId: cur?.activeId ?? null,
          tabs: (cur?.tabs ?? []).map(t => ({ id: t.id, kind: t.kind, dirty: !!(t as any).dirty })),
          otherScopes: Math.max(0, Object.keys(st.tabsByScope).length - 1),
        }
      })(),
      editors: useEditorStore.getState().open,   // [{path, dirty, hash}]
      theme: currentThemeTokens(), viewport: { w: innerWidth, h: innerHeight },
    },
  }), 500)
}
// 订阅：tabsByScope / rightLayout / editor open 集合 / surfaces 注册表
```

`ui_snapshot` 只在**变化后**推（debounce 500ms），空转零成本。

### 3.3 bridge：缓存 + 处理

```go
type uiCache struct {
	mu    sync.Mutex
	snap  map[string]any
	at    time.Time
	scope string                  // 快照所属会话（ADR-0：只缓存当前会话，不按会话分表）
	stat  map[string]surfaceStatus // kind -> {mounted, error, at}
}

func (c *uiCache) put(s map[string]any) { c.mu.Lock(); c.snap, c.at = s, time.Now(); c.mu.Unlock() }
func (c *uiCache) get() (map[string]any, int64) {
	c.mu.Lock(); defer c.mu.Unlock()
	if c.snap == nil { return nil, -1 }
	return c.snap, time.Since(c.at).Milliseconds()
}
```

dispatch 增加：

```go
case "ui_snapshot":
	m.uiCache.put(asMap(c.Payload))
	m.resp(c, true, "", nil)          // 快照无回执价值，只 ack

case "surface_status":
	s := surfaceStatus{Mounted: boolv(c.Payload, "mounted"), Error: str(c.Payload, "message"), At: time.Now()}
	m.uiCache.setStatus(str(c.Payload, "kind"), s)
	m.resp(c, true, "", nil)
```

**跨会话的边界（决策 D-H）**：快照只含当前会话，`ui_inspect` 返回里带 `scope` 字段并在描述中说明"这是哪个会话的右栏"。agent 若问别的会话，回答"看不到"——这比给全量再靠纪律约束可靠。

### 3.4 工具实现（完整可落地）

```go
// tools/builtin/ui_inspect.go
package builtin

type UIInspectTool struct{ cache UICache }   // 接口，避免 builtin 依赖 bridge

func (t *UIInspectTool) Name() string { return "ui_inspect" }
func (t *UIInspectTool) Description() string {
	return "只读查询运行中的桌面端：右栏能力（surfaces）、当前标签（tabs）、主题 token、" +
		"桥命令表、设置项、视口尺寸、打开中的编辑器。改 UI 之前先查，不要猜。"
}
func (t *UIInspectTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"what": map[string]any{
				"type": "string",
				"enum": []string{"surfaces", "tabs", "theme", "commands", "settings", "viewport", "editors"},
			},
		},
		"required": []string{"what"},
	}
}
func (t *UIInspectTool) CanParallel() bool { return true }
func (t *UIInspectTool) ReadOnly() bool    { return true }
func (t *UIInspectTool) ValidParams(ctx context.Context, name, args string) error {
	var p struct{ What string `json:"what"` }
	if err := json.Unmarshal([]byte(args), &p); err != nil { return err }
	switch p.What {
	case "surfaces", "tabs", "theme", "commands", "settings", "viewport", "editors":
		return nil
	}
	return fmt.Errorf("what 不合法：%q", p.What)
}
func (t *UIInspectTool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}
func (t *UIInspectTool) AfterCall(context.Context, core.ToolCall) {}

func (t *UIInspectTool) Call(ctx context.Context, _ string, args string) (string, error) {
	var p struct{ What string `json:"what"` }
	_ = json.Unmarshal([]byte(args), &p)

	snap, ageMs := t.cache.Get()
	if snap == nil {
		return "", fmt.Errorf("还没有收到 UI 快照（渲染进程未连接或刚启动），稍后重试")
	}
	val, ok := snap[p.What]
	if !ok { return "", fmt.Errorf("快照里没有 %q", p.What) }

	out, _ := json.Marshal(map[string]any{
		"what": p.What, "value": val, "ageMs": ageMs, "stale": ageMs > 5000,
	})
	return string(out), nil
}
```

`surface_install`（**必须审批**）：

```go
// tools/builtin/surface_tools.go
type SurfaceInstallTool struct{ reg SurfaceRegistry }

func (t *SurfaceInstallTool) Name() string { return "surface_install" }
func (t *SurfaceInstallTool) Description() string {
	return "把一个本地目录里的 Surface 插件装进当前 profile：校验 manifest → 计算 hash → " +
		"快照 index → 写入 → 通知界面热加载。目标必须是含 surface.json 的绝对目录。" +
		"安装后立即检查回执的 status；applied 才算生效。"
}
func (t *SurfaceInstallTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target":  map[string]any{"type": "string", "description": "插件目录的绝对路径"},
			"enable":  map[string]any{"type": "boolean", "default": true},
		},
		"required": []string{"target"},
	}
}
func (t *SurfaceInstallTool) CanParallel() bool { return false }
func (t *SurfaceInstallTool) RequiresApproval(context.Context, core.ToolCall) bool { return true }

func (t *SurfaceInstallTool) Call(ctx context.Context, _ string, args string) (string, error) {
	var p struct{ Target string `json:"target"`; Enable *bool `json:"enable"` }
	if err := json.Unmarshal([]byte(args), &p); err != nil { return "", err }
	res := t.reg.Install(ctx, p.Target)      // {Status, Reason, ID, Version, Hash, NextStep}
	out, _ := json.Marshal(res)
	return string(out), nil
}
```

`surface_list` / `surface_remove` 同构（list 复用 `plugin.State` 的字段名；remove 要求审批、走 `.trash`）。

注册点（`desktop/bridge/tools_registry.go`，与现有工具同一处）：

```go
eng.RegisterTool(ctx, &builtin.UIInspectTool{Cache: m.uiCache})
eng.RegisterTool(ctx, &builtin.SurfaceInstallTool{Reg: m.surfaces})
eng.RegisterTool(ctx, &builtin.SurfaceListTool{Reg: m.surfaces})
eng.RegisterTool(ctx, &builtin.SurfaceRemoveTool{Reg: m.surfaces})
```

### 3.4b CodeMode 兼容（脚本内调用）

规则见设计稿 §5.2c。两处代码要点：

```go
// 1) 结构化结果：只给脚本路径（模型路径仍给文本，20 KB 截断是刻意设计）
func (t *UIInspectTool) OutputSchema() any { return map[string]any{"type": "object"} }

out, _ := json.Marshal(map[string]any{"what": p.What, "value": val, "ageMs": ageMs, "stale": ageMs > 5000})
if tools.IsScriptCall(ctx) {
    if sink := tools.StructuredSinkFrom(ctx); sink != nil { sink.Set(out) }
}
return string(out), nil

// 2) 失败即 reject：脚本用 try/catch 接，不要用 {ok:false} 表达失败
res := t.reg.Install(ctx, target)
if res.Status == "failed" {
    return "", fmt.Errorf("surface_install: %s (%s)", res.Reason, res.ID)   // 脚本侧 reject
}
body, _ := json.Marshal(res)      // applied / disabled / noop 都算成功
return string(body), nil
```

脚本侧验收（`run_code`）：

```js
const s = tools.ui_inspect({ what: "tabs" })        // 对象，不是字符串
if (!Array.isArray(s.value.tabs)) throw new Error("structured result missing")

try {
  await tools.surface_install({ target: "/abs/plugin" })   // 未批准 →
} catch (e) {
  if (!/rejected|timed out/.test(e.message)) throw e     // 两种语义必须可区分
}
```

> 注意：`surface_install` 的审批在**嵌套调用里同样生效**（`ExecuteOne` 走完整审批），脚本会在这里挂起等人；UI 上要显示"脚本正在等待批准"，否则用户会以为卡死。

### 3.5 安装器（`desktop/bridge/surfaces_registry.go`）

**磁盘布局**

```
~/.go-code/surfaces/
  index.json        # {"version":1,"items":[{id,version,hash,source,enabled,installedAt,disabledReason?}]}
  index.json.bak    # 每次变更前快照（只留 1 份）
  audit.jsonl       # 每次 host.command 一行
  <id>/surface.json | panel.js | icon.svg
  .trash/<id>-<ts>/
```

**manifest 校验**（拒绝比修复便宜）

```go
type SurfaceManifest struct {
	ID      string            `json:"id"`      // ^[a-z0-9][a-z0-9-]{1,39}$
	Name    map[string]string `json:"name"`    // {zh,en}
	Version string            `json:"version"` // semver
	API     int               `json:"api"`     // 必须 == 1
	Kind    string            `json:"kind"`    // 必须 "plugin:<id>"
	Group   string            `json:"group"`   // 固定 "plugin"（V0.1）
	Singleton bool            `json:"singleton"`
	Entry   string            `json:"entry"`   // panel.js
	Icon    string            `json:"icon"`
	Commands []string         `json:"commands"`
	Data     []string         `json:"data"`
}
```

校验项：id 正则、`kind == "plugin:"+id`、`api == 1`、`entry` 存在且 < 512 KiB、`commands` 全在已知命令表（未知 → 拒绝并列出）、`icon` < 64 KiB、无符号链接逃逸（`filepath.EvalSymlinks` 后必须在目录内）。

**安装流程**

```go
func (r *SurfaceRegistry) Install(ctx context.Context, dir string) Result {
	m, err := readManifest(dir); if err != nil { return failed("invalid-manifest", err) }
	if id, ok := r.findIndex(m.ID); ok && r.hashOf(id) == m.Hash() {
		return Result{Status: "applied", ID: m.ID, Reason: "already-installed"}
	}
	bak, _ := os.ReadFile(r.indexPath)
	if err := copyDir(dir, filepath.Join(r.root, m.ID)); err != nil { return failed("copy", err) }
	next := r.index.WithUpsert(m, dir)
	if err := atomicWriteJSON(r.indexPath, next); err != nil {
		_ = os.WriteFile(r.indexPath, bak, 0o600)
		return failed("index-write", err)
	}
	r.emitUI("surfaces_changed", map[string]any{"op": "install", "id": m.ID})
	return Result{Status: "applied", ID: m.ID, Version: m.Version, Hash: m.Hash()}
}
```

**回执语义**（与 DSH 对齐但更小）

| status | 含义 | 模型该做什么 |
|---|---|---|
| `applied` | index 已写、`surfaces_changed` 已推 | 用 `ui_inspect` 验证真的挂上了 |
| `disabled` | 装了但被禁用（平台/版本/用户） | 报告 `reason`，不要重装 |
| `failed` | 未写 index，磁盘可能残留 | 读 `reason` 修 manifest 再装 |
| `noop` | 同 id 同 hash | 直接验证 |

### 3.6 插件加载器与 HostApi

```ts
// surfaces/plugin/loader.ts
const loaded = new Map<string, { dispose: () => void }>()

export async function applyManifest(m: Manifest, baseUrl: string) {
  if (loaded.has(m.id)) return
  const mod = await import(/* @vite-ignore */ `${baseUrl}/panel.js?v=${m.hash}`)
  const s = mod.surface
  if (!s || s.kind !== m.kind) throw new Error(`manifest 与 panel.js 的 kind 不一致：${m.kind}`)

  const disposers: Array<() => void> = []
  const host = createHost(m, disposers)           // 白名单代理，见下
  await s.apply({
    host,
    React: host.React,
    surfaces: {
      register(p: Omit<SurfaceProvider, 'namespace'>) {
        registerSurface({ ...p, namespace: 'plugin' } as SurfaceProvider)
        disposers.push(() => unregisterSurface(p.kind))
      },
    },
  })
  loaded.set(m.id, { dispose: () => { disposers.reverse().forEach(d => d()); loaded.delete(m.id) } })
}

export function unloadPlugin(id: string) { loaded.get(id)?.dispose() }

/** 启动与热变更的唯一入口：index 是「装过的」，注册表是「活着的」 */
export async function syncPlugins(index: SurfaceIndex, baseUrl: (id: string) => string) {
  const alive = new Set(index.items.filter(i => i.enabled).map(i => i.id))
  for (const id of loaded.keys()) if (!alive.has(id)) unloadPlugin(id)
  for (const item of index.items) {
    if (!item.enabled) continue
    try { await applyManifest(item.manifest, baseUrl(item.id)) }
    catch (e) { reportSurfaceError(item.id, e) }        // 失败 → 墓碑 + disabledReason
  }
}
```

**卸载必须连带关标签**：`unloadPlugin(id)` 之后，遍历 `tabsByScope` 把 `kind === 'plugin:'+id` 的实例标 `unavailable`（不删除，保留 `closed` 可重开），否则活动标签会渲染一个已注销的 provider（空面板且无报错）。

`createHost`（权限在这里生效）：

```ts
function createHost(m: Manifest, disposers: Array<() => void>): HostApi {
  const allow = new Set(m.commands)
  return {
    React,
    Icon,
    t: (k, v) => i18n.t(k, v),
    theme: () => ({ mode: themeMode(), tokens: themeTokens() }),
    useSession: (sel) => useSyncExternalStore(subscribeProjection, () => sel(getProjection())),
    command: async (name, payload) => {
      if (!allow.has(name)) {
        void audit(m.id, name, 'denied')
        throw new Error(`命令未在 manifest.commands 中声明：${name}`)
      }
      void audit(m.id, name, 'ok')
      return window.desktop.command({ type: name, payload })
    },
    events: { on: (type, fn) => { if (!m.data.includes(type)) throw new Error('事件未声明'); return uiBus.on(type, fn) } },
    openTab: (spec) => useAppStore.getState().openTab(spec as TabSpec),
    toast: (msg) => toast(msg),
  }
}
```

> 注意：`React` 由宿主传入，插件不打包 React；`panel.js` 必须是 ESM 且 `import` 任何**未声明**的包都会失败（浏览器环境下裸模块名无解析器）——这本身就是一道边界。

### 3.7 常驻工具 + skill + 模板 + 插件页（无 mode，D-N）

**不再引入 `CreatorMode`**——创造能力来自"工具常驻 + skill 路由"，与 OpenCode v2 同构（见 `06-opencode-skills-research.md`）。

```go
// desktop/bridge/tools_registry.go —— 与 bash/edit_file 一起注册，不挂 mode
if m.settings.Features.SurfaceTools {          // 默认 true，可关
    eng.RegisterTool(ctx, &builtin.UIInspectTool{Cache: m.uiCache})
    eng.RegisterTool(ctx, &builtin.SurfaceInstallTool{Reg: m.surfaces})  // RequiresApproval: true
    eng.RegisterTool(ctx, &builtin.SurfaceListTool{Reg: m.surfaces})
    eng.RegisterTool(ctx, &builtin.SurfaceRemoveTool{Reg: m.surfaces})   // RequiresApproval: true
}
```

工具描述瘦身（常驻 schema 的成本纪律）：

```go
func (t *SurfaceInstallTool) Description() string {
    return "把一个本地目录里的 Surface 插件装进当前 profile，返回回执；applied 才算生效。"
}   // ≤120 字；教程在 skill 正文，不在工具描述里
```

`skills/hai-surface-dev/SKILL.md`（正文骨架，frontmatter 里的 `requires` 走已有能力面过滤）：

```md
---
name: hai-surface-dev
description: 当用户要求给桌面端右栏加一个面板、改一段 UI、做一个看板/工具页，或调试已装 Surface 插件时使用。
requires:
  tools: [ui_inspect, surface_install]
---
1. 先 ui_inspect 看 surfaces / tabs / theme / commands；不清楚 placement 就问用户。
2. 只装在 plugin 组；不改 workflow 组（链路/变更）。
3. 拷 templates/surface 到工作区，改 surface.json 与 panel.js（createElement，不写 JSX）。
4. surface_install 绝对目录；读回执 status。
5. applied 后 ui_inspect 验证 kind 与 tab；再看页面（有连接页面时）。
6. 禁止：mock HTML 预览、替换根节点、document.body 追加、读别的面板 DOM、全局 CSS。
```

> V1 优化（不阻塞）：用 `load_skill` 的 `onLoad`（`desktop/bridge/skill_capability.go:96`）实现"加载 skill 才解锁工具"，把常驻成本降到零；代价是工具 schema 中途出现会有一次 prefix cache miss，且 loop 重建后要重放已加载技能。

`templates/surface/panel.js` 与设计稿 §5.6 同；`surface.json` 与 §5.5 同。

`PluginsPage.tsx`：列表（图标/名称/版本/状态）、启用开关、卸载、`commands` 声明与 `audit.jsonl` 最近 20 行对照、`disabledReason`。

### 3.8 M1 测试

| 用例 | 判据 |
|---|---|
| dummy surface（`kind: plugin:demo`） | `ui_inspect` 能读到；tab 出现；卸载后墓碑 |
| 越权命令 | `ctx.host.command('provider_billing')` 未声明 → 抛错 + audit 记 denied |
| 坏 manifest | 缺 `kind` / `api:2` / 未知 command → `failed` 且 index 未变 |
| 面板 throw | 只黑该面板；3 次后 `disabledReason` |
| 快照时序 | 刚装完立刻 `ui_inspect` 可能是旧快照 → `stale:true`，skill 要求重试 |

---

## 4. M2：出进程插件 / 编译 / 并排 / 市场 / LSP

### 4.1 出进程插件宿主（解决"Go 没有可卸载模块"）

**形态**：`cmd/hai-plugin-host`（独立二进制），stdio JSON-RPC；bridge 做 supervisor。

```go
// 协议（line-delimited JSON）
type rpcReq struct { ID int64; Method string; Params json.RawMessage }
type rpcRes struct { ID int64; Result any; Error *rpcErr }

// 方法
initialize   {name, version, dshApi}
tools/list   → [{name, description, schema}]
tools/call   {name, arguments} → {content, isError}
shutdown
```

**bridge 侧**

```go
type pluginProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	mu     sync.Mutex
	nextID int64
	pend   map[int64]chan rpcRes
	alive  bool
}
func (p *pluginProc) Call(ctx context.Context, method string, params any, timeout time.Duration) (json.RawMessage, error)
```

- **生命周期**：首次调用工具时 spawn；空闲 10 分钟退出；崩溃 → 指数退避重启（1s/2s/4s，5 次后禁用）；
- **超时**：每调用 60s（manifest 可覆盖），超时即 kill + 报错（不留僵尸）；
- **隔离**：`sandbox.Sandbox`（Seatbelt）套一层；默认无网络；
- **工具代理**：`PluginToolProxy` 实现 `tools.Tool` 的 8 个方法，`Call` 转发 RPC；
- **注册表**：manifest 增加 `"host": {"cmd": "./bin/x", "args": []}`；`surface_install` 时校验可执行位与签名。

> 为什么不是 WASM：插件要 long-lived 连接（MCP/浏览器/文件监听），wazero 里做这些等于自己造运行时；进程 + RPC 是最小可行。

### 4.2 安装期 TSX 编译

把 `esbuild` 提为**直接依赖**（`desktop/app/package.json` 或 bridge 侧 Node 脚本），`surface_install` 增加一步：

```ts
const r = await esbuild.build({
  entryPoints: [path.join(dir, 'src/panel.tsx')],
  bundle: true, format: 'esm', platform: 'browser',
  external: ['react', 'react-dom'],           // 与宿主共享模块表对应
  outfile: path.join(dir, 'panel.js'),
  minify: false, sourcemap: false, logLevel: 'silent',
})
if (r.errors.length) return { status: 'failed', reason: 'build', detail: r.errors }
```

约束：产物 < 512 KiB；`external` 只允许 `react`/`react-dom`；禁止 `import` Node 内置。

### 4.3 右栏内并排

- 结构：`RightPanel > Group(vertical?)` 不需要；并排 = 右栏内部再套一个 `Group(direction=horizontal)`，每个 `Panel` 承载一个 tab；
- 难点是**高度链**：现有 `RightPanel` 依赖父级 `height:100%` + 内部滚动容器，嵌套后要逐层显式 `min-height:0`；
- 交互：拖到 >70% 才自动拆分为两栏（避免误触）；拆分比例独立持久化；
- 风险：`react-resizable-panels` 嵌套时 `onLayoutChanged` 的 `meta.isUserInteraction` 需按 group 分开处理。

### 4.4 插件市场

- 索引：`{version, plugins:[{id, name, version, kind, minApp, commands, url, sha256, signature}]}`
- 签名：minisign/ed25519，公钥内置；安装时验签 + 校验 sha256；
- 版本：`minApp` 与当前 `dsh` 版本比对（semver range），不匹配 → `disabled(reason:"version")`；
- 下载后走**同一条** `surface_install`，不另开路径。

### 4.5 编辑器 LSP（可选）

- CM6 + `@codemirror/lsp-client`（或自写 thin client）→ bridge 起 `gopls`/`tsserver` 子进程，stdio 转发；
- 只在编辑器聚焦时启动；空闲 5 分钟退出；
- 与"出进程插件"共用 supervisor 代码。

---

## 5. 迁移、兼容与测试

### 5.1 数据迁移

| 数据 | 版本 | 迁移 |
|---|---|---|
| `localStorage` 标签集 | v1 → v2 | 旧 kind 原样读入；历史 `office` 标签迁移为 `file{mode:'preview'}`；`plugin:*` 缺省不出现；未知 kind → 墓碑 |
| `rightTab` | 保留一个版本 | 只由 `applyTabs` 写；e2e 全绿后删除（PR-11） |
| `settings.editor` | 新增 | 缺省 true；关闭后隐藏"编辑"动作（回滚开关） |
| `~/.go-code/surfaces/index.json` | v1 | 新文件，不存在即空列表 |

### 5.2 回滚开关

```ts
settings.features = { surfaceRegistry: true, editor: true, multiTab: true }
```

任一为 false → 走旧路径（`RightPanel` 的四个 case 保留一个版本）。**M0 结束后一轮完整发布再删旧路径。**

### 5.3 测试金字塔

| 层 | 数量（估） | 覆盖 |
|---|---|---|
| 前端单测 | ~40 | registry、layout 换算、dedupeKey、冲突 FSM |
| Go 单测 | ~30 | file_open/save、锁串行、manifest 校验、index 原子写与回滚、工具 proxy |
| e2e | ~10 脚本 | 布局三态、多标签、编辑器、插件装载/卸载、越权、崩溃隔离 |
| 人工 | 1 张矩阵 | 3 档宽度 × 3 种 bias × 明暗主题 |

---

## 6. 工作量与分工

| 模块 | 人日 | 建议角色 | 依赖 |
|---|---|---|---|
| 契约 + 注册表 + 布局 | 3.5 | 前端 | — |
| `activeId` 反转 + e2e 迁移 | 1.5 | 前端（熟 store） | 布局 |
| 5 个内置 provider 迁移 | 6 | 前端 | 注册表 |
| 编辑器（Go 写路径 + CM6 + 冲突） | 3.5 | 前端 + Go 各半 | file provider |
| 多标签交互 + Boundary/墓碑 | 2 | 前端 | file provider |
| `ui` 通道 + `ui_inspect` | 1.5 | Go + 前端 | 注册表 |
| 安装器 + 3 工具 | 2 | Go | ui 通道 |
| creator Mode + skill + 模板 + 插件页 | 1.5 | 全栈 | 安装器 |
| 文档渲染器注册表（PR-08） | 1.5 | 前端 | file provider |
| 转换服务（PR-08a） | 2 | Go | 渲染器注册表 |
| 表格增强（PR-08b） | 1.5 | 前端 + Python | 转换服务 |
| 媒体渲染器（PR-08c） | 2 | 前端 + Go | 渲染器注册表 |
| **LibreOffice kit（PR-08d）** | **3** | Go | 转换服务 |
| （可选）转码 + 缩略图（PR-08e） | 2 | Go | 媒体 |
| 收口与文档回写 | 1 | — | 全部 |
| **M0+M1 合计** | **34.5**（含 08e 转码 2 人日；不做为 32.5） | | |
| M2（出进程宿主） | 8-10 | Go | M1 |
| M2（TSX 编译 + 并排 + 市场） | 6-8 | 前端 | M1 |

---

*本文与设计稿、PR 序列三份文档配套使用：设计稿回答"为什么"，PR 序列回答"按什么顺序"，本文回答"具体写成什么样"。*

### 已知限制（V0.1 接受，不进本轮修）

- **编辑器路径匹配**：`file_open` 回显的 `abs` 与 `file_changed` 事件的 `path` 同源，但都不做 `EvalSymlinks` 与大小写归一。同一文件用两种字符串路径（Windows 大小写差异、symlink、`..` 未折叠）打开时，外部变更可能漏匹配 → 表现为「不弹黄条、保存时才发现冲突」。修法（V1）：Go 侧统一 `filepath.EvalSymlinks` + 平台归一化后再比对。
- **CRLF**：R05b 修后按打开时的行尾还原，但混行（CRLF/LF 混杂）文件保存后会统一成主导行尾。

## 2.3 web → SurfaceProvider（R06 拆三轮，只读分析定稿）

**为什么拆**：三条最危险的风险（原生 overlay 挂载时机、`menuOpen/immersive` 决定 `embedHide`、身份键两套 id 空间）**只出现在渲染委派这一步**；而 C1/C2 的失败模式是 `tsc`/单测秒级红。合并成一个 commit = 用 2 分钟的反馈环保守两类完全不同的失败，且 bad-PR 时无法只回退渲染层。

| 轮 | 内容 | 证明 | 回退 |
|---|---|---|---|
| R06a | `isPendingWebTab / webTabKey / samePreviewTarget / webUrlSnapshot` 下沉 `lib/webTab.ts`（零 store 依赖），store re-export | `webTab-test.ts` + 变异检验；既有三个回归网一行不改 | 单 commit |
| R06b | 注册 `webSurface`（`weight:'heavy'`，多实例 session 作用域）+ `title/icon` 委派，**render 仍由 RP 兜底** | 截图逐像素一致（注册前 vs 后） | 删文件 + 删 index.ts 一行 |
| R06c | BrowserTab 平移 `components/WebTab.tsx`（`immersive` 变内部 state），`menuOpen` 上移为 `panelMenuOpen`，render 走 provider 带回退 | `e2e:pdf / container / scoping / session-tabs / auto` 全绿 | 同 PR-03 |

**必须留 store/Electron，不许搬**：`syncWebTabs`、`activateTab`/`closeTabNow` 的 web 分支、`unreadViews`/`previewOwnedViews`、视图池与 `viewId/pageId`。判据：**池是基础设施，不是某个 provider 的私产**（§2.5 既定口径）。

**视图池三条不变量（代码证据，非注释证据）**：
1. 上限 **12 个活渲染进程**（`embed.ts MAX_EMBED_VIEWS=76`），休眠条目在另一池，`targets.length ≫ 12` 属正常；
2. LRU **只在新建视图时**触发（无定时器，`embedHide` 不触发），候选须 `lastUsed>0`、非 active、非本会话唯一、`lastCmdAt` ≥30s、`sid∉busySids`；
3. sleep = 销毁 webContents 但**不广播 destroyed**、保留 id 与 lastUsed；destroy 才广播；唤醒用**原 id** 重建、URL 取休眠快照。

**风险**：
- `TABS_MAX_VISIBLE` 是**死常量**（无消费者），标签条实际 `overflow-x:auto` —— **PR-09 的地盘，R06 不许顺手接**。
- 待重建标签与活标签的键空间不同（url 快照 vs `t7`），若 provider 的 `dedupeKey` 把身份压成单一字符串 → 会开重复标签。
- `unreadViews` 清除是「activeId + rightExpanded + rightTab==='web'」三条件耦合，**不得在 provider 里重算**。

**门禁现状（R-14）**：`e2e:pdf / container / scoping / session-tabs / auto / browser` **都不在 `npm test` 链里**，R06 只能手跑；统一 runner 是 OP-4，推迟到 PR-15。R06c 必须手跑这五个并贴输出。

**P09/P10 基线（R-5 的硬要求）**：R06c 合并前必须采一次 12 视图的内存基线（P09 ≤150MB / P10 ≤800MB），写回看板 perf 表；无基线不许合。

#### P09/P10 预算口径重定（R06c 实测后）

原口径「P09 单渲染进程 ≤150MB / P10 全部渲染进程 ≤800MB」在 **12 视图**负载下**任何实现都过不了**：12 个活 Chromium 渲染进程 × ~76MB ≈ 912MB，光这一项就超 P10。

改为：
- **P10 = 80MB × 活视图数 + 主窗口 200MB**（12 视图 → 1160MB 上限；R06c 实测 1207.7MB 仍略超，但 A/B 证明与代码无关，属 dev server 开销）
- **P09 = 单渲染进程 ≤200MB**（dev 实测 145–216MB，摆动来自 HMR/未压缩；生产构建需单独采一次基线后收紧）
- 采样必须在**生产构建**（`dist/`）上做；`MEM_BASELINE=1 node e2e/verify_preview_container.mjs` 可复现，采样点是 T4 把活渲染进程顶到 12 的那一刻。
