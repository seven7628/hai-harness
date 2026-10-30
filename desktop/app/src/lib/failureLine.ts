// 「这次调用为什么失败」只该有一句人话 —— 这一处是唯一来源：
// 工具行收缩态的失败行、批量组收缩态的失败摘要行都从这里取，不各写一套。
//
// 背景（用户 2026-10 报障：收缩态那行只剩 `✗ .`）：结果文本的第一行**不一定是有信息量的**。
// bash 的真实结果长这样（`~/.go-code/sessions` 里的记录）：
//   ---
//   /Users/admin/cybertron-gogroup/agent-server-g
//   [exit: exit status 1]
// 首行是 `---`（分隔线），照原样搬过来就是 `✗ ---`；脚本信封的首行是 `Script failed`，
// 搬过来就是 `✗ Script failed`（等于没说）。所以按**优先级**找一句话：
//
//   1. 引擎自己写的结局标记 `[exit: exit status N]` —— 它是退出码的原文，最短最准；
//   2. 否则取第一条**真有内容**的行（含 ≥2 个字母/数字/汉字，去掉列表符与分隔线）；
//   3. 否则退到 errorInfo.code（结构化错误码，例如 `tool_timeout`）；
//   4. 都没有就返回空串 —— 上层**不渲染**这一行。宁可没有，也不要一个 `✗ .` 的假原因。
//
// 截断在末尾（保留开头，开头才是有辨识度的部分）。

const EXIT_RE = /\[exit:\s*([^\]]+)\]/i

/** 结果首行/退出码里的一句话原因；取不到就返回空串（调用方据此不渲染）。 */
export function failureLine(
  result?: string,
  errorInfo?: { code?: string; nextAction?: string },
  max = 120,
): string {
  const text = result ?? ''
  const exit = EXIT_RE.exec(text)
  if (exit?.[1]) {
    // 标记内文本身常是 `exit status 1`（引擎原文）—— 别再叠一个 `exit`
    const inner = exit[1].trim()
    return clip(/^exit\b/i.test(inner) ? inner : `exit ${inner}`, max)
  }
  for (const raw of text.split('\n')) {
    const line = raw.trim().replace(/^[-•*>]+\s*/, '')
    if (!isInformative(line)) continue
    return clip(line, max)
  }
  if (errorInfo?.code) return clip(errorInfo.code, max)
  if (errorInfo?.nextAction) return clip(errorInfo.nextAction, max)
  return ''
}

/** 一行是否算「有信息量」：至少两个字母/数字/汉字，且不是信封里那几句套话。 */
function isInformative(line: string): boolean {
  if (!line) return false
  if (/^(script (failed|completed)|wall time .*|output:?|result:?)$/i.test(line)) return false
  if (/^[-=_*~#·—–.\s]+$/.test(line)) return false
  const meaningful = line.match(/[\p{L}\p{N}]/gu)?.length ?? 0
  return meaningful >= 2
}

function clip(s: string, max: number): string {
  return s.length > max ? s.slice(0, Math.max(1, max - 1)) + '…' : s
}
