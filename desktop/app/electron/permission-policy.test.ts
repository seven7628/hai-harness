// permission-policy.test.ts — 权限策略单测（node:test，纯 Node 无 Electron 依赖，毫秒级）。
//
// 为什么值得单测：这条策略曾**看起来已修但只覆盖一半** —— 旧实现只放行 clipboard-read，
// 而真实点击「复制」按钮走的是 clipboard-sanitized-write → 用户每次点击必报错。
// 当时的实测是在无手势场景做的，所以程序化调用通过、真实交互失败。本测用「真实点击」
// 这条路径做断言，锁住不回归。
//
// 运行：esbuild bundle 后 node --test（见 package.json test:permission-policy）。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  CLIPBOARD_PERMISSIONS,
  clipboardPermissionFor,
  isMainWindowPermissionAllowed,
} from './permission-policy.ts'

test('真实点击（用户手势）请求 clipboard-sanitized-write 且被放行', () => {
  const permission = clipboardPermissionFor('gesture')
  assert.equal(permission, 'clipboard-sanitized-write')
  assert.equal(isMainWindowPermissionAllowed(permission), true,
    '真实点击是「复制」按钮的默认路径，必须放行（旧实现漏掉此项 → 用户反馈「点复制一直报错」）')
})

test('程序化调用（无手势）请求 clipboard-read 且被放行', () => {
  const permission = clipboardPermissionFor('no-gesture')
  assert.equal(permission, 'clipboard-read')
  assert.equal(isMainWindowPermissionAllowed(permission), true)
})

test('两种触发方式都必须放行（缺任一种 → 复制在真实交互下失效）', () => {
  for (const state of ['gesture', 'no-gesture'] as const) {
    assert.equal(isMainWindowPermissionAllowed(clipboardPermissionFor(state)), true,
      `${state} 路径未被放行`)
  }
  assert.equal(CLIPBOARD_PERMISSIONS.length, 2)
})

test('media（麦克风，语音输入）仍放行', () => {
  assert.equal(isMainWindowPermissionAllowed('media'), true)
})

test('未列入的权限一律拒绝（边界不收窄）', () => {
  for (const p of ['geolocation', 'notifications', 'usb', 'display-capture', 'midi', 'openExternal', 'unknown']) {
    assert.equal(isMainWindowPermissionAllowed(p), false, `${p} 不应被放行`)
  }
})

test('反证：若只放行 clipboard-read（旧实现），真实点击路径必然失败', () => {
  // 把旧策略写进断言 —— 这条一旦被"优化"回去会立刻红。
  const legacyAllow = (p: string) => p === 'media' || p === 'clipboard-read'
  assert.equal(legacyAllow(clipboardPermissionFor('no-gesture')), true, '旧策略下程序化调用通过（当年因此误判已修）')
  assert.equal(legacyAllow(clipboardPermissionFor('gesture')), false, '旧策略下真实点击失败 —— 这正是用户看到的现象')
})
