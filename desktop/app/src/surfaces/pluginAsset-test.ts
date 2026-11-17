// surfaces/pluginAsset-test.ts —— 插件图标「emoji vs 目录内路径」的判定。
//
// 用户 2026-11 报障：左栏插件图标位置显示的是 `icon.svg` 四个字。根因就是把路径当文本画；
// 判据只有一份（本文件），三种渲染点都读它。
import test from 'node:test'
import assert from 'node:assert/strict'
import { canLoadPluginAsset, isPluginAssetPath } from './pluginAsset'

test('目录内图片路径 → true（左栏走 hai-surface 协议渲染）', () => {
  for (const p of ['icon.svg', 'assets/logo.PNG', 'a/b/c.webp', 'x.jpeg', 'x.jpg', 'x.gif']) {
    assert.equal(isPluginAssetPath(p), true, p)
  }
})

test('emoji / 纯文本 / 空值 → false（左栏按文本画）', () => {
  for (const p of ['📕', '插件', '', '   ', undefined, null, 'v1.0', 'readme.md', 'icon .svg', 'icon.svg?v=1']) {
    assert.equal(isPluginAssetPath(p as string), false, String(p))
  }
})

test('浏览器里不发 hai-surface:// 请求；同源静态目录照发；Electron 里才发协议', () => {
  const g = globalThis as unknown as { document?: { body: { classList: { contains: (c: string) => boolean } } } }
  g.document = { body: { classList: { contains: () => false } } }
  assert.equal(canLoadPluginAsset('hai-surface://x/icon.svg'), false)
  assert.equal(canLoadPluginAsset('/e2e/probe/plugins/x/icon.svg'), true)
  assert.equal(canLoadPluginAsset(''), false)
  g.document = { body: { classList: { contains: (c: string) => c === 'is-electron' } } }
  assert.equal(canLoadPluginAsset('hai-surface://x/icon.svg'), true)
})
