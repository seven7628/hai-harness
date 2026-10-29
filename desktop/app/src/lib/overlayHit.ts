// 「右栏被 DOM 覆盖层压住」的判定 —— 原生预览视图（WebContentsView）恒在窗口 DOM 之上，
// CSS z-index 压不住它，所以只能反过来：DOM 里出现了压在右栏上的东西，就把原生视图隐藏。
//
// 名单（.modal-hint / .img-zoom / .rt-lb …）是手维护的，新增弹窗必漏 —— 用户 2026-10 报的
// 「先开右栏 HTML 预览、再点开聊天里的大图，大图整个躲在预览画面后面」就是漏掉的那一类。
// 这里换成问 DOM 本身：右栏矩形**中点**那一叠元素里，只要有一个是**不属于右栏的 fixed 元素**，
// 就是有覆盖层压在它上面。
//
// 为什么限定 fixed：右栏自己那块 DOM 通常是占位（pointer-events:none 的容器），用
// 「中点上的元素不在右栏里」会把它误判成覆盖层，于是预览刚显示就自己躲起来。全屏弹窗/灯箱
// 一律 position:fixed，这条限定既够用又不会误伤。

export type Rect = { left: number; top: number; right: number; bottom: number }
export type Point = { x: number; y: number }
export type Viewport = { width: number; height: number }

/** 矩形中点；尺寸无效或中点在视口外 → null（宁可少隐藏一次，也不要因为量不到就来回闪）。 */
export function centerOf(panel: Rect | undefined, viewport?: Viewport): Point | null {
  if (!panel) return null
  const w = panel.right - panel.left
  const h = panel.bottom - panel.top
  if (w < 1 || h < 1) return null
  const x = Math.round(panel.left + w / 2)
  const y = Math.round(panel.top + h / 2)
  if (viewport && (x < 0 || y < 0 || x > viewport.width || y > viewport.height)) return null
  return { x, y }
}

/**
 * 中点那一叠元素里是否压着覆盖层。
 * `stack` 是 elementsFromPoint 的结果（从最顶层开始）；`positionOf` 给出计算后的 position。
 */
export function overlayCoversRect(panel: Rect | undefined, stack: unknown[], opts: { contains: (el: unknown) => boolean; positionOf: (el: unknown) => string; viewport?: Viewport }): boolean {
  if (!centerOf(panel, opts.viewport)) return false
  return stack.some((el) => !!el && !opts.contains(el) && opts.positionOf(el) === 'fixed')
}

/** 两个矩形是否相交（任一方为空 → 视为相交：名单兜底时不该否决隐藏）。 */
export function rectsIntersect(a: Rect | undefined, b: Rect | undefined): boolean {
  if (!a || !b) return true
  return a.left < b.right && a.right > b.left && a.top < b.bottom && a.bottom > b.top
}
