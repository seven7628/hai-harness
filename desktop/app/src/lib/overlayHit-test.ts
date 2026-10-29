import { test } from 'node:test'
import assert from 'node:assert/strict'
import { centerOf, overlayCoversRect, rectsIntersect } from './overlayHit'

const panel = { left: 900, top: 100, right: 1400, bottom: 900 }

test('中点：视口内给出整数坐标', () => {
  assert.deepEqual(centerOf(panel), { x: 1150, y: 500 })
})

test('中点：量不到尺寸 / 消失在视口外 → null（不动视图，避免来回闪）', () => {
  assert.equal(centerOf({ left: 0, top: 0, right: 0, bottom: 0 }), null)
  assert.equal(centerOf(undefined), null)
  assert.equal(centerOf({ left: 2000, top: 100, right: 2400, bottom: 900 }, { width: 1440, height: 900 }), null)
})

const opts = (positions: string[], insideIdx: number[] = []) => ({
  contains: (_: unknown) => false,
  positionOf: (el: unknown) => positions[(el as { i: number }).i],
})

test('中点那一叠里有不属于右栏的 fixed 元素 → 被压住', () => {
  const stack = [{ i: 0 }, { i: 1 }]
  assert.equal(overlayCoversRect(panel, stack, opts(['fixed', 'static'])), true)
})

test('只有右栏自己的元素 / 只有 static 元素 → 没被压住', () => {
  const stack = [{ i: 0 }, { i: 1 }]
  assert.equal(overlayCoversRect(panel, stack, { contains: (el) => (el as { i: number }).i === 0, positionOf: () => 'fixed' }), true)
  assert.equal(overlayCoversRect(panel, stack, { contains: () => true, positionOf: () => 'fixed' }), false)
  assert.equal(overlayCoversRect(panel, stack, opts(['absolute', 'static'])), false)
})

test('空栈 / 量不到尺寸 → 不判定', () => {
  assert.equal(overlayCoversRect(panel, [], opts(['fixed'])), false)
  assert.equal(overlayCoversRect({ left: 0, top: 0, right: 0, bottom: 0 }, [{ i: 0 }], opts(['fixed'])), false)
})

test('相交判定：名单兜底用（缺一方的矩形视为相交）', () => {
  assert.equal(rectsIntersect(panel, { left: 1200, top: 400, right: 1600, bottom: 500 }), true)
  assert.equal(rectsIntersect(panel, { left: 10, top: 400, right: 20, bottom: 500 }), false)
  assert.equal(rectsIntersect(panel, undefined), true)
  assert.equal(rectsIntersect(undefined, { left: 0, top: 0, right: 1, bottom: 1 }), true)
})
