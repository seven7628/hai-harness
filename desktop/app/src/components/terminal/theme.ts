// [recovered] from /private/tmp/builtin-surfaces.test.cjs（模块标记 `// src/components/terminal/theme.ts`）
// [implemented] 依据 docs/TERMINAL_VISUAL_SPEC.md §3（终端配色取自设计令牌，不硬编码色值）：
//   切片里三个函数都没有 export、类型全丢。本文件补导出名（TerminalPane.tsx 的 import：
//   readTermTheme / watchTheme）与类型标注，**逻辑逐字未改**；移除 @ts-nocheck。
//
// 为什么不写死色值：终端是右栏里唯一「不是 React 画出来的」区域，主题切换（data-theme）
// 时它不会自己重绘 —— 所以颜色必须每次从 CSS 变量现读，并由 watchTheme 在切换时重读一次。
import type { ITheme } from '@xterm/xterm'

function readVar(name: string): string {
  // 非浏览器环境（node 单测 / 预渲染）没有 document：给空串，让 pick 继续找下一个候选。
  if (typeof window === 'undefined') return ''
  const v = getComputedStyle(document.documentElement).getPropertyValue(name)
  return v.trim()
}

function pick(...names: string[]): string | undefined {
  for (const n of names) {
    const v = readVar(n)
    if (v) return v
  }
  return undefined
}

/** 当前主题下的 xterm 配色（每次调用现读 CSS 变量）。 */
export function readTermTheme(): ITheme {
  const bg = pick('--code-block-bg', '--paper-3', '--paper')
  const fg = pick('--ink')
  const bright = pick('--ink')
  const dim = pick('--ink-3')
  return {
    background: bg,
    foreground: fg,
    cursor: pick('--cinnabar', '--cinnabar-2'),
    cursorAccent: bg,
    selectionBackground: pick('--accent-weak') || 'rgba(217, 122, 102, 0.28)',
    black: pick('--paper-3', '--ink-line-2'),
    red: pick('--crimson'),
    green: pick('--bamboo'),
    yellow: pick('--ochre'),
    blue: pick('--indigo'),
    magenta: pick('--cat-write'),
    cyan: pick('--cat-cmd'),
    white: pick('--ink-2'),
    brightBlack: dim,
    brightRed: pick('--crimson'),
    brightGreen: pick('--bamboo'),
    brightYellow: pick('--ochre'),
    brightBlue: pick('--indigo'),
    brightMagenta: pick('--cat-write'),
    brightCyan: pick('--cat-cmd'),
    brightWhite: bright,
  }
}

/**
 * 订阅主题切换（`<html data-theme>` 的 attribute 变化）。返回取消订阅函数。
 * 非浏览器环境给一个空 disposer：调用方不必到处判 `typeof MutationObserver`。
 */
export function watchTheme(onChange: () => void): () => void {
  if (typeof MutationObserver === 'undefined') return () => {}
  const mo = new MutationObserver(onChange)
  mo.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] })
  return () => mo.disconnect()
}
