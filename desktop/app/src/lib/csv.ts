// CSV/TSV → GFM 表格（右栏「最终文件」的表格预览）。
//
// 为什么不自己画 <table>：项目已有 react-markdown + remark-gfm（对话区与 Markdown 文件
// 预览共用同一条管线），GFM 表格的表头、边框、宽表横滚（.md-table-wrap）都已就位 ——
// 复用后 CSV 预览与 Markdown 文件里的表格**观感天然一致**，且不引入任何新依赖。
// 于是本模块只干三件事：① 解析（RFC 4180）② 分隔符推断 ③ 转义并拼成 markdown 表格。
//
// 纯函数模块（无 React / 无 store）→ 便于 scripts/test-csv.mjs 直接 esbuild 打包单测
// （与 store/trace.ts 的单测方式一致）。UI 位置见 components/CsvPreview.tsx。

// 行/列上限：这三个数字是「防卡死」的硬闸，不是审美取舍。
// 实测（本机 Node + React 生产版 SSR，跑的就是同一条 remark-gfm 管线；浏览器里还要再加
// 一次 DOM 提交，量级同阶）：**耗时随单元格总数线性增长**，与「行数」几乎无关 ——
//   6000 格（500×12 或 2000×3）≈ 650ms，10000 格 ≈ 1.7s，20000 格 ≈ 6.5s
// 所以只限行数是不够的：500 行 × 200 列的导出表有十万级节点，必卡。故**双向 + 总量**三重设闸：
//   ① 列 ≤ 40（超出提示「只显示前 N 列」）
//   ② 行 ≤ 500，且 ③ 总格数 ≤ 8000 → 宽表按 ③ 进一步收敛行数（宽表少显示几行，好过整页卡住）
// 收敛结果经 CsvTable.hiddenRows / wide 如实告知用户，不静默丢弃。
export const MAX_TABLE_ROWS = 500
export const MAX_TABLE_COLS = 40
export const MAX_TABLE_CELLS = 8000

/** .csv/.tsv 判定（CSV 表格视图的唯一开关：ArtifactCard 主按钮与 RightPanel 渲染分支共用）。 */
export function isCsvPath(p: string): boolean {
  const i = p.lastIndexOf('.')
  if (i <= 0) return false
  const ext = p.slice(i + 1).toLowerCase()
  return ext === 'csv' || ext === 'tsv'
}

/** 扩展名 → 首选分隔符（.tsv 是制表符；其余按逗号，再靠首行推断兜底）。 */
export function delimiterForPath(path: string): string {
  return path.toLowerCase().endsWith('.tsv') ? '\t' : ','
}

export interface CsvTable {
  /** 实际使用的分隔符：',' | '\t' | ';'（来自扩展名或首行推断） */
  delimiter: string
  header: string[]
  /** 已按 cols 对齐、并截断到行上限的数据行 */
  rows: string[][]
  /** 表格列数（全表最宽记录的行字段数，受列上限约束） */
  cols: number
  /** 数据行总数（不含表头；**未**受行上限影响，用于「还有 M 行」） */
  dataRows: number
  /** 因行上限未显示的行数 */
  hiddenRows: number
  /** 存在字段数与最宽记录不一致的行（脏数据：多列/少列） */
  ragged: boolean
  /** 存在单元格内含换行（展示时折叠为 ↵） */
  multiline: boolean
  /** 列数超上限被裁 */
  wide: boolean
}

// 换行统一 + BOM 剥离（文件开头的 BOM 会黏进首个表头单元格，Excel 导出的 CSV 很常见：
// 表头显示成 "\uFEFF姓名"，肉眼看不见却导致按名字取列全部落空）。
function normalizeInput(text: string): string {
  return text.replace(/^\uFEFF/, '').replace(/\r\n?/g, '\n')
}

// RFC 4180 状态机解析。**不要用 split(',') / split('\n') 之类玩具实现**，真实 CSV 里：
//   "a,b",c        字段内的逗号（被引号包裹）→ 必须解析为 2 个字段，不是 3 个
//   "多\n行",x     字段内的换行（引号包裹的多行字段）→ 不能按行切
//   "他说""好""",y 转义引号（"" = 一个字面引号）
//   "未闭合,x      引号未闭合（脏数据/被截断）→ 不崩，按「一直引用到末尾」收尾
// 引号只在字段**起始位**才是引用开始（Excel 同款宽容：a"b 里的引号是字面量）。
export function parseDelimited(text: string, delimiter: string): string[][] {
  const rows: string[][] = []
  let row: string[] = []
  let field = ''
  let quoted = false
  let started = false // 本行是否已有内容：区分「空文件」与「空行」（末尾换行不该多出一行）
  for (let i = 0; i < text.length; i++) {
    const c = text[i]
    if (quoted) {
      if (c === '"') {
        if (text[i + 1] === '"') { field += '"'; i++ } // "" → 一个字面引号
        else quoted = false
      } else {
        field += c // 引号内的分隔符与换行都属字段内容
      }
      started = true
      continue
    }
    if (c === '"' && field === '') { quoted = true; started = true; continue }
    if (c === delimiter) { row.push(field); field = ''; started = true; continue }
    if (c === '\n') { row.push(field); rows.push(row); row = []; field = ''; started = false; continue }
    field += c
    started = true
  }
  if (started || row.length > 0) { row.push(field); rows.push(row) }
  // 空行不算记录：与 Go 标准库 encoding/csv 一致（Blank lines are skipped），
  // 也让「末尾多一个空行」这种常见文件不必背着「列数不一致」的假警报。
  return rows.filter((r) => !(r.length === 1 && r[0] === ''))
}

// 首行分隔符推断：**只扫首条记录**（引号内的分隔符不计），首个出现的候选者胜出。
// 为什么需要：扩展名与真实内容并不总是一致（.csv 实际是制表符 / 分号分隔，
// 欧洲区 Excel 导出的分号 CSV 很常见）——按首选分隔符切会发现「整行只有一列」，
// 表格预览就退化成一个巨大的单列表格。候选顺序 = 首选优先，故正常文件的行为不变。
export function pickDelimiter(text: string, primary = ','): string {
  const cands = primary === '\t' ? ['\t', ',', ';'] : [',', '\t', ';']
  const counts = new Map<string, number>(cands.map((c) => [c, 0]))
  let quoted = false
  for (let i = 0; i < text.length; i++) {
    const c = text[i]
    if (quoted) {
      if (c === '"') { if (text[i + 1] === '"') i++; else quoted = false }
      continue
    }
    if (c === '"') { quoted = true; continue }
    if (c === '\n') break
    const n = counts.get(c)
    if (n !== undefined) counts.set(c, n + 1)
  }
  for (const c of cands) if ((counts.get(c) ?? 0) > 0) return c
  return primary
}

/** 补齐/裁剪到 n 列（保持输入数组不变，调用方常复用 header）。 */
function fit(cells: string[], n: number): string[] {
  const out = cells.slice(0, n)
  while (out.length < n) out.push('')
  return out
}

/**
 * 文本 → 表格模型。空文件返回 null（调用方给「文件为空」提示）。
 *
 * 表头取舍：**首行一律当表头**。CSV 没有标准说「首行必须是表头」，也没有可靠的判据
 * （真去猜「这行像不像表头」必然出错），故统一按表头处理并在 UI 上写明
 * （rp.file.csvHeaderNote）——猜错的代价远高于说明白的代价。
 */
export function buildCsvTable(text: string, path: string): CsvTable | null {
  const src = normalizeInput(text)
  const delimiter = pickDelimiter(src, delimiterForPath(path))
  const records = parseDelimited(src, delimiter)
  if (records.length === 0) return null

  const header = records[0]
  const body = records.slice(1)
  // 列数 = 全表最宽记录的字段数（表头比数据短/长都按最宽的对齐，脏数据不丢列），再受列上限约束。
  // 注意不能用 Math.max(...arr)：十万行的表会因参数展开过多而栈溢出（实测 RangeError）。
  let widest = header.length
  for (const r of body) if (r.length > widest) widest = r.length
  const cols = Math.max(1, Math.min(widest, MAX_TABLE_COLS))
  const wide = widest > cols

  // 脏数据判定按**真实最宽值**（widest）比，而不是裁过的 cols —— 否则宽表里每一行
  // 都会被误判成「不一致」。
  let ragged = header.length !== widest
  for (const r of body) if (r.length !== widest) { ragged = true; break }

  // 行闸 = min(500, 总格数预算/列数)：宽表自动少渲染几行（见上方实测）。
  const rowCap = Math.min(MAX_TABLE_ROWS, Math.floor(MAX_TABLE_CELLS / cols))
  const hiddenRows = Math.max(0, body.length - rowCap)
  const shown = hiddenRows > 0 ? body.slice(0, rowCap) : body
  let multiline = header.some((c) => c.includes('\n'))
  for (const r of shown) { if (r.some((c) => c.includes('\n'))) { multiline = true; break } }

  return {
    delimiter,
    header: fit(header, cols),
    rows: shown.map((r) => fit(r, cols)),
    cols,
    dataRows: body.length,
    hiddenRows,
    ragged,
    multiline,
    wide,
  }
}

// —— 单元格 → markdown 字面量（本模块最关键的一步）——
//
// **必须逐字符转义**：CSV 是「数据」，markdown 表格是「语法」。把单元格原样拼进表格行时，
// 任何一个字符都可能被解析成结构，而不是内容（实测，都是真实 CSV 里跑出来的）：
//   a|b            → 该行被切成多余单元格，整行右移，后续行全部错位（表格被撑坏）
//   *x*  _x_  ~x~  → 变成粗体/斜体/删除线（数据被「格式化」）
//   反引号          → 变成行内代码
//   [x](url)       → 变成链接
//   <b>hi</b>      → 未开启 raw HTML，会显示成字面量；但仍转义，避免将来开启 rehype-raw 后复活
//   &copy;         → 被当 HTML 实体解成 ©（数据被改写）
//   $x$  $PATH:$HOME → 被 KaTeX 吞成公式（对话区 Markdown 管线挂了 remark-math；
//                      markdown.tsx 的 remarkMathGuard 只挡「定界符内侧带空白」的假阳性）
// 顺序很重要：先转义 `\` 自身，否则 `C:\Users` 里的 `\U` 会与随后插入的转义符串味
// （先补 `\\` 后补 `\|`，`\` 才稳定显示为一个反斜杠）。
// 换行：GFM 单元格内不能有真换行（实测：含 \n 的行会被拆成两行 → 列数错乱），
// 且 raw HTML 未开启（写 <br> 也只会显示字面量）→ 统一折叠成可见的 ↵（U+21B5）。
const MD_ESCAPE = /[\\|`*_[\]~<&$]/g
// 控制字符（NUL/退格/垂直制表…）在表格里毫无意义，且会渲染成「替换符」糊在单元格里 → 直接删。
// 写成「按码点判定」而不是控制字符正则字面量：字符类里的裸控制字符不可读（复制粘贴易丢），
// 且会触发 ESLint no-control-regex（这正是本条被改写的原因）。
const isControlChar = (c: string): boolean => {
  const n = c.charCodeAt(0)
  return n < 0x20 || n === 0x7f // 换行已在上一行折叠成 ↵，不会走到这里
}

export function escapeCell(v: string): string {
  return v
    .replace(/\r\n?|\n/g, '↵')
    .split('').filter((c) => !isControlChar(c)).join('')
    .replace(MD_ESCAPE, (c) => '\\' + c)
}

/** 表格模型 → GFM 表格 markdown（交给既有的 Markdown 组件渲染）。 */
export function toGfmTable(t: CsvTable): string {
  const row = (cells: string[]) => `| ${cells.map(escapeCell).join(' | ')} |`
  // 表头必须补到 cols：GFM 以分隔行的列数决定表格宽度，表头缺列会让后续行多出的单元格被丢掉。
  const lines = [row(t.header), `|${' --- |'.repeat(t.cols)}`]
  for (const r of t.rows) lines.push(row(r))
  return lines.join('\n')
}
