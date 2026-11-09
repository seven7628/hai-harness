// selectTurns-test.ts —— 简洁档「时间线 + 一个 Working 块」的归属判据（node:test 无 DOM）。
//
// 这批用例锁的是三条口径：
//   1. 记录（signal / task_delivery）的位置看**到达时刻**：运行中到达 → 盒内、按事件
//      顺序夹在工作行之间；AgentEnd 之后到达 → 时间线上（盒子已折叠，折进去等于藏了）；
//   2. 结构性/结论性块始终在外面：error / divider / artifacts；
//   3. 盒子存在 = workBody 非空（不是「有工具」）；没有盒子的轮（纯对话）正文回到时间线。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { ALWAYS_SHOWN, FOLDED_KINDS, defersToTurnEnd, foldedBody, hasWork, mergeArtifacts, noticeBlocks, outsideOf, runningBody, runWorkBody, segmentUsage, selectTurns, statsOfItems, turnRuns, turnUsage, workBody } from './selectTurns'
import type { MsgBlock, TurnMetric } from '../../../store/useAppStore'

const user = (id: number, text = 'go'): MsgBlock => ({ kind: 'user', id, text })
const interjected = (id: number, text = '插一句'): MsgBlock => ({ kind: 'user', id, text, injected: true })
const assistant = (id: number, text: string, streaming = false, runId?: string): MsgBlock => ({
  kind: 'assistant', id, text, streaming, thinking: '', ...(runId ? { runId } : {}),
})
const tool = (id: number, status: 'running' | 'done' = 'done', runId?: string): MsgBlock => ({
  kind: 'tool', id, toolId: 't' + id, name: 'bash', status, ...(runId ? { parentRunId: runId } : {}),
})
// afterRun 传 undefined = 旧快照（没有这一位，走位置兜底）；true/false = store 显式盖的戳。
const delivery = (id: number, taskId = 'task-1', afterRun?: boolean): MsgBlock => ({
  kind: 'task_delivery', id, taskId, status: 'completed', result: 'done', ...(afterRun != null ? { afterRun } : {}),
})
const signal = (id: number, afterRun?: boolean): MsgBlock => ({
  kind: 'signal',
  id,
  fact: { sig: 'promote', etype: 'task_promoted', name: 'bash' },
  ...(afterRun != null ? { afterRun } : {}),
})

// 主 run：user → 工具行 → 运行中到达的 promote / 回执 → 收尾正文；AgentEnd 之后回执才到。
const turnWithRecords: MsgBlock[] = [
  user(1),
  tool(2),
  signal(3),
  delivery(4),
  tool(5),
  delivery(6),
  assistant(7, '改好了'),
  delivery(8),
]

test('运行中到达的记录进盒子，并且保持事件顺序（夹在工作行之间）', () => {
  const turn = selectTurns(turnWithRecords)[0]
  assert.equal(ALWAYS_SHOWN.has('signal'), false)
  assert.equal(ALWAYS_SHOWN.has('task_delivery'), false)
  // 顺序 = 事件顺序：2 工具 → 3 摘后台 → 4 回执 → 5 工具 → 6 回执
  assert.deepEqual(workBody(turn).map((b) => b.id), [2, 3, 4, 5, 6])
})

test('AgentEnd 之后到达的回执留在时间线上（盒子外面，且在回答之后）', () => {
  const turn = selectTurns(turnWithRecords)[0]
  // 8 = 回答之后才到的回执：位置判据（没有 afterRun 位时同样成立）
  assert.deepEqual(foldedBody(turn).map((b) => b.id), [7, 8])
})

test('afterRun 位压过位置（续跑在同一轮里又产出正文时，位置会骗人）', () => {
  // agent_resume 原地续跑：交卷后插入的回执之后又有了一段正文 —— 位置判据会把它算进
  // 盒子，块上的 afterRun 位不会。位置只是旧快照（没有这一位）的兜底。
  const blocks: MsgBlock[] = [user(1), tool(2), delivery(3, 'task-1', true), assistant(4, '续跑结论')]
  const turn = selectTurns(blocks)[0]
  assert.deepEqual(workBody(turn).map((b) => b.id), [2])
  assert.deepEqual(foldedBody(turn).map((b) => b.id), [3, 4])
})

test('恢复后的视图：运行中到达（afterRun:false）的记录仍在盒内，哪怕位置在正文之后', () => {
  // 用户 2026-11 报障：刷新（暖恢复）之后 compress_end 没有收缩进 Working 盒子。
  // 恢复出来的视图里「正在写的那段正文」已经不再是 streaming —— answerIndex 把它当成
  // 这一轮的结论，位置判据（i > answerAt）于是把之后到达的每条记录都判成「交卷后」。
  // 块上的 afterRun:false 是「它是运行中到达的」这个事实，压过位置。
  const blocks: MsgBlock[] = [user(1), assistant(2, '写到一半'), signal(3, false), delivery(4, 'task-1', false)]
  const turn = selectTurns(blocks)[0]
  assert.deepEqual(workBody(turn).map((b) => b.id), [3, 4], '运行中到达的记录必须还在盒子里')
  assert.deepEqual(foldedBody(turn).map((b) => b.id), [2], '正文是这一轮的结论，留在外面')
})

test('旧快照（没有 afterRun 位）：夹在同一次运行中间的记录留在盒内 —— 哪怕轮内更早有正文', () => {
  // 用户 2026-11 截图：19 分钟的运行里 05:57 已经交付过一段正文（narration），06:05 到达的
  // `Task … completed` 被位置判据（i > answerAt）判成「交卷后」，于是全被甩到盒子外面、
  // 永远排在 Working 下方。位置兜底必须看**邻居运行块**：后面还有同一个 run 的块 = 运行中到达。
  const blocks: MsgBlock[] = [
    user(1),
    assistant(2, 'D2 方案完成，先交代一下结论', false, 'r1'),
    tool(3, 'done', 'r1'),
    delivery(4), // 旧 record：没有 after_run 位
    tool(5, 'done', 'r1'),
    assistant(6, '', false, 'r1'),
  ]
  const turn = selectTurns(blocks)[0]
  assert.deepEqual(workBody(turn).map((b) => b.id), [3, 4, 5, 6], '运行中到达的回执必须还在盒子里')
  assert.deepEqual(foldedBody(turn).map((b) => b.id), [2], '正文仍是这一轮的结论；记录不重复渲染')
})

test('旧快照：两次运行之间的回执留在时间线上（后面不再有同一个 run 的块）', () => {
  // 情况二：run1 交卷 → 后台任务回执到达 → run2 起新盒子。前后邻居是**两次不同**的运行，
  // 说明这条回执排在前一次运行之后 → 盒外。
  const blocks: MsgBlock[] = [
    user(1),
    tool(2, 'done', 'r1'),
    delivery(3),
    assistant(4, '续跑结论', false, 'r2'),
    tool(5, 'done', 'r2'),
  ]
  const turn = selectTurns(blocks)[0]
  assert.deepEqual(workBody(turn).map((b) => b.id), [2, 5])
  assert.deepEqual(foldedBody(turn).map((b) => b.id), [3, 4])
})

test('同一批块不会既在盒子里又在外面（无重复渲染）', () => {
  const turn = selectTurns(turnWithRecords)[0]
  const inside = new Set(workBody(turn).map((b) => b.id))
  for (const b of foldedBody(turn)) {
    if (b.kind !== 'assistant') assert.equal(inside.has(b.id), false, '记录块不得同时进盒子')
  }
})

test('盒子里有记录但还没收尾：运行期间记录也在盒内（外面只剩结论/结构块）', () => {
  const blocks: MsgBlock[] = [user(1), tool(2, 'running'), signal(3), assistant(4, '正在写…', true)]
  const turn = selectTurns(blocks)[0]
  assert.deepEqual(workBody(turn).map((b) => b.id), [2, 3])
  // 运行中：外面什么都不该有（正文在盒子里，记录也在盒子里）
  assert.deepEqual(runningBody(turn, false).map((b) => b.id), [])
})

test('盒子存在 = workBody 非空（async_task-only 的轮不再整轮消失）', () => {
  const onlyTask: MsgBlock[] = [
    user(1),
    { kind: 'async_task', id: 2, taskId: 'task-9', label: '后台任务', status: 'running', startedAt: 1 },
  ]
  const turn = selectTurns(onlyTask)[0]
  assert.equal(hasWork(turn), true)
  assert.deepEqual(workBody(turn).map((b) => b.kind), ['async_task'])
})

test('纯对话轮（无工作过程）不画盒子 → 正文必须回到时间线', () => {
  const chat: MsgBlock[] = [user(1), assistant(2, '正在写…', true)]
  const turn = selectTurns(chat)[0]
  assert.equal(hasWork(turn), false)
  // 没有盒子时：外面 = 记录 + 正在写的正文（否则整轮在 AgentEnd 之前一个字都不显示）
  assert.deepEqual(runningBody(turn, true).map((b) => b.id), [2])
  // 有盒子时：正文仍留在盒子里，外面只有记录
  const withTool: MsgBlock[] = [user(1), tool(2), assistant(3, '正在写…', true)]
  const t2 = selectTurns(withTool)[0]
  assert.equal(hasWork(t2), true)
  assert.deepEqual(runningBody(t2, false).map((b) => b.kind), [])
})

test('中间轮正文进盒子、结论在外面（简洁档收起时的内容集合）', () => {
  const blocks: MsgBlock[] = [user(1), assistant(2, '先看看'), tool(3), assistant(4, '结论')]
  const turn = selectTurns(blocks)[0]
  assert.deepEqual(workBody(turn).map((b) => b.id), [2, 3])
  assert.deepEqual(foldedBody(turn).map((b) => b.id), [4])
})

test('结构性/结论性块始终在外面；执行阶段载荷在盒子里', () => {
  for (const k of ['error', 'divider', 'artifacts'] as const) {
    assert.equal(ALWAYS_SHOWN.has(k), true, k + ' 应留在时间线上')
  }
  for (const k of ['tool', 'agent', 'async_task', 'system', 'task_result'] as const) {
    assert.equal(FOLDED_KINDS.has(k), true, k + ' 应进工作过程')
  }
})

test('轮上记着自己属于哪次运行（agent 块的 runId 是子 run，不能拿来当这一轮的 run）', () => {
  const blocks: MsgBlock[] = [
    user(1),
    {
      kind: 'agent', id: 2, runId: 'sub-run', label: 'subagent', status: 'completed', spawnedAt: 0,
      usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0, costUsd: 0 }, items: [],
    },
    { kind: 'tool', id: 3, toolId: 't3', name: 'bash', status: 'done', runId: 'main-run' },
    assistant(4, '结论'),
  ]
  const turn = selectTurns(blocks)[0]
  assert.equal(turn.run, 'main-run', '取正文/工具块的 runId，跳过子 run')
})

// —— 轮尾账（Ledger）的归属：只算这一轮**自己的 run** ——
//
// 用户 2026-11 报障：AgentEnd 时这行是「7k tokens · LLM 2s」，发下一条消息后变成
// 「17k tokens · LLM 3s」。原因是子 agent 的 runId 挂在卡片上也被算进了这一轮，
// 而后台子 agent 在主 run 交卷之后还在产生 llm_end —— 已结算的轮尾账被后来的事件改写。
const metric = (runId: string, input: number, output: number, durMs = 1000): TurnMetric => ({
  model: 'm', input, output, cacheRead: 0, cacheWrite: 0, reasoning: 0, durMs, costUsd: 0, runId,
})
const agentBlock = (id: number, runId: string, status: 'running' | 'completed' = 'running'): MsgBlock => ({
  kind: 'agent', id, runId, label: 'subagent', status, spawnedAt: 0,
  usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0, costUsd: 0 }, items: [],
})
const withRun = (b: MsgBlock, runId: string): MsgBlock => ({ ...b, runId } as MsgBlock)

test('轮尾账只算这一轮自己的 run：子 agent 的用量不进这一轮', () => {
  const blocks: MsgBlock[] = [
    user(1),
    withRun(tool(2), 'r1'),
    agentBlock(3, 'sub-1'),
    withRun(assistant(4, '结论'), 'r1'),
  ]
  const turn = selectTurns(blocks)[0]
  const first = turnUsage(turn, [metric('r1', 1000, 100), metric('sub-1', 5000, 500)])
  assert.equal(first.matched, true)
  assert.equal(first.input, 1000, '子 run 的 5000 不进来')
  assert.equal(first.total, 1100)

  // 下一轮开始后，子 agent 又完成一次调用（迟到 metric）：已交卷那一轮的数不得变化
  const later = turnUsage(turn, [
    metric('r1', 1000, 100),
    metric('sub-1', 5000, 500),
    metric('sub-1', 9000, 900),
  ])
  assert.deepEqual(later, first, '已结算的轮尾账不得被后来的子 run 事件改写')
})

test('同一轮里的多段主 run（agent_resume 续跑）都算进这一轮', () => {
  const blocks: MsgBlock[] = [
    user(1),
    withRun(tool(2), 'r1'),
    withRun(assistant(3, '先到这'), 'r1'),
    withRun(tool(4), 'r2'),
    withRun(assistant(5, '续跑结论'), 'r2'),
  ]
  const turn = selectTurns(blocks)[0]
  assert.equal(turn.run, 'r2', '轮上的 run = 最近一次')
  const u = turnUsage(turn, [metric('r1', 1000, 100), metric('r2', 2000, 200)])
  assert.equal(u.input, 3000)
  assert.equal(u.output, 300)
})

// —— 运行中插话：同一个 run 只有一个 Working 盒子 ——
//
// 用户 2026-11：Working 正在跑时又说一句，转录里出现**第二个** Working 盒子
// （同一个 run 被 user 块切成两轮，两轮都 live）。插话属于当前这一轮：
// 按事件顺序留在 items / 盒子内部，轮的开口仍然是第一句。
test('运行中插话不另起一轮：留在当前轮，轮的开口还是第一条消息', () => {
  const blocks: MsgBlock[] = [user(1, '第一句'), tool(2, 'running'), interjected(3, '再补一句'), tool(4, 'running')]
  const turns = selectTurns(blocks)
  assert.equal(turns.length, 1, '插话不能把同一 run 切成两轮')
  assert.equal(turns[0].user?.id, 1)
  assert.deepEqual(turns[0].items.map((b) => b.id), [2, 3, 4], '插话按事件顺序夹在工作行之间')
})

test('插话在盒子里；盒子仍然只有一个（没有第二行 Working）', () => {
  const blocks: MsgBlock[] = [user(1), tool(2, 'running'), interjected(3), assistant(4, '正在写…', true)]
  const turn = selectTurns(blocks)[0]
  assert.deepEqual(workBody(turn).map((b) => b.id), [2, 3])
  assert.equal(hasWork(turn), true)
})

test('没有当前轮时的插话（会话从中间恢复）仍然有一个家', () => {
  const blocks: MsgBlock[] = [interjected(1, '半途恢复'), assistant(2, '结论')]
  const turns = selectTurns(blocks)
  assert.equal(turns.length, 1)
  assert.equal(turns[0].user?.id, 1)
})

test('总览：一轮工作里只有第一条 user 在外面（插话不画第二遍）', () => {
  const blocks: MsgBlock[] = [user(1), tool(2, 'running'), interjected(3), assistant(4, '答')]
  const turn = selectTurns(blocks)[0]
  assert.equal(foldedBody(turn).some((b) => b.id === 3), false, '插话不在时间线上')
})

// —— 产出卡：一轮里的多次运行合并成一张 ——
type ArtifactsBlock = Extract<MsgBlock, { kind: 'artifacts' }>
const art = (id: number, runId: string, items: Array<[string, number, number]>, truncated = false): ArtifactsBlock => ({
  kind: 'artifacts', id, runId, truncated,
  items: items.map(([path, added, removed]) => ({ name: path.split('/').pop() ?? path, path, kind: 'modified' as const, added, removed })),
})

test('同一轮两次运行的产出合并成一张卡：按 path 去重，后一次覆盖前一次', () => {
  // 用户 2026-11 截图：同一轮连续两张「本轮产出 1 个文件」（后台任务结果回推 → 续跑 →
  // 第二个 AgentEnd 又带一份清单）。一个 AgentEnd 一块是对的，卡片只该画一张。
  const merged = mergeArtifacts([
    art(1, 'r1', [['a.md', 75, 0], ['same.md', 10, 2]]),
    art(2, 'r2', [['b.md', 60, 0], ['same.md', 12, 3]]),
  ])
  assert.deepEqual(merged.items.map((i) => i.path), ['a.md', 'same.md', 'b.md'], '按首次出现排序')
  assert.deepEqual(merged.items.find((i) => i.path === 'same.md'), {
    name: 'same.md', path: 'same.md', kind: 'modified', added: 12, removed: 3,
  }, '同一文件取最后一次的数字')
  assert.equal(merged.id, 1, 'key 用第一块，合并前后不重挂')
})

test('产出合并：只有一块时原样返回；truncated 取或', () => {
  const one = art(1, 'r1', [['a.md', 1, 0]])
  assert.equal(mergeArtifacts([one]), one)
  const merged = mergeArtifacts([art(1, 'r1', [['a.md', 1, 0]]), art(2, 'r2', [['b.md', 2, 0]], true)])
  assert.equal(merged.truncated, true)
})

// —— 一轮多次运行（用户 2026-11 的两个形态）——
const toolOf = (id: number, runId: string, status: 'running' | 'done' = 'done'): MsgBlock =>
  ({ kind: 'tool', id, toolId: 't' + id, name: 'bash', status, parentRunId: runId })
const asstOf = (id: number, runId: string, text: string): MsgBlock =>
  ({ kind: 'assistant', id, text, streaming: false, thinking: '', runId })
const asstThinking = (id: number, runId: string): MsgBlock =>
  ({ kind: 'assistant', id, text: '', thinking: '想一想', streaming: false, runId })

test('case 2：run1 交卷 → 任务回执 → run2 起新段（回执在段与段之间）', () => {
  // 用户口径：一个盒子 = 一次运行；交卷后到达的回执在**顶层时间线**上，下一个盒子在它后面。
  const blocks: MsgBlock[] = [
    user(1),
    asstOf(2, 'A', '第一段'),
    toolOf(3, 'A'),
    delivery(4, 'task-x', true), // 交卷后到达
    asstOf(5, 'B', '第二段'),
    toolOf(6, 'B'),
    signal(7, false),             // 运行中到达（store 显式盖 afterRun:false）
  ]
  const turn = selectTurns(blocks)[0]
  const runs = turnRuns(turn)
  assert.equal(runs.length, 2, '两次运行 = 两段')
  assert.deepEqual(runs.map((r) => r.runId), ['A', 'B'])
  // user 块是 turn.user，不在 items 里 —— 段从第一块工作行开始
  assert.deepEqual(runs.map((r) => r.items.map((b) => b.id)), [[2, 3, 4], [5, 6, 7]], '每段是自己的连续区间')
  const answerAt = turn.items.indexOf(turn.answer as MsgBlock)
  // 段内的盒子内容：A 段只有工作行（回执是交卷后到达 → 不在盒子里）
  assert.deepEqual(runWorkBody(runs[0].items, { answer: turn.answer, answerAt, offset: runs[0].start }).map((b) => b.id), [2, 3])
  // B 段：运行中到达的 signal 在盒子里
  assert.deepEqual(runWorkBody(runs[1].items, { answer: turn.answer, answerAt, offset: runs[1].start }).map((b) => b.id), [6, 7])
  // 每段报自己的数
  assert.equal(statsOfItems(runs[0].items).calls, 1)
  assert.equal(statsOfItems(runs[1].items).calls, 1)
})

test('case 2 分段：子 agent 卡片（runId 是子 run）不切段，工具按 parentRunId 归属', () => {
  const sub: MsgBlock = {
    kind: 'agent', id: 3, runId: 'sub-1', parentRunId: 'A', label: 'sub', status: 'completed',
    spawnedAt: 1, usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0 }, items: [],
  }
  const blocks: MsgBlock[] = [user(1), asstOf(2, 'A', 'x'), sub, toolOf(4, 'A'), asstOf(5, 'B', 'y')]
  const runs = turnRuns(selectTurns(blocks)[0])
  assert.equal(runs.length, 2, '子 agent 卡片 / 工具都不许把 A 段切断')
  assert.deepEqual(runs[0].items.map((b) => b.id), [2, 3, 4])
})

test('缺 run id + 结算边界：按「上一次结算时的块数」切出后台 run 自己的段', () => {
  // 冷恢复 / 旧快照 / 事件缺 run_id 的形态：块上没有任何主 run id，按 run id 一刀切不出来。
  // 用户 2026-11：主 run 已 AgentEnd、后台 run 起跑 → 整轮只有一个段 → live 一翻就把
  // 已结算的那段重新展开成 Working（答案从轮尾被挪进盒子，几百像素一进一出）。
  const blocks: MsgBlock[] = [user(1), assistant(2, '结论'), assistant(3, '后台新活')]
  const turn = selectTurns(blocks)[0]
  assert.equal(turnRuns(turn).length, 1, '不给结算边界时行为不变（老路径一行不动）')
  // settleAt = 结算那一刻的 items.length（= 边界下标）：结算时只有 assistant(2) → 1
  const runs = turnRuns(turn, 1)
  assert.equal(runs.length, 2, '结算边界之后新到的块切出第二段')
  assert.deepEqual(runs.map((r) => r.items.map((b) => b.id)), [[2], [3]])
})

test('缺 run id：结算边界在两端时不分段（没有「新到的块」可切）', () => {
  const blocks: MsgBlock[] = [user(1), assistant(2, '结论')]
  const turn = selectTurns(blocks)[0]
  assert.equal(turnRuns(turn, 0).length, 1)
  assert.equal(turnRuns(turn, turn.items.length).length, 1, '结算之后什么都没来 → 还是一段')
})

test('连续两次运行都没交付（没有正文/记录/产出）→ 合成一个盒子', () => {
  // 用户 2026-11 截图：一次用户输入底下排了 4 个 Worked，其中三个 0 calls、没有正文 ——
  // 那些不是三次交付，只是一次运行被回执/续跑切出来的 run 边界。没有交付的段不单开盒子。
  const blocks: MsgBlock[] = [
    user(1),
    asstThinking(2, 'A'),   // 只有思考
    toolOf(3, 'A'),
    asstThinking(4, 'B'),   // 续跑：也只有思考 + 一次工具
    toolOf(5, 'B'),
  ]
  const turn = selectTurns(blocks)[0]
  const runs = turnRuns(turn)
  assert.equal(runs.length, 1, '没有交付 → 一个段 = 一个 Worked')
  assert.deepEqual(runs[0].items.map((b) => b.id), [2, 3, 4, 5])
})

test('后一段才有交付：前一段并进同一个盒子，盒子仍然只有一个', () => {
  const blocks: MsgBlock[] = [
    user(1),
    asstThinking(2, 'A'),
    toolOf(3, 'A'),
    toolOf(4, 'B'),
    asstOf(5, 'B', '结论'),   // 最后一段的正文 = 整轮结论，在盒子外面
  ]
  const runs = turnRuns(selectTurns(blocks)[0])
  assert.equal(runs.length, 1)
})

test('上一段交付了正文 → 仍然切段（它的结论要留在两个盒子之间的时间线上）', () => {
  const blocks: MsgBlock[] = [user(1), asstOf(2, 'A', '第一段结论'), toolOf(3, 'A'), asstThinking(4, 'B'), toolOf(5, 'B')]
  const runs = turnRuns(selectTurns(blocks)[0])
  assert.equal(runs.length, 2)
  assert.deepEqual(runs.map((r) => r.runId), ['A', 'B'])
})

test('上一段只有记录（回执）→ 仍然切段（记录要落在段与段之间）', () => {
  const blocks: MsgBlock[] = [user(1), toolOf(2, 'A'), delivery(3, 'task-x', true), toolOf(4, 'B')]
  const runs = turnRuns(selectTurns(blocks)[0])
  assert.equal(runs.length, 2)
  assert.deepEqual(runs.map((r) => r.items.map((b) => b.id)), [[2, 3], [4]])
})

test('一段自己的账只认这一段里的 run（缺 runId 不再回落到整轮）', () => {
  // 用户 2026-11 截图：0 calls 的 Worked 底下写着整轮的 11.00M tokens。
  const metrics = [
    { model: 'm', input: 1_000_000, output: 0, cacheRead: 0, cacheWrite: 0, reasoning: 0, durMs: 0, runId: 'A' },
    { model: 'm', input: 10_000_000, output: 0, cacheRead: 0, cacheWrite: 0, reasoning: 0, durMs: 0, runId: 'B' },
  ] as never
  const blocks: MsgBlock[] = [user(1), asstOf(2, 'A', '第一段结论'), toolOf(3, 'A'), asstThinking(4, 'B'), toolOf(5, 'B')]
  const turn = selectTurns(blocks)[0]
  const runs = turnRuns(turn)
  assert.equal(segmentUsage(turn, runs[0], metrics).total, 1_000_000)
  const noRun: MsgBlock[] = [user(1), assistant(2, '没有 run id 的段')]
  const t2 = selectTurns(noRun)[0]
  const r2 = turnRuns(t2)
  assert.equal(segmentUsage(t2, r2[0], metrics).matched, false, '没有自己的 run 就没有账，不拿整轮充数')
})

test('有 run id 时结算边界不掺和（不许把一个边界切两次）', () => {
  const blocks: MsgBlock[] = [
    user(1),
    asstOf(2, 'A', '第一段'),
    toolOf(3, 'A'),
    asstOf(4, 'B', '第二段'),
  ]
  const turn = selectTurns(blocks)[0]
  assert.deepEqual(turnRuns(turn, 2).map((r) => r.runId), ['A', 'B'], '仍是按 run id 切的两段')
})

test('case 1 不分段：单 run 仍然是一段（老路径一行不变）', () => {
  const blocks: MsgBlock[] = [user(1), tool(2), signal(3), assistant(4, '答'), delivery(5)]
  const runs = turnRuns(selectTurns(blocks)[0])
  assert.equal(runs.length, 1)
})

test('交卷后到达的记录按段归位：只有最后一段才延后到轮尾结算之后', () => {
  // 用户 2026-11 截图：一轮跨多次运行，几条 `Task … completed` 全堆在最后一个 Worked
  // 后面。记录没有 runId，位置（到达时落在刚交卷那段的尾巴）就是归属：段间的记录跟
  // 自己那一段的盒子走，只有最后一段的才配轮尾那**一份**合并的账/卡。
  const blocks: MsgBlock[] = [
    user(1),
    asstOf(2, 'A', '第一段'),
    toolOf(3, 'A'),
    delivery(4, 'task-x', true), // A 段交卷后到达 → A 段的尾巴
    asstOf(5, 'B', '第二段'),
    toolOf(6, 'B'),
    delivery(7, 'task-y', true), // B 段（最后一段）交卷后到达 → 轮尾
  ]
  const turn = selectTurns(blocks)[0]
  const runs = turnRuns(turn)
  assert.equal(runs.length, 2, '两次运行 = 两段')
  assert.deepEqual([0, 1, 2, 3, 4, 5].map((i) => defersToTurnEnd(runs, i)),
    [false, false, false, true, true, true], 'A 段的记录跟盒子走；B 段的记录延后到轮尾')
  assert.equal(defersToTurnEnd([runs[0]], 0), true, '单段（老路径）保持全部延后')
  assert.equal(defersToTurnEnd([], 0), true, '段没切出来时保持老行为')
})

// ---------------------------------------------------------------------------
// 时长口径：workedMs（可归因片段之和）vs wallMs（真墙钟 = AgentEnd.DurationMS）
//
// 2026-11-04 实名报障：Worked for 26m28s < LLM 31m48s。两个量取自两把尺子 ——
// 「Worked/turn」是 Σ 块 durMs（漏掉无正文的工具调用轮、并行工具还重复计入），
// 「LLM」是 Σ 每次调用的区间长度。修复后结算态优先用 wallMs。
// ---------------------------------------------------------------------------
const assistantWith = (id: number, text: string, extra: Record<string, unknown>): MsgBlock =>
  ({ kind: 'assistant', id, text, thinking: '', ...extra }) as MsgBlock
const toolWith = (id: number, durMs: number, runId?: string): MsgBlock =>
  ({ kind: 'tool', id, toolId: 't' + id, name: 'bash', status: 'done', durMs, ...(runId ? { runId } : {}) }) as MsgBlock

test('statsOfItems：wallMs 按 run 取一次 AgentEnd 时长，多 run 相加', () => {
  const items: MsgBlock[] = [
    // 同一 run 的两条 assistant 块都带 agentDurMs（只应计一次）
    assistantWith(1, '', { runId: 'r1', agentDurMs: 60_000, durMs: 20_000 }),
    assistantWith(2, 'ok', { runId: 'r1', agentDurMs: 60_000, durMs: 40_000 }),
    // 第二轮（agent_resume 原地续跑 = 新 run）→ 串行相加
    assistantWith(3, 'again', { runId: 'r2', agentDurMs: 30_000, durMs: 10_000 }),
  ]
  const st = statsOfItems(items)
  assert.equal(st.workedMs, 70_000) // Σ 块 durMs（不带 agentDurMs 的块不参与）
  assert.equal(st.wallMs, 90_000) // 60s + 30s：真墙钟
})

test('statsOfItems：没有 AgentEnd 时长（还在跑 / 旧数据）时 wallMs 缺省', () => {
  const st = statsOfItems([assistantWith(1, '', { runId: 'r1', durMs: 1000 }), toolWith(2, 2000, 'r1')])
  assert.equal(st.workedMs, 3000)
  assert.equal(st.wallMs, undefined)
})

test('statsOfItems：纯工具调用轮（无正文）的 durMs 也进 workedMs', () => {
  // 回归：store 曾把 durMs 关在「有正文」的门里 → 181 个工具轮整段漏计。
  const st = statsOfItems([assistantWith(1, '', { runId: 'r1', durMs: 5000 }), toolWith(2, 1000, 'r1')])
  assert.equal(st.workedMs, 6000)
  assert.ok(st.workedMs >= 5000, '工具轮的空正文块必须计入工作耗时')
})

// —— 交卷之后的手动 /compact：命令会新开一次 run，里面只有压缩 + compress_end 信号 ——
//
// 这次 run 没有正文、没有工具，唯一的内容就是压缩。按 run 切段时它独占一段，
// 旧口径画出来是「Worked · 1 compress」（时长为空）：读起来像「又干了一段活」，
// 而事实是「交卷之后做了一次上下文维护」。新口径 = 整段是**通知**，两个块都走时间线。
const compress = (id: number, runId?: string, before = 457, after = 16): MsgBlock => ({
  kind: 'compression', id, before, after, ...(runId ? { runId } : {}),
})
const compressSignal = (id: number): MsgBlock => ({
  kind: 'signal', id, fact: { sig: 'compact', etype: 'compress_end', before: 457, after: 16 }, afterRun: false,
})
const postCompactBlocks: MsgBlock[] = [
  user(1),
  tool(2, 'done', 'run-1'),
  assistant(3, '干完了', false, 'run-1'),
  compress(4, 'run-c'),
  compressSignal(5), // 记录没有 runId：跟着当前段走（与 turnRuns 同一规则）
]

test('交卷后 compact：整段是通知（压缩 + compress_end 信号），不画 Worked 盒子', () => {
  const turn = selectTurns(postCompactBlocks)[0]
  const runs = turnRuns(turn)
  assert.equal(runs.length, 2, '压缩是一次独立运行 = 独立一段')
  assert.deepEqual(runs.map((r) => r.runId), ['run-1', 'run-c'])
  const notice = noticeBlocks(turn)
  assert.deepEqual([...notice].map((b) => b.id), [4, 5], '压缩块与它那一行信号都属于通知')
  // 段内没有盒子内容 → Narrative 的 boxAt 不会给这一段建盒子
  assert.deepEqual(runWorkBody(runs[1].items, {
    answer: turn.answer, answerAt: turn.items.indexOf(turn.answer as MsgBlock), offset: runs[1].start, notice,
  }), [])
  // 第一段不受影响（盒子仍是它自己的工具行）
  assert.deepEqual(runWorkBody(runs[0].items, {
    answer: turn.answer, answerAt: turn.items.indexOf(turn.answer as MsgBlock), offset: runs[0].start, notice,
  }).map((b) => b.id), [2])
})

test('交卷后 compact：两个通知都在时间线上，且排在结算之前（不被当成 postRecord 延后）', () => {
  const turn = selectTurns(postCompactBlocks)[0]
  const outside = outsideOf(turn)
  assert.equal(outside(postCompactBlocks[3], 3), true, '压缩块在盒子外面')
  assert.equal(outside(postCompactBlocks[4], 4), true, 'compress_end 信号在盒子外面')
  // 顺序：结论 → 压缩 → 信号（Narrative 的 plan 按 turn.items 顺序落）
  assert.deepEqual(foldedBody(turn).map((b) => b.id), [3, 4, 5])
})

test('交卷后 compact：不进 workBody / hasWork，但数值口径与压缩前完全一致', () => {
  const turn = selectTurns(postCompactBlocks)[0]
  assert.deepEqual(workBody(turn).map((b) => b.id), [2], '压缩与信号都不在盒子里')
  const before: MsgBlock[] = postCompactBlocks.slice(0, 3)
  const a = statsOfItems(before)
  const b = statsOfItems(turn.items)
  // 压缩不贡献时长（workedMs 只数 assistant/tool 的 durMs；wallMs 只认 agentDurMs），
  // 只多一条 compressions 计数 —— 所以把盒子拿掉不会改变 Worked / turn 的任何数值。
  assert.equal(b.workedMs, a.workedMs)
  assert.equal(b.wallMs, a.wallMs)
  assert.equal(b.calls, a.calls)
  assert.equal(b.compressions, 1)
  const pure = selectTurns([user(1), compress(2, 'run-c'), compressSignal(3)])[0]
  assert.equal(hasWork(pure, noticeBlocks(pure)), false, '纯压缩轮没有工作内容 → 不画盒子')
})

test('运行中的自动压缩仍属于过程：留在盒子里，不算通知', () => {
  // 同一个 run 里有工具 / 正文 → 这次压缩是「跑的时候压的」，不是交卷后的维护。
  const blocks: MsgBlock[] = [user(1), tool(2, 'done', 'run-1'), compress(3, 'run-1'), assistant(4, '结果', false, 'run-1')]
  const turn = selectTurns(blocks)[0]
  assert.equal(noticeBlocks(turn).size, 0)
  assert.deepEqual(workBody(turn).map((b) => b.id), [2, 3], '过程里的压缩留在盒子里')
})
