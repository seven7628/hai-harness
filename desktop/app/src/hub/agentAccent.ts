import type { HubGroup } from './types'

// 智能体/群组的视觉身份色（P4-UI-1 细化）。
//
// 规则：
//   - 智能体：用 profile.accent（编辑器里的 color input，#rrggbb）；缺省时按 id 稳定散列到调色板。
//   - 群组：群主 accent → 第一个成员 accent → 按群 id 稳定散列。
//   - 这个 accent 只用于「侧栏环/未读徽标/状态点」；头像与名字的身份色来自
//     lunaAvatar 的 12 色 role palette（严格对齐 demo），两者不得互相回读。
// 纯函数，方便在 UI 与测试里复用（不依赖 store / DOM）。

const FALLBACK_PALETTE = [
  '#b8492f', '#2f7a4f', '#4a5aa8', '#9a5b13',
  '#7a4fa8', '#1f7a7a', '#a8425f', '#5b6b2f',
]

/** 校验 accent：只接受 #rgb/#rgba/#rrggbb/#rrggbbaa。
 *  统一限制为 hex，保证 contrastText 能解析出同一个颜色（避免浅色 badge 配白字）。 */
export function safeAccent(raw: unknown): string | null {
  const s = typeof raw === 'string' ? raw.trim() : ''
  if (!s) return null
  return /^#(?:[0-9a-f]{3,4}|[0-9a-f]{6}|[0-9a-f]{8})$/i.test(s) ? s : null
}

function hashString(input: string): number {
  let h = 2166136261
  for (let i = 0; i < input.length; i++) {
    h ^= input.charCodeAt(i)
    h = Math.imul(h, 16777619)
  }
  return h >>> 0
}

/** 稳定回退色：同一 id 永远同一支，跨会话/刷新不跳色。 */
export function accentForID(id: string): string {
  const key = id.trim() || 'agent'
  return FALLBACK_PALETTE[hashString(key) % FALLBACK_PALETTE.length]
}

export function accentOfAgent(agent: { id?: string; name?: string; accent?: string } | null | undefined): string {
  if (!agent) return FALLBACK_PALETTE[0]
  return safeAccent(agent.accent) ?? accentForID(agent.id || agent.name || 'agent')
}

/**
 * 群组身份色：优先群主，其次成员（用各自 accent / 稳定回退）；
 * 都没有时按群 id 散列。成员顺序照后端返回，保证稳定。
 */
export function accentOfGroup(
  group: Pick<HubGroup, 'id' | 'name' | 'owner_agent_id' | 'members'> | null | undefined,
  agents: Array<{ id?: string; name?: string; accent?: string }>,
): string {
  if (!group) return FALLBACK_PALETTE[0]
  const byID = new Map(agents.filter((a) => a.id).map((a) => [a.id as string, a]))
  const ids = [group.owner_agent_id, ...(group.members ?? []).map((m) => m.agent_id)].filter(
    (id): id is string => typeof id === 'string' && id !== '',
  )
  for (const id of ids) {
    const member = byID.get(id)
    if (member) return accentOfAgent(member)
  }
  if (ids.length > 0) return accentForID(ids[0])
  return accentForID(group.id || group.name || 'group')
}

/** 实心身份色上的文字：按相对亮度选近黑/白，避免浅绿/浅蓝上的白字读不清。 */
export function contrastText(accent: string): '#ffffff' | '#1b1b1b' {
  const rgb = parseHex(accent)
  if (!rgb) return '#ffffff'
  const [r, g, b] = rgb.map((v) => {
    const c = v / 255
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4
  })
  const luminance = 0.2126 * r + 0.7152 * g + 0.0722 * b
  const whiteContrast = 1.05 / (luminance + 0.05)
  const blackContrast = (luminance + 0.05) / 0.05
  return blackContrast >= whiteContrast ? '#1b1b1b' : '#ffffff'
}

function parseHex(accent: string): [number, number, number] | null {
  const s = accent.trim()
  const m = /^#([0-9a-f]{3,4}|[0-9a-f]{6}|[0-9a-f]{8})$/i.exec(s)
  if (!m) return null
  let hex = m[1]
  if (hex.length === 3 || hex.length === 4) hex = hex.slice(0, 3).split('').map((ch) => ch + ch).join('')
  return [parseInt(hex.slice(0, 2), 16), parseInt(hex.slice(2, 4), 16), parseInt(hex.slice(4, 6), 16)]
}
