// 会话页密度：把 settings.narrative_density 映射为 <html data-narrative-density>。
// 与 lib/theme.ts 的 applyTheme 并列——只写 data-narrative-density，不动 data-theme。
//
// 会话页**皮肤**不在这里：只有 runtime 一套，直接静态写在 index.html / mobile.html 的
// <html data-skin="runtime"> 上（选择项已移除，见 runtime-skin.css 的选择器前缀），
// 所以没有运行时可写的代码。

/** 会话密度：简洁（工作过程收进一行）/ 详细（全展开）。写 <html data-narrative-density>：
 *  轮盒自己带 data-density，但皮肤层要用它切换行高与留白，所以在 <html> 上也落一份。 */
export type NarrativeDensity = 'concise' | 'detailed'

export function applyNarrativeDensity(d: NarrativeDensity): void {
  document.documentElement.setAttribute('data-narrative-density', d === 'detailed' ? 'detailed' : 'concise')
}
