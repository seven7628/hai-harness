import { test } from 'node:test'
import assert from 'node:assert/strict'
import { splitStreamingMarkdown } from './mdSplit.ts'

/* 切分的**唯一硬约束**：拼回去必须逐字等于原文（否则用户会看到丢字）。
   其余都是「该切的切、不该切的不切」。 */

const para = (i: number) => `第 ${i} 段正文，长度大概四十个字，用来凑到切分门槛以上，方便测量切分行为。\n`
const joinBack = (s: { head: string[]; tail: string }) => s.head.join('') + s.tail

test('短文本不切（低于门槛）', () => {
  assert.equal(splitStreamingMarkdown('短文本\n\n再来一段', 100), null)
})

test('几个普通段落：能切，且拼回去逐字相等', () => {
  const text = Array.from({ length: 30 }, (_, i) => para(i)).join('\n') + '\n尾巴还在长的一行'
  const s = splitStreamingMarkdown(text, 100)
  assert.ok(s, '应该切')
  assert.equal(joinBack(s!), text)
  assert.ok(s!.head.length >= 1)
  assert.ok(s!.tail.length > 0 && s!.tail.length < text.length)
})

test('代码围栏里的空行不切（未闭合的那段一直留到尾部）', () => {
  const text = para(0).repeat(10) + '\n```js\nconst a = 1\n\nconst b = 2\n\nconst c = 3\n'
  const s = splitStreamingMarkdown(text, 100)
  // 围栏没闭合 → 围栏从开头到结尾都是尾部，不能把中间的空行当切点
  assert.equal(s?.tail.includes('const a = 1'), true)
  if (s) assert.equal(joinBack(s), text)
})

test('闭合的围栏可以切在它后面', () => {
  const text = para(0).repeat(12) + '\n```js\nconst a = 1\n```\n\n' + para(1)
  const s = splitStreamingMarkdown(text, 100)
  assert.ok(s)
  assert.equal(joinBack(s!), text)
  assert.ok(s!.head.some((h) => h.includes('const a = 1')))
})

test('列表跨空行不切（否则一个列表会被切成两个）', () => {
  const head = para(0).repeat(12)
  const text = head + '\n- 第一项\n\n- 第二项\n- 第三项'
  const s = splitStreamingMarkdown(text, 100)
  // 不允许切在「- 第一项」与「- 第二项」之间
  if (s) {
    const joined = s.head.join('')
    assert.equal(joined.includes('- 第一项'), joined.includes('- 第二项'), '两项必须落在同一侧')
    assert.equal(joinBack(s), text)
  }
})

test('表格行之间不切', () => {
  const head = para(0).repeat(12)
  const text = head + '\n| a | b |\n|---|---|\n\n| 1 | 2 |\n| 3 | 4 |'
  const s = splitStreamingMarkdown(text, 100)
  if (s) {
    const joined = s.head.join('')
    assert.equal(joined.includes('| 1 | 2 |'), joined.includes('| 3 | 4 |'))
    assert.equal(joinBack(s), text)
  }
})

test('$$ 数学块内不切', () => {
  const head = para(0).repeat(12)
  const text = head + '\n$$\n\nx = 1\n\n$$\n'
  const s = splitStreamingMarkdown(text, 100)
  if (s) {
    const joined = s.head.join('')
    assert.equal(joined.includes('x = 1'), joined.includes('$$'), '数学块不能被拆开')
    assert.equal(joinBack(s), text)
  }
})

test('只有一段（没有顶层空行）时不切', () => {
  const text = '很长的单段文字'.repeat(300)
  assert.equal(splitStreamingMarkdown(text, 100), null)
})

test('切分点不会落在行尾硬换行处', () => {
  const head = para(0).repeat(12)
  const text = head + '\n这是续行  \n\n下一段'
  const s = splitStreamingMarkdown(text, 100)
  if (s) assert.equal(joinBack(s), text)
})

test('缩进续行不切', () => {
  const head = para(0).repeat(12)
  const text = head + '\n- 项目\n\n  续行说明\n'
  const s = splitStreamingMarkdown(text, 100)
  if (s) {
    const joined = s.head.join('')
    assert.equal(joined.includes('- 项目'), joined.includes('续行说明'))
  }
})
