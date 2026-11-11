// 智能体头像 sigil v2 的纯逻辑（方案 03 §3.3 / demo avatar-v2.html）。
//
// 两条硬规则：
//   1. 身份不动，状态才动：shape/hue 只由 agentId 决定，responsibility 只改内侧 trait；
//   2. 状态不靠颜色：四态由环/弧/加粗环+点/空心轮廓的形状差异承载。
//
// 本文件不 import React、不碰 DOM、不读时钟/随机/storage/网络，Node 直测
// （scripts/test-hub-avatar.mjs）。渲染在 AgentSigil.tsx / AgentStack.tsx。

export type AvatarState = 'idle' | 'working' | 'needs_attention' | 'offline'

/** 只允许这三档：更小的头像轮廓与状态环都不可辨（方案 §3.3.5）。 */
export const SIGIL_SIZES = [24, 32, 48] as const
export type SigilSize = (typeof SIGIL_SIZES)[number]

/** 48 单位视图框：环半径统一 22，外接 22.75，留 1 单位余量不裁切。 */
export const SIGIL_VIEWBOX = 48

export const AVATAR_STATES: readonly AvatarState[] = ['idle', 'working', 'needs_attention', 'offline']

/** 优先级：审批最高（唯一需要人立刻行动），其次离线，再工作中。 */
export const STATE_PRIORITY: Record<AvatarState, number> = {
  needs_attention: 4,
  offline: 3,
  working: 2,
  idle: 1,
}

export interface ShapeDef {
  id: string
  d: string
}

/** 8 种轮廓：40×40 内切盒，24px 下圆/方/尖/长/斜仍可区分。 */
export const SHAPES: readonly ShapeDef[] = [
  { id: 'orb', d: 'M24 4a20 20 0 1 1 0 40 20 20 0 0 1 0-40z' },
  { id: 'squircle', d: 'M17 4h14a13 13 0 0 1 13 13v14a13 13 0 0 1-13 13H17A13 13 0 0 1 4 31V17A13 13 0 0 1 17 4z' },
  { id: 'hex', d: 'M12 6h24l8 18-8 18H12L4 24z' },
  { id: 'tri', d: 'M24 6q2 0 3 2l15 29q1.5 3-2 3H8q-3.5 0-2-3L21 8q1-2 3-2z' },
  { id: 'diamond', d: 'M24 5q1.6 0 2.9 1.9L41.1 21q2.9 3-.1 5.9L26.9 41.1Q24 44 21.1 41.1L6.9 26.9Q4 24 6.9 21.1L21.1 6.9Q22.4 5 24 5z' },
  { id: 'leaf', d: 'M24 5c12 7 17 29 0 38C7 34 12 12 24 5z' },
  { id: 'capsule', d: 'M24 4a14 14 0 0 1 14 14v12a14 14 0 0 1-28 0V18A14 14 0 0 1 24 4z' },
  { id: 'arch', d: 'M4 44V24A20 20 0 0 1 44 24v20z' },
]

export interface HueDef {
  id: string
  /** ink（深色主题）填充值。 */
  dark: string
  /** paper（亮色主题）填充值。 */
  light: string
}

/** 8 组身份色：同一色相按主题取填充值，不靠透明混色（对比度见 §3.3.3）。 */
export const HUES: readonly HueDef[] = [
  { id: 'sage', dark: '#8fbfae', light: '#3f6b5d' },
  { id: 'clay', dark: '#d3927a', light: '#79452f' },
  { id: 'sky', dark: '#8ab4f8', light: '#2f5c9e' },
  { id: 'lilac', dark: '#b6a4e0', light: '#5a4a86' },
  { id: 'moss', dark: '#a9c47c', light: '#4e6a27' },
  { id: 'rose', dark: '#d99aa6', light: '#7d3d4b' },
  { id: 'slate', dark: '#9aa8b6', light: '#46525f' },
  { id: 'sand', dark: '#cbb493', light: '#6b5738' },
]

/** 内侧标记的几何：由渲染层翻译成 circle/rect/path，避免组件里拼 SVG 字符串。 */
export type TraitMark =
  | { type: 'circle'; cx: number; cy: number; r: number }
  | { type: 'rect'; x: number; y: number; width: number; height: number; rx: number }
  | { type: 'line'; d: string; width: number }

export interface TraitDef {
  id: 'dot' | 'bar' | 'chevron' | 'dot_pair'
  marks: readonly TraitMark[]
}

/** 4 个内侧标记，24px 下由 CSS 隐藏（不在生成逻辑里分叉）。 */
export const TRAITS: readonly TraitDef[] = [
  { id: 'dot', marks: [{ type: 'circle', cx: 24, cy: 24, r: 4 }] },
  { id: 'bar', marks: [{ type: 'rect', x: 15, y: 22, width: 18, height: 4, rx: 2 }] },
  { id: 'chevron', marks: [{ type: 'line', d: 'M16 29 24 19 32 29', width: 3.2 }] },
  {
    id: 'dot_pair',
    marks: [
      { type: 'circle', cx: 18.5, cy: 24, r: 3 },
      { type: 'circle', cx: 29.5, cy: 24, r: 3 },
    ],
  },
]

/** 归一化职责：NFKC + trim + 小写 + 折叠空白 + 截断 256。 */
export function normalizeResponsibility(input: unknown): string {
  return String(input == null ? '' : input)
    .normalize('NFKC')
    .trim()
    .toLowerCase()
    .replace(/\s+/g, ' ')
    .slice(0, 256)
}

/** 32 位 FNV-1a（UTF-8 字节）：跨平台同值，不依赖 JS 字符编码细节。 */
export function fnv1a(input: string): number {
  let h = 0x811c9dc5
  const bytes = new TextEncoder().encode(input)
  for (let i = 0; i < bytes.length; i++) {
    h ^= bytes[i]
    h = Math.imul(h, 0x01000193) >>> 0
  }
  return h >>> 0
}

export interface SigilIdentity {
  shape: number
  hue: number
  trait: number
  shapeId: string
  hueId: string
  traitId: string
  hId: number
  hTrait: number
  /** 实际参与散列的 id（无 id 时为 `anon|<norm(resp)>`）。 */
  seedId: string
}

/**
 * 确定性推导：同一 (agentId, responsibility) 在任何位置渲染出同一图形。
 * 缺 id 时退化为 `anon|<norm(resp)>`，仍然渲染，不出现空头像。
 */
export function deriveSigil(agentId?: string | null, responsibility?: string | null): SigilIdentity {
  const resp = normalizeResponsibility(responsibility)
  let id = String(agentId == null ? '' : agentId)
  if (!id) id = `anon|${resp}`
  const hId = fnv1a(`avatar1|${id}`)
  // trait 只受 responsibility 影响；改职责不会换形状/色相。
  const hTrait = fnv1a(`avatar1|${id}|${resp}`)
  const shape = hId % 8
  const hue = (hId >>> 3) % 8
  const trait = (hTrait >>> 5) % 4
  return {
    shape,
    hue,
    trait,
    shapeId: SHAPES[shape].id,
    hueId: HUES[hue].id,
    traitId: TRAITS[trait].id,
    hId,
    hTrait,
    seedId: id,
  }
}

export function isAvatarState(value: unknown): value is AvatarState {
  return typeof value === 'string' && (AVATAR_STATES as readonly string[]).includes(value)
}

/** 状态优先级：数值大者胜；未知值按 idle。 */
export function stateRank(state: AvatarState | string | null | undefined): number {
  return isAvatarState(state) ? STATE_PRIORITY[state] : STATE_PRIORITY.idle
}

export function highestState(states: Iterable<AvatarState>): AvatarState {
  let best: AvatarState = 'idle'
  for (const state of states) {
    if (stateRank(state) > stateRank(best)) best = state
  }
  return best
}

export interface AgentStateInput {
  /** store.running[agentId] === true。 */
  running?: boolean
  /** needs 里该 agent 的 status === 'pending'。 */
  pending?: boolean
  /** agent.deleted（停用/删除）。 */
  deleted?: boolean
}

/**
 * 数据 → 四态：pending > deleted > running > idle。
 * 顺序就是优先级（needs_attention > offline > working > idle），不是先到先得。
 */
export function resolveAgentState(input: AgentStateInput | null | undefined): AvatarState {
  if (!input) return 'idle'
  if (input.pending) return 'needs_attention'
  if (input.deleted) return 'offline'
  if (input.running) return 'working'
  return 'idle'
}

export interface StateSources {
  /** store.running。 */
  running?: Readonly<Record<string, boolean>>
  /** needs 里 status==='pending' 的 agent_id。 */
  pending?: ReadonlySet<string>
}

/** 把 store 的两张表收成一个 `(agentId, deleted?) => state`，列表/叠放逐成员复用。 */
export function stateResolver(sources: StateSources): (agentId: string, deleted?: boolean) => AvatarState {
  const running = sources.running ?? {}
  const pending = sources.pending ?? new Set<string>()
  return (agentId, deleted) => resolveAgentState({
    running: running[agentId] === true,
    pending: pending.has(agentId),
    deleted,
  })
}

export interface StateCounts {
  idle: number
  working: number
  needs_attention: number
  offline: number
  total: number
}

/** 叠放口径的状态摘要（渲染层用 i18n 拼 aria-label，不在这里硬编码文案）。 */
export function countStates(states: Iterable<AvatarState>): StateCounts {
  const out: StateCounts = { idle: 0, working: 0, needs_attention: 0, offline: 0, total: 0 }
  for (const state of states) {
    out[isAvatarState(state) ? state : 'idle']++
    out.total++
  }
  return out
}

export interface StackInputMember {
  id: string
  responsibility?: string
  state: AvatarState
}

export interface StackPlanMember {
  id: string
  responsibility: string
  state: AvatarState
  /** 最左成员承担整组状态。 */
  lead: boolean
  /** 只有 lead 画状态层；其余成员只画身份。 */
  withStatus: boolean
}

export interface StackPlan {
  members: StackPlanMember[]
  /** `+N` 的数字；0 表示不画徽标。 */
  more: number
  total: number
  /** 组状态 = 成员最高优先级（群会话在跑且无人更高时升级为 working）。 */
  state: AvatarState
  counts: StateCounts
}

/**
 * Group 叠放：2–4 个全显示；≥5 显示前 4 + `+N`（demo 口径）。
 * 状态层只画一次（lead），4 个环叠在一起会变成噪声。
 */
export function planStack(
  input: readonly StackInputMember[],
  opts?: { groupRunning?: boolean },
): StackPlan {
  const states = input.map((m) => (isAvatarState(m.state) ? m.state : 'idle'))
  const counts = countStates(states)
  let state = highestState(states)
  // 群会话在跑：没有更高优先级（needs_attention/offline）时，lead 显示 working。
  if (opts?.groupRunning && state === 'idle') state = 'working'
  const visible = input.length >= 5 ? input.slice(0, 4) : input.slice()
  const members = visible.map((m, i) => ({
    id: m.id,
    responsibility: m.responsibility ?? '',
    state: i === 0 ? state : (isAvatarState(m.state) ? m.state : 'idle'),
    lead: i === 0,
    withStatus: i === 0,
  }))
  return {
    members,
    more: input.length - visible.length,
    total: input.length,
    state,
    counts,
  }
}
