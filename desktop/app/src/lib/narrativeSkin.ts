// 会话页样式应用：把 settings.narrative_skin 映射为 <html data-skin>：
//   current（默认，:root 上的现状样式） / v3（[data-skin='v3'] 前缀的组件样式）。
// 与 lib/theme.ts 的 applyTheme 并列——只写 data-skin，不动 data-theme，两套开关正交。

export type NarrativeSkin = 'current' | 'v3'

/** 会话页样式：写 <html data-skin>，样式规则见 src/narrative-skin.css（[data-skin='v3'] 前缀） */
export function applyNarrativeSkin(skin: NarrativeSkin): void {
  document.documentElement.setAttribute('data-skin', skin === 'v3' ? 'v3' : 'current')
}
