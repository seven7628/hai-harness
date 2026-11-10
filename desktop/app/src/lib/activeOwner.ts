// 「当前操作上下文」→ 归属（owner）的唯一派生入口。
//
// 为什么需要这样一层（B1 的修法）：归属要同时看两套 store——
//   · Hub 的会话 id（hub.<agent>.dm）与群 id 在 useHubStore 里；
//   · 工作区会话 id 在 useAppStore 里。
// 而 useAppStore.send() 是**非 React 路径**（不能订阅 hook），只能读 getState()。
// 改造前它只读 useAppStore.activeSessionId —— Hub 打开时那个值是项目会话（或者是空），
// 于是设置页在 Hub DM/群里发出的 hooks_trust / plugin_enable / im_route_* 全被算成
// workspace:<工作区>：用户以为是给这个智能体确认的，服务端却记到项目名下，而派发按
// agent:/group: 查 → 「明明确认过了却不生效」。
//
// 设计：App.tsx 单点把 Hub 上下文写进这里（订阅 useHubStore），读侧有两个入口：
//   · resolveActiveOwner(appSid, ws) —— 非 React 路径（send / 事件回调）；
//   · ownerForContext(ctx, appSid, ws) —— React 路径（组件拿到响应的订阅值再算，
//     这样 Hub 切换会触发重渲染，而不是读到一份可能过期的模块级快照）。
//
// 不猜：Hub 已打开但没有选中 DM/群（sid 与 groupId 都空）→ 返回空串（「未绑定归属」）。
// 此时写操作会被服务端明确拒绝（hooks_trust 缺 owner → 报错），**不会**悄悄落到项目归属上。
import { groupOwner, sessionOwner } from './owner'

/** Hub 的当前上下文（open=false 时其余字段无意义）。 */
export interface HubOwnerContext {
  open: boolean
  /** 当前 DM 的桥侧 session_id（hub.<agent>.dm）；群视图下为 null。 */
  sid: string | null
  /** 当前群 id（hub 群视图；DM 视图下为 null）。 */
  groupId: string | null
}

const HUB_CLOSED: HubOwnerContext = { open: false, sid: null, groupId: null }

let current: HubOwnerContext = HUB_CLOSED

/** setHubOwnerContext 由 App.tsx 的订阅写入（Hub 打开/切 DM/切群/关闭）。 */
export function setHubOwnerContext(next: HubOwnerContext): void {
  current = {
    open: next.open === true,
    sid: next.sid ? String(next.sid) : null,
    groupId: next.groupId ? String(next.groupId) : null,
  }
}

/** 当前 Hub 上下文（诊断/测试用；正常读路径走 resolveActiveOwner）。 */
export function getHubOwnerContext(): HubOwnerContext {
  return current
}

/**
 * ownerForContext 给定上下文 → 归属：
 *   Hub 打开 + 选中群   → group:<群 id>
 *   Hub 打开 + 选中 DM  → agent:<agent>（由 hub.<agent>.dm 派生，与桥侧同一规则）
 *   Hub 打开但未选中    → ''（未绑定，绝不猜成 workspace）
 *   Hub 关闭            → 工作区会话 → workspace:<ws>；Hub 会话 id 残留也仍按 hub 规则派生
 */
export function ownerForContext(ctx: HubOwnerContext, appSessionId: string, workspace: string): string {
  if (ctx.open) {
    if (ctx.groupId) return groupOwner(ctx.groupId)
    if (ctx.sid) {
      // 只解析 hub.* 形状；Hub 里若拿到非 hub 的 sid（不该发生）→ 未绑定，不回落 workspace。
      const sid = String(ctx.sid)
      return sid.startsWith('hub.') ? sessionOwner(sid, '') : ''
    }
    return ''
  }
  return sessionOwner(appSessionId, workspace)
}

/** resolveActiveOwner 非 React 路径用：读最近一次写入的 Hub 上下文。 */
export function resolveActiveOwner(appSessionId: string, workspace: string): string {
  return ownerForContext(current, appSessionId, workspace)
}

/** 测试/热重载用：清空上下文（生产路径只有 setHubOwnerContext）。 */
export function resetHubOwnerContext(): void {
  current = HUB_CLOSED
}
