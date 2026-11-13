// IM 路由的「投递目标」纯逻辑（列表展示 / 编辑合并 / 归属派生）。
//
// 为什么单独一层（B2 的修法）：路由的投递目标有三态（workspace / agent / group），
// 而编辑器的表单此前只认 {chat, workspace}。结果是：
//   · 列表只显示 workspace，agent/群路由的目标看不见；
//   · 「同一 chat 再加一条」按 chat 覆盖 → 原有的 agent/group 字段被静默抹掉，
//     消息从「驱动智能体」变成「驱动项目会话」——用户什么提示都没有。
// 这里把「目标怎么读、怎么显示、编辑后怎么合并」做成纯函数（可 Node 直测），
// 组件只负责把用户真正编辑的字段喂进来。
import type { IMRouteCfg } from '../transport/types'
import { agentOwner, groupOwner, ownerValid, workspaceOwner } from './owner'

export type RouteTargetKind = 'workspace' | 'agent' | 'group'

/** 目标维度：群 > 智能体 > 工作区（与 Go 的 RouteConfig.EffectiveOwner 同序）。 */
export function routeTargetKind(r: IMRouteCfg): RouteTargetKind | '' {
  const g = String(r.group ?? '').trim()
  if (g) return 'group'
  if (String(r.agent ?? '').trim()) return 'agent'
  if (String(r.workspace ?? '').trim()) return 'workspace'
  return ''
}

/** 目标值（group 目标取群 id；agent 目标取 agent id；workspace 取路径）。 */
export function routeTargetValue(r: IMRouteCfg): string {
  switch (routeTargetKind(r)) {
    case 'group':
      return String(r.group ?? '').trim()
    case 'agent':
      return String(r.agent ?? '').trim()
    case 'workspace':
      return String(r.workspace ?? '').trim()
    default:
      return ''
  }
}

/** 由目标派生的归属（忽略显式 owner）——与 bridge 的 EffectiveOwner 一致。 */
export function derivedRouteOwner(r: IMRouteCfg): string {
  switch (routeTargetKind(r)) {
    case 'group':
      return groupOwner(String(r.group ?? ''))
    case 'agent':
      return agentOwner(String(r.agent ?? ''))
    case 'workspace':
      return workspaceOwner(String(r.workspace ?? ''))
    default:
      return ''
  }
}

/**
 * 路由的生效归属（展示/筛选用）：
 * 显式 owner 合法就用它（服务端要求它与目标一致），否则由目标派生。
 * 目标缺失且无显式 owner → ''（这类路由不会投递，列表要明确标注「只可删除」）。
 */
export function routeOwner(r: IMRouteCfg): string {
  const own = String(r.owner ?? '').trim()
  if (ownerValid(own)) return own
  return derivedRouteOwner(r)
}

/** 列表展示的目标文案：workspace:<path> / agent:<id> / group:<id>。 */
export function routeTargetLabel(r: IMRouteCfg): string {
  const kind = routeTargetKind(r)
  if (!kind) return ''
  return `${kind}:${routeTargetValue(r)}`
}

/** 目标是否完整可投递（group 目标还要求有执行者 agent，见 im.Config.Validate）。 */
export function routeTargetComplete(r: IMRouteCfg): boolean {
  const kind = routeTargetKind(r)
  if (kind === 'group') return Boolean(String(r.agent ?? '').trim())
  return kind !== ''
}

/** 两个 chat 是否同一条（网关 + chat_id + thread_id；与 Go 的 Chat.Normalize 同口径）。 */
export function sameChat(a: IMRouteCfg['chat'], b: IMRouteCfg['chat']): boolean {
  return (
    String(a?.gateway ?? '') === String(b?.gateway ?? '') &&
    String(a?.chat_id ?? '') === String(b?.chat_id ?? '') &&
    String(a?.thread_id ?? '') === String(b?.thread_id ?? '')
  )
}

/**
 * mergeRouteTarget 把「用户这次编辑的目标」合进既有路由，**只改目标维度本身**：
 *   · workspace 目标改路径 → 只换 path；
 *   · agent 目标改 id   → 只换 agent id；
 *   · group 目标改群 id → 群 id 换掉，**执行者 agent 保留**（否则改一次群就丢了执行者）；
 *   · 目标维度变化（workspace → agent 等）→ 旧维度的字段清掉（残留字段会让归属派生歧义）；
 *   · 显式 owner 仅在仍等于目标派生值时保留（否则服务端 Validate 会拒绝整份配置）。
 */
export function mergeRouteTarget(prev: IMRouteCfg, kind: RouteTargetKind, value: string): IMRouteCfg {
  const v = String(value ?? '').trim()
  const out: IMRouteCfg = { chat: prev.chat }
  if (kind === 'workspace') out.workspace = v
  if (kind === 'agent') out.agent = v
  if (kind === 'group') {
    out.group = v
    const actor = String(prev.agent ?? '').trim()
    if (actor) out.agent = actor
  }
  const own = String(prev.owner ?? '').trim()
  if (own && derivedRouteOwner(out) === own) out.owner = own
  return out
}

/**
 * upsertRoute 按 chat 增/改一条路由：
 * 同 chat 已存在 → 合并（保留 / 更新用户编辑的目标），**绝不清空未被编辑的目标字段**；
 * 否则追加。返回新数组（不改入参）。
 */
export function upsertRoute(routes: IMRouteCfg[], next: IMRouteCfg): IMRouteCfg[] {
  const i = routes.findIndex((r) => sameChat(r.chat, next.chat))
  if (i < 0) return [...routes, next]
  const out = routes.slice()
  out[i] = next
  return out
}

/** 新建一条目标路由（新增表单的提交值）。 */
export function makeRoute(chat: IMRouteCfg['chat'], kind: RouteTargetKind, value: string): IMRouteCfg {
  const v = String(value ?? '').trim()
  switch (kind) {
    case 'workspace':
      return { chat, workspace: v }
    case 'agent':
      return { chat, agent: v }
    case 'group':
      return { chat, group: v }
  }
}
