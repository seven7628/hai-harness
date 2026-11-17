// scrollFollow-test.ts —— 「是否跟随最新内容」判定的单测（node:test，无 DOM）。
//
// 为什么钉这些：同一类 bug 被用户报障 4 次，每次修好又换个形态复发。三个成因
// 都是「判定本身对，但被时序/位置信息污染成相反结论」，看代码几乎看不出来：
//
//   ① 方向判据被位置判据撤销（settle 里再用 isNearBottom 复核一次）；
//   ② 内容塌缩导致的 scrollTop 钳制被当成「用户上滑」；
//   ③ 基准（expectedTop）在 effect 重跑时被重置，外部改动判据永不触发。
//
// 下面的断言就是这三条的可执行规格。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { REVEAL_PER_FRAME, decideFollow, isExternalMoveUp, isNearBottom, isUserScrollUp, revealWindow, shouldPinBottom, shouldResumeFollow, snapToTurnStart, stepReveal, wheelConsumedByScroller, FOLLOW_THRESHOLD } from './scrollFollow'

// 便捷构造：容器高 500、内容高 3000 → max = 2500
const MAX = 2500
const atBottom = { top: MAX, max: MAX }
const scrolledUp = { top: MAX - 400, max: MAX }

test('贴底判定：容差内算贴底，超过即离开', () => {
  assert.equal(isNearBottom(atBottom), true)
  assert.equal(isNearBottom({ top: MAX - FOLLOW_THRESHOLD + 1, max: MAX }), true)
  assert.equal(isNearBottom({ top: MAX - FOLLOW_THRESHOLD - 1, max: MAX }), false)
})

test('① 手势方向优先：上滑后即使位置仍在阈值内也不跟随', () => {
  // 这是「按钮闪一下就消失、视图继续被拽回」的直接成因：
  // 用户在阈值内上滑 50px，位置判定会说是「在底部」，跟随被重新打开。
  const inThreshold = { top: MAX - 50, max: MAX }
  assert.equal(isNearBottom(inThreshold), true, '位置判据确实认为在底部')
  assert.equal(decideFollow({ dir: 'up', ...inThreshold }), false, '方向是 up → 绝不跟随')
  assert.equal(decideFollow({ dir: 'down', ...inThreshold }), true, '方向是 down 且在阈值内 → 跟随')
  assert.equal(decideFollow({ dir: null, ...inThreshold }), true, '无方向信息（拖滚动条）→ 按位置判断')
})

test('① 上滑后离开阈值：无论方向都保持不跟随', () => {
  assert.equal(decideFollow({ dir: 'up', ...scrolledUp }), false)
  assert.equal(decideFollow({ dir: 'down', ...scrolledUp }), false, '往下滚了但还没到底 → 仍不跟')
  assert.equal(decideFollow({ dir: null, ...scrolledUp }), false)
})

test('② 内容塌缩导致的钳制不算用户上滑', () => {
  // 切会话：blocks 清空 → max 从 2500 塌到 0，scrollTop 被钳到 0
  const collapsed = { top: 0, prevTop: 2500, max: 0, prevMax: 2500 }
  assert.equal(isUserScrollUp(collapsed), false, '高度收缩引起的变小必须排除')
})

test('② 真上滑要能识别（高度不变 / 高度增长两种）', () => {
  assert.equal(isUserScrollUp({ top: 2100, prevTop: 2500, max: MAX, prevMax: MAX }), true)
  // 流式中内容持续变高：用户上滑同样是上滑
  assert.equal(isUserScrollUp({ top: 2100, prevTop: 2500, max: 2700, prevMax: 2500 }), true)
})

test('② 已经贴着当前底部时的变小不算上滑', () => {
  // top 与 max 一起变小（高度收缩但仍贴底）→ 用户没离开底部
  assert.equal(isUserScrollUp({ top: 2400, prevTop: 2500, max: 2401, prevMax: MAX }), false)
})

test('② 微小抖动（<2px）不算上滑', () => {
  assert.equal(isUserScrollUp({ top: 2499, prevTop: 2500, max: MAX, prevMax: MAX }), false)
})

test('③ 外部把视图往上移 → 校准帧必须停跟随', () => {
  assert.equal(isExternalMoveUp({ top: 2100, expectedTop: 2500, max: MAX, prevMax: MAX }), true)
})

test('③ 视图被往上推但高度同时收缩 → 是钳制，不判定为外部移动', () => {
  assert.equal(isExternalMoveUp({ top: 2100, expectedTop: 2500, max: 2100, prevMax: MAX }), false)
})

test('③ 没有基准（-1）时不得判定为外部移动', () => {
  assert.equal(isExternalMoveUp({ top: 0, expectedTop: -1, max: MAX, prevMax: MAX }), false)
})

test('③ tick 自己往底部推（top 增大）不算外部移动', () => {
  assert.equal(isExternalMoveUp({ top: 2500, expectedTop: 2100, max: MAX, prevMax: MAX }), false)
})

test('③ 贴底时的「变小」是高度估算的结果，不能判成外部位移', () => {
  // 切会话铺开期间每帧重排：scrollTop 被钳到新的 max，而 expectedTop 还是上一帧的值。
  // 不排除这一种就会「按钮闪一下又消失」（用户 2026-10 / 2026-11 两次报障）。
  assert.equal(isExternalMoveUp({ top: 2100, expectedTop: 2500, max: 2100, prevMax: 2000 }), false)
})

test('③ 亚像素误差不该关闭跟随', () => {
  assert.equal(isExternalMoveUp({ top: 2499, expectedTop: 2500, max: MAX, prevMax: MAX }), false)
})

test('组合：完整复现「上滑后流式继续」这条路径', () => {
  // 用户上滑 400px
  let top = MAX - 400
  const dir: 'up' | 'down' = 'up'
  let follow = decideFollow({ dir, top, max: MAX })
  assert.equal(follow, false, '手势结束即停止跟随')
  // 流式追加 3 帧：内容变高、tick 不应把视图推回底部
  let max = MAX
  let expectedTop = top
  for (let i = 0; i < 3; i++) {
    max += 150
    const external = isExternalMoveUp({ top, expectedTop, max, prevMax: max - 150 })
    assert.equal(external, false, '用户没有再次滚动 → 不该有外部移动')
    if (follow) top = max // 只有 follow 才钉底
    expectedTop = top
    assert.equal(top, MAX - 400, `第 ${i} 帧后位置必须原封不动`)
  }
})

test('④ 跟随态：高度没变但位置不对，也必须对齐底部（刷新后停在半途的成因）', () => {
  // 上一版要求「高度刚变过」才钉底 → 进入后高度稳定但位置错时就永远不修
  assert.equal(shouldPinBottom({ follow: true, top: 1000, max: 2500 }), true)
  // 已经在底部 → 不写（避免每帧无意义的写入）
  assert.equal(shouldPinBottom({ follow: true, top: 2500, max: 2500 }), false)
})

test('④ 不跟随时一帧都不写（用户在看历史）', () => {
  assert.equal(shouldPinBottom({ follow: false, top: 0, max: 2500 }), false)
  assert.equal(shouldPinBottom({ follow: false, top: 2500, max: 2500 }), false)
})

test('④ 空内容（max=0）不写，避免把 0 当成底部', () => {
  assert.equal(shouldPinBottom({ follow: true, top: 0, max: 0 }), false)
})

// —— ⑤ 自动折叠导致的「按钮挂住」（用户 2026-10 报障） ————————————————
//
// 现场：一轮结束时 Working / 工具组 / 折叠块自动收起 → 内容几帧内变矮 → 浏览器下调
// scrollTop。只要有一帧被 isExternalMoveUp 读成「外部往上移」，away 就永久为 true，
// 于是明明停在最底部，按钮还挂着。修法不是把那个启发式删掉（它接住的是没有 wheel
// 事件的滚动条拖动/键盘翻页），而是把 away 的**原因**分开，并让非用户原因的 away
// 可以被几何撤销。

test('⑤ 塌缩误判（非用户原因）在贴底后自动撤销：按钮不该挂住', () => {
  assert.equal(
    shouldResumeFollow({ follow: false, top: MAX, max: MAX, hardAway: false }),
    true,
    '已经贴底且不是用户亲手翻的 → 恢复跟随（按钮消失）',
  )
})

test('⑤ 用户亲手翻上去的，即使位置还在阈值内也不撤销（保住 bug ①）', () => {
  assert.equal(
    shouldResumeFollow({ follow: false, top: MAX - 50, max: MAX, hardAway: true }),
    false,
    '用户意图优先：阈值内上滑也不许被自动翻案',
  )
})

test('⑤ 内容塌到不需要滚动（max=0）：任何原因都恢复，按钮已无意义', () => {
  assert.equal(shouldResumeFollow({ follow: false, top: 0, max: 0, hardAway: true }), true)
})

test('⑤ 真离开了底部（非用户原因也不该撤销）', () => {
  assert.equal(
    shouldResumeFollow({ follow: false, top: MAX - 800, max: MAX, hardAway: false }),
    false,
  )
})

test('⑤ 跟随态下不重复恢复（幂等，不产生多余 setState）', () => {
  assert.equal(shouldResumeFollow({ follow: true, top: MAX, max: MAX }), false)
})

// —— ⑥ wheel 冒泡：内层滚动容器吃掉的滚动不算「翻这个对话」 ————————————
test('⑥ 工具输出里往回翻：内层还能滚 → 由它消费，不动对话的跟随状态', () => {
  assert.equal(
    wheelConsumedByScroller({ deltaY: -100, scrollTop: 200, scrollHeight: 1000, clientHeight: 300 }),
    true,
  )
})

test('⑥ 内层已经到顶还继续往回滚：交给对话（用户确实想往上）', () => {
  assert.equal(
    wheelConsumedByScroller({ deltaY: -100, scrollTop: 0, scrollHeight: 1000, clientHeight: 300 }),
    false,
  )
})

test('⑥ 内层不可滚（内容比容器矮）→ 不消费', () => {
  assert.equal(
    wheelConsumedByScroller({ deltaY: -100, scrollTop: 0, scrollHeight: 200, clientHeight: 300 }),
    false,
  )
})

test('⑥ 往前翻：内层还没到底 → 消费；到底了 → 不消费', () => {
  assert.equal(
    wheelConsumedByScroller({ deltaY: 100, scrollTop: 100, scrollHeight: 1000, clientHeight: 300 }),
    true,
  )
  assert.equal(
    wheelConsumedByScroller({ deltaY: 100, scrollTop: 700, scrollHeight: 1000, clientHeight: 300 }),
    false,
  )
})


// ── 历史铺开的起始窗口（2026-11 切会话抖动）────────────────────────────────
//
// 用户报障：切会话时页面「先上滑再下滑」、最新位置定位不准。根因是起始窗口
// 由 effect 决定 —— 先把整段挂上 DOM，再砍到尾部 25 条：scrollHeight 从整段
// 高度塌到一屏，浏览器钳 scrollTop，随后每帧补一批、每帧重新钉底。
// 下面的规格钉住三件事：不先全挂、不跨会话串、不因无关的全局进入而重启。

test('铺开：新的转录在第一帧就只挂尾部窗口（不是先全挂再砍）', () => {
  const w = revealWindow({ key: '', start: 0 }, 'A', 200, true)
  assert.equal(w.start, 200 - REVEAL_PER_FRAME, '第一帧就是尾部 25 条，没有全挂的中间态')
  assert.equal(w.key, 'A')
})

test('铺开：短会话（≤25 条）不铺，全部显示', () => {
  assert.equal(revealWindow({ key: '', start: 0 }, 'A', 12, true).start, 0)
})

test('铺开：同一段转录只决定一次（流式增长不重启铺开）', () => {
  const first = revealWindow({ key: '', start: 0 }, 'A', 200, true)
  const stepped = { ...first, start: stepReveal(first.start) } // 175 → 150，铺到一半
  assert.deepEqual(revealWindow(stepped, 'A', 260, true), stepped, '新块到达不把进度打回起点')
})

test('铺开：换转录 → 重新决定，且**不会**沿用上一段的下标', () => {
  // 上一段（200 块）铺到 start=100，此时切到只有 30 块的 B：
  // 沿用旧下标 = blocks.slice(100) 为空 = 白屏一帧。
  const midA = { key: 'A', start: 100 }
  assert.equal(revealWindow(midA, 'B', 30, true).start, 30 - REVEAL_PER_FRAME)
  // 切到只有 10 块的 C：全部显示（不能用 100 去切）
  assert.equal(revealWindow(midA, 'C', 10, true).start, 0)
})

test('铺开：冷加载（转录已切换、块还没到）→ 数据到了再决定', () => {
  const empty = revealWindow({ key: '', start: 0 }, 'A', 0, true)
  assert.equal(empty.start, -1, '-1 = 等数据，别把「还没到」当成「全部显示」')
  assert.equal(revealWindow(empty, 'A', 200, true).start, 200 - REVEAL_PER_FRAME)
})

test('铺开：铺开推进单调朝 0，不会后移', () => {
  assert.equal(stepReveal(60, 25), 35)
  assert.equal(stepReveal(10, 25), 0)
  assert.equal(stepReveal(0, 25), 0)
})


// —— 铺开只能从 turn 边界切（2026-11「Working 盒回放」的回归规格）——
// 切在 turn 中间 → selectTurns 的合成 key 换成切片首块 id → 整个 Working 盒重挂。
test('snapToTurnStart：空边界 = 0（没有 user 块就整段一次渲染）', () => {
  assert.equal(snapToTurnStart(775, []), 0)
})

test('snapToTurnStart：落在两个 turn 之间取较小的起点', () => {
  assert.equal(snapToTurnStart(775, [0, 498, 508]), 508, '尾部 turn 从 508 开始')
  assert.equal(snapToTurnStart(507, [0, 498, 508]), 498, '不能取到 508（越界）')
  assert.equal(snapToTurnStart(0, [0, 498, 508]), 0)
  assert.equal(snapToTurnStart(10, [7, 20]), 7)
  assert.equal(snapToTurnStart(6, [7, 20]), 0, '小于第一个边界 → 0')
  assert.equal(snapToTurnStart(999, [7, 20]), 20)
})

test('snapToTurnStart：铺开每一步都落在 turn 边界（多轮）', () => {
  const starts = [0, 120, 260, 300]
  let s = snapToTurnStart(380, starts) // 首屏：尾部 turn
  assert.equal(s, 300)
  s = snapToTurnStart(stepReveal(s), starts)
  assert.equal(s, 260)
  s = snapToTurnStart(stepReveal(s), starts)
  assert.equal(s, 120)
  s = snapToTurnStart(stepReveal(s), starts)
  assert.equal(s, 0)
  assert.equal(stepReveal(0), 0, '到底后单调停在 0')
})
