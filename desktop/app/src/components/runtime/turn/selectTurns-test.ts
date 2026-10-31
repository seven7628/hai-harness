// selectTurns-test.ts —— 简洁档「时间线 + 一个 Working 块」的归属判据（node:test 无 DOM）。
//
// 这批用例锁的是三条口径：
//   1. 记录（signal / task_delivery）的位置看**到达时刻**：运行中到达 → 盒内、按事件
//      顺序夹在工作行之间；AgentEnd 之后到达 → 时间线上（盒子已折叠，折进去等于藏了）；
//   2. 结构性/结论性块始终在外面：error / divider / artifacts；
//   3. 盒子存在 = workBody 非空（不是「有工具」）；没有盒子的轮（纯对话）正文回到时间线。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { ALWAYS_SHOWN, FOLDED_KINDS, foldedBody, hasWork, runningBody, selectTurns, workBody } from './selectTurns'
import type { MsgBlock } from '../../../store/useAppStore'

const user = (id: number, text = 'go'): MsgBlock => ({ kind: 'user', id, text })
const assistant = (id: number, text: string, streaming = false): MsgBlock => ({ kind: 'assistant', id, text, streaming, thinking: '' })
const tool = (id: number, status: 'running' | 'done' = 'done'): MsgBlock => ({ kind: 'tool', id, toolId: 't' + id, name: 'bash', status })
const delivery = (id: number, taskId = 'task-1', afterRun?: boolean): MsgBlock => ({
  kind: 'task_delivery', id, taskId, status: 'completed', result: 'done', ...(afterRun ? { afterRun: true } : {}),
})
const signal = (id: number, afterRun?: boolean): MsgBlock => ({
  kind: 'signal',
  id,
  fact: { sig: 'promote', etype: 'task_promoted', name: 'bash' },
  ...(afterRun ? { afterRun: true } : {}),
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
