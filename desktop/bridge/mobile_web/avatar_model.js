// avatar_model.js —— 手机端 sigil v2 头像的纯逻辑（无 DOM、无网络）。
//
// 与桌面 desktop/app/src/hub/avatar.ts 同一套算法（demo avatar-v2.html / 03 §3.3）：
//   fnv1a("avatar1|id") → shape + hue；fnv1a("avatar1|id|norm(resp)") → trait；
//   无 id 退化为 anon|norm(resp)；状态层由四态优先级决定。
// 手机页面没有构建步骤，无法直接 import TS，所以这里复制一份纯 JS；
// scripts/test-hub-avatar.mjs 会用同一批 seed 与桌面实现对照，防两套算法漂移。
//
// 约定：不产生 HTML 字符串、不改入参；app.js 只用它返回的值画 DOM。
;(function (root, factory) {
  if (typeof module === 'object' && module.exports) module.exports = factory()
  else root.HaiMobileAvatar = factory()
})(typeof globalThis !== 'undefined' ? globalThis : this, function () {
  'use strict'

  const SIGIL_SIZES = [24, 32, 48]
  const SIGIL_VIEWBOX = 48
  const AVATAR_STATES = ['idle', 'working', 'needs_attention', 'offline']
  const STATE_PRIORITY = { needs_attention: 4, offline: 3, working: 2, idle: 1 }

  const SHAPES = [
    { id: 'orb', d: 'M24 4a20 20 0 1 1 0 40 20 20 0 0 1 0-40z' },
    { id: 'squircle', d: 'M17 4h14a13 13 0 0 1 13 13v14a13 13 0 0 1-13 13H17A13 13 0 0 1 4 31V17A13 13 0 0 1 17 4z' },
    { id: 'hex', d: 'M12 6h24l8 18-8 18H12L4 24z' },
    { id: 'tri', d: 'M24 6q2 0 3 2l15 29q1.5 3-2 3H8q-3.5 0-2-3L21 8q1-2 3-2z' },
    { id: 'diamond', d: 'M24 5q1.6 0 2.9 1.9L41.1 21q2.9 3-.1 5.9L26.9 41.1Q24 44 21.1 41.1L6.9 26.9Q4 24 6.9 21.1L21.1 6.9Q22.4 5 24 5z' },
    { id: 'leaf', d: 'M24 5c12 7 17 29 0 38C7 34 12 12 24 5z' },
    { id: 'capsule', d: 'M24 4a14 14 0 0 1 14 14v12a14 14 0 0 1-28 0V18A14 14 0 0 1 24 4z' },
    { id: 'arch', d: 'M4 44V24A20 20 0 0 1 44 24v20z' },
  ]

  const HUES = [
    { id: 'sage', dark: '#8fbfae', light: '#3f6b5d' },
    { id: 'clay', dark: '#d3927a', light: '#79452f' },
    { id: 'sky', dark: '#8ab4f8', light: '#2f5c9e' },
    { id: 'lilac', dark: '#b6a4e0', light: '#5a4a86' },
    { id: 'moss', dark: '#a9c47c', light: '#4e6a27' },
    { id: 'rose', dark: '#d99aa6', light: '#7d3d4b' },
    { id: 'slate', dark: '#9aa8b6', light: '#46525f' },
    { id: 'sand', dark: '#cbb493', light: '#6b5738' },
  ]

  const TRAITS = [
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

  function normalizeResponsibility(input) {
    return String(input == null ? '' : input)
      .normalize('NFKC')
      .trim()
      .toLowerCase()
      .replace(/\s+/g, ' ')
      .slice(0, 256)
  }

  function fnv1a(input) {
    let h = 0x811c9dc5
    const bytes = new TextEncoder().encode(input)
    for (let i = 0; i < bytes.length; i++) {
      h ^= bytes[i]
      h = Math.imul(h, 0x01000193) >>> 0
    }
    return h >>> 0
  }

  function deriveSigil(agentId, responsibility) {
    const resp = normalizeResponsibility(responsibility)
    let id = String(agentId == null ? '' : agentId)
    if (!id) id = 'anon|' + resp
    const hId = fnv1a('avatar1|' + id)
    const hTrait = fnv1a('avatar1|' + id + '|' + resp)
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

  function isAvatarState(value) {
    return typeof value === 'string' && AVATAR_STATES.indexOf(value) >= 0
  }

  function stateRank(state) {
    return isAvatarState(state) ? STATE_PRIORITY[state] : STATE_PRIORITY.idle
  }

  function highestState(states) {
    let best = 'idle'
    for (const state of states) {
      if (stateRank(state) > stateRank(best)) best = state
    }
    return best
  }

  // 数据 → 四态：pending > deleted > running > idle（顺序即优先级）。
  function resolveAgentState(input) {
    if (!input) return 'idle'
    if (input.pending) return 'needs_attention'
    if (input.deleted) return 'offline'
    if (input.running) return 'working'
    return 'idle'
  }

  function countStates(states) {
    const out = { idle: 0, working: 0, needs_attention: 0, offline: 0, total: 0 }
    for (const state of states) {
      out[isAvatarState(state) ? state : 'idle']++
      out.total++
    }
    return out
  }

  function planStack(input, opts) {
    const states = input.map((m) => (isAvatarState(m.state) ? m.state : 'idle'))
    const counts = countStates(states)
    let state = highestState(states)
    if (opts && opts.groupRunning && state === 'idle') state = 'working'
    const visible = input.length >= 5 ? input.slice(0, 3) : input.slice()
    const members = visible.map((m, i) => ({
      id: m.id,
      responsibility: m.responsibility || '',
      identity: deriveSigil(m.id, m.responsibility || ''),
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

  return {
    SIGIL_SIZES,
    SIGIL_VIEWBOX,
    AVATAR_STATES,
    STATE_PRIORITY,
    SHAPES,
    HUES,
    TRAITS,
    normalizeResponsibility,
    fnv1a,
    deriveSigil,
    isAvatarState,
    stateRank,
    highestState,
    resolveAgentState,
    countStates,
    planStack,
  }
})
