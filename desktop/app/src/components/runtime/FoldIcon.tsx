/* 折角图标：一个箭头画两次（右 = 折叠 / 下 = 展开），两份交叉淡入。
   设计是有意用「同一个形状转身」而不是「换一个图标」—— 两个 SVG 叠在一起，
   opacity 130ms、transform 220ms spring，旋转 ±30° 并缩到 0.86（见 port/motion.css）。
   时长与曲线全部来自令牌，别在这里写死数字。 */
export function FoldIcon({ open }: { open: boolean }) {
  return (
    <span className="rt-fold" aria-hidden="true">
      <svg className="rt-fi rt-fi-closed" viewBox="0 0 12 12">
        <path d="M4.2 2.6 8.6 6.8l-4.4 4.2" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" />
      </svg>
      <svg className="rt-fi rt-fi-open" viewBox="0 0 12 12">
        <path d="M2.6 4.2 6.8 8.6l4.2-4.4" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" />
      </svg>
    </span>
  )
}
