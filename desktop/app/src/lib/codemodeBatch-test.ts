import { test } from 'node:test'
import assert from 'node:assert/strict'
import { batchLineOf, batchProgressOf } from './codemodeBatch'

const call = (o: { isError?: boolean; durMs?: number } = {}) => ({
  id: 'codemode#1/0', name: 'bash', args: '{}', result: '', isError: !!o.isError, error: '', durMs: o.durMs,
})

test('没有子调用 → 没有进度（不画 0/0）', () => {
  assert.equal(batchProgressOf([]), undefined)
  assert.equal(batchProgressOf(undefined), undefined)
})

test('结束后：总数/完成/失败/耗时全部来自 nested_calls 真账', () => {
  const p = batchProgressOf([call({ durMs: 4200 }), call({ durMs: 900 }), call({ isError: true, durMs: 300 })])!
  assert.deepEqual(p, { total: 3, done: 2, failed: 1, durMs: 5400, pct: 2 / 3 })
})

test('全部成功 → 100%，行内不带 failed 段', () => {
  const p = batchProgressOf([call({ durMs: 1000 }), call({ durMs: 2000 })])!
  assert.equal(p.pct, 1)
  assert.equal(batchLineOf(p, 'execute', '3.0s'), 'Execute · 2/2 completed · 3.0s')
})

test('有失败 → 失败段必须在（收缩态也要看见）', () => {
  const p = batchProgressOf([call(), call({ isError: true })])!
  assert.equal(batchLineOf(p, 'execute'), 'Execute · 2/2 completed · 1 failed')
})

test('检索档说 Search（同一份数字，不同的类型词）', () => {
  const p = batchProgressOf([call()])!
  assert.match(batchLineOf(p, 'search'), /^Search · 1\/1 completed/)
})
