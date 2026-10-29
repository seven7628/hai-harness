import { test } from 'node:test'
import assert from 'node:assert/strict'
import { isBlankCell, isNumericCell, markTableNumbers, numericColumnFlags } from './tableNumbers'

test('数字单元：整数、千分位、小数、单位、货币、符号', () => {
  for (const s of ['12', '1,234', '1 234', '12.5', '42%', '12.5%', '3.5s', '120ms', '76k', '−3', '-3', '+3', '¥1,200', '$1,200', '€0.42', '2h', '800MB']) {
    assert.equal(isNumericCell(s), true, s)
  }
})

test('非数字单元：标识符、版本号、句子、带字母的数字', () => {
  for (const s of ['abc', '1a', 'v1.2', 'GPT-4', '3 calls · 2 fails', 'run0', 'SIGTERM']) {
    assert.equal(isNumericCell(s), false, s)
  }
})

test('空白占位不算数字，但也不否决整列', () => {
  assert.equal(isBlankCell(''), true)
  assert.equal(isBlankCell('—'), true)
  assert.equal(isBlankCell('n/a'), true)
  assert.equal(isNumericCell('—'), false)
})

test('整列判定：表头行不参与，全数字才算', () => {
  const rows = [
    ['调用', '耗时', '说明'],
    ['3', '1.2s', 'ok'],
    ['12', '880ms', 'retry'],
  ]
  assert.deepEqual(numericColumnFlags(rows), [true, true, false])
})

test('一列里混进一句说明就不是数字列（宁可左对齐，不要整张表变歪）', () => {
  const rows = [
    ['值', '备注'],
    ['1', 'fast'],
    ['n/a', 'slow'],
  ]
  assert.deepEqual(numericColumnFlags(rows), [true, false])
  assert.deepEqual(numericColumnFlags([['值'], ['1'], ['两']]), [false])
})

test('空表 / 只有表头：没有数字列', () => {
  assert.deepEqual(numericColumnFlags([]), [])
  assert.deepEqual(numericColumnFlags([['a', 'b']]), [false, false])
})

// ---- hast 侧 ----------------------------------------------------------------
const cell = (tagName: string, text: string, className?: unknown) => ({
  type: 'element',
  tagName,
  properties: className === undefined ? {} : { className },
  children: [{ type: 'text', value: text }],
})
const row = (tagName: string, cells: ReturnType<typeof cell>[]) => ({ type: 'element', tagName, children: cells })
const table = (rows: ReturnType<typeof row>[]) => ({
  type: 'element',
  tagName: 'table',
  properties: {},
  children: [{ type: 'element', tagName: 'thead', children: [rows[0]] }, { type: 'element', tagName: 'tbody', children: rows.slice(1) }],
})

test('markTableNumbers：数字列两侧都打 .n（th 与 td），文字列不动', () => {
  const t = table([row('tr', [cell('th', '调用'), cell('th', '耗时')]), row('tr', [cell('td', '3'), cell('td', '1.2s')]), row('tr', [cell('td', '12'), cell('td', '880ms')])])
  markTableNumbers(t)
  const [, body] = t.children
  const [h1, h2] = t.children[0].children[0].children
  const [d1, d2] = body.children[0].children
  assert.deepEqual(h1.properties.className, ['n'])
  assert.deepEqual(h2.properties.className, ['n'])
  assert.deepEqual(d1.properties.className, ['n'])
  assert.deepEqual(d2.properties.className, ['n'])
})

test('markTableNumbers：已有类名保留，不重复加 .n', () => {
  const t = table([row('tr', [cell('th', '值', 'n'), cell('th', '备注')]), row('tr', [cell('td', '1'), cell('td', 'ok')])])
  markTableNumbers(t)
  const [h1, h2] = t.children[0].children[0].children
  assert.deepEqual(h1.properties.className, ['n'])
  assert.equal(h2.properties.className, undefined)
})

test('markTableNumbers：字符串形式的 className 也能吃（原始 HTML 表格）', () => {
  const t = table([row('tr', [cell('th', '值', 'x'), cell('th', '备注')]), row('tr', [cell('td', '1'), cell('td', 'ok')])])
  markTableNumbers(t)
  const [h1] = t.children[0].children[0].children
  assert.deepEqual(h1.properties.className, ['x', 'n'])
})
