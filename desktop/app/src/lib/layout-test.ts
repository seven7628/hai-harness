// 右栏布局换算单测（PR-01 验收；2026-11 收口为「右重 + 自定义」）
// 覆盖的是生产真正调用的三个函数：afterDrag / biasOfRatio / normalizeLayout。
import test from 'node:test'
import assert from 'node:assert/strict'
import { BIAS_RATIO, afterDrag, biasOfRatio, collapse, expand, normalizeLayout } from './layout'
import type { RightLayout } from './layout'

const ratioOf = (l: RightLayout) => (l.open ? l.ratio : -1)

test('预设：expand 默认右重（唯一预设），比例 64', () => {
  assert.deepEqual(expand(), { open: true, bias: 'right', ratio: 64 })
  assert.equal(BIAS_RATIO.right, 64)
  assert.deepEqual(collapse, { open: false })
})

test('拖拽换算：layout 百分比 → 右栏占「除左栏外」的比例', () => {
  // 1200 宽三栏：左 236(19.67%) + 中 241(20.08%) + 右 723(60.25%)
  const expanded = afterDrag(60.25, 19.67)
  assert.deepEqual(expanded, { open: true, bias: 'custom', ratio: 75 })
  // 拖到 64%：吸附回 right 预设
  const snapped = afterDrag(51.4, 19.67)
  assert.deepEqual(snapped, { open: true, bias: 'right', ratio: 64 })
})

test('窄拖拽也要记账（旧守卫按像素比两次基准不同，会把 <25% 的真拖拽丢掉）', () => {
  // 右栏只有 15%：ratio = 15 / (100-20) ≈ 18.75 → 19，而不是被当成抖动
  const narrow = afterDrag(15, 20)
  assert.equal(narrow.open, true)
  assert.equal(biasOfRatio(narrow.open ? narrow.ratio : 0), 'custom')
  assert.equal(ratioOf(narrow), 19)
})

test('拖拽落在预设 ±1 内保留预设，否则标 custom', () => {
  assert.equal(biasOfRatio(64), 'right')
  const custom = afterDrag(41.7, 19.67)
  assert.equal(custom.open && custom.bias, 'custom')
  assert.equal(ratioOf(custom), 52)
})

test('已下线的 even / center 不再吸附（旧值 50/36 = custom）', () => {
  assert.equal(biasOfRatio(50), 'custom')
  assert.equal(biasOfRatio(36), 'custom')
  assert.equal(biasOfRatio(64), 'right')
  assert.equal(biasOfRatio(65), 'right')
})

test('极端输入被夹在 0..100', () => {
  assert.equal(ratioOf(afterDrag(-100, 236)), 0)
  assert.equal(ratioOf(afterDrag(999, 236)), 100)
  // 左栏占满也不除零
  assert.equal(afterDrag(10, 100).open, true)
})

test('老数据归一化：没有 rightLayout 时从 rightExpanded 推（默认右重）', () => {
  assert.deepEqual(normalizeLayout(undefined, true), expand('right'))
  assert.deepEqual(normalizeLayout(undefined, false), collapse)
  // 已下线的预设迁移到右重：老会话不能还停在 50%/36%
  assert.deepEqual(normalizeLayout(expand('right'), false), expand('right'))
  assert.deepEqual(
    normalizeLayout({ open: true, bias: 'even' as never, ratio: 50 }, true),
    expand('right'),
  )
  assert.deepEqual(
    normalizeLayout({ open: true, bias: 'center' as never, ratio: 36 }, true),
    expand('right'),
  )
  // 用户亲手拖出来的宽度不能被一次改版抹掉
  assert.deepEqual(
    normalizeLayout({ open: true, bias: 'custom', ratio: 42 }, true),
    { open: true, bias: 'custom', ratio: 42 },
  )
})
