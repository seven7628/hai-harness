// embedRectOf：把一个 DOM 容器量成「给原生视图用的矩形」；**量不出来返回 undefined**。
//
// R08b 从 components/WebTab.tsx 提到这里：pdf 预览面板也要驱动同一个原生视图（它不在 DOM 里，
// 只能按矩形浮在容器上）。同一份量测写两遍，迟早有一份忘了下面这些坑。
//
// 为什么不返回一个钳过的最小矩形（2026-09-21 用户报障的根因）：
//   收起右栏时面板卸载，而 ResizeObserver 对**已脱离文档**的元素还会补发一次「尺寸 0」的
//   通知 → 量到 0×0。旧实现在这里钳成 `Math.max(80, …)`，于是这次「量不到」被当成
//   「容器只有 80×80」下发：`embed:resize` 把**所有**原生视图缩到 80×80，而
//   `Emulation.setDeviceMetricsOverride` 更狠 —— 它把面板里**正显示的那一页**的排版视口
//   钉死在 80×80（页面按 80px 宽排版：内容只剩左上角一小块、页面底色铺满整块面板）。
//   而且它**不会自愈**：同尺寸下切换标签不重挂载组件、ResizeObserver 也不再触发。
//   正确的处置是「量不到就什么都不下发」：隐藏路径本就有 embedHide 兜底；主进程对 show 收到
//   非法/缺失 rect 时也有右侧栏默认区域兜底（见 electron/embed.ts 的 isValidEmbedRect）。
export interface EmbedRect {
  x: number
  y: number
  width: number
  height: number
}

export function embedRectOf(el: Element | null): EmbedRect | undefined {
  if (!el || !el.isConnected) return undefined
  const r = el.getBoundingClientRect()
  const width = Math.round(r.width)
  const height = Math.round(r.height)
  if (width < 1 || height < 1) return undefined
  return { x: Math.round(r.x), y: Math.round(r.y), width, height }
}
