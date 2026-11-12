// 内置 provider 注册冒烟测试：证明「副作用 import 真的把 git 注册进去了」
// （这类接线最容易在重构里被 tree-shaking 或循环依赖悄悄弄丢）
import test from 'node:test'
import assert from 'node:assert/strict'
import './index'
import { getSurface } from '../registry'

test('builtin/index 的副作用注册了 git provider', () => {
  const p = getSurface('git')
  assert.ok(p, 'git provider 未注册')
  assert.equal(p.kind, 'git')
  assert.equal(p.namespace, 'builtin')
  assert.equal(p.group, 'workflow')
  assert.equal(p.weight, 'light')
  assert.equal(p.singleton, true)
  assert.deepEqual(p.capabilities, ['read', 'view'])
  assert.equal(typeof p.render, 'function')
})
