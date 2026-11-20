// [recovered] from /private/tmp/builtin-surfaces.test.cjs（模块标记 `// src/components/terminal/formatSelection.ts`）
// [implemented] 依据 docs/TERMINAL_E2E_CONTRACTS.md「formatSelection 裁剪与围栏」：
//   切片里 `formatSelection` 没有 export、类型全丢。本文件补导出名（TerminalPane.tsx 的
//   import：formatSelection）与类型标注，**逻辑逐字未改**；移除 @ts-nocheck。
//
// 终端摘录要能直接进对话：单行用一对反引号（含反引号就原文返回，别造坏行内代码），
// 多行用围栏（正文自己含 ``` 就换 ~~~），并且**有界** —— 200 行 / 20000 字节，
// 截断时明说「已截断，共 N 行」，用户才知道自己引用的是残片。

const MAX_LINES = 200
const MAX_BYTES = 20000

function byteLength(text: string): number {
  return new TextEncoder().encode(text).length
}

/** 按 UTF-8 **字节**截断，并吃掉截断点留下的半个字符（U+FFFD）。 */
function cutBytes(text: string, limit: number): string {
  const bytes = new TextEncoder().encode(text)
  if (bytes.length <= limit) return text
  return new TextDecoder('utf-8').decode(bytes.slice(0, limit)).replace(/\uFFFD$/, '')
}

function clip(text: string): string {
  const kept = text.split('\n').slice(0, MAX_LINES).join('\n')
  return byteLength(kept) > MAX_BYTES ? cutBytes(kept, MAX_BYTES) : kept
}

/** 选区/整屏 → 可进对话的 Markdown 片段。空/全空白 → 空串（调用方据此不弹「已引用」）。 */
export function formatSelection(text: string): string {
  const trimmed = text.trim()
  if (trimmed === '') return ''
  // 单行：用 trim 后的；多行：只去**末尾**换行，保留首行缩进（缩进就是代码语义）。
  let body = trimmed.includes('\n') ? text.replace(/\n+$/, '') : trimmed
  const lines = body.split('\n').length
  if (lines > MAX_LINES || byteLength(body) > MAX_BYTES) {
    body = `${clip(body)}\n… （已截断，共 ${lines} 行）`
  }
  if (!body.includes('\n')) return body.includes('`') ? body : `\`${body}\``
  const fence = body.includes('```') ? '~~~' : '```'
  return `${fence}\n${body}\n${fence}`
}
