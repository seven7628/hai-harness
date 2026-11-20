// 注册表单测（PR-00 验收）：node --test + esbuild，见 00-conventions.md §1.9
import test from 'node:test'
import assert from 'node:assert/strict'
import {
  BUILTIN_KINDS,
  getSurface,
  isBuiltinKind,
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

// —— PR-11：持久化恢复的 kind 过滤 ——
//
// 为什么这三条断言在注册表侧：BUILTIN_KINDS 原本是模块私有常量（只给 assertProvider 校验用），
// PR-11 把它提升为 store 的 loadPersistedTabs 也读的**唯一白名单**（替换掉 store 自己那份
// TAB_KINDS）。它一旦被改宽（例如把 plugin: 放进来），症状是「重启后一个没有 provider 的
// 幽灵标签复活」，而 store 侧看不见这个常量 —— 所以口径钉在它定义的地方。
test('五个内置 kind 全部通过（与 BuiltinKind 联合逐项对齐）', () => {
  // 2026-11：加入第五个内置 kind `term`（右栏终端，docs/TERMINAL_DESIGN.md §4）。
  // 这条断言刻意**逐字列举**而不是从 BuiltinKind 反射：列举是白名单的独立副本，
  // 悄悄放宽（例如把 plugin: 漏进来）会在这一行先红。
  assert.deepEqual([...BUILTIN_KINDS].sort(), ['activity', 'file', 'git', 'term', 'web'])
  for (const k of ['activity', 'git', 'file', 'web', 'term'] as const) {
    assert.equal(isBuiltinKind(k), true, `${k} 必须被认作内置 kind`)
  }
})

test('plugin: 被拒（插件标签要等 R11b/PR-14 再放开）', () => {
  // 今天的口径：持久化里出现 plugin: 一律丢弃。插件 provider 还没有 loader（PR-12/PR-14），
  // 认了它只会恢复出一个没有 manifest、没人做过入参校验的幽灵标签。
  // 放开时改的是这一条 + loadPersistedTabs 的注释，而不是悄悄放宽白名单。
  assert.equal(isBuiltinKind('plugin:x'), false)
  assert.equal(BUILTIN_KINDS.has('plugin:x'), false)
})

test('未知 kind 被拒（历史枚举、空串、非字符串）', () => {
  assert.equal(isBuiltinKind('office'), false) // 03 §5.1 里迁移掉的历史 kind
  assert.equal(isBuiltinKind(''), false)
  assert.equal(isBuiltinKind(undefined), false)
  assert.equal(isBuiltinKind(42), false)
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
