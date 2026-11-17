// surfaces/plugin/shortcuts.ts —— 插件入口的键盘入口（右侧栏 rail 的「快捷键入口」）。
//
// 口径：入口按 listPluginEntries() 的顺序（与 ＋ 菜单一致）取前 9 个，**⌥⌘1..9**
//（非 macOS 用 Ctrl+Alt+1..9）。为什么不用更直觉的 ⌘⇧1..9：macOS 把 ⌘⇧3/4/5 注册成
// 系统截图快捷键，按键根本到不了应用 —— 一半的入口会「设了快捷键但按不动」。
// **用 e.code 而不是 e.key**：Option 按下后 `key` 是 `¡` `™` 这类字符，拿它匹配数字
// 在部分键盘布局/输入法上会直接失效。
//
// 为什么独立成模块：三个消费者（App 的全局键盘、rail 的 tooltip、＋ 菜单的 kbd 提示）
// 必须用同一套编号 —— 各写一次必然出现「提示写 3、按 3 打开的是另一个」。
export const PLUGIN_SHORTCUT_MAX = 9

/** macOS 系（含 iPad 模拟）用 ⌘，其余用 Ctrl。只影响**显示**，匹配同时接受 meta/ctrl。 */
export function isMacLike(): boolean {
  if (typeof navigator === 'undefined') return true
  const nav = navigator as Navigator & { userAgentData?: { platform?: string } }
  const platform = nav.userAgentData?.platform || nav.platform || nav.userAgent || ''
  return /mac|iphone|ipad|ipod/i.test(platform)
}

/** 第 index 个插件入口的快捷键文案（index 从 0 起；越界返回空串 = 没有快捷键）。 */
export function pluginEntryShortcut(index: number): string {
  if (!Number.isInteger(index) || index < 0 || index >= PLUGIN_SHORTCUT_MAX) return ''
  return isMacLike() ? `⌥⌘${index + 1}` : `Ctrl+Alt+${index + 1}`
}

/**
 * aria-keyshortcuts 的**规范写法**（`Meta+Shift+1`）：它要能被读屏/操作系统解析，
 * `⌥⌘1` 这种展示符号在这里是非法值。展示归 pluginEntryShortcut，语义归这里。
 */
export function pluginEntryShortcutAria(index: number): string {
  if (!Number.isInteger(index) || index < 0 || index >= PLUGIN_SHORTCUT_MAX) return ''
  return `${isMacLike() ? 'Alt+Meta' : 'Control+Alt'}+${index + 1}`
}

/** 键盘事件 → 入口序号（0-based）；不是插件快捷键返回 -1。 */
export function pluginShortcutIndex(e: {
  metaKey?: boolean
  ctrlKey?: boolean
  shiftKey?: boolean
  altKey?: boolean
  code?: string
  key?: string
}): number {
  if (!(e.metaKey || e.ctrlKey) || !e.altKey || e.shiftKey) return -1
  const code = String(e.code || '')
  const m = /^Digit([1-9])$/.exec(code)
  if (m) return Number(m[1]) - 1
  // 兜底：某些合成事件（测试/旧浏览器）没有 code，但 key 是单字符数字。
  const k = String(e.key || '')
  return /^[1-9]$/.test(k) ? Number(k) - 1 : -1
}
