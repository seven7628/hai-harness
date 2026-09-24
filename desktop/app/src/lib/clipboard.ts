// 复制文本到系统剪贴板（全应用统一入口：Markdown 代码块 / artifacts 路径 / 设置页连接串）。
//
// 为什么需要这一层（2026-09-15 实测教训，别再退回裸 navigator.clipboard）：
// renderer 的 `navigator.clipboard.writeText` 在 Electron 下受权限判定约束，而**请求的权限名
// 会随「是否用户手势」漂移**：
//   · 程序化调用（无手势）→ 请求 `clipboard-read`
//   · 真实点击「复制」按钮（有手势）→ 请求 `clipboard-sanitized-write`
// 只放行前者时，无头/脚本化调用一切正常，**真实点击却每次 NotAllowedError** —— 这正是用户
// 报的「点复制一直报错」：报错是真失败，系统剪贴板并未写入。权限名属于 Electron 内部行为，
// 会随版本变化，因此这里不赌它：
//   ① 先走浏览器 Clipboard API（Web 标准路径，非 Electron 环境同样可用）
//   ② 失败则回退主进程 clipboard 模块（`window.desktop.clipboardWrite`）—— 不受 renderer
//      权限体系约束，是同一端能力的稳定入口
//   ③ 两条都失败才返回 false，由调用方给出可见反馈（不静默）
//
// 返回是否真的写入成功（调用方据此显示「已复制」/「复制失败」）。
export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    /* 权限被拒 / 非安全上下文 / 无 Clipboard API：回退主进程 */
  }
  try {
    const r = await window.desktop?.clipboardWrite?.(text)
    return Boolean(r?.ok)
  } catch {
    return false
  }
}
