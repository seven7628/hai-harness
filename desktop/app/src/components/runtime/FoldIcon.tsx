/* 折角图标：一个箭头画两次（右 = 折叠 / 下 = 展开），两份交叉淡入。
   设计是有意用「同一个形状转身」而不是「换一个图标」—— 两个 SVG 叠在一起，
   opacity 130ms、transform 220ms spring，旋转 ±30° 并缩到 0.86（见 port/motion.css）。
   时长与曲线全部来自令牌，别在这里写死数字。 */

// 几何与显隐必须写在组件上：简洁档的 WorkFold/LivePanel 在 current/v3 皮肤里也会
// 渲染这个图标，而 .rt-fold 的几何只存在于 runtime-skin.css。少了内联兜底时，
// 两个 SVG 会退化成浏览器默认 300×150（真机表现为正文中间两个巨大箭头）。
const svgStyle = (open: boolean, visible: boolean): React.CSSProperties => ({
  position: 'absolute',
  inset: 0,
  width: 12,
  height: 12,
  opacity: visible ? 1 : 0,
  transform: visible
    ? 'none'
    : open ? 'rotate(30deg) scale(0.86)' : 'rotate(-30deg) scale(0.86)',
  transition: 'opacity 130ms var(--ease, ease), transform 220ms var(--spring, ease)',
  pointerEvents: 'none',
})

export function FoldIcon({ open }: { open: boolean }) {
  return (
    <span
      className="rt-fold"
      aria-hidden="true"
      style={{ position: 'relative', display: 'inline-block', width: 12, height: 12, flex: 'none' }}
    >
      <svg className="rt-fi rt-fi-closed" width={12} height={12} viewBox="0 0 12 12" style={svgStyle(open, !open)}>
        <path d="M4.2 2.6 8.6 6.8l-4.4 4.2" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" />
      </svg>
      <svg className="rt-fi rt-fi-open" width={12} height={12} viewBox="0 0 12 12" style={svgStyle(open, open)}>
        <path d="M2.6 4.2 6.8 8.6l4.2-4.4" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" />
      </svg>
    </span>
  )
}
