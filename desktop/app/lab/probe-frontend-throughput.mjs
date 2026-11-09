// 前端吞吐探针（2026-11-09）：把**真实会话事件流**回放进 store，
// 量「每条事件的 store 侧成本」随 blocks 增长的曲线。
//
// 背景：用户报「会话一直卡住」。实测：后台（bridge/Go）事件流正常推进，
// 但 Electron renderer 常驻 20~30% CPU、1.3GB RSS，主线程热点在**文字排版**
// （font/text shaping）而非 JS 逻辑 → 怀疑「每条事件都让整棵转录树重排」。
//
// 本探针只量 store 侧（dispatchEvent → setState），不含 React 渲染，
// 用来回答两个问题：
//   ① 每条事件的 store 成本是否随 blocks 数线性增长（O(n²) 累积）？
//   ② 真实事件流的「每事件成本」在会话尾段有多大？
//
// 用法：node lab/probe-frontend-throughput.mjs [事件文件路径]
import { build } from 'esbuild'
import { mkdtempSync, readFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, dirname } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = dirname(fileURLToPath(import.meta.url))
const require = createRequire(import.meta.url)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const EVENTS = process.argv[2] ??
  '/Users/admin/.go-code/events/Users_admin_workspace_go-code-855c9368/s-1793856293837663000-16.jsonl'
const SID = 's-probe'
const TAIL = Number(process.env.TAIL ?? 4000) // 只回放尾部 N 条（真实长会话的尾部最接近现场）

const dir = mkdtempSync(join(tmpdir(), 'fe-throughput-'))
const outfile = join(dir, 'store.cjs')
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true, format: 'cjs', platform: 'node', outfile, logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' },
})

const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
}
global.window = {
  localStorage: global.localStorage,
  matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
  addEventListener() {}, removeEventListener() {},
  desktop: {
    startBridge: async () => ({ ok: true }), pickWorkspace: async () => null, homeDir: async () => '/tmp',
    createWorkspaceFolder: async () => ({ ok: false }), settingsGet: async () => null, settingsSet: async () => null,
    externalSkillsStatus: async () => null, importExternalSkills: async () => ({ ok: true }),
    pickApp: async () => null, providerSave: async () => ({ ok: true }), providerRemove: async () => ({ ok: true }),
    providerKeyStatus: async () => false, mcpSave: async () => ({ ok: true }),
    fsList: async () => ({ ok: true, entries: [] }), fsReadRef: async () => ({ ok: true, content: '' }),
    pickFsFile: async () => null, pickFsDir: async () => null,
    command: async () => ({ ok: true }),
    onEvent: () => () => {}, onExit: () => () => {},
  },
}

const { useAppStore, dispatchEvent } = require(outfile)
const st = () => useAppStore.getState()

// 读真实事件流（jsonl 每行一个事件）
const raw = readFileSync(EVENTS, 'utf8')
const all = []
for (const line of raw.split('\n')) {
  const s = line.trim()
  if (!s) continue
  try { all.push(JSON.parse(s)) } catch { /* 半行，跳过 */ }
}
const evs = all.slice(-TAIL).filter((e) => e && typeof e.event_type === 'string')
console.log('事件文件：%s', EVENTS)
console.log('总事件 %d 条，回放尾部 %d 条', all.length, evs.length)

// 先把历史铺进去（模拟「打开一个已经有很长历史的会话」）：
// 用尾部之前的事件按真实顺序回放，让 blocks 长到真实量级。
const hist = all.slice(0, Math.max(0, all.length - TAIL)).filter((e) => e && typeof e.event_type === 'string')
console.log('历史事件 %d 条（先铺）', hist.length)

useAppStore.setState({
  workspace: '/ws', activeSessionId: SID, bridgeStatus: 'running', views: {},
  sessions: [{ id: SID, title: 'P' }],
  projects: [{ path: '/ws', sessions: [{ id: SID, title: 'P' }] }],
  blocks: [], runNodes: [], approvals: [], diffs: [], todos: [], turns: [], usage: {}, costUsd: 0,
  streaming: false, pendingQueue: [], toolStartTs: {}, thinkStartTs: {}, turnThinkMs: {},
  recentSessions: [], titleOverrides: {}, settings: { provider: { active: 'p' } }, model: 'm', provider: 'p',
})

// 铺历史：只走「会进 blocks」的事件类型，避免把纯指标事件也算进去
const HIST_TYPES = new Set([
  'agent_start', 'agent_end', 'tool_run_start', 'tool_run_end', 'tool_start', 'tool_response',
  'tool_output', 'content_chunk', 'reasoning_chunk', 'llm_start', 'llm_end', 'user_inputs_consumed',
  'task_started', 'task_end', 'task_promoted', 'task_message', 'task_result_delivered',
])
let t0 = Date.now()
for (const e of hist) {
  if (!HIST_TYPES.has(e.event_type)) continue
  dispatchEvent({ ...e, session_id: SID })
}
const histMs = Date.now() - t0
const v0 = st().views[SID]
console.log('铺历史：%d ms，blocks=%d runNodes=%d turns=%d',
  histMs, v0?.blocks.length ?? 0, v0?.runNodes.length ?? 0, v0?.turns.length ?? 0)
console.log('')

// 回放尾部：每条事件计时，并按 blocks 规模分段统计
const buckets = new Map() // blocks规模档 → {n, totalMs, maxMs, types}
function bucketOf(n) {
  if (n < 100) return '<100'
  if (n < 500) return '100-500'
  if (n < 1000) return '500-1k'
  if (n < 2000) return '1k-2k'
  if (n < 5000) return '2k-5k'
  if (n < 10000) return '5k-10k'
  return '>10k'
}
const perType = new Map()
const N = evs.length
for (let i = 0; i < N; i++) {
  const e = evs[i]
  const nb = st().views[SID]?.blocks.length ?? 0
  const bk = bucketOf(nb)
  const s = performance.now()
  dispatchEvent({ ...e, session_id: SID })
  const dt = performance.now() - s
  let b = buckets.get(bk)
  if (!b) { b = { n: 0, total: 0, max: 0 }; buckets.set(bk, b) }
  b.n++; b.total += dt; if (dt > b.max) b.max = dt
  let pt = perType.get(e.event_type)
  if (!pt) { pt = { n: 0, total: 0, max: 0 }; perType.set(e.event_type, pt) }
  pt.n++; pt.total += dt; if (dt > pt.max) pt.max = dt
  if (i % 500 === 0) await sleep(0)
}

console.log('=== 按 blocks 规模分档（尾部回放）===')
console.log('blocks档   事件数   总耗时ms    平均ms    最大ms')
for (const [bk, b] of [...buckets.entries()].sort()) {
  console.log('%-10s %7d %11.1f %9.3f %8.2f', bk, b.n, b.total, b.total / b.n, b.max)
}
console.log('')
console.log('=== 按事件类型 ===')
console.log('类型                  事件数   总耗时ms    平均ms    最大ms')
for (const [k, v] of [...perType.entries()].sort((a, b) => b[1].total - a[1].total).slice(0, 12)) {
  console.log('%-20s %7d %11.1f %9.3f %8.2f', k, v.n, v.total, v.total / v.n, v.max)
}
const vEnd = st().views[SID]
console.log('')
console.log('结束：blocks=%d runNodes=%d turns=%d',
  vEnd?.blocks.length ?? 0, vEnd?.runNodes.length ?? 0, vEnd?.turns.length ?? 0)
