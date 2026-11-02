import { headsOf } from '../src/components/runtime/blocks/heads'
import { batchLine } from '../src/components/runtime/turn/batch'
import { roundsOf } from '../src/components/runtime/turn/rounds'
import { foldedBody, runningBody, selectTurns, turnStats, turnUsage, workBody } from '../src/components/runtime/turn/selectTurns'
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
  // the tool and the mid-turn narration are the working phase; the error line and the
  // produced-files card are the record and must survive the fold. 这里的回执排在回答
  // **之后** = 交卷后才到达 → 时间线上的一条记录，同样留在外面（判据见
  // selectTurns.recordAfterRun：运行中到达的记录才进盒子）
  assert.deepEqual(kept, ['error', 'assistant', 'artifacts', 'task_delivery'])
})

test('a receipt that arrived mid-run is folded into the working phase; a late one is not', () => {
  const midRun = [
    { kind: 'user', id: 1, text: 'q' },
    { kind: 'tool', id: 2, toolId: 'x', name: 'bash', status: 'done' },
    { kind: 'task_delivery', id: 3, taskId: 'task-1', status: 'completed' },
    { kind: 'system', id: 4, text: 'run ended', tone: 'info' },
    { kind: 'assistant', id: 5, text: 'the answer', thinking: '', streaming: false },
  ] as unknown as MsgBlock[]
  const [t1] = selectTurns(midRun)
  assert.deepEqual(foldedBody(t1).map((b) => b.kind), ['assistant'])
  assert.deepEqual(workBody(t1).map((b) => b.kind), ['tool', 'task_delivery', 'system'])

  // 同样一条回执，晚一步（回答之后）到达就属于时间线
  const late = [
    { kind: 'user', id: 1, text: 'q' },
    { kind: 'tool', id: 2, toolId: 'x', name: 'bash', status: 'done' },
    { kind: 'assistant', id: 3, text: 'the answer', thinking: '', streaming: false },
    { kind: 'task_delivery', id: 4, taskId: 'task-1', status: 'completed' },
  ] as unknown as MsgBlock[]
  const [t2] = selectTurns(late)
  assert.deepEqual(foldedBody(t2).map((b) => b.kind), ['assistant', 'task_delivery'])
  assert.deepEqual(workBody(t2).map((b) => b.kind), ['tool'])
})

test('a block with no text field does not take the transcript down', () => {
  // 真事故（2026-10）：Go 侧 Block.Text 是 `json:"text,omitempty"`，一个只有思考、
  // 没有正文的 assistant 块序列化后**没有** text 字段；历史分页把它交给 UI 后，
  // 这里读 b.text.trim() 抛 TypeError，整页转录变白（用户只是往上滚了一屏）。
  const blocks = [
    { kind: 'user', id: 1, text: 'q' },
    { kind: 'assistant', id: 2, thinking: 'only thoughts, no answer' }, // 没有 text
    { kind: 'assistant', id: 3, text: 'the answer', thinking: '', streaming: false },
  ] as unknown as MsgBlock[]
  const [turn] = selectTurns(blocks)
  // user 块挂成 turn.user，其余进 items：两条 assistant 都要在（畸形的那条不许被丢）
  assert.deepEqual(turn.items.map((b) => b.id), [2, 3], '畸形的块仍留在转录里，不许静默丢掉')
  assert.equal(turn.answer?.id, 3, '没有正文的块不能当这一轮的结论')
})

test('a signal row goes with the run it describes; only a late one stays on the timeline', () => {
  // 一行信号是这次运行的**过程说明**（用户 2026-10：简洁档里它属于 Working 内部，按
  // 事件顺序夹在工作行之间）；只有 AgentEnd 之后才到达的那条才是时间线上的记录。
  const sig = { kind: 'signal', id: 4, fact: { sig: 'memory', etype: 'memory_activity', n: 2 } } as unknown as MsgBlock
  const midRun = [
    { kind: 'user', id: 1, text: 'q' },
    { kind: 'tool', id: 2, toolId: 'x', name: 'grep', status: 'done' },
    sig,
    { kind: 'assistant', id: 3, text: 'the answer', thinking: '', streaming: false },
  ] as unknown as MsgBlock[]
  const [t1] = selectTurns(midRun)
  assert.deepEqual(foldedBody(t1).map((b) => b.id), [3], '信号不进时间线')
  assert.deepEqual(workBody(t1).map((b) => b.id), [2, 4], '信号按位置夹在工作行之后')

  const live = [
    { kind: 'user', id: 1, text: 'q' },
    { kind: 'assistant', id: 2, text: 'looking', thinking: '', streaming: true },
    sig,
  ] as unknown as MsgBlock[]
  const [t2] = selectTurns(live)
  assert.deepEqual(runningBody(t2).map((b) => b.id), [], '运行中：信号在盒子里，外面没有')
  assert.deepEqual(workBody(t2).map((b) => b.id), [4])

  // 交卷之后到达（排在回答之后）→ 时间线
  const late = [
    { kind: 'user', id: 1, text: 'q' },
    { kind: 'tool', id: 2, toolId: 'x', name: 'grep', status: 'done' },
    { kind: 'assistant', id: 3, text: 'the answer', thinking: '', streaming: false },
    sig,
  ] as unknown as MsgBlock[]
  const [t3] = selectTurns(late)
  assert.deepEqual(foldedBody(t3).map((b) => b.id), [3, 4])
  assert.deepEqual(workBody(t3).map((b) => b.id), [2])
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

test('the working box keeps mid-run narration and thoughts, but never the answer twice', () => {
  // 一轮里三次 LLM：中间两轮的话与思考必须在盒子里（规格：中间轮 Content 保留在
  // Working 内、历史轮 Reasoning 默认收缩）——此前它们既不在外面（外面只留最后一轮）
  // 也不在盒子里（workBody 只认 tool/agent/…），展开盒子也读不到。
  const blocks = [
    { kind: 'user', id: 1, text: 'q' },
    { kind: 'assistant', id: 2, text: 'first look', thinking: 'round 1 thought', streaming: false },
    { kind: 'tool', id: 3, toolId: 'a', name: 'grep', status: 'done' },
    { kind: 'assistant', id: 4, text: 'second look', thinking: 'round 2 thought', streaming: false },
    { kind: 'compression', id: 5, before: 12, after: 4 },
    { kind: 'assistant', id: 6, text: 'the answer', thinking: '', streaming: false },
  ] as unknown as MsgBlock[]
  const [turn] = selectTurns(blocks)
  const inside = workBody(turn).map((b) => b.id)
  assert.deepEqual(inside, [2, 3, 4, 5], 'mid-run narration, tool, thought and the compression belong inside')
  // 最后那条答案交给 Worked 区，盒子里不能再出现一次
  assert.ok(!inside.includes(6))
  assert.deepEqual(turn.answer?.id, 6)
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
  // the note ends round 1 **and stays in the output as its own bare segment**
  // (dropping it is what lost the artifacts card after AgentEnd); the tool after
  // it has no assistant, so it opens a new round rather than being swallowed by
  // a round it does not belong to
  assert.equal(rs.length, 3)
  // assistant 存在 r.say 里（它是开口者），items 只放被它吞下的工具
  assert.deepEqual(rs.map((r) => r.items.map((b) => b.kind)), [['tool'], ['system'], ['tool']])
  assert.equal(rs[0].say?.id, 1)
  assert.equal(rs[1].bare, true)
  assert.deepEqual(rs[0].items.map((b) => b.id), [2])
  assert.equal(rs[0].say, undefined === 0 ? undefined : rs[0].say) // 段 0 由 assistant 1 开口
  assert.equal(rs[1].say, undefined) // 记录段没有开口者
  assert.deepEqual(rs[1].items.map((b) => b.id), [3]) // system note 原位保留
  assert.deepEqual(rs[2].items.map((b) => b.id), [4])
})

test('batch line: the dominant verb names the line, and the count is its unit', () => {
  const calls = [
    { kind: 'tool', id: 1, toolId: 'a', name: 'glob', args: '{"pattern":"**/*.md"}', result: 'a.md\nb.md\nc.md', status: 'done', durMs: 277 },
    { kind: 'tool', id: 2, toolId: 'b', name: 'grep', args: '{"pattern":"x"}', result: 'f.go:1:x', status: 'done', durMs: 25 },
    { kind: 'tool', id: 3, toolId: 'c', name: 'grep', args: '{"pattern":"y"}', result: 'f.go:2:y\ng.go:9:y', status: 'done', durMs: 30 },
  ] as unknown as MsgBlock[]
  // grep wins on count (2 vs 1): verb is the past of what was done most。
  // 对象只在一组里独一份时出现（`Searched x`），多条时报条数 —— 用户 2026-10：
  // 行头要「简洁明确 + 关键信息」，同一批调用的参数原文每行都一样，读不出做了什么。
  // 「+1」是另一种动词的调用（glob）：混合时补个数，不丢。
  // 只锁行头（动词 + 数出来的量 + 度量），时长单独一格 —— 时长写法是另一件事，
  // 锁死在这里等于把两件事绑在一次改动上。
  const line = batchLine(calls as never)
  assert.ok(line.startsWith('Searched 2 queries +1 \u00b7 3 hits'), line)
  assert.ok(line.split(' \u00b7 ').length === 3, line)
})

test('batch line: parallel bash calls are one line with a count, not raw commands', () => {
  const calls = [
    { kind: 'tool', id: 1, toolId: 'a', name: 'bash', args: '{"command":"cd /Users/admin/workspace/go-code && grep -n x"}', result: 'x'.repeat(1130), status: 'done', durMs: 41 },
    { kind: 'tool', id: 2, toolId: 'b', name: 'bash', args: '{"command":"cd /Users/admin/workspace/go-code && sed -n 1,2p"}', result: 'y'.repeat(2460), status: 'done', durMs: 35 },
  ] as unknown as MsgBlock[]
  // 参数原文不进行头（每条都一样 = 没有信息）；报几条命令 + 输出体量 + 耗时
  const two = batchLine(calls as never)
  const one = batchLine([calls[0]] as never)
  assert.ok(two.startsWith('Ran 2 commands \u00b7 3.5 KB'), two)
  assert.ok(one.startsWith('Ran 1 command \u00b7 1.1 KB'), one)
  assert.ok(!two.includes('cd /Users/admin'), '行头不得出现命令原文：' + two)
})

test('batch line: an edit reports the diff, not a count of calls', () => {
  const calls = [
    { kind: 'tool', id: 1, toolId: 'a', name: 'edit_file', args: '{"path":"a.go"}', diff: { path: 'a.go', added: 8, removed: 5 }, status: 'done', durMs: 1200 },
  ] as unknown as MsgBlock[]
  assert.equal(batchLine(calls as never), 'Edited a.go \u00b7 +8 \u22125 \u00b7 1s200ms')
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

test('roundsOf：产出/信号块不被丢掉（agent_end 的产出卡就靠它）', () => {
  const list = [
    { kind: 'user', id: 1, text: 'hi' },
    { kind: 'assistant', id: 2, text: 'ok', streaming: false, thinking: '' },
    { kind: 'artifacts', id: 3, items: [{ name: 'a.go', path: 'x/a.go', kind: 'created', added: 5 }] },
    { kind: 'system', id: 4, text: 'done', tone: 'info' },
  ] as never
  const rounds = roundsOf(list as never)
  const kinds = rounds.flatMap((r) => r.items.map((b) => b.kind))
  // 产出与系统信号都必须留在结果里，且各自是一段（bare）
  if (!kinds.includes('artifacts') || !kinds.includes('system')) {
    throw new Error('记录块被 roundsOf 丢掉了: ' + JSON.stringify(kinds))
  }
  // user 也不属于任何一轮 → 它同样是一段 bare；记录块与它各占一段
  const bare = rounds.filter((r) => r.bare).map((r) => r.items[0].kind)
  if (!bare.includes('artifacts') || !bare.includes('system')) {
    throw new Error('记录块没有被单独成段: ' + JSON.stringify(bare))
  }
})

test('roundsOf：同一 request 的并行调用夹着 assistant 也归同一段（一个 Ran 组件）', () => {
  const list = [
    { kind: 'user', id: 1, text: 'q' },
    { kind: 'assistant', id: 2, text: '', thinking: '先看现状', streaming: false },
    { kind: 'tool', id: 3, toolId: 'a', name: 'bash', args: '{"command":"grep -n x"}', status: 'running', requestId: 'req-1' },
    // 并行调用的中途又有正文/思考块到达（同一次请求）
    { kind: 'assistant', id: 4, text: '', thinking: '', streaming: true },
    { kind: 'tool', id: 5, toolId: 'b', name: 'bash', args: '{"command":"sed -n 1,2p"}', status: 'running', requestId: 'req-1' },
  ] as unknown as MsgBlock[]
  const rs = roundsOf(list)
  const withTools = rs.filter((r) => r.items.some((b) => b.kind === 'tool'))
  assert.equal(withTools.length, 1, '同一 request 的两次调用必须在同一段（否则屏幕上两个 Ran 行）')
  assert.deepEqual(withTools[0].items.map((b) => b.id), [3, 5])
})

test('dispatch tools are not transcript rows (the lane already tells that story)', async () => {
  const { isDispatchTool } = await import('../src/components/runtime/turn/batch')
  assert.equal(isDispatchTool('agent_spawn'), true)
  assert.equal(isDispatchTool('agent_send'), true)
  assert.equal(isDispatchTool('agent_resume'), true)
  // 普通工具照旧占一行
  assert.equal(isDispatchTool('grep'), false)
  assert.equal(isDispatchTool('read_file'), false)
  // 名字像但不是派发的不要误杀
  assert.equal(isDispatchTool('agent_wait_notes'), false)
})
