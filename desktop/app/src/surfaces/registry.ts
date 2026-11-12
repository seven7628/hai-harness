// Surface 注册表：模块级单例，唯一写者（01 §3.2 / 03 §2.2）
// 约束：注册即校验，重复注册抛错；插件不可声明 write（D-E）；排序必须字节稳定。
import type { SurfaceKind, SurfaceProvider, SurfaceTab } from './types'

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

/** 稳定排序：group → order（缺省 100）→ kind（按码点，不用 localeCompare，避免 ICU 差异） */
export function listSurfaces(): SurfaceProvider[] {
  return [...byKind.values()].sort(
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

/** 仅供测试：清空注册表与订阅者 */
export function resetSurfacesForTest() {
  byKind.clear()
  listeners.clear()
}
