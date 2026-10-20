// todoScope-test.ts —— 待办归属判定的单测（node:test，无 DOM）。
//
// 为什么钉这个：这个 bug 的表现是「子智能体里显示主会话的 TODO」，而它的成因是
// **一条无条件赋值**（todos: raw.todos ?? v.todos）。这种代码看一遍很难看出错，
// 也很容易被后续重构"顺手统一"回去。断言把两条硬约束写成可执行规格：
//   1. 子 run 的 todo_* 只能写该子 agent 的卡片，会话清单一个字节都不许动；
//   2. 子 agent 没有清单时**不回退**会话清单（返回空）。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { pickTodoList, todoTargetOf, withAgentTodos } from './todoScope'

const T = (id: string, title: string, done = false) => ({ id, title, done })

test('归属：主 run 的 todo_* → 会话清单', () => {
  assert.deepEqual(todoTargetOf(false, 'run-main'), { kind: 'session' })
  assert.deepEqual(todoTargetOf(false, undefined), { kind: 'session' }, '无 run_id 的旧事件按主 run 处理')
})

test('归属：子 run 的 todo_* → 该 agent 卡片（不是会话）', () => {
  assert.deepEqual(todoTargetOf(true, 'run-sub'), { kind: 'agent', runId: 'run-sub' })
  // 子 run 但缺 run_id（异常事件）：宁可按会话处理，也不写一张猜出来的卡片
  assert.deepEqual(todoTargetOf(true, undefined), { kind: 'session' })
})

test('取值：main 作用域用会话清单', () => {
  const session = [T('t1', '主任务')]
  assert.deepEqual(pickTodoList('main', session, [T('t9', '子任务')]), session)
})

test('取值：agent 作用域用**它自己的**清单，绝不回退会话清单', () => {
  const session = [T('t1', '主任务')]
  assert.deepEqual(pickTodoList('agent', session, [T('t9', '子任务')]), [T('t9', '子任务')])
  // 子 agent 没有清单 → 空（显示「没有待办」），而不是显示主会话的计划
  assert.deepEqual(pickTodoList('agent', session), [])
  assert.deepEqual(pickTodoList('agent', session, []), [])
})

test('写入：withAgentTodos 只改目标卡片，其它块保持同一引用', () => {
  const other = { kind: 'assistant', text: 'x' }
  const target = { kind: 'agent', runId: 'sub-1' }
  const sibling = { kind: 'agent', runId: 'sub-2' }
  const out = withAgentTodos([other, target, sibling], 'sub-1', [T('t1', '子任务')] as never)
  assert.equal(out[0], other, '无关块必须同一引用（流式重渲优化）')
  assert.equal(out[2], sibling, '别的 agent 卡不动')
  assert.notEqual(out[1], target, '目标卡是新对象')
  assert.deepEqual((out[1] as unknown as { todos: unknown }).todos, [T('t1', '子任务')])
})

test('写入：目标不存在 → 返回原数组（调用方据此跳过 setState）', () => {
  const blocks = [{ kind: 'agent', runId: 'sub-2' }]
  assert.equal(withAgentTodos(blocks, 'sub-1', [T('t1', 'x')] as never), blocks)
})
