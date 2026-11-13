// Surface 注册表：模块级单例，唯一写者（01 §3.2 / 03 §2.2）
// 约束：注册即校验，重复注册抛错；插件不可声明 write（D-E）；排序必须字节稳定。
import type { SurfaceKind, SurfaceProvider, SurfaceTab } from './types'
// 与 health.ts 是**函数级**循环依赖（registry 只在 listSurfaces 里问「这个 kind 被禁用了吗」，
// health 只在改动后调 notifySurfacesChanged 广播）。两个方向都只在调用期取值，模块求值期
// 不互相读任何绑定 —— 两种 import 顺序都不会踩 TDZ，esbuild 打包同样成立。
import { listDisabled } from './health'

const BUILTIN_KINDS = new Set<SurfaceKind>(['activity', 'git', 'file', 'web'])
const GROUP_ORDER = { workflow: 0, content: 1, plugin: 2 } as const

const byKind = new Map<SurfaceKind, SurfaceProvider>()
const listeners = new Set<() => void>()

function assertProvider<T extends SurfaceTab>(p: SurfaceProvider<T>) {
  if (!p.kind) throw new Error('surface provider 缺少 kind')
  if (p.namespace === 'builtin') {
    if (p.kind.startsWith('plugin:')) throw new Error(`builtin provider 不允许 plugin: 前缀：${p.kind}`)
    if (!BUILTIN_KINDS.has(p.kind)) throw new Error(`未知的内置 kind：${p.kind}`)
  } else {
    if (!p.kind.startsWith('plugin:')) throw new Error(`plugin provider 必须使用 plugin: 前缀：${p.kind}`)
    if (p.capabilities?.includes('write')) throw new Error(`插件不允许 write 能力：${p.kind}`)
  }
  if (typeof p.render !== 'function') throw new Error(`surface provider 缺少 render：${p.kind}`)
}

/** 注册。同一 kind 重复注册直接抛错（不静默覆盖）。 */
export function registerSurface<T extends SurfaceTab>(p: SurfaceProvider<T>) {
  assertProvider(p)
  if (byKind.has(p.kind)) throw new Error(`surface kind 重复注册：${p.kind}`)
  // 泛型擦除：kind ↔ T 的对应由 provider 作者保证，注册表只做容器。
  byKind.set(p.kind, p as unknown as SurfaceProvider)
  emit()
}

/** 注销。返回是否真的删除了（重复注销不抛错）。 */
export function unregisterSurface(kind: SurfaceKind): boolean {
  const deleted = byKind.delete(kind)
  if (deleted) emit()
  return deleted
}

export function getSurface(kind: SurfaceKind | undefined): SurfaceProvider | undefined {
  return kind ? byKind.get(kind) : undefined
}

/**
 * 稳定排序：group → order（缺省 100）→ kind（按码点，不用 localeCompare，避免 ICU 差异）。
 *
 * PR-10 起**过滤掉被禁用的 provider**（连续 3 次崩溃自动禁用 / 手动禁用）：这个函数是
 * 「现在能开/能列出什么」的唯一答案（＋ 菜单与空态能力面板都读它）。
 * 注意 `getSurface()` **不过滤** —— 墓碑要靠它判断「provider 在，只是被禁用」（面板名从
 * provider.title 取），两者口径必须分开。
 */
export function listSurfaces(): SurfaceProvider[] {
  // 一次读出禁用集（不是每个 provider 各读一次）：listSurfaces 在渲染期被调，
  // 每次 getItem + JSON.parse 乘以 provider 数没必要。
  const off = new Set<SurfaceKind>(listDisabled())
  return [...byKind.values()].filter((p) => !off.has(p.kind)).sort(
    (a, b) =>
      GROUP_ORDER[a.group] - GROUP_ORDER[b.group] ||
      (a.order ?? 100) - (b.order ?? 100) ||
      (a.kind < b.kind ? -1 : a.kind > b.kind ? 1 : 0),
  )
}

export function subscribeSurfaces(fn: () => void): () => void {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

function emit() {
  for (const fn of listeners) fn()
}

/**
 * 广播「表面层变了」——注册/注销之外，**可用性（禁用集）的变化也走这一条通道**。
 * health.ts 每次改动后调它；UI 只需订阅 subscribeSurfaces 一处就能同时收到两类变化，
 * 不另造第二套订阅（两份订阅器必然分叉出「一边刷新一边不刷新」）。
 */
export function notifySurfacesChanged() {
  emit()
}

/** 仅供测试：清空注册表与订阅者 */
export function resetSurfacesForTest() {
  byKind.clear()
  listeners.clear()
}
