// 后台任务用量（tokens + 成本）展示回归：真实 store + esbuild，Node 直接跑。
//
// 覆盖：task_end / task_result_delivered 的 usage → 任务卡片（agent / async_task /
// promoted tool）记 tokens 与成本（µUSD → USD）；两路事件同源幂等（不叠加）；
// 无 usage 不留空白用量位；卡片成本不进会话累计（不双计）；checkpoint 恢复
// （cost_usd 随 usage 落盘）后卡片成本仍在。
//
// 用法：node scripts/test-task-usage.mjs
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)

// 1) shim localStorage（store 模块加载时读取项目树/偏好）
const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
}

// 2) esbuild 打包 store 到临时 CJS
const dir = mkdtempSync(join(tmpdir(), 'task-usage-'))
const outfile = join(dir, 'store-bundle.cjs')
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile,
  logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' }, // mock.ts 的 ?raw/?url 资源
})
const { useAppStore, hydrateCheckpoint, dispatchEvent } = require(outfile)

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
    blocks: [],
    runNodes: [],
    approvals: [],
    diffs: [],
    todos: [],
    usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, reasoning: 0 },
    turns: [],
    costUsd: 0,
    toolStartTs: {},
    rightExpanded: false,
    rightTab: 'activity',
  })
}
const view = () => useAppStore.getState().views.s1
const blockOf = (kind, taskId) => view().blocks.find((b) => b.kind === kind && b.taskId === taskId)
const toolBlock = (toolId) => view().blocks.find((b) => b.kind === 'tool' && b.toolId === toolId)

// 任务全量用量（wire 形态 = core.Usage：大写字段 + Cost.Total µUSD）
const USAGE = { Input: 1000, Output: 200, CacheRead: 800, CacheWrite: 0, Reasoning: 0, TotalTokens: 1200, Cost: { Input: 140, CacheRead: 12, Output: 56, Total: 196 } }
const WANT_COST = 196 / 1e6

// ===== 场景 1：子 agent 卡片（agent_start → task_end）=====
setup()
dispatchEvent({ event_type: 'task_started', session_id: 's1', task_id: 'a1', name: '审查', timestamp: 1000 })
dispatchEvent({ event_type: 'agent_start', session_id: 's1', run_id: 'sub-1', parent_run_id: 'root-1', depth: 1, task_id: 'a1', name: '审查', timestamp: 1100 })
let ag = blockOf('agent', 'a1')
assert.ok(ag, 'agent 卡片应已建立并带 taskId')
assert.equal(ag.usage.input, 0, '未结算时用量为 0')

// 运行中：每轮 llm_end 累加（桥按价表打标 cost_usd）
dispatchEvent({ event_type: 'llm_end', session_id: 's1', run_id: 'sub-1', request_id: 'r1', model: 'm1', usage: { Input: 400, Output: 50, CacheRead: 300 }, cost_usd: 0.0001, timestamp: 1200 })
ag = blockOf('agent', 'a1')
assert.equal(ag.usage.input, 400, '子 agent 每轮用量累加到卡片')
assert.ok(Math.abs(ag.usage.costUsd - 0.0001) < 1e-12, `运行中卡片成本应累加: ${ag.usage.costUsd}`)

// 终态：task_end 携任务全量（含压缩轮）→ **覆盖**近似增量，并带上成本
dispatchEvent({ event_type: 'task_end', session_id: 's1', task_id: 'a1', status: 'completed', result: 'ok', usage: USAGE, timestamp: 1300 })
ag = blockOf('agent', 'a1')
assert.equal(ag.usage.input, 1000, '终态用量覆盖运行中增量（压缩轮不经 llm_end，增量会少算）')
assert.equal(ag.usage.output, 200)
assert.equal(ag.usage.cacheRead, 800)
assert.ok(Math.abs(ag.usage.costUsd - WANT_COST) < 1e-12, `卡片成本应为 µUSD→USD: ${ag.usage.costUsd}`)
// 会话累计成本只来自 llm_end 的 cost_usd（子 agent 每轮 llm_end 同样进会话口径），
// 不因卡片成本变化 —— 两处独立记账，叠加会双计。
assert.ok(Math.abs(view().costUsd - 0.0001) < 1e-12, `卡片成本不得计入会话累计: ${view().costUsd}`)

// ===== 场景 2：两路事件同源幂等（不叠加）=====
dispatchEvent({ event_type: 'task_result_delivered', session_id: 's1', task_id: 'a1', status: 'completed', result: 'ok', usage: USAGE, timestamp: 1400 })
ag = blockOf('agent', 'a1')
assert.equal(ag.usage.input, 1000, 'task_result_delivered 不得叠加用量')
assert.ok(Math.abs(ag.usage.costUsd - WANT_COST) < 1e-12, 'task_result_delivered 不得叠加成本')
assert.equal(ag.taskResult, 'ok', '结果仍按原语义回填卡片')

// ===== 场景 3：仅 push 路径（无 task_end，旧宿主/异常顺序）=====
setup()
dispatchEvent({ event_type: 'task_started', session_id: 's1', task_id: 'a2', name: '调研', timestamp: 1000 })
dispatchEvent({ event_type: 'task_result_delivered', session_id: 's1', task_id: 'a2', status: 'completed', result: 'body', usage: USAGE, timestamp: 1400 })
const at = blockOf('async_task', 'a2')
assert.ok(at, 'async_task 卡片应存在')
assert.equal(at.usage.input, 1000, 'async_task 卡片应记 tokens')
assert.ok(Math.abs(at.usage.costUsd - WANT_COST) < 1e-12, 'async_task 卡片应记成本')

// ===== 场景 4：promoted 工具任务（TaskEnd 携用量）=====
setup()
dispatchEvent({ event_type: 'tool_run_start', session_id: 's1', run_id: 'root-1', id: 'call-1', name: 'bash', arguments: '{"command":"sleep 30"}', task_id: 'tooltask-1', timestamp: 1000 })
dispatchEvent({ event_type: 'task_promoted', session_id: 's1', task_id: 'tooltask-1', kind: 'tool', name: 'bash', timestamp: 1100 })
dispatchEvent({ event_type: 'task_end', session_id: 's1', task_id: 'tooltask-1', status: 'completed', result: 'done', usage: USAGE, timestamp: 1200 })
const tl = blockOf('tool', 'tooltask-1')
assert.ok(tl, 'promoted 工具行应存在')
assert.equal(tl.promoted, true)
assert.equal(tl.taskStatus, 'completed', 'TaskEnd 收敛工具任务终态')
assert.equal(tl.usage.input, 1000, 'promoted 工具任务应记 tokens')
assert.ok(Math.abs(tl.usage.costUsd - WANT_COST) < 1e-12, 'promoted 工具任务应记成本')

// ===== 场景 5：无 usage（普通后台工具，不花模型钱）→ 不留空白用量位 =====
setup()
dispatchEvent({ event_type: 'tool_run_start', session_id: 's1', run_id: 'root-1', id: 'call-2', name: 'bash', arguments: '{"command":"sleep 5"}', task_id: 'tooltask-2', timestamp: 1000 })
dispatchEvent({ event_type: 'task_promoted', session_id: 's1', task_id: 'tooltask-2', kind: 'tool', name: 'bash', timestamp: 1100 })
dispatchEvent({ event_type: 'task_end', session_id: 's1', task_id: 'tooltask-2', status: 'completed', result: 'done', timestamp: 1200 })
assert.equal(blockOf('tool', 'tooltask-2').usage, undefined, '无用量信息不应造出空用量对象')

// ===== 场景 6：in-loop 调用级用量（同步子 agent 的工具行）=====
setup()
// 主 run 里的一次子 agent 类工具调用：ToolResponse.Usage 携本次调用用量（含成本）
dispatchEvent({ event_type: 'tool_run_start', session_id: 's1', run_id: 'root-1', id: 'call-9', name: 'subagent_x', arguments: '{}', timestamp: 1000 })
dispatchEvent({ event_type: 'tool_run_end', session_id: 's1', run_id: 'root-1', id: 'call-9', name: 'subagent_x', result: 'done', usage: USAGE, timestamp: 1200 })
const call = toolBlock('call-9')
assert.ok(call, '工具行应存在')
assert.equal(call.usage.input, 1000, 'in-loop 工具行应记本次调用 tokens')
assert.ok(Math.abs(call.usage.costUsd - WANT_COST) < 1e-12, `in-loop 工具行应记成本: ${call.usage.costUsd}`)

// 普通工具：引擎恒填 usage 指针（零值）→ 不写空用量
dispatchEvent({ event_type: 'tool_run_start', session_id: 's1', run_id: 'root-1', id: 'call-10', name: 'read_file', arguments: '{}', timestamp: 1300 })
dispatchEvent({ event_type: 'tool_run_end', session_id: 's1', run_id: 'root-1', id: 'call-10', name: 'read_file', result: 'ok', usage: { Input: 0, Output: 0, CacheRead: 0, Cost: { Total: 0 } }, timestamp: 1400 })
assert.equal(toolBlock('call-10').usage, undefined, '普通工具不得写出空用量')

// ===== 场景 7：同步子 agent 的卡片用量（AgentEnd.Usage 权威，无 task_end）=====
setup()
dispatchEvent({ event_type: 'agent_start', session_id: 's1', run_id: 'sub-sync', parent_run_id: 'root-1', depth: 1, task_id: 'task-sync', name: '同步子 agent', timestamp: 1000 })
dispatchEvent({ event_type: 'llm_end', session_id: 's1', run_id: 'sub-sync', request_id: 'r1', model: 'm1', usage: { Input: 400, Output: 50, CacheRead: 300 }, cost_usd: 0.0001, timestamp: 1100 })
dispatchEvent({ event_type: 'agent_end', session_id: 's1', run_id: 'sub-sync', parent_run_id: 'root-1', finish_reason: 'end', usage: USAGE, timestamp: 1300 })
const syncCard = blockOf('agent', 'task-sync')
assert.ok(syncCard, '同步子 agent 卡片应存在')
assert.equal(syncCard.usage.input, 1000, 'AgentEnd.Usage 应作卡片权威用量（覆盖按轮累加）')
assert.ok(Math.abs(syncCard.usage.costUsd - WANT_COST) < 1e-12, `同步子 agent 卡片应记成本: ${syncCard.usage.costUsd}`)

// ===== 场景 8：压缩轮用量（主压缩块 + 子 agent 卡片内压缩项）=====
setup()
dispatchEvent({ event_type: 'agent_start', session_id: 's1', run_id: 'root-1', content: '主会话', timestamp: 900 })
dispatchEvent({ event_type: 'compress_start', session_id: 's1', run_id: '', before: 100, timestamp: 1000 })
dispatchEvent({ event_type: 'compress_end', session_id: 's1', run_id: '', before: 100, after: 20, ctx_tokens: 5000, model: 'm1', summary: 's', usage: USAGE, timestamp: 1200 })
const comp = view().blocks.find((b) => b.kind === 'compression')
assert.ok(comp, '压缩块应存在')
assert.equal(comp.usage.input, 1000, '压缩块应记压缩轮 tokens')
assert.ok(Math.abs(comp.usage.costUsd - WANT_COST) < 1e-12, `压缩块应记压缩成本: ${comp.usage.costUsd}`)

// ===== 场景 9：checkpoint 恢复（cost_usd 随 usage 落盘）=====
setup()
hydrateCheckpoint('s1', {
  schema_version: 1,
  blocks: [
    { kind: 'agent', id: 1, run_id: 'sub-1', label: '审查', status: 'completed', spawned_at: 1000, task_id: 'a1', usage: { input: 1000, output: 200, cache_read: 800, cache_write: 0, reasoning: 0, cost_usd: WANT_COST } },
    { kind: 'async_task', id: 2, task_id: 'a2', label: '调研', status: 'completed', started_at: 1100, usage: { input: 500, output: 100, cache_read: 400, reasoning: 0, cost_usd: 0.0001 } },
    { kind: 'tool', id: 3, tool_id: 'call-1', name: 'bash', status: 'done', task_id: 'tooltask-1', promoted: true, task_status: 'completed', usage: { input: 300, output: 20, cache_read: 0, reasoning: 0, cost_usd: 0.00005 } },
    { kind: 'tool', id: 4, tool_id: 'call-2', name: 'read_file', status: 'done', result: 'ok' },
    { kind: 'compression', id: 5, before: 100, after: 20, ctx_tokens: 5000, model: 'm1', summary: 's', usage: { input: 700, output: 90, cache_read: 0, reasoning: 0, cost_usd: 0.00007 } },
    { kind: 'agent', id: 6, run_id: 'sub-2', label: '同步子 agent', status: 'completed', spawned_at: 1600, task_id: 'a3', usage: { input: 10, output: 5, cache_read: 0, reasoning: 0, cost_usd: 0.00001 }, items: [{ kind: 'tool', tool_id: 'call-9', name: 'subagent_x', status: 'done', result: 'ok', usage: { input: 900, output: 120, cache_read: 0, reasoning: 0, cost_usd: 0.00012 } }] },
  ],
  run_nodes: [],
  turns: [],
  usage: { input: 1800, output: 320, cache_read: 1200, cache_write: 0, reasoning: 0 },
  cost_usd: 0.01,
  degraded: false,
}, 1, { revision: 1, clearGeneration: 0, degraded: false })
const ra = blockOf('agent', 'a1')
assert.ok(Math.abs(ra.usage.costUsd - WANT_COST) < 1e-12, `恢复后子 agent 卡片成本应保留: ${ra.usage.costUsd}`)
assert.equal(ra.usage.input, 1000, '恢复后卡片 tokens 保留')
const rt = blockOf('async_task', 'a2')
assert.ok(Math.abs(rt.usage.costUsd - 0.0001) < 1e-12, '恢复后 async_task 卡片成本应保留')
const rtool = blockOf('tool', 'tooltask-1')
assert.ok(Math.abs(rtool.usage.costUsd - 0.00005) < 1e-12, '恢复后 promoted 工具任务成本应保留')
assert.equal(toolBlock('call-2').usage, undefined, '无用量工具行不产生空用量位')
const rcomp = view().blocks.find((b) => b.kind === 'compression')
assert.ok(Math.abs(rcomp.usage.costUsd - 0.00007) < 1e-12, `恢复后压缩块成本应保留: ${rcomp.usage.costUsd}`)
assert.equal(rcomp.usage.input, 700, '恢复后压缩块 tokens 保留')
const ritem = blockOf('agent', 'a3').items.find((it) => it.kind === 'tool')
assert.ok(Math.abs(ritem.usage.costUsd - 0.00012) < 1e-12, `恢复后卡片内工具项成本应保留: ${ritem.usage.costUsd}`)
assert.equal(view().costUsd, 0.01, '会话级成本仍走顶层 cost_usd（与卡片成本互不干扰）')

console.log('✓ task usage/cost：卡片展示、幂等、无用量不占位、恢复保留 — 全部通过')
