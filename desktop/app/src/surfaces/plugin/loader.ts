// surfaces/plugin/loader.ts —— 渲染层的插件加载器（PR-14 C；03-code-plan §3.6）。
//
// 一句话：**index 是「装过的」，注册表是「活着的」，这个文件负责让后者追上前者。**
//
// 三条硬约束（每条都有对应的失败形态，注释里写的是踩过/预见过的样子）：
//
//  1. **单插件失败 = 只黑自己**：坏模块（语法错 / kind 不符 / apply 抛错）必须只让
//     这一个 kind 进不了注册表，内置面板与其它插件照旧。做法是「import + 校验 + apply
//     整段 try/catch，且 apply 之前登记的 provider 在失败时**逐个撤销**」——只 catch
//     不撤销的话，面板一半挂上了、一半没挂上，症状是「有时能开，有时空白」（最难查的一类）。
//  2. **失败要看得见**：`status.ts` 登记 + console.warn + `surface_status` 上报。
//     静默跳过的插件在 ui_inspect 里与「没装」长得一模一样。
//  3. **卸载必须连带注销**：`dispose()` 调 `unregisterSurface` 并清掉 host 订阅；
//     漏了这一步，标签还开着但 provider 没了 —— PR-10 的墓碑机制能兜住（unavailable
//     reason = missing），但「关掉再打开」会变成空面板。
//
// 缓存：URL 里带 `?v=<hash>`（bridge 安装时算的 sha256）。hash 变 = 新 URL = 新模块实例
// （浏览器不会复用旧的 module record）；hash 不变 = 用同一份，不重复求值。
//
// 依赖方向：本文件**不 import store**（store 反过来要 import 它来启动，直接互相 import
// 会形成运行期环 —— 与 lib/uiSnapshot 同一取舍）。store 的能力（openTab / toast /
// 当前 workspace / 上报通道）由 startPluginLoader 的 deps 注入。
import * as React from 'react'
import type { ReactNode } from 'react'
import { subscribeUI } from '../../lib/uiBus'
import { t as i18nT } from '../../i18n'
import { registerSurface, unregisterSurface } from '../registry'
import { noteSurfaceVersion } from '../health'
import { setSurfaceEntryIndex } from './entries'
import { clearSurfaceLoadError, setSurfaceLoadError } from './status'
import type { SurfaceIndex, SurfaceIndexItem, SurfaceCommandReply } from '../../transport/types'
import type { DistributiveOmit, HostApi, SurfaceKind, SurfaceProvider, SurfaceTab } from '../types'

/** 生产 base URL：`hai-surface://<id>`（协议见 electron/surface-protocol.ts）。 */
export function surfaceBaseUrl(id: string): string {
  return `hai-surface://${id}`
}

/**
 * base URL 的**测试替身**（生产恒 undefined）。
 *
 * 为什么必须有它：e2e 跑在 Chromium 里、没有 Electron 主进程，`hai-surface://` 这个
 * 协议在那台「浏览器」上是未注册的（import 直接 Failed to fetch）。而探针要验的恰恰是
 * **store → loader 这条接线**（自己调 syncPlugins 会把最易漏的一段测成不存在）。
 * 于是保留接线、只换字节来源：同源静态目录 `/e2e/_pluginprobe_run/plugins/<id>`。
 *
 * 命名带 ForTest 是刻意的（本仓先例：resetUIBusForTest / unloadAllPluginsForTest）：
 * 生产代码里出现这个调用就是错的。
 */
let baseUrlOverride: SurfaceBaseUrl | undefined
export function setPluginBaseUrlForTest(base: SurfaceBaseUrl | undefined): void {
  baseUrlOverride = base
}

/** e2e / 探针可以把 base 换成同源的静态目录（`/e2e/_pluginprobe_run/plugins/<id>`）。 */
export type SurfaceBaseUrl = string | ((id: string) => string)

export interface PluginHostDeps {
  /** `ui` 通道上报（渲染层 → bridge 的 `surface_status`）。缺省 = 只登记本地错误。 */
  send?: (op: string, data: unknown) => void
  /** 插件要开标签（`host.openTab`）。缺省 = 静默不动作（宿主不提供该能力时也不该抛）。 */
  openTab?: (spec: DistributiveOmit<SurfaceTab, 'id'>) => void
  toast?: (msg: string) => void
  /** 当前 workspace（`host.command` 自动带上：看板类面板的命令大多按工作区取数）。 */
  getWorkspace?: () => string | undefined
  /** i18n（缺省用 src/i18n 的 t）。 */
  t?: (key: string, vars?: Record<string, unknown>) => string
  /** 主题（缺省读 <html data-theme> + index.css 的 CSS 变量）。 */
  theme?: () => { mode: 'light' | 'dark'; tokens: Record<string, string> }
}

export interface PluginFailure {
  id: string
  kind: SurfaceKind
  error: string
}

export interface SyncReport {
  /** 这一轮真的 import + apply 成功的 id */
  loaded: string[]
  /** 这一轮因为「禁用 / 消失 / hash 变了」被卸载的 id */
  unloaded: string[]
  /** 已是最新（hash 未变）而跳过的 id */
  skipped: string[]
  /** 失败（已登记 + console.warn）的插件 */
  failed: PluginFailure[]
}

interface LoadedPlugin {
  id: string
  kind: SurfaceKind
  hash: string
  entry: string
  dispose: () => void
}

/** 已加载的插件（id → 句柄）。模块级单例：注册表也是单例，两者生命周期必须一致。 */
const loaded = new Map<string, LoadedPlugin>()

/** 最近一次 sync 用的 base（渲染层拼入口图标 URL 时沿用同一条来源，不另猜协议）。 */
let currentBase: SurfaceBaseUrl | undefined

/**
 * pluginAssetUrl 插件目录内某个文件的 URL（入口图标 / 声明图标）。
 *
 * 与 applyManifest 拼入口时**同一份 base**：生产是 `hai-surface://<id>`，探针里由
 * setPluginBaseUrlForTest 换成同源静态目录。两处各拼一次迟早漂移（一处带 hash 一处不带、
 * 一处用了 `<id>` 一处忘了），而图标的失败形态是「空白 12px」—— 最难被注意到的那类。
 */
export function pluginAssetUrl(id: string, rel: string): string {
  if (!id || !rel) return ''
  const base = baseOf(currentBase ?? surfaceBaseUrl, id)
  return base ? `${base}/${rel.replace(/^\/+/, '')}` : ''
}

/** 免疫：正在进行的 sync（并发调用来一个就排队，绝不两个同时 import 同一个 id）。 */
let syncChain: Promise<SyncReport> = Promise.resolve({ loaded: [], unloaded: [], skipped: [], failed: [] })

const pluginKindOf = (id: string): SurfaceKind => `plugin:${id}`

function baseOf(baseUrl: SurfaceBaseUrl, id: string): string {
  const base = typeof baseUrl === 'function' ? baseUrl(id) : baseUrl
  return String(base ?? '').replace(/\/+$/, '')
}

const THEME_TOKENS: Record<string, string> = {
  bg: '--paper',
  fg: '--ink',
  border: '--ink-line',
  accent: '--cinnabar',
  ok: '--bamboo',
  warn: '--ochre',
  err: '--crimson',
  info: '--indigo',
}

function defaultTheme(): { mode: 'light' | 'dark'; tokens: Record<string, string> } {
  if (typeof document === 'undefined') return { mode: 'dark', tokens: {} }
  const root = document.documentElement
  const mode: 'light' | 'dark' = root.getAttribute('data-theme') === 'paper' ? 'light' : 'dark'
  const cs = getComputedStyle(root)
  const tokens: Record<string, string> = {}
  for (const [name, cssVar] of Object.entries(THEME_TOKENS)) {
    const v = cs.getPropertyValue(cssVar).trim()
    if (v) tokens[name] = v // 空值不给：插件读到 '' 与没有这个 token 是两件事
  }
  return { mode, tokens }
}

function iconNode(src: string): ReactNode {
  // 12×12 与内置 provider 的图标同尺寸（标签条只有这么大）。inline style 而不是新 CSS 类：
  // 插件资产不该要求主包同时发一版样式（那会让「装插件」变成「装插件 + 升级客户端」）。
  return React.createElement('img', { src, width: 12, height: 12, alt: '', draggable: false, style: { width: 12, height: 12 } })
}

/**
 * createHost 插件唯一的宿主能力面（03 §3.6）。
 *
 * `disposers` 由调用方持有：插件注册的事件订阅在这里登记撤销函数，卸载时逐个执行 ——
 * 否则 uiBus 的订阅表会在每次「卸载→重装」后多留一份僵尸监听（回调里还引用着上一条
 * 闭包里的 host）。
 */
function createHost(item: SurfaceIndexItem, deps: PluginHostDeps, disposers: Array<() => void>): HostApi {
  const declared = new Set(item.commands)
  const ops = new Set(item.data)
  const id = item.id
  const t = deps.t ?? ((k: string, _v?: Record<string, unknown>) => i18nT(k))

  return {
    React,
    t,
    theme: deps.theme ?? defaultTheme,
    // command 的**两级闸门**：这里先按 manifest.commands 拒（省一次 IPC），
    // bridge 再按「manifest ∩ 宿主只读表」判一次（那才是边界）。返回值恒为对象，
    // **永不抛** —— 插件在 render 里 `await host.command(...)` 时，一次未捕获拒绝
    // 就是整棵子树卸载（PR-10 的崩溃卡），而那并不是「越权」该有的表现。
    command: async (name: string, payload?: unknown): Promise<SurfaceCommandReply> => {
      if (!declared.has(name)) return { ok: false, reason: 'denied' }
      const api = typeof window === 'undefined' ? undefined : window.desktop
      if (!api?.surfaceCommand) return { ok: false, reason: 'denied' }
      try {
        const args = payload && typeof payload === 'object' && !Array.isArray(payload)
          ? (payload as Record<string, unknown>)
          : {}
        const reply = await api.surfaceCommand({
          id,
          name,
          args,
          ...(deps.getWorkspace?.() ? { workspace: deps.getWorkspace?.() } : {}),
        })
        return reply?.ok === true
          ? { ok: true, data: reply.data }
          : { ok: false, reason: reply?.reason || 'denied' }
      } catch {
        // 传输层异常也收敛成固定文案：插件拿不到内部错误（与 bridge 的 error 同一条口径）。
        return { ok: false, reason: 'error' }
      }
    },
    events: {
      on: (type: string, fn: (e: unknown) => void): (() => void) => {
        if (!ops.has(type)) throw new Error(`事件未在 manifest.data 中声明：${type}`)
        const off = subscribeUI(type, fn)
        disposers.push(off)
        return off
      },
    },
    openTab: (spec) => {
      if (!deps.openTab) return
      deps.openTab(spec)
    },
    toast: (msg: string) => {
      deps.toast?.(String(msg))
    },
  }
}

/**
 * applyManifest 加载并应用一个插件（失败时**把已登记的 provider 全部撤销**再抛）。
 *
 * 校验顺序（先便宜后昂贵）：kind（manifest）→ import → 模块形状 → kind（模块）→ apply。
 * `kind` 判两次是刻意的：manifest 说 `plugin:demo`、panel.js 说 `plugin:other` 时，
 * 无论信哪一侧都会出现「注册表里有个 kind 与文件名对不上的 provider」——注册表按 kind
 * 索引，那种错位的症状是「另一个插件的标签渲染成了这个插件的面板」。
 */
async function applyManifest(item: SurfaceIndexItem, baseUrl: SurfaceBaseUrl, deps: PluginHostDeps): Promise<LoadedPlugin> {
  const id = item.id
  const expected = pluginKindOf(id)
  if (item.kind !== expected) {
    throw new Error(`surface.json 的 kind 必须是 ${expected}（实得 ${item.kind || '空'}）`)
  }
  const base = baseOf(baseUrl, id)
  if (!base) throw new Error('baseUrl 为空')
  const entry = item.entry || 'panel.js'
  // `?v=<hash>` 是缓存键：hash 变了 URL 就变了，浏览器不会把旧模块记录复用到新版本上。
  const url = `${base}/${entry}?v=${encodeURIComponent(item.hash || '0')}`

  let mod: { surface?: unknown; default?: unknown }
  try {
    mod = (await import(/* @vite-ignore */ url)) as { surface?: unknown; default?: unknown }
  } catch (e) {
    // import 失败最常见的是「panel.js 语法错 / 404」——原样上抛，由 sync 登记。
    // cause 带上原始错误：reason 只给一条人读的话，栈要留给 console.warn 与排查。
    throw new Error(`import ${url} 失败：${e instanceof Error ? e.message : String(e)}`, { cause: e })
  }
  const surface = (mod?.surface ?? mod?.default) as { kind?: unknown; apply?: unknown } | undefined
  if (!surface || typeof surface !== 'object') throw new Error('panel.js 没有导出 surface')
  if (surface.kind !== expected) {
    throw new Error(`panel.js 的 surface.kind 必须是 ${expected}（实得 ${String(surface.kind ?? '空')}）`)
  }
  if (typeof surface.apply !== 'function') throw new Error('surface.apply 不是函数')

  const disposers: Array<() => void> = []
  const host = createHost(item, deps, disposers)
  const iconUrl = item.icon ? `${base}/${item.icon}` : ''
  const surfacesApi = {
    register(p: Omit<SurfaceProvider, 'namespace'>) {
      if (!p || p.kind !== expected) {
        throw new Error(`插件只能注册自己的 kind：期望 ${expected}，实得 ${String(p?.kind)}`)
      }
      const provider = {
        ...p,
        namespace: 'plugin' as const,
        // 插件没给 title 时用 id 兜底：注册表不校验 title，而缺失的 title 会在
        // RightPanel 的 tabTitle 里抛 TypeError —— 那时表现是「整条标签条崩了」。
        title: typeof p.title === 'function' ? p.title : () => id,
        // icon.svg 存在就用协议 URL（插件自己声明了 icon 则尊重它的）。
        ...(item.icon && !p.icon ? { icon: () => iconNode(iconUrl) } : {}),
      } as SurfaceProvider
      registerSurface(provider)
      disposers.push(() => {
        unregisterSurface(provider.kind)
      })
    },
  }

  try {
    await (surface.apply as (ctx: unknown) => unknown)({ host, React: host.React, surfaces: surfacesApi })
  } catch (e) {
    // 半成功必须回滚：apply 里可能已经 register 过一个 provider 之后再抛。
    for (const d of [...disposers].reverse()) {
      try {
        d()
      } catch {
        /* 撤销自己的失败不该掩盖原始错误 */
      }
    }
    throw e
  }

  const dispose = (): void => {
    // 逆序：先撤销后注册的，事件订阅最后（与 React 的 effect cleanup 同一顺序直觉）。
    for (const d of [...disposers].reverse()) {
      try {
        d()
      } catch {
        /* 单个 disposer 失败不影响其余（否则一个坏插件能锁住整张注册表） */
      }
    }
    loaded.delete(id)
  }
  return { id, kind: expected, hash: item.hash, entry, dispose }
}

/** reportFailure 登记 + 上报（`ui_inspect` 的 surfaces[] 与 console 都看得见）。 */
function reportFailure(item: SurfaceIndexItem, deps: PluginHostDeps, err: unknown): PluginFailure {
  const kind = pluginKindOf(item.id)
  setSurfaceLoadError(kind, item.id, err)
  deps.send?.('surface_status', { kind, mounted: false, disabled: true, error: String((err as Error)?.message ?? err).slice(0, 300) })
  // console.warn 而不是 error：一个装坏的第三方插件不该把控制台刷红（它已经被 _inspect 看见了）。
  console.warn(`[surface] 插件 ${item.id} 加载失败（已跳过，其它插件不受影响）：`, err)
  return { id: item.id, kind, error: err instanceof Error ? err.message : String(err) }
}

/** unloadPlugin 卸载一个已加载的插件（幂等）。 */
export function unloadPlugin(id: string): boolean {
  const p = loaded.get(id)
  if (!p) return false
  p.dispose()
  clearSurfaceLoadError(p.kind)
  return true
}

/** loadedPluginIds 已加载的插件 id（码点排序；测试与诊断用）。 */
export function loadedPluginIds(): string[] {
  return [...loaded.keys()].sort()
}

/** isPluginLoaded 该 id 是否已经活在注册表里（标签条/诊断用）。 */
export function isPluginLoaded(id: string): boolean {
  return loaded.has(id)
}

async function syncOnce(index: SurfaceIndex, baseUrl: SurfaceBaseUrl, deps: PluginHostDeps): Promise<SyncReport> {
  const report: SyncReport = { loaded: [], unloaded: [], skipped: [], failed: [] }
  currentBase = baseUrl
  // 入口快照必须在**注册/卸载之前**更新：宿主的所有入口都读这一份（R16 / D-2），
  // 而禁用 → 入口消失、启用 → 入口回来这两件事都发生在同一轮 sync 里。放在这里
  // 还保证「插件加载失败」时入口照样在（D-3 最后一行：点它能开出墓碑标签）。
  setSurfaceEntryIndex(index)
  const alive = new Map<string, SurfaceIndexItem>()
  for (const item of index?.items ?? []) {
    if (!item || item.id === undefined) continue
    if (!item.enabled) continue
    if (item.error) {
      // 坏 manifest：跳过它自己（带 error 上报），**不影响其它插件**。
      report.failed.push(reportFailure(item, deps, new Error(item.error)))
      continue
    }
    alive.set(item.id, item)
  }

  // ① 先卸载：禁用 / 从 index 消失 / 声明坏了。顺序在加载之前是刻意的 ——
  //    否则同一轮里「A 被换成一个同名的新版本」会撞上 registerSurface 的重复注册断言。
  for (const id of [...loaded.keys()]) {
    const item = alive.get(id)
    if (!item) {
      const before = loaded.get(id)
      if (unloadPlugin(id)) report.unloaded.push(id)
      if (before) console.info(`[surface] 插件 ${id} 已卸载`)
      continue
    }
    if (item.hash !== loaded.get(id)?.hash || item.entry !== loaded.get(id)?.entry) {
      if (unloadPlugin(id)) report.unloaded.push(id)
    }
  }

  // ② 再加载/复用。
  for (const [id, item] of alive) {
    const cur = loaded.get(id)
    if (cur && cur.hash === item.hash && cur.entry === item.entry) {
      report.skipped.push(id)
      clearSurfaceLoadError(cur.kind) // 之前失败过、这一轮 index 又好了 → 清掉旧的错误行
      continue
    }
    try {
      // R-20：**换版本 = 干净重来**。放在 apply 之前是刻意的 —— 新版本连 import 都过不去
      // 时，旧版本留下的「3 次失败 → 自动禁用」会继续挂在墓碑上，用户看到的是「装了新版
      // 还是坏的」。真正的坏版本由 status.ts 的 load error 报告，不走这套计数。
      noteSurfaceVersion(pluginKindOf(id), item.hash)
      const p = await applyManifest(item, baseUrl, deps)
      loaded.set(id, p)
      clearSurfaceLoadError(p.kind)
      deps.send?.('surface_status', { kind: p.kind, mounted: true, disabled: false, error: '' })
      report.loaded.push(id)
    } catch (e) {
      report.failed.push(reportFailure(item, deps, e))
    }
  }
  return report
}

/**
 * syncPlugins 启动与热变更的**唯一入口**。
 *
 * 并发语义：调用方（启动 + 每次 surfaces_changed）不需要自己去重 —— 这里串成一条链，
 * 后来的调用在前一次结束后再跑一次（合并语义而不是排队 N 次：链上永远只多一次）。
 * 传进来的 index 是**调用那一刻**的快照，所以「连续两次 surfaces_changed」第二次
 * 通常只需要处理增量（hash 未变的走 skipped）。
 */
export function syncPlugins(index: SurfaceIndex, baseUrl: SurfaceBaseUrl = baseUrlOverride ?? surfaceBaseUrl, deps: PluginHostDeps = {}): Promise<SyncReport> {
  const run = syncChain.then(() => syncOnce(index, baseUrl, deps))
  // 链上只保留「最后一次」失败不炸链：否则一次失败会让后续 sync 永远 reject。
  syncChain = run.catch(() => ({ loaded: [], unloaded: [], skipped: [], failed: [] }))
  return run
}

export interface PluginLoaderDeps extends PluginHostDeps {
  /** 读索引（生产 = window.desktop.surfaceIndex，走主进程；e2e = 探针内的假数据） */
  readIndex: () => Promise<SurfaceIndex>
  baseUrl?: SurfaceBaseUrl
}

/**
 * startPluginLoader 生产入口：立刻同步一次 + 每次 `surfaces_changed` 再同步一次。
 *
 * 返回 stop()：**只停止监听，不卸载已加载的插件**。理由：注册表住在渲染层，与 bridge
 * 进程无关 —— bridge 重启（start() 再来一次）时把面板全卸掉，用户会看到标签一个接一个
 * 变墓碑，而它们一秒后又会全回来（那是噪音，不是状态）。
 */
export function startPluginLoader(deps: PluginLoaderDeps): () => void {
  let stopped = false
  const baseUrl = baseUrlOverride ?? deps.baseUrl ?? surfaceBaseUrl
  const run = (): void => {
    if (stopped) return
    void deps
      .readIndex()
      .then((index) => syncPlugins(index, baseUrl, deps))
      .catch((e: unknown) => {
        // 读索引失败**不改动注册表**：一次 IPC 抖动不该等于「所有插件都被卸载」。
        console.warn('[surface] 读插件索引失败（保持现状）：', e)
      })
  }
  run()
  const off = subscribeUI('surfaces_changed', () => run())
  return () => {
    stopped = true
    off()
  }
}

/** 仅供测试/探针：卸载全部（生产无调用点）。 */
export function unloadAllPluginsForTest(): void {
  for (const id of [...loaded.keys()]) unloadPlugin(id)
}
