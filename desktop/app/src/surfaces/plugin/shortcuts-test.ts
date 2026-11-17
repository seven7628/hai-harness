// surfaces/plugin/shortcuts-test.ts —— 插件入口快捷键（⌥⌘1..9）的编号、文案与匹配。
//
// 为什么值得单测：三个消费点（App 全局键盘 / rail tooltip / ＋ 菜单 kbd）必须同一套编号。
// 漂移的症状是「提示写 ⌥⌘3，按 3 打开的是另一个插件」—— 只有按下去才会发现。
import test from 'node:test'
import assert from 'node:assert/strict'
import {
  PLUGIN_SHORTCUT_MAX,
  pluginEntryShortcut,
  pluginEntryShortcutAria,
  pluginShortcutIndex,
} from './shortcuts'

// Node 25 的 globalThis.navigator 是只读 getter：用 defineProperty 覆盖。
const nav = { platform: 'MacIntel', userAgent: 'Mozilla/5.0 (Macintosh)' }
Object.defineProperty(globalThis, 'navigator', { value: nav, configurable: true, writable: true })

test('mac 文案：⌥⌘1..9，越界没有快捷键', () => {
  assert.equal(pluginEntryShortcut(0), '⌥⌘1')
  assert.equal(pluginEntryShortcut(8), '⌥⌘9')
  assert.equal(pluginEntryShortcut(9), '')
  assert.equal(pluginEntryShortcut(-1), '')
  assert.equal(PLUGIN_SHORTCUT_MAX, 9)
})

test('非 mac 文案用 Ctrl+Alt（匹配仍同时接受 meta/ctrl）', () => {
  nav.platform = 'Win32'
  nav.userAgent = 'Windows'
  assert.equal(pluginEntryShortcut(2), 'Ctrl+Alt+3')
  assert.equal(pluginEntryShortcutAria(2), 'Control+Alt+3')
  nav.platform = 'MacIntel'
  nav.userAgent = 'Mac'
})

test('aria-keyshortcuts 用规范写法（不是 ⌥⌘ 展示符号）', () => {
  assert.equal(pluginEntryShortcutAria(0), 'Alt+Meta+1')
  assert.equal(pluginEntryShortcutAria(8), 'Alt+Meta+9')
  assert.equal(pluginEntryShortcutAria(9), '')
})

test('匹配：⌥⌘digit → 序号；缺 alt / 带 shift / 纯 ⌘ 数字都不算', () => {
  assert.equal(pluginShortcutIndex({ metaKey: true, altKey: true, code: 'Digit3' }), 2)
  assert.equal(pluginShortcutIndex({ ctrlKey: true, altKey: true, code: 'Digit1' }), 0)
  assert.equal(pluginShortcutIndex({ metaKey: true, code: 'Digit3' }), -1, '⌘3 是切最近会话')
  assert.equal(pluginShortcutIndex({ metaKey: true, shiftKey: true, altKey: true, code: 'Digit3' }), -1)
  assert.equal(pluginShortcutIndex({ metaKey: true, altKey: true, key: '3' }), 2, '没有 code 的合成事件走 key 兜底')
  assert.equal(pluginShortcutIndex({ metaKey: true, altKey: true, code: 'Digit0' }), -1)
  assert.equal(pluginShortcutIndex({ metaKey: true, altKey: true, code: 'KeyB' }), -1)
})
