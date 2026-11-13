// Surface 健康度：挂载失败计数 + 自动禁用（03-code-plan §2.9；02-pr-plan PR-10）
//
// 存储：`localStorage['surface.disabled']` 一条 JSON 对象（key = kind）。为什么是对象不是数组：
// 每个写入都是「按 kind 覆盖一条」，对象少一层查找、天然不重复；读的时候逐条校验，
// **一条坏记录只丢它自己**（不能让整个右栏白屏 —— 这是本节最硬的一条）。
//
// 纪律（00 §4.7 / F53 的形状）：这里是失败计数的**唯一**读写点。组件、注册表、探针都走
// 下面这几个函数；再在别处建一份「等价物」当真相源，就会出现「测试里那份是绿的、
// 生产那份没生效」。
//
// **不做内存缓存**（PR-10 实测踩过）：加一层模块级 cache 后，探针里动态 import 出来的
// health 与 RightPanel 走的那份一旦被解析成两个模块实例，两份 cache 立刻分叉 ——
// 一边写、另一边按旧值渲染，症状是「localStorage 里明明禁用了，菜单/墓碑却当没发生」。
// 真相源只有 localStorage 一个，每次读它（一条记录的 getItem + JSON.parse 是微秒级，
// 不值得为它引入第二种真相）。
import { notifySurfacesChanged } from './registry'
import type { SurfaceKind } from './types'

export const SURFACE_HEALTH_KEY = 'surface.disabled'
/** 连续失败到这个数就自动禁用。一次偶发失败不该让面板消失，三次连续则基本可以断定是坏的。 */
export const AUTO_DISABLE_AFTER = 3

export interface SurfaceHealth {
  kind: SurfaceKind
  /** 连续失败数：成功渲染一次即归零（recordMountSuccess） */
  count: number
  /** 最后一次失败的 message（只存窄化后的字符串，不把整个对象塞进 localStorage） */
  lastError: string
  /** 最后一次失败的时刻（epoch ms） */
  at: number
  disabled: boolean
  /** 是否是「连续失败自动禁用」（墓碑据此选文案；手动禁用不算） */
  auto?: boolean
}

type Store = Record<string, SurfaceHealth>

function messageOf(err: unknown): string {
  const msg = err instanceof Error ? err.message : String(err)
  return msg.length > 300 ? `${msg.slice(0, 300)}…` : msg
}

/** 一条记录的窄化：任何字段不合规就整条丢弃（坏数据只影响它自己）。 */
function narrow(key: string, v: unknown): SurfaceHealth | null {
  if (!v || typeof v !== 'object' || Array.isArray(v)) return null
  const o = v as Record<string, unknown>
  if (typeof o.count !== 'number' || !Number.isFinite(o.count) || o.count < 0) return null
  if (typeof o.disabled !== 'boolean') return null
  if (typeof o.lastError !== 'string') return null
  if (typeof o.at !== 'number' || !Number.isFinite(o.at)) return null
  return {
    kind: key as SurfaceKind,
    count: o.count,
    lastError: o.lastError,
    at: o.at,
    disabled: o.disabled,
    ...(o.auto === true ? { auto: true } : {}),
  }
}

function readStore(): Store {
  const next: Store = {}
  try {
    const raw = typeof localStorage === 'undefined' ? null : localStorage.getItem(SURFACE_HEALTH_KEY)
    if (raw) {
      const parsed: unknown = JSON.parse(raw)
      if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
        for (const [k, v] of Object.entries(parsed as Record<string, unknown>)) {
          const entry = narrow(k, v)
          if (entry) next[k] = entry
        }
      }
    }
  } catch {
    // JSON 坏 / localStorage 不可用（隐私模式等）→ 当作「没有禁用记录」，
    // 面板照常渲染。这里绝不抛：一条坏数据不该让右栏白屏。
  }
  return next
}

/** 只留「还被记得」的条目：被禁用，或者还有连续失败没消掉。 */
function prune(store: Store): Store {
  const next: Store = {}
  for (const [k, v] of Object.entries(store)) if (v.disabled || v.count > 0) next[k] = v
  return next
}

function writeStore(next: Store) {
  try {
    if (typeof localStorage !== 'undefined') localStorage.setItem(SURFACE_HEALTH_KEY, JSON.stringify(next))
  } catch {
    // 写不进去（配额/隐私模式）就当作没有禁用记录：面板照常渲染，绝不因此白屏。
  }
  // 通知复用注册表那一条订阅通道（不另造一套）：右栏的重渲染、listSurfaces 的过滤
  // 与标签条的灰态都挂在 subscribeSurfaces 上。
  notifySurfacesChanged()
}

/** 挂载/渲染失败一次。连续 AUTO_DISABLE_AFTER 次 → 自动禁用。 */
export function recordMountFailure(kind: SurfaceKind, err: unknown): SurfaceHealth {
  const store = readStore()
  const prev = store[kind]
  const count = (prev?.count ?? 0) + 1
  const wasDisabled = prev?.disabled === true
  const nowAutoDisable = !wasDisabled && count >= AUTO_DISABLE_AFTER
  const entry: SurfaceHealth = {
    kind,
    count,
    lastError: messageOf(err),
    at: Date.now(),
    disabled: wasDisabled || nowAutoDisable,
    ...(prev?.auto === true || nowAutoDisable ? { auto: true } : {}),
  }
  writeStore({ ...store, [kind]: entry })
  return entry
}

/**
 * 这一轮**真的渲染成功了**（boundary 的 didMount / didUpdate 里调）。
 *
 * 为什么是可靠代理：子组件在 render 期抛错时，boundary 自己的 componentDidMount 不会执行
 * —— 所以能走到这里的，一定是「children 完整渲染了一轮」。连续失败数在这里归零。
 */
export function recordMountSuccess(kind: SurfaceKind): void {
  const store = readStore()
  const prev = store[kind]
  if (!prev) return
  if (prev.count === 0 && !prev.disabled) return
  // 已禁用的不因一次成功就自动复活：禁用是显式状态，只由 enableSurface 解除。
  writeStore(prune({ ...store, [kind]: { ...prev, count: 0 } }))
}

/** 手动禁用（崩溃卡上的「禁用此面板」；也供探针/测试摆状态）。 */
export function disableSurface(kind: SurfaceKind): void {
  const store = readStore()
  const prev = store[kind]
  writeStore({
    ...store,
    [kind]: {
      kind,
      count: prev?.count ?? 0,
      lastError: prev?.lastError ?? '',
      at: prev?.at ?? Date.now(),
      disabled: true,
      auto: false,
    },
  })
}

/** 重新启用：整条记录删掉（count / lastError / at / disabled 一起清）。 */
export function enableSurface(kind: SurfaceKind): void {
  const store = readStore()
  if (!(kind in store)) return
  const next = { ...store }
  delete next[kind]
  writeStore(next)
}

export function isSurfaceDisabled(kind: SurfaceKind | undefined): boolean {
  if (!kind) return false
  return readStore()[kind]?.disabled === true
}

/** 被禁用的 kind 列表（码点排序，保证稳定输出）。 */
export function listDisabled(): SurfaceKind[] {
  return Object.values(readStore())
    .filter((e) => e.disabled)
    .map((e) => e.kind)
    .sort((a, b) => (a < b ? -1 : a > b ? 1 : 0))
}

/** 读一条完整记录（墓碑的「自动禁用」文案要用）；没有则 undefined。 */
export function getSurfaceHealth(kind: SurfaceKind): SurfaceHealth | undefined {
  return readStore()[kind]
}
