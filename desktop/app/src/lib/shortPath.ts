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

// 文件**名**（不是路径）在窄栏里要保留尾部：扩展名 + 名字末段才是有辨识度的部分。
// 右栏标签最宽 168px，长名字现在是从尾巴截断的（`conversation_route…`），
// 于是 `conversation_router.go` 与 `conversation_router_test.go` 长得一模一样，
// 分不清哪个是测试、哪个是源码（用户 2026-10）。这里改成中间截断。
//
//   conversation_router_helper_test.go  →  conversation_rout…_test.go
//
// 短名字原样返回（绝大多数文件不该被改动）；完整名字始终在 title 里。
const NAME_MAX = 24
// 尾巴 = 名字末段（5 字符，够放下 `_test` / `_util` / `_view` 这类区分段）+ 扩展名。
// 预算不能给太多：尾巴每多一字符，头部的辨识度就少一字符，而文件名开头往往才是检索词。
const NAME_TAIL_BASE = 5

export function shortName(input: string, max = NAME_MAX): string {
  if (!input || input.length <= max) return input
  const dot = input.lastIndexOf('.')
  const base = dot > 0 ? input.slice(0, dot) : input
  const ext = dot > 0 ? input.slice(dot) : ''
  const tail = base.slice(-NAME_TAIL_BASE) + ext
  const head = input.slice(0, Math.max(1, max - tail.length - 1))
  return head + '…' + tail
}
