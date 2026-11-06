/* 流式 markdown 的**切分**：把还在长的正文切成「已完成的整块」+「尾部还在长的一块」。
 *
 * 为什么需要（实测，见 lab/bench-markdown.mjs / lab/probe-md-split.mjs）：
 * 结构化长回答（列表+表格+代码围栏）每个 chunk 都要把整段重新解析一遍 —— 8.2k 字时
 * 单次解析 24ms（占一次渲染的 93%），整段流式因此是 O(n²)。切成整块之后，已完成的块
 * 各自 memo，只有尾部那一块重新解析。
 *
 * 这个文件只做**纯文本切分**，不碰渲染；正确的判据是「切出来的片段顺序拼回去 === 原文」
 *（head.join('') + tail === text），所以任何拿不准的地方都保守地不切。
 *
 * 不切的位置（切了会改变语义/版式）：
 *   · 代码围栏内部（``` / ~~~，含未闭合的那一段）
 *   · $$ 数学块内部
 *   · 列表项之间（空白行之后仍属于同一个列表时）
 *   · 表格行之间（GFM 表格允许行间无空行，但空行后紧跟表格行时切开会拆表）
 *   · 引用块 / 缩进续行 / 行尾硬换行（两个空格或反斜杠）
 */

export type MdSplit = { head: string[]; tail: string }

const FENCE = /^ {0,3}(`{3,}|~{3,})/
const LIST = /^ {0,3}(?:[-*+]|\d{1,9}[.)])(?:\s|$)/
const TABLE = /^ {0,3}\|/
const QUOTE = /^ {0,3}>/
const INDENT = /^(?:\t| {1,})/
const MATH = /^\s*\$\$\s*$/

const prevNonBlank = (lines: string[], from: number): number => {
  for (let i = from - 1; i >= 0; i--) if (lines[i].trim() !== '') return i
  return -1
}
const nextNonBlank = (lines: string[], from: number): number => {
  for (let i = from + 1; i < lines.length; i++) if (lines[i].trim() !== '') return i
  return -1
}

/**
 * @param text      流式中的正文
 * @param minChars  低于这个长度不切（切分本身也有成本，短回答没必要）
 * @returns 切分结果；不值得切时返回 null（调用方直接整段渲染）
 */
export function splitStreamingMarkdown(text: string, minChars = 1200): MdSplit | null {
  if (text.length < minChars) return null
  const lines = text.split('\n')
  // 按**字符偏移**切（不是按行数组拼回去）：行数组 join 会把「块尾那个空行」少算一个
  // 换行，拼出来的文本与原文差一个 \n（实测丢字就是这么来的）。
  const starts: number[] = new Array(lines.length)
  let off = 0
  for (let i = 0; i < lines.length; i++) { starts[i] = off; off += lines[i].length + 1 }
  let fence: string | null = null // 当前围栏标记（``` / ```` / ~~~ …）
  let math = false
  const cuts: number[] = []
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i]
    const m = FENCE.exec(line)
    if (m) {
      const mark = m[1]
      if (!fence) fence = mark
      // 收尾围栏：同字符、长度不短于开启、且这一行只有围栏（允许尾随空格）
      else if (mark[0] === fence[0] && mark.length >= fence.length && line.trim() === mark) fence = null
      continue
    }
    if (!fence && MATH.test(line)) { math = !math; continue }
    if (fence || math || line.trim() !== '') continue
    // 顶层空行 → 候选切分点：切在**下一个非空行的起始偏移**
    const p = prevNonBlank(lines, i)
    const n = nextNonBlank(lines, i)
    if (p < 0 || n < 0) continue
    const pl = lines[p]
    const nl = lines[n]
    const unsafe = LIST.test(pl) || TABLE.test(pl) || QUOTE.test(pl) || INDENT.test(pl)
      || / {2,}$/.test(pl) || pl.endsWith('\\') // 行尾硬换行
      || LIST.test(nl) || TABLE.test(nl) || INDENT.test(nl) // 空行后仍是同一个列表/表格/续行
    if (!unsafe) cuts.push(starts[n])
  }
  if (!cuts.length) return null
  const head: string[] = []
  let from = 0
  for (const c of cuts) {
    if (c <= from) continue
    head.push(text.slice(from, c))
    from = c
  }
  const tail = text.slice(from)
  if (!head.length || !tail.length) return null
  return { head, tail }
}
