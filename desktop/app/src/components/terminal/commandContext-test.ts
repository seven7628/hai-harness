// [recovered] from /private/tmp/term-context.test.cjs（模块标记 `// src/components/terminal/commandContext-test.ts`）
// [implemented] 依据 docs/TERMINAL_DESIGN.md §7 + docs/TERMINAL_E2E_CONTRACTS.md「命令追踪」：
//   切线把 commandContext.ts 内联进来了；本文件改为 `import` 真模块，断言逐条保留
//   （不删不改）。切线里被吃掉的控制字符按原义写成转义序列（\x15 = ^U，\x03 = ^C，
//   \x1b = ESC，\x7f = DEL），与切线等价。
import test from 'node:test'
import assert from 'node:assert/strict'

import {
  describeCwd,
  emptyCommandLine,
  findCommandLine,
  trackCommandLine,
  withCommandHeader,
  type CommandLineState,
} from './commandContext'

/** 逐块喂输入，收集所有被提交的命令。 */
function feed(
  chunks: string[],
  start: CommandLineState = emptyCommandLine,
): { state: CommandLineState; submitted: string[] } {
  let state = start
  const submitted: string[] = []
  for (const c of chunks) {
    const step = trackCommandLine(state, c)
    state = step.state
    if (step.submitted !== undefined) submitted.push(step.submitted)
  }
  return { state, submitted }
}

test('逐字符输入 + 回车 → 提交去掉首尾空白的命令', () => {
  const { submitted, state } = feed(['n', 'p', 'm', ' ', 't', 'e', 's', 't', '\r'])
  assert.deepEqual(submitted, ['npm test'])
  assert.deepEqual(state, emptyCommandLine)
})

test('退回（DEL 0x7f）只删一个字符', () => {
  const { submitted } = feed(['l', 's', 'x', '\x7f', '\r'])
  assert.deepEqual(submitted, ['ls'])
})

test('空回车不提交（提示符上直接回车不是命令）', () => {
  const { submitted } = feed(['\r', '  ', '\r'])
  assert.deepEqual(submitted, [])
})

test('^U 清行后重新输入 → 提交的是新命令', () => {
  const { submitted } = feed(['g', 'i', 't', ' ', 'x', '\x15', 'l', 's', '\r'])
  assert.deepEqual(submitted, ['ls'])
})

test('Tab 补全 ⇒ 这一行 dirty，回车不提交命令', () => {
  const { submitted, state } = feed(['g', 'i', 't', ' ', 's', '\t', '\r'])
  assert.deepEqual(submitted, [])
  assert.equal(state.dirty, false, '回车后回到干净状态')
})

test('方向键（ESC 序列）同样 dirty', () => {
  const { submitted } = feed(['\x1b', '[', 'A', '\r'])
  assert.deepEqual(submitted, [])
})

test('dirty 之后按退格不会「洗白」：仍然不提交', () => {
  const { submitted } = feed(['n', 'p', 'm', '\t', '\x7f', '\r'])
  assert.deepEqual(submitted, [])
})

test('^C 中断后再输入的命令是干净的', () => {
  const { submitted, state } = feed(['s', 'l', 'e', 'e', 'p', ' ', '9', '\x03', 'l', 's', '\r'])
  assert.deepEqual(submitted, ['ls'])
  assert.equal(state.line, '')
})

test('多行粘贴（含 \\r）按多次提交处理', () => {
  const { submitted } = feed(['ls\rpwd\r'])
  assert.deepEqual(submitted, ['pwd'])
})

test('正文里没有这条命令 → 补一行 `$ cmd` 并空一行', () => {
  assert.equal(withCommandHeader('`boom`', 'npm test'), '$ npm test\n\n`boom`')
})

test('正文已经含这条命令（引用整屏）→ 不加头', () => {
  const body = '```\nadmin@host ~ % npm test\nboom\n```'
  assert.equal(withCommandHeader(body, 'npm test'), body)
})

test('没有命令 / 空正文 → 原样返回', () => {
  assert.equal(withCommandHeader('`boom`', undefined), '`boom`')
  assert.equal(withCommandHeader('`boom`', '   '), '`boom`')
  assert.equal(withCommandHeader('', 'npm test'), '')
})

test('命令前后空白被 trim（终端里常带尾部空格）', () => {
  assert.equal(withCommandHeader('`boom`', '  npm test  '), '$ npm test\n\n`boom`')
})

test('从后往前找：同名命令出现两次时命中最后一条', () => {
  const lines = ['% npm test', 'ok', '% npm test', 'fail']
  assert.equal(findCommandLine(lines, 'npm test'), 2)
})

test('提示符与命令同一行也能命中', () => {
  assert.equal(findCommandLine(['admin@m ~ % git status', 'clean'], 'git status'), 0)
})

test('没找到 / 没命令 → -1（调用方退回整屏）', () => {
  assert.equal(findCommandLine(['a', 'b'], 'npm test'), -1)
  assert.equal(findCommandLine(['a'], ''), -1)
  assert.equal(findCommandLine(['a'], undefined), -1)
})

test('同一个命令 + 目录 → `$ cmd   (in dir)`', () => {
  assert.equal(withCommandHeader('`boom`', 'npm test', 'src/api'), '$ npm test   (in src/api)\n\n`boom`')
})

test('目录为空 / 未给 → 与原来一致（不写括号）', () => {
  assert.equal(withCommandHeader('`boom`', 'npm test', ''), '$ npm test\n\n`boom`')
  assert.equal(withCommandHeader('`boom`', 'npm test', '   '), '$ npm test\n\n`boom`')
})

test('正文已含命令 → 即使给了目录也不重复', () => {
  assert.equal(
    withCommandHeader('```\n% npm test\nboom\n```', 'npm test', 'src'),
    '```\n% npm test\nboom\n```',
  )
})

test('describeCwd：与工作区相同（inside 且 rel 空）→ 空串（不制造噪音）', () => {
  assert.equal(describeCwd({ cwd: '/w/proj', rel: '', inside: true }), '')
})

test('describeCwd：工作区之内 → 相对路径', () => {
  assert.equal(describeCwd({ cwd: '/w/proj/src/api', rel: 'src/api', inside: true }), 'src/api')
})

test('describeCwd：工作区之外 → 绝对路径（相对路径会误导）', () => {
  assert.equal(describeCwd({ cwd: '/Users/x/other', rel: '', inside: false }), '/Users/x/other')
})

test('describeCwd：拿不到 / undefined → 空串', () => {
  assert.equal(describeCwd(undefined), '')
  assert.equal(describeCwd({}), '')
  assert.equal(describeCwd({ inside: true, rel: '   ' }), '')
})
