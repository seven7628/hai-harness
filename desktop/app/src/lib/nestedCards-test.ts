// nestedCards-test.ts —— 嵌套任务卡的视图规则单测（node:test，无 DOM/Electron）。
//
// 为什么这些断言值得有：这条规则决定「一张卡画在哪一页」。画错的两种形态都很像正常：
//   · 孙卡画在主转录 → 看起来只是「主智能体多派了几个子任务」（用户 2026-11 的报障），
//     而且会连带出一个只有它的「0 calls」空盒子；
//   · 父卡不在窗口时把孙卡也藏掉 → 卡凭空消失，翻页也找不回来（信息缺失比错位更糟）。
// 另外 parentTaskId 是外部数据（checkpoint / 事件日志），成环时必须还能画出东西。
//
// 运行：esbuild bundle 后 node --test（见 package.json 的 test:nested-cards）。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import type { MsgBlock, UsageAgg } from '../store/useAppStore'
import { childrenOf, nestCards, parentAnchorOf, placeChildCards } from './nestedCards'

const usage = (): UsageAgg => ({
  input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0,
})

const agent = (
  id: number,
  runId: string,
  over: Partial<Extract<MsgBlock, { kind: 'agent' }>> = {},
): Extract<MsgBlock, { kind: 'agent' }> => ({
  kind: 'agent', id, runId, label: runId, status: 'running', spawnedAt: id,
  usage: usage(), items: [], ...over,
})

const task = (
  id: number,
  taskId: string,
  over: Partial<Extract<MsgBlock, { kind: 'async_task' }>> = {},
): Extract<MsgBlock, { kind: 'async_task' }> => ({
  kind: 'async_task', id, taskId, label: taskId, status: 'running', startedAt: id, ...over,
})

test('本层的卡（无 parentTaskId）都留下', () => {
  const blocks: MsgBlock[] = [agent(1, 'r1'), task(2, 't1')]
  const v = nestCards(blocks)
  assert.deepEqual(v.shown.map((b) => b.id), [1, 2])
  assert.equal(v.childrenByTask.size, 0)
  assert.equal(v.hidden.size, 0)
})

test('孙卡摘掉并归到父卡（主转录只画本层）', () => {
  const blocks: MsgBlock[] = [
    agent(1, 'r-parent', { taskId: 'task-parent', parentRunId: 'root-run' }),
    agent(2, 'r-child', { taskId: 'task-child', parentTaskId: 'task-parent', parentRunId: 'r-parent' }),
  ]
  const v = nestCards(blocks)
  assert.deepEqual(v.shown.map((b) => b.id), [1])
  assert.deepEqual([...v.hidden], [2])
  assert.deepEqual(v.childrenByTask.get('task-parent')?.map((b) => b.taskId), ['task-child'])
  // 子会话页读的就是这条
  assert.deepEqual(childrenOf(blocks, 'task-parent').map((b) => b.taskId), ['task-child'])
  assert.deepEqual(childrenOf(blocks, 'task-unknown'), [])
})

test('父卡不在窗口里：孙卡留在本层（宁可多一条也不丢）', () => {
  const blocks: MsgBlock[] = [
    agent(1, 'r1'),
    agent(2, 'r-grand', { taskId: 'task-grand', parentTaskId: 'task-trimmed-away' }),
  ]
  const v = nestCards(blocks)
  assert.deepEqual(v.shown.map((b) => b.id), [1, 2])
  assert.equal(v.hidden.size, 0)
})

test('链式：孙卡的孙卡也归到同一个父（父卡自己被摘时，孙卡跟它走）', () => {
  const blocks: MsgBlock[] = [
    agent(1, 'r-root', { taskId: 'task-root' }),
    agent(2, 'r-child', { taskId: 'task-child', parentTaskId: 'task-root' }),
    agent(3, 'r-grand', { taskId: 'task-grand', parentTaskId: 'task-child' }),
  ]
  const v = nestCards(blocks)
  assert.deepEqual(v.shown.map((b) => b.id), [1])
  assert.deepEqual(v.childrenByTask.get('task-root')?.map((b) => b.taskId), ['task-child'])
  assert.deepEqual(v.childrenByTask.get('task-child')?.map((b) => b.taskId), ['task-grand'])
})

test('成环（坏数据）：两张卡都留在本层，不能两边都不画', () => {
  const blocks: MsgBlock[] = [
    agent(1, 'r-a', { taskId: 'task-a', parentTaskId: 'task-b' }),
    agent(2, 'r-b', { taskId: 'task-b', parentTaskId: 'task-a' }),
  ]
  const v = nestCards(blocks)
  assert.deepEqual(v.shown.map((b) => b.id), [1, 2])
  assert.equal(v.hidden.size, 0)
})

test('同一子任务的双来源（占位 + agent 卡）只归一次，agent 卡胜出', () => {
  const blocks: MsgBlock[] = [
    agent(1, 'r-parent', { taskId: 'task-parent' }),
    task(2, 'task-child', { parentTaskId: 'task-parent', startedAt: 20 }),
    agent(3, 'r-child', { taskId: 'task-child', parentTaskId: 'task-parent', spawnedAt: 21 }),
  ]
  const v = nestCards(blocks)
  assert.deepEqual(v.shown.map((b) => b.id), [1])
  const kids = v.childrenByTask.get('task-parent') ?? []
  assert.equal(kids.length, 1)
  assert.equal(kids[0].kind, 'agent')
})

test('子卡按派发时刻排序（并发派发时不靠块顺序猜）', () => {
  const blocks: MsgBlock[] = [
    agent(1, 'r-parent', { taskId: 'task-parent' }),
    agent(2, 'r-late', { taskId: 'task-late', parentTaskId: 'task-parent', spawnedAt: 500 }),
    agent(3, 'r-early', { taskId: 'task-early', parentTaskId: 'task-parent', spawnedAt: 100 }),
  ]
  assert.deepEqual(
    nestCards(blocks).childrenByTask.get('task-parent')?.map((b) => b.taskId),
    ['task-early', 'task-late'],
  )
})

test('parentAnchorOf 只认任务卡（正文/工具块没有嵌套锚）', () => {
  assert.equal(parentAnchorOf(agent(1, 'r', { parentTaskId: 'task-p' })), 'task-p')
  assert.equal(parentAnchorOf(agent(1, 'r')), undefined)
  assert.equal(parentAnchorOf({ kind: 'user', id: 2, text: 'hi' }), undefined)
  assert.equal(
    parentAnchorOf({ kind: 'tool', id: 3, toolId: 'x', name: 'bash', status: 'done' }),
    undefined,
  )
})

// ——— 落点（父页里插在哪一行之后） ———

const tool = (id: number, taskId: string, startedAt: number): Extract<MsgBlock, { kind: 'tool' }> => ({
  kind: 'tool', id, toolId: 'c' + id, name: 'agent_spawn', status: 'done', taskId, startedAt,
})

test('落点 1：派发工具行（tool.taskId === 子卡.taskId）优先于时间锚', () => {
  const blocks: MsgBlock[] = [
    tool(1, 'task-child', 100),
    { kind: 'assistant', id: 2, text: '后面的话', thinking: '', streaming: false, ts: 900 },
  ]
  const child = agent(3, 'r-child', { taskId: 'task-child', parentTaskId: 'task-p', spawnedAt: 500 })
  let n = -1
  const out = placeChildCards(blocks, [child], () => n--)
  assert.deepEqual(out.map((b) => b.kind), ['tool', 'agent', 'assistant'])
  assert.equal((out[1] as Extract<MsgBlock, { kind: 'agent' }>).id, -1)
  assert.equal(blocks.length, 2) // 不改输入
})

test('落点 2：无工具行 taskId 时按时间插在最后一个「不晚于 spawnedAt」的块后', () => {
  const blocks: MsgBlock[] = [
    { kind: 'assistant', id: 1, text: '先说的话', thinking: '', streaming: false, ts: 100 },
    tool(2, 'other-task', 200),
    { kind: 'assistant', id: 3, text: '后说的话', thinking: '', streaming: false, ts: 900 },
  ]
  const child = agent(9, 'r-child', { taskId: 'task-child', parentTaskId: 'task-p', spawnedAt: 250 })
  const out = placeChildCards(blocks, [child], (() => { let n = -1; return () => n-- })())
  assert.deepEqual(out.map((b) => b.kind), ['assistant', 'tool', 'agent', 'assistant'])
})

test('落点 3：没有任何时间锚 → 追加到末尾（旧行为，不猜）', () => {
  const blocks: MsgBlock[] = [
    { kind: 'user', id: 1, text: 'hi' }, // 无 ts
    { kind: 'assistant', id: 2, text: '正文', thinking: '', streaming: false }, // 无 ts
  ]
  const child = agent(3, 'r-child', { taskId: 'task-child', parentTaskId: 'task-p', spawnedAt: 500 })
  const out = placeChildCards(blocks, [child], (() => { let n = -1; return () => n-- })())
  assert.deepEqual(out.map((b) => b.kind), ['user', 'assistant', 'agent'])
})

test('落点 4：多个子卡同锚时按派发时刻排（不靠入参顺序）', () => {
  const blocks: MsgBlock[] = [
    { kind: 'assistant', id: 1, text: '起跑', thinking: '', streaming: false, ts: 100 },
    { kind: 'assistant', id: 2, text: '收尾', thinking: '', streaming: false, ts: 900 },
  ]
  const late = agent(4, 'r-late', { taskId: 'task-late', parentTaskId: 'task-p', spawnedAt: 400 })
  const early = agent(3, 'r-early', { taskId: 'task-early', parentTaskId: 'task-p', spawnedAt: 300 })
  const out = placeChildCards(blocks, [late, early], (() => { let n = -1; return () => n-- })())
  assert.deepEqual(
    out.filter((b) => b.kind === 'agent').map((b) => (b as Extract<MsgBlock, { kind: 'agent' }>).label),
    ['r-early', 'r-late'],
  )
  // 两张都插在「起跑」之后、「收尾」之前
  assert.deepEqual(out.map((b) => b.kind), ['assistant', 'agent', 'agent', 'assistant'])
})

test('落点 5：子卡时间早于所有块 → 没有可用的「不晚于」锚也不匹配工具行 → 页尾', () => {
  const blocks: MsgBlock[] = [
    { kind: 'assistant', id: 1, text: '正文', thinking: '', streaming: false, ts: 500 },
  ]
  const child = agent(2, 'r-child', { taskId: 'task-child', parentTaskId: 'task-p', spawnedAt: 100 })
  const out = placeChildCards(blocks, [child], (() => { let n = -1; return () => n-- })())
  assert.deepEqual(out.map((b) => b.kind), ['assistant', 'agent'])
})
