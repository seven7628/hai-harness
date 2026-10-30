// 长路径在工具行里要**先短掉**，否则它会把后面的度量（+96 −0）和耗时（17 ms）挤没。
//
// 规则（用户 2026-10：这种超长路径简化一下，让后边的时间信息显示出来）：
//   · 短路径（≤ 56 字符）原样返回 —— 绝大多数路径不该被改动；
//   · 长路径保留最后两段，前面用 `…/` 收掉：`…/references/architecture-an…`；
//   · 单段仍然过长时按字符截断（尾部留 `…`）；
//   · 结尾的 `:行号[-行号]`（read_file 的区间）与路径分开处理，截断不吞掉它。
// 完整路径始终在 title 里（点击预览也用完整路径），这里只影响**显示**。

const MAX = 56
const KEEP_SEGMENTS = 2
const SEG_MAX = 26

export function shortPath(input: string): string {
  if (!input) return input
  if (input.length <= MAX) return input
  // 行号后缀单独拿出来
  const m = /^(.*?)(:\d+(?:-\d+)?)?$/.exec(input)
  const body = m?.[1] ?? input
  const suffix = m?.[2] ?? ''
  const parts = body.split('/').filter((s) => s.length > 0)
  if (parts.length <= 1) return truncate(body) + suffix
  const tail = parts.slice(-KEEP_SEGMENTS).map((seg, i, arr) => (i === arr.length - 1 ? truncate(seg) : truncate(seg, SEG_MAX))).join('/')
  return '…/' + tail + suffix
}

function truncate(s: string, max = SEG_MAX): string {
  if (s.length <= max) return s
  return s.slice(0, Math.max(1, max - 1)) + '…'
}
