// 复现「切换会话时历史没加载」的完整前端链路（2026-11-09）：
//   1. 切到未缓存的 B（new_session 在途）
//   2. 在途期间 B 的实时事件被 wire() 缓冲
//   3. 用户又切到已缓存的 C → 导航代际变化
//   4. B 的响应到达 → 按陈旧导航丢弃，但缓冲事件照常 dispatch → views[B].blocks>0
//   5. 再切回 B → isHydrated 只看 blocks>0 → 不再请求历史 → 只剩当时那一小段
//
// 用法：node lab/probe-session-switch-stale-response.mjs
import { build } from 'esbuild'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const LS = new Map()
global.localStorage = {
  getItem: (k) => (LS.has(k) ? LS.get(k) : null),
  setItem: (k, v) => LS.set(k, String(v)),
  removeItem: (k) => LS.delete(k),
}
const WS = '/ws-stale'
const SID_A = 'sA', SID_B = 'sB', SID_C = 'sC'
LS.set('go-code.projects', JSON.stringify({ paths: [WS], active: WS, lastSession: { path: WS, id: SID_A } }))

const calls = []
const F = { cb: null, held: null, holdB: true, sessions: {
  [SID_A]: { id: SID_A, title: 'A', updated_at: 3 },
  [SID_B]: { id: SID_B, title: 'B', updated_at: 2 },
  [SID_C]: { id: SID_C, title: 'C', updated_at: 1 },
} }
const list = () => Object.values(F.sessions)
const emitLine = (obj) => { if (F.cb) F.cb(JSON.stringify(obj)) }
const emit = (id, data, workspace = WS) => emitLine({ event_type: 'command_response', id, ok: true, data, workspace })
const checkpoint = (tag) => ({
  restored: 'checkpoint', checkpoint_version: 1, revision: 1, reducer_seq: 1, clear_generation: 0,
  degraded: false, blocks_total: 4, blocks_cursor: 1, blocks_has_more: false, turns_total: 2,
  live: false,
  view_checkpoint: {
    schema_version: 1,
    blocks: [
      { kind: 'user', id: 1, text: tag + ' 第一条', ts: 1000 },
      { kind: 'assistant', id: 2, text: tag + ' 回答一', model: 'm', run_id: 'r0', ts: 1100 },
      { kind: 'user', id: 3, text: tag + ' 第二条', ts: 2000 },
      { kind: 'assistant', id: 4, text: tag + ' 回答二', model: 'm', run_id: 'r0', ts: 2100 },
    ],
    run_nodes: [], turns: [{ run_id: 'r0', dur_ms: 10 }, { run_id: 'r0', dur_ms: 20 }],
    usage: { input: 10, output: 5 }, cost_usd: 0, todos: [], diffs: [],
  },
})
const desktop = {
  startBridge: async () => ({ ok: true }), pickWorkspace: async () => null, homeDir: async () => '/tmp',
  createWorkspaceFolder: async () => ({ ok: false }), settingsGet: async () => null, settingsSet: async () => null,
  externalSkillsStatus: async () => null, importExternalSkills: async () => ({ ok: true }),
  pickApp: async () => null, providerSave: async () => ({ ok: true }), providerRemove: async () => ({ ok: true }),
  providerKeyStatus: async () => false, mcpSave: async () => ({ ok: true }),
  fsList: async () => ({ ok: true, entries: [] }), fsReadRef: async () => ({ ok: true, content: '' }),
  pickFsFile: async () => null, pickFsDir: async () => null,
  inputHistoryGet: async () => ({ ok: true, items: [] }), inputHistorySet: async () => ({ ok: true }),
  inputHistoryForget: async () => ({ ok: true }),
  command: async (cmd) => {
    calls.push({ type: cmd.type, id: cmd.id, sid: String(cmd.payload?.id ?? cmd.payload?.session_id ?? '') })
    const ws = String(cmd.payload?.workspace ?? WS)
    if (cmd.type === 'list') { emit(cmd.id, { sessions: list() }, ws); return { ok: true } }
    if (cmd.type === 'new_session') {
      const want = String(cmd.payload?.id ?? '')
      if (want === SID_A) { emit(cmd.id, { session_id: SID_A, sessions: list(), todos: [], live: false, ...checkpoint('A') }, ws); return { ok: true } }
      if (want === SID_B) {
        if (F.holdB) { F.held = { cmd, ws }; return { ok: true } } // 扣住第一次，模拟冷开慢
        emit(cmd.id, { session_id: SID_B, sessions: list(), todos: [], live: false, ...checkpoint('B') }, ws)
        return { ok: true }
      }
      return { ok: true }
    }
    emit(cmd.id, {}, ws)
    return { ok: true }
  },
  onEvent: (cb) => { F.cb = cb; return () => { F.cb = null } },
  onExit: () => () => {},
}
// 必须在 require store 之前挂 window：store 顶层 const transport = getTransport()（3292）
global.window = {
  localStorage: global.localStorage,
  matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
  addEventListener() {}, removeEventListener() {}, desktop,
}
global.document = { documentElement: { classList: { add() {}, remove() {} } } }

const dir = mkdtempSync(join(tmpdir(), 'stale-restore-'))
const bundle = join(dir, 'store.cjs')
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true, format: 'cjs', platform: 'node', outfile: bundle, logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' },
})
const { useAppStore, hydrateCheckpoint } = require(bundle)
const st = () => useAppStore.getState()

await st().start()
await sleep(200)
console.log('start 后 active=%s workspace=%s calls=%s', st().activeSessionId, st().workspace, JSON.stringify(calls.map(c => c.type + ':' + c.sid)))
if (st().activeSessionId !== SID_A) throw new Error('A 未恢复：' + st().activeSessionId)
// C 预先 hydrate（模拟之前打开过 C）
hydrateCheckpoint(SID_C, { schema_version: 1, blocks: [{ kind: 'user', id: 101, text: 'C 历史', ts: 1 }], run_nodes: [], turns: [], usage: {} }, 1, { revision: 1, live: false })
// 切到 B：请求在途
st().setActiveSession(SID_B)
await sleep(10)
if (!F.held) throw new Error('B 的 new_session 未被扣住：' + JSON.stringify(calls.slice(-4)))
// 在途期间 B 产生实时事件（wire 会缓冲，因为 snapshotPending 有 B）
emitLine({ event_type: 'content_chunk', session_id: SID_B, run_id: 'rB', request_id: 'qB', content: 'B 在途时的一条输出' })
// 用户又切到 C（已缓存）→ 代际变化
st().setActiveSession(SID_C)
if (st().activeSessionId !== SID_C) throw new Error('C 未激活')
// B 的响应此时才到：按陈旧导航丢弃，但缓冲事件被 dispatch
emit(F.held.cmd.id, { session_id: SID_B, sessions: list(), todos: [], live: false, ...checkpoint('B') }, F.held.ws)
await sleep(100)
const vb = st().views[SID_B]
console.log('陈旧响应到达后 views[B]：blocks=%d revision=%s turns=%d', vb?.blocks.length, vb?.checkpointRevision, vb?.turns.length)
// 用户再切回 B
const midCalls = calls.filter((c) => c.type === 'new_session').length
F.holdB = false // 第二次请求立即回完整 checkpoint
st().setActiveSession(SID_B)
await sleep(60)
const afterCalls = calls.filter((c) => c.type === 'new_session').length
const v2 = st().views[SID_B]
console.log('切回 B 后：又发 new_session=%s blocks=%d turns=%d（完整历史应为 4 块 / 2 轮）',
  afterCalls > midCalls, v2?.blocks.length, v2?.turns.length)
console.log('  → 结论：%s', afterCalls > midCalls && v2?.hydrated === true && v2?.blocks.length === 4
  ? '已修：陈旧尾部 hydrated=false → 切回重新请求并完整落地（4 块 / 2 轮）'
  : '仍未修：切回没有重新请求或历史不完整')
process.exit(0)
