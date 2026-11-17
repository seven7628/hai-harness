/** live 盒一页渲染多少块（「在可视范围内展示，其余用加载更早」）。
 *
 * 为什么不是全量：Working 盒只有 46vh（最高 520px），一屏看得见的就十几行；
 * 一次跑一小时的 run 可能有几百上千块，全挂进 DOM 的代价是切换时一次几百 ms
 * 的长任务（实测 800 块真实会话：全量首帧 ~0.4s longtask；120 块 ~0.15s，0 个
 * >50ms 帧）。所以首屏只铺尾部一页，更早的由盒内顶部的「加载更早」按页补。
 *
 * 为什么按块而不是按像素：块高差异很大（工具行 ~30px、正文几百 px），按像素
 * 要先测量每一行，而测量本身就是要避免的成本；按块是 O(1) 的保守上界。
 */
export const LIVE_BOX_PAGE = 120

export interface LiveWindow<T> {
  /** 这一页要渲染的尾部切片（新数组，调用方可直接交给 React）。 */
  shown: T[]
  /** 被挡在「加载更早」后面的块数；0 = 已经到头。 */
  hidden: number
}

/**
 * liveWindow 取尾部 `page + pages*page` 条。
 *
 * pages 是用户点了几次「加载更早」（0 = 首屏）。窗口永远贴着**尾部**：
 * 新块追加时窗口跟着走，不会把用户已经看到的尾部挤掉。
 */
export function liveWindow<T>(all: readonly T[], pages = 0, page = LIVE_BOX_PAGE): LiveWindow<T> {
  const take = page + Math.max(0, pages) * page
  if (all.length <= take) return { shown: all.slice(), hidden: 0 }
  return { shown: all.slice(all.length - take), hidden: all.length - take }
}
