// plugin-app-reply-test.ts —— 页面回执归一的回归：顶层展开字段不能丢。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { normalizeHostReply } from './plugin-app-reply.ts'

test('agent.* 的顶层展开字段与 data 同时保留', () => {
  const r = normalizeHostReply({ ok: true, data: { session_id: 's-1' }, session_id: 's-1' })
  assert.equal(r.ok, true)
  assert.equal(r.session_id, 's-1')
  assert.deepEqual(r.data, { session_id: 's-1' })
})

test('拒绝信封的 reason 保留；ok 只认严格 true', () => {
  const r = normalizeHostReply({ ok: 'yes', reason: 'denied', extra: 1 })
  assert.equal(r.ok, false)
  assert.equal(r.reason, 'denied')
  assert.equal(r.extra, 1)
})

test('非对象 / 空值收敛成 ok:false（永不抛）', () => {
  assert.deepEqual(normalizeHostReply(null), { ok: false })
  assert.deepEqual(normalizeHostReply('nope'), { ok: false })
  assert.deepEqual(normalizeHostReply(undefined), { ok: false })
})

test('data:undefined 时不额外加字段（与渲染层回执形状一致）', () => {
  assert.deepEqual(normalizeHostReply({ ok: true }), { ok: true })
})
