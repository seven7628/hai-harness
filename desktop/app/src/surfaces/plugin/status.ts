// surfaces/plugin/status.ts —— 插件加载失败的**唯一**登记处（PR-14 C）。
//
// 为什么单独一个模块（而不是塞进 loader 或 health）：
//   · loader 要写它、uiSnapshot 要读它，两边直接 import 就成了「store → uiSnapshot →
//     loader → store」的运行期环（loader 要调 store.openTab）；
//   · 本模块**不 import 任何会反向依赖它的东西**（只 import registry 的广播函数），
//     所以任何一侧引用它都不会成环。
//
// 与 PR-10 的 health 是什么关系：health 数「provider 挂载后崩溃」（面板在、渲染炸），
// 这里记「provider 根本没装上」（模块 import 失败 / kind 不符 / apply 抛错）。两件事，
// 两类失败，两条恢复路径 —— 混成一份会让模型分不清「插件坏了」还是「插件没装成」。
//
// 消费面有三处（同一份数据，不许各自再记一份）：
//   1. `ui_inspect` 的 surfaces[]（lib/uiSnapshot.ts 追加带 error 的条目）；
//   2. 渲染层墓碑（没注册上 = `plugin:<id>` 的标签显示 missing，PR-10 机制）；
//   3. 控制台（loader 自己 console.warn，单插件失败不静默）。
import { notifySurfacesChanged } from '../registry'
import type { SurfaceKind } from '../types'

export interface PluginSurfaceError {
  /** 期望的 kind（`plugin:<id>`）；插件连 kind 都没声明时也用它做键 */
  kind: SurfaceKind
  /** 插件 id（= 目录名） */
  id: string
  /** 窄化后的失败原因（≤300 字符，与 health 的 messageOf 同一口径） */
  error: string
  at: number
}

const errors = new Map<string, PluginSurfaceError>()

function clip(msg: unknown): string {
  const s = msg instanceof Error ? msg.message : String(msg)
  return s.length > 300 ? `${s.slice(0, 300)}…` : s
}

/**
 * setSurfaceLoadError 记一条失败并广播（广播 = 快照重算，`ui_inspect` 立刻能看到）。
 *
 * 键用 kind 而不是 id：墓碑与快照都按 kind 索引（`plugin:<id>`），用 id 就得在两处
 * 各自拼一次字符串，而拼错的表现是「错误看不见」。
 */
export function setSurfaceLoadError(kind: SurfaceKind, id: string, err: unknown): PluginSurfaceError {
  const entry: PluginSurfaceError = { kind, id, error: clip(err), at: Date.now() }
  errors.set(kind, entry)
  notifySurfacesChanged()
  return entry
}

/** clearSurfaceLoadError 这一条恢复了（重装/重新启用成功）→ 从清单里消失。 */
export function clearSurfaceLoadError(kind: SurfaceKind): void {
  if (!errors.delete(kind)) return
  notifySurfacesChanged()
}

/** listSurfaceLoadErrors 码点排序（稳定输出：快照逐字节可比，告警不抖动）。 */
export function listSurfaceLoadErrors(): PluginSurfaceError[] {
  return [...errors.values()].sort((a, b) => (a.kind < b.kind ? -1 : a.kind > b.kind ? 1 : 0))
}

/** getSurfaceLoadError 单个 kind 的失败原因（加载器重试前先清）。 */
export function getSurfaceLoadError(kind: SurfaceKind): PluginSurfaceError | undefined {
  return errors.get(kind)
}

/** 仅供测试/探针：清空登记表（生产无调用点）。 */
export function resetSurfaceLoadErrorsForTest(): void {
  errors.clear()
}
