// 会话级恢复的卡片回归：agent_resume 复用同一 task_id 重跑，但**每次运行是一张新卡**。
//
// 旧契约（本测试原先钉的）：task_started 把同一 task_id 的卡片从终态翻回 running、清掉
// 上一轮的 durMs/result/error。它把「任务」和「运行」混成了一张卡 —— 实际 agent_start
// 每次运行都会新建一张 agent 卡，旧卡被翻回 running 的结果是：
//   · 上一轮已结算的 Worked 卡在续跑期间变回 Working、时长消失；
//   · task_end 把 durMs = ts - spawnedAt 写给所有卡（旧卡变成整个任务寿命）；
//   · task_result_delivered 把新结果写回旧卡，最新卡反而没有；
//   · live 与刷新后（Go checkpoint）不一致。
//
// 新契约：
//   · terminal 卡在 task_started 时**不动**；新 run 的 agent_start 建自己的卡；
//   · task_end / task_result_delivered 只落**最新**一张卡；
//   · agent_user_message 按 RunId 精确归属，没有 RunId 才落最新卡；
//   · 每张卡的 items 带自己的 runId（前端按 run 切 Worked 盒）。
//
// 用法：node scripts/test-subagent-resume-card.mjs
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync, writeFileSync } from 'node:fs'
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

const dir = mkdtempSync(join(tmpdir(), 'subagent-resume-card-'))
const outfile = join(dir, 'store-bundle.cjs')
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile,
  logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'dataurl', '.png': 'dataurl' },
})
const { useAppStore, dispatchEvent } = require(outfile)

// 子会话页的合成块链路（AgentItem → MsgBlock → turn）：单独打一个小包，
// 用真实卡片数据断言「两次运行 = 两个 Worked 段」。
const runsEntry = join(dir, 'runs-entry.ts')
writeFileSync(runsEntry, [
  `export { agentItemsToBlocks } from ${JSON.stringify(join(__dirname, '../src/lib/agentSession'))}`,
  `export { selectTurns, turnRuns } from ${JSON.stringify(join(__dirname, '../src/components/runtime/turn/selectTurns'))}`,
].join('\n'))
const runsOutfile = join(dir, 'runs-bundle.cjs')
await build({
  entryPoints: [runsEntry],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile: runsOutfile,
  logLevel: 'silent',
})
const { agentItemsToBlocks, selectTurns, turnRuns } = require(runsOutfile)

function setup() {
  useAppStore.setState({ activeSessionId: 's1', connections: {}, views: {} })
  const empty = {
    blocks: [], approvals: [], runNodes: [], diffs: [], compressing: false,
    todos: [], turns: [], toolStartTs: {}, reqBlock: {}, thinkStartTs: {}, turnThinkMs: {},
    traceEvents: [], gitFiles: [], gitHistory: [], pendingQueue: [],
    usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0 },
    git: { is_repo: false, ahead: 0, behind: 0, staged: 0, modified: 0, untracked: 0, conflicted: 0, clean: true },
    costUsd: 0, streaming: false, aborted: false, runningAgentCount: 0,
    gitHasMore: false, gitHistLoading: false, gitLoading: false,
    rightExpanded: false,
  }
  useAppStore.setState({ views: { s1: empty } })
}
const blocksOf = (kind, taskId) =>
  useAppStore.getState().views.s1.blocks.filter((b) => b.kind === kind && b.taskId === taskId)
const lastBlockOf = (kind, taskId) => blocksOf(kind, taskId).at(-1)
const ev = (event_type, payload) => dispatchEvent({ event_type, session_id: 's1', ...payload })
const USAGE = { Input: 1000, Output: 200, TotalTokens: 1200, Cost: { Total: 196 } }

// ===== 场景 1：async_task 占位卡 → 续跑由 agent_start 建新卡（不再翻旧卡） =====
setup()
ev('task_started', { task_id: 'task-abc123fg', name: '长文调研', timestamp: 1000 })
ev('task_end', { task_id: 'task-abc123fg', status: 'interrupted', error: '预算耗尽', usage: USAGE, timestamp: 2000 })
let b = lastBlockOf('async_task', 'task-abc123fg')
assert.equal(b.status, 'interrupted', '中断后卡片为终态')
assert.ok(b.endedAt, '终态卡片应记 endedAt')
assert.equal(b.taskError, '预算耗尽', '终态卡片应带错误')

// 续跑：task_started 不再把终态卡翻回 running —— 它只是任务生命周期事件；
// 新 run 的 agent_start 会移除占位卡并建自己的 agent 卡。
ev('task_started', { task_id: 'task-abc123fg', name: '长文调研', timestamp: 3000 })
b = lastBlockOf('async_task', 'task-abc123fg')
assert.equal(b.status, 'interrupted', 'task_started 不许把已终止的占位卡翻回 running')
assert.equal(b.taskError, '预算耗尽', '旧卡的终态残留必须保留')
ev('agent_start', { run_id: 'sub-abc99', parent_run_id: 'root-1', depth: 1, task_id: 'task-abc123fg', name: '长文调研', timestamp: 3100 })
assert.equal(blocksOf('async_task', 'task-abc123fg').length, 0, 'agent_start 应替换占位卡')
const ag1 = lastBlockOf('agent', 'task-abc123fg')
assert.ok(ag1, '续跑应有一张新的 agent 卡')
assert.equal(ag1.runId, 'sub-abc99')
assert.equal(ag1.status, 'running')

// 再次终态：completed → 收敛到最新（唯一）卡。
ev('agent_end', { run_id: 'sub-abc99', parent_run_id: 'root-1', task_id: 'task-abc123fg', finish_reason: 'stop', duration_ms: 500, timestamp: 3200 })
ev('task_end', { task_id: 'task-abc123fg', status: 'completed', result: '写完了', timestamp: 4000 })
b = lastBlockOf('agent', 'task-abc123fg')
assert.equal(b.status, 'completed', '续跑完成后回到 completed')
assert.equal(b.durMs, 500, 'agent_end 的时长必须保留（不被 task_end 的 ts-spawnedAt 覆盖）')

// ===== 场景 2：agent 卡终态不被 task_started 改写；每轮一张卡 =====
setup()
ev('task_started', { task_id: 'task-xyz12345', name: '审查', timestamp: 1000 })
ev('agent_start', { run_id: 'sub-1', parent_run_id: 'root-1', depth: 1, task_id: 'task-xyz12345', name: '审查', timestamp: 1100 })
ev('content_chunk', { run_id: 'sub-1', request_id: 'r1', content: '第一轮', timestamp: 1200 })
ev('agent_end', { run_id: 'sub-1', parent_run_id: 'root-1', task_id: 'task-xyz12345', finish_reason: 'stop', duration_ms: 1000, timestamp: 1300 })
ev('task_end', { task_id: 'task-xyz12345', status: 'completed', result: 'R1', timestamp: 1400 })
ev('task_result_delivered', { task_id: 'task-xyz12345', status: 'completed', result: 'R1', timestamp: 1500 })
let card1 = lastBlockOf('agent', 'task-xyz12345')
assert.equal(card1.status, 'completed')
assert.equal(card1.durMs, 1000)
assert.equal(card1.taskResult, 'R1')
assert.equal(card1.deliveredToMain, true)

// 续跑开始：旧卡保持不变（这是「agentResume 影响 Worked」的根因）
ev('task_started', { task_id: 'task-xyz12345', name: '审查', timestamp: 3000 })
card1 = lastBlockOf('agent', 'task-xyz12345')
assert.equal(card1.status, 'completed', 'task_started 不许把上一轮 agent 卡翻回 running')
assert.equal(card1.durMs, 1000, '旧卡时长必须保留')
assert.equal(card1.taskResult, 'R1', '旧卡结果必须保留')

ev('agent_start', { run_id: 'sub-2', parent_run_id: 'root-1', depth: 1, task_id: 'task-xyz12345', name: '审查', timestamp: 3100 })
const cards = blocksOf('agent', 'task-xyz12345')
assert.equal(cards.length, 2, '续跑 = 新 run，新卡（旧卡保留）')
assert.equal(cards[0].runId, 'sub-1')
assert.equal(cards[1].runId, 'sub-2')

ev('task_end', { task_id: 'task-xyz12345', status: 'completed', result: 'R2', timestamp: 4000 })
const afterEnd = blocksOf('agent', 'task-xyz12345')
assert.equal(afterEnd[0].durMs, 1000, '旧卡时长不许被新 task_end 改写')
assert.equal(afterEnd[0].taskResult, 'R1', '旧卡结果不许被新 task_end 改写')
assert.equal(afterEnd[1].taskResult, 'R2', '新结果必须落最新卡')
assert.equal(afterEnd[1].durMs, 900, '没有 agent_end 时新卡按 ts-spawnedAt 兜底（4000-3100）')

// ===== 场景 3：未结束的卡片再次 task_started（幂等，不清空进行中的数据） =====
setup()
ev('task_started', { task_id: 'task-run11111', name: 'x', timestamp: 1000 })
ev('task_started', { task_id: 'task-run11111', name: 'x', timestamp: 1100 })
b = lastBlockOf('async_task', 'task-run11111')
assert.equal(b.status, 'running', 'running 卡片重复 task_started 仍为 running')
assert.equal(b.startedAt, 1000, 'running 卡片不应被重置 startedAt')

// ===== 场景 4：续跑的**第二轮交付通知**不能被按 taskId 去重吞掉 =====
setup()
ev('task_started', { task_id: 'task-r2r2r2r2', name: 'x', timestamp: 1000 })
ev('task_end', { task_id: 'task-r2r2r2r2', status: 'interrupted', error: 'e1', timestamp: 2000 })
ev('task_result_delivered', { task_id: 'task-r2r2r2r2', status: 'interrupted', error: 'e1', timestamp: 2100 })
ev('task_started', { task_id: 'task-r2r2r2r2', name: 'x', timestamp: 3000 })
ev('task_end', { task_id: 'task-r2r2r2r2', status: 'completed', result: 'R2', timestamp: 4000 })
ev('task_result_delivered', { task_id: 'task-r2r2r2r2', status: 'completed', result: 'R2', timestamp: 4100 })
const deliveries = blocksOf('task_delivery', 'task-r2r2r2r2')
assert.equal(deliveries.length, 2, '两轮各一条交付通知（续跑那轮不能被按 taskId 去重吞掉）')
const card = lastBlockOf('async_task', 'task-r2r2r2r2')
assert.equal(card.status, 'completed', '续跑完成后回到 completed')
assert.equal(card.taskResult, 'R2', '续跑那轮的结果必须回填最新卡')

// 同轮重复投递仍幂等（同 status+result 指纹）
ev('task_result_delivered', { task_id: 'task-r2r2r2r2', status: 'completed', result: 'R2', timestamp: 4200 })
assert.equal(blocksOf('task_delivery', 'task-r2r2r2r2').length, 2, '同轮重复投递必须幂等（不加第三条）')

// ===== 场景 5：resume prompt / item runId / 旧卡结算不被覆盖 =====
setup()
ev('task_started', { task_id: 'task-perrun01', name: 'per-run', timestamp: 1000 })
ev('agent_start', { run_id: 'run-1', parent_run_id: 'root-1', depth: 1, task_id: 'task-perrun01', name: 'per-run', timestamp: 1100 })
ev('content_chunk', { run_id: 'run-1', request_id: 'r1', content: 'run1 answer', timestamp: 1200 })
ev('tool_start', { id: 'tool-1', name: 'bash', arguments: '{}', run_id: 'run-1', timestamp: 1250 })
ev('agent_end', { run_id: 'run-1', parent_run_id: 'root-1', task_id: 'task-perrun01', finish_reason: 'stop', duration_ms: 1000, timestamp: 1300 })
ev('task_end', { task_id: 'task-perrun01', status: 'completed', result: 'R1', timestamp: 1400 })
ev('task_result_delivered', { task_id: 'task-perrun01', status: 'completed', result: 'R1', timestamp: 1500 })

ev('task_started', { task_id: 'task-perrun01', name: 'per-run', timestamp: 2000 })
ev('agent_start', { run_id: 'run-2', parent_run_id: 'root-1', depth: 1, task_id: 'task-perrun01', name: 'per-run', timestamp: 2100 })
ev('agent_user_message', { task_id: 'task-perrun01', run_id: 'run-2', text: 'resume prompt for run 2', timestamp: 2200 })
ev('content_chunk', { run_id: 'run-2', request_id: 'r2', content: 'run2 answer', timestamp: 2300 })
ev('tool_start', { id: 'tool-2', name: 'bash', arguments: '{}', run_id: 'run-2', timestamp: 2350 })
ev('agent_end', { run_id: 'run-2', parent_run_id: 'root-1', task_id: 'task-perrun01', finish_reason: 'stop', duration_ms: 2000, timestamp: 2400 })
ev('task_end', { task_id: 'task-perrun01', status: 'completed', result: 'R2', timestamp: 2500 })
ev('task_result_delivered', { task_id: 'task-perrun01', status: 'completed', result: 'R2', timestamp: 2600 })

const perRun = blocksOf('agent', 'task-perrun01')
assert.equal(perRun.length, 2, '两次运行 = 两张卡')
const [oldCard, newCard] = perRun
assert.equal(oldCard.durMs, 1000, '旧卡时长不许被 run2 的 task_end 改写')
assert.equal(oldCard.taskResult, 'R1', '旧卡结果不许被 run2 的 delivery 改写')
assert.equal(oldCard.deliveredToMain, true)
assert.equal(newCard.durMs, 2000, '新卡时长必须是 agent_end 的 2000ms（不是 task_end 的 ts-spawnedAt）')
assert.equal(newCard.taskResult, 'R2')
assert.equal(newCard.deliveredToMain, true)
assert.equal(oldCard.items.some((it) => it.text === 'resume prompt for run 2'), false, 'resume prompt 写进了旧卡')
assert.equal(newCard.items.some((it) => it.kind === 'user' && it.text === 'resume prompt for run 2' && it.runId === 'run-2'), true, 'resume prompt 必须进新卡并带 runId')
for (const it of oldCard.items) assert.equal(it.runId, 'run-1', `旧卡 item 缺/错 runId: ${it.kind}`)
for (const it of newCard.items) assert.equal(it.runId, 'run-2', `新卡 item 缺/错 runId: ${it.kind}`)

// ===== 场景 6：子会话页的合成块按 run 切段（两次运行 = 两个 Worked 段） =====
// 这是用户直接看到的东西：子会话页不是自己一套渲染，而是 agentItemsToBlocks → Narrative。
// 每张卡的 item 带自己的 runId（旧 record 用卡片的 runId 兜底）→ selectTurns/turnRuns
// 必须在同一段转录里切出两段，而不是把两轮合成一个 Working。
const subBlocks = []
let subId = -1
for (const c of [oldCard, newCard]) {
  subBlocks.push(...agentItemsToBlocks(c.items, { fallbackRunId: c.runId, nextId: () => subId-- }))
}
const subIds = subBlocks.map((b) => b.id)
assert.equal(new Set(subIds).size, subIds.length, '多张卡合并时负数 id 不许撞车（React key）')
const turns = selectTurns(subBlocks)
assert.equal(turns.length, 2, 'resume prompt 是新的 user 块 → 应有 2 轮')
const runs1 = turnRuns(turns[0]).map((r) => r.runId)
const runs2 = turnRuns(turns[1]).map((r) => r.runId)
assert.deepEqual(runs1, ['run-1'], '第一轮应只有 run-1 一段（Worked 盒）')
assert.deepEqual(runs2, ['run-2'], '第二轮应只有 run-2 一段（Worked 盒）')
for (const t of turns) {
  assert.ok(t.items.some((b) => b.kind === 'tool'), '每轮的 Worked 段里应有该轮自己的工具行')
}

console.log('✓ subagent resume card：每轮一张卡、旧卡不翻 running/不被覆盖、prompt 按 runId 归属、item 带 runId、子会话按 run 切段 — 全部通过')
