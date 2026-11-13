// 右栏 Surface 契约（PR-00：只钉接口，不接 UI）
// 设计口径见 docs/creator-mode-v0.1/01-design.md §3、§5.1；实现参考 03-code-plan.md §2.1。
import type { ReactNode } from 'react'
import type { AppState, TabInstance } from '../store/useAppStore'

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

/** M1 填充：插件面板唯一可用的宿主能力（见 03 §3.6）；M0 阶段不注入 */
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
