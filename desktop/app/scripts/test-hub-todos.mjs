#!/usr/bin/env node
// 场景2：Project 待办在 Hub DM/群内联展示的**投递链路**验收（Node 直跑，无需浏览器）。
//
// 钉的是 store.handleEvent 的两个入口与显示口径 —— 这两处最容易在重构中被"顺手统一"：
//   1. 收集在**所有 sid 闸门之前**：`sid` 是 Hub 自己的会话，Project 会话永远对不上，
//      先闸门后处理 = 整条事件丢掉（这个功能最初就是这样"什么都看不到"的）；
//   2. 只收 Project 会话（`hub.` 开头的 Hub 会话一个字节都不记），只读、不写
//      hub_message / refs（断言 messages/groupMessages 始终为空）。
// 事件行按线上真实形状构造：main.go emit 给每行注入顶层 session_id/workspace，
// tool_run_end 额外带 run_id/todos（bridge 只在 todo_*/编排调用上注入 todos）。
import { build } from 'esbuild'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const srcDir = join(__dirname, '..', 'src')
const result = await build({
  stdin: {
    contents: [
      "export * as todos from './lib/hubTodoScope.ts'",
      "export * as store from './hub/useHubStore.ts'",
    ].join('\n'),
    resolveDir: srcDir,
    sourcefile: 'hub-todos-entry.ts',
    loader: 'ts',
  },
  bundle: true,
  platform: 'node',
  format: 'esm',
  write: false,
  logLevel: 'silent',
  // useHubStore 会把 mock transport 一起拉进来；Vite 的 ?raw/?url/?no-inline 在
  // esbuild 里没有对应 loader，给它们一个空模块（本测试不渲染品牌图）。
  plugins: [{
    name: 'stub-vite-assets',
    setup(build) {
      build.onResolve({ filter: /\?(raw|url|no-inline)$/ }, (args) => ({ path: args.path, namespace: 'vite-asset' }))
      build.onLoad({ filter: /.*/, namespace: 'vite-asset' }, () => ({ contents: 'export default ""', loader: 'js' }))
    },
  }],
})
const mod = await import(`data:text/javascript;base64,${Buffer.from(result.outputFiles[0].text).toString('base64')}`)
const { todos: lib, store } = mod
const H = store.useHubStore

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }
const eq = (got, want, m) => assert(got === want, `${m}（got=${JSON.stringify(got)} want=${JSON.stringify(want)}）`)

const WS1 = '/Users/me/work/proj-one'
const WS2 = '/Users/me/work/proj-two'
const S1 = 's_proj_one'
const S2 = 's_proj_two'
const T0 = Date.parse('2026-05-01T10:00:00Z')
const ts = (seconds) => new Date(T0 + seconds * 1000).toISOString()

/** 重置 store 到「空 Hub」：只覆盖状态字段，action 保留。 */
const reset = (patch = {}) => H.setState({
  open: false, activeId: null, activeGroupId: null, sid: null,
  tasks: [], todoSnapshots: {}, runNames: {}, messages: [], groupMessages: [],
  groupDrafts: [], finalizedMessageIds: [], dm: {}, groupInfo: {}, needs: [], runLog: [],
  ...patch,
})
const ev = (x) => H.getState().handleEvent(JSON.stringify(x))

/** 线上 tool_run_end 的形状（只保留与本场景相关的字段，另有 index/request_id 等噪声）。 */
let toolSeq = 0
const toolEnd = (session, run, opts = {}) => {
  const { workspace = WS1, todos, timestamp = ts(0), name = 'todo_write', extra = {} } = opts
  const line = {
    event_type: 'tool_run_end',
    session_id: session,
    workspace,
    run_id: run,
    request_id: `req_${++toolSeq}`,
    index: toolSeq,
    id: `tool_${toolSeq}`,
    name,
    arguments: '{}',
    result: 'ok',
    is_error: false,
    parent_call_id: '',
    depth: 0,
    timestamp,
    ...extra,
  }
  // 缺字段 = 这条工具没有待办快照；[] / null 才是「清单被清空」。
  if (todos !== undefined) line.todos = todos
  ev(line)
}
const agentStart = (session, run, opts = {}) => ev({
  event_type: 'agent_start',
  session_id: session,
  workspace: opts.workspace ?? WS1,
  run_id: run,
  name: opts.name ?? '',
  parent_run_id: opts.parentRunId ?? '',
  depth: opts.depth ?? 0,
  content: '',
  model: 'test-model',
  timestamp: ts(0),
})

const item = (id, title, done = false) => ({ id, title, done, blocked_by: [] })
const task = (id, opts = {}) => ({
  id,
  title: `任务 ${id}`,
  status: opts.status ?? 'running',
  origin: opts.origin ?? { kind: 'dm', agent_id: 'ag_res', agent_name: 'Researcher' },
  target: opts.target ?? { kind: 'workspace', workspace: WS1, session_id: S1 },
  ...(opts.dispatch ? { dispatch: opts.dispatch } : {}),
  updated_at: opts.updated_at ?? new Date(T0).toISOString(),
})
const cards = (scope) => lib.activeTodos(
  H.getState().tasks, H.getState().todoSnapshots, scope, H.getState().runNames,
)
/** 走一遍 store：把任务放进表（与 hub_task_list 落地同形）。 */
const withTasks = (...list) => H.setState({ tasks: list })

reset()
console.log('场景2 · Project ToDo 内联展示（DM/群）')

// —— 1) 两个 Project 会话：各自落表，互不覆盖 ——
{
  reset()
  toolEnd(S1, 'main1', { todos: [item('t1', '勘察'), item('t2', '实现', true)] })
  toolEnd(S2, 'main2', { workspace: WS2, todos: [item('u1', '另一个项目的活')] })
  const snaps = Object.values(H.getState().todoSnapshots)
  eq(snaps.length, 2, '两个 Project 会话各一条快照（键 = session + run）')
  eq(H.getState().todoSnapshots[lib.hubRunKey(S1, 'main1')].todos.length, 2, 's1 的清单条数')
  eq(H.getState().todoSnapshots[lib.hubRunKey(S2, 'main2')].todos.length, 1, 's2 的清单条数')
  eq(H.getState().todoSnapshots[lib.hubRunKey(S1, 'main1')].workspace, WS1, '快照带 workspace（target 兜底要用）')
  assert(lib.hubRunKey(S1, 'main1') !== lib.hubRunKey(S2, 'main1'), '同 run id 不同会话不串键')
}

// —— 2) DM 作用域：只显示发起者自己的任务 ——
{
  reset()
  withTasks(
    task('task_dm'),
    task('task_other', {
      origin: { kind: 'dm', agent_id: 'ag_other', agent_name: 'Other' },
      // 每个派发出去的任务在自己的会话里跑；同 session 的两条任务无法区分清单
      // （清单属于会话，不属于任务），所以这里必须是另一个会话。
      target: { kind: 'workspace', workspace: WS2, session_id: S2 },
    }),
  )
  toolEnd(S1, 'main1', { todos: [item('t1', '勘察'), item('t2', '实现', true), item('t3', '收口')] })
  const dm = cards({ kind: 'dm', agentId: 'ag_res' })
  eq(dm.length, 1, 'DM 只出自己那条任务的卡')
  eq(dm[0].taskId, 'task_dm', '卡归属任务')
  eq(dm[0].done, 1, '已完成计数')
  eq(dm[0].total, 3, '总条数')
  eq(dm[0].mainRun, false, '没收到 agent_start 的 run 不冒充主会话')
  eq(dm[0].runName, 'run_main1', '未知 run 显示短 id')
  eq(cards({ kind: 'dm', agentId: 'ag_other' }).length, 0, '别的 DM 看不到这条任务')
  eq(cards({ kind: 'group', groupId: 'g1' }).length, 0, 'DM 任务不进群卡')
}

// —— 3) 群作用域：origin.group_id 决定归属（workspace 目标的顶层 group_id 为空）——
{
  reset()
  withTasks(
    task('task_group', {
      origin: { kind: 'group', group_id: 'g1', agent_id: 'ag_res', agent_name: 'Researcher' },
      target: { kind: 'workspace', workspace: WS2, session_id: S2 },
    }),
  )
  toolEnd(S2, 'main2', { workspace: WS2, todos: [item('t1', '群里的活')] })
  eq(cards({ kind: 'group', groupId: 'g1' }).length, 1, '群 g1 看到本群任务')
  eq(cards({ kind: 'group', groupId: 'g2' }).length, 0, '别的群看不到')
  eq(cards({ kind: 'dm', agentId: 'ag_res' }).length, 0, '群任务不进发起者 DM')
  eq(cards({ kind: 'dm', agentId: null }).length, 0, 'activeId 为空时不显示任何卡')
}

// —— 4) run 名：主会话 / 子智能体名 / 未知 run ——
{
  reset()
  withTasks(task('task_dm'))
  agentStart(S1, 'main1') // 根 run：parent_run_id 空 + depth 0，name 空
  toolEnd(S1, 'main1', { todos: [item('t1', '主 run 的清单')] })
  eq(cards({ kind: 'dm', agentId: 'ag_res' })[0].runName, '主会话', '主 run 显示「主会话」')

  agentStart(S1, 'sub1', { name: 'Data Scout', parentRunId: 'main1', depth: 1 })
  toolEnd(S1, 'sub1', { todos: [item('s1', '子智能体的清单')], timestamp: ts(1) })
  const sub = cards({ kind: 'dm', agentId: 'ag_res' })
  eq(sub.length, 1, '同一任务只出一张卡（最新 run）')
  eq(sub[0].runName, 'Data Scout', '子 run 用 agent_start.name')
  eq(sub[0].mainRun, false, '子 run 不是主 run')
  eq(sub[0].todos[0].title, '子智能体的清单', '显示的是最新 run 的清单')

  toolEnd(S1, 'ghost0deadbeef', { todos: [item('g1', '未知 run 的清单')], timestamp: ts(2) })
  const unknown = cards({ kind: 'dm', agentId: 'ag_res' })
  eq(unknown[0].runName, 'run_ghost0de', '未知 run 显示 run_ + 前 8 位（绝不回退主会话）')
  eq(unknown[0].mainRun, false, '未知 run 不当主会话')
}

// —— 5) agent_start 先后顺序：先事件后命名 / 先命名后事件 ——
{
  reset()
  withTasks(task('task_dm'))
  // 先 tool_run_end（app 中途重连/乱序），再补到 agent_start：名字要跟着更新。
  toolEnd(S1, 'late1', { todos: [item('t1', '先到')] })
  eq(cards({ kind: 'dm', agentId: 'ag_res' })[0].runName, 'run_late1', '没有 agent_start 时只显示短 id')
  agentStart(S1, 'late1', { name: 'Writer', parentRunId: 'main1', depth: 1 })
  eq(cards({ kind: 'dm', agentId: 'ag_res' })[0].runName, 'Writer', '迟到的 agent_start 也能补上名字')
  // 反过来：先命名后事件（正常顺序）。
  agentStart(S1, 'early1', { name: 'Critic', parentRunId: 'main1', depth: 1 })
  toolEnd(S1, 'early1', { todos: [item('t2', '后到')], timestamp: ts(9) })
  eq(cards({ kind: 'dm', agentId: 'ag_res' })[0].runName, 'Critic', '先命名后事件同样命中')
}

// —— 6) dispatch.run_id 兜底「主 run」（没见过 agent_start 的恢复场景）——
{
  reset()
  withTasks(task('task_dm', { dispatch: { run_id: 'main1' } }))
  toolEnd(S1, 'main1', { todos: [item('t1', '主 run')] })
  const card = cards({ kind: 'dm', agentId: 'ag_res' })[0]
  eq(card.mainRun, true, 'dispatch.run_id 指向的 run 当主 run')
  eq(card.runName, '主会话', '于是显示「主会话」')
}

// —— 7) 终态清卡：hub_task_update 一到就收（不等下一条事件）——
{
  reset()
  withTasks(task('task_dm'))
  toolEnd(S1, 'main1', { todos: [item('t1', '活着的清单')] })
  eq(cards({ kind: 'dm', agentId: 'ag_res' }).length, 1, '收集到一张卡')
  ev({
    event_type: 'hub_task_update', task_id: 'task_dm', status: 'completed',
    task: task('task_dm', { status: 'completed' }),
  })
  eq(Object.keys(H.getState().todoSnapshots).length, 0, '终态：该会话的快照被回收')
  eq(cards({ kind: 'dm', agentId: 'ag_res' }).length, 0, '终态：卡立即消失')
  // 只有 task_id + status、没有整条 task 的更新（hub_task_update 的瘦形态）也要清。
  reset()
  withTasks(task('task_dm'))
  toolEnd(S1, 'main1', { todos: [item('t1', '再来一次')] })
  ev({ event_type: 'hub_task_update', task_id: 'task_dm', status: 'failed' })
  eq(cards({ kind: 'dm', agentId: 'ag_res' }).length, 0, '瘦终态更新也收卡')
}

// —— 8) 显示层也按终态兜底（快照还在，但任务已 completed）——
{
  reset()
  withTasks(task('task_dm', { status: 'completed' }))
  toolEnd(S1, 'main1', { todos: [item('t1', '迟到的清单')] })
  eq(Object.keys(H.getState().todoSnapshots).length, 1, '迟到的快照仍然落表（可能是下一条任务的）')
  eq(cards({ kind: 'dm', agentId: 'ag_res' }).length, 0, '但终态任务不出卡')
  H.setState({ tasks: [task('task_dm', { status: 'dispatched' })] })
  eq(cards({ kind: 'dm', agentId: 'ag_res' }).length, 1, '同一任务回到 dispatched 时清单重新可见')
}

// —— 9) todos: [] = 清空必须生效；todos 缺失 = 不改 ——
{
  reset()
  withTasks(task('task_dm'))
  toolEnd(S1, 'main1', { todos: [item('t1', '会回来')], timestamp: ts(1) })
  const key = lib.hubRunKey(S1, 'main1')
  const before = H.getState().todoSnapshots[key]
  toolEnd(S1, 'main1', { name: 'bash', timestamp: ts(2) }) // 没有 todos 字段
  eq(H.getState().todoSnapshots[key], before, '缺 todos 字段：快照与引用都不变')
  eq(cards({ kind: 'dm', agentId: 'ag_res' }).length, 1, '缺字段不清卡')
  toolEnd(S1, 'main1', { todos: [], timestamp: ts(3) })
  eq(H.getState().todoSnapshots[key].todos.length, 0, '空数组清空生效')
  eq(cards({ kind: 'dm', agentId: 'ag_res' }).length, 0, '空清单不再出卡（0/0 的卡是噪声）')
  toolEnd(S1, 'main1', { todos: [item('t9', '重建')], timestamp: ts(4) })
  eq(cards({ kind: 'dm', agentId: 'ag_res' }).length, 1, '清空后可以重建')
  // 迟到（旧 timestamp）的快照不得把刚清空的清单顶回来。
  toolEnd(S1, 'main1', { todos: [item('t1', '旧清单')], timestamp: ts(2) })
  eq(cards({ kind: 'dm', agentId: 'ag_res' })[0].todos[0].id, 't9', '旧时间戳不覆盖新快照')
  // null 与 [] 同义（事件带 null 也算「本条有快照且为空」）。
  toolEnd(S1, 'main1', { todos: null, timestamp: ts(5) })
  eq(cards({ kind: 'dm', agentId: 'ag_res' }).length, 0, 'todos: null 同样按清空处理')
}

// —— 10) Hub 自己的会话不写；只读（绝不产生 hub_message / refs）——
{
  reset()
  withTasks(task('task_dm'))
  toolEnd('hub.ag_res.dm', 'hubrun1', { todos: [item('h1', 'Hub 自己的清单')] })
  toolEnd('hub.ag_res.g.g1', 'hubrun2', { workspace: WS1, todos: [item('h2', '群会话的清单')] })
  agentStart('hub.ag_res.dm', 'hubrun1', { name: 'Hub 自己' })
  eq(Object.keys(H.getState().todoSnapshots).length, 0, 'hub.* 会话的待办快照一条都不记')
  eq(Object.keys(H.getState().runNames).length, 0, 'hub.* 会话的 run 名也不记')
  eq(H.getState().messages.length, 0, '没有写 DM 消息')
  eq(H.getState().groupMessages.length, 0, '没有写群消息')
  eq(H.getState().groupDrafts.length, 0, '没有草稿')
}

// —— 11) Hub 关闭仍收集（用户回来时卡已经是最新的）——
{
  reset({ open: false })
  withTasks(task('task_dm'))
  toolEnd(S1, 'main1', { todos: [item('t1', '关着也要收')] })
  eq(Object.keys(H.getState().todoSnapshots).length, 1, 'Hub 关闭时照样落表')
  H.getState().closeHub()
  eq(Object.keys(H.getState().todoSnapshots).length, 1, 'closeHub 不清快照（否则重开要等下一次 todo_*）')
  eq(cards({ kind: 'dm', agentId: 'ag_res' }).length, 1, '重开后立刻有卡')
}

// —— 12) 作用域不串台：切 DM / 切群只换 scope，不共享卡 ——
{
  reset()
  withTasks(
    task('task_a', { origin: { kind: 'dm', agent_id: 'ag_a', agent_name: 'A' } }),
    task('task_b', {
      origin: { kind: 'dm', agent_id: 'ag_b', agent_name: 'B' },
      target: { kind: 'workspace', workspace: WS2, session_id: S2 },
    }),
  )
  toolEnd(S1, 'main1', { todos: [item('a1', 'A 的活')] })
  toolEnd(S2, 'main2', { workspace: WS2, todos: [item('b1', 'B 的活')] })
  eq(cards({ kind: 'dm', agentId: 'ag_a' })[0].todos[0].title, 'A 的活', 'A 的 DM 只有 A 的清单')
  eq(cards({ kind: 'dm', agentId: 'ag_b' })[0].todos[0].title, 'B 的活', 'B 的 DM 只有 B 的清单')
  eq(cards({ kind: 'dm', agentId: 'ag_a' }).length, 1, '切到 A 不会带上 B 的卡')
}

// —— 13) 折叠口径：最多 3 条 + N、最多 3 张卡 ——
{
  reset()
  withTasks(task('task_dm'))
  const many = Array.from({ length: 7 }, (_, i) => item(`t${i}`, `第 ${i} 条`, i < 2))
  toolEnd(S1, 'main1', { todos: many })
  const card = cards({ kind: 'dm', agentId: 'ag_res' })[0]
  eq(card.todos.length, 3, '最多列 3 条')
  eq(card.more, 4, '其余折成 +N')
  eq(card.total, 7, '标题分母仍是总条数')
  eq(card.todos.map((t) => t.id).join(','), 't0,t1,t2', '按模型顺序取前 3 条（不改写顺序）')

  // 4 个任务同时跑：只出 3 张卡（最新在最前）。
  reset()
  const four = [1, 2, 3, 4].map((n) => task(`task_${n}`, {
    target: { kind: 'workspace', workspace: WS1, session_id: `s_p${n}` },
    updated_at: new Date(T0 + n * 1000).toISOString(),
  }))
  withTasks(...four)
  for (const n of [1, 2, 3, 4]) {
    toolEnd(`s_p${n}`, `main${n}`, { todos: [item(`x${n}`, `任务 ${n} 的待办`)], timestamp: ts(n) })
  }
  const top = cards({ kind: 'dm', agentId: 'ag_res' })
  eq(top.length, 3, '最多 3 张卡')
  eq(top[0].taskId, 'task_4', '最新 run 的卡在最前')
}

// —— 14) target.session_id 为空：用 workspace 兜底（刚派发、会话还没建的窗口）——
{
  reset()
  withTasks(task('task_ws', { target: { kind: 'workspace', workspace: WS2 } }))
  toolEnd(S2, 'main2', { workspace: WS2, todos: [item('w1', '还没建会话时的清单')] })
  eq(cards({ kind: 'dm', agentId: 'ag_res' }).length, 1, 'session 缺失时按 workspace 认领')
  eq(cards({ kind: 'dm', agentId: 'ag_res' })[0].sessionId, S2, '卡带事件的真实 session')
}

// —— 15) 有界：run 名表与快照表都不会无限增长 ——
{
  reset()
  withTasks(task('task_dm'))
  for (let i = 0; i < lib.HUB_RUN_NAME_MAX + 20; i++) {
    agentStart(S1, `run${i}`, { name: `子 ${i}`, parentRunId: 'main1', depth: 1 })
  }
  eq(Object.keys(H.getState().runNames).length, lib.HUB_RUN_NAME_MAX, 'run 名表按上限裁剪')
  for (let i = 0; i < lib.HUB_TODO_MAX_RUNS + 15; i++) {
    toolEnd(S1, `run${i}`, { todos: [item(`t${i}`, `清单 ${i}`)], timestamp: ts(i) })
  }
  eq(Object.keys(H.getState().todoSnapshots).length, lib.HUB_TODO_MAX_RUNS, '快照表按 LRU 上限裁剪')
  assert(H.getState().todoSnapshots[lib.hubRunKey(S1, 'run0')] === undefined, '最旧的 run 被淘汰')
  assert(H.getState().todoSnapshots[lib.hubRunKey(S1, `run${lib.HUB_TODO_MAX_RUNS + 14}`)] !== undefined, '最新的 run 还在')
}

console.log(`\n全部通过（${pass} 项）`)
