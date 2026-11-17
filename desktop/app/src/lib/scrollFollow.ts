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
  // 已经贴着底部：这个「变小」只能是高度估算/取整的结果，不是用户往上移。
  // （切会话铺开期间每帧都会重排，实测会在这里假报一次，然后在下一帧被
  //   shouldResumeFollow 撤销 —— 用户看到的是「按钮闪一下」。）
  if (a.top >= a.max - EPS) return false
  return a.top < a.expectedTop - EPS
}

/**
 * 校准帧是否应该写 scrollTop。
 *
 * **不要求「高度刚变过」**：上一版要求 `max !== prevMax` 才钉底，于是「高度已
 * 稳定、但位置是错的」永远修不回来 —— 刷新后停在半途就一直停着。用户报的
 * 「刷新后不在最底部，在某个位置停住了」正是它。
 *
 * 跟随态下「在底部」是不变式，所以只要不在底部就对齐；不跟随时一帧都不写。
 */
export function shouldPinBottom(a: { follow: boolean; top: number; max: number }): boolean {
  return a.follow && a.max > 0 && a.top !== a.max
}

/**
 * 「不跟随」状态下，是否该把跟随恢复回来（== 按钮该消失）。
 *
 * ## 为什么需要这条（用户 2026-10）
 *
 * `away`（「↓ 回到最新」按钮）由两种完全不同的原因被置位，之前**混成一个布尔**，
 * 于是第二类一旦误置就没有任何路径能撤销：
 *   ① 用户**真的**往上翻（wheel / 触摸 / 键盘 / 拖滚动条）—— 意图明确，必须尊重；
 *   ② tick 观察到「scrollTop 变小」（`isExternalMoveUp`）—— 这是个**启发式兜底**，
 *      用来接住没有 wheel 事件的手势，但它也会被**内容塌缩**骗到：一轮结束时
 *      Working / 工具组 / 折叠块自动收起，高度几帧内变矮，浏览器把 scrollTop 下调，
 *      只要有一帧读成「外部往上移」，按钮就永久挂住。用户截图里就是这个：
 *      明明停在最底部，按钮还在。
 *
 * 所以判据按**置位的原因**分开（`hardAway` = 原因①）：
 *   · 原因① → 只有用户自己把视图带回底部才恢复（这条保住既有的「上滑不被拽回」）；
 *   · 原因② → 允许用**几何**撤销：非跟随态下 tick 一帧都不写 scrollTop，位置可信；
 *   · 特例 max<=0（内容塌到不需要滚动）→ 无论哪种原因都该消失，按钮已经没有意义。
 */
export function shouldResumeFollow(a: {
  follow: boolean
  top: number
  max: number
  hardAway?: boolean
  threshold?: number
}): boolean {
  if (a.follow) return false
  if (a.max <= 0) return true
  if (a.hardAway) return false
  return isNearBottom({ top: a.top, max: a.max }, a.threshold)
}

/**
 * 一次 wheel 事件是不是被**内层滚动容器**吃掉了（因此不能当成「对话在往上翻」）。
 *
 * ## 为什么需要判（用户 2026-10：自动折叠后「回到最新」按钮行为异常）
 *
 * 对话里现在有大量可滚动的小容器：工具输出 `<pre>`、diff 体、思考折叠体、运行面板……
 * wheel 事件**会冒泡**到 `.narrative`，而上层的处理是「向上滚 → 立刻停止跟随」。
 * 于是用户只是在工具输出里往回翻两行，整个对话就被判定成「用户要离开底部」，
 * 按钮出现；此后内容一变矮，位置判据又互相矛盾 —— 表现得就像按钮坏了。
 *
 * 判据只看两个事实：内层容器在**这个方向**上还能不能滚。能滚 = 它消费了这次滚动。
 */
export function wheelConsumedByScroller(a: {
  deltaY: number
  scrollTop: number
  scrollHeight: number
  clientHeight: number
}): boolean {
  const max = a.scrollHeight - a.clientHeight
  if (max <= 0) return false
  if (a.deltaY < 0) return a.scrollTop > 0 // 往回翻：还有更早的内容
  if (a.deltaY > 0) return a.scrollTop < max - 1 // 往前翻：还没到底
  return false
}

// ── 历史铺开（「先出最近一屏，再往上续」）的起始窗口 ────────────────────────────
//
// ## 为什么这几行要抽成纯函数（2026-11）
//
// 铺开的实现方式就决定了两件事：**第一帧挂多少块**、**什么时候重新决定**。
// 两者都是纯决策，却直接决定切换会话时会不会「先窜上去再滑下来」：
//
//   ① 起始窗口必须在**渲染期**决定。上一版用 effect 决定 —— 先按上一段转录的
//      窗口把整段挂上 DOM，再砍到尾部 25 条。这一砍把 scrollHeight 从整段高度
//      缩到一屏，浏览器立刻把 scrollTop 钳到 0，随后每帧补一批、每帧重新钉底。
//      长会话里就是切换瞬间的上下窜动。
//   ② 身份必须是**转录**，不能是全局的进入序号（enterAnimAt）。后台会话的
//      new_session 响应也会 ++ 它；算进身份的话，与本段无关的进入会在这里重新
//      开始铺开 —— 在用户眼前把整段砍到 25 条再长回来（「有子智能体在跑时页面
//      自己上下抖」）。
//
// 用缺席的 -1 表示「还没决定」（冷加载：转录已切换、blocks 还没到）。
export const REVEAL_PER_FRAME = 25

export interface RevealWindow {
  /** 身份：这一段转录（换会话才变）。 */
  key: string
  /** 只渲染 blocks.slice(start)；0 = 全部显示；-1 = 等数据到位再决定。 */
  start: number
}

/** 渲染期决定这一帧的铺开窗口。同一个 key 只决定一次，之后只由 stepReveal 推进。 */
export function revealWindow(
  prev: RevealWindow,
  key: string,
  total: number,
  animate: boolean,
  perFrame = REVEAL_PER_FRAME,
): RevealWindow {
  const decide = (): RevealWindow => ({
    key,
    start: total ? (animate && total > perFrame ? total - perFrame : 0) : -1,
  })
  if (prev.key !== key) return decide()
  // 同一次进入：数据刚到（冷加载）→ 现在决定窗口
  if (prev.start < 0 && total > 0) return decide()
  return prev
}

/** 铺开推进一批（单调朝 0，永不后移 —— 流式增长不重启）。 */
export function stepReveal(start: number, perFrame = REVEAL_PER_FRAME): number {
  return Math.max(0, start - perFrame)
}

/**
 * 把铺开起点对齐到 turn 起点：返回 ≤ start 的最后一个 turn 起点；没有则 0。
 *
 * 为什么需要（2026-11 用户报障）：切片切在 turn 中间时，selectTurns 找不到
 * user 块，就用「切片首块 id」合成 turn key。每铺一批首块都变 → key 变 →
 * 整个 .rt-turn（含 LivePanel）卸载重挂；重挂后盒内 scrollTop=0，再被钉回底部。
 * 长时运行的会话尾部全在同一个 live turn 里，于是每次切换都把这一个小时
 * 的 Working 内容重挂几十遍（800 块实测 27~29 次，4.8s）。
 *
 * 起点落在 turn 边界上，切片的第一个 turn 就是完整 turn → key 稳定，
 * React 更新而不是重挂；首屏也自然把尾部 live turn 整个纳入。
 */
export function snapToTurnStart(start: number, turnStarts: readonly number[]): number {
  let lo = 0
  let hi = turnStarts.length - 1
  let best = 0
  while (lo <= hi) {
    const mid = (lo + hi) >> 1
    if (turnStarts[mid] <= start) {
      best = turnStarts[mid]
      lo = mid + 1
    } else {
      hi = mid - 1
    }
  }
  return best
}
