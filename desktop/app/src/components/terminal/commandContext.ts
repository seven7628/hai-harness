// [recovered] from /private/tmp/term-context.test.cjs（模块标记 `// src/components/terminal/commandContext.ts`）
// [implemented] 依据 docs/TERMINAL_DESIGN.md §7（「引用屏幕」要带上是哪条命令、在哪个目录跑的）：
//   切片里五个符号都没有 export、类型全丢。本文件补导出与类型标注，**逻辑逐字未改**；
//   移除 @ts-nocheck。
//
// 纯函数集合（无 DOM / 无 store / 无 transport）：从「终端输入字节」里跟出用户刚提交的那条
// 命令，再决定引用整屏时要不要补一行 `$ cmd`。之所以要跟字节而不是读 xterm 缓冲区：
// 缓冲区里混着提示符、补全、彩色转义，反推命令等于重新实现一个 shell 解析器。

/** 命令行跟踪状态：当前行内容 + 「这行被 shell 改过（补全/方向键）」的脏标记。 */
export interface CommandLineState {
  line: string
  dirty: boolean
}

export const emptyCommandLine: CommandLineState = { line: '', dirty: false }

/**
 * 喂一段输入字节，返回新状态与（若回车提交了一条**干净**命令）那条命令。
 *
 * 脏行不提交：Tab 补全 / ESC 序列（方向键、历史）之后，界面上看到的已经不是用户敲的东西，
 * 把「用户敲的前缀」当成命令引用会张冠李戴 —— 宁可只引用屏幕内容。
 */
export function trackCommandLine(
  state: CommandLineState,
  data: string,
): { state: CommandLineState; submitted?: string } {
  let line = state.line
  let dirty = state.dirty
  let submitted: string | undefined
  for (const ch of data) {
    if (ch === '\r' || ch === '\n') {
      const cmd = line.trim()
      if (!dirty && cmd !== '') submitted = cmd
      line = ''
      dirty = false
      continue
    }
    if (ch === '\x7f' || ch === '\b') {
      // DEL / BS：只删一个字符。删不会「洗白」一行 —— dirty 保持不变。
      line = line.slice(0, -1)
      continue
    }
    if (ch === '\x15') {
      // ^U 清行
      line = ''
      dirty = false
      continue
    }
    if (ch === '\x03') {
      // ^C 中断：行没了，也**没有**提交
      line = ''
      dirty = false
      submitted = undefined
      continue
    }
    if (ch === '\t' || ch === '\x1b') {
      // Tab 补全 / ESC 序列起点：这行不干净了（后续字节可能是 `[A` 之类）
      dirty = true
      continue
    }
    if (ch >= ' ' && ch !== '\x7f') {
      line += ch
      continue
    }
    dirty = true // 其余控制字节：不认识，但一定不是用户直接敲的字面内容
  }
  return { state: { line, dirty }, submitted }
}

/** 正文里没有这条命令时，补一行 `$ cmd`（有目录再补 `   (in dir)`）+ 一个空行。 */
export function withCommandHeader(body: string, command?: string, dir?: string): string {
  const cmd = (command ?? '').trim()
  if (body === '') return body
  if (cmd === '') return body
  const where = (dir ?? '').trim()
  const line = where ? `$ ${cmd}   (in ${where})` : `$ ${cmd}`
  if (body.includes(cmd)) return body // 「引用整屏」时正文里已经有提示符行，别重复
  return `${line}\n\n${body}`
}

/** cwd 展示：工作区内用相对路径（噪声最小），工作区外必须用绝对路径（相对会误导）。 */
export function describeCwd(info?: { rel?: string; cwd?: string; inside?: boolean }): string {
  if (!info) return ''
  const rel = (info.rel ?? '').trim()
  const cwd = (info.cwd ?? '').trim()
  if (info.inside) return rel
  return cwd
}

/** 从屏幕行里**从后往前**找命令所在行号（同名命令以最后一次为准）；找不到 → -1。 */
export function findCommandLine(lines: string[], command?: string): number {
  const cmd = (command ?? '').trim()
  if (cmd === '') return -1
  for (let i = lines.length - 1; i >= 0; i--) {
    if (lines[i].includes(cmd)) return i
  }
  return -1
}
