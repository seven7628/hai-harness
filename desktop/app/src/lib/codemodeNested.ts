// codemode 宿主渲染的纯逻辑：嵌套调用清单取用 + 脚本输出的「视觉行」预览 + spill 路径。
//
// 为什么单独成文件（而不是写在组件/store 里）：这些都是**可判定的数据变换**，与
// React/DOM 无关 —— 抽出来后能被 node --test 直接覆盖（src/lib/codemodeNested-test.ts），
// 不依赖图形环境。store 只做「事件字段 → 块字段」的搬运，组件只做「块字段 → DOM」。
//
// 三条硬要求（设计文档 §17「交互与渲染（宿主侧）」+ §20.3 必须有的不变量）：
//   1. 嵌套调用**不进独立工具行**（pi renderer.ts:69-94 的 `if (event.parentToolCallId) break`）：
//      吞掉是 store 的职责（见 useAppStore 的 tool_start/tool_response），本文件只负责把
//      外层结果带的 nested_calls 摘要渲染成行内清单；
//   2. 折叠态只显示**最后 8 条** + 一行 "…N earlier calls"，args 预览截断 200 字符、
//      错误截断 500 字符（对齐 pi ARGS_PREVIEW_CHARS / ERROR_PREVIEW_CHARS，execute.ts:39-40）；
//   3. 输出预览按**渲染后的视觉行**限行（折叠态 5 行）—— 脚本输出常是一行几十 KB 的
//      minified JSON，按 \n 数「行」会当场炸屏（pi 踩过：CHANGELOG.md:39；
//      不变量「单行输出不炸屏」= §20.3）。

/** 折叠态显示的嵌套调用条数（对齐 pi renderer.ts 的「最后 8 条」）。 */
export const NESTED_COLLAPSED_LIMIT = 8

/** 单条嵌套调用 args 预览的字符上限（pi ARGS_PREVIEW_CHARS）。 */
export const NESTED_ARGS_PREVIEW_CHARS = 200

/** 单条嵌套调用错误文本的预览上限（pi ERROR_PREVIEW_CHARS）。 */
export const NESTED_ERROR_PREVIEW_CHARS = 500

/** codemode 结果**折叠态**的输出预览行数（视觉行，不是逻辑行）。 */
export const OUTPUT_PREVIEW_LINES = 5

/** 编排型工具名（渲染分支判据之一；未落地的工具不会带 nested_calls，故另需名字兜底）。 */
export const CODEMODE_TOOL_NAME = 'codemode'

/**
 * 工具名是否是编排型工具（codemode）。判据取「包含」而非精确相等：codemode 工具本身
 * 还在 Wave 2（tools/builtin/codemode/），脚本执行/委派形态下名字可能带前后缀
 * （如 `codemode_run`）；写死精确名字会让渲染在改名的瞬间**静默**失效。
 */
export function isCodemodeTool(name: string): boolean {
  return name.toLowerCase().includes(CODEMODE_TOOL_NAME)
}

/**
 * 折叠态是否该渲染脚本输出预览：有结果，且看起来是编排型工具的输出
 *（名字命中 / 带 spill 路径 / 带嵌套清单 —— 后两者是编排型工具的独有字段）。
 */
export function hasScriptOutput(b: { name: string; result?: string; spillPath?: string; nestedCalls?: NestedCallView[] }): boolean {
  if (!b.result) return false
  return isCodemodeTool(b.name) || Boolean(b.spillPath) || Boolean(b.nestedCalls && b.nestedCalls.length > 0)
}

// —— 事件字段 → 视图对象 ——————————————————————————————————————————————

/** 一条嵌套调用的展示视图（core.NestedCallRecord 的宿主侧投影，字段已归一化）。 */
export interface NestedCallView {
  id: string
  name: string
  /** 引擎已按 8 KiB 截断的**原文**参数（渲染前再过一次 200 字符预览） */
  args: string
  /** 引擎已按 8 KiB 截断的结果原文 */
  result: string
  isError: boolean
  /** 引擎已按 500 字符截断的错误文本 */
  error: string
  /** 纳秒 → 毫秒（Go 的 time.Duration 序列化成纳秒整数） */
  durMs?: number
}

// 不落 Usage：嵌套用量已由引擎并入外层 ToolResult.Usage（IMPLEMENTATION-SPEC §6.1），
// 工具行的用量 chip 显示的就是含嵌套的总量 —— 再逐条展示会让人以为要两处相加。

/**
 * 解析 events.ToolResponse.NestedCalls（JSON 字段名 = core.NestedCallRecord 的 tag）。
 * 逐项校验并按需丢弃：无名记录无信息量（脏数据/未来演进），缺失即 undefined。
 */
export function parseNestedCalls(raw: unknown): NestedCallView[] | undefined {
  if (!Array.isArray(raw) || raw.length === 0) return undefined
  const out: NestedCallView[] = []
  for (const it of raw) {
    if (!it || typeof it !== 'object') continue
    const o = it as { id?: unknown; name?: unknown; args?: unknown; result?: unknown; is_error?: unknown; error?: unknown; duration?: unknown }
    const name = String(o.name ?? '')
    if (!name) continue
    const ns = typeof o.duration === 'number' ? o.duration : Number(o.duration ?? 0)
    out.push({
      id: String(o.id ?? ''),
      name,
      args: String(o.args ?? ''),
      result: String(o.result ?? ''),
      isError: o.is_error === true,
      error: String(o.error ?? ''),
      ...(Number.isFinite(ns) && ns > 0 ? { durMs: ns / 1e6 } : {}),
    })
  }
  return out.length ? out : undefined
}

/** 折叠/展开时要渲染的条目。 */
export interface NestedCallsPick {
  /** 保持时间序（原序）；折叠态 = 最后 limit 条 */
  visible: NestedCallView[]
  /** 被折叠掉的**较早条数**（折叠态 "…N earlier calls" 的 N；展开态恒 0） */
  hidden: number
}

/**
 * 折叠态取最后 limit 条（+ 被折叠的条数），展开态取全部。
 * 折叠的是**头部**而不是尾部：脚本内的调用序列里，最后几条最接近「脚本最终做了什么」，
 * 也正是出错时最需要看的部分。
 */
export function pickNestedCalls(calls: NestedCallView[], collapsed: boolean, limit: number = NESTED_COLLAPSED_LIMIT): NestedCallsPick {
  const n = Math.max(0, Math.floor(limit))
  if (!collapsed || calls.length <= n) return { visible: calls, hidden: 0 }
  const visible = calls.slice(calls.length - n)
  return { visible, hidden: calls.length - visible.length }
}

// —— 单行预览（args / 错误文本） ——————————————————————————————————————

export interface CharsPreview {
  text: string
  truncated: boolean
}

/** 单行化：换行与连续空白压成一个空格（工具行的一行摘要必须是**一行**）。 */
export function oneLine(text: string): string {
  return text.replace(/\s+/g, ' ').trim()
}

/** 按字符数截断（超限补省略号）。代理对不会被劈开（劈开会产生半个字符的乱码）。 */
export function previewChars(text: string, max: number): CharsPreview {
  const n = Math.floor(max)
  if (n <= 0) return { text: '', truncated: text.length > 0 }
  if (text.length <= n) return { text, truncated: false }
  return { text: safeSlice(text, n) + '…', truncated: true }
}

/** 不劈开代理对的前缀切片。 */
function safeSlice(text: string, n: number): string {
  if (n <= 0) return ''
  if (n >= text.length) return text
  const prev = text.charCodeAt(n - 1)
  return prev >= 0xd800 && prev <= 0xdbff ? text.slice(0, n - 1) : text.slice(0, n)
}

// —— 视觉行预览（脚本输出） ————————————————————————————————————————————

/**
 * 一个码点占几个字符单元格：东亚宽字符 = 2，组合记号 = 0，其余 = 1。
 * 与终端 width（wcwidth/string-width）同口径 —— 等宽字体下 CJK 正好占两格，
 * 按「字符个数」估行宽的话，一行中文会被低估一半、预览少显示一半内容。
 */
export function charCells(cp: number): number {
  if (cp === 0) return 0
  if (cp >= 0x0300 && cp <= 0x036f) return 0 // 组合记号（重音等）
  if (cp === 0x200b || cp === 0x200c || cp === 0x200d || cp === 0xfeff) return 0 // 零宽
  return isWideCodePoint(cp) ? 2 : 1
}

function isWideCodePoint(cp: number): boolean {
  return (
    (cp >= 0x1100 && cp <= 0x115f) || // 韩文字母
    (cp >= 0x2e80 && cp <= 0x303e) || // CJK 部首 / 康熙部首 / 注音 / CJK 符号
    (cp >= 0x3041 && cp <= 0x33ff) || // 假名 → CJK 兼容
    (cp >= 0x3400 && cp <= 0x4dbf) || // CJK 扩展 A
    (cp >= 0x4e00 && cp <= 0x9fff) || // CJK 统一表意
    (cp >= 0xa000 && cp <= 0xa4cf) || // 彝文
    (cp >= 0xac00 && cp <= 0xd7a3) || // 韩文音节
    (cp >= 0xf900 && cp <= 0xfaff) || // CJK 兼容表意
    (cp >= 0xfe30 && cp <= 0xfe6f) || // CJK 兼容形式
    (cp >= 0xff00 && cp <= 0xff60) || // 全角形式
    (cp >= 0xffe0 && cp <= 0xffe6) || // 全角符号（￥￦ 等）
    (cp >= 0x1f300 && cp <= 0x1faff) || // emoji
    (cp >= 0x20000 && cp <= 0x3fffd) // CJK 扩展 B 及以上
  )
}

/**
 * 制表位宽度（CSS 的 tab-size 默认 8）。\t 不是「1 个单元格」而是补齐到下一个 8 格边界 ——
 * 按 1 格算会**低估**折行数，多出来的行会被 CSS 的 max-height 悄悄裁掉（用户看不见、
 * 也没有任何提示）。脚本输出里制表符很常见（ls/ps 之类），这条必须与浏览器同口径。
 */
const TAB_STOP = 8

/** 文本的单元格宽度（含制表位对齐）。 */
export function textCells(text: string): number {
  let n = 0
  for (const ch of text) {
    if (ch === '\t') {
      n += TAB_STOP - (n % TAB_STOP)
      continue
    }
    n += charCells(ch.codePointAt(0) ?? 0)
  }
  return n
}

/**
 * 逻辑行切分：\r\n / \r 归一化，末尾换行视为**行终止符**（'\n' 结尾的输出是 1 行不是 2 行）。
 * 空串 = 0 行。
 */
function logicalLines(text: string): string[] {
  if (!text) return []
  const lines = text.replace(/\r\n?/g, '\n').split('\n')
  if (lines.length > 1 && lines[lines.length - 1] === '') lines.pop()
  return lines
}

/** 文本在「每行 width 个单元格」的容器里占用的视觉行数（空行也占 1 行）。 */
export function visualLineCount(text: string, width: number): number {
  const w = Math.max(1, Math.floor(width))
  return logicalLines(text).reduce((sum, l) => sum + wrappedLines(l, w), 0)
}

function wrappedLines(line: string, w: number): number {
  return Math.max(1, Math.ceil(textCells(line) / w))
}

export interface VisualLinePreview {
  /** 预览文本：**至多** lines 个视觉行（被截断时是原文的前缀，行内可能被折断） */
  text: string
  /** 原文还有没显示出来的内容 */
  truncated: boolean
  /** 预览自身占用的视觉行数（≤ lines） */
  visualLines: number
  /** 未显示的视觉行数（截断态 > 0；供 UI 明示「藏了多少」） */
  hiddenVisualLines: number
}

/**
 * 按**视觉行**限行（= 先按容器宽度折行，再数行）。
 *
 * width = 每视觉行放得下的字符单元格数（由渲染层量得，见 ToolOutputPreview 的探针）。
 * 折叠态传 lines = OUTPUT_PREVIEW_LINES：一行 100 KB 的 minified JSON 只截出前 5 行
 * 宽度，而不是把整行原样交给 DOM（§20.3「单行输出不炸屏」）。
 *
 * 截断点落在行内：尾部省略号不加在文本里 —— 藏了多少行由 hiddenVisualLines 交给 UI
 * 单独表达（pi 的教训：折叠预览会把尾部的截断通知一起藏掉，所以路径必须另起一行）。
 */
export function previewVisualLines(text: string, opts: { lines: number; width: number }): VisualLinePreview {
  const budget = Math.max(1, Math.floor(opts.lines))
  const w = Math.max(1, Math.floor(opts.width))
  const lines = logicalLines(text)
  if (lines.length === 0) return { text: '', truncated: false, visualLines: 0, hiddenVisualLines: 0 }
  const total = lines.reduce((sum, l) => sum + wrappedLines(l, w), 0)
  const out: string[] = []
  let used = 0
  let truncated = false
  for (const line of lines) {
    const need = wrappedLines(line, w)
    if (used + need <= budget) {
      out.push(line)
      used += need
      continue
    }
    // 整行放不下：按**剩余整行预算**切字符（长行在这里被折断），后面的行全部折叠
    const left = budget - used
    if (left > 0) {
      out.push(cutByCells(line, left * w))
      used += left
    }
    truncated = true
    break
  }
  return {
    text: out.join('\n'),
    truncated,
    visualLines: used,
    hiddenVisualLines: Math.max(0, total - used),
  }
}

/** 取前缀，使宽度 ≤ cells（不劈开代理对；组合记号跟随其基字符；制表符按制表位算）。 */
function cutByCells(text: string, cells: number): string {
  let used = 0
  let end = 0
  for (const ch of text) {
    const c = ch === '\t' ? TAB_STOP - (used % TAB_STOP) : charCells(ch.codePointAt(0) ?? 0)
    if (used + c > cells) break
    used += c
    end += ch.length
  }
  return text.slice(0, end)
}

// —— spill（完整输出落盘）路径 ————————————————————————————————————————

/** 结果文本里点名落盘路径的既有文案（本仓 slim 先例 + pi 的 execute.ts:195）。 */
const SPILL_PATTERNS: RegExp[] = [
  /\[Full output:\s*([^\s\]]+)/, // pi：`[Full output: /tmp/pi-codemode-xxx.txt (read with offset/limit)]`
  /\[完整输出已写\s*([^\s，,、\]]+)/, // 本仓 agents/slim.go:107：`[完整输出已写 <path>，以下为前 2048 字节预览]`
  /(?:^|\n)\s*(?:Full output|完整输出)\s*[:：]\s*([^\s，,、\]]+)/,
]

/**
 * 从结果里取 spill（完整输出落盘）路径；取不到返回 ''（调用方**不显示**该行）。
 *
 * 两个通道，优先结构化：
 *   1. structured：bash 的 OutputSchemaProvider 已有 `full_output_path` 字段（codemode 的
 *      Wave 2 结果会带同类字段）—— 有就用它，不做文本猜测；
 *   2. 文本：路径是写在结果正文里的（pi 与本仓 slim 都如此），按上面的既有文案模式提取。
 *      只认**看起来像路径**的整串（无空白、以 / ~ . 或盘符开头），否则丢弃 —— 宁可不显示，
 *      也不显示一个猜出来的路径（设计文档 §17 要求「点名 spill 路径」，不是编造路径）。
 */
export function spillPathFrom(text: string, structured?: unknown): string {
  const explicit = structuredSpillPath(structured)
  if (explicit) return explicit
  for (const re of SPILL_PATTERNS) {
    const m = re.exec(text)
    const p = normalizePath(m?.[1])
    if (p) return p
  }
  return ''
}

function structuredSpillPath(s: unknown): string {
  if (!s || typeof s !== 'object') return ''
  const o = s as { full_output_path?: unknown; spill_path?: unknown; fullOutputPath?: unknown }
  return normalizePath(String(o.full_output_path ?? o.spill_path ?? o.fullOutputPath ?? ''))
}

function normalizePath(raw?: string): string {
  const p = (raw ?? '').trim().replace(/^[`'"]+/, '').replace(/[`'"。；;，,)\]]+$/, '')
  if (!p || p.length > 4096 || /\s/.test(p)) return ''
  // 「像路径」的判据取三者之一：分隔符 / 盘符 / 文件扩展名。
  // 只认绝对路径会漏掉本仓 slim 先例的相对落盘名（`spill-<runId>-<n>.txt`，
  // agents/slim.go:99）；完全不校验则会把「Full output: 见上文」这类说明文字
  // 当成路径显示出来 —— 宁可少显示（不显示），不可编造。
  if (!/[/\\]|[A-Za-z]:|\.\w{1,8}$/.test(p)) return ''
  return p
}
