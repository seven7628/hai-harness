// 注册表单测（PR-00 验收）：node --test + esbuild，见 00-conventions.md §1.9
import test from 'node:test'
import assert from 'node:assert/strict'
import {
  getSurface,
  listSurfaces,
  registerSurface,
  resetSurfacesForTest,
  subscribeSurfaces,
  unregisterSurface,
} from './registry'
import type { DistributiveOmit, SurfaceKind, SurfaceProvider, SurfaceTab } from './types'

function make(over: Partial<SurfaceProvider> = {}): SurfaceProvider {
  return {
    kind: 'git',
    namespace: 'builtin',
    group: 'workflow',
    title: () => 'Git',
    render: () => null,
    ...over,
  }
}

test.beforeEach(() => resetSurfacesForTest())

test('注册后可按 kind 取回', () => {
  const p = make()
  registerSurface(p)
  assert.equal(getSurface('git'), p)
  assert.equal(getSurface(undefined), undefined)
})

test('同一 kind 重复注册抛错，且不覆盖已有实例', () => {
  const first = make()
  registerSurface(first)
  assert.throws(() => registerSurface(make()), /重复注册/)
  assert.equal(getSurface('git'), first)
})

test('builtin 不允许 plugin: 前缀', () => {
  assert.throws(() => registerSurface(make({ kind: 'plugin:x' })), /不允许 plugin: 前缀/)
})

test('plugin 必须带 plugin: 前缀', () => {
  assert.throws(() => registerSurface(make({ namespace: 'plugin' })), /必须使用 plugin: 前缀/)
})

test('插件不允许声明 write 能力', () => {
  const p = make({ kind: 'plugin:sheet', namespace: 'plugin', group: 'plugin', capabilities: ['read', 'write'] })
  assert.throws(() => registerSurface(p), /不允许 write 能力/)
})

test('内置 provider 可以声明 write（编辑器需要）', () => {
  assert.doesNotThrow(() => registerSurface(make({ kind: 'file', capabilities: ['read', 'write'] })))
})

test('未知内置 kind 被拒', () => {
  // 刻意绕类型：验证运行期拒绝未在 BuiltinKind 里的 kind
  assert.throws(() => registerSurface(make({ kind: 'office' as unknown as SurfaceKind })), /未知的内置 kind/)
})

test('排序：group → order（缺省 100）→ kind', () => {
  registerSurface(make({ kind: 'web', group: 'content' }))
  registerSurface(make({ kind: 'git', group: 'workflow', order: 200 }))
  registerSurface(make({ kind: 'activity', group: 'workflow' }))
  registerSurface(make({ kind: 'plugin:z', namespace: 'plugin', group: 'plugin', order: 1 }))
  registerSurface(make({ kind: 'plugin:a', namespace: 'plugin', group: 'plugin', order: 1 }))
  assert.deepEqual(
    listSurfaces().map((p) => p.kind),
    ['activity', 'git', 'web', 'plugin:a', 'plugin:z'],
  )
})

test('重复注销不抛错，注销后取不到', () => {
  registerSurface(make())
  assert.equal(unregisterSurface('git'), true)
  assert.equal(getSurface('git'), undefined)
  assert.equal(unregisterSurface('git'), false)
})

test('注销后同一 kind 可以重新注册（安装/卸载会反复走这条路径）', () => {
  registerSurface(make())
  unregisterSurface('git')
  assert.doesNotThrow(() => registerSurface(make({ title: () => 'Git 2' })))
  assert.equal(getSurface('git')?.title({ id: 't', kind: 'git' }, (k) => k), 'Git 2')
})

test('标签形状用分配式 Omit：插件带 params、file 带 path/mode 都能通过类型检查', () => {
  // 回归评审发现：Omit<联合,'id'> 会收缩成 { kind }，这里编译不过就说明又退回去了
  const plugin: DistributiveOmit<SurfaceTab, 'id'> = { kind: 'plugin:x', params: { a: 1 } }
  const file: DistributiveOmit<SurfaceTab, 'id'> = { kind: 'file', path: '/a.md', mode: 'source' }
  assert.equal(plugin.kind, 'plugin:x')
  assert.equal(file.kind, 'file')
})

test('订阅在注册/注销时各通知一次，取消后不再通知', () => {
  let n = 0
  const off = subscribeSurfaces(() => n++)
  registerSurface(make())
  assert.equal(n, 1)
  unregisterSurface('git')
  assert.equal(n, 2)
  unregisterSurface('git') // 未删除，不通知
  assert.equal(n, 2)
  off()
  registerSurface(make({ kind: 'activity' }))
  assert.equal(n, 2)
})
