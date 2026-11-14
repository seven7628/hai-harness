// 浏览器 UI 颜色跟随主题（P8-03a）：iOS 状态栏 / Android 地址栏吃 <meta name="theme-color">。
//
// applyTheme 只写 <html data-theme>；system 跟随是 lib/theme.ts 里的 matchMedia 回调
// 直接改属性，没有回调可挂。所以这里观察 data-theme 的变化统一更新 meta ——
// 手动切主题与系统切换都覆盖，且不改 lib/theme.ts（桌面行为零变化）。
//
// 颜色取 runtime-palette 的 --paper 原值：ink #08080a / paper #f4f3f1。
const THEME_COLOR: Record<'ink' | 'paper', string> = {
  ink: '#08080a',
  paper: '#f4f3f1',
}

export function installThemeColor(): () => void {
  const meta = document.querySelector<HTMLMetaElement>('meta[name="theme-color"]')
  if (!meta) return () => {}

  const apply = (): void => {
    const theme = document.documentElement.dataset.theme
    meta.content = THEME_COLOR[theme === 'paper' ? 'paper' : 'ink']
  }

  apply()
  if (typeof MutationObserver === 'undefined') return () => {}
  const mo = new MutationObserver(apply)
  mo.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] })
  return () => mo.disconnect()
}
