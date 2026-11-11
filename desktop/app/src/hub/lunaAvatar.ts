// Luna avatar 引擎（demo 原样移植）。
//
// 几何/动效来自 hai-super-assistant/docs/demo/luna-avatar-preview/：
//   mascot-bots.js  9 种眼形与头部几何
//   luna-face.js    球面投影、视线、blink/look/nod/sway/bounce/spin
// 这里只负责“给 React 一个身份色 + 状态表达式 + 一个 scene 单例”，
// SVG 本身由引擎填充，不重画几何。

import './vendor/mascot-bots.js'
import './vendor/luna-face.js'

export type LunaState = 'idle' | 'working' | 'needs_attention' | 'offline'

const PALETTE = {
  teal: '#54B9A6',
  cyan: '#38A9C9',
  blue: '#3C82F6',
  indigo: '#6464EC',
  violet: '#885CF4',
  magenta: '#B45BDC',
  rose: '#EC5F8C',
  red: '#E2554C',
  orange: '#ED712E',
  amber: '#F19D38',
  green: '#4FA968',
  slate: '#6F7C8C',
} as const

// 职责 → 固定色；没有命中才按 id 散列。颜色是身份，不随机。
const ROLE_RULES: Array<[RegExp, keyof typeof PALETTE]> = [
  [/chief|coordinator|orchestrat|主理|协调/i, 'teal'],
  [/critic|review|risk|反例|评审|风险/i, 'red'],
  [/research|talent|recruit|hiring|研究|招聘/i, 'blue'],
  [/writer|content|copy|写作|文案|稿/i, 'violet'],
  [/inbox|triage|mail|收件|分诊/i, 'indigo'],
  [/design|brand|visual|设计|视觉/i, 'magenta'],
  [/support|success|service|客服|客户成功/i, 'rose'],
  [/sales|outbound|growth|marketing|销售|增长|市场/i, 'amber'],
  [/expense|finance|billing|invoice|财务|报销|账/i, 'orange'],
  [/engineer|infra|platform|工程|基础/i, 'cyan'],
  [/ops|schedule|calendar|运营|日程/i, 'green'],
  [/legal|compliance|policy|合规|法务|政策/i, 'slate'],
]

const KEYS = Object.keys(PALETTE) as Array<keyof typeof PALETTE>

export function hashLuna(input: string): number {
  let h = 0
  for (let i = 0; i < input.length; i += 1) h = (h * 31 + input.charCodeAt(i)) >>> 0
  return h
}

export function lunaColor(id: string, name = '', responsibility = ''): string {
  const seed = `${id} ${name} ${responsibility}`
  for (const [pattern, key] of ROLE_RULES) if (pattern.test(seed)) return PALETTE[key]
  return PALETTE[KEYS[hashLuna(id) % KEYS.length]]
}

export function lunaPhase(id: string): string {
  return ((hashLuna(id) % 83) / 10).toFixed(1)
}

export function lunaExpression(state: LunaState): string {
  switch (state) {
    case 'working': return 'inspecting'
    case 'needs_attention': return 'surprised'
    case 'offline': return 'sleepy'
    default: return 'neutral'
  }
}

export function lunaSamples(px: number): number {
  return px <= 32 ? 40 : 56
}

export function lunaDamp(px: number): number {
  return px <= 32 ? 0.4 : px >= 52 ? 0.62 : 0.55
}

interface LunaScene {
  sync(root: ParentNode): void
  setBase(svg: SVGElement, name: string): void
  setExpr(svg: SVGElement, name: string): void
  poke(svg: SVGElement, action: string): void
  start(): void
  stop(): void
}

let scene: LunaScene | null = null
let activeFaces = 0
let stopTimer: ReturnType<typeof setTimeout> | null = null
let clickBound = false

type LunaFaceGlobal = {
  createScene(opts: Record<string, unknown>): LunaScene
}

function api(): LunaFaceGlobal | null {
  if (typeof window === 'undefined') return null
  return (window as unknown as { LUNA_FACE?: LunaFaceGlobal }).LUNA_FACE ?? null
}

/** 全页一个 scene：所有头像共享同一条 requestAnimationFrame / 视线监听。 */
export function ensureLunaScene(): LunaScene | null {
  if (scene) return scene
  const face = api()
  if (!face) return null
  scene = face.createScene({ aim: document.body, living: true, gap: [3200, 7000] })
  scene.start()
  if (!clickBound) {
    clickBound = true
    // 点头像 = spin（与 demo 的 document 级委托一致；React 重渲染不会重复绑）。
    document.addEventListener('click', (e) => {
      const target = e.target as Element | null
      const svg = target?.closest?.('svg[data-luna-face]') as SVGElement | null
      if (svg && scene) scene.poke(svg, 'spin')
    })
  }
  return scene
}

/** 每颗挂载的头像持有一份引用；最后一个卸载时停掉整页 rAF/gaze。 */
export function retainLunaScene(): LunaScene | null {
  if (stopTimer) { clearTimeout(stopTimer); stopTimer = null }
  const sc = ensureLunaScene()
  if (sc) activeFaces += 1
  return sc
}

export function releaseLunaScene(): void {
  activeFaces = Math.max(0, activeFaces - 1)
  if (activeFaces === 0 && scene && !stopTimer) {
    // 状态变化会 cleanup→setup 连跑；给一个短宽限，避免每颗头换表情都重建 scene。
    stopTimer = setTimeout(() => {
      stopTimer = null
      if (activeFaces === 0 && scene) {
        scene.stop()
        scene = null
      }
    }, 250)
  }
}

export function syncLunaScene(root: ParentNode | null | undefined): void {
  const sc = ensureLunaScene()
  if (!sc || !root) return
  sc.sync(root)
}
