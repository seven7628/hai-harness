// uiSnapshot.ts —— 渲染层 UI 快照（PR-12）：把「现在界面长什么样」的**元数据**推给桥，
// 供 `ui_inspect` 工具读。
//
// 三条纪律（02-pr-plan PR-12 / 03-code-plan §3.2）：
//  1. **只放元数据**：id/kind/标题/计数/布局/主题/视口。文件内容、页面 HTML、diff 一律
//     不进 —— 快照是给模型看「结构」的，不是给它看内容的（内容它有 read_file）。
//     标题按 200 字符截断：URL/文件名可能很长，而快照有 256 KiB 的桥侧上限。
//  2. **变化才推**：debounce 500ms，同一窗口内的多次变化合并成一条；内容没变不重复发
//     （比较序列化结果，`at` 除外）。空转零成本 —— 用户不动界面就不产生任何 IPC。
//  3. **不进事件流**：走 `{type:'ui'}` 信封，与 agent 事件（28 类）彻底分开。
//
// 依赖注入（getState/subscribe/send/t/scopeKeyOf）而不是直接 import store：
// store 反过来要 import 本模块来启动它，直接互相 import 会形成运行期环。
// 这里只有 `import type`（编译期擦除），store 侧传进来的是**同一个** store 实例。
import type { AppState, TabInstance } from '../store/useAppStore'
import { getSurface, listSurfaces, subscribeSurfaces } from '../surfaces/registry'
import { listDisabled } from '../surfaces/health'
import { listSurfaceLoadErrors } from '../surfaces/plugin/status'
import { shortName } from './shortPath'

/** debounce 窗口（与 02-pr-plan 的 500ms 口径一致；改它要同步改 e2e 的等待窗口）。 */
export const UI_SNAPSHOT_DEBOUNCE_MS = 500
/** 标题截断上限：快照里最长的一类字符串。 */
export const UI_TITLE_MAX = 200

export interface UISurfaceEntry {
  kind: string
  group: string
  order: number
  disabled: boolean
  /**
   * 插件**加载失败**的原因（PR-14 C）。字段口径：
   *   · 只有「provider 根本没进注册表」的插件才有它（import 失败 / kind 不符 / apply 抛错）；
   *   · `disabled:true` 与之同时出现，因为对快照的消费者（模型）来说「这个 kind 现在不能开」
   *     是同一件事 —— 但**恢复动作不同**：disabled 走 `enableSurface`（health 的禁用集），
   *     带 error 的没有 provider 可启用，只能重装/修插件。
   * 字段名与 `surface_status` 的 `error` 同字（ui_state.go 的 uiSurfaceStatus.Error）。
   */
  error?: string
}

export interface UITabEntry {
  id: string
  kind: string
  title: string
  active: boolean
}

export interface UISnapshot {
  layout: { leftExpanded: boolean; right: { open: boolean; bias?: string } }
  surfaces: UISurfaceEntry[]
  tabs: { scope: string; activeId: string | null; tabs: UITabEntry[] }
  editors: { open: number; dirty: number }
  theme: string
  viewport: { w: number; h: number }
  at: number
}

export interface UISnapshotDeps {
  getState: () => AppState
  /** 订阅 store 变化（zustand 的 subscribe）。 */
  subscribe: (fn: () => void) => () => void
  /** 发一条 `ui` 信封（生产实现走 transport.send；永不在失败时抛）。 */
  send: (payload: { op: string; data: unknown }) => void
  /** i18n 文案（面板标题要的是人话，不是 kind）。 */
  t: (key: string) => string
  /** 当前标签集归属键（store 的 scopeKeyOf：会话 id，无会话时退化为工作区路径）。 */
  scopeKeyOf: (s: AppState) => string
}

function clip(s: string): string {
  return s.length > UI_TITLE_MAX ? s.slice(0, UI_TITLE_MAX) : s
}

function basename(path: string): string {
  return path.split('/').filter(Boolean).pop() ?? ''
}

/** 标签标题：与 RightPanel 的 tabTitle 同源（provider.title），file 用短文件名。 */
function titleOf(tab: TabInstance, t: (k: string) => string): string {
  if (tab.kind === 'file') {
    const base = basename(tab.path ?? '')
    return clip((base && shortName(base)) || t('rp.tab.file'))
  }
  const prov = getSurface(tab.kind)
  if (prov) {
    try {
      return clip(prov.title(tab as never, t) || tab.kind)
    } catch {
      return clip(tab.kind) // provider 自己的 bug 不该让整份快照消失
    }
  }
  return clip(tab.kind)
}

/**
 * 面板清单：**含被禁用的**（disabled:true）。
 *
 * 为什么不用 listSurfaces() 一把梭：它按设计过滤掉被禁用的 provider（「现在能开什么」），
 * 但快照要回答的是「为什么不能开」—— 禁用状态正是 agent 排查插件/面板问题的第一手证据。
 * 所以：listSurfaces() 的顺序（权威排序）+ 禁用集按 kind 附在其后。
 */
function surfaceEntries(): UISurfaceEntry[] {
  const off = new Set(listDisabled())
  const out: UISurfaceEntry[] = listSurfaces().map((p) => ({
    kind: p.kind,
    group: p.group,
    order: p.order ?? 100,
    disabled: false,
  }))
  for (const kind of [...off].sort()) {
    const p = getSurface(kind)
    out.push({ kind, group: p?.group ?? 'plugin', order: p?.order ?? 100, disabled: true })
  }
  // 插件加载失败（PR-14）：这些 kind **没有 provider**（没进注册表），所以上面的三个来源
  // 都看不到它们。不补这一段，「插件坏了」与「没装过这个插件」在 ui_inspect 里长得一样 ——
  // 而模型正是靠这份快照判断「要不要让用户重装」。
  for (const err of listSurfaceLoadErrors()) {
    const hit = out.find((e) => e.kind === err.kind)
    if (hit) {
      hit.error = err.error
      hit.disabled = true
      continue
    }
    out.push({ kind: err.kind, group: 'plugin', order: 100, disabled: true, error: err.error })
  }
  return out
}

/** 生成一份快照（纯读，无副作用；导出供探针/单测直接读结构）。 */
export function buildUISnapshot(deps: UISnapshotDeps): UISnapshot {
  const s = deps.getState()
  const scope = deps.scopeKeyOf(s)
  const set = s.tabsByScope[scope]
  const tabs = set?.tabs ?? []
  const editTabs = tabs.filter((tab) => tab.kind === 'file' && tab.mode === 'edit')
  const layout = s.rightLayout
  const viewport =
    typeof window === 'undefined' ? { w: 0, h: 0 } : { w: window.innerWidth, h: window.innerHeight }
  return {
    layout: {
      leftExpanded: !!s.leftExpanded,
      right: layout.open ? { open: true, bias: layout.bias } : { open: false },
    },
    surfaces: surfaceEntries(),
    tabs: {
      scope,
      activeId: set?.activeId ?? null,
      tabs: tabs.map((tab) => ({
        id: tab.id,
        kind: tab.kind,
        title: titleOf(tab, deps.t),
        // active 逐条落进数组（而不是只给 activeId）：agent 读的是「哪个开着」，
        // 让它自己拿 activeId 去 join 只是把一步机械工作推给模型。
        active: !!set && set.activeId === tab.id,
      })),
    },
    // 当前作用域里开着的编辑标签数 + 其中有未保存改动的。
    editors: {
      open: editTabs.length,
      dirty: editTabs.filter((tab) => s.dirtyTabs[tab.id] === true).length,
    },
    theme: s.settings.theme,
    viewport,
    at: Date.now(),
  }
}

/** 去重键：与 `at` 无关的序列化结果（同内容 = 同一份快照，不必再发）。 */
function snapshotKey(snap: UISnapshot): string {
  return JSON.stringify({ ...snap, at: 0 })
}

/**
 * 启动快照推送：立即发一次，之后按 debounce 合并推送。返回停止函数（幂等）。
 *
 * 订阅面只有三处，都是为了「不白跑」：
 *  · store：只认与快照有关的切片（引用比较）。**不用** selector 订阅是因为 store 没装
 *    subscribeWithSelector；用 O(1) 的引用比较做闸门，流式输出时（每 token 都在 set）
 *    不会产生任何序列化开销；
 *  · 注册表：注册/注销/禁用（health 走同一条 notifySurfacesChanged）；
 *  · window resize：视口也是快照的一部分。
 */
export function startUISnapshot(deps: UISnapshotDeps): () => void {
  let timer: ReturnType<typeof setTimeout> | undefined
  let lastKey = ''
  let stopped = false

  const flush = (): void => {
    timer = undefined
    if (stopped) return
    const snap = buildUISnapshot(deps)
    const key = snapshotKey(snap)
    if (key === lastKey) return
    lastKey = key
    deps.send({ op: 'snapshot', data: snap })
  }
  const schedule = (): void => {
    if (stopped) return
    if (timer) clearTimeout(timer)
    timer = setTimeout(flush, UI_SNAPSHOT_DEBOUNCE_MS)
  }

  // 挂载即发一次：工具在 boot 之后就能问「现在什么情况」，不必等用户动界面。
  // lastKey 归零也覆盖了「桥重启后缓存已空」的情况（同内容也要补发一份）。
  lastKey = ''
  flush()

  const slicesOf = (s: AppState) => [
    s.leftExpanded,
    s.rightLayout,
    s.tabsByScope,
    s.dirtyTabs,
    s.settings.theme,
    s.settings.lang,
  ]
  let prev = slicesOf(deps.getState())
  const offStore = deps.subscribe(() => {
    const cur = slicesOf(deps.getState())
    if (cur.length === prev.length && cur.every((v, i) => v === prev[i])) return
    prev = cur
    schedule()
  })
  const offSurfaces = subscribeSurfaces(schedule)
  const onResize = (): void => schedule()
  if (typeof window !== 'undefined') window.addEventListener('resize', onResize)

  return () => {
    stopped = true
    if (timer) clearTimeout(timer)
    timer = undefined
    offStore()
    offSurfaces()
    if (typeof window !== 'undefined') window.removeEventListener('resize', onResize)
  }
}
