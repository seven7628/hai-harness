// 虚拟 Project（插件）= 第三类会话容器的**纯函数**：识别 / 分组 / 回执归一化 / 合并。
//
// 虚拟 Project 仍放在 `projects` 数组里（docs/creator-mode-v0.1/09-agent-bridge.md §4.1），
// 这样会话列表、`list` 回执、左栏会话渲染都能原样复用；区别只有 `virtual: true`。
// 因此「哪些地方要看它、哪些地方必须看不见它」全部靠这里的小函数表达，
// 而不是在每个调用点各写一次 `p.virtual`（漏一处就是一次跨项目误投）。
//
// 依赖方向：本模块只 type-only import store 的 Project，运行时不碰 store（node 单测可直跑）。

import type { Project } from '../store/useAppStore'
import { isPluginProjectPath } from './ownerPath'

/** 只读视图：调用方可能传 Partial（回执归一的中间态）。 */
type ProjectLike = Pick<Project, 'path'> & Partial<Project>

/** isVirtualProject 是不是虚拟 Project。
 *
 *  两个判据取或：`virtual:true` 是权威标记（后端回执/本地水合都写它）；
 *  路径特征兜住「回执还没到、手上只有 path」的早期窗口（左栏点击、命令路由）。
 *  两者都为假 → 真实工作区，行为与 L0 完全一致。 */
export function isVirtualProject(p: ProjectLike | undefined | null): boolean {
  if (!p) return false
  return p.virtual === true || isPluginProjectPath(p.path)
}

/** realProjects 只留真实工作区（workspace 选择器 / 最近项目 / 置顶 / 项目层 MCP·技能 /
 *  hooks 信任 / IM 路由候选都用它）—— 虚拟 Project 在这些地方出现就是把插件当项目。 */
export function realProjects<T extends ProjectLike>(list: readonly T[]): T[] {
  return list.filter((p) => !isVirtualProject(p))
}

/** splitPluginProjects 左栏插件分组的两个子组：安装中（installed !== false）/ 已卸载。
 *
 *  `installed` 缺省按「已安装」处理：后端早期回执没有该字段时，插件项目必须依然可见
 *  （否则一次字段缺失就让整个分组消失）。 */
export function splitPluginProjects<T extends ProjectLike>(list: readonly T[]): { installed: T[]; uninstalled: T[] } {
  const installed: T[] = []
  const uninstalled: T[] = []
  for (const p of list) {
    if (!isVirtualProject(p)) continue
    if (p.installed === false) uninstalled.push(p)
    else installed.push(p)
  }
  return { installed, uninstalled }
}

/** pluginProjectLabel 行内显示名：后端给 name → pluginId → 路径尾段（`…/plugin-sessions/<id>/home`
 *  的尾段是 `home`，意义不大，所以再退回上一段）。 */
export function pluginProjectLabel(p: ProjectLike): string {
  const name = String(p.name ?? '').trim()
  if (name) return name
  const id = String(p.pluginId ?? '').trim()
  if (id) return id
  const segs = String(p.path ?? '').split('/').filter(Boolean)
  const tail = segs[segs.length - 1] ?? ''
  return tail === 'home' ? (segs[segs.length - 2] ?? tail) : tail || String(p.path ?? '')
}

function asString(v: unknown): string {
  return typeof v === 'string' ? v.trim() : ''
}

/** pluginProjectFromWire 把一条回执条目归一成 Project 增量（未知形状 → undefined，不猜）。
 *
 *  字段兼容：`path` / `home`；`plugin_id` / `pluginId` / `id`；`installed` / `enabled`
 *  缺省为 true（见 splitPluginProjects 的说明）。 */
export function pluginProjectFromWire(raw: unknown): Partial<Project> | undefined {
  if (!raw || typeof raw !== 'object') return undefined
  const o = raw as Record<string, unknown>
  const path = asString(o.path) || asString(o.home)
  if (!path) return undefined
  const pluginId = asString(o.plugin_id) || asString(o.pluginId) || asString(o.id)
  const name = asString(o.name) || pluginId || undefined
  const icon = asString(o.icon)
  return {
    path,
    virtual: true,
    pluginId: pluginId || undefined,
    name,
    icon: icon || undefined,
    installed: o.installed !== false,
    enabled: o.enabled !== false,
  }
}

/** pluginProjectsFromResponse 从 command_response 里取项目数组，兼容三种形状：
 *   · `data.projects`（与 `list` 同类：数据都在 data 里）
 *   · `data.data.projects`（桥再包一层的回执）
 *   · 顶层 `projects`（早期/调试形态：字段挂在回执根上，不进 data）
 *
 *  都取不到 → **undefined**，与「空数组」严格区分：
 *   · undefined = 形状不认识（老 bridge / mock 的 `respond({})`）→ 调用方**不动**已加载的
 *     虚拟项目（当成「插件全没了」会把整组抹掉）；
 *   · [] = 后端明确说「当前没有插件 Project」→ 允许合并（有会话的转已卸载）。 */
export function pluginProjectsFromResponse(r: {
  data?: Record<string, unknown> | undefined
  raw?: Record<string, unknown> | undefined
}): unknown[] | undefined {
  const data = r.data ?? {}
  const nested = data.data && typeof data.data === 'object' ? (data.data as Record<string, unknown>) : undefined
  for (const candidate of [data.projects, nested?.projects, r.raw?.projects]) {
    if (Array.isArray(candidate)) return candidate
  }
  return undefined
}

/** mergePluginProjects 把 `plugin_projects_list` 回执合并进项目树。
 *
 *  规则（每条都有个「不这样做会怎样」）：
 *   · 按 path 去重，**保留本地 sessions** —— 回执只描述插件容器，会话列表由 `list` 水合；
 *     覆盖 sessions 会让左栏会话闪烁消失。
 *   · 真实项目原样保留且**位置不变**（orderProjects 只把 pinned 提前，插件项目追加在尾部，
 *     左栏自己分组，所以现有排序/折叠状态不受影响）。
 *   · 回执里没有的虚拟项目：有会话 → 标 `installed:false`（已卸载，仍可读/删）；
 *     无会话 → 摘掉（没有内容可展示，留着只是一个空壳）。
 *   · 新路径追加到尾部。
 */
export function mergePluginProjects(
  prev: readonly Project[],
  incoming: readonly unknown[],
): { projects: Project[]; paths: string[] } {
  const wireByPath = new Map<string, Partial<Project>>()
  for (const raw of incoming) {
    const wire = pluginProjectFromWire(raw)
    if (wire?.path) wireByPath.set(wire.path, wire)
  }
  const next: Project[] = []
  const merged = new Set<string>()
  for (const p of prev) {
    if (!isVirtualProject(p)) {
      next.push(p)
      continue
    }
    const wire = wireByPath.get(p.path)
    if (!wire) {
      if (p.sessions?.length) next.push({ ...p, installed: false })
      continue
    }
    merged.add(p.path)
    next.push({ ...p, ...wire, sessions: p.sessions ?? [] })
  }
  for (const [path, wire] of wireByPath) {
    if (merged.has(path)) continue
    next.push({ path, sessions: [], ...wire, virtual: true })
  }
  return { projects: next, paths: [...wireByPath.keys()] }
}
