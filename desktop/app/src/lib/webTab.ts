// web 标签的**身份判据**（纯函数模块，零 store 运行时依赖）—— R06 C1。
//
// 为什么必须先落这一层：R06 要把 web 迁到 SurfaceProvider，而 provider 唯一需要的
// 就是「这个标签是谁 / 走得通哪条数据源 / 恢复出来的标签对上哪个页面」。这些判据今天
// 长在 store 里（`useAppStore.ts` 的标签模型段），直接搬进 provider 会形成
//
//   store → provider → store（runtime cycle，且 store 顶层有 loadPersistedTabs /
//   subscribeEmbedEvents 副作用 → TDZ 与重复订阅双重风险）
//
// 故落 lib：store 与 provider 都只能**单向**依赖它。store 侧保留同名 re-export，
// RightPanel 等调用点的 import 路径不变（本轮零行为变化）。
//
// 依赖纪律：本文件只允许 **type-only** 依赖 store（`import type`，esbuild/tsc 整句擦除，
// 打包后 store 一行都不会进来）；任何 store 的运行时值（useAppStore、常量、其他函数）
// 都不得在此出现 —— 否则上面的环立刻回来。
import type { AppState, TabInstance, WebTabInstance } from '../store/useAppStore'
import { toRelIfInside } from './filePaths'

// isPendingWebTab：待重建的 web 标签（重启恢复出来的那种）。
// 判据 = 「有 url 快照 + 没有任何池身份」：池身份是运行时 id，跨进程不复用，故恢复时一律
// 不带；带上就会指向重启后新建的**另一个**页面（主进程 't'+自增序号从 t1 重来）。
export function isPendingWebTab(t: TabInstance): t is WebTabInstance {
  return t.kind === 'web' && !t.viewId && t.pageId == null && !!t.url
}

// webUrlSnapshot：一个 web 标签此刻能拿到的最可信的 url（+ 源文件路径）快照。
// 三级反查见 toPersistedTab 的注释；返回 undefined = 这个标签**不可恢复**。
export function webUrlSnapshot(t: WebTabInstance, s: AppState): { url: string; srcPath?: string } | undefined {
  if (t.url) return { url: t.url, ...(t.srcPath ? { srcPath: t.srcPath } : {}) }
  if (t.viewId) {
    const tp = s.embedNav?.targets?.find((x) => x.id === t.viewId)
    // about:blank = 自举占位（还没导航到真实页面）→ 不是「用户打开的页面」，不恢复。
    if (tp?.url && tp.url !== 'about:blank') return { url: tp.url, ...(tp.srcPath ? { srcPath: tp.srcPath } : {}) }
    return undefined
  }
  if (t.pageId != null) {
    const pg = s.browserPages.find((p) => p.id === t.pageId)
    if (pg?.url) return { url: pg.url }
  }
  return undefined
}

// webTabKey：web 标签在**当前数据源**下的身份（另一数据源的身份返回 undefined → 不参与本次同步）。
export function webTabKey(t: TabInstance, source: 'native' | 'mcp'): string | undefined {
  if (t.kind !== 'web') return undefined
  if (source === 'native') return t.viewId
  return t.pageId != null ? `p${t.pageId}` : undefined
}

// samePreviewTarget：一个「待重建的恢复标签」（阶段 5）是不是就对应这个页面 —— 用户点它 →
// 重建出视图 → syncWebTabs 用它把视图接回**原来那个标签实例**（否则恢复的标签永远停在
// 待重建态，点一次还多一个重复标签）。
// 口径与主进程 embed:open-target 的去重一致：两侧都有 srcPath 时比 srcPath（office 转换
// 产物每次换临时 html_path，只有源文件认得出「是同一个」），否则比 url。
export function samePreviewTarget(t: WebTabInstance, x: { url: string; srcPath?: string }, ws: string | undefined): boolean {
  const a = t.srcPath
  const b = x.srcPath
  if (a && b) return toRelIfInside(a, ws) === toRelIfInside(b, ws)
  return !!t.url && t.url === x.url
}
