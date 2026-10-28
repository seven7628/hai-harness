import { headsOf } from '../src/components/runtime/blocks/heads'
import { batchLine } from '../src/components/runtime/turn/batch'
import { roundsOf } from '../src/components/runtime/turn/rounds'
import { foldedBody, runningBody, selectTurns, turnStats, turnUsage } from '../src/components/runtime/turn/selectTurns'
import { roundsOf } from '../src/components/runtime/turn/rounds'
import assert from 'node:assert'
import test from 'node:test'
import type { MsgBlock, TurnMetric } from '../src/store/useAppStore'

const blocks = [
  { kind: 'system', id: 0, text: 'boot' },
  { kind: 'user', id: 1, text: 'q1' },
  { kind: 'assistant', id: 2, text: '', thinking: 'a', streaming: false, runId: 'r1', durMs: 1000, ttftMs: 300 },
  { kind: 'tool', id: 3, toolId: 'x', name: 'grep', status: 'done', durMs: 500 },
  { kind: 'tool', id: 4, toolId: 'y', name: 'edit_file', status: 'done', durMs: 250,
    diff: { path: 'a.go', added: 3, removed: 1 } },
  { kind: 'tool', id: 5, toolId: 'z', name: 'edit_file', status: 'done', durMs: 250,
    diff: { path: 'b.go', added: 4, removed: 2 } },
  { kind: 'assistant', id: 6, text: 'done', thinking: '', streaming: false, runId: 'r1', durMs: 2000 },
  { kind: 'user', id: 7, text: 'q2' },
  { kind: 'assistant', id: 8, text: 'second', thinking: '', streaming: false, runId: 'r2', durMs: 3000 },
] as unknown as MsgBlock[]

const metrics: TurnMetric[] = [
  { model: 'm', input: 100, output: 10, cacheRead: 90, cacheWrite: 0, reasoning: 5, durMs: 1000, costUsd: 0, runId: 'r1' },
  { model: 'm', input: 200, output: 20, cacheRead: 190, cacheWrite: 0, reasoning: 6, durMs: 2000, costUsd: 0, runId: 'r1' },
  { model: 'm', input: 50, output: 5, cacheRead: 40, cacheWrite: 0, reasoning: 1, durMs: 3000, costUsd: 0, runId: 'r2' },
]

test('blocks before the first user message still get a home', () => {
  const turns = selectTurns(blocks)
  assert.equal(turns.length, 3)
  assert.equal(turns[0].user, undefined)
  assert.equal(turns[0].items.length, 1)
})

test('a turn runs until the next user message, and the answer is the last assistant with text', () => {
  const [, t1, t2] = selectTurns(blocks)
  assert.equal(t1.user!.text, 'q1')
  assert.equal(t1.tools.length, 3)
  assert.equal(t1.answer!.text, 'done')
  assert.equal(t2.answer!.text, 'second')
})

test('stats merge diffs by path and only sum measured segments', () => {
  const [, t1] = selectTurns(blocks)
  const s = turnStats(t1)
  assert.equal(s.calls, 3)
  assert.equal(s.files, 2)
  assert.equal(s.added, 7)
  assert.equal(s.removed, 3)
  // 1000 + 2000 (assistant) + 500 + 250 + 250 (tools); the empty assistant text adds nothing
  assert.equal(s.workedMs, 4000)
})

test('usage joins by runId, not by position', () => {
  const [, t1, t2] = selectTurns(blocks)
  const u1 = turnUsage(t1, metrics)
  assert.equal(u1.matched, true)
  assert.equal(u1.input, 300)
  assert.equal(u1.output, 30)
  assert.equal(u1.cacheRead, 280)
  assert.equal(u1.llmMs, 3000)
  assert.equal(u1.ttftMs, 300)
  const u2 = turnUsage(t2, metrics)
  assert.equal(u2.input, 50)
  assert.equal(u2.llmMs, 3000)
})

test('no metrics means no numbers, not zeros dressed as data', () => {
  const [, t1] = selectTurns(blocks)
  const u = turnUsage(t1, undefined)
  assert.equal(u.matched, false)
  assert.equal(u.llmMs, 0)
})

test('a folded turn keeps the record and drops the work', () => {
  const withRecord = [
    { kind: 'user', id: 1, text: 'q' },
    { kind: 'tool', id: 2, toolId: 'x', name: 'grep', status: 'done' },
    { kind: 'assistant', id: 3, text: 'I will look', thinking: '', streaming: false },
    { kind: 'error', id: 4, text: 'llm_error' },
    { kind: 'assistant', id: 5, text: 'the answer', thinking: '', streaming: false },
    { kind: 'artifacts', id: 6, items: [{ name: 'a.go', path: 'a.go', kind: 'created' }] },
    { kind: 'task_delivery', id: 7, taskId: 'task-1', status: 'completed' },
  ] as unknown as MsgBlock[]
  const [turn] = selectTurns(withRecord)
  const kept = foldedBody(turn).map((b) => b.kind)
  // the tool and the mid-turn narration are the working phase; everything else
  // is the record and must survive the fold
  assert.deepEqual(kept, ['error', 'assistant', 'artifacts', 'task_delivery'])
})

test('while running, the working block holds the whole run', () => {
  const live = [
    { kind: 'user', id: 1, text: 'q' },
    { kind: 'assistant', id: 2, text: 'let me look', thinking: '', streaming: false },
    { kind: 'tool', id: 3, toolId: 'a', name: 'grep', status: 'done' },
    { kind: 'agent', id: 4, runId: 'r1', label: 'audit', status: 'running', spawnedAt: 0, usage: {}, items: [] },
    // the reply being typed right now
    { kind: 'assistant', id: 5, text: 'the answer so far', thinking: '', streaming: true },
  ] as unknown as MsgBlock[]
  const [turn] = selectTurns(live)
  // nothing of the run is outside the working block — not even the sentence
  // being written; only the record would survive, and this turn has none
  assert.deepEqual(runningBody(turn).map((b) => b.id), [])
  const withRecord = [
    ...live,
    { kind: 'artifacts', id: 9, items: [{ name: 'a.go', path: 'a.go', kind: 'created' }] },
  ] as unknown as MsgBlock[]
  const [t2] = selectTurns(withRecord)
  assert.deepEqual(runningBody(t2).map((b) => b.id), [9])
})

test('rounds: a new request id starts a new round, even with no assistant in between', () => {
  const items = [
    { kind: 'assistant', id: 1, text: 'first', thinking: '', streaming: false },
    { kind: 'tool', id: 2, toolId: 'a', name: 'grep', requestId: 'R1', status: 'done' },
    { kind: 'tool', id: 3, toolId: 'b', name: 'grep', requestId: 'R1', status: 'done' },
    // 下一轮 LLM 调用（还没有 assistant 块）—— 相邻性会把这三条并成一段
    { kind: 'tool', id: 4, toolId: 'c', name: 'grep', requestId: 'R2', status: 'done' },
  ] as unknown as MsgBlock[]
  const rs = roundsOf(items)
  assert.equal(rs.length, 2)
  assert.deepEqual(rs[0].items.map((b) => b.id), [2, 3])
  assert.deepEqual(rs[1].items.map((b) => b.id), [4])
  assert.equal(rs[1].req, 'R2')
})

test('rounds: without request ids the old adjacency still groups them', () => {
  const items = [
    { kind: 'assistant', id: 1, text: 'first', thinking: '', streaming: false },
    { kind: 'tool', id: 2, toolId: 'a', name: 'grep', status: 'done' },
    { kind: 'tool', id: 3, toolId: 'b', name: 'grep', status: 'done' },
  ] as unknown as MsgBlock[]
  const rs = roundsOf(items)
  assert.equal(rs.length, 1)
  assert.deepEqual(rs[0].items.map((b) => b.id), [2, 3])
})

test('rounds: an assistant opens one, its tools belong to it, bare tools open their own', () => {
  const items = [
    { kind: 'assistant', id: 1, text: 'first', thinking: '', streaming: false },
    { kind: 'tool', id: 2, toolId: 'a', name: 'grep', status: 'done' },
    { kind: 'tool', id: 3, toolId: 'b', name: 'read_file', status: 'done' },
    { kind: 'assistant', id: 4, text: 'second', thinking: '', streaming: false },
    { kind: 'tool', id: 5, toolId: 'c', name: 'bash', status: 'done' },
  ] as unknown as MsgBlock[]
  const rs = roundsOf(items)
  assert.equal(rs.length, 2)
  assert.equal(rs[0].say?.id, 1)
  assert.deepEqual(rs[0].items.map((b) => b.id), [2, 3])
  assert.equal(rs[1].say?.id, 4)
  assert.deepEqual(rs[1].items.map((b) => b.id), [5])
})

test('rounds: tools before any assistant are a round without words', () => {
  const items = [
    { kind: 'tool', id: 1, toolId: 'a', name: 'grep', status: 'done' },
    { kind: 'tool', id: 2, toolId: 'b', name: 'bash', status: 'done' },
    { kind: 'assistant', id: 3, text: 'then', thinking: '', streaming: false },
  ] as unknown as MsgBlock[]
  const rs = roundsOf(items)
  assert.equal(rs.length, 2)
  assert.equal(rs[0].say, undefined)
  assert.deepEqual(rs[0].items.map((b) => b.id), [1, 2])
  assert.equal(rs[1].say?.id, 3)
})

test('rounds: a non-work block closes the open round', () => {
  const items = [
    { kind: 'assistant', id: 1, text: 'a', thinking: '', streaming: false },
    { kind: 'tool', id: 2, toolId: 'a', name: 'grep', status: 'done' },
    { kind: 'system', id: 3, text: 'note' },
    { kind: 'tool', id: 4, toolId: 'b', name: 'bash', status: 'done' },
  ] as unknown as MsgBlock[]
  const rs = roundsOf(items)
  // the note ends round 1; the tool after it has no assistant, so it opens a new
  // round rather than being swallowed by a round it does not belong to
  assert.equal(rs.length, 2)
  assert.deepEqual(rs[0].items.map((b) => b.id), [2])
  assert.equal(rs[1].say, undefined)
  assert.deepEqual(rs[1].items.map((b) => b.id), [4])
})

test('batch line: the dominant verb names the line, and the count is its unit', () => {
  const calls = [
    { kind: 'tool', id: 1, toolId: 'a', name: 'glob', args: '{"pattern":"**/*.md"}', result: 'a.md\nb.md\nc.md', status: 'done', durMs: 277 },
    { kind: 'tool', id: 2, toolId: 'b', name: 'grep', args: '{"pattern":"x"}', result: 'f.go:1:x', status: 'done', durMs: 25 },
    { kind: 'tool', id: 3, toolId: 'c', name: 'grep', args: '{"pattern":"y"}', result: 'f.go:2:y\ng.go:9:y', status: 'done', durMs: 30 },
  ] as unknown as MsgBlock[]
  // grep wins on count (2 vs 1): verb is the past of what was done most
  assert.equal(batchLine(calls as never), 'Searched x +1 \u00b7 3 hits \u00b7 332 ms')
})

test('batch line: an edit reports the diff, not a count of calls', () => {
  const calls = [
    { kind: 'tool', id: 1, toolId: 'a', name: 'edit_file', args: '{"path":"a.go"}', diff: { path: 'a.go', added: 8, removed: 5 }, status: 'done', durMs: 1200 },
  ] as unknown as MsgBlock[]
  assert.equal(batchLine(calls as never), 'Edited a.go \u00b7 +8 \u22125 \u00b7 1.2 s')
})

test('batch line: nothing to say is nothing, not a zero', () => {
  assert.equal(batchLine([]), '')
})

test('compression scan heads: a tool is a verb and its object, and time is relative', () => {
  const list = [
    { kind: 'tool', id: 1, toolId: 'a', name: 'grep', args: '{"pattern":"os.UserHomeDir"}', status: 'done', ts: 1000 },
    { kind: 'tool', id: 2, toolId: 'b', name: 'read_file', args: '{"path":"session/restore.go","start_line":118,"end_line":164}', status: 'done', ts: 1400 },
  ] as unknown as MsgBlock[]
  const heads = headsOf(list)
  assert.equal(heads.length, 2)
  assert.equal(heads[0].t, '+00.0')
  assert.equal(heads[0].h, 'Grep os.UserHomeDir')
  assert.equal(heads[1].t, '+00.4')
  assert.equal(heads[1].h, 'Read session/restore.go:118-164')
})

test('compression scan heads: no timestamp means no made-up time', () => {
  const list = [
    { kind: 'assistant', id: 1, text: 'first line\nsecond', thinking: '', streaming: false },
  ] as unknown as MsgBlock[]
  const heads = headsOf(list)
  assert.equal(heads[0].t, '')
  assert.equal(heads[0].h, 'first line')
})
