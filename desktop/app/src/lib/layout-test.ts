// 三态布局换算单测（PR-01 验收）
// 覆盖的是生产真正调用的三个函数：afterDrag / biasOfRatio / normalizeLayout。
import test from 'node:test'
import assert from 'node:assert/strict'
import { BIAS_RATIO, afterDrag, biasOfRatio, collapse, expand, normalizeLayout } from './layout'
import type { RightLayout } from './layout'

const ratioOf = (l: RightLayout) => (l.open ? l.ratio : -1)
const biasOf = (l: RightLayout) => (l.open ? l.bias : 'collapsed')

test('预设：expand 默认 even；三个预设比例固定', () => {
  assert.deepEqual(expand(), { open: true, bias: 'even', ratio: 50 })
  assert.deepEqual(expand('center'), { open: true, bias: 'center', ratio: BIAS_RATIO.center })
  assert.deepEqual(expand('right'), { open: true, bias: 'right', ratio: BIAS_RATIO.right })
  assert.deepEqual(collapse, { open: false })
})

test('拖拽换算：layout 百分比 → 右栏占「除左栏外」的比例', () => {
  // 1200 宽三栏：左 236(19.67%) + 中 482(40.17%) + 右 482(40.17%)
  const expanded = afterDrag(40.17, 19.67)
  assert.deepEqual(expanded, { open: true, bias: 'even', ratio: 50 })
  // 左栏 rail 44(3.67%) + 中 578 + 右 578
  const railed = afterDrag(48.17, 3.67)
  assert.deepEqual(railed, { open: true, bias: 'even', ratio: 50 })
})

test('窄拖拽也要记账（旧守卫按像素比两次基准不同，会把 <25% 的真拖拽丢掉）', () => {
  // 右栏只有 15%：ratio = 15 / (100-20) ≈ 18.75 → 19，而不是被当成抖动
  const narrow = afterDrag(15, 20)
  assert.equal(narrow.open, true)
  assert.equal(biasOf(narrow), 'custom')
  assert.equal(ratioOf(narrow), 19)
})

test('拖拽落在预设 ±1 内保留预设，否则标 custom', () => {
  assert.equal(biasOf(afterDrag(40.17, 19.67)), 'even')
  const custom = afterDrag(41.7, 19.67)
  assert.equal(biasOf(custom), 'custom')
  assert.equal(ratioOf(custom), 52)
})

test('拖拽到 center/right 预设区间会吸附', () => {
  assert.equal(biasOf(afterDrag(28.9, 19.67)), 'center')
  assert.equal(biasOf(afterDrag(51.4, 19.67)), 'right')
})

test('极端输入被夹在 0..100', () => {
  assert.equal(ratioOf(afterDrag(-100, 236)), 0)
  assert.equal(ratioOf(afterDrag(999, 236)), 100)
  // 左栏占满也不除零
  assert.equal(afterDrag(10, 100).open, true)
})

test('百分比匹配预设：±1 容差内保留 bias，否则 custom', () => {
  assert.equal(biasOfRatio(50), 'even')
  assert.equal(biasOfRatio(51), 'even')
  assert.equal(biasOfRatio(36), 'center')
  assert.equal(biasOfRatio(64), 'right')
  assert.equal(biasOfRatio(47), 'custom')
})

test('老数据归一化：没有 rightLayout 时从 rightExpanded 推', () => {
  assert.deepEqual(normalizeLayout(undefined, true), expand('even'))
  assert.deepEqual(normalizeLayout(undefined, false), collapse)
  assert.deepEqual(normalizeLayout(expand('center'), false), expand('center'))
})
