// 客户端暂存队列 store 行为回归测试（真实 store + esbuild 打包，Node 直接跑，无需浏览器/vitest）。
//
// 覆盖：Agent 运行中连发 → 本地暂存（不立即推送）；Agent End → 会话回到空闲；空闲再发 →
// 立即整队推送（残留暂存 + 新消息一起 ask_batch），绝不把新消息误判为 busy 塞回暂存。
//
// 用法：npm run test:store
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
const dir = mkdtempSync(join(tmpdir(), 'store-queue-'))
const outfile = join(dir, 'store-bundle.cjs')
// mock transport 会 import 带 `?raw` / `?url` / `?no-inline` 后缀的静态资源
// （README、logo、截图）。esbuild 不认识这些后缀，必须显式 stub 掉，
// 否则打包阶段直接失败（此脚本因此长期跑不起来）。
const assetPlugin = {
  name: 'asset-suffix',
  setup(b) {
    b.onResolve({ filter: /\?(raw|url|no-inline)$/ }, () => ({
      path: 'asset',
      namespace: 'asset-stub',
    }))
    b.onLoad({ filter: /.*/, namespace: 'asset-stub' }, () => ({
      contents: 'module.exports = ""',
      loader: 'js',
    }))
  },
}

await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile,
  logLevel: 'silent',
  plugins: [assetPlugin],
})
const { useAppStore, sessionBusy, mainRunActive, dispatchEvent, applySessionPrefs, runningTaskCount, hydrateCheckpoint, __runSettles, __clearRunSettles } = require(outfile)
// 2026-10：右栏「任务」tab 退役后 asyncTaskBlocks（给它用的平铺投影）已删除。
// 它原来的两条判据（「有占位卡」「占位有状态」）在这里按**块本身**判，不再借道那个投影。
const liveTasks = (blocks) => blocks.filter((b) => b.kind === 'async_task' || b.kind === 'agent'
  || (b.kind === 'tool' && b.promoted))

const text = (q) => q.contents.map((c) => (c.type === 'text' ? c.content : '')).join('\n')

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
  })
}
const pending = () => useAppStore.getState().views.s1?.pendingQueue ?? []
const busy = () => {
  const v = useAppStore.getState().views.s1
  return v ? sessionBusy(v) : false
}
const EV = (t, p = {}) => dispatchEvent({ event_type: t, session_id: 's1', ...p })

// ===== 场景 1：空闲 → 发送 → 会话进入运行态 → Agent End → 空闲 → 再发立即推送 =====
setup()
useAppStore.getState().ask('第一条') // 空闲 → 立即推送（启动 run）
assert.deepStrictEqual(pending().map(text), ['第一条'])
EV('agent_start', { run_id: 'r1' })
assert.ok(busy(), 'agent_start 后应 busy')
EV('agent_end', { run_id: 'r1', finish_reason: 'stop' })
assert.ok(!busy(), 'Agent End 后应空闲')
useAppStore.getState().ask('第二条') // 空闲 → 立即推送
assert.ok(pending().find((q) => text(q) === '第二条')?.pushed, '空闲再发应立即推送')
console.log('PASS 场景1：Agent End 后空闲，新消息立即推送')

// ===== 场景 2（核心回归）：运行中连发统一在 llm_end 整队 ask_batch；agent_end 不补发 =====
setup()
useAppStore.getState().ask('M1')
EV('agent_start', { run_id: 'r1' })
useAppStore.getState().ask('运行中A')
useAppStore.getState().ask('运行中B')
let staged = pending().filter((q) => !q.pushed).map(text)
assert.deepStrictEqual(staged, ['运行中A', '运行中B'], '运行中连发应暂存，不逐条推送')
EV('llm_end', { run_id: 'r1', request_id: 'req-batch', model: 'gpt-test', usage: {} })
assert.ok(pending().every((q) => q.pushed), 'llm_end 应把运行中积累的消息一次性 ask_batch 推送')
assert.deepStrictEqual(pending().map(text), ['M1', '运行中A', '运行中B'], '批量内容及顺序应保持')
EV('agent_end', { run_id: 'r1', finish_reason: 'stop' })
assert.ok(!busy(), 'Agent End 后会话应空闲')
staged = pending().filter((q) => !q.pushed).map(text)
assert.deepStrictEqual(staged, [], 'agent_end 不应留下或补发未推送消息')
// 空闲没有未来 llm_end：新消息必须立即以 ask_batch 启动下一轮（用户已确认）。
useAppStore.getState().ask('新消息')
assert.ok(pending().find((q) => text(q) === '新消息')?.pushed, '空闲消息应立即 ask_batch，避免等待不存在的 llm_end')
console.log('PASS 场景2：忙时消息在 llm_end 统一批量发送；空闲消息立即启动下一轮')

// ===== 场景 2b（回归）：推送时机 = 本轮结束；编辑窗口 = 推送之前 =====
// 用户反馈①：消息进了等待队列后点「停止」，队列数据没有自动推送到服务端（一直挂「待发送」）。
// 用户反馈②：修好后「立马就推送」，没有编辑时间 —— 明确语义：队列项在**推送前**始终可逐条
// 编辑/删除（运行中 llm_start→llm_end 之间、点停止后中断生效前、LLM 异常重试期间），
// 推送只发生在「本轮真的结束了」那一刻（llm_end / 停止生效 / 运行失败）。
setup()
useAppStore.getState().ask('先来一条')
EV('agent_start', { run_id: 'r-stop' })
useAppStore.getState().ask('停止前的A')
useAppStore.getState().ask('停止前的B')
let staged2b = pending().filter((q) => !q.pushed)
assert.deepStrictEqual(staged2b.map(text), ['停止前的A', '停止前的B'], '运行中连发应暂存（未推送）')
assert.equal(staged2b.length, 2, '待发送必须非空（否则下面的 every() 会空真）')
// 运行中：可编辑、可删除（编辑窗口）
useAppStore.getState().updatePending(staged2b[0].id, '停止前改过的A')
assert.deepStrictEqual(pending().filter((q) => !q.pushed).map(text), ['停止前改过的A', '停止前的B'], '运行中应可逐条编辑')
useAppStore.getState().removePending(pending().find((q) => text(q) === '停止前的B').id)
assert.deepStrictEqual(pending().filter((q) => !q.pushed).map(text), ['停止前改过的A'], '运行中应可删除待发送项')
// 异常重试阶段：同样可编辑（用户明确要求）
EV('llm_error', {
  run_id: 'r-stop', attempt: 1, max_attempts: 11, will_retry: true, retry_delay_ms: 1000,
  error_kind: 'upstream_unavailable', message: 'upstream unavailable',
})
assert.deepStrictEqual(pending().filter((q) => !q.pushed).map(text), ['停止前改过的A'], '重试阶段不得推送（保持可编辑）')
useAppStore.getState().ask('重试时又输入的一条')
assert.deepStrictEqual(
  pending().filter((q) => !q.pushed).map(text),
  ['停止前改过的A', '重试时又输入的一条'],
  '重试阶段连发同样暂存（可编辑）',
)
// 点「停止」→ 中断在途（agent_end 尚未到达）：仍可编辑、尚未推送
useAppStore.getState().interrupt()
assert.deepStrictEqual(
  pending().filter((q) => !q.pushed).map(text),
  ['停止前改过的A', '重试时又输入的一条'],
  '中断生效前（agent_end 未到）必须保持可编辑',
)
EV('agent_end', { run_id: 'r-stop', finish_reason: 'abort' }) // 中断生效：后端 abort 收尾
assert.equal(pending().filter((q) => !q.pushed).length, 0, '推送后不得留下未推送项（推送只打 pushed 标记）')
assert.ok(pending().every((q) => q.pushed), '中断生效后必须把暂存队列整队推送到服务端')
assert.ok(!busy(), '中断后会话应空闲')
// 子 agent 的中断不得借机启动主会话新一轮
setup()
useAppStore.getState().ask('主消息')
EV('agent_start', { run_id: 'r-main' })
EV('agent_start', { run_id: 'r-sub', parent_run_id: 'r-main', name: 'sub' })
useAppStore.getState().ask('子跑时的输入')
EV('agent_end', { run_id: 'r-sub', finish_reason: 'abort' })
assert.deepStrictEqual(
  pending().filter((q) => !q.pushed).map(text),
  ['子跑时的输入'],
  '子 agent 中断不得推送暂存队列（主 run 未结束）',
)
// 运行失败（session_run_error）：同样整队推送（用户要求补充）—— 失败后不会再有 llm_end，
// 不推则「失败前排队等待的消息」永远滞留本机。
setup()
useAppStore.getState().ask('M1')
EV('agent_start', { run_id: 'r-err' })
useAppStore.getState().ask('失败前排队的消息')
EV('agent_end', { run_id: 'r-err', finish_reason: 'error' })
assert.deepStrictEqual(
  pending().filter((q) => !q.pushed).map(text),
  ['失败前排队的消息'],
  'agent_end(error) 本身不推送（等会话级失败事件，避免错误块与新 run 交错）',
)
EV('session_run_error', { error: 'upstream failed', error_kind: 'permanent' })
assert.equal(pending().filter((q) => !q.pushed).length, 0, '失败推送后不得留下未推送项')
assert.ok(pending().every((q) => q.pushed), '运行失败后必须把暂存队列整队推送到服务端')
assert.ok(!busy(), '失败后会话应收敛为空闲')
// 自然结束（stop）仍不推送：既有语义（避免用旧输入触发「意外新 run」）；但下一条输入会整队推送
setup()
useAppStore.getState().ask('M1')
EV('agent_start', { run_id: 'r-tail' })
EV('llm_end', { run_id: 'r-tail', request_id: 'req-tail', model: 'gpt-test', usage: {} })
useAppStore.getState().ask('末轮后输入')
assert.deepStrictEqual(pending().filter((q) => !q.pushed).map(text), ['末轮后输入'], 'llm_end 之后的输入仍暂存')
EV('agent_end', { run_id: 'r-tail', finish_reason: 'stop' })
assert.deepStrictEqual(
  pending().filter((q) => !q.pushed).map(text),
  ['末轮后输入'],
  '自然结束不得补发暂存消息（既有语义不变）',
)
useAppStore.getState().ask('下一条')
assert.ok(pending().every((q) => q.pushed), '空闲时发送应把残留暂存一起整队推送（消息不丢）')
console.log('PASS 场景2b：推送时机 = 本轮结束（停止/失败）；编辑窗口 = 推送前（运行中/重试中/停止生效前）')

// ===== 场景 3：LLM 重试/最终失败在叙述通道可见，且 run/streaming 收敛 =====
setup()
EV('agent_start', { run_id: 'r-error' })
EV('llm_error', {
  run_id: 'r-error', attempt: 1, max_attempts: 11, will_retry: true, retry_delay_ms: 1000,
  error_kind: 'upstream_unavailable', message: 'upstream unavailable', timestamp: 1000,
})
let view = useAppStore.getState().views.s1
// 文案语义（2026-09-18）：Attempt 是**已失败的那一次** —— 必须显示「第 N/M 次失败 → 重试
// 第 N+1/M 次」（旧的「正在重试 1」在下一次尝试挂住时被读成「卡住/重试没了」）。
let errBlock = view.blocks.find((b) => b.kind === 'error')
assert.ok(errBlock && errBlock.text.includes('第 1/11 次尝试失败'), '重试应显示进度错误块')
assert.ok(errBlock.text.includes('重试第 2/11 次'), '重试应显示下一次尝试序号')
// RetryProgress 的实时状态依据（倒计时 → 本次尝试已进行 Xs）
assert.equal(errBlock.retrying, true, '重试态应落块（前端据此计时）')
assert.equal(errBlock.retryAttempt, 1)
assert.equal(errBlock.retryMax, 11)
assert.equal(errBlock.retryDelayMs, 1000)
assert.equal(errBlock.retryStartedAt, 1000, '本轮重试起点 = 首个失败时刻')
assert.equal(view.streaming, true, '重试期间停止按钮应保持可用')
EV('agent_end', { run_id: 'r-error', finish_reason: 'error' })
EV('session_run_error', { error: 'upstream failed', error_kind: 'permanent' })
view = useAppStore.getState().views.s1
assert.ok(view.blocks.some((b) => b.kind === 'error' && b.text.includes('（运行失败）upstream failed')), '最终失败应更新为可见错误')
errBlock = view.blocks.find((b) => b.kind === 'error')
assert.equal(errBlock.retrying, undefined, '最终失败后不得再打重试态（否则计时器一直走）')
assert.equal(view.streaming, false, '最终失败应清流式态')
assert.ok(!sessionBusy(view), '最终失败后 run 应收敛为空闲')
console.log('PASS 场景3：重试与最终失败均在叙述通道可见')

// ===== 场景 4：重试后成功清除临时错误块 =====
setup()
EV('agent_start', { run_id: 'r-recover' })
EV('llm_error', { run_id: 'r-recover', attempt: 1, will_retry: true, message: 'temporary' })
EV('llm_start', { run_id: 'r-recover', request_id: 'req-ok', model: 'gpt-test' })
EV('llm_end', { run_id: 'r-recover', request_id: 'req-ok', model: 'gpt-test', usage: {} })
view = useAppStore.getState().views.s1
assert.ok(!view.blocks.some((b) => b.kind === 'error'), '成功响应应清除临时重试错误块')
console.log('PASS 场景4：重试成功后清除临时错误块')

// ===== 场景 5：session view 严格 10 项 LRU（VIEW_CACHE_MAX=10）；活跃会话保留，最久未访问项淘汰 =====
setup()
for (let i = 1; i <= 11; i++) {
  dispatchEvent({ event_type: 'agent_start', session_id: `s${i}`, run_id: `r${i}` })
  dispatchEvent({ event_type: 'agent_end', session_id: `s${i}`, run_id: `r${i}`, finish_reason: 'stop' })
}
// setView 在首次创建后台 view 后以 microtask 执行淘汰，等待队列收敛。
await Promise.resolve()
const cached = Object.keys(useAppStore.getState().views)
assert.ok(cached.length <= 10, `session view 缓存应最多 10 项，实际 ${cached.length}`)
assert.ok(cached.includes('s1'), '当前活跃 session 必须保留')
assert.ok(!cached.includes('s2'), '应淘汰最久未访问的非活跃 session')
assert.ok(cached.includes('s11'), '最近访问 session 应保留')
console.log('PASS 场景5：空闲 session view 严格 10 项 LRU')

// ===== 场景 6：并发运行 session 不淘汰；结束后自动按 10 项 LRU 收敛（VIEW_CACHE_MAX=10） =====
setup()
for (let i = 1; i <= 12; i++) {
  dispatchEvent({ event_type: 'agent_start', session_id: `s${i}`, run_id: `concurrent-${i}` })
}
await Promise.resolve()
let concurrent = useAppStore.getState().views
assert.equal(Object.keys(concurrent).length, 12, '12 个并发运行 session 必须全部保留，允许临时超过 10')
for (let i = 1; i <= 12; i++) assert.ok(sessionBusy(concurrent[`s${i}`]), `s${i} 应保持运行态`)
// 结束两个非活跃会话；agent_end 会安排 LRU 收敛。其余 10 个运行会话仍不可淘汰。
for (const i of [2, 3]) {
  dispatchEvent({ event_type: 'agent_end', session_id: `s${i}`, run_id: `concurrent-${i}`, finish_reason: 'stop' })
}
await Promise.resolve()
concurrent = useAppStore.getState().views
assert.equal(Object.keys(concurrent).length, 10, '并发会话结束后应自动收敛到 10 项')
assert.ok(concurrent.s1, '当前活跃 session 必须保留')
for (const i of [4, 5, 6, 7, 8, 9, 10, 11, 12]) assert.ok(concurrent[`s${i}`], `仍在运行的 s${i} 不得淘汰`)
assert.ok(!concurrent.s2 && !concurrent.s3, '已结束且最久未访问的会话应被淘汰')
console.log('PASS 场景6：并发运行会话全部保留，结束后 LRU 自动收敛到 10')

// ===== 场景 7：不同 session 的 provider/model/mode/effort 在缓存切换与冷恢复时独立 =====
setup()
const settings = useAppStore.getState().settings
useAppStore.setState({
  workspace: '/mock/w', // 与 path 一致：缓存分支（sameProject=true）才走 applySessionPrefs 恢复
  settings: {
    ...settings,
    provider: {
      ...settings.provider,
      active: 'deepseek',
      deepseek: { ...settings.provider.deepseek, models: { 'deepseek-chat': 128000 } },
      opencode: { ...settings.provider.opencode, models: { 'deepseek-v4-pro': 1000000 } },
    },
  },
  sessions: [{ id: 's1', title: 'A' }, { id: 's2', title: 'B' }],
  projects: [{ path: '/mock/w', sessions: [{ id: 's1', title: 'A' }, { id: 's2', title: 'B' }] }],
})
// 建立 A 的缓存 view（需 hydrated=true 才走缓存恢复分支；blocks>0 已不再算「已恢复」——
// 后台尾部事件/陈旧 preBuf 也会让 blocks>0）。
EV('agent_start', { run_id: 'prefs-a' })
EV('agent_end', { run_id: 'prefs-a', finish_reason: 'stop' })
// 标记 s1 view 已完整恢复（hydrated 是 setActiveSession 走缓存分支的唯一判据）
useAppStore.setState((s) => ({
  views: { ...s.views, s1: { ...s.views.s1, hydrated: true } },
}))
useAppStore.getState().switchMode('hitl')
useAppStore.getState().switchEffort('high')
useAppStore.getState().switchModel('deepseek-v4-pro', 'opencode')
let savedPrefs = JSON.parse(mem.get('go-code.sessionPrefs'))
assert.deepStrictEqual(
  savedPrefs['/mock/w s1'],
  { mode: 'hitl', model: 'deepseek-v4-pro', provider: 'opencode', effort: 'high', persona: 'code' },
  'A 应持久化完整 provider/model/mode/effort',
)
// 切到 B（首次冷恢复），再保存另一组配置。
useAppStore.getState().setActiveSession('s2', '/mock/w')
useAppStore.getState().switchMode('auto')
useAppStore.getState().switchEffort('low')
useAppStore.getState().switchModel('deepseek-chat', 'deepseek')
savedPrefs = JSON.parse(mem.get('go-code.sessionPrefs'))
assert.equal(savedPrefs['/mock/w s2'].provider, 'deepseek', 'B 应保存自己的 Provider')
// A 仍在缓存：切回时也必须恢复其配置，而不是沿用 B。
useAppStore.getState().setActiveSession('s1', '/mock/w')
let prefState = useAppStore.getState()
assert.equal(prefState.settings.provider.active, 'opencode', '缓存切回 A 应恢复 Provider')
assert.equal(prefState.model, 'deepseek-v4-pro', '缓存切回 A 应恢复 model')
assert.equal(prefState.mode, 'hitl', '缓存切回 A 应恢复 mode')
assert.equal(prefState.effort, 'high', '缓存切回 A 应恢复 effort')
// 模拟 LRU 淘汰/应用重启后的冷恢复：删掉 A view，再从 new_session 响应路径恢复。
const withoutA = { ...prefState.views }
delete withoutA.s1
useAppStore.setState({ views: withoutA, activeSessionId: 's1', settings: { ...prefState.settings, provider: { ...prefState.settings.provider, active: 'deepseek' } }, model: 'deepseek-chat', mode: 'auto', effort: 'low' })
applySessionPrefs('/mock/w', 's1')
prefState = useAppStore.getState()
assert.equal(prefState.settings.provider.active, 'opencode', '冷恢复 A 应恢复 Provider')
assert.equal(prefState.model, 'deepseek-v4-pro', '冷恢复 A 应恢复 model')
assert.equal(prefState.mode, 'hitl', '冷恢复 A 应恢复 mode')
assert.equal(prefState.effort, 'high', '冷恢复 A 应恢复 effort')
console.log('PASS 场景7：会话级 Provider/model/mode/effort 缓存切换与冷恢复均独立')

// ===== 场景 8：warm session 快照不把真实运行任务伪标完成；task_end 后才完成 =====
setup()
EV('task_started', { task_id: 'task-live', name: 'review reminder semantics', status: 'running' })
let liveView = useAppStore.getState().views.s1
assert.equal(runningTaskCount(liveView.blocks), 1, '仅 task_started 时数量徽章也应显示 1')
assert.equal(liveTasks(liveView.blocks).length, 1, '仅 task_started 时应有一个任务占位（agent_start 还没合并）')
EV('agent_start', { run_id: 'sub-live', parent_run_id: 'root-live', name: 'review reminder semantics' })
liveView = useAppStore.getState().views.s1
assert.ok(liveView.blocks.some((b) => (b.kind === 'async_task' || b.kind === 'agent') && b.taskId === 'task-live' && b.status === 'running'), '运行任务应显示 running')
assert.ok(liveView.blocks.some((b) => b.kind === 'agent' && b.runId === 'sub-live' && b.status === 'running'), '运行 agent 应显示 running')
assert.equal(runningTaskCount(liveView.blocks), 1, 'task_started 占位与 agent 卡片合并后徽章不得重复或消失')
// 真实 warm 切换由 bridge data.live=true 标识；其快照不得走冷恢复 finalize。
// 这里直接验证 task_end 是唯一把 async task 标为完成的事件。
EV('task_end', { task_id: 'task-live', status: 'completed', result: 'done' })
liveView = useAppStore.getState().views.s1
assert.ok(liveView.blocks.some((b) => (b.kind === 'async_task' || b.kind === 'agent') && b.taskId === 'task-live' && b.status === 'completed'), 'task_end 后才应显示 completed')
EV('task_result_delivered', { task_id: 'task-live', status: 'completed', result: '# 真实任务结果' })
liveView = useAppStore.getState().views.s1
const deliveredCard = liveView.blocks.find((b) => (b.kind === 'async_task' || b.kind === 'agent') && b.taskId === 'task-live')
assert.equal(deliveredCard?.deliveredToMain, true, '交付事件应标记匹配卡片已交付主任务')
assert.ok(deliveredCard?.taskResult?.includes('真实任务结果'), '完整结果应只挂在匹配卡片上')
const notices = liveView.blocks.filter((b) => b.kind === 'task_delivery' && b.taskId === 'task-live')
assert.equal(notices.length, 1, '交付事件应追加一条轻量叙述通知')
assert.equal(notices[0].status, 'completed', '交付通知应携带终态')
assert.ok(!liveView.blocks.some((b) => b.kind === 'task_result' && b.taskId === 'task-live'), '已有卡片时不得复制完整结果块')
EV('task_result_delivered', { task_id: 'task-live', status: 'completed', result: '# 真实任务结果' })
liveView = useAppStore.getState().views.s1
assert.equal(liveView.blocks.filter((b) => b.kind === 'task_delivery' && b.taskId === 'task-live').length, 1, '重复交付不得重复通知')
console.log('PASS 场景8：运行任务不提前完成；交付卡片、轻量通知与去重正确')

// ===== 场景 9：后端六态与旧状态兼容；interrupted 不得伪装完成 =====
setup()
for (const [taskId, status, expected] of [
  ['new-interrupting', 'interrupting', 'interrupting'],
  ['new-interrupted', 'interrupted', 'interrupted'],
  ['new-failed', 'failed', 'failed'],
  ['new-abandoned', 'abandoned', 'abandoned'],
  ['legacy-done', 'done', 'completed'],
  ['legacy-error', 'error', 'failed'],
]) {
  EV('task_started', { task_id: taskId, name: taskId })
  EV('task_end', { task_id: taskId, status })
  const card = useAppStore.getState().views.s1.blocks.find((b) => (b.kind === 'async_task' || b.kind === 'agent') && b.taskId === taskId)
  assert.equal(card?.status, expected, `${status} 应归一化为 ${expected}`)
}
assert.equal(runningTaskCount(useAppStore.getState().views.s1.blocks), 1, 'interrupting 仍应计入运行任务')
EV('task_result_delivered', { task_id: 'new-interrupted', status: 'interrupted', result: 'partial' })
const interruptedNotice = useAppStore.getState().views.s1.blocks.find((b) => b.kind === 'task_delivery' && b.taskId === 'new-interrupted')
assert.equal(interruptedNotice?.status, 'interrupted', '已中断交付通知必须保留 interrupted 终态')
console.log('PASS 场景9：异步任务六态与旧状态兼容，interrupted 状态准确')

// ===== 场景 10：promoted 工具任务 tool_run_end 即收敛（交付回执丢失不再卡「执行中」）=====
// 回归（2026-08-25）：tool_run_end 此前只置 status='done' 不收敛 taskStatus → 卡片读
// taskStatus 永远「执行中」计时器空转，角标 runningTaskCount 读 status 却已归零 ——
// 列表与徽标互相矛盾。tool_run_end 是工具运行结束的权威信号（交付回执在会话停止/
// bridge 重启时不保证可达），必须在到达时收敛任务态；task_end / task_result_delivered
// 只做权威终态的覆盖修正。
setup()
// 任务 A：正常完成，但 task_end / task_result_delivered 均未达（回执丢失）
EV('tool_start', { id: 'tw-1', name: 'bash', arguments: '{"command":"sleep 60"}', task_id: 'tooltask-1', run_id: 'r1' })
EV('task_promoted', { task_id: 'tooltask-1', kind: 'tool', name: 'bash' })
let v10 = useAppStore.getState().views.s1
assert.equal(runningTaskCount(v10.blocks), 1, 'promote 后徽章应计 1')
EV('tool_response', { id: 'tw-1', name: 'bash', result: 'ok', run_id: 'r1' })
v10 = useAppStore.getState().views.s1
const doneTool = v10.blocks.find((b) => b.kind === 'tool' && b.toolId === 'tw-1')
assert.equal(doneTool?.status, 'done', 'tool_run_end 应结束工具行')
assert.equal(doneTool?.taskStatus, 'completed', 'tool_run_end 应收敛任务态为 completed（回执丢失不卡执行中）')
assert.notEqual(doneTool?.interrupting, true, '正常完成不得残留中断标记')
assert.equal(runningTaskCount(v10.blocks), 0, 'tool_run_end 后徽章应归零（与卡片状态一致）')
// 任务 B：用户点停止 → 取消回执（tool_run_end）先到、交付回执丢失 → 应收敛 interrupted
EV('tool_start', { id: 'tw-2', name: 'bash', arguments: '{"command":"sleep 600"}', task_id: 'tooltask-2', run_id: 'r1' })
EV('task_promoted', { task_id: 'tooltask-2', kind: 'tool', name: 'bash' })
useAppStore.getState().interruptTask('tooltask-2')
v10 = useAppStore.getState().views.s1
const stopping = v10.blocks.find((b) => b.kind === 'tool' && b.toolId === 'tw-2')
assert.equal(stopping?.taskStatus, 'interrupting', '点停止后应乐观进入中断中')
assert.equal(runningTaskCount(v10.blocks), 1, '中断中仍应计入运行（尚未确认停止）')
EV('tool_response', { id: 'tw-2', name: 'bash', result: '[exit: context canceled]', run_id: 'r1' })
v10 = useAppStore.getState().views.s1
const stopped = v10.blocks.find((b) => b.kind === 'tool' && b.toolId === 'tw-2')
assert.equal(stopped?.status, 'done', '取消回执应结束工具行')
assert.equal(stopped?.taskStatus, 'interrupted', '取消回执应收敛为已中断（计时器停走）')
assert.notEqual(stopped?.interrupting, true, '收敛后应清除中断标记')
assert.equal(runningTaskCount(v10.blocks), 0, '确认中断后徽章应归零')
// 交付回执迟到：以权威终态覆盖（停止竞态下任务实际跑完 → completed）
EV('task_result_delivered', { task_id: 'tooltask-2', status: 'completed', result: 'late delivery' })
v10 = useAppStore.getState().views.s1
assert.equal(v10.blocks.find((b) => b.kind === 'tool' && b.toolId === 'tw-2')?.taskStatus, 'completed', '迟到回执应覆盖为权威终态')
console.log('PASS 场景10：promoted 工具 tool_run_end 即收敛；徽标与卡片一致，迟到回执可覆盖')

// ===== 场景 11：todo 工具块定格「调用完成时刻」的 todos 快照 =====
// 历史计划卡必须显示当时状态（bridge 随 tool_response 携带 todos），不能全部跟随
// store 最新列表 —— 否则多次更新时每张卡都显示最终态（#t5 的卡也显示 6/6）。
setup()
EV('tool_start', { id: 'tw-t1', name: 'todo_add', arguments: '{"title":"A"}', run_id: 'r1' })
EV('tool_response', { id: 'tw-t1', name: 'todo_add', result: 'ok', run_id: 'r1', todos: [{ id: 't1', title: 'A', done: false }] })
let v11 = useAppStore.getState().views.s1
const addBlock = v11.blocks.find((b) => b.kind === 'tool' && b.toolId === 'tw-t1')
assert.equal(addBlock?.todos?.length, 1, 'todo_add 响应应定格当刻 todos 快照')
EV('tool_start', { id: 'tw-t2', name: 'todo_update', arguments: '{"id":"t1","done":true}', run_id: 'r1' })
EV('tool_response', { id: 'tw-t2', name: 'todo_update', result: 'ok', run_id: 'r1', todos: [{ id: 't1', title: 'A', done: true }] })
v11 = useAppStore.getState().views.s1
const updBlock = v11.blocks.find((b) => b.kind === 'tool' && b.toolId === 'tw-t2')
assert.equal(updBlock?.todos?.[0]?.done, true, 'todo_update 响应应定格当刻 todos 快照')
assert.equal(addBlock?.todos?.[0]?.done, false, '前一张卡的快照不得被后续调用改写（历史态保持）')
// 非 todo 工具不携带 todos → 不落字段（零额外内存）
EV('tool_start', { id: 'tw-b1', name: 'bash', arguments: '{"command":"ls"}', run_id: 'r1' })
EV('tool_response', { id: 'tw-b1', name: 'bash', result: 'x', run_id: 'r1' })
v11 = useAppStore.getState().views.s1
const bashBlock = v11.blocks.find((b) => b.kind === 'tool' && b.toolId === 'tw-b1')
assert.equal(bashBlock?.todos, undefined, '非 todo 工具不应落 todos 字段')
console.log('PASS 场景11：todo 工具块定格调用时刻快照；历史卡不被后续调用改写')


// ===== 场景 12：AgentEnd 之后才完成的后台任务 —— 回执归属所属轮 + 输入不被后台任务扣住 =====
// ① 回执落位：轮边界是 user 块（见 components/runtime/turn/selectTurns）。回执若按「到达
//    时刻」追加到 blocks 末尾，用户已经发了下一条消息时就会算进**下一轮** —— 时间线与
//    Working 盒同时错位（用户 2026-10 报障的形态：后台任务完成的通知跑到新一轮里）。
setup()
EV('user_inputs_consumed', { user_texts: ['开个后台任务'] })
EV('task_started', { task_id: 'tk1', name: 'bg' })
EV('user_inputs_consumed', { user_texts: ['下一条'] })
EV('task_result_delivered', { task_id: 'tk1', result: 'done' })
const v12 = useAppStore.getState().views.s1
const cardIdx = v12.blocks.findIndex((b) => b.kind === 'async_task' && b.taskId === 'tk1')
const didx = v12.blocks.findIndex((b) => b.kind === 'task_delivery' && b.taskId === 'tk1')
const u2 = v12.blocks.findIndex((b) => b.kind === 'user' && b.text === '下一条')
assert.ok(cardIdx >= 0 && didx > cardIdx, '回执应在任务卡片之后')
assert.ok(u2 > 0 && didx > 0 && didx < u2, '回执必须落在所属轮（下一条 user 之前），不得算进下一轮')

// ①b 运行中到达的回执：不是「时间线上的记录」，而是这次运行的一行过程 —— 落在**到达
//     位置**（后面还有工作行就排在它前面），不得被挪到轮尾，也不得打上 afterRun。
setup()
EV('user_inputs_consumed', { user_texts: ['跑一下'] })
EV('agent_start', { run_id: 'r-in-run' })
EV('task_started', { task_id: 'tk2', name: 'bg' })
EV('task_result_delivered', { task_id: 'tk2', result: 'done' })
EV('tool_start', { id: 'after-1', name: 'bash', arguments: '{"command":"ls"}', run_id: 'r-in-run' })
const v12b = useAppStore.getState().views.s1
const dIdx = v12b.blocks.findIndex((b) => b.kind === 'task_delivery' && b.taskId === 'tk2')
const afterIdx = v12b.blocks.findIndex((b) => b.kind === 'tool' && b.toolId === 'after-1')
const dBlock = v12b.blocks[dIdx]
assert.ok(dIdx > 0 && afterIdx > dIdx, '运行中到达的回执按到达位置落位（在后续工作行之前），不挪到轮尾')
assert.notEqual(dBlock?.afterRun, true, '运行中到达的回执不得打上 afterRun 位（否则渲染层会把它折到盒子外）')

// ② 发送闸门与 Composer 的按钮同源（mainRunActive，不含后台子 agent）：主 run 已交卷、
//    后台子 agent 仍在跑 → 新输入立即推送。旧判据 sessionBusy（含子 run）会让消息停在
//    「待发送」，等子 run 的下一个 llm_end 才代劳。
setup()
EV('agent_start', { run_id: 'r-main' })
EV('agent_start', { run_id: 'r-sub', parent_run_id: 'r-main' })
EV('agent_end', { run_id: 'r-main', finish_reason: 'stop' })
assert.ok(busy(), '子 run 仍在跑 → sessionBusy 仍为真（左栏运行点保留）')
useAppStore.getState().ask('主 run 结束后立刻发')
const q12 = pending().find((q) => text(q) === '主 run 结束后立刻发')
assert.ok(q12 && q12.pushed, '主 run 已交卷时，新输入必须立即推送（不等后台子 agent）')
console.log('PASS 场景12：迟到回执归属所属轮；主 run 交卷后输入不再被后台任务扣住')


// ===== 场景 14：一行信号的归属 —— 按 session 隔离，按子 agent 隔离 =====
// ① 子 run 的提示归卡片：hook 警告 / 静默收尾 / 目标提醒是**那次运行**的事实。挂到主转录
//    就是「主 run 说了这句」（用户 2026-10 报障：subagent 的提示混进主会话）。
setup()
EV('agent_start', { run_id: 'r-main' })
EV('agent_start', { run_id: 'r-sub', parent_run_id: 'r-main', name: 'sub' })
dispatchEvent({ event_type: 'tool_hook_warning', run_id: 'r-sub', name: 'PreToolUse', message: 'blocked by policy' })
let v14 = useAppStore.getState().views.s1
assert.equal(
  v14.blocks.filter((b) => b.kind === 'signal').length,
  0,
  '子 run 的信号不得进主转录',
)
const subCard = v14.blocks.find((b) => b.kind === 'agent' && b.runId === 'r-sub')
assert.ok(subCard, '子 agent 卡片应存在')
assert.ok(
  subCard.items.some((it) => it.kind === 'signal' && it.fact.etype === 'tool_hook_warning'),
  '子 run 的信号应挂在该子 agent 卡片的 items 上',
)
// 同一事实重复投递不刷两条
dispatchEvent({ event_type: 'tool_hook_warning', run_id: 'r-sub', name: 'PreToolUse', message: 'blocked by policy' })
v14 = useAppStore.getState().views.s1
assert.equal(
  v14.blocks.find((b) => b.kind === 'agent' && b.runId === 'r-sub').items.filter((it) => it.kind === 'signal').length,
  1,
  '同一子 run 的同类信号只留一条',
)

// ② 事件不带 session_id 时，按 run_id 归属会话 —— 不落到「当前打开的那个会话」
setup()
EV('agent_start', { run_id: 'r-s1' }) // s1 自己的 run
dispatchEvent({ event_type: 'agent_start', session_id: 's2', run_id: 'r-s2' }) // 另一个会话在跑
dispatchEvent({ event_type: 'goal_alignment_reminder', run_id: 'r-s2', round: 3 }) // 只带 run_id
const views = useAppStore.getState().views
assert.equal(
  (views.s1?.blocks ?? []).filter((b) => b.kind === 'signal').length,
  0,
  '别会话的 run 信号不得落到当前会话（跨会话串台）',
)
assert.ok(
  (views.s2?.blocks ?? []).some((b) => b.kind === 'signal' && b.fact.etype === 'goal_alignment_reminder'),
  '信号应落到拥有该 run 的会话',
)
// 找不到归属才退回 active（宁可在当前会话里留一条，也不要静默丢）
dispatchEvent({ event_type: 'goal_alignment_reminder', run_id: 'r-unknown', round: 4 })
assert.ok(
  useAppStore.getState().views.s1.blocks.some((b) => b.kind === 'signal' && b.fact.round === 4),
  'run 找不到归属时退回当前会话（不静默丢弃）',
)
// ③ 任务级提示分两半（用户 2026-11 的口径，修正 409a99c 的一半）：
//    · **摘离**归它自己的会话：子 agent 在自己的会话里把工具摘成后台任务 → 那行挂在
//      **它的卡片**上。主转录里根本没有那次工具调用，凭空一行「已摘为后台任务」读起来
//      像从天上掉下来的（用户 2026-11 截图报障）。
//    · **完成**推回主会话：结果是会话级的（卡片在主转录、结果回填主会话 inbox），
//      模型在主 run 的下一轮读到它，signal 组件也显示在主会话。
setup()
EV('agent_start', { run_id: 'r-main' })
EV('agent_start', { run_id: 'r-sub2', parent_run_id: 'r-main', name: 'sub' })
EV('tool_start', { id: 'tt1', name: 'bash', arguments: '{"command":"sleep 60"}', run_id: 'r-sub2', task_id: 'tooltask-sub' })
EV('task_promoted', { task_id: 'tooltask-sub', name: 'bash' })
let v14c = useAppStore.getState().views.s1
const subCard2 = v14c.blocks.find((b) => b.kind === 'agent' && b.runId === 'r-sub2')
assert.ok(
  subCard2.items.some((it) => it.kind === 'signal' && it.fact.etype === 'task_promoted'),
  '子 agent 摘离的任务：摘离信号要挂回**它的**会话（卡片项）',
)
assert.equal(
  subCard2.items.find((it) => it.kind === 'tool' && it.taskId === 'tooltask-sub')?.promoted,
  true,
  '卡片里的工具行也要切后台态（子会话里那行不能再写「运行中」）',
)
assert.equal(
  v14c.blocks.filter((b) => b.kind === 'signal' && b.fact.etype === 'task_promoted').length,
  0,
  '子 agent 摘离的任务：摘离提示不得画到主转录（主转录里没有那次调用）',
)
// 主 run 自己的工具摘离 → 仍然落在主转录（工具行就在那里）
EV('tool_start', { id: 'tt2', name: 'bash', arguments: '{"command":"sleep 90"}', run_id: 'r-main', task_id: 'tooltask-main' })
EV('task_promoted', { task_id: 'tooltask-main', name: 'bash' })
v14c = useAppStore.getState().views.s1
assert.ok(
  v14c.blocks.some((b) => b.kind === 'signal' && b.fact.etype === 'task_promoted' && b.fact.message === 'tooltask-main'),
  '主 run 自己的工具摘离：一行信号落在主会话',
)
// 完成通知必须在主会话（子 agent 摘离的任务也不例外）
EV('task_result_delivered', { task_id: 'tooltask-sub', name: 'bash', result: 'done' })
v14c = useAppStore.getState().views.s1
assert.ok(
  v14c.blocks.some((b) => b.kind === 'task_delivery' && b.taskId === 'tooltask-sub'),
  '任务完成的通知必须通知到主会话（不得只留在子 agent 里）',
)
// 终态也要收回**它自己的会话**：子会话里那行不能永远写着「运行中」
const subTool = v14c.blocks
  .find((b) => b.kind === 'agent' && b.runId === 'r-sub2')
  .items.find((it) => it.kind === 'tool' && it.taskId === 'tooltask-sub')
assert.equal(subTool?.status, 'done', '完成回执要把卡片里的工具项收敛成 done')
assert.equal(subTool?.result, 'done', '结果写回那一项（子会话里读得到）')
assert.equal(
  v14c.blocks.filter((b) => b.kind === 'task_result' && b.taskId === 'tooltask-sub').length,
  0,
  '结果已在它自己的会话里，主会话不再多一张完整结果卡（只留一行 signal）',
)

console.log('PASS 场景14：信号按 session / 子 agent 隔离')

// ===== 场景 15：本轮产出（artifacts）按**运行**归属，子运行的产出不进主转录 =====
// 用户 2026-10 报障：主转录里冒出「本轮产出 4 个文件」，路径全在别的 worktree —— 那是
// 一个**子运行**的产出。判据以前只看视图里有没有它的登记（agent 卡片 / runNode），而
// runNodes 在每次新的顶层 run 开始时被重置，晚收尾的子运行于是被判成「主运行」。
// 现在先看事件自己的 ParentRunId。
const art = (p) => ({ name: p.split('/').pop(), path: p, kind: 'modified', added: 3, removed: 1 })
setup()
EV('agent_start', { run_id: 'r-main' })
EV('task_started', { task_id: 'task-bg', run_id: 'r-main', name: 'bg' }) // 后台任务卡片，无 runId 登记
EV('agent_start', { run_id: 'r-main-2' }) // 新顶层 run：runNodes 被重置，旧登记消失
EV('agent_end', {
  run_id: 'r-bg',
  parent_run_id: 'r-main',
  finish_reason: 'stop',
  artifacts: [art('.go-code/worktrees/ent-c345/internal/co/permission_reader.go')],
})
let v15 = useAppStore.getState().views.s1
assert.equal(
  v15.blocks.filter((b) => b.kind === 'artifacts').length,
  0,
  '子运行的产出不得作为「本轮产出」追加到主转录',
)
// 主运行自己收尾照常落块（别把正常路径一起挡掉）
EV('agent_end', {
  run_id: 'r-main-2',
  finish_reason: 'stop',
  artifacts: [art('desktop/app/src/components/App.tsx')],
})
v15 = useAppStore.getState().views.s1
const mainArts = v15.blocks.filter((b) => b.kind === 'artifacts')
assert.equal(mainArts.length, 1, '主运行的产出仍然要落「本轮产出」块')
assert.ok(mainArts[0].items.some((a) => a.path === 'desktop/app/src/components/App.tsx'), '块里是主运行自己的文件')
console.log('PASS 场景15：产出按运行归属（父运行 id 优先，子运行不进主转录）')

// ===== 场景 16：记录按**事件时刻**落位（任务完成的通知必须先于新的 Working）=====
// 用户 2026-10 报障：后台任务完成的通知被排到了「新的 AgentStart 的 Working」最底下。
// 成因：一轮的结束不等于下一条 user 消息 —— 任务结果推回会话后主 run 会自动续跑，
// 同一个 user 轮里再起一次 AgentStart；那一刻「轮尾」已经在新的 Working 之后。
const base = Date.now() - 60_000
const at = (ms) => new Date(base + ms).toISOString()
setup()
EV('agent_start', { run_id: 'r-first', timestamp: at(0) })
EV('task_started', { task_id: 'tk-order', run_id: 'r-first', name: 'bg', timestamp: at(10) })
EV('agent_end', { run_id: 'r-first', finish_reason: 'stop', timestamp: at(100) })
// 新的一次运行（同一个 user 轮里续跑），先来了它自己的行
EV('agent_start', { run_id: 'r-second', timestamp: at(200) })
EV('tool_start', { id: 'order-t1', name: 'bash', arguments: '{"command":"x"}', run_id: 'r-second', timestamp: at(300) })
// 任务完成事件**此刻才被处理**，但它的时刻（150）早于 r-second 的开始（200）
EV('task_result_delivered', { task_id: 'tk-order', result: 'done', timestamp: at(150) })
let v16 = useAppStore.getState().views.s1.blocks
const orderIdx = v16.findIndex((b) => b.kind === 'task_delivery' && b.taskId === 'tk-order')
// 工具块记的是 parentRunId（主 run 也走这条），assistant 块记 runId —— 两种都算
const newRunIdx = v16.findIndex((b) => b.runId === 'r-second' || b.parentRunId === 'r-second')
assert.ok(orderIdx >= 0, '回执必须存在')
assert.ok(newRunIdx >= 0, '新 run 的块必须存在')
assert.ok(orderIdx < newRunIdx, `任务完成通知必须排在新 Working 之前（delivery@${orderIdx}, run@${newRunIdx}）`)

// 同样一条规则适用于**所有信号**：事件时刻早于当前 run 的信号也插到那次 run 之前
EV('tool_hook_warning', { name: 'PreToolUse', message: 'late hook', run_id: 'r-first', timestamp: at(120) })
v16 = useAppStore.getState().views.s1.blocks
const sigIdx = v16.findIndex((b) => b.kind === 'signal' && b.fact.etype === 'tool_hook_warning')
assert.ok(sigIdx >= 0, '信号必须存在')
const newRunIdx2 = v16.findIndex((b) => b.runId === 'r-second' || b.parentRunId === 'r-second')
assert.ok(sigIdx < newRunIdx2, '信号同样按事件时刻落位，不得积在最底部')
// 产出块与任务卡片同一判据：迟到的事件不得把「本轮产出」排到下一次运行之后
setup()
EV('agent_start', { run_id: 'r-a', timestamp: at(0) })
EV('agent_start', { run_id: 'r-b', timestamp: at(100) })
EV('tool_start', { id: 'a-1', name: 'bash', arguments: '{}', run_id: 'r-b', timestamp: at(200) })
EV('agent_end', { run_id: 'r-a', finish_reason: 'stop', timestamp: at(50), artifacts: [{ name: 'x.go', path: 'x.go', kind: 'modified', added: 1, removed: 0 }] })
const v16b = useAppStore.getState().views.s1.blocks
const artIdx = v16b.findIndex((b) => b.kind === 'artifacts')
const runBIdx = v16b.findIndex((b) => b.runId === 'r-b' || b.parentRunId === 'r-b')
assert.ok(artIdx >= 0 && runBIdx >= 0, '产出块与 run-b 的块都要在')
assert.ok(artIdx < runBIdx, `本轮产出必须排在 r-b 的块之前（art@${artIdx}, runB@${runBIdx}）`)
/* 另：延迟到达的 task_started 也要落在发起它的那次运行那一侧，而不是新的 Working 之后 */
setup()
EV('agent_start', { run_id: 'r-c', timestamp: at(0) })
EV('agent_start', { run_id: 'r-d', timestamp: at(100) })
EV('tool_start', { id: 'd-1', name: 'bash', arguments: '{}', run_id: 'r-d', timestamp: at(200) })
EV('task_started', { task_id: 'tk-late-start', run_id: 'r-c', name: 'bg', timestamp: at(50) })
const v16c = useAppStore.getState().views.s1.blocks
const lateCardIdx = v16c.findIndex((b) => b.kind === 'async_task' && b.taskId === 'tk-late-start')
const runDIdx = v16c.findIndex((b) => b.runId === 'r-d' || b.parentRunId === 'r-d')
assert.ok(lateCardIdx >= 0 && runDIdx >= 0 && lateCardIdx < runDIdx, `任务卡片必须排在 r-d 之前（card@${lateCardIdx}, runD@${runDIdx}）`)
/* 错误块同样按事件时刻落位（用户明确要求：error 不能例外） */
setup()
EV('agent_start', { run_id: 'r-e1', timestamp: at(0) })
EV('agent_start', { run_id: 'r-e2', timestamp: at(100) })
EV('tool_start', { id: 'e-1', name: 'bash', arguments: '{}', run_id: 'r-e2', timestamp: at(200) })
EV('llm_error', {
  run_id: 'r-e1',
  request_id: 'q-e1',
  model: 'm1',
  error_kind: 'rate_limit',
  message: 'boom',
  will_retry: false,
  timestamp: at(50),
})
const v16d = useAppStore.getState().views.s1.blocks
const errIdx = v16d.findIndex((b) => b.kind === 'error')
const runE2Idx = v16d.findIndex((b) => b.runId === 'r-e2' || b.parentRunId === 'r-e2')
assert.ok(errIdx >= 0, '错误块必须存在')
assert.ok(errIdx < runE2Idx, `错误块必须排在 r-e2 之前（err@${errIdx}, runE2@${runE2Idx}）`)

console.log('PASS 场景16：记录按事件时刻落位（任务回执/信号都先于新 run 的 Working）')

// ===== 场景 13：失败 attempt 的记账与轮次收尾（会重试的**不算**一轮）=====
// 用户 2026-10 口径：中断恰好发生在 llm_start ↔ llm_end 之间时，这一轮永远没有 llm_end
// —— 不在这里收尾，轮尾账（按 turns 求和）与回合数就少一轮。但自动重试的中间失败
// 不是轮尾（同一个逻辑轮会换 request id 重来），只记账、不落 Turn。
setup()
EV('llm_start', { run_id: 'r-abort', request_id: 'q1', model: 'm1' })
const a1 = { Input: 100, Output: 10, TotalTokens: 110, Cost: { Input: 1, Output: 1, Total: 2, Priced: true } }
EV('llm_error', { run_id: 'r-abort', request_id: 'q1', model: 'm1', attempt: 1, max_attempts: 3, will_retry: true, error_kind: 'rate_limit', message: 'boom', usage: a1, cost_usd: 2e-6 })
let v13 = useAppStore.getState().views.s1
assert.equal(v13.turns.length, 0, 'will_retry=true 的中间失败不得落轮次')
assert.equal(v13.usage.input, 100, '失败 attempt 的 tokens 必须进会话累计（此前只记成本）')
assert.ok(v13.turnStartTs['r-abort'] != null, '中间失败不得清轮起点')
// 第二次尝试（新 request id）→ 最终失败（aborted）：轮次收尾
EV('llm_start', { run_id: 'r-abort', request_id: 'q2', model: 'm1' })
const a2 = { Input: 120, Output: 12, TotalTokens: 132, Cost: { Input: 2, Output: 1, Total: 3, Priced: true } }
EV('llm_error', { run_id: 'r-abort', request_id: 'q2', model: 'm1', attempt: 2, max_attempts: 3, will_retry: false, error_kind: 'aborted', message: 'aborted', usage: a2, cost_usd: 3e-6 })
v13 = useAppStore.getState().views.s1
assert.equal(v13.turns.length, 1, '最终失败应补一条轮次（被中断的那轮不再缺席）')
assert.equal(v13.turns[0].runId, 'r-abort')
assert.equal(v13.turns[0].input, 120, '轮次用量只含最终失败那次 attempt')
assert.ok(v13.turns[0].durMs != null && v13.turns[0].durMs >= 0, '轮次应带时长')
assert.equal(v13.usage.input, 220, '两次 attempt 的 tokens 都进累计')
assert.equal(v13.turnStartTs['r-abort'], undefined, '收尾后不得残留轮起点')
EV('agent_end', { run_id: 'r-abort', finish_reason: 'abort', usage: { Input: 220, Output: 22 } })
v13 = useAppStore.getState().views.s1
assert.equal(v13.turns.length, 1, 'AgentEnd 不得重复补轮（轮起点已清）')

// 退避等待中被 abort：只有 agent_end → 兜底一条「只带时长」的轮次
setup()
EV('llm_start', { run_id: 'r-orphan', request_id: 'q9', model: 'm1' })
EV('llm_error', { run_id: 'r-orphan', request_id: 'q9', model: 'm1', attempt: 1, max_attempts: 3, will_retry: true, error_kind: 'rate_limit', message: 'boom' })
EV('agent_end', { run_id: 'r-orphan', finish_reason: 'abort' })
v13 = useAppStore.getState().views.s1
assert.equal(v13.turns.length, 1, 'AgentEnd 应兜底收尾本轮')
assert.ok((v13.turns[0].durMs ?? -1) >= 0 && v13.turns[0].input === 0, '兜底只记时长，token 不编')
console.log('PASS 场景13：失败 attempt 记账；重试不算轮尾，最终失败/AgentEnd 才收尾')

// ===== 场景 17：运行中插话（injected）留在同一个 run 里 =====
// 服务端两条消费路径：Session.Run 的 takeBatch（新运行开口）与 AgentLoop 的插话 Poll
// （运行中插进来的一条）。事件带 injected 位 —— 前者没有，后者有；转录据此不另起一轮
// （否则同一个 run 画出两个 Working 盒子，用户 2026-11 报障）。
setup()
EV('agent_start', { run_id: 'r-mid' })
EV('user_inputs_consumed', { user_texts: ['第一句'] }) // 新运行的开头（无 injected）
EV('user_inputs_consumed', { user_texts: ['跑的时候补一句'], injected: true })
const v17 = useAppStore.getState().views.s1
const users17 = v17.blocks.filter((b) => b.kind === 'user')
assert.equal(users17.length, 2)
assert.notEqual(users17[0].injected, true, '新运行的开口不是插话')
assert.equal(users17[1].injected, true, '插话 Poll 消费的消息带 injected 位（渲染层据此不切轮）')

// ===== 场景 18：上下文压缩完成 → 对应会话的一行 signal =====
// 用户 2026-11：压缩结束后，对应的 session 没有「压缩完成」的一行通知。
// 归属与其它信号同一条规则：子 run 的压缩挂在它的卡片上，主 run 的落主会话。
setup()
EV('agent_start', { run_id: 'r-main' })
EV('compress_start', { run_id: 'r-main', before: 40 })
EV('compress_end', { run_id: 'r-main', before: 40, after: 12, ctx_tokens: 2000 })
const v18 = useAppStore.getState().views.s1
assert.ok(
  v18.blocks.some((b) => b.kind === 'compression' && b.runId === 'r-main'),
  '压缩块照旧落库',
)
const compact18 = v18.blocks.find((b) => b.kind === 'signal' && b.fact.etype === 'compress_end')
assert.ok(compact18, '主 run 压缩完成要有一行 signal（落在主会话）')
assert.equal(compact18.fact.sig, 'compact')
assert.equal(compact18.fact.before, 40)
assert.equal(compact18.fact.after, 12)

// 子 run 的压缩：挂回子 agent 的卡片，不进主转录
EV('agent_start', { run_id: 'r-sub18', parent_run_id: 'r-main', name: 'sub' })
EV('compress_start', { run_id: 'r-sub18', before: 30 })
EV('compress_end', { run_id: 'r-sub18', before: 30, after: 9 })
const v18b = useAppStore.getState().views.s1
const card18 = v18b.blocks.find((b) => b.kind === 'agent' && b.runId === 'r-sub18')
assert.ok(
  card18.items.some((it) => it.kind === 'signal' && it.fact.etype === 'compress_end'),
  '子 agent 压缩完成的一行要挂在它的卡片上（它的会话）',
)
assert.equal(
  v18b.blocks.filter((b) => b.kind === 'signal' && b.fact.etype === 'compress_end').length,
  1,
  '子 run 的压缩完成不得多出一条到主转录',
)
// 失败/中止的压缩没有「完成」可报：不写 signal（原因已在压缩块上）
EV('agent_start', { run_id: 'r-bad', parent_run_id: 'r-main', name: 'sub2' })
EV('compress_start', { run_id: 'r-bad', before: 20 })
EV('compress_end', { run_id: 'r-bad', before: 20, after: 20, error: 'upstream failed' })
const v18c = useAppStore.getState().views.s1
const badCard = v18c.blocks.find((b) => b.kind === 'agent' && b.runId === 'r-bad')
assert.equal(
  badCard.items.filter((it) => it.kind === 'signal' && it.fact.etype === 'compress_end').length,
  0,
  '失败的压缩不写「压缩完成」信号',
)
console.log('PASS 场景17-18：插话带 injected 位；压缩完成的一行 signal 归对应会话')

// ===== 场景 19：记录落在**这一轮内部**，不得插到用户消息上方 =====
// 用户 2026-11 报障（截图里一条光秃秃的信号行 + 下面才是正文）：只有一轮时，
// 「下一条 user 之前」被实现成从数组开头找 user —— 第一个 user 立刻命中，于是运行中
// 到达的信号/回执全都插到了用户消息**上方**，一轮被切成两段（第一段没有 user）。
setup()
const b19t = Date.now() - 5000
const at19 = (ms) => new Date(b19t + ms).toISOString()
EV('agent_start', { run_id: 'r19', timestamp: at19(0) })
EV('user_inputs_consumed', { user_texts: ['这一轮的问题'], timestamp: at19(10) })
EV('tool_start', { id: 'x19', name: 'bash', arguments: '{"command":"sleep 1"}', run_id: 'r19', timestamp: at19(20) })
EV('tool_hook_warning', { run_id: 'r19', name: 'PreToolUse', message: 'warn', timestamp: at19(30) })
EV('task_result_delivered', { task_id: 'tk19', result: 'done', timestamp: at19(40) })
const b19 = useAppStore.getState().views.s1.blocks
const u19 = b19.findIndex((b) => b.kind === 'user')
const s19 = b19.findIndex((b) => b.kind === 'signal' && b.fact.etype === 'tool_hook_warning')
const d19 = b19.findIndex((b) => b.kind === 'task_delivery' && b.taskId === 'tk19')
assert.ok(u19 >= 0 && s19 >= 0 && d19 >= 0, '用户消息 / 信号 / 回执都要在')
assert.ok(s19 > u19, `运行中到达的信号必须在这一轮内部（信号@${s19}, user@${u19}）`)
assert.ok(d19 > u19, `运行中到达的回执必须在这一轮内部（回执@${d19}, user@${u19}）`)

// 迟到的事件仍按事件时刻落位，但**下界是这一轮的开头**（不能跑到上一轮/用户上方）
setup()
EV('agent_start', { run_id: 'r19a', timestamp: at19(1000) })
EV('user_inputs_consumed', { user_texts: ['第二轮的问题'], timestamp: at19(1100) })
EV('agent_start', { run_id: 'r19b', timestamp: at19(1200) })
EV('tool_start', { id: 'x19b', name: 'bash', arguments: '{}', run_id: 'r19b', timestamp: at19(1300) })
// 事件时刻早于 r19b 的开始：插到 r19b 第一块之前，但仍在 user 之后
EV('task_result_delivered', { task_id: 'tk19b', result: 'done', timestamp: at19(1150) })
const b19b = useAppStore.getState().views.s1.blocks
const u19b = b19b.findIndex((b) => b.kind === 'user')
const d19b = b19b.findIndex((b) => b.kind === 'task_delivery' && b.taskId === 'tk19b')
const run19b = b19b.findIndex((b) => b.runId === 'r19b' || b.parentRunId === 'r19b')
assert.ok(d19b > u19b, `迟到的回执也不能插到用户消息上方（回执@${d19b}, user@${u19b}）`)
assert.ok(run19b < 0 || d19b < run19b, `迟到的回执仍要排在新 run 的块之前（回执@${d19b}, run@${run19b}）`)
console.log('PASS 场景19：记录落在这一轮内部（不再插到用户消息上方）')

// ===== 场景 20：暖恢复（刷新，进程内 run 还在跑）不许把这次运行判成交卷 =====
// 用户 2026-11 报障：上下文压缩之后刷新页面，Working 变成 Worked for，压缩完成那一行
// 也没有收缩到 Working 盒子里面（落在盒外的时间线上）。两个破坏点：
//   a) checkpoint 恢复**无条件**归一化：running 的 run 节点被改成 done、runEnded 恒 true
//      → afterRunNow() 恒真 → 之后到达的每条记录都被判成「交卷后到达」；
//   b) 运行中到达的记录只在 true 时写位 → 位置兜底（正文之后 = 交卷后）兜回来又把它甩出去。
setup()
const cp20 = {
  schema_version: 1,
  blocks: [
    { kind: 'user', id: 1, text: '这一轮的问题', ts: 1000 },
    { kind: 'assistant', id: 2, text: '写到一半的正文', run_id: 'r20', ts: 2000 },
    { kind: 'tool', id: 3, name: 'bash', args: '{}', status: 'running', run_id: 'r20', ts: 2100 },
  ],
  run_nodes: [{ id: 'r20', label: 'run', status: 'running', depth: 0, kind: 'run', started_at: 1000 }],
  turns: [],
}
hydrateCheckpoint('s1', cp20, undefined, { live: true })
let v20 = useAppStore.getState().views.s1
assert.strictEqual(v20.runEnded, false, '暖恢复：runEnded 必须保持 false（AgentEnd 还没来）')
assert.strictEqual(v20.runNodes[0]?.status, 'running', '暖恢复：在跑的 run 节点不许收敛成 done')
assert.strictEqual(v20.blocks.find((b) => b.kind === 'tool')?.status, 'running', '暖恢复：在跑的工具不许改成 done')
EV('tool_hook_warning', { run_id: 'r20', name: 'PreToolUse', message: 'warn' })
EV('task_result_delivered', { task_id: 'tk20', result: 'done' })
v20 = useAppStore.getState().views.s1
assert.strictEqual(v20.blocks.find((b) => b.kind === 'signal')?.afterRun, false,
  '运行中到达的信号必须盖 afterRun:false（否则它被甩出 Working 盒子）')
assert.strictEqual(v20.blocks.find((b) => b.kind === 'task_delivery')?.afterRun, false,
  '运行中到达的回执必须盖 afterRun:false（否则它被甩出 Working 盒子）')

// 冷恢复（进程重启/磁盘）：残留 running 就是幽灵 —— 必须收敛，记录按位置落到盒外
setup()
hydrateCheckpoint('s1', cp20)
v20 = useAppStore.getState().views.s1
assert.strictEqual(v20.runEnded, true, '冷恢复：结算位必须是 true')
assert.strictEqual(v20.runNodes[0]?.status, 'done', '冷恢复：幽灵 run 收敛为 done')
assert.strictEqual(v20.blocks.find((b) => b.kind === 'tool')?.status, 'done', '冷恢复：幽灵工具收敛为 done')
EV('tool_hook_warning', { run_id: 'r20', name: 'PreToolUse', message: 'warn' })
v20 = useAppStore.getState().views.s1
assert.strictEqual(v20.blocks.find((b) => b.kind === 'signal')?.afterRun, true, '交卷之后到达 = 时间线上的记录')
console.log('PASS 场景20：暖恢复保留运行态 / 冷恢复收敛；记录按到达时刻归属盒子')

// ===== 场景 21：待答的 ask_user 批次进 checkpoint（刷新后提问面板还能复原）=====
// 用户 2026-11：ask_user 是工具调用，理论上就该进 checkpoint —— 事实是它只活在前端的
// views[sid].questions 里，刷新一次面板就没了，而 run 还在等这个回答（等到超时才动）。
setup()
const cp21 = {
  schema_version: 1,
  blocks: [{ kind: 'user', id: 1, text: '问一句', ts: 1000 }],
  run_nodes: [{ id: 'r21', kind: 'run', status: 'running', label: 'run', depth: 0, started_at: 1000 }],
  turns: [],
  pending_questions: [{
    id: 'qb21', run_id: 'r21', timeout_ms: 60000, ts: Date.now(),
    items: [{ id: 'q21-1', question: '选哪个？', options: [{ label: 'A', preview: 'a' }, { label: 'B' }] }],
  }],
}
hydrateCheckpoint('s1', cp21, undefined, { live: true })
let v21 = useAppStore.getState().views.s1
assert.strictEqual(v21.questions.length, 1, '暖恢复：待答批次必须还在')
assert.strictEqual(v21.questions[0].id, 'qb21', '批次 id 是 answer_question 的锚')
assert.strictEqual(v21.questions[0].runId, 'r21')
assert.strictEqual(v21.questions[0].timeoutMs, 60000, '倒计时窗口要一起回来')
assert.strictEqual(v21.questions[0].items[0].options.length, 2, '选项（含 preview）要一起回来')
assert.strictEqual(v21.questions[0].items[0].options[0].preview, 'a')
// 回答/超时/中断都汇于 tool_run_end(ask_user) → 镜像同步清掉
EV('tool_run_end', { id: 'aq21', name: 'ask_user', run_id: 'r21', result: 'A', is_error: false })
assert.strictEqual(useAppStore.getState().views.s1.questions.length, 0, '答完面板与镜像一起消失')

// 冷恢复（进程死了，回答通道不存在）→ 不许画一个骗人的「等你回答」
setup()
hydrateCheckpoint('s1', cp21)
assert.strictEqual(useAppStore.getState().views.s1.questions.length, 0, '冷恢复不带待答批次')
console.log('PASS 场景21：ask_user 批次进 checkpoint；暖恢复复原、冷恢复丢弃')

// ===== 场景 22：产出卡的归属（AgentEnd + 那个 run + 这个 session）=====
// 用户 2026-11 问：产出卡与轮尾账是不是也按 AgentEnd / session 归属？
//   · 只有**主 run** 的 AgentEnd 落产物块（子运行的产出归它自己的卡片）；
//   · 一个 AgentEnd 一块（同 runId 重放不追加第二块）；
//   · 子运行的 AgentEnd 不得把文件写进主转录。
setup()
const at22 = (ms) => new Date(Date.now() - 20000 + ms).toISOString()
EV('agent_start', { run_id: 'm22', timestamp: at22(0) })
EV('user_inputs_consumed', { user_texts: ['主 run'], timestamp: at22(10) })
EV('llm_start', { run_id: 'm22', request_id: 'q22', timestamp: at22(20) })
EV('content_chunk', { run_id: 'm22', request_id: 'q22', content: '正文', timestamp: at22(30) })
EV('llm_end', { run_id: 'm22', request_id: 'q22', timestamp: at22(40), usage: { Input: 100, Output: 20 } })
// 子运行的 AgentEnd（带产出）：不得在主转录落产出块
EV('agent_start', { run_id: 's22sub', parent_run_id: 'm22', depth: 1, timestamp: at22(50) })
EV('agent_end', { run_id: 's22sub', parent_run_id: 'm22', finish_reason: 'stop', timestamp: at22(60),
  artifacts: [{ name: 'sub.md', path: 'w/sub.md', kind: 'created', added: 5 }] })
let b22 = useAppStore.getState().views.s1.blocks
assert.strictEqual(b22.filter((b) => b.kind === 'artifacts').length, 0, '子运行的产出不落主转录')
// 主 AgentEnd：一块，runId 是这个 run
EV('agent_end', { run_id: 'm22', finish_reason: 'stop', timestamp: at22(70),
  artifacts: [{ name: 'a.md', path: 'w/a.md', kind: 'created', added: 75 }] })
b22 = useAppStore.getState().views.s1.blocks
let arts22 = b22.filter((b) => b.kind === 'artifacts')
assert.strictEqual(arts22.length, 1, '主 run 的 AgentEnd 落一块产出')
assert.strictEqual(arts22[0].runId, 'm22', '产出块记住是哪个 run 的')
// 重连补发 / 重放同一条 AgentEnd：不追加第二块
EV('agent_end', { run_id: 'm22', finish_reason: 'stop', timestamp: at22(70),
  artifacts: [{ name: 'a.md', path: 'w/a.md', kind: 'created', added: 75 }] })
arts22 = useAppStore.getState().views.s1.blocks.filter((b) => b.kind === 'artifacts')
assert.strictEqual(arts22.length, 1, '同一 runId 的 agent_end 重放不得再落一块')
console.log('PASS 场景22：产出按 AgentEnd 的 run 与 session 归属，同 run 幂等')

// ===== 场景 23：跨会话/跨项目的「已摘为后台任务」不得落到当前会话 =====
// 用户 2026-11 报障：agent-server-g 的会话里出现了 cybertron-gogroup 那次的
// 「已摘为后台任务（bash · tooltask-42rsycmw）」。
// 根因（事件日志实证）：events.TaskPromoted 自带 `"session_id":""`（无 omitempty），
// bridge 又在首部注入了一份正确的 —— JSON 重复键**后者胜**，前端读到空串；而该事件既没有
// run_id 可反查，旧实现就回退成「当前打开的会话」，于是串台。
// 修法两层：bridge 先删事件自带的同名键再注入（见 jsonwire）；前端在没有归属时**丢弃**，
// 并且允许按 task_id 反查它到底在哪个视图里。
const EV2 = (sid, t, p = {}) => dispatchEvent({ event_type: t, session_id: sid, ...p })
const signalsIn = (sid) => (useAppStore.getState().views[sid]?.blocks ?? [])
  .filter((b) => b.kind === 'signal' && b.fact?.etype === 'task_promoted')

setup()
// 别的会话里有这个任务（工具块带 taskId），当前会话 s1 里没有
EV2('s2', 'tool_run_start', { id: 'x23', name: 'bash', arguments: '{}', run_id: 'r23', task_id: 'tk-other' })
// 事件既没有 session_id、也没有 run_id（正是事故里的形态）
EV('task_promoted', { session_id: '', task_id: 'tk-other', name: 'bash' })
const v23 = useAppStore.getState().views
assert.strictEqual(signalsIn('s1').length, 0, '当前会话（s1）不得收到别的会话的摘离信号')
const tool23 = (v23.s2?.blocks ?? []).find((b) => b.kind === 'tool' && b.taskId === 'tk-other')
assert.ok(tool23 && tool23.promoted === true, '信号要落回任务所在的会话（s2），并把工具行标成后台态')

// 连 task_id 都找不到归属（视图没加载/被翻页裁掉）→ 丢弃，不许猜「当前会话」
setup()
EV('task_promoted', { session_id: '', task_id: 'tk-nowhere', name: 'bash' })
assert.strictEqual(signalsIn('s1').length, 0, '归属不明的摘离信号必须丢弃，不得回退到当前会话')
assert.strictEqual(useAppStore.getState().views[''] == null, true, '不得写出 views[""] 幽灵视图')

// 正常路径不受影响：同一会话里的工具被摘离 → 一行信号 + 工具行后台态
setup()
EV('agent_start', { run_id: 'r23b' })
EV('tool_run_start', { id: 'x23b', name: 'bash', arguments: '{}', run_id: 'r23b', task_id: 'tk-own' })
EV('task_promoted', { session_id: 's1', task_id: 'tk-own', name: 'bash' })
const own = useAppStore.getState().views.s1.blocks
assert.strictEqual(signalsIn('s1').length, 1, '本会话的摘离仍要有一行信号')
assert.strictEqual(own.find((b) => b.kind === 'tool' && b.taskId === 'tk-own')?.promoted, true, '工具行切后台态')
console.log('PASS 场景23：摘离信号按 session/任务归属，归属不明则丢弃（不再串台）')

// ===== 场景 24：运行中到达的回执归**当前轮**，不按卡片所在轮 =====
// 用户 2026-11 截图：后台子 agent 的卡片在几轮之前，运行中到达的 `Task … completed`
// 按卡片落位 → 被插回旧轮（跟自己的工作行分家），而 Go reducer 按事件时刻把它放在
// 当前运行的中间。判据：运行中到达（afterRun=false）→ 当前轮、按事件时刻；
// 交卷后到达（afterRun=true）→ 才回卡片所在轮（避免算进下一轮）。
setup()
const b24 = Date.now() - 60_000
const at24 = (ms) => new Date(b24 + ms).toISOString()
EV('agent_start', { run_id: 'r24-a', timestamp: at24(0) })
EV('user_inputs_consumed', { user_texts: ['第一轮的问题'], timestamp: at24(10) })
EV('task_started', { task_id: 'tk24', run_id: 'r24-a', name: 'bg', timestamp: at24(20) })
EV('agent_end', { run_id: 'r24-a', finish_reason: 'stop', timestamp: at24(100) })
EV('user_inputs_consumed', { user_texts: ['第二轮的问题'], timestamp: at24(200) })
EV('agent_start', { run_id: 'r24-b', timestamp: at24(210) })
EV('tool_start', { id: 'x24', name: 'bash', arguments: '{"command":"x"}', run_id: 'r24-b', timestamp: at24(220) })
// 第二轮**运行中**：后台任务（卡片在第一轮）完成
EV('task_result_delivered', { task_id: 'tk24', status: 'completed', result: 'done', timestamp: at24(300) })
const blocks24 = useAppStore.getState().views.s1.blocks
const lastUser24 = blocks24.map((b) => b.kind).lastIndexOf('user')
const d24 = blocks24.findIndex((b) => b.kind === 'task_delivery' && b.taskId === 'tk24')
assert.ok(lastUser24 >= 0 && d24 >= 0, '第二轮的用户消息与回执都要在')
assert.ok(d24 > lastUser24, `运行中到达的回执必须落在**当前轮**内部（回执@${d24}, user@${lastUser24}）`)
const delivery24 = blocks24[d24]
assert.equal(delivery24.afterRun, false, '运行中到达的回执必须显式记 afterRun:false（恢复后仍能判）')
console.log('PASS 场景24：运行中到达的回执落在当前轮内部（不按卡片回旧轮），afterRun 显式盖章')

// ===== 场景 25–31（BR-15）：丢一条 agent_end 不得永久「运行中」 =====
//
// 运行位的结束证据本应只有 agent_end。事件会丢，所以当**别的终止事实**到达时，残留的
// running 必须收敛；但绝不能按时间/超时猜（那会把真在跑判成结束），也不能顺手杀掉
// 摘离的异步任务（它们的终态证据是 task_end）。
const viewOf = () => useAppStore.getState().views.s1
const nodes = () => viewOf().runNodes
const nodeOf = (id) => nodes().find((n) => n.id === id)
const settles = () => __runSettles()

// 场景 25：会话级运行失败（session_run_error）= 主 run 终态。
// 它前面那条 agent_end 丢了，只剩这条会话级事件 —— 此前只清 streaming，runEnded 仍 false、
// 根节点仍 running：主 run 的停止按钮/结算行永久卡在「运行中」。
// 子 run 不动：既有不变量允许子 run 晚于主 run 收尾（见场景12），不能顺手杀掉。
setup(); __clearRunSettles()
EV('agent_start', { run_id: 'r25' })
EV('agent_start', { run_id: 'r25-sub', parent_run_id: 'r25', depth: 1, task_id: 'tk25', name: 'bg-sub' })
assert.ok(busy(), '两个 run 起跑后应 busy')
assert.ok(mainRunActive(viewOf()), '主 run 在跑')
EV('session_run_error', { error: 'upstream 401' })
const v25 = viewOf()
assert.equal(v25.runEnded, true, '会话级失败必须把结算位落地')
assert.equal(nodeOf('r25')?.status, 'error', '主 run 节点收敛为 error')
assert.ok(!mainRunActive(viewOf()), '主 run 不得再显示运行中')
assert.equal(nodeOf('r25-sub')?.status, 'running', '子 run 仍可晚于主 run 收尾，不许凭主 run 失败下结论')
const t25 = settles().find((r) => r.source === 'session_run_error')
assert.ok(t25, '收敛必须留诊断（为什么它不转了）')
assert.deepEqual(t25.runIds, ['r25'])
console.log('PASS 场景25：session_run_error 收敛主 run（子 run 保持自己的生命周期）')

// 场景 26：会话关闭（session_closed）= 运行已停、不会再有任何事件。
// 只置 runEnded 而留着 runNodes/tool 的 running，busySids 仍把它算作忙。
setup(); __clearRunSettles()
EV('agent_start', { run_id: 'r26' })
EV('agent_start', { run_id: 'r26-bg', parent_run_id: 'r26', depth: 1, task_id: 'tk26', name: 'bg' })
EV('tool_run_start', { id: 'x26', name: 'bash', arguments: '{}', run_id: 'r26' })
assert.ok(busy())
EV('session_closed', { runs: 1, consumed: 0, abandoned_mid_tool: 1 })
assert.equal(viewOf().runEnded, true)
assert.ok(!busy(), '关闭后不得再有任何 running 节点（含子 run / 工具）')
assert.ok(nodes().every((n) => n.status !== 'running'), '所有 run/tool 节点都应落地')
const bg26 = viewOf().blocks.find((b) => b.kind === 'agent' && b.taskId === 'tk26')
assert.equal(bg26?.status, 'abandoned', '残留的子 agent 卡片应收敛为 abandoned')
assert.equal(useAppStore.getState().runningAgentCount, 0)
assert.ok(settles().some((r) => r.source === 'session_closed' && r.runIds.length === 3))
console.log('PASS 场景26：session_closed 全量落地（run/tool/卡片），busy 不再残留')

// 场景 27：子 run 自己的 agent_end 丢了，但它的 task_end 到达 —— 任务终态就是它运行的终态。
setup(); __clearRunSettles()
EV('agent_start', { run_id: 'r27' })
EV('agent_start', { run_id: 'sub27', parent_run_id: 'r27', depth: 1, task_id: 'tk27', name: 'bg' })
EV('agent_end', { run_id: 'r27', finish_reason: 'stop' })
assert.ok(busy(), '父 run 已交卷，但摘离任务的 run 仍在跑（不许被父 run 杀掉）')
assert.equal(nodeOf('sub27')?.status, 'running')
EV('task_end', { task_id: 'tk27', status: 'completed', result: 'ok' })
assert.ok(!busy(), 'task_end 后不得再显示该任务在跑')
assert.equal(nodeOf('sub27')?.status, 'done')
assert.equal(useAppStore.getState().runningAgentCount, 0, 'task_end 也要把子 agent 角标归零')
assert.ok(settles().some((r) => r.source === 'task_end' && r.runIds.includes('sub27')))
console.log('PASS 场景27：task_end 收敛子 run（agent_end 丢失也能落地）')

// 场景 29（反向）：摘离的异步任务还在跑 —— 父 run 的 agent_end 不得顺手杀掉它。
setup(); __clearRunSettles()
EV('agent_start', { run_id: 'r29' })
EV('agent_start', { run_id: 'sub29', parent_run_id: 'r29', depth: 1, task_id: 'tk29', name: 'bg' })
EV('agent_end', { run_id: 'r29', finish_reason: 'stop' })
assert.ok(busy(), '异步任务仍在跑：busy 是真的，不许收敛')
assert.equal(nodeOf('sub29')?.status, 'running')
assert.ok(!settles().some((r) => r.source === 'agent_end'), '没有收敛就不写诊断')
console.log('PASS 场景29：父 run 收尾不误杀摘离的异步任务')

// 场景 30：主 run 的 agent_end 缺 run_id（旧日志/降级宿主）—— 只认唯一的顶层 run。
setup(); __clearRunSettles()
EV('agent_start', { run_id: 'r30' })
EV('agent_end', { finish_reason: 'stop' })
assert.equal(viewOf().runEnded, true)
assert.ok(!busy(), '缺 run_id 的主 run 收尾也必须落地顶层节点')
assert.equal(nodeOf('r30')?.status, 'done')
console.log('PASS 场景30：缺 run_id 的 agent_end 收敛唯一顶层 run')

// 场景 31：run_id 找不到节点（迟到的旧 run）—— 只记录，不猜、不改当前 run 的结算位。
setup(); __clearRunSettles()
EV('agent_start', { run_id: 'r31' })
EV('agent_end', { run_id: 'r-stale', finish_reason: 'stop' })
assert.ok(busy(), '找不到归属的 agent_end 不得杀掉当前 run')
assert.equal(viewOf().runEnded, false, '迟到的旧 agent_end 不得把当前 run 画成已交卷')
const t31 = settles().find((r) => r.source === 'agent_end_unmatched')
assert.ok(t31 && t31.eventRunId === 'r-stale', '未匹配的 agent_end 必须留诊断')
console.log('PASS 场景31：未匹配的 agent_end 只诊断、不猜归属')

// 场景 32：新批次开口（injected=false）证明上一轮已结束 —— agent_end 丢了也能收敛。
setup(); __clearRunSettles()
EV('agent_start', { run_id: 'r32' })
EV('user_inputs_consumed', { user_texts: ['下一轮的问题'], injected: false })
assert.ok(!busy(), '新批次开口后上一轮不得还在 running')
assert.equal(nodeOf('r32')?.status, 'done')
assert.ok(settles().some((r) => r.source === 'new_run_batch' && r.runIds.includes('r32')))
console.log('PASS 场景32：新批次开口收敛上一轮残留的 run')

console.log('STORE_TEST_PASS')
process.exit(0)
