// signalFacts-test.ts —— 「Small signals」9 类事件的一行文案（node:test，无 DOM）。
// 规则：有的字段才出现；没有的字段不编（缺 count 就别写 0 条）。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { signalText } from './signalFacts'

const t = (k: string) => k
const rep = (s: string, n: number) => s.split('{n}').join(String(n))

test('memory：saved / recalled 两条腿，副题是文件名（最多 4 个）', () => {
  const saved = signalText({ sig: 'memory', etype: 'memory_activity', n: 2, files: ['a.md', 'b.md'] }, (k) => rep(k, 2))
  assert.equal(saved.title, 'sig.memory.saved')
  assert.equal(saved.detail, 'a.md · b.md')
  const recalled = signalText({ sig: 'memory', etype: 'memory_activity_recalled', n: 3 }, (k) => rep(k, 3))
  assert.equal(recalled.title, 'sig.memory.recalled')
  assert.equal(recalled.detail, undefined, '没有文件名就不画副题')
})

test('hook：name 与 message 拼一行，缺哪个省哪个', () => {
  assert.equal(signalText({ sig: 'hook', etype: 'tool_hook_warning', name: 'gofmt', message: 'returned 2' }, t).detail, 'gofmt · returned 2')
  assert.equal(signalText({ sig: 'hook', etype: 'tool_hook_warning', message: 'x' }, t).detail, 'x')
  assert.equal(signalText({ sig: 'hook', etype: 'tool_hook_warning' }, t).detail, undefined)
})

test('silent：只报真实存在的事实（轮数/工具数/全程无输出）', () => {
  const a = signalText({ sig: 'silent', etype: 'run_silent_end', rounds: 4, toolCalls: 3, sawText: true }, (k) => rep(k, 4))
  assert.match(a.title, /sig\.silent/)
  assert.match(a.detail ?? '', /sig\.silent\.rounds/)
  assert.match(a.detail ?? '', /sig\.silent\.calls/)
  assert.doesNotMatch(a.detail ?? '', /neverSpoke/, 'sawText=true 时不该说"全程无输出"')
  const b = signalText({ sig: 'silent', etype: 'run_silent_end', sawText: false }, t)
  assert.match(b.detail ?? '', /sig\.silent\.neverSpoke/)
  const c = signalText({ sig: 'silent', etype: 'run_silent_end' }, t)
  assert.equal(c.detail, undefined, '一个字段都没有 → 不画副题')
})

test('inputs：条数进标题，副题只放前两条原文摘要', () => {
  const s = signalText({ sig: 'inputs', etype: 'user_inputs_consumed', n: 3, texts: ['a', 'b', 'c'] }, (k) => rep(k, 3))
  assert.equal(s.title, 'sig.inputs')
  assert.equal(s.detail, 'a · b')
})

test('budget：金额用同一把尺（<1 美元 4 位小数）', () => {
  const s = signalText({ sig: 'budget', etype: 'session_budget_exceeded', cost: 0.0234, maxCost: 0.02 }, t)
  assert.equal(s.detail, '$0.0234 / $0.0200')
  assert.equal(signalText({ sig: 'budget', etype: 'session_budget_exceeded' }, t).detail, undefined)
})

test('persist：有 error 才写副题', () => {
  assert.equal(signalText({ sig: 'persist', etype: 'session_persist_error', message: 'disk full' }, t).detail, 'disk full')
  assert.equal(signalText({ sig: 'persist', etype: 'session_persist_error' }, t).detail, undefined)
})

test('closed：运行账三个数都是事实，缺哪个省哪个', () => {
  // 设计那一行：「18 runs · 17 consumed · 1 abandoned mid-tool」
  const full = signalText(
    { sig: 'closed', etype: 'session_closed', runs: 18, consumed: 17, abandoned: 1 },
    (k) => k,
  )
  assert.equal(full.title, 'sig.closed')
  assert.equal(full.detail, 'sig.closed.runs · sig.closed.consumed · sig.closed.abandoned')
  // 一次都没死在工具上 → 不写「0 次停在工具中间」（那是废话，也像是故障）
  const clean = signalText({ sig: 'closed', etype: 'session_closed', runs: 3, consumed: 3 }, (k) => k)
  assert.equal(clean.detail, 'sig.closed.runs · sig.closed.consumed')
  // 老 bridge 不发这些字段 → 只有标题
  assert.deepEqual(signalText({ sig: 'closed', etype: 'session_closed' }, (k) => k), { title: 'sig.closed' })
})

test('goal / promote：轮次与任务名各自成副题', () => {
  assert.equal(signalText({ sig: 'goal', etype: 'goal_alignment_reminder', round: 7 }, (k) => rep(k, 7)).detail, 'sig.goal.round')
  assert.equal(signalText({ sig: 'promote', etype: 'task_promoted', name: 'bash', message: 'tooltask-1' }, t).detail, 'bash · tooltask-1')
})

test('compact：压缩完成只报手里的两个数，缺一端不编', () => {
  const full = signalText({ sig: 'compact', etype: 'compress_end', before: 40, after: 12 }, (k) => k)
  assert.equal(full.title, 'sig.compact')
  assert.equal(full.detail, 'sig.compact.msgs')
  // 老事件没有 before/after → 只留标题（不写「0 → 0 条消息」）
  assert.deepEqual(signalText({ sig: 'compact', etype: 'compress_end' }, (k) => k), { title: 'sig.compact' })
  assert.deepEqual(signalText({ sig: 'compact', etype: 'compress_end', after: 12 }, (k) => k), { title: 'sig.compact' })
})
