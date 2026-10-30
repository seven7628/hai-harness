// bash 运行中尾巴（tool_output）的**行级语义**：设计（v2 §18.2 Live output）给每行两样东西 ——
// 一个固定宽度的刻度槽（`+1.02` 秒）和一段正文，正文按行首标记取级色（PASS/FAIL/WARN/RUN）。
//
// 为什么单独抽出来：这是唯一的"从纯文本里读语义"的地方，它错就会整块染色错
// （把正常结果里出现的 "Error:" 染成红）。所以规则定死三条，并用单测钉住：
//  1. 只有**行首**的标记算数（前面允许时间戳与空白），句子中间出现的关键词一律不染色；
//  2. 标记是**独立 token**（后面跟空白或行尾）—— `FAILED_BUILD=1` 这种不是标记；
//  3. 不认识的行原样输出（`plain`），不做任何猜测。
//
// 刻度槽只在行首真的有时间戳时才占位（`+1.02`），否则留空 —— 空槽比错位的槽诚实。

export type TailKind = 'ok' | 'warn' | 'err' | 'dim' | 'plain'

export interface TailLine {
  /** 刻度槽内容（`+1.02`），没有就是空串 */
  gut: string
  /** 正文（已去掉刻度槽） */
  text: string
  /** 级色：行首标记决定 */
  kind: TailKind
}

/** 行首标记 → 级色。刻意只认这几个，多一个都会把普通输出染花。 */
const MARKERS: Record<string, TailKind> = {
  PASS: 'ok',
  OK: 'ok',
  FAIL: 'err',
  ERROR: 'err',
  WARN: 'warn',
  RUN: 'dim',
}

const stampRe = /^(\+\d+(?:\.\d+)?)\s+/
// 测试运行器的行首前缀：`--- PASS: …` / `=== RUN …` / `?   pkg [no test files]`
// 这三类前缀后面紧跟的就是状态词，属于**行首**语义（设计里这些行也是绿/灰的）。
// 但只认这三种带标点的前缀 —— 否则 `wrote 3 files; Error: …` 这种也会被染。
const prefixRe = /^(?:---|===|\?)\s+/
const markerRe = /^([A-Za-z_]+)(?:[:\s]|$)/

/**
 * 把一段尾巴文本切成可渲染的行。
 *
 * @param text tool_output 的一帧（**替换**语义，不是增量）
 * @param max  最多保留多少行（尾巴只要尾部；调用方再交给 CSS 限高）
 */
export function tailLines(text: string, max = 120): TailLine[] {
  if (!text) return []
  const all = text.split('\n')
  // 末尾换行会产生一个空串：它不是一行内容，别让它在底部留出一条空行
  while (all.length > 0 && all[all.length - 1].trim() === '') all.pop()
  const lines = all.slice(-max).map((raw) => {
    let rest = raw
    let gut = ''
    const stamp = stampRe.exec(rest)
    if (stamp) {
      gut = stamp[1]
      rest = rest.slice(stamp[0].length)
    }
    const prefixed = prefixRe.exec(rest)
    const marker = markerRe.exec(prefixed ? rest.slice(prefixed[0].length) : rest)
    const kind = marker ? MARKERS[marker[1].toUpperCase()] ?? 'plain' : 'plain'
    return { gut, text: rest, kind }
  })
  return lines
}
