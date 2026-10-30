// tailLine-test.ts —— bash 尾巴按行染色规则的单元测试（node:test，无 DOM）。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { tailLines } from './tailLine'

test('刻度槽：行首 `+1.02` 进槽，正文不含它', () => {
  const [l] = tailLines('+1.02 WARN checkpoint encode took 412ms')
  assert.equal(l.gut, '+1.02')
  assert.equal(l.text, 'WARN checkpoint encode took 412ms')
  assert.equal(l.kind, 'warn')
})

test('级色只认行首标记：PASS / FAIL / RUN / OK', () => {
  const k = (s: string) => tailLines(s)[0].kind
  // go test 的两类行首前缀（`--- ` / `=== `）后面才是状态词 —— 设计里这些行也是绿的/灰的
  assert.equal(k('+1.24 --- PASS: TestCheckpointResume (0.84s)'), 'ok')
  assert.equal(k('+1.26 === RUN   TestMergeConcurrentFlush'), 'dim')
  assert.equal(k('?   \tmp/pkg\t[no test files]'), 'plain', '`?` 前缀后不是状态词 → 不染色')
  assert.equal(k('PASS: x'), 'ok')
  assert.equal(k('FAIL merge: peak rss'), 'err')
  assert.equal(k('ERROR: disk full'), 'err')
  assert.equal(k('RUN TestMerge'), 'dim')
  assert.equal(k('ok'), 'ok')
})

test('句子中间的关键词不染色（正常结果里出现 Error: 不该整行变红）', () => {
  const l = tailLines('wrote 3 files; skipped 1 because Error: nothing')[0]
  assert.equal(l.kind, 'plain')
  assert.equal(l.text, 'wrote 3 files; skipped 1 because Error: nothing')
})

test('标记必须是独立 token：FAILED_BUILD=1 不是标记', () => {
  assert.equal(tailLines('FAILED_BUILD=1 make')[0].kind, 'plain')
})

test('空行收尾不产生多余空行；没有时间戳时槽为空', () => {
  const ls = tailLines('created e1\nthen this\n')
  assert.equal(ls.length, 2)
  assert.deepEqual(ls.map((l) => l.gut), ['', ''])
})

test('只保留尾部 max 行', () => {
  const ls = tailLines(Array.from({ length: 10 }, (_, i) => `line ${i}`).join('\n'), 3)
  assert.deepEqual(ls.map((l) => l.text), ['line 7', 'line 8', 'line 9'])
})

test('空输入返回空数组', () => {
  assert.deepEqual(tailLines(''), [])
})
