// 会话级恢复的卡片回归：同一 task_id 再次 task_started（子任务原地续跑）→ 卡片必须从
// 终态**翻回 running**，并清掉上一轮的终态残留（结束时刻 / 结果 / 错误）。
//
// 背景（docs/SUBAGENT_OUTPUT_JOURNAL_TASKOUTPUT.md §13）：子任务与主会话同档持久化后，
// agent_resume 复用同一 task_id 重跑，宿主重发 task_started。若 reducer 只改 status，
// 卡片会显示「运行中」却仍挂着旧结论与旧时长 —— 本测试钉住这个 case。
//
// 用法：node scripts/test-subagent-resume-card.mjs
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

function setup() {
  useAppStore.setState({ activeSessionId: 's1', connections: {}, views: {} })
  // 完整 SessionView 形状（缺 field 会让 reducer 在 setView 时抛错）——从空 view 复制。
  useAppStore.setState((s) => ({ views: { ...s.views } }))
  const empty = {
    blocks: [], approvals: [], runNodes: [], diffs: [], compressing: false,
    todos: [], turns: [], toolStartTs: {}, reqBlock: {}, thinkStartTs: {}, turnThinkMs: {},
    traceEvents: [], gitFiles: [], gitHistory: [], pendingQueue: [],
    usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0 },
    git: { is_repo: false, ahead: 0, behind: 0, staged: 0, modified: 0, untracked: 0, conflicted: 0, clean: true },
    costUsd: 0, streaming: false, aborted: false, runningAgentCount: 0,
    gitHasMore: false, gitHistLoading: false, gitLoading: false,
    rightExpanded: false, rightTab: 'activity',
  }
  useAppStore.setState({ views: { s1: empty } })
}
const blockOf = (kind, taskId) =>
  useAppStore.getState().views.s1.blocks.find((b) => b.kind === kind && b.taskId === taskId)

const USAGE = { Input: 1000, Output: 200, TotalTokens: 1200, Cost: { Total: 196 } }

// ===== 场景 1：async_task 卡片原地续跑 =====
setup()
dispatchEvent({ event_type: 'task_started', session_id: 's1', task_id: 'task-abc123fg', name: '长文调研', timestamp: 1000 })
dispatchEvent({ event_type: 'task_end', session_id: 's1', task_id: 'task-abc123fg', status: 'interrupted', error: '预算耗尽', usage: USAGE, timestamp: 2000 })
let b = blockOf('async_task', 'task-abc123fg')
assert.equal(b.status, 'interrupted', '中断后卡片为终态')
assert.ok(b.endedAt, '终态卡片应记 endedAt')
assert.equal(b.taskError, '预算耗尽', '终态卡片应带错误')

// 续跑：宿主重发 task_started（同一 task_id）→ 翻回 running 且清掉终态残留。
dispatchEvent({ event_type: 'task_started', session_id: 's1', task_id: 'task-abc123fg', name: '长文调研', timestamp: 3000 })
b = blockOf('async_task', 'task-abc123fg')
assert.equal(b.status, 'running', '同一 task_id 的 task_started 应把卡片翻回 running')
assert.equal(b.endedAt, undefined, '翻回 running 必须清掉上一轮的 endedAt')
assert.equal(b.taskError, undefined, '翻回 running 必须清掉上一轮的错误')
assert.equal(b.taskResult, undefined, '翻回 running 必须清掉上一轮的结果')
assert.equal(useAppStore.getState().views.s1.blocks.length, 1, '续跑复用同一卡片（不新建）')

// 再次终态：completed → 正常收敛。
dispatchEvent({ event_type: 'task_end', session_id: 's1', task_id: 'task-abc123fg', status: 'completed', result: '写完了', timestamp: 4000 })
b = blockOf('async_task', 'task-abc123fg')
assert.equal(b.status, 'completed', '续跑完成后回到 completed')
assert.equal(b.taskResult, '写完了', '结果按原语义回填')

// ===== 场景 2：agent 卡片（durMs 而不是 endedAt） =====
setup()
dispatchEvent({ event_type: 'task_started', session_id: 's1', task_id: 'task-xyz12345', name: '审查', timestamp: 1000 })
dispatchEvent({ event_type: 'agent_start', session_id: 's1', run_id: 'sub-1', parent_run_id: 'root-1', depth: 1, task_id: 'task-xyz12345', name: '审查', timestamp: 1100 })
dispatchEvent({ event_type: 'task_end', session_id: 's1', task_id: 'task-xyz12345', status: 'failed', error: 'boom', timestamp: 2000 })
let ag = blockOf('agent', 'task-xyz12345')
assert.equal(ag.status, 'failed')
assert.ok(ag.durMs !== undefined, 'agent 终态应记 durMs')

dispatchEvent({ event_type: 'task_started', session_id: 's1', task_id: 'task-xyz12345', name: '审查', timestamp: 3000 })
ag = blockOf('agent', 'task-xyz12345')
assert.equal(ag.status, 'running', 'agent 卡片同样应翻回 running')
assert.equal(ag.durMs, undefined, 'agent 卡片翻回 running 应清掉 durMs')
assert.equal(ag.taskError, undefined, 'agent 卡片翻回 running 应清掉错误')

// ===== 场景 3：未结束的卡片再次 task_started（幂等，不清空进行中的数据） =====
setup()
dispatchEvent({ event_type: 'task_started', session_id: 's1', task_id: 'task-run11111', name: 'x', timestamp: 1000 })
dispatchEvent({ event_type: 'task_started', session_id: 's1', task_id: 'task-run11111', name: 'x', timestamp: 1100 })
b = blockOf('async_task', 'task-run11111')
assert.equal(b.status, 'running', 'running 卡片重复 task_started 仍为 running')
assert.equal(b.startedAt, 1000, 'running 卡片不应被重置 startedAt')

// ===== 场景 4：续跑的**第二轮交付通知**不能被按 taskId 去重吞掉 =====
// 回归锁：task_delivery 曾按 taskId 全局去重 → 续跑那轮的结束没有任何结果通知，
// 用户看到的是「任务结束了但没结果」。
setup()
dispatchEvent({ event_type: 'task_started', session_id: 's1', task_id: 'task-r2r2r2r2', name: 'x', timestamp: 1000 })
dispatchEvent({ event_type: 'task_end', session_id: 's1', task_id: 'task-r2r2r2r2', status: 'interrupted', error: 'e1', timestamp: 2000 })
dispatchEvent({ event_type: 'task_result_delivered', session_id: 's1', task_id: 'task-r2r2r2r2', status: 'interrupted', error: 'e1', timestamp: 2100 })
dispatchEvent({ event_type: 'task_started', session_id: 's1', task_id: 'task-r2r2r2r2', name: 'x', timestamp: 3000 })
dispatchEvent({ event_type: 'task_end', session_id: 's1', task_id: 'task-r2r2r2r2', status: 'completed', result: 'R2', timestamp: 4000 })
dispatchEvent({ event_type: 'task_result_delivered', session_id: 's1', task_id: 'task-r2r2r2r2', status: 'completed', result: 'R2', timestamp: 4100 })
const deliveries = useAppStore.getState().views.s1.blocks.filter((b) => b.kind === 'task_delivery' && b.taskId === 'task-r2r2r2r2')
assert.equal(deliveries.length, 2, '两轮各一条交付通知（续跑那轮不能被按 taskId 去重吞掉）')
const card = blockOf('async_task', 'task-r2r2r2r2')
assert.equal(card.status, 'completed', '续跑完成后回到 completed')
assert.equal(card.taskResult, 'R2', '续跑那轮的结果必须回填')

// 同轮重复投递仍幂等（同 status+result 指纹）
dispatchEvent({ event_type: 'task_result_delivered', session_id: 's1', task_id: 'task-r2r2r2r2', status: 'completed', result: 'R2', timestamp: 4200 })
const after = useAppStore.getState().views.s1.blocks.filter((b) => b.kind === 'task_delivery' && b.taskId === 'task-r2r2r2r2')
assert.equal(after.length, 2, '同轮重复投递必须幂等（不加第三条）')

console.log('✓ subagent resume card：同一 task_id 翻回 running、清终态残留、复用卡片、续跑交付通知 — 全部通过')
