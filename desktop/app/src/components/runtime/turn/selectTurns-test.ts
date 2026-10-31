// selectTurns-test.ts —— 简洁档「时间线 + 一个 Working 块」的归属判据（node:test 无 DOM）。
//
// 这批用例锁的是三条口径：
//   1. 记录留在时间线上（盒子外）：signal / artifacts / task_delivery / error / divider；
//   2. 工作过程进盒子：tool / agent / async_task / system / task_result / 中间轮正文；
//   3. 盒子存在 = workBody 非空（不是「有工具」）；没有盒子的轮（纯对话）正文回到时间线。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { ALWAYS_SHOWN, FOLDED_KINDS, foldedBody, hasWork, runningBody, selectTurns, workBody } from './selectTurns'
import type { MsgBlock } from '../../../store/useAppStore'

const user = (id: number, text = 'go'): MsgBlock => ({ kind: 'user', id, text })
const assistant = (id: number, text: string, streaming = false): MsgBlock => ({ kind: 'assistant', id, text, streaming, thinking: '' })
const tool = (id: number, status: 'running' | 'done' = 'done'): MsgBlock => ({ kind: 'tool', id, toolId: 't' + id, name: 'bash', status })
const delivery = (id: number, taskId = 'task-1'): MsgBlock => ({ kind: 'task_delivery', id, taskId, status: 'completed', result: 'done' })

// 用户主 run：user → 思考/工具行 → 回答；AgentEnd 之后后台任务才交付回执。
const turnWithLateDelivery: MsgBlock[] = [
  user(1),
  tool(2),
  assistant(3, '改好了'),
  delivery(4),
]

test('后台任务回执留在时间线上（盒子外面）', () => {
  assert.equal(ALWAYS_SHOWN.has('task_delivery'), true)
  assert.equal(FOLDED_KINDS.has('task_delivery'), false)
  const turn = selectTurns(turnWithLateDelivery)[0]
  // 顺序 = 事件顺序：回执晚于回答到达，就排在回答之后（filter 保持 turn.items 顺序）
  assert.deepEqual(foldedBody(turn).map((b) => b.id), [3, 4])
  assert.deepEqual(foldedBody(turn).map((b) => b.kind), ['assistant', 'task_delivery'])
})

test('同一批块不会既在盒子里又在外面（无重复渲染）', () => {
  const turn = selectTurns(turnWithLateDelivery)[0]
  const inside = new Set(workBody(turn).map((b) => b.id))
  for (const b of foldedBody(turn)) {
    if (b.kind !== 'assistant') assert.equal(inside.has(b.id), false, '记录块不得同时进盒子')
  }
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

test('signal / artifacts 与回执同属「记录」：都在外面', () => {
  for (const k of ['signal', 'artifacts', 'error', 'divider', 'task_delivery'] as const) {
    assert.equal(ALWAYS_SHOWN.has(k), true, k + ' 应留在时间线上')
  }
  // 执行阶段的载荷仍在盒子里
  for (const k of ['tool', 'agent', 'async_task', 'system', 'task_result'] as const) {
    assert.equal(FOLDED_KINDS.has(k), true, k + ' 应进工作过程')
  }
})
