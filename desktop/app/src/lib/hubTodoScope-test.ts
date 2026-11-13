// hubTodoScope-test.ts —— Project 待办在 Hub 内联卡上的纯投影单测（node:test，无 DOM）。
//
// 为什么单独钉一遍：这些函数决定「哪条清单属于谁、要不要显示、显示几条」。它们一旦被
// 后续重构"顺手统一"（比如未知 run 回退主会话、todos 缺失当空清单），表现是**不报错**
// 的错数据：子智能体的清单显示成主会话的、或者一张卡被每条普通工具事件清空。断言把
// 这几条硬约束写成可执行规格。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  HUB_RUN_NAME_MAX, HUB_TODO_MAX_CARDS, HUB_TODO_MAX_ITEMS, HUB_TODO_MAX_RUNS,
  activeTodos, forgetTodoSnapshots, hubRunKey, isLiveTaskStatus, isProjectSession,
  isTerminalTaskStatus, mergeTodoSnapshot, normalizeTodos, putRunName, putTodoSnapshot,
  resolveRunName, resolveTaskForTool, scopeMatch, shortRunName,
  type HubRunNames, type HubTodoSnapshot,
} from './hubTodoScope'
import type { HubTask } from '../hub/types'

const WS = '/w/proj'
const SID = 's_proj'
const item = (id: string, title: string, done = false) => ({ id, title, done })

const snapEv = (runId: string, todos: unknown, extra: Record<string, unknown> = {}) => ({
  session_id: SID, workspace: WS, run_id: runId, timestamp: '2026-05-01T10:00:00.000Z',
  todos, ...extra,
})

const task = (id: string, extra: Partial<HubTask> = {}): HubTask => ({
  id,
  title: `任务 ${id}`,
  status: 'running',
  origin: { kind: 'dm', agent_id: 'ag_res', agent_name: 'Researcher' },
  target: { kind: 'workspace', workspace: WS, session_id: SID },
  updated_at: '2026-05-01T10:00:00Z',
  ...extra,
})

test('会话判据：Hub 自己的会话不记，Project 会话都收', () => {
  assert.equal(isProjectSession('s_proj'), true)
  assert.equal(isProjectSession('s_anything'), true)
  assert.equal(isProjectSession('hub.ag_a.dm'), false, 'Hub DM 会话不记（清单已在 Hub 自己的 UI 里）')
  assert.equal(isProjectSession('hub.ag_a.g.grp_1'), false, 'Hub 群会话不记')
  assert.equal(isProjectSession(''), false)
  assert.equal(isProjectSession(undefined), false)
})

test('状态：只在 dispatched/running 出卡；终态按 agenthub.TaskStatus.Terminal', () => {
  assert.equal(isLiveTaskStatus('dispatched'), true)
  assert.equal(isLiveTaskStatus('running'), true)
  assert.equal(isLiveTaskStatus('queued'), false, 'queued 还没跑，没有清单可显示')
  assert.equal(isLiveTaskStatus('completed'), false)
  assert.equal(isLiveTaskStatus(undefined), false)
  for (const s of ['completed', 'failed', 'interrupted', 'delivered', 'canceled', 'blocked_budget', 'declined']) {
    assert.equal(isTerminalTaskStatus(s), true, `${s} 是终态`)
  }
  assert.equal(isTerminalTaskStatus('running'), false)
})

test('作用域：群看 origin.group_id，DM 看「无群 + 发起者」', () => {
  const dm = task('t1')
  const grp = task('t2', { origin: { kind: 'group', group_id: 'g1', agent_id: 'ag_res' } })
  assert.equal(scopeMatch(dm, { kind: 'dm', agentId: 'ag_res' }), true)
  assert.equal(scopeMatch(dm, { kind: 'dm', agentId: 'ag_other' }), false, '别人的 DM 不显示')
  assert.equal(scopeMatch(dm, { kind: 'dm', agentId: null }), false, '没有 activeId 时不显示')
  assert.equal(scopeMatch(grp, { kind: 'dm', agentId: 'ag_res' }), false, '群任务不进发起者 DM')
  assert.equal(scopeMatch(grp, { kind: 'group', groupId: 'g1' }), true)
  assert.equal(scopeMatch(grp, { kind: 'group', groupId: 'g2' }), false)
  assert.equal(scopeMatch(dm, { kind: 'group', groupId: 'g1' }), false, 'DM 任务不进群')
  assert.equal(scopeMatch({} as HubTask, { kind: 'dm', agentId: 'ag_res' }), false)
})

test('认领：任务有 session 就用 session，没有才用 workspace 兜底', () => {
  const t = task('t1')
  assert.equal(resolveTaskForTool(SID, WS, [t])?.id, 't1')
  assert.equal(resolveTaskForTool('s_other', WS, [t]), null, '有 session 的任务不按 workspace 认领')
  const noSid = task('t2', { target: { kind: 'workspace', workspace: WS } })
  assert.equal(resolveTaskForTool('s_whatever', WS, [noSid])?.id, 't2', '任务没有 session 时按 workspace 兜底')
  assert.equal(resolveTaskForTool('s_whatever', '/w/other', [noSid]), null)
  assert.equal(
    resolveTaskForTool(SID, WS, [task('t3', { target: { kind: 'agent', agent_id: 'ag_x' } })]),
    null,
    'agent 目标的任务不参与（Hub 自己的智能体任务没有 Project 会话）',
  )
})

test('认领：同一 target 多条取最新非终态，没有非终态才退回终态', () => {
  const old = task('old', { updated_at: '2026-05-01T10:00:00Z' })
  const fresh = task('fresh', { updated_at: '2026-05-01T10:05:00Z' })
  assert.equal(resolveTaskForTool(SID, WS, [old, fresh])?.id, 'fresh')
  const dead = task('dead', { status: 'completed', updated_at: '2026-05-01T11:00:00Z' })
  assert.equal(resolveTaskForTool(SID, WS, [old, dead])?.id, 'old', '终态更新但不在跑：优先级低于非终态')
  assert.equal(resolveTaskForTool(SID, WS, [old, fresh, dead])?.id, 'fresh')
  assert.equal(resolveTaskForTool(SID, WS, [dead])?.id, 'dead', '只剩终态时仍能认领（调用方据此收卡）')
})

test('run 名：主会话 / 子智能体名 / 未知 run 短 id（绝不回退主会话）', () => {
  const names = {
    [hubRunKey(SID, 'main1')]: { root: true, name: '' },
    [hubRunKey(SID, 'sub1')]: { root: false, name: 'Data Scout' },
    [hubRunKey(SID, 'sub2')]: { root: false, name: '' },
  }
  assert.equal(resolveRunName('main1', SID, names), '主会话')
  assert.equal(resolveRunName('sub1', SID, names), 'Data Scout')
  assert.equal(resolveRunName('sub2', SID, names), shortRunName('sub2'), '子 run 没名字时用短 id')
  assert.equal(resolveRunName('ghost0deadbeef', SID, names), 'run_ghost0de')
  assert.equal(resolveRunName('ghost0deadbeef', SID, names).includes('主会话'), false, '未知 run 绝不显示主会话')
  assert.equal(resolveRunName('main1', 's_other', names), shortRunName('main1'), 'run 名按会话隔离（不跨会话命中）')
  assert.equal(resolveRunName('', SID, names), '')
  assert.equal(shortRunName('run_abcdefgh1234'), 'run_abcdefgh', '已经是 run_ 前缀时只取后 8 位')
})

test('合并：缺字段不改、[]/null 清空、旧 timestamp/seq 不覆盖新', () => {
  assert.equal(mergeTodoSnapshot(undefined, { session_id: SID, run_id: 'r1' }), null, '没有 todos 字段 = 无快照')
  assert.equal(mergeTodoSnapshot(undefined, snapEv('r1', [item('t1', 'A')], { session_id: 'hub.ag.dm' })), null, 'Hub 会话不记')
  assert.equal(mergeTodoSnapshot(undefined, { session_id: SID, todos: [] }), null, '没有 run_id 不记')

  const first = mergeTodoSnapshot(undefined, snapEv('r1', [item('t1', 'A'), { id: 't2', title: 'B', done: true, blocked_by: ['t1'] }]))
  assert.ok(first)
  assert.equal(first!.key, hubRunKey(SID, 'r1'))
  assert.deepEqual(first!.todos.map((t) => t.title), ['A', 'B'])
  assert.deepEqual(first!.todos[1].blockedBy, ['t1'], 'blocked_by 归一成 blockedBy')

  const cleared = mergeTodoSnapshot(first!, snapEv('r1', [], { timestamp: '2026-05-01T10:00:01.000Z', arrival: 2 }))
  assert.deepEqual(cleared!.todos, [], '[] = 清空生效')
  assert.equal(cleared!.at, 2)

  const nul = mergeTodoSnapshot(first!, snapEv('r1', null, { timestamp: '2026-05-01T10:00:02.000Z', arrival: 3 }))
  assert.deepEqual(nul!.todos, [], 'null 与 [] 同义')

  const newer = mergeTodoSnapshot(first!, snapEv('r1', [item('t9', '新')], { timestamp: '2026-05-01T10:00:05.000Z', arrival: 4 }))
  assert.equal(newer!.todos[0].id, 't9')
  const stale = mergeTodoSnapshot(newer!, snapEv('r1', [item('t1', '旧')], { timestamp: '2026-05-01T10:00:03.000Z', arrival: 5 }))
  assert.equal(stale, null, '旧 timestamp 不覆盖新（即使后到）')

  const seqNew = { ...newer!, seq: 10 }
  assert.equal(
    mergeTodoSnapshot(seqNew, snapEv('r1', [item('t1', '旧')], { seq: 9, timestamp: '2026-05-01T10:00:09.000Z', arrival: 6 })),
    null,
    '旧 seq 不覆盖新',
  )
  assert.ok(mergeTodoSnapshot(seqNew, snapEv('r1', [item('t1', '新')], { seq: 11, timestamp: '2026-05-01T10:00:09.000Z', arrival: 7 })))

  const replay = { ...newer!, at: 8 }
  assert.equal(mergeTodoSnapshot(replay, snapEv('r1', replay.todos, { timestamp: replay.ts, arrival: 8 })), replay, '同到达序同内容：返回原引用（不触发重渲）')
  const nextAt = mergeTodoSnapshot(replay, snapEv('r1', replay.todos, { timestamp: replay.ts, arrival: 9 }))
  assert.notEqual(nextAt, replay, '新到达序要换对象（LRU 排序要用）')

  // 迟到的更旧 at（重放/乱序）也不许回退。
  assert.equal(mergeTodoSnapshot(replay, snapEv('r1', [item('t1', 'x')], { arrival: 1 })), null, '旧到达序不覆盖')
})

test('归一化：坏数据不抛、空项丢掉', () => {
  assert.deepEqual(normalizeTodos(undefined), [])
  assert.deepEqual(normalizeTodos({ not: 'array' }), [])
  assert.deepEqual(normalizeTodos([null, 3, 'x']), [])
  assert.deepEqual(normalizeTodos([{ id: 'a', title: 'A', done: 1 }]), [{ id: 'a', title: 'A', done: false }], 'done 只认 true')
  assert.deepEqual(normalizeTodos([{ title: '只有标题' }]), [{ id: '只有标题', title: '只有标题', done: false }], '缺 id 用标题兜底（React key 不能空）')
})

test('表：按 run 有界（LRU），同一对象不换引用', () => {
  let table: Record<string, HubTodoSnapshot> = {}
  for (let i = 0; i < HUB_TODO_MAX_RUNS + 10; i++) {
    const snap = mergeTodoSnapshot(undefined, snapEv(`r${i}`, [item(`t${i}`, `x${i}`)], { arrival: i + 1 }))!
    table = putTodoSnapshot(table, snap)
  }
  assert.equal(Object.keys(table).length, HUB_TODO_MAX_RUNS)
  assert.equal(table[hubRunKey(SID, 'r0')], undefined, '最旧的被淘汰')
  assert.ok(table[hubRunKey(SID, `r${HUB_TODO_MAX_RUNS + 9}`)])
  const same = putTodoSnapshot(table, table[hubRunKey(SID, `r${HUB_TODO_MAX_RUNS + 5}`)])
  assert.equal(same, table, '同一条快照重复 put 返回原表')
})

test('回收：按会话/按 run 清，空操作返回原引用', () => {
  let table: Record<string, HubTodoSnapshot> = {}
  for (const [sid, run] of [[SID, 'r1'], [SID, 'r2'], ['s_other', 'r3']] as const) {
    const snap = mergeTodoSnapshot(undefined, {
      session_id: sid, workspace: WS, run_id: run, todos: [item('t', 'x')], arrival: 1,
    })!
    table = putTodoSnapshot(table, snap)
  }
  assert.equal(forgetTodoSnapshots(table, {}), table, '什么都没给 = 不清（返回原表）')
  const bySession = forgetTodoSnapshots(table, { sessionId: SID })
  assert.equal(Object.keys(bySession).length, 1, '整个会话的 run 都清掉')
  assert.ok(bySession[hubRunKey('s_other', 'r3')])
  const byRun = forgetTodoSnapshots(table, { runIds: ['r3'] })
  assert.equal(Object.keys(byRun).length, 2, '按 run 只清那一条')
  assert.equal(forgetTodoSnapshots(byRun, { sessionId: 's_none' }), byRun, '没命中返回原引用')
})

test('run 名表：有界、同值不换引用、root 可更新', () => {
  let names = putRunName({}, hubRunKey(SID, 'r1'), { root: false, name: ' A ' })
  assert.deepEqual(names[hubRunKey(SID, 'r1')], { root: false, name: 'A' }, '去空格')
  assert.equal(putRunName(names, hubRunKey(SID, 'r1'), { root: false, name: 'A' }), names, '同值不换引用')
  names = putRunName(names, hubRunKey(SID, 'r1'), { root: true, name: '' })
  assert.deepEqual(names[hubRunKey(SID, 'r1')], { root: true, name: '' }, '同一个 run 后来标成根 run')
  let big: HubRunNames = {}
  for (let i = 0; i < HUB_RUN_NAME_MAX + 5; i++) {
    big = putRunName(big, hubRunKey(SID, `r${i}`), { root: false, name: `n${i}` })
  }
  assert.equal(Object.keys(big).length, HUB_RUN_NAME_MAX)
  assert.equal(big[hubRunKey(SID, 'r0')], undefined, '最旧的 run 名被淘汰')
})

test('显示：终态收卡、空清单不出卡、每任务只出一张（最新 run）', () => {
  const scope = { kind: 'dm' as const, agentId: 'ag_res' }
  const t = task('t1')
  const main = mergeTodoSnapshot(undefined, snapEv('main1', [item('a', '主'), item('b', '主 2', true)], { arrival: 1 }))!
  assert.equal(activeTodos([t], [main], scope).length, 1)
  assert.equal(activeTodos([task('t1', { status: 'completed' })], [main], scope).length, 0, '终态立刻收卡')
  assert.equal(activeTodos([t], [mergeTodoSnapshot(undefined, snapEv('main1', [], { arrival: 1 }))!], scope).length, 0, '空清单不出 0/0 的卡')
  assert.equal(activeTodos([t], [main], { kind: 'group', groupId: 'g1' }).length, 0, '作用域外不显示')
  assert.equal(activeTodos([], [main], scope).length, 0, '没有任务就没有卡（快照先到也一样）')

  const sub = mergeTodoSnapshot(undefined, snapEv('sub1', [item('c', '子')], { arrival: 2 }))!
  const one = activeTodos([t], [main, sub], scope)
  assert.equal(one.length, 1, '同一任务只出一张卡')
  assert.equal(one[0].runId, 'sub1', '取最新 run')
  assert.equal(one[0].runName, shortRunName('sub1'), '没有 agent_start 时用短 id')
  const named = activeTodos([t], [main, sub], scope, { [hubRunKey(SID, 'sub1')]: { root: false, name: 'Scout' } })
  assert.equal(named[0].runName, 'Scout')
  assert.equal(named[0].mainRun, false)
  assert.equal(named[0].runKnown, true, 'agent_start 见过 = 归属已知')
  assert.equal(one[0].runKnown, false, '只有 run id 时归属未知（不冒充子智能体）')
  const root = activeTodos([t], [main], scope, { [hubRunKey(SID, 'main1')]: { root: true } })
  assert.equal(root[0].runName, '主会话')
  assert.equal(root[0].mainRun, true)
})

test('显示：dispatch.run_id 兜底主 run（没见过 agent_start 的恢复场景）', () => {
  const scope = { kind: 'dm' as const, agentId: 'ag_res' }
  const snap = mergeTodoSnapshot(undefined, snapEv('main1', [item('a', 'A')], { arrival: 1 }))!
  const card = activeTodos([task('t1', { dispatch: { run_id: 'main1' } })], [snap], scope)[0]
  assert.equal(card.mainRun, true)
  assert.equal(card.runName, '主会话')
  const other = activeTodos([task('t1', { dispatch: { run_id: 'zzz' } })], [snap], scope)[0]
  assert.equal(other.mainRun, false, 'dispatch 指向别的 run 时不冒充主会话')
  assert.equal(other.runKnown, false, '没有 agent_start 时归属未知')
})

test(`显示：最多 ${HUB_TODO_MAX_ITEMS} 条 + N，最多 ${HUB_TODO_MAX_CARDS} 张卡，最新在前`, () => {
  const scope = { kind: 'dm' as const, agentId: 'ag_res' }
  const many = Array.from({ length: 6 }, (_, i) => item(`t${i}`, `第 ${i} 条`, i === 0))
  const snap = mergeTodoSnapshot(undefined, snapEv('main1', many, { arrival: 1 }))!
  const card = activeTodos([task('t1')], [snap], scope)[0]
  assert.equal(card.todos.length, HUB_TODO_MAX_ITEMS)
  assert.equal(card.more, 6 - HUB_TODO_MAX_ITEMS)
  assert.equal(card.total, 6)
  assert.equal(card.done, 1)
  assert.deepEqual(card.todos.map((t) => t.id), ['t0', 't1', 't2'], '按模型顺序取前几条')

  const tasks: HubTask[] = []
  const snaps: HubTodoSnapshot[] = []
  for (let i = 0; i < HUB_TODO_MAX_CARDS + 2; i++) {
    const sid = `s_${i}`
    tasks.push(task(`t${i}`, { target: { kind: 'workspace', workspace: WS, session_id: sid } }))
    snaps.push(mergeTodoSnapshot(undefined, {
      session_id: sid, workspace: WS, run_id: `r${i}`, todos: [item(`x${i}`, 'x')], arrival: i + 1,
    })!)
  }
  const cards = activeTodos(tasks, snaps, scope)
  assert.equal(cards.length, HUB_TODO_MAX_CARDS)
  assert.equal(cards[0].taskId, `t${HUB_TODO_MAX_CARDS + 1}`, '最新 run 的卡在最前')
})

test('显示：归属字段（谁 · workspace/session · 主会话/子智能体）齐全', () => {
  const scope = { kind: 'dm' as const, agentId: 'ag_res' }
  const snap = mergeTodoSnapshot(undefined, snapEv('sub1', [item('a', 'A')], { arrival: 1 }))!
  const card = activeTodos([task('t1')], [snap], scope, { [hubRunKey(SID, 'sub1')]: { root: false, name: 'Scout' } })[0]
  assert.equal(card.agentName, 'Researcher')
  assert.equal(card.workspace, WS)
  assert.equal(card.sessionId, SID)
  assert.equal(card.runName, 'Scout')
  assert.equal(card.taskTitle, '任务 t1')
  assert.ok(activeTodos([task('t1', { origin: { kind: 'dm', agent_id: 'ag_x' } })], [snap], scope).length === 0, '归属按发起者过滤')
})
