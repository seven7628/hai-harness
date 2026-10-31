// checkpoint hydrate（目标恢复主路径）store 行为回归：真实 store + esbuild，Node 直接跑。
//
// 覆盖：restored="checkpoint" 的 view_checkpoint 全 block 类型映射、归一化
// （running → abandoned / streaming 复位）、revision/clear_generation/degraded
// 元数据挂载、hydrate 后实时事件继续正常应用（不重复追加）。
//
// 用法：node scripts/test-checkpoint-hydrate.mjs
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

// 2) esbuild 打包 store（含真实 mock transport）到临时 CJS
const dir = mkdtempSync(join(tmpdir(), 'checkpoint-hydrate-'))
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
const { useAppStore, hydrateCheckpoint, dispatchEvent, materializeSnapshot } = require(outfile)

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
    usage: { input: 0, output: 0, cacheRead: 0, reasoning: 0 },
    turns: [],
    costUsd: 0,
    toolStartTs: {},
    rightExpanded: false,
    rightTab: 'activity',
  })
}
const view = () => useAppStore.getState().views.s1

const cp = (over = {}) => ({
  schema_version: 1,
  blocks: [
    { kind: 'user', id: 1, text: '你好', ts: 1000, contents: [{ type: 'image', content: 'data:image/png;base64,x', mime_type: 'image/png' }] },
    { kind: 'assistant', id: 2, text: '回复', thinking: '思考', model: 'm1', run_id: 'r1', ts: 1100, think_ms: 90, ttft_ms: 10, dur_ms: 500 },
    { kind: 'tool', id: 3, tool_id: 'call-1', name: 'write_file', args: '{"path":"a.txt"}', result: 'ok', status: 'done', diff: { path: 'a.txt', added: 1, removed: 0 }, started_at: 1200, dur_ms: 40 },
    { kind: 'agent', id: 4, run_id: 'sub-1', label: '调查', status: 'running', spawned_at: 1300, task_id: 'task-1', items: [{ kind: 'thinking', text: '想' }, { kind: 'tool', tool_id: 't1', name: 'grep', status: 'running' }] },
    { kind: 'async_task', id: 5, task_id: 'task-2', label: '后台', status: 'completed', started_at: 1400, ended_at: 1500, task_result: 'body' },
    { kind: 'task_result', id: 6, task_id: 'ghost', result: 'r', error: 'e' },
    { kind: 'task_delivery', id: 7, task_id: 'ghost', status: 'failed', result: 'r', error: 'e' },
    { kind: 'compression', id: 8, before: 10, after: 4, ctx_tokens: 500, model: 'm1', summary: 's', reason: 'automatic', dur_ms: 30 },
    { kind: 'system', id: 9, text: '提示', tone: 'info' },
    { kind: 'error', id: 10, text: '（运行失败）boom' },
  ],
  run_nodes: [
    { id: 'r1', label: '主会话', status: 'done', depth: 0, kind: 'run', duration_ms: 0 },
    { id: 'tool-call-1', parent_id: 'r1', label: 'write_file', status: 'done', depth: 0, kind: 'tool' },
  ],
  turns: [{ model: 'm1', input: 100, output: 20, cache_read: 50, cache_write: 12, reasoning: 30, dur_ms: 500, think_ms: 90, cost_usd: 0.001, run_id: 'r1' }],
  usage: { input: 100, output: 20, cache_read: 50, cache_write: 12, reasoning: 30 },
  cost_usd: 0.001,
  lifetime_usage: { input: 500, output: 100, cache_read: 200, reasoning: 60 },
  lifetime_cost_usd: 0.005,
  todos: [{ id: 't1', title: '任务', done: false, blocked_by: [] }],
  ctx_tokens: 4096,
  context_window: 128000,
  diffs: [{ path: 'a.txt', added: 1, removed: 0 }],
  degraded: false,
  ...over,
})

// ===== 场景 1：全 block 类型映射 =====
setup()
hydrateCheckpoint('s1', cp(), 99, { revision: 7, clearGeneration: 2, degraded: false })
let v = view()
assert.ok(v, '视图应已创建')
assert.equal(v.blocks.length, 10, '10 种 block 全部映射')
assert.equal(v.blocks[0].kind, 'user')
assert.equal(v.blocks[0].text, '你好')
assert.deepEqual(v.blocks[0].contents, [{ type: 'image', content: 'data:image/png;base64,x', mimeType: 'image/png' }], '图片附件映射')
assert.equal(v.blocks[1].kind, 'assistant')
assert.equal(v.blocks[1].text, '回复')
assert.equal(v.blocks[1].thinking, '思考')
assert.equal(v.blocks[1].streaming, false, 'checkpoint 块恒非流式')
assert.equal(v.blocks[1].thinkMs, 90)
assert.equal(v.blocks[1].ttftMs, 10)
assert.equal(v.blocks[2].kind, 'tool')
assert.equal(v.blocks[2].status, 'done')
assert.equal(v.blocks[2].diff?.added, 1)
assert.equal(v.blocks[3].kind, 'agent')
assert.equal(v.blocks[3].items.length, 2, 'agent 卡片 items 映射')
assert.equal(v.blocks[3].items[0].kind, 'thinking')
assert.equal(v.blocks[4].kind, 'async_task')
assert.equal(v.blocks[4].taskResult, 'body')
assert.equal(v.blocks[5].kind, 'task_result')
assert.equal(v.blocks[6].kind, 'task_delivery')
assert.equal(v.blocks[6].status, 'failed')
assert.equal(v.blocks[7].kind, 'compression')
assert.equal(v.blocks[7].before, 10)
assert.equal(v.blocks[7].after, 4)
assert.equal(v.blocks[8].kind, 'system')
assert.equal(v.blocks[8].tone, 'info')
assert.equal(v.blocks[9].kind, 'error')
assert.equal(v.runNodes.length, 2)
assert.equal(v.runNodes[1].kind, 'tool')
assert.equal(v.turns.length, 1)
assert.equal(v.turns[0].input, 100)
assert.equal(v.usage.input, 100)
// 缓存命中量必须从**蛇形** cache_read 读回（bridge wire 形态）：曾因读 camelCase cacheRead
// 恒为 0 → 重启/恢复后状态栏「缓存」掉到个位数百分比（用户实测 4.20%/23.88%）。
assert.equal(v.usage.cacheRead, 50, 'hydrate 必须读回 cache_read（会话累计缓存命中量）')
assert.equal(v.turns[0].cacheRead, 50, 'hydrate 必须读回每轮 cache_read（sparkline/当前上下文口径）')
// 写入量同源（Anthropic cache_creation）：只读 cache_read 时「本轮是不是在整段重写缓存」
// 在恢复后的会话里看不见（面板写入行会静默消失）。
assert.equal(v.usage.cacheWrite, 12, 'hydrate 必须读回 cache_write（会话累计写入量）')
assert.equal(v.turns[0].cacheWrite, 12, 'hydrate 必须读回每轮 cache_write（本轮写入量）')
assert.equal(v.costUsd, 0.001)
assert.equal(v.todos.length, 1)
assert.equal(v.todos[0].id, 't1')
assert.equal(v.ctxTokens, 4096)
assert.equal(v.contextWindow, 128000)
assert.deepEqual(v.diffs, [{ path: 'a.txt', added: 1, removed: 0 }])
// 元数据
assert.equal(v.checkpointRevision, 7)
assert.equal(v.checkpointGeneration, 2)
assert.equal(v.degraded, false)
// 瞬态语义：approvals/question/release/pendingQueue 不跨重启
assert.deepEqual(v.approvals, [])
assert.deepEqual(v.pendingQueue, [])
assert.equal(v.compressing, false)

// ===== 场景 2：归一化（running → abandoned / done，中断防御）=====
setup()
hydrateCheckpoint('s1', cp({ degraded: true }), 100, { revision: 9, clearGeneration: 1, degraded: true })
v = view()
assert.equal(v.blocks[3].status, 'abandoned', '子 agent running → abandoned')
// 卡片内 tool item 的 running 态与 materializeSnapshot 现状一致（finalizeReplayView
// 只归一化顶层块；卡片整体 abandoned 已表达"重启后不续跑"语义）
assert.equal(v.blocks[3].items[1].status, 'running')
assert.equal(v.blocks[4].status, 'completed')
assert.equal(v.degraded, true, 'degraded 标记透出')
assert.equal(v.streaming, false)
assert.equal(v.llmStartTs, undefined)

// ===== 场景 3：hydrate 后实时事件继续正常应用（不重复追加）=====
setup()
hydrateCheckpoint('s1', cp(), 101)
dispatchEvent({ event_type: 'llm_start', session_id: 's1', run_id: 'r2', request_id: 'q2', model: 'm1' })
dispatchEvent({ event_type: 'content_chunk', session_id: 's1', run_id: 'r2', request_id: 'q2', content: '追加', index: 0 })
dispatchEvent({ event_type: 'llm_end', session_id: 's1', run_id: 'r2', request_id: 'q2', model: 'm1', content: '追加', usage: { Input: 10, Output: 2, CacheRead: 4, CacheWrite: 3 }, finish_reason: 'stop' })
v = view()
// 原 10 块 - 主 llm_end 清除 error 块(1) + 新 assistant 块(1) = 10；新块 id 在 checkpoint 最大 id 之后
assert.equal(v.blocks.length, 10, 'hydrate 后事件正常应用（+1 新 assistant，-1 error 被主 llm_end 清除）')
assert.equal(v.blocks[9].kind, 'assistant')
assert.equal(v.blocks[9].id, 11, '新块 id 在 checkpoint 最大 id 之后（单调，不重复）')
assert.ok(v.blocks[9].text.includes('追加'), 'chunk 追加到新块')
assert.equal(v.blocks[9].streaming, false, 'llm_end 落地')
assert.equal(v.turns.length, 2, '新 turn 追加（1 旧 + 1 新）')
assert.equal(v.usage.input, 110, 'usage 在 hydrate 基础上继续累计')
// 缓存写入量同源累计（llm_end 事件 → usage.cacheWrite / 每轮 turn.cacheWrite）：
// 只累计 cacheRead 时「本轮是不是在整段重写缓存」在运行期就看不见（面板写入行会一直空）。
assert.equal(v.usage.cacheWrite, 15, 'usage.cacheWrite 继续累计（hydrate 12 + 本轮 3）')
assert.equal(v.turns[1].cacheWrite, 3, '每轮 turn.cacheWrite = 本轮写入量')

// ===== 场景 4：clear 代际后的 checkpoint（清空提示块 + 展示 usage 归零 + lifetime 保留）=====
setup()
hydrateCheckpoint('s1', cp({
  // 实现 2026-08 变更：「（上下文已清空）」文本块被过滤（checkpointToView），
  // clear 语义由 divider(reason=clear) 表达——与 Go 侧 NormalizeForRestore 对齐。
  blocks: [{ kind: 'divider', id: 1, divider_reason: 'clear', ts: 2000 }],
  run_nodes: [], turns: [], usage: { input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: 0 },
  cost_usd: 0, lifetime_usage: { input: 500, output: 0, cache_read: 0, reasoning: 0 }, lifetime_cost_usd: 0.005,
  diffs: [], todos: [{ id: 't1', title: '任务', done: false, blocked_by: [] }],
}), 102, { revision: 8, clearGeneration: 1 })
v = view()
assert.equal(v.blocks.length, 1)
assert.equal(v.blocks[0].kind, 'divider')
assert.equal(v.blocks[0].reason, 'clear')
assert.equal(v.turns.length, 0)
assert.equal(v.usage.input, 0)
assert.equal(v.todos.length, 1, 'clear 后会话待办保留')

// ===== 场景 5：重试进度（llm_error 文案 + 实时状态字段 + 快照恢复）=====
// 2026-09-18：Attempt 语义 = 已失败的那一次。旧文案「正在重试 1…」在下一次尝试挂住时
// （实测 187s 无首字）被读成「卡住/重试没了」→ 文案改「第 N/M 次失败 → 重试第 N+1/M 次」，
// 并把 ts/retryDelayMs 落进块里给 Narrative 的 RetryProgress 算倒计时与已等待时长。
setup()
const errEv = (over) => ({
  event_type: 'llm_error', session_id: 's1', run_id: 'r1',
  message: 'read tcp 192.168.3.195:64654->172.65.90.21:443: read: connection reset by peer',
  attempt: 1, max_attempts: 11, will_retry: true, retry_delay_ms: 1000,
  error_kind: 'upstream_unavailable', timestamp: 1000, ...over,
})
dispatchEvent(errEv({}))
v = view()
let errB = v.blocks.find((b) => b.kind === 'error')
assert.ok(errB, 'llm_error 应产生错误块')
assert.ok(errB.text.includes('第 1/11 次尝试失败'), `文案应含进度: ${errB.text}`)
assert.ok(errB.text.includes('重试第 2/11 次'), `文案应含下一次: ${errB.text}`)
assert.equal(errB.retrying, true, '重试态（RetryProgress 开始计时）')
assert.equal(errB.retryAttempt, 1)
assert.equal(errB.retryMax, 11)
assert.equal(errB.retryDelayMs, 1000, '倒计时窗口')
assert.equal(errB.ts, 1000, '锚点 = 错误事件时刻（下一次尝试起点 = ts + retryDelayMs）')

// 失败尝试记账（2026-09-23 决策「失败尝试也要记账」）：llm_error 携带的 cost_usd
//（桥按价表把部分用量折算的金额）→ 会话成本累计 + 错误块「重试已花费」跨次累加。
const near = (a, b) => Math.abs(a - b) < 1e-12
dispatchEvent(errEv({ attempt: 1, cost_usd: 0.00002 }))
errB = view().blocks.find((b) => b.kind === 'error')
assert.ok(near(errB.retryCostUsd, 0.00002), `错误块应记重试成本: ${errB.retryCostUsd}`)
assert.ok(near(view().costUsd, 0.00002), `会话成本应含失败尝试: ${view().costUsd}`)

// 第 2 次失败：同一错误块原位更新（不刷屏），进度前移
dispatchEvent(errEv({ attempt: 2, timestamp: 3000, cost_usd: 0.00003 }))
v = view()
const errs = v.blocks.filter((b) => b.kind === 'error')
assert.equal(errs.length, 1, '多次重试只留一条错误块')
assert.ok(errs[0].text.includes('第 2/11 次尝试失败') && errs[0].text.includes('重试第 3/11 次'), errs[0].text)
assert.equal(errs[0].retryAttempt, 2)
assert.equal(errs[0].ts, 3000, '锚点随最新失败前移')
assert.equal(errs[0].retryStartedAt, 1000, '本轮重试起点保持首个失败时刻（累计「已等待时长」）')
assert.ok(near(errs[0].retryCostUsd, 0.00005), `重试成本跨次累加（0.00002 + 0.00003）: ${errs[0].retryCostUsd}`)
assert.ok(near(view().costUsd, 0.00005), `会话成本累计两次失败尝试: ${view().costUsd}`)

// 重试成功：主 llm_end 清除错误块（RetryProgress 随之卸载，计时停止）
dispatchEvent({ event_type: 'llm_start', session_id: 's1', run_id: 'r1', request_id: 'q3', model: 'm1' })
dispatchEvent({ event_type: 'llm_end', session_id: 's1', run_id: 'r1', request_id: 'q3', model: 'm1', content: 'ok', usage: { Input: 10, Output: 1 }, finish_reason: 'stop' })
assert.equal(view().blocks.filter((b) => b.kind === 'error').length, 0, '成功后清除错误块')

// 重试耗尽：终态不打重试态（否则计时器会一直走）+ 归因上游
dispatchEvent(errEv({ attempt: 11, will_retry: false, retry_delay_ms: 0, message: '上游无响应' }))
errB = view().blocks.find((b) => b.kind === 'error')
assert.ok(errB.text.includes('重试已耗尽'), errB.text)
assert.ok(errB.text.includes('供应商侧瞬时故障'), '上游不可用耗尽需给归因')
assert.equal(errB.retrying, false)
assert.equal(errB.retryStartedAt, undefined, '耗尽后不再计「已等待时长」')
// 成功后错误块已被清除 → 新块从零起算；但**会话成本**保留（真花掉的钱不随块消失）
assert.equal(errB.retryCostUsd, undefined, '错误块被清除后新块不带旧成本')
assert.ok(near(view().costUsd, 0.00005), `会话成本保留重试浪费: ${view().costUsd}`)

// 快照恢复：重试进度字段随 checkpoint 回到 UI（刷新/重连后倒计时仍能算）
setup()
hydrateCheckpoint('s1', cp({
  blocks: [{ kind: 'error', id: 1, text: '（LLM 请求失败：第 3/11 次尝试失败 → 重试第 4/11 次）boom', ts: 5000, retrying: true, retry_attempt: 3, retry_max_attempts: 11, retry_delay_ms: 1000, retry_started_at: 2000, retry_cost_usd: 0.00005 }],
}), 103)
v = view()
assert.equal(v.blocks[0].kind, 'error')
assert.equal(v.blocks[0].retrying, true)
assert.equal(v.blocks[0].retryAttempt, 3)
assert.equal(v.blocks[0].retryMax, 11)
assert.equal(v.blocks[0].retryDelayMs, 1000)
assert.equal(v.blocks[0].ts, 5000)
assert.equal(v.blocks[0].retryStartedAt, 2000, '累计等待锚点随快照恢复')
assert.ok(near(v.blocks[0].retryCostUsd, 0.00005), `重试成本随快照恢复: ${v.blocks[0].retryCostUsd}`)

// ===== 场景 6：维度补齐（2026-09-23）—— 思考/1h 缓存写入 =====
// Reasoning ⊆ Output、CacheWrite1h ⊆ CacheWrite：这两个维度此前完全缺失（思考只有 token、
// 1h 写入连 token 都没有）。此处钉住会话累计与快照恢复两条链路。
setup()
dispatchEvent({
  event_type: 'llm_end', session_id: 's1', run_id: 'r1', request_id: 'q9', model: 'claude-sonnet-4-5',
  content: 'ok', finish_reason: 'stop',
  usage: { Input: 1080, Output: 200, CacheRead: 120, CacheWrite: 420, CacheWrite1h: 420, Reasoning: 150, TotalTokens: 1280 },
  cost_usd: 0.004416,
})
v = view()
assert.equal(v.usage.cacheWrite, 420)
assert.equal(v.usage.cacheWrite1h, 420, '1h 档写入量应进会话累计（状态栏 tooltip 数据源）')
assert.equal(v.usage.reasoning, 150, '思考 token 累计不受影响')

setup()
hydrateCheckpoint('s1', cp({
  usage: { input: 1080, output: 200, cache_read: 120, cache_write: 420, cache_write_1h: 420, reasoning: 150 },
}), 104)
v = view()
assert.equal(v.usage.cacheWrite1h, 420, '1h 写入量随快照恢复（重启后 tooltip 仍能区分 5m/1h）')

// ===== 场景 7：legacy 快照重放（restored=snapshot）—— llm_end 必须能补出正文/思考 =====
// 现场（2026-09-23）：bridge 事件日志**不落盘流式 chunk**（isStreamingChunkType），历史重放
// 只剩 llm_end；前端此前只在 llm_end 里把块标成「非流式」，不读 content/reasoning，于是
// 快照恢复后每个 assistant 块都是空的 → Narrative 跳过 → 对话只剩工具/产出块
//（用户截图：「完全看不到 Reasoning、Content」）。Go reducer 一直有这层兜底，前端补齐。
setup()
materializeSnapshot('s1', [
  { event_type: 'agent_start', run_id: 'r1', content: '任务' },
  { event_type: 'llm_start', run_id: 'r1', request_id: 'q1', model: 'm1' },
  { event_type: 'tool_run_start', run_id: 'r1', tool_id: 't1', name: 'read_file', args: '{"path":"a.go"}' },
  { event_type: 'tool_run_end', run_id: 'r1', tool_id: 't1', name: 'read_file', result: 'ok' },
  { event_type: 'llm_start', run_id: 'r1', request_id: 'q2', model: 'm1' },
  // 无任何 content_chunk / reasoning_chunk（日志里本来就没有）
  { event_type: 'llm_end', run_id: 'r1', request_id: 'q2', model: 'm1', content: '最终正文', reasoning: '最终思考', finish_reason: 'stop' },
], undefined, true)
{
  const repl = view()
  const asst = repl.blocks.filter((b) => b.kind === 'assistant')
  assert.equal(asst.length, 2, `两轮各一块 assistant（实得 ${asst.length}）`)
  assert.equal(asst[0].text || '', '', '工具轮无正文')
  assert.equal(asst[1].text, '最终正文', 'llm_end 的 content 必须补进块（否则对话看不到回复）')
  assert.equal(asst[1].thinking, '最终思考', 'llm_end 的 reasoning 必须补进块')
  assert.equal(asst[1].streaming, false)
}

// 已有 chunk 的块不被 llm_end 覆盖（实时/半实时重放：chunk 增量优先）
setup()
dispatchEvent({ event_type: 'llm_start', session_id: 's1', run_id: 'r2', request_id: 'q3', model: 'm1' })
dispatchEvent({ event_type: 'content_chunk', session_id: 's1', run_id: 'r2', request_id: 'q3', content: '流式正文' })
dispatchEvent({ event_type: 'llm_end', session_id: 's1', run_id: 'r2', request_id: 'q3', model: 'm1', content: '流式正文', reasoning: '流式思考', finish_reason: 'stop' })
{
  const blk = view().blocks.find((b) => b.kind === 'assistant')
  assert.equal(blk.text, '流式正文', 'chunk 已填的正文不重复/不覆盖')
  assert.equal(blk.thinking, '流式思考', 'chunk 缺失的思考由 llm_end 补上（块为空即补）')
}

console.log('CHECKPOINT_HYDRATE_TEST_PASS')