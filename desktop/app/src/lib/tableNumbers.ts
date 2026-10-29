// 表格里的“数字列”判定 —— 给设计里 `.bl-table .n`（右对齐 + tabular-nums）找主人。
//
// 设计把表格当“数字的网格”：表头退成标签，数字列右对齐好让小数点对齐着读。
// 但 markdown 没有“这一列是数字”的语法，`.n` 在设计稿里是手写的类名 —— 落到渲染器里
// 只能靠列内容推断。宁可漏判（左对齐，读起来只是不整齐）也不要误判
// （把一列说明文字推到右边，整张表看起来就歪了）。

// 一个单元格“是个数”：可选的货币/正负号前缀，数字本体（允许千分位空格、下划线、逗号、
// 小数），可选单位后缀。单位只收真正会出现在度量里的那些，不收任意字母 ——
// `1a` 这种是标识符，不是数字。
const NUMERIC = /^[¥$€£]?\s*[+−\-±]?\d[\d,_ ]*(?:\.\d+)?\s*(?:%|‰|µs|ms|ns|s|min|h|d|w|k|K|M|B|GB|MB|KB|px|x|×)?$/
// 空单元 / 连字符占位（`—`、`-`、`n/a`）不参与判定，也不否决整列
const BLANK = /^(?:[-—–~]+|n\/a|null|)$/i

export function isNumericCell(text: string): boolean {
  const s = text.replace(/\s+/g, ' ').trim()
  if (BLANK.test(s)) return false
  return NUMERIC.test(s)
}

export function isBlankCell(text: string): boolean {
  return BLANK.test(text.replace(/\s+/g, ' ').trim())
}

/**
 * 逐列判定“是不是数字列”。rows[0] 是表头行。
 * 规则：该列至少有一个数字单元，且其余单元也都是数字或空白占位。
 * 只有一列也被判成数字时不影响观感（表里就那么几列），故不做“至少两列”的额外限制。
 */
export function numericColumnFlags(rows: string[][]): boolean[] {
  const cols = rows.reduce((n, r) => Math.max(n, r.length), 0)
  const flags: boolean[] = []
  for (let c = 0; c < cols; c++) {
    const cells = rows.slice(1).map((r) => r[c] ?? '')
    const nums = cells.filter((s) => isNumericCell(s)).length
    const rest = cells.every((s) => isNumericCell(s) || isBlankCell(s))
    flags.push(nums > 0 && rest)
  }
  return flags
}

// ---- hast 侧：把判定结果写回单元格 -------------------------------------------
// 这里只做「给定一张 hast 表，给数字列打上 .n」。遍历整棵树找 table 的部分留在
// markdown.tsx 的 rehype 插件里（那里才有 Root 类型与其它插件的上下文）。

type HastNode = {
  type: string
  tagName?: string
  value?: string
  properties?: Record<string, unknown>
  children?: HastNode[]
}

const textOf = (node: HastNode): string => {
  let s = node.type === 'text' ? node.value ?? '' : ''
  for (const c of node.children ?? []) s += textOf(c)
  return s
}

export function markTableNumbers(table: HastNode): void {
  const rows: HastNode[] = []
  const collectRows = (node: HastNode) => {
    for (const child of node.children ?? []) {
      if (child.type !== 'element') continue
      if (child.tagName === 'tr') rows.push(child)
      else collectRows(child)
    }
  }
  collectRows(table)
  const cellsOf = (tr: HastNode) => (tr.children ?? []).filter((c) => c.type === 'element' && (c.tagName === 'th' || c.tagName === 'td'))
  const flags = numericColumnFlags(rows.map((tr) => cellsOf(tr).map(textOf)))
  for (const tr of rows) {
    cellsOf(tr).forEach((cell, i) => {
      if (!flags[i]) return
      // hast 的 className 正常是数组；原始 HTML 表格经过 sanitize 也可能是字符串，两种都收
      const cls = cell.properties?.className
      const list = typeof cls === 'string' ? cls.split(/\s+/).filter(Boolean) : Array.isArray(cls) ? cls.map(String) : []
      if (!list.includes('n')) list.push('n')
      cell.properties = { ...(cell.properties ?? {}), className: list }
    })
  }
}
