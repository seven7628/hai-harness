// 子会话隔离回归（真实 store + mock transport，Node 直跑）。
//
// 覆盖用户 2026-11 报障的四条链路：
//   1. 子会话输入：sendToAgent → 子会话自己的队列（不是主会话 pendingQueue）→
//      agent_user_message 到达后出队，转录里出现 user 气泡；
//   2. 队列失败：task 已结束 → 回滚为未推送并给出错误，不静默消失；
//   3. 续跑：同一 task 新 run 到达时 focusedAgent.runId 跟到新 run；
//   4. 子会话的审批/压缩作用域读当前 run，不读上一轮。
//
// 运行：npm run test:subagent-session
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)
const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
}

const dir = mkdtempSync(join(tmpdir(), 'subagent-session-'))
const outfile = join(dir, 'store-bundle.cjs')
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile,
  logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' },
})
const { useAppStore, dispatchEvent, agentQueueKey } = require(outfile)

function setup() {
  useAppStore.setState({
    workspace: '/mock/w',
    activeSessionId: 's1',
    bridgeStatus: 'running',
    sessions: [{ id: 's1', title: 't' }],
    projects: [{ path: '/mock/w', sessions: [{ id: 's1', title: 't' }] }],
    views: {},
    streaming: false,
    pendingQueue: [],
    agentQueues: {},
    agentQueueErrors: {},
    blocks: [],
    runNodes: [],
    approvals: [],
    diffs: [],
    todos: [],
    usage: { input: 0, output: 0, cacheRead: 0, reasoning: 0 },
    turns: [],
    costUsd: 0,
    toolStartTs: {},
    focusedAgent: undefined,
    agentSendError: undefined,
    rightExpanded: false,
    tabsByScope: { s1: { tabs: [{ id: 'act0', kind: 'activity' }], activeId: 'act0', closed: [] } },
  })
}
const EV = (t, p = {}) => dispatchEvent({ event_type: t, session_id: 's1', ...p })
const st = () => useAppStore.getState()
const view = () => st().views.s1
const wait = (ms) => new Promise((r) => setTimeout(r, ms))

function startSub(runId, taskId, ts = 1000) {
  EV('task_started', { task_id: taskId, name: 'review', timestamp: ts })
  EV('agent_start', {
    run_id: runId, parent_run_id: 'root', depth: 1, task_id: taskId,
    name: 'review', timestamp: ts + 100,
  })
}

// ===== 1. 子会话输入走自己的队列，事件到达后出队并落 user 气泡 =====
setup()
startSub('run-1', 'task-1')
useAppStore.getState().focusAgent({ nodeId: 'task-1', runId: 'run-1', taskId: 'task-1' })
st().sendToAgent('task-1', '把 401 分支也改掉')
const key = agentQueueKey('s1', 'task-1')
let q = st().agentQueues[key] ?? []
assert.equal(q.length, 1, '子会话消息必须先入子会话队列（可见反馈）')
assert.equal(q[0].pushed, true, '运行中应立即推送')
assert.equal(st().pendingQueue.length, 0, '不能混进主会话的 ask_batch 队列')
assert.equal(st().agentSendError, undefined)
EV('agent_user_message', { task_id: 'task-1', run_id: 'run-1', text: '把 401 分支也改掉', source: 'user', timestamp: 1300 })
q = st().agentQueues[key] ?? []
assert.equal(q.length, 0, 'agent_user_message 到达后必须出队（否则永远显示待发送）')
const card = view().blocks.find((b) => b.kind === 'agent' && b.taskId === 'task-1')
assert.ok(card && card.kind === 'agent', '子 agent 卡片应在')
assert.ok(card.items.some((it) => it.kind === 'user' && it.text === '把 401 分支也改掉'),
  '子会话转录里必须出现用户消息（用户报障的「无当前用户信息」）')
console.log('PASS 1：子会话队列隔离 + 用户消息可见 + 服务端确认后出队')

// ===== 2. 已结束任务：队列保留、回滚为未推送、错误可见 =====
setup()
startSub('run-1', 'task-finished')
useAppStore.getState().focusAgent({ nodeId: 'task-finished', runId: 'run-1', taskId: 'task-finished' })
st().sendToAgent('task-finished', '还在吗')
await wait(40)
q = st().agentQueues[agentQueueKey('s1', 'task-finished')] ?? []
assert.equal(q.length, 1, '失败时不能把消息丢掉')
assert.equal(q[0].pushed, false, '失败必须回滚 pushed，允许编辑/重试')
assert.ok(String(st().agentSendError || '').includes('finished'), '失败原因必须可见')
assert.equal(st().pendingQueue.length, 0, '失败也不能落到主会话队列')
console.log('PASS 2：子会话发送失败保留在子队列并回滚，不静默、不串主会话')

// ===== 3. agent_resume：焦点 runId 跟到新 run =====
setup()
startSub('run-1', 'task-1', 1000)
useAppStore.getState().focusAgent({ nodeId: 'task-1', runId: 'run-1', taskId: 'task-1' })
EV('agent_end', { run_id: 'run-1', parent_run_id: 'root', task_id: 'task-1', finish_reason: 'stop', duration_ms: 9000, timestamp: 1200 })
startSub('run-2', 'task-1', 2000)
assert.equal(st().focusedAgent?.runId, 'run-2', '续跑后焦点必须指向新 run（审批/压缩/运行条都靠它）')
const cards = view().blocks.filter((b) => b.kind === 'agent' && b.taskId === 'task-1')
assert.equal(cards.length, 2, '续跑保留旧卡 + 新卡（历史不能丢）')
const latest = cards[cards.length - 1]
assert.equal(latest.runId, 'run-2')
assert.equal(latest.status, 'running', '新卡必须是 running，不是上一轮的 completed')
assert.equal(latest.durMs, undefined, '新卡不能继承上一轮的 durMs')
console.log('PASS 3：续跑后焦点/卡片状态切到新 run，旧历史保留')

// ===== 4. 作用域：子会话压缩只属于当前 run =====
setup()
startSub('run-1', 'task-1', 1000)
useAppStore.getState().focusAgent({ nodeId: 'task-1', runId: 'run-1', taskId: 'task-1' })
EV('compress_start', { run_id: 'run-1', before: 10, timestamp: 1300 })
EV('compress_end', { run_id: 'run-1', before: 10, after: 4, timestamp: 1400 })
startSub('run-2', 'task-1', 2000)
EV('compress_start', { run_id: 'run-2', before: 8, timestamp: 2300 })
const c2 = view().blocks.find((b) => b.kind === 'agent' && b.runId === 'run-2')
assert.ok(c2 && c2.kind === 'agent' && c2.items.some((it) => it.kind === 'compression' && it.active),
  '当前 run 的压缩必须在自己的卡上')
const c1 = view().blocks.find((b) => b.kind === 'agent' && b.runId === 'run-1')
assert.ok(c1 && c1.kind === 'agent' && !c1.items.some((it) => it.kind === 'compression' && it.active),
  '上一轮的压缩不能借给新 run')
console.log('PASS 4：压缩按 run 隔离（续跑不会复用上一轮的压缩态）')

// ===== 5. 任务结束后暂存的消息：续跑 agent_start 自动补发 =====
setup()
startSub('run-1', 'task-1', 1000)
useAppStore.getState().focusAgent({ nodeId: 'task-1', runId: 'run-1', taskId: 'task-1' })
EV('agent_end', { run_id: 'run-1', parent_run_id: 'root', task_id: 'task-1', finish_reason: 'stop', duration_ms: 9000, timestamp: 1200 })
st().sendToAgent('task-1', '恢复后继续')
await wait(30)
q = st().agentQueues[agentQueueKey('s1', 'task-1')] ?? []
assert.equal(q.length, 1, '结束后发送：消息留在队列')
assert.equal(q[0].pushed, false, '结束后发送：不假装已推送')
startSub('run-2', 'task-1', 2000)
await wait(60)
q = st().agentQueues[agentQueueKey('s1', 'task-1')] ?? []
assert.equal(q.length, 1, '续跑后队列项仍在（等 agent_user_message 确认）')
assert.equal(q[0].pushed, true, '续跑 agent_start 必须自动补发（不是等用户再点一次）')
EV('agent_user_message', { task_id: 'task-1', run_id: 'run-2', text: '恢复后继续', source: 'user', timestamp: 2400 })
q = st().agentQueues[agentQueueKey('s1', 'task-1')] ?? []
assert.equal(q.length, 0, '补发确认后出队')
const run2 = view().blocks.find((b) => b.kind === 'agent' && b.runId === 'run-2')
assert.ok(run2 && run2.kind === 'agent' && run2.items.some((it) => it.kind === 'user' && it.text === '恢复后继续'),
  '补发的消息落在当前 run 的转录里')
console.log('PASS 5：结束后暂存的消息在续跑时自动补发，并落到新 run')

// ===== 6. 消息已推送但 run 先结束：队列回滚为未推送，不能永远显示已送达 =====
setup()
startSub('run-1', 'task-1', 1000)
useAppStore.getState().focusAgent({ nodeId: 'task-1', runId: 'run-1', taskId: 'task-1' })
st().sendToAgent('task-1', '来不及消费')
await wait(30)
q = st().agentQueues[agentQueueKey('s1', 'task-1')] ?? []
assert.equal(q[0]?.pushed, true, '先置为已推送')
EV('agent_end', { run_id: 'run-1', parent_run_id: 'root', task_id: 'task-1', finish_reason: 'abort', timestamp: 1300 })
q = st().agentQueues[agentQueueKey('s1', 'task-1')] ?? []
assert.equal(q[0]?.pushed, false, 'run 结束时仍未确认 → 回滚为未推送')
assert.ok(String(st().agentQueueErrors[agentQueueKey('s1', 'task-1')] || '').includes('没有送达'),
  '回滚时给出「没有送达」原因')
console.log('PASS 6：run 结束未消费的队列项回滚并报因，不会永远停在已推送')

console.log("SUBAGENT_SESSION_TEST_PASS")
process.exit(0)
