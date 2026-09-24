// Electron 权限策略（纯函数，便于毫秒级单测 —— 不必拉起整个应用）。
//
// 为什么单独成文件：剪贴板写入的**权限名会随「是否用户手势」漂移**（见下），这类"看起来
// 已修、实际只覆盖一半"的问题，只有把策略从 Electron 生命周期里剥出来才测得快、测得准。
// 历史教训：旧实现只放行 `clipboard-read`，而当时是在**无手势**场景做的实测 → 结论「已修」，
// 但真实点击走的是 `clipboard-sanitized-write`，用户每次点复制必报错。

/** 是否用户手势触发 —— 决定 writeText 请求哪个权限名（实测，非推测）。 */
export type GestureState = 'gesture' | 'no-gesture'

// 实测（Electron 33.4.11，最小 app 逐变量隔离）：
//   · 无手势调用 navigator.clipboard.writeText → 请求 'clipboard-read'
//   · 真实点击调用                            → 请求 'clipboard-sanitized-write'
// 两个名字都必须放行，否则「复制」在真实交互下失效（程序化调用却正常，极易漏测）。
// 与 origin（file:// / http://localhost）、是否装 preload、是否装 check handler 无关。
export const CLIPBOARD_PERMISSIONS = ['clipboard-read', 'clipboard-sanitized-write'] as const

// writeText 会请求的权限名（给定触发方式）。
export function clipboardPermissionFor(state: GestureState): string {
  return state === 'gesture' ? 'clipboard-sanitized-write' : 'clipboard-read'
}

/** 主窗口需要放行的权限（其余一律拒绝）。 */
export function isMainWindowPermissionAllowed(permission: string): boolean {
  // media：麦克风（语音输入）—— 无 handler 时 getUserMedia 一律拒绝，必须显式授权。
  // 剪贴板两项：见上（真实点击走 sanitized-write）。
  return permission === 'media' || (CLIPBOARD_PERMISSIONS as readonly string[]).includes(permission)
}
