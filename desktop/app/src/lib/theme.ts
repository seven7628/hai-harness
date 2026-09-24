// 主题应用：把 theme 偏好映射为 <html data-theme>（index.css 已定义双套 palette）：
//   ink（深色，:root 默认） / paper（亮色，[data-theme='paper']）。
// 随系统（system）→ 监听 prefers-color-scheme 实时跟随。

export type ThemePref = 'dark' | 'light' | 'system'

let mq: MediaQueryList | null = null
let mqHandler: ((e: MediaQueryListEvent) => void) | null = null

function resolve(name: ThemePref): 'ink' | 'paper' {
  if (name === 'system') {
    return window.matchMedia('(prefers-color-scheme: light)').matches ? 'paper' : 'ink'
  }
  return name === 'light' ? 'paper' : 'ink'
}

export function applyTheme(pref: ThemePref): void {
  // system：挂 matchMedia 监听，系统切换实时跟随
  if (mqHandler && mq) {
    mq.removeEventListener('change', mqHandler)
    mqHandler = null
    mq = null
  }
  if (pref === 'system') {
    mq = window.matchMedia('(prefers-color-scheme: light)')
    mqHandler = () => document.documentElement.setAttribute('data-theme', resolve('system'))
    mq.addEventListener('change', mqHandler)
  }
  document.documentElement.setAttribute('data-theme', resolve(pref))
}
