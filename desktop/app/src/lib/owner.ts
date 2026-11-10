// 归属（owner）在前端的唯一编码实现 —— 与 Go 侧的 owner 包逐字对应。
//
// 为什么前端也需要它：hooks（信任）与 IM（启用/路由）的管理面都按归属展示与写入。
// 如果前端自己拼字符串（或干脆不传、让服务端猜），就会出现「设置页显示给 A，实际确认
// 给了 B」—— 而那种错配不会报错，只会表现为「明明确认过了却不生效」。
//
// 语法（与 bridge/owner_wire.go、hooks/trust.go 一致）：
//
//	workspace:<绝对路径>   项目会话
//	agent:<agentID>       Agent Hub 智能体 DM
//	group:<groupID>       Agent Hub 群聊
//
// **不共享**：同一 agent 的 DM 与它的群聊是两个归属（产品决策：最安全的默认）。

export type OwnerKind = 'workspace' | 'agent' | 'group' | ''

export const LEGACY_OWNER = 'legacy'

export function workspaceOwner(path: string): string {
  const p = String(path ?? '').trim()
  return p ? `workspace:${p}` : ''
}

export function agentOwner(id: string): string {
  const s = String(id ?? '').trim()
  return s ? `agent:${s}` : ''
}

export function groupOwner(id: string): string {
  const s = String(id ?? '').trim()
  return s ? `group:${s}` : ''
}

export function ownerKind(owner: string): OwnerKind {
  const s = String(owner ?? '')
  if (s.startsWith('workspace:') && s.length > 'workspace:'.length) return 'workspace'
  if (s.startsWith('agent:') && s.length > 'agent:'.length) return 'agent'
  if (s.startsWith('group:') && s.length > 'group:'.length) return 'group'
  return ''
}

/** ownerValid 可写入的归属（legacy 只读，不算）。 */
export function ownerValid(owner: string): boolean {
  return ownerKind(owner) !== ''
}

/** ownerId 归属的标识部分（workspace 取路径，agent/group 取 id）。 */
export function ownerId(owner: string): string {
  const s = String(owner ?? '')
  for (const p of ['workspace:', 'agent:', 'group:']) {
    if (s.startsWith(p)) return s.slice(p.length)
  }
  return ''
}

/**
 * sessionOwner 会话身份 → 归属（与 Go 的 owner.FromSessionID 同一套规则）：
 *   hub.<agent>.dm        → agent:<agent>
 *   hub.<agent>.g.<group> → group:<group>
 *   其它                   → workspace:<workspace>
 *
 * hub 前缀但形状不对 → 空串（显式「无法归属」，**不**回落成 workspace —— 那会把智能体
 * home 当成一个项目归属）。桥侧对未知归属的写入会明确拒绝。
 */
export function sessionOwner(sessionId: string, workspace: string): string {
  const sid = String(sessionId ?? '')
  const ws = String(workspace ?? '')
  const PREFIX = 'hub.'
  if (sid.startsWith(PREFIX)) {
    const rest = sid.slice(PREFIX.length)
    const gi = rest.indexOf('.g.')
    if (gi >= 0) return groupOwner(rest.slice(gi + '.g.'.length))
    if (rest.endsWith('.dm')) return agentOwner(rest.slice(0, -'.dm'.length))
    return ''
  }
  return workspaceOwner(ws)
}

/** ownerLabel 人类可读短标签（设置页展示；判定一律用 owner 字符串本身）。 */
export function ownerLabel(owner: string): string {
  switch (ownerKind(owner)) {
    case 'workspace':
      return `项目会话（${ownerId(owner)}）`
    case 'agent':
      return `智能体 ${ownerId(owner)}`
    case 'group':
      return `群聊 ${ownerId(owner)}`
    default:
      return owner === LEGACY_OWNER ? '迁移前的全局信任' : '未绑定归属'
  }
}
