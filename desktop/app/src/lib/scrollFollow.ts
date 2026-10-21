// scrollFollow —— 对话滚动「是否跟随最新内容」的判定规则（纯函数，无 DOM）。
//
// ## 为什么单独抽出来
//
// 这块逻辑被同一类 bug 反复击穿（用户报障 4 次），每次成因都不同：
//   ① 判据放在 effect 闭包里 → 流式每帧重启 effect，基准被重置，判据永不触发；
//   ② 手势结束后用「距底部多远」重新评估 → tick 已把位置钉回底部，
//      于是刚置的「不跟随」在同一函数末尾被撤销（按钮闪一下就消失）；
//   ③ 切会话清空 blocks → 容器塌缩、scrollTop 被浏览器钳到 0，
//      这个「变小」被当成用户上滑 → 跟随被永久关闭（新会话再也不贴底）。
//
// 三条规则的共同点：它们都是**纯判定**，却和 rAF / 事件时序纠缠在一起难以复现。
// 抽成纯函数后可以用普通单测把每条不变量钉死（见 scrollFollow-test.ts）。

/** 视为「贴底」的容差（px）。content-visibility 的高度是估算值，需要留余量。 */
export const FOLLOW_THRESHOLD = 120

/** 亚像素/取整误差容差。 */
const EPS = 2

/** 当前位置是否离底部足够近（仅用于「用户自己滚到底了吗」这一问）。 */
export function isNearBottom(a: { top: number; max: number }, threshold = FOLLOW_THRESHOLD): boolean {
  return a.max - a.top < threshold
}

/**
 * 一次 scroll 事件是否是「用户往更早的内容滚」。
 *
 * 程序性钉底只会让 top 增大（趋向 max），所以「变小」通常源于外部；但有两种
 * **非用户**的变小必须排除，否则跟随会被误关：
 *   ① 内容塌缩导致的钳制：切会话/清空 blocks 使容器变矮，浏览器把 scrollTop
 *      从上一会话底部钳到 0（实测这一步会让新会话永久不跟随）；
 *   ② 已经贴着当前底部：top 变小只是高度变化的结果，用户并没有离开底部。
 */
export function isUserScrollUp(a: { top: number; prevTop: number; max: number; prevMax: number }): boolean {
  // ① 内容塌缩 → 钳制，不是用户
  if (a.prevMax >= 0 && a.max < a.prevMax - EPS) return false
  if (!(a.top < a.prevTop - EPS)) return false
  // ② 已贴底 → 高度变化导致的位移
  if (a.top >= a.max - EPS) return false
  return true
}

/**
 * 手势结束后的跟随决定。
 *
 * 方向优先于位置：用户刚往上滚 → 一律不跟随，**不要**再用 isNearBottom 复核。
 * 那一步会把 tick 自己钉出来的位置当成「用户在底部」，从而撤销刚做出的决定。
 * 只有当手势方向不是「上」（向下滚 / 拖滚动条 / 未知）时，才按位置判断。
 */
export function decideFollow(a: {
  dir: 'up' | 'down' | null
  top: number
  max: number
  threshold?: number
}): boolean {
  if (a.dir === 'up') return false
  return isNearBottom({ top: a.top, max: a.max }, a.threshold)
}

/**
 * 校准帧里判断「视图是否被外部往上移了」。
 *
 * 依赖单写口径：expectedTop 是 tick 上一次**自己**写入的值（必须是跨 effect
 * 重跑存活的 ref，不能是闭包变量）。只判「变小」：tick 自己只会往 max 推。
 * 同时排除高度收缩引起的钳制（内容变矮时浏览器会下调 scrollTop）。
 */
export function isExternalMoveUp(a: {
  top: number
  expectedTop: number
  max: number
  prevMax: number
}): boolean {
  if (a.expectedTop < 0) return false
  if (a.max < a.prevMax) return false
  return a.top < a.expectedTop - EPS
}
