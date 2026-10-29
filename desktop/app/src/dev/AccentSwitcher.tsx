import { useEffect, useState } from 'react'

/* 强调色试色条（仅 dev）—— 用户要「多弄一些颜色，我切换看看然后选一个」。
   不碰任何设计令牌文件：只是往 <html> 上写三条 inline 变量（--cinnabar 家族），
   三档皮肤都是从这里取色的（runtime: --accent: var(--cinnabar)；
   app 两档: --accent: var(--cinnabar-2)），所以切一次全站都变。

   选中的那一支记在 localStorage，刷新后还在；正式定色时把 hex 抄进
   runtime-palette.css / index.css 的基色层，然后删掉这个文件。 */

type Palette = {
  id: string
  name: string
  /** 深色底上的强调色（字/图标/实心块） */
  dark: string
  darkSoft: string
  /** 亮色底上的强调色（要压得住白底） */
  light: string
  lightSoft: string
}

const PALETTES: Palette[] = [
  { id: 'periwinkle', name: 'Periwinkle（现设计）', dark: '#8aa0ff', darkSoft: 'rgba(124,147,255,0.14)', light: '#3b5bdb', lightSoft: 'rgba(59,91,219,0.09)' },
  { id: 'blue-deep', name: 'Blue · 深', dark: '#6f82d6', darkSoft: 'rgba(111,130,214,0.16)', light: '#2f4bc0', lightSoft: 'rgba(47,75,192,0.09)' },
  { id: 'steel', name: 'Steel · 灰蓝', dark: '#9fb0c8', darkSoft: 'rgba(159,176,200,0.14)', light: '#4a6b8a', lightSoft: 'rgba(74,107,138,0.09)' },
  { id: 'teal', name: 'Teal · 青', dark: '#5fc9c2', darkSoft: 'rgba(95,201,194,0.14)', light: '#0f766e', lightSoft: 'rgba(15,118,110,0.09)' },
  { id: 'jade', name: 'Jade · 玉', dark: '#8fbf9f', darkSoft: 'rgba(143,191,159,0.14)', light: '#3f7a55', lightSoft: 'rgba(63,122,85,0.10)' },
  { id: 'amber', name: 'Amber · 琥珀', dark: '#f5b544', darkSoft: 'rgba(245,181,68,0.13)', light: '#b47500', lightSoft: 'rgba(180,117,0,0.10)' },
  { id: 'cinnabar', name: 'Cinnabar · 朱砂', dark: '#ef7a4d', darkSoft: 'rgba(239,122,77,0.14)', light: '#c2410c', lightSoft: 'rgba(194,65,12,0.10)' },
  { id: 'rose', name: 'Rose · 绯', dark: '#e8798b', darkSoft: 'rgba(232,121,139,0.14)', light: '#be2e4b', lightSoft: 'rgba(190,46,75,0.10)' },
  { id: 'violet', name: 'Violet · 紫', dark: '#b492ff', darkSoft: 'rgba(180,146,255,0.14)', light: '#6d4bd8', lightSoft: 'rgba(109,75,216,0.10)' },
]

const KEY = 'dev-accent'

function apply(p: Palette) {
  const root = document.documentElement
  const light = root.dataset.theme === 'light'
  const c = light ? p.light : p.dark
  const soft = light ? p.lightSoft : p.darkSoft
  root.style.setProperty('--cinnabar', c)
  root.style.setProperty('--cinnabar-2', c)
  root.style.setProperty('--accent-weak', soft)
  // 实心块（发送、主按钮）走的是 --accent-solid / --accent-hover，不经过
  // --cinnabar 那一族（设计里它们刻意不重映射），所以这两条也要一起换
  root.style.setProperty('--accent-solid', c)
  root.style.setProperty('--accent-hover', c)
  root.dataset.accent = p.id
  // 实心块上的字：浅色强调色（深色主题）压深字，深色强调色（亮色主题）压白字
  root.style.setProperty('--rt-accent-ink', light ? '#ffffff' : '#0a0a0c')
}

export function AccentSwitcher() {
  const [cur, setCur] = useState<string>(() => localStorage.getItem(KEY) || 'periwinkle')
  const [theme, setTheme] = useState<string>(() => document.documentElement.dataset.theme || 'dark')

  useEffect(() => {
    const p = PALETTES.find((x) => x.id === cur) || PALETTES[0]
    apply(p)
    localStorage.setItem(KEY, p.id)
  }, [cur, theme])

  // 主题切换时重新取对应那支（同一个 id 在两种主题下是两个 hex）
  useEffect(() => {
    const mo = new MutationObserver(() => setTheme(document.documentElement.dataset.theme || 'dark'))
    mo.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] })
    return () => mo.disconnect()
  }, [])

  useEffect(() => {
    // 脚本也能切：window.__accent('teal')
    ;(window as unknown as Record<string, unknown>).__accent = (id: string) => setCur(id)
  }, [])

  return (
    <div className="accent-switcher" role="group" aria-label="Accent (dev)">
      <span className="as-lab">accent</span>
      {PALETTES.map((p) => {
        const c = theme === 'light' ? p.light : p.dark
        return (
          <button
            key={p.id}
            type="button"
            className="as-dot"
            data-on={p.id === cur ? '1' : undefined}
            title={`${p.name} · ${c}`}
            aria-label={p.name}
            aria-pressed={p.id === cur}
            style={{ background: c }}
            onClick={() => setCur(p.id)}
          />
        )
      })}
      <span className="as-name">{PALETTES.find((p) => p.id === cur)?.name}</span>
    </div>
  )
}
