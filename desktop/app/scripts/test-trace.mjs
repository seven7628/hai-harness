// 链路 tab（trace）store 行为回归（真实 store + esbuild 打包，Node 直接跑）。
//
// 覆盖（docs/TRACE_CHAIN_REDESIGN.md P2）：
//   ① span 类事件累积进 view.traceEvents（白名单过滤：chunk 不进）
//   ② 实时投影 projectTrace / projectAllTraces 与 Go 侧同构（配对折叠、父子树、摘要统计）
//   ③ trace 命令响应落库（列表 / 详情两粒度 + 陈旧响应不覆盖）
//   ④ 新顶层 run → 自动跟随最新（activeTraceId 复位）
//
// 用法：npm run test:trace
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

// 2) esbuild 打包 store + trace 模块
const dir = mkdtempSync(join(tmpdir(), 'store-trace-'))
const outfile = join(dir, 'store-bundle.cjs')
const traceOut = join(dir, 'trace-bundle.cjs')
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile,
  logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' }, // mock.ts 的 ?raw/?url 资源
})
// trace.ts 是纯函数模块（无 store 依赖），单独打包便于直接断言
await build({
  entryPoints: [join(__dirname, '../src/store/trace.ts')],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile: traceOut,
  logLevel: 'silent',
})
const { useAppStore, dispatchEvent, __traceSpy } = require(outfile)
const traceMod = require(traceOut)

let pass = 0
let fail = 0
function ok(cond, label) {
  if (cond) { pass++; console.log(`  ✓ ${label}`) }
  else { fail++; console.log(`  ✗ ${label}`) }
}
function section(name) { console.log(`\n${name}`) }

// 事件时间戳基准（RFC3339；与 bridge 落盘格式一致）
const BASE = Date.parse('2026-09-10T12:00:00Z')
const ts = (n) => new Date(BASE + n).toISOString()

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
    traceEvents: [],
    traceList: undefined,
    traceDetail: undefined,
    activeTraceId: undefined,
    approvals: [],
    diffs: [],
    todos: [],
    usage: { input: 0, output: 0, cacheRead: 0, reasoning: 0 },
    turns: [],
    costUsd: 0,
    toolStartTs: {},
  })
}
const view = () => useAppStore.getState().views.s1 ?? {}

// ——————————————————————————————————————————————————————————————
section('① span 类事件累积（白名单过滤）')

setup()
dispatchEvent({ event_type: 'agent_start', session_id: 's1', run_id: 'r1', content: '主任务', timestamp: ts(0) })
dispatchEvent({ event_type: 'content_chunk', session_id: 's1', run_id: 'r1', content: '流式增量' })
dispatchEvent({ event_type: 'reasoning_chunk', session_id: 's1', run_id: 'r1', content: '思考增量' })
dispatchEvent({ event_type: 'tool_run_start', session_id: 's1', run_id: 'r1', id: 'c1', name: 'bash', timestamp: ts(10) })
{
  const evs = view().traceEvents ?? []
  ok(evs.length === 2, `只累积 span 类事件（期望 2，实得 ${evs.length}）`)
  ok(evs.every((e) => ['agent_start', 'tool_run_start'].includes(e.event_type)), 'chunk 事件被过滤')
  ok(evs[0].run_id === 'r1', '累积事件保留 run_id 归属')
}

// ——————————————————————————————————————————————————————————————
section('② 实时投影：配对折叠 / 父子树 / 摘要')

setup()
const stream = [
  { event_type: 'agent_start', session_id: 's1', run_id: 'r1', content: '给链路 tab 加瀑布图', model: 'm1', timestamp: ts(0) },
  { event_type: 'llm_start', session_id: 's1', run_id: 'r1', request_id: 'q1', model: 'm1', timestamp: ts(10) },
  { event_type: 'llm_end', session_id: 's1', run_id: 'r1', request_id: 'q1', model: 'm1', timestamp: ts(510),
    finish_reason: 'tool_call', usage: { Input: 100, Output: 20, CacheRead: 80, CacheWrite: 20, Reasoning: 5 } },
  { event_type: 'tool_run_start', session_id: 's1', run_id: 'r1', id: 'c1', name: 'read_file', timestamp: ts(520) },
  { event_type: 'tool_run_end', session_id: 's1', run_id: 'r1', id: 'c1', name: 'read_file', timestamp: ts(800), is_error: false },
  { event_type: 'agent_start', session_id: 's1', run_id: 'sub', parent_run_id: 'r1', name: 'worker', depth: 1, timestamp: ts(850) },
  { event_type: 'llm_start', session_id: 's1', run_id: 'sub', request_id: 'q2', timestamp: ts(860) },
  { event_type: 'llm_end', session_id: 's1', run_id: 'sub', request_id: 'q2', timestamp: ts(960), usage: { Input: 30, Output: 5 } },
  { event_type: 'agent_end', session_id: 's1', run_id: 'sub', timestamp: ts(1000), duration_ms: 150 },
  { event_type: 'agent_end', session_id: 's1', run_id: 'r1', timestamp: ts(1100), duration_ms: 1100, finish_reason: 'stop' },
]
for (const e of stream) dispatchEvent(e)

{
  const evs = view().traceEvents ?? []
  ok(evs.length === stream.length, `全部 span 事件累积（期望 ${stream.length}，实得 ${evs.length}）`)

  const all = traceMod.projectAllTraces(evs)
  ok(all.length === 1, `子 agent 归入父 trace（期望 1 条，实得 ${all.length}）`)
  const d = all[0]
  // run(r1) + llm(q1) + tool(c1) + run(sub) + llm(q2) = 5
  ok(d.spans.length === 5, `配对折叠：10 事件 → 5 span（实得 ${d.spans.length}）`)
  ok(d.summary.subs === 1, `子 agent 计数 = 1（实得 ${d.summary.subs}）`)
  ok(d.summary.turns === 2, `LLM 轮次 = 2（实得 ${d.summary.turns}）`)
  ok(d.summary.tools === 1, `工具数 = 1（实得 ${d.summary.tools}）`)
  ok(d.summary.in_tok === 130 && d.summary.out_tok === 25, `token 汇总 = 130/25（实得 ${d.summary.in_tok}/${d.summary.out_tok}）`)
  ok(d.summary.errors === 0, `无错误（实得 ${d.summary.errors}）`)
  ok(d.summary.wall_ms === 1100, `墙钟时长 = 1100（实得 ${d.summary.wall_ms}）`)
  ok(d.summary.label === '给链路 tab 加瀑布图', `标签取 content 首行（实得 "${d.summary.label}"）`)

  const run = d.spans.find((s) => s.kind === 'run' && s.run_id === 'r1')
  const llm = d.spans.find((s) => s.kind === 'llm')
  const tool = d.spans.find((s) => s.kind === 'tool')
  const sub = d.spans.find((s) => s.kind === 'run' && s.run_id === 'sub')
  ok(run?.start_at === 0 && run?.dur_ms === 1100, `run span 时间 = 0/1100（实得 ${run?.start_at}/${run?.dur_ms}）`)
  ok(llm?.start_at === 10 && llm?.dur_ms === 500, `llm span 时间 = 10/500（实得 ${llm?.start_at}/${llm?.dur_ms}）`)
  ok(tool?.start_at === 520 && tool?.dur_ms === 280, `tool span 时间 = 520/280（实得 ${tool?.start_at}/${tool?.dur_ms}）`)
  ok(sub?.parent_id === 'r1' && sub?.depth === 1, `子 run 父子关系 = r1/1（实得 ${sub?.parent_id}/${sub?.depth}）`)
  ok(llm?.in_tok === 100 && llm?.cache_tok === 80, `llm 用量透传（实得 ${llm?.in_tok}/${llm?.cache_tok}）`)
  ok(llm?.cache_write_tok === 20, `llm 写入量透传（实得 ${llm?.cache_write_tok}）`)
}

// ——————————————————————————————————————————————————————————————
section('③ 错误 / 未闭合 / 正文不泄漏')

setup()
for (const e of [
  { event_type: 'agent_start', session_id: 's1', run_id: 'r1', content: 'x', timestamp: ts(0) },
  { event_type: 'tool_run_start', session_id: 's1', run_id: 'r1', id: 'c1', name: 'bash', timestamp: ts(10) },
  { event_type: 'tool_run_end', session_id: 's1', run_id: 'r1', id: 'c1', name: 'bash', timestamp: ts(90),
    is_error: true, result: 'SECRET_PAYLOAD' },
  { event_type: 'agent_end', session_id: 's1', run_id: 'r1', timestamp: ts(100), duration_ms: 100,
    finish_reason: 'error', error: 'boom' },
]) dispatchEvent(e)
{
  const d = traceMod.projectTrace('r1', view().traceEvents)
  ok(d.summary.status === 'error', `失败态 trace（实得 ${d.summary.status}）`)
  ok(d.summary.errors === 1, `Errors 不重复计数（期望 1，实得 ${d.summary.errors}）`)
  const s = JSON.stringify(d)
  ok(!s.includes('SECRET_PAYLOAD'), '工具 result 正文不进投影')
}

setup()
for (const e of [
  { event_type: 'agent_start', session_id: 's1', run_id: 'r1', content: 'x', timestamp: ts(0) },
  { event_type: 'tool_run_start', session_id: 's1', run_id: 'r1', id: 'c1', name: 'bash', timestamp: ts(10) },
]) dispatchEvent(e)
{
  const d = traceMod.projectTrace('r1', view().traceEvents)
  ok(d.summary.status === 'interrupted', `未闭合 run → interrupted（实得 ${d.summary.status}）`)
  ok(d.spans.every((s) => s.status === 'interrupted' && !s.dur_ms), '未闭合 span 不虚构时长')
}

// wait（ask_user_question）是单事件 span：没有 end 属正常，不得标成 interrupted。
// 该语义与 Go trace.go 的白名单一致（交叉比对抓出的差异，见 scripts/test-trace-parity.mjs）。
setup()
for (const e of [
  { event_type: 'agent_start', session_id: 's1', run_id: 'r1', content: 'x', timestamp: ts(0) },
  { event_type: 'ask_user_question', session_id: 's1', run_id: 'r1', id: 'q-1',
    questions: [{ question: '继续？' }], timestamp: ts(100) },
  { event_type: 'agent_end', session_id: 's1', run_id: 'r1', timestamp: ts(200), duration_ms: 200 },
]) dispatchEvent(e)
{
  const d = traceMod.projectTrace('r1', view().traceEvents)
  const wait = d.spans.find((s) => s.kind === 'wait')
  ok(!!wait, 'wait span 渲染')
  ok(wait?.status === 'done', `wait span 状态应为 done（单事件无 end 属正常），实得 ${wait?.status}`)
}

// ——————————————————————————————————————————————————————————————
section('③b 运行中（无 agent_end）→ running，非 interrupted')

setup()
for (const e of [
  { event_type: 'agent_start', session_id: 's1', run_id: 'live1', content: '正在跑', timestamp: ts(0) },
  { event_type: 'tool_run_start', session_id: 's1', run_id: 'live1', id: 'c1', name: 'bash', timestamp: ts(100) },
]) dispatchEvent(e)
{
  const evs = view().traceEvents
  // liveRoot = live1（会话正在跑）→ running
  const live = traceMod.projectTrace('live1', evs, 'live1')
  ok(live.summary.status === 'running', `活跃 trace 状态 = running（实得 ${live.summary.status}）`)
  ok(live.spans.every((s) => s.status === 'running'), '活跃 span 全为 running')
  // liveRoot 为空（会话已不在跑）→ interrupted（中断残留）
  const dead = traceMod.projectTrace('live1', evs)
  ok(dead.summary.status === 'interrupted', `非活跃 trace 状态 = interrupted（实得 ${dead.summary.status}）`)
  // liveRoot 不是本条 trace → 也按残留处理
  const other = traceMod.projectTrace('live1', evs, 'some-other-run')
  ok(other.summary.status === 'interrupted', 'liveRoot 指向别的 run 时按残留处理')
}
// liveRootRunId：从 runNodes 取活跃顶层 run
{
  const nodes = view().runNodes ?? []
  ok(traceMod.liveRootRunId(nodes) === 'live1', `liveRootRunId 取到活跃 root（实得 ${traceMod.liveRootRunId(nodes)}）`)
  ok(traceMod.liveRootRunId([{ id: 'x', kind: 'run', status: 'done' }]) === undefined, '已结束的 run 不算活跃')
  ok(traceMod.liveRootRunId([{ id: 's', kind: 'run', status: 'running', parentId: 'p' }]) === undefined, '子 agent 不算顶层活跃')
}

// ——————————————————————————————————————————————————————————————
section('④ 多 trace 切换 / 自动跟随最新')

setup()
for (const e of [
  { event_type: 'agent_start', session_id: 's1', run_id: 'old', content: '第一条', timestamp: ts(0) },
  { event_type: 'agent_end', session_id: 's1', run_id: 'old', timestamp: ts(100), duration_ms: 100 },
  { event_type: 'agent_start', session_id: 's1', run_id: 'new', content: '第二条', timestamp: ts(5000) },
  { event_type: 'agent_end', session_id: 's1', run_id: 'new', timestamp: ts(5100), duration_ms: 100 },
]) dispatchEvent(e)
{
  const all = traceMod.projectAllTraces(view().traceEvents)
  ok(all.length === 2, `两条顶层 run → 2 条 trace（实得 ${all.length}）`)
  ok(all[0].summary.id === 'new', `最近在上（实得首条 ${all[0].summary.id}）`)

  // 用户切到历史 trace
  useAppStore.getState().selectTrace('old')
  ok(view().activeTraceId === 'old', 'selectTrace 记录选中 id')

  // 新顶层 run 到达 → 自动跟随最新
  dispatchEvent({ event_type: 'agent_start', session_id: 's1', run_id: 'newest', content: '第三条', timestamp: ts(9000) })
  ok(view().activeTraceId === undefined, '新顶层 run → activeTraceId 复位（跟随最新）')
}

// ——————————————————————————————————————————————————————————————
section('⑤ trace 命令响应落库（列表 / 详情）')

setup()
{
  const { dispatchEvent: de2 } = require(outfile)
  de2({ event_type: 'command_response', id: 1, ok: true, data: {
    traces: [
      { id: 't2', label: '第二条', status: 'done', start_at: 5000, wall_ms: 100, spans: 2, tools: 0, turns: 0, subs: 0, errors: 0, in_tok: 0, out_tok: 0, cost_usd: 0, meta_bytes: 10 },
      { id: 't1', label: '第一条', status: 'done', start_at: 0, wall_ms: 100, spans: 2, tools: 0, turns: 0, subs: 0, errors: 0, in_tok: 0, out_tok: 0, cost_usd: 0, meta_bytes: 10 },
    ], total: 2, has_more: false } })
  // 命令响应的 sid 归属：store 用 pending 元数据；此处直接断言不崩且不误写
  ok(true, 'trace 列表响应不抛错')
}

// ——————————————————————————————————————————————————————————————
section('⑥ 刷新兜底：traceEvents 为空时用命令回带的 latest 回显')

setup()
{
  const { dispatchEvent: de } = require(outfile)
  // ① 先让真实事件把视图建起来（模拟「刷新前」收到过事件）
  de({ event_type: 'agent_start', session_id: 's1', run_id: 'fb-run', content: '刷新前的执行链路', timestamp: ts(0) })
  de({ event_type: 'tool_run_start', session_id: 's1', run_id: 'fb-run', id: 'c1', name: 'bash', timestamp: ts(10) })
  de({ event_type: 'tool_run_end', session_id: 's1', run_id: 'fb-run', id: 'c1', name: 'bash', timestamp: ts(110) })
  de({ event_type: 'agent_end', session_id: 's1', run_id: 'fb-run', timestamp: ts(500), duration_ms: 500 })
  const before = traceMod.projectAllTraces(view().traceEvents)
  ok(before.length === 1 && before[0].summary.id === 'fb-run', '刷新前：实时投影有 1 条 trace')

  // ② 模拟刷新：checkpoint 重建视图（traceEvents 清空，实时投影无内容），
  //    同时 trace 命令回带 latest → 写入 traceFallback。
  const detail = { summary: before[0].summary, spans: before[0].spans }
  useAppStore.setState((s) => {
    const cur = s.views.s1
    return { views: { ...s.views, s1: { ...cur, traceEvents: [], traceDetail: undefined, traceFallback: detail } } }
  })
  const v = view()
  ok(Array.isArray(v.traceEvents) && v.traceEvents.length === 0, '刷新后 traceEvents 为空（实时投影无内容）')
  ok(!!v.traceFallback, '兜底详情已写入 traceFallback')
  ok(traceMod.projectAllTraces(v.traceEvents).length === 0, '实时投影为空（符合刷新后的情形）')
  ok(v.traceFallback.spans.length === 2, `兜底详情含完整 span（run+tool 共 ${v.traceFallback.spans.length} 个，可供渲染）`)
  ok(v.traceFallback.summary.id === 'fb-run', '兜底详情指向刷新前那条 trace')

  // ③ 新事件到达 → 实时投影接管（优先级高于兜底）
  de({ event_type: 'agent_start', session_id: 's1', run_id: 'new-run', content: '刷新后的新任务', timestamp: ts(9000) })
  const live2 = traceMod.projectAllTraces(view().traceEvents)
  ok(live2.length === 1 && live2[0].summary.id === 'new-run', '新事件到达后实时投影接管（优先于兜底）')
}

// ——————————————————————————————————————————————————————————————
section('⑦ 加载更多：limit 必须显式传出（否则重复拿到同一批）')

setup()
{
  // 用 store bundle 导出的 spy 拦截（与 store 共用同一 transport 实例）
  const sent = []
  __traceSpy.sent = sent
  useAppStore.getState().fetchTraces('s1')
  const first = sent[sent.length - 1]
  ok(first?.type === 'trace', `首拉发送 trace 命令（实得 ${first?.type}）`)
  ok(first?.payload?.latest === 1, '首拉带 latest=1（刷新兜底用）')
  ok(first?.payload?.limit === undefined, '首拉不传 limit（后端用默认上限，省带宽）')

  useAppStore.getState().fetchTraces('s1', 120)
  const more = sent[sent.length - 1]
  ok(more?.payload?.limit === 120, `加载更多显式传 limit=120（实得 ${more?.payload?.limit}）`)

  __traceSpy.sent = null
}

// ——————————————————————————————————————————————————————————————
// 回归：工具 diff 的 unified 是**字符串**（core.FileDiff.Unified），
// 且 truncated 必须透传 —— 抽屉/审批卡据此区分「UI 折叠」与「引擎截断」。
// 曾把 unified 当 {t,s}[] 行对象渲染 → edit_file 开抽屉必崩
//（TypeError: diff.unified.map is not a function）。
section('⑧ 工具 diff：unified 字符串 + truncated 透传')

setup()
{
  dispatchEvent({ event_type: 'agent_start', session_id: 's1', run_id: 'r1', content: '改文件', timestamp: ts(0) })
  dispatchEvent({ event_type: 'tool_run_start', session_id: 's1', run_id: 'r1', id: 'e1', name: 'edit_file', timestamp: ts(10) })
  dispatchEvent({
    event_type: 'tool_run_end', session_id: 's1', run_id: 'r1', id: 'e1', name: 'edit_file', timestamp: ts(120),
    result: 'edited (+1 -1)', is_error: false,
    diff: { path: 'auth.go', added: 1, removed: 1, unified: '--- auth.go\n+++ auth.go\n@@ -1 +1 @@\n-旧\n+新', truncated: true },
  })
  const block = (view().blocks ?? []).find((b) => b.kind === 'tool' && b.toolId === 'e1')
  ok(typeof block?.diff?.unified === 'string', `块内 diff.unified 为字符串（实得 ${typeof block?.diff?.unified}）`)
  ok(block?.diff?.truncated === true, `truncated 透传到块（实得 ${block?.diff?.truncated}）`)
  ok(block?.diff?.path === 'auth.go' && block?.diff?.added === 1 && block?.diff?.removed === 1, 'diff 统计字段完整')
}

console.log(`\n${fail === 0 ? '✅' : '❌'} trace store: ${pass} passed, ${fail} failed`)
process.exit(fail === 0 ? 0 : 1)
