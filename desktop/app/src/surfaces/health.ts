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

/**
 * 「每个 kind 现在跑的是哪个版本」（`{ [kind]: hash }`）。
 *
 * 为什么健康记录里的 `hash` 还不够、非要再存一份：一条记录是**失败那一刻**诞生的，
 * 而那一刻没人告诉 health「这是哪一版」。只在 noteSurfaceVersion 里补值有个死角 ——
 * 插件 v3 加载成功（此时还没有记录，补不上），随后渲染失败 3 次，记录诞生（没有 hash），
 * 用户跨重启装上 v4：noteSurfaceVersion 看到「记录没有 hash」按规则只能补值不清计数，
 * 于是 R-20 原样复发。所以**应用版本要独立于记录存在**：成功加载就记下来，失败时盖戳，
 * 升级时就能比出「这条账是 v3 的」。
 *
 * 它有意的取舍：只在 localStorage 里存活（与 `surface.disabled` 同一份真相源纪律，
 * 不引入内存缓存 —— 见文件头的 PR-10 踩坑记录）。
 */
export const SURFACE_VERSIONS_KEY = 'surface.versions'
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
  /**
   * 这条计数属于**哪个版本**（插件 = index 里的 sha256；内置 provider 没有，留空）。
   *
   * 为什么必须有（R-20）：键只有 kind 的时候，插件修好发新版本、装上、第一帧又错一次 →
   * `count` 从 3 直接到 4，立刻再次自动禁用。用户看到的是「装了新版还是坏的」，
   * 而真相是**旧版本**的账还在。判据见 noteSurfaceVersion：**换版本 = 干净重来**。
   */
  hash?: string
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
    // 老记录（R17a 之前写的）没有 hash：照常读，noteSurfaceVersion 会把它补上。
    ...(typeof o.hash === 'string' && o.hash ? { hash: o.hash } : {}),
  }
}

/** 版本表的读：坏行只丢它自己，任何异常都退化成「不知道版本」（= 不清任何计数）。 */
function readVersions(): Record<string, string> {
  const next: Record<string, string> = {}
  try {
    const raw = typeof localStorage === 'undefined' ? null : localStorage.getItem(SURFACE_VERSIONS_KEY)
    if (!raw) return next
    const parsed: unknown = JSON.parse(raw)
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return next
    for (const [k, v] of Object.entries(parsed as Record<string, unknown>)) {
      if (typeof v === 'string' && v) next[k] = v
    }
  } catch {
    // 隐私模式 / 坏 JSON → 当没有这张表（下一次 noteSurfaceVersion 会重建）。
  }
  return next
}

/**
 * 版本表的写：**不** notifySurfacesChanged —— 它不改变任何渲染结果（只给下一次失败盖戳），
 * 通知它等于每轮 sync 都让右栏与快照重算一次。
 */
function writeVersions(next: Record<string, string>): void {
  try {
    if (typeof localStorage !== 'undefined') localStorage.setItem(SURFACE_VERSIONS_KEY, JSON.stringify(next))
  } catch {
    // 写不进去 → 只是这一次的版本没记住（最坏结果是多累计一次），绝不因此白屏。
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
  // 记录这一刻盖的版本章：已经有章的跟着走；没有章的（这条是刚诞生的）从版本表取。
  const version = prev?.hash ?? readVersions()[kind] ?? ''
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
    // 版本标记跟着记录走：丢了它，下一次 noteSurfaceVersion 会以为「没有版本」而只补值，
    // 于是升级后的第一次失败仍然从旧 count 往上加 —— R-20 原封不动地回来。
    ...(version ? { hash: version } : {}),
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
      ...(prev?.hash ? { hash: prev.hash } : {}),
    },
  })
}

/**
 * noteSurfaceVersion 告诉健康度「这个 kind 现在跑的是哪个版本」（loader 应用 provider 时调）。
 *
 * 语义（R-20 的裁决，改之前先读）：
 *   · 有记录且 `hash` **不同** → **整条删掉**：换版本 = 干净重来。count / disabled /
 *     lastError / at 一起清 —— 半清（只清 count 不清 disabled）等于新版照样被禁，
 *     那正是这条缺陷的症状本身。
 *   · 相同 → 只补值，**不动 count**：同一个版本反复挂载失败仍然累计（否则 3 次自动禁用
 *     永远触发不了，那才是把「可见的坏」换成「不可见的坏」）。
 *   · 没有记录 → 什么都不做（用不着为「从没失败过的插件」占一条记录，prune 也会扔掉它）。
 *   · `hash` 为空（老调用点 / 内置 provider）→ 不动记录：宁可继续累计，也不要因为
 *     拿不到版本就把用户的失败计数无端清零。
 */
export function noteSurfaceVersion(kind: SurfaceKind, hash: string | undefined): void {
  const h = typeof hash === 'string' ? hash.trim() : ''
  if (!h) return
  // 先记「现在跑的是这一版」：即使这一刻还没有健康记录（正常插件就是这样），
  // 这张表也要先有值 —— 否则记录诞生时盖不上戳，跨重启的升级就比不出新旧（R-20）。
  const versions = readVersions()
  if (versions[kind] !== h) writeVersions({ ...versions, [kind]: h })
  const store = readStore()
  const prev = store[kind]
  if (!prev) return
  if (prev.hash && prev.hash !== h) {
    // 手动禁用（用户显式点的「禁用此面板」）不因版本变更复活：保留那条记录，只把这一版的
    // 账归零。判据是 `auto !== true`（自动禁用一定带 auto:true；老记录没有该字段 = 手动）。
    if (prev.disabled === true && prev.auto !== true) {
      writeStore({
        ...store,
        [kind]: { ...prev, count: 0, lastError: '', at: Date.now(), hash: h, disabled: true, auto: false },
      })
      return
    }
    const next = { ...store }
    delete next[kind]
    writeStore(next)
    return
  }
  if (prev.hash === h) return
  writeStore({ ...store, [kind]: { ...prev, hash: h } })
}

/**
 * forgetSurfaceVersion 忘掉一个 kind 的版本（插件数据清理时调：kind 都没了，版本也没意义）。
 * 与 clearSurfaceHealth 分开是因为语义不同 —— 「重新启用」不该抹掉版本表（下一次 sync 会再写回来）。
 */
export function forgetSurfaceVersion(kind: SurfaceKind): void {
  const versions = readVersions()
  if (!(kind in versions)) return
  const next = { ...versions }
  delete next[kind]
  writeVersions(next)
}

/**
 * 整条记录删掉（count / lastError / at / disabled / hash 一起清）。
 *
 * 两个调用点共用这一条：`enableSurface`（墓碑的「重新启用」）与插件数据清理
 * （`plugin/storage.ts` —— kind 已经不存在了，禁用记录留着的唯一效果是「同名插件重装
 * 后一上来就是禁用态」）。**不**在这里做「只清一半」的事：半清状态没有消费者。
 */
export function clearSurfaceHealth(kind: SurfaceKind): void {
  const store = readStore()
  if (!(kind in store)) return
  const next = { ...store }
  delete next[kind]
  writeStore(next)
}

/** 重新启用：整条记录删掉（count / lastError / at / disabled 一起清）。 */
export function enableSurface(kind: SurfaceKind): void {
  clearSurfaceHealth(kind)
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
