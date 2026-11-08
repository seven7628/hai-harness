// ProviderGlyph —— Provider 字标（目录卡片 / 列表行 / 详情页共用）。
//
// 为什么不做品牌 logo：23 家 × 两套主题的颜色/商标资产维护成本高，且多数厂商的 logo
// 使用受商标条款约束。字标（1~2 字符 + 厂商色相）在列表密度下更稳：不会出现「某家没图」
// 的空洞，也不会因为深浅色切换而糊成一团。色相只用来**区分**，不做品牌还原。
//
// 色相通过 CSS 变量 --pv-h 下传，深浅两套主题各自算透明度/亮度（见 index.css .pv-glyph），
// 组件本身不感知主题。
export function ProviderGlyph({ entry, size = 32, className = '' }: {
  entry: { glyph: string; hue: number; id: string }
  size?: number
  className?: string
}) {
  const two = entry.glyph.length > 1
  return (
    <span
      className={`pvp-glyph ${className}`}
      style={{ ['--pv-h' as string]: String(entry.hue), width: size, height: size, fontSize: two ? Math.round(size * 0.34) : Math.round(size * 0.42) }}
      aria-hidden="true"
      data-glyph={entry.id}
    >
      {entry.glyph}
    </span>
  )
}
