// [recovered] from /private/tmp/term-format.test.cjs（模块标记 `// src/components/terminal/formatSelection-test.ts`）
// [implemented] 依据 docs/TERMINAL_E2E_CONTRACTS.md「formatSelection 裁剪与围栏」：
//   切线把 formatSelection.ts 内联进来了；本文件改为 `import` 真模块，断言逐条保留（不删不改）。
import test from 'node:test'
import assert from 'node:assert/strict'

import { formatSelection } from './formatSelection'

const bytesOf = (text: string): number => new TextEncoder().encode(text).length

test('单行、不含反引号 → 一对反引号包起来', () => {
  assert.equal(formatSelection('git status'), '`git status`')
  assert.equal(formatSelection('echo hi && pwd'), '`echo hi && pwd`')
})

test('单行但含反引号 → 原文返回，不包', () => {
  assert.equal(formatSelection('echo `date`'), 'echo `date`')
  assert.equal(formatSelection('`'), '`')
})

test('单行：前后的空白与行尾换行被 trim，结果不含首尾换行', () => {
  const out = formatSelection('  ls -la  \n')
  assert.equal(out, '`ls -la`')
  assert.equal(out.includes('\n'), false)
})

test('多行 → 三反引号围栏，围栏各自单独成行', () => {
  assert.equal(formatSelection('a\nb'), '```\na\nb\n```')
  assert.equal(formatSelection('a\nb\n\n\n'), '```\na\nb\n```', 'text 末尾的换行先去重')
})

test('多行正文保留首行缩进（缩进就是代码的语义）', () => {
  assert.equal(formatSelection('  a\n    b\n'), '```\n  a\n    b\n```')
})

test('多行且本身含三反引号 → 围栏改用三波浪线', () => {
  const out = formatSelection('x\n```\ny\n```')
  assert.equal(out, '~~~\nx\n```\ny\n```\n~~~')
})

test('空串或全空白 → 空串', () => {
  assert.equal(formatSelection(''), '')
  assert.equal(formatSelection('   '), '')
  assert.equal(formatSelection('\n\n \t\n'), '')
})

test('恰好 200 行不截断', () => {
  const text = Array.from({ length: 200 }, (_, i) => `line${i}`).join('\n')
  const out = formatSelection(text)
  assert.equal(out.includes('已截断'), false)
  assert.equal(out.split('\n').length, 202, '围栏两行 + 正文 200 行')
})

test('超过 200 行 → 截到 200 行，并注明原文行数', () => {
  const text = Array.from({ length: 201 }, (_, i) => `line${i}`).join('\n')
  const out = formatSelection(text)
  const inner = out.slice(4, -4)
  const lines = inner.split('\n')
  assert.equal(lines.length, 201, '200 行正文 + 1 行截断标记')
  assert.equal(lines[0], 'line0')
  assert.equal(lines[199], 'line199')
  assert.equal(lines[200], '… （已截断，共 201 行）')
})

test('超过 20000 字节 → 截断（超长单行从中间断）', () => {
  const out = formatSelection('a'.repeat(20001))
  const inner = out.slice(4, -4)
  const body = inner.split('\n')[0]
  assert.equal(body.length, 20000, '正文按字节上限截断')
  assert.equal(inner.endsWith('… （已截断，共 1 行）'), true, '单行也要报总行数')
  assert.equal(out.startsWith('```\n'), true)
  assert.equal(out.endsWith('\n```'), true)
})

test('恰好 20000 字节不截断（边界是「超过」）', () => {
  const out = formatSelection('a'.repeat(20000))
  assert.equal(out.includes('已截断'), false)
})

test('中文按 UTF-8 字节截断，不留半个字（U+FFFD）', () => {
  const out = formatSelection('中'.repeat(10000))
  const body = out.slice(4, -4).split('\n')[0]
  assert.equal(body.includes('\uFFFD'), false)
  assert.equal(bytesOf(body) <= 20000, true)
  assert.equal(body, '中'.repeat(6666), '整字边界：20000 字节里只放得下 6666 个汉字')
})

test('结果形状：单行结果首尾没有多余换行，多行结果首尾各恰好一个围栏行', () => {
  for (const text of ['ls', 'a\nb', 'x\n```\ny', 'a'.repeat(20001)]) {
    const out = formatSelection(text)
    if (!out.includes('\n')) {
      assert.equal(out.startsWith('\n'), false)
      assert.equal(out.endsWith('\n'), false)
      continue
    }
    const lines = out.split('\n')
    assert.equal(lines[0].startsWith('```') || lines[0].startsWith('~~~'), true, '首行是围栏')
    assert.equal(lines[lines.length - 1], lines[0], '尾行与首行同一种围栏')
    assert.equal(lines.length > 2, true, '围栏之间至少有一行内容')
  }
})
