// 右栏 Surface 契约（PR-00：只钉接口，不接 UI）
// 设计口径见 docs/creator-mode-v0.1/01-design.md §3、§5.1；实现参考 03-code-plan.md §2.1。
import type { ReactNode } from 'react'
import type { AppState, TabInstance } from '../store/useAppStore'
import type { SurfaceCommandReply, SurfaceEvent } from '../transport/types'

/** 内置 kind 保持 4 个：文档/媒体不是 kind，是 file{mode:'preview'} 的渲染器（D-I） */
export type BuiltinKind = 'activity' | 'git' | 'file' | 'web'
export type SurfaceKind = BuiltinKind | `plugin:${string}`

export type SurfaceGroup = 'workflow' | 'content' | 'plugin'
export type SurfaceScope = 'session' | 'workspace'
export type SurfaceWeight = 'light' | 'heavy'
export type SurfaceCapability = 'read' | 'view' | 'write'

/** Omit 在联合类型上不分配（keyof 取交集），会丢掉 params/path/mode；标签形状必须用这个。 */
export type DistributiveOmit<T, K extends PropertyKey> = T extends unknown ? Omit<T, K> : never

/** 插件标签：身份 = (kind, params)。unavailable = 所属插件缺失/被禁用时的墓碑态 */
export interface PluginTabInstance {
  id: string
  kind: `plugin:${string}`
  params?: Record<string, unknown>
  unavailable?: { reason: string }
}

/** 全量标签联合（内置沿用 store 里既有的四类 + plugin） */
export type SurfaceTab = TabInstance | PluginTabInstance

/** M1 填充：插件面板唯一可用的宿主能力（见 03 §3.6）。
 *
 * 与 03 §3.6 的两处落地差（PR-14 裁决，改回之前先读这段）：
 *   · `command` 的返回是 `SurfaceCommandReply` 且**永不抛**（连 reject 也没有）。
 *     03 里的写法是「未声明就 throw」——实测那会在 `render` 里变成未捕获的 Promise
 *     rejection，而 React 的崩溃卡只接同步 throw：症状是面板白着、控制台一行红、墓碑
 *     不出现。越权是**可编程状态**，与 PR-13 的 `status:"rejected"` 同一口径。
 *   · `Icon` / `useSession` 标成可选，本轮不注入。理由不是省事：`Icon` 需要一套宿主
 *     图标表（本仓没有可动态寻址的图标库），`useSession` 需要一份 SessionProjection
 *     （store 里今天没有这个投影）。先给假实现，插件作者会按假的写，之后改不动。
 */
export interface HostApi {
  React: typeof import('react')
  Icon?: (p: { name: string; size?: number }) => ReactNode
  t: (key: string, vars?: Record<string, unknown>) => string
  theme(): { mode: 'light' | 'dark'; tokens: Record<string, string> }
  useSession?<T>(sel: (s: SessionProjection) => T): T
  /** 白名单命令：未声明/未允许/超时/内部错误一律回 `{ok:false, reason}`，不抛。 */
  command(name: string, payload?: unknown): Promise<SurfaceCommandReply>
  /**
   * 事件入口（09 §5 / §5.2）—— **只有这一个**，且只能订阅 manifest.data 里声明过的 type
   *（未声明 = 抛：写错的事件名必须在 apply 时炸出来，否则症状是「订阅成功但永远收不到」）。
   *
   * 返回 disposer：`const off = host.onEvent('agent.delta', fn)` → 卸载/换订阅时 `off()`。
   * loader 也会把它登记进插件的 disposer 列表（双保险：插件忘了调也不会变成僵尸监听）。
   */
  onEvent(type: string, fn: (ev: SurfaceEvent) => void): () => void
}

export interface SessionProjection {
  id: string
  workspace: string
  model: string
  status: 'idle' | 'running' | 'waiting'
  cost: { usd: number; calls: number; cachePct: number }
  ctx: { used: number; max: number }
}

export interface SurfaceRenderContext<T extends SurfaceTab = SurfaceTab> {
  tab: T
  scope: string
  /** M1 起必填；M0 的内置 provider 不依赖宿主能力 */
  host?: HostApi
}

/** M1 起插件 provider 用这个：host 必填。内置 provider 继续用 SurfaceRenderContext（host 可选）。 */
export type PluginRenderContext<T extends SurfaceTab = SurfaceTab> = SurfaceRenderContext<T> & { host: HostApi }

export interface SurfaceProvider<T extends SurfaceTab = SurfaceTab> {
  kind: SurfaceKind
  namespace: 'builtin' | 'plugin'
  group: SurfaceGroup
  order?: number

  title: (tab: T, t: (k: string, v?: Record<string, unknown>) => string) => string
  /**
   * 标签图标（PR-11 起 RightPanel 不再有本地 kind→图标 switch，这里是唯一真相）。
   *
   * `tab` 是**可选**的：调用点有两种 —— 标签条/溢出菜单有实例；＋ 菜单与空态能力面板只有
   * kind（「能开什么」的静态清单）。实现忽略入参或按 tab 细化（file 的 preview 用眼睛、
   * web 按页面类型）都可以，但**不要**为了让类型通过而造假 tab（传 undefined 就是事实）。
   */
  icon?: (tab?: T) => ReactNode
  badge?: (tab: T, s: AppState) => number
  /**
   * 原生宿主判定（R11c）：这个标签此刻是不是**一块原生视图的宿主** ——
   * 即面板里贴着 Electron 的 WebContentsView，右栏收起 / 换到别的标签时必须把它 hide 掉。
   *
   * 为什么是 provider 声明的能力位，而不是 RightPanel 里的 `kind === 'web' || (kind ===
   * 'file' && …)`：判定权属于 provider（只有它知道自己渲的是什么）。RP 只问「谁在当宿主」，
   * 不认识任何 kind。将来插件面板要挂原生视图，加一条实现即可，RP 一行不改。
   *
   * 为什么不叫 `isNative` / `embedsNativeView`：语义是「**宿主**」（谁负责让原生视图可见，
   * 与 hide 的分工成对），不是「这个标签里有原生内容」—— file{mode:'preview'} 只对
   * pdf/图片这类渲染器成立，而判定落点必须是一个布尔闸门，不是「大概有」。
   *
   * 入参带 `s`（与 badge 同款）：宿主要看**运行时状态**，不能只看标签形状。file 的
   * `mode:'preview'` 只说明「可能渲 pdf」；真正把视图挂上去的是预览面板（它把
   * `previewEmbedActive` 写进 store），而且父级 effect 晚于子级 effect 跑 —— 只看 mode 会在
   * 面板还没挂载时就宣称「我是宿主」，闸门随即不 hide，症状是 pdf 预览一片空白、视图却
   * 在池里（R08b 实测）。所以 file 的实现读这个运行时标记，**不**读自己的 mode。
   *
   * 缺省 = false：未声明 / provider 缺失（加载失败、墓碑态）/ 被禁用的面板都不当宿主。
   * 这条缺省是「面板白屏」的安全侧：宁可我方多 hide 一次（幂等），也不要让一个无人认领的
   * 原生视图浮在界面上（后台会话触发 Browser Use 时的白屏事故）。
   */
  nativeHost?: (tab: T, s: AppState) => boolean

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
