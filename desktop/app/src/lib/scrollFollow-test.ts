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
import { decideFollow, isExternalMoveUp, isNearBottom, isUserScrollUp, shouldPinBottom, FOLLOW_THRESHOLD } from './scrollFollow'

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
