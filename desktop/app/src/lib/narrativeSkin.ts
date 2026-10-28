// 会话页样式应用：把 settings.narrative_skin 映射为 <html data-skin>：
//   current（默认，:root 上的现状样式） / v3（[data-skin='v3'] 前缀的组件样式） /
//   runtime（[data-skin='runtime'] 前缀，v2 运行时设计移植，见 runtime-skin.css）。
// 与 lib/theme.ts 的 applyTheme 并列——只写 data-skin，不动 data-theme，两套开关正交。

export type NarrativeSkin = 'current' | 'v3' | 'runtime'

/** 会话页样式：写 <html data-skin>，样式规则见 src/narrative-skin.css（[data-skin='v3'] 前缀） */
export function applyNarrativeSkin(skin: NarrativeSkin): void {
  const known = skin === 'v3' || skin === 'runtime' ? skin : 'current'
  document.documentElement.setAttribute('data-skin', known)
}

/** 会话密度：简洁（工作过程收进一行）/ 详细（全展开）。写 <html data-narrative-density>：
 *  轮盒自己带 data-density，但皮肤层要用它切换行高与留白，所以在 <html> 上也落一份。 */
export type NarrativeDensity = 'concise' | 'detailed'

export function applyNarrativeDensity(d: NarrativeDensity): void {
  document.documentElement.setAttribute('data-narrative-density', d === 'detailed' ? 'detailed' : 'concise')
}
