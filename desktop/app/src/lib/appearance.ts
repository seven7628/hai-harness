// 外观偏好（主题之外的样式项）：代码主题（亮/暗）、界面缩放、亮色背景色。
//
// 为什么落 localStorage 而不是 settings.json：settings.json 由 bridge(Go) 的结构体收口，
// 前端单方面加字段会在回写时被丢掉（settings:set 只认 Go 侧认识的字段，且要同步改 Go）。
// 这几项是**纯渲染偏好**：不参与模型/权限/会话逻辑，bridge 完全不需要知道，
// 就地持久化最省事（与 titleKey / 项目树同策略，见 useAppStore 的 loadJSON/saveJSON）。
//
// 应用方式：偏好 → <html> 上的 CSS 变量（--paper/--paper-2 覆盖）+ Electron 原生缩放。
// 副作用都在 applyAppearance() 一处，React 侧只读（useAppearance）。

import { useSyncExternalStore } from 'react'

const LS_KEY = 'go-code.appearance'

export interface Appearance {
  codeThemeLight: string // shiki 主题名（亮色）
  codeThemeDark: string // shiki 主题名（深色）
  scale: number // 界面缩放：0.9 / 1 / 1.1 / 1.25
  lightBg: string // 亮色主题的底色覆盖（默认 #ffffff = 不改）
  // 'one-light → catppuccin-latte' 那次迁移的**一次性标记**（见 normalize）。
  // 为什么需要它：迁移条件写成"值是 one-light 就改掉"时，用户**主动**选 one-light
  // 也会被同一条规则打回 latte —— 表现为「第一张卡点了没反应」（2026-09-21 用户报障）。
  // 有标记之后：老数据只迁一次，之后用户点它就被尊重。
  oneLightMigrated?: boolean
}

export const DEFAULT_APPEARANCE: Appearance = {
  // 柔和化后改用 Catppuccin Latte：它是这 6 套里唯一「暖色少（1/4）+ 平均彩度低」的粉彩主题；
  // one-light 的关键字紫、属性砖红、类型琥珀三色都很扎眼（朱砂感的主来源之一）。
  codeThemeLight: 'catppuccin-latte',
  // 2026-09-21 定稿朱砂皮肤时成对挑的深色代码主题：latte/mocha 是同一套配色的亮暗两版，
  // 与"暖白纸 + 朱砂"这套皮肤温度一致（github-dark 偏冷，落在暖黑底上像贴上去的）。
  codeThemeDark: 'catppuccin-mocha',
  scale: 1,
  lightBg: '#ffffff',
}

// 亮色代码主题候选：即 code-theme-compare.png 里那六套（用户挑图定稿的候选集）。
// 顺序有讲究：one-light 是当前默认，其余按「紫关键字 + 红属性」相似度排。
export const LIGHT_CODE_THEMES: { id: string; label: string }[] = [
  { id: 'one-light', label: 'One Light' },
  { id: 'material-theme-lighter', label: 'Material Lighter' },
  { id: 'vitesse-light', label: 'Vitesse Light' },
  { id: 'catppuccin-latte', label: 'Catppuccin Latte' },
  { id: 'github-light', label: 'GitHub Light' },
  { id: 'min-light', label: 'Min Light' },
]

// 深色侧只给成对的几个（默认 github-dark = 现状，切了也不会让深色主题走形）。
export const DARK_CODE_THEMES: { id: string; label: string }[] = [
  { id: 'github-dark', label: 'GitHub Dark' },
  { id: 'one-dark-pro', label: 'One Dark Pro' },
  { id: 'catppuccin-mocha', label: 'Catppuccin Mocha' },
  { id: 'min-dark', label: 'Min Dark' },
  { id: 'vitesse-dark', label: 'Vitesse Dark' },
]

export const SCALE_OPTIONS: { value: number; labelKey: string }[] = [
  { value: 0.9, labelKey: 'settings.appearance.scale.small' },
  { value: 1, labelKey: 'settings.appearance.scale.normal' },
  { value: 1.1, labelKey: 'settings.appearance.scale.large' },
  { value: 1.25, labelKey: 'settings.appearance.scale.xlarge' },
]

// 预设底色：中性白系（纯白 / 灰白 / 浅灰 / 冷灰）。要别的颜色走取色器。
export const BG_PRESETS = ['#ffffff', '#fafafa', '#f5f5f5', '#f1f5f9']

// 用户把底色选成深色时，文字必须整段换掉（否则黑底黑字）——取深色主题那套亮墨。
// 注意这里**只放文字**：线与其他面由下面的 BG_DERIVED_* 按"你选的底"现推
//（写死的线只对主题自带的那一支底成立，换成任意底色就失效）。
const DARK_BG_INK: Record<string, string> = {
  // 与 index.css 的 ink 主题一致（2026-09-21 朱砂定稿后同步；主题块改了这里也要改）
  '--ink': '#f0e9e4',
  '--ink-2': '#ada19a',
  '--ink-3': '#857a73',
}

// 自选底色的**派生 token**：底是你选的，但"层次"不能被一起抹平。
//
// 为什么需要（2026-09-21 用户报障「亮色下 artifacts 等边框看不出来」）：只覆盖
// --paper / --paper-2 时，页面与卡面同色、发丝线还是主题自带的那支（为近白底 #FDFCFB 调的
// #EAE5E1）——底一换成 #E8E8E8，线与底几乎同亮度（实测 232 vs 234），整片糊在一起。
//
// 做法：以**你选的底**为基准现推卡面/凹陷面/线（color-mix 在 var(--paper) 上求值，
// 所以底改了它们跟着改）。数值对齐主题自带的那套语言：
//   亮色方向：卡面比底更白、凹陷面比底更深、线比底深一档；
//   深色方向：卡面比底更亮（深色模式的分层语言）、线比底亮一档。
const BG_DERIVED_LIGHT: Record<string, string> = {
  '--paper-2': 'color-mix(in srgb, var(--paper) 72%, #fff)', // 卡面：比底更白（底越暗差别越大，自适应）
  '--paper-3': 'color-mix(in srgb, var(--paper) 94%, #000)', // 凹陷面（按下/输入/浮层底）
  '--ink-line': 'color-mix(in srgb, var(--paper) 86%, #000)', // 发丝线：在底上认得出
  '--ink-line-2': 'color-mix(in srgb, var(--paper) 74%, #000)',
  '--code-bg': 'color-mix(in srgb, var(--paper) 95%, #000)', // 代码面：比底略深
}
const BG_DERIVED_DARK: Record<string, string> = {
  '--paper-2': 'color-mix(in srgb, var(--paper) 94%, #fff)',
  '--paper-3': 'color-mix(in srgb, var(--paper) 88%, #fff)',
  '--ink-line': 'color-mix(in srgb, var(--paper) 90%, #fff)',
  '--ink-line-2': 'color-mix(in srgb, var(--paper) 82%, #fff)',
  '--code-bg': 'color-mix(in srgb, var(--paper) 68%, #000)', // 深色下代码面比底更深
}
const BG_OVERRIDE_VARS = [
  '--paper',
  ...Object.keys(DARK_BG_INK),
  ...Object.keys(BG_DERIVED_LIGHT),
]

function luminance(hex: string): number {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex.trim())
  if (!m) return 1 // 解析失败按白处理（不误切深色文字）
  const n = parseInt(m[1], 16)
  const ch = [(n >> 16) & 255, (n >> 8) & 255, n & 255].map((c) => {
    const v = c / 255
    return v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4
  })
  return 0.2126 * ch[0] + 0.7152 * ch[1] + 0.0722 * ch[2]
}

function normalize(raw: unknown): Appearance {
  const r = (raw ?? {}) as Partial<Appearance>
  const known = (list: { id: string }[]) => new Set(list.map((x) => x.id))
  const light = known(LIGHT_CODE_THEMES)
  const dark = known(DARK_CODE_THEMES)
  // 迁移：'one-light' 是上一版的内置默认（不是用户挑的），**第一次加载**时跟着新默认一起柔和掉。
  // 为什么带 oneLightMigrated 这个标记：条件只写"值是 one-light 就迁"会把用户**主动**的选择
  // 一起迁走 —— 用户看到的就是"One Light 这张卡点了没反应"（2026-09-21 报障）。带上标记后，
  // 迁移只发生一次，之后 one-light 是合法值。
  const migrateOneLight = r.codeThemeLight === 'one-light' && r.oneLightMigrated !== true
  return {
    codeThemeLight: typeof r.codeThemeLight === 'string' && light.has(r.codeThemeLight) && !migrateOneLight
      ? r.codeThemeLight
      : DEFAULT_APPEARANCE.codeThemeLight,
    codeThemeDark: typeof r.codeThemeDark === 'string' && dark.has(r.codeThemeDark) ? r.codeThemeDark : DEFAULT_APPEARANCE.codeThemeDark,
    scale: typeof r.scale === 'number' && SCALE_OPTIONS.some((o) => o.value === r.scale) ? r.scale : 1,
    lightBg: typeof r.lightBg === 'string' && /^#[0-9a-f]{6}$/i.test(r.lightBg) ? r.lightBg.toLowerCase() : DEFAULT_APPEARANCE.lightBg,
    // 归一化后的对象一律带标记：任何一次写入（setAppearance）都会把它落盘，
    // 于是"这条数据已经迁过了"这件事有了持久记录。老数据没这个字段 → 迁一次 → 下次不再迁。
    oneLightMigrated: true,
  }
}

let current: Appearance = (() => {
  try {
    const raw = localStorage.getItem(LS_KEY)
    return normalize(raw ? JSON.parse(raw) : null)
  } catch {
    return DEFAULT_APPEARANCE
  }
})()

// 模块一加载就清一次历史残留的 inline zoom：早期实现把缩放写成 documentElement.style.zoom，
// inline style 不会被 HMR 替换掉。放模块顶层是因为 HMR 场景下 App 的 effect 不保证重跑
//（只靠 applyAppearance 会漏，用户就会一直看到"放大了但溢出/被裁"的旧状态）。
if (typeof document !== 'undefined') document.documentElement.style.removeProperty('zoom')

const listeners = new Set<() => void>()

export function getAppearance(): Appearance {
  return current
}

// 代码主题名（按解析后的主题取亮/暗那一支）。highlight.ts 用它决定 shiki 主题。
export function codeThemeOf(resolved: 'ink' | 'paper'): string {
  return resolved === 'paper' ? current.codeThemeLight : current.codeThemeDark
}

export function subscribeAppearance(fn: () => void): () => void {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

export function setAppearance(patch: Partial<Appearance>): void {
  current = normalize({ ...current, ...patch })
  try {
    localStorage.setItem(LS_KEY, JSON.stringify(current))
  } catch {
    /* 配额/隐私模式：忽略，持久化非致命 */
  }
  applyAppearance()
  for (const fn of listeners) fn()
}

// applyAppearance 把偏好写进 <html>。唯一有副作用的地方，读值走 getAppearance()。
export function applyAppearance(pref: Appearance = current): void {
  const root = document.documentElement
  // 清掉历史版本留在 <html> 上的 inline zoom：早期实现把缩放写成 documentElement.style.zoom，
  // 而 inline style **不会**被 HMR/重建代码清掉 —— 升级后旧值还在，页面依然是"放大了但溢出"的样子
  //（用户报的「字体调大后样式全异常」有一部分就是这种残留态）。放在最前面，无条件执行。
  root.style.removeProperty('zoom')
  // —— 亮色底色覆盖 ——
  // inline 变量优先级高于 [data-theme='paper'] 规则，所以在深色主题下**必须清掉**，
  // 否则会把深色的底也刷成浅色（applyTheme 切主题后会再次调用本函数，届时自然清/挂）。
  const isPaper = root.getAttribute('data-theme') === 'paper'
  const custom = isPaper && pref.lightBg.toLowerCase() !== DEFAULT_APPEARANCE.lightBg
  if (custom) {
    // 底：用户选的那个（只覆盖 --paper，卡面/线/凹陷面由下面的派生表推）
    root.style.setProperty('--paper', pref.lightBg)
    const deep = luminance(pref.lightBg) < 0.45
    // 派生面与线：**必须跟着一起挂**，否则卡面与底同色 + 线是主题自带的那支
    //（为近白底调的）→ 灰底上卡片边界整片消失（用户报障「artifacts 边框看不出来」）。
    for (const [k, v] of Object.entries(deep ? BG_DERIVED_DARK : BG_DERIVED_LIGHT)) {
      root.style.setProperty(k, v)
    }
    // 文字：深底才换（浅底继续用主题的深墨）
    for (const [k, v] of Object.entries(DARK_BG_INK)) {
      if (deep) root.style.setProperty(k, v)
      else root.style.removeProperty(k)
    }
  } else {
    for (const v of BG_OVERRIDE_VARS) root.style.removeProperty(v)
  }
  // —— 界面缩放 ——
  // Electron 下走**原生缩放**（等于 Ctrl +/-）：vh / position:fixed / 媒体查询一并跟着变。
  //
  // 为什么 must catch + 失败退回 CSS：main 是长生命周期进程，preload 却会在每次刷新时从磁盘重载 ——
  // 开发中极易出现「preload 已新版（有 setUiZoom）、main 还是旧产物（没注册 ui:set-zoom）」的错配，
  // 此时 invoke 会 reject（`No handler registered for 'ui:set-zoom'`）：既在控制台冒未捕获错误，
  // 缩放也整个失效（用户 2026-09-21 报的就是这个）。退回 CSS 路径至少让设置可用。
  if (window.desktop?.setUiZoom) {
    root.style.setProperty('--ui-scale', '1') // 原生缩放生效时 CSS 退化路径必须关掉（否则双重缩放）
    window.desktop.setUiZoom(pref.scale)
      .then((r) => { if (!r?.ok) root.style.setProperty('--ui-scale', String(pref.scale)) })
      .catch(() => root.style.setProperty('--ui-scale', String(pref.scale)))
  } else {
    // 浏览器（npm run dev 的 vite / e2e 探针）没有 desktop 桥：退化为 CSS zoom，
    // 但**只挂 #root 且做尺寸补偿**（见 index.css）——
    // 教训：早先这里写成 documentElement.style.zoom，用户「字体调大后页面样式全乱」就是它：
    // CSS zoom 不作用于 vh 单位（实测 zoom 1.25 下 100vh 仍等于真实视口高），
    // 挂在 html 上会让 height:100vh 的应用根直接顶出可视区、fixed 浮层错位。
    root.style.setProperty('--ui-scale', String(pref.scale))
  }
}

// useAppearance：React 侧只读订阅（写入一律走 setAppearance）。
// useSyncExternalStore 而非 useState + effect：拿到的永远是当前快照，
// 不会出现「订阅前已变更 → 首帧读到旧值」的撕裂（设置页里切主题时最容易踩）。
export function useAppearance(): Appearance {
  return useSyncExternalStore(subscribeAppearance, getAppearance, getAppearance)
}
