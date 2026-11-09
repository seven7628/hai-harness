// 会话切换回归（2026-11-09）：
//   S1 陈旧 new_session 响应 + 在途 preBuf → view 只有尾部（blocks>0，但 hydrated=false）；
//      再切回必须重新请求并完整落地（旧实现只看 blocks>0，永远不再请求 → 历史缺失无报错）。
//   S2 新建会话：点击即进入 loading shell（activeSessionId=undefined + 骨架屏），
//      响应到达后激活新会话（旧实现点击后界面原样停在旧会话，直到 6–15s 后响应）。
//   S3 bridge 未运行：transport.send 的 ok:false 必须变成可见错误（toast + bridgeStatus=error），
//      不能静默吞掉。
//   S4 焦点跨会话保留（产品决策：子智能体是视图模式，切回恢复它；刷新才清空）——
//      钉住「只修 hydrated，不清 focusedAgent」的行为，防止后续误改。
//
// 用法：npm run test:session-switch
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync, copyFileSync } from 'node:fs'
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
global.document = { documentElement: { classList: { add() {}, remove() {} } } }

const dir = mkdtempSync(join(tmpdir(), 'session-switch-'))
const bundle = join(dir, 'store.cjs')
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true, format: 'cjs', platform: 'node', outfile: bundle, logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' },
})
let seq = 0
function freshStore(tag) {
  const out = join(dir, `store-${tag}-${++seq}.cjs`)
  copyFileSync(bundle, out)
  return require(out)
}

const WS = '/ws-switch'
const A = 'sA', B = 'sB', C = 'sC', NEW = 'sNew'
const sessions = () => [
  { id: A, title: 'A', updated_at: 4 },
  { id: B, title: 'B', updated_at: 3 },
  { id: C, title: 'C', updated_at: 2 },
]
function cp(tag) {
  return {
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
  }
}
function makeDesktop({ onCommand, list = sessions() }) {
  const listArr = list
  const listFn = () => listArr
  const calls = []
  const F = { cb: null, held: null, heldBare: null }
  const emitLine = (obj) => { if (F.cb) F.cb(JSON.stringify(obj)) }
  const emit = (id, data, workspace = WS) => emitLine({ event_type: 'command_response', id, ok: true, data, workspace })
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
      if (cmd.type === 'list') { emit(cmd.id, { sessions: listArr }); return { ok: true } }
      return onCommand(cmd, { emit, emitLine, F, list: listFn, calls })
    },
    onEvent: (cb) => { F.cb = cb; return () => { F.cb = null } },
    onExit: () => () => {},
  }
  return { desktop, F, calls, emit, emitLine }
}
function install(desktop) {
  global.window = {
    localStorage: global.localStorage,
    matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
    addEventListener() {}, removeEventListener() {}, desktop,
  }
}
function seedProjects(last = A) {
  LS.clear()
  LS.set('go-code.projects', JSON.stringify({ paths: [WS], active: WS, lastSession: { path: WS, id: last } }))
}
async function boot(store) {
  const st = () => store.getState()
  await st().start()
  await sleep(150)
  return st
}
let pass = 0
function ok(label) { pass++; console.log(`  ✅ ${label}`) }

// ============ S1：陈旧响应只剩尾部 → hydrated=false → 切回重新请求 ============
console.log('S1 陈旧 new_session 响应 + preBuf → 切回必须重新请求完整历史')
{
  seedProjects()
  let store
  const { desktop, F, calls, emit } = makeDesktop({
    onCommand: (cmd, { emit, list }) => {
      if (cmd.type !== 'new_session') { emit(cmd.id, {}); return { ok: true } }
      const want = String(cmd.payload?.id ?? '')
      if (want === A) { emit(cmd.id, { session_id: A, sessions: list(), todos: [], ...cp('A') }); return { ok: true } }
      if (want === B) {
        if (!F.holdB) { emit(cmd.id, { session_id: B, sessions: list(), todos: [], ...cp('B') }); return { ok: true } }
        F.held = { cmd } // 扣住第一次：模拟冷开慢
        return { ok: true }
      }
      emit(cmd.id, { session_id: want, sessions: list(), todos: [], ...cp(want) })
      return { ok: true }
    },
  })
  F.holdB = true
  install(desktop)
  const mod = freshStore('s1')
  store = mod.useAppStore
  const st = await boot(store)
  assert.equal(st().activeSessionId, A, 'A 自动恢复')
  // C 预 hydrate（模拟之前打开过 C）
  mod.hydrateCheckpoint(C, { schema_version: 1, blocks: [{ kind: 'user', id: 101, text: 'C 历史', ts: 1 }], run_nodes: [], turns: [], usage: {} }, 1, { revision: 1, live: false })
  // 切 B：请求在途
  st().setActiveSession(B)
  await sleep(10)
  assert(F.held, 'B 的 new_session 应被扣住')
  // 在途事件 → preBuf
  F.cb(JSON.stringify({ event_type: 'content_chunk', session_id: B, run_id: 'rB', request_id: 'qB', content: 'B 在途输出' }))
  // 切 C → 代际变化
  st().setActiveSession(C)
  await sleep(10)
  // B 响应迟到 → 陈旧丢弃，preBuf 照常 dispatch
  emit(F.held.cmd.id, { session_id: B, sessions: sessions(), todos: [], ...cp('B') })
  await sleep(80)
  const vb = st().views[B]
  assert.equal(vb.blocks.length, 1, '陈旧尾部只有 1 块')
  assert.notEqual(vb.hydrated, true, '陈旧尾部不得标记 hydrated')
  // 切回 B：必须重新请求；这次立即回完整 checkpoint
  F.holdB = false
  const before = calls.filter((c) => c.type === 'new_session' && c.sid === B).length
  st().setActiveSession(B)
  await sleep(120)
  const after = calls.filter((c) => c.type === 'new_session' && c.sid === B).length
  assert(after > before, '切回 B 应重新发 new_session')
  const v2 = st().views[B]
  assert.equal(v2.hydrated, true, '完整恢复后 hydrated=true')
  assert.equal(v2.blocks.length, 4, '完整历史 4 块落地')
  assert.equal(v2.turns.length, 2, '完整历史 2 轮落地')
  ok('陈旧 preBuf 不伪装完整历史；切回重新请求并完整落地')
}

// ============ S2：新建会话立即 loading shell ============
console.log('S2 新建会话：点击即 loading shell，响应后激活')
{
  seedProjects()
  let store
  const { desktop, F, calls, emit } = makeDesktop({
    onCommand: (cmd, { emit, list }) => {
      if (cmd.type !== 'new_session') { emit(cmd.id, {}); return { ok: true } }
      const want = String(cmd.payload?.id ?? '')
      if (!want) { F.heldBare = cmd; return { ok: true } } // 扣住新建
      emit(cmd.id, { session_id: want, sessions: list(), todos: [], ...cp(want) })
      return { ok: true }
    },
  })
  install(desktop)
  const mod = freshStore('s2')
  store = mod.useAppStore
  const st = await boot(store)
  assert.equal(st().activeSessionId, A, 'A 自动恢复')
  await st().newSession()
  await sleep(200)
  assert.equal(st().activeSessionId, undefined, '点击后立即离开旧会话（loading shell）')
  assert(st().skeleton.phase === 'pending' || st().skeleton.phase === 'visible', `骨架屏应进入 pending/visible（实际 ${st().skeleton.phase}）`)
  assert(F.heldBare, 'bare new_session 应已发出并被扣住')
  // 响应到达 → 激活新会话
  emit(F.heldBare.id, { session_id: NEW, sessions: [...sessions(), { id: NEW, title: 'New Chat', updated_at: 9 }], todos: [], ...cp('NEW') })
  await sleep(120)
  assert.equal(st().activeSessionId, NEW, '响应后激活新会话')
  assert.equal(st().views[NEW]?.hydrated, true, '新会话视图 hydrated')
  assert.notEqual(st().skeleton.phase, 'visible', '数据到达后骨架屏退场')
  assert(!calls.some((c) => c.type === 'new_session' && c.sid === NEW), '不应把新会话再当成恢复请求')
  ok('点击即 loading shell；响应后激活新会话并退场骨架屏')
}

// ============ S3：bridge 未运行 → 可见错误 ============
console.log('S3 bridge 未运行：transport.send 的 ok:false 必须可见')
{
  seedProjects()
  let store
  const { desktop, emit } = makeDesktop({
    onCommand: (cmd, { emit, list }) => {
      if (cmd.type !== 'new_session') { emit(cmd.id, {}); return { ok: true } }
      const want = String(cmd.payload?.id ?? '')
      if (want) { emit(cmd.id, { session_id: want, sessions: list(), todos: [], ...cp(want) }); return { ok: true } }
      return { ok: false, error: 'bridge 未运行' }
    },
  })
  install(desktop)
  const mod = freshStore('s3')
  store = mod.useAppStore
  const st = await boot(store)
  assert.equal(st().activeSessionId, A)
  await st().newSession()
  await sleep(200)
  assert.equal(st().bridgeStatus, 'error', 'bridgeStatus 应为 error')
  assert.equal(st().toast?.kind, 'error', '应有错误 toast')
  assert(String(st().toast?.msg).includes('bridge'), 'toast 文案包含 bridge')
  assert.equal(st().activeSessionId, undefined, '新建失败后仍停在 loading shell（可点击左栏恢复）')
  ok('ok:false 被合成为 command_response；toast + bridgeStatus=error 可见')
}

// ============ S4：焦点跨会话保留（产品决策） ============
console.log('S4 子智能体焦点跨会话保留（只修 hydrated，不清 focusedAgent）')
{
  seedProjects()
  let store
  const { desktop, emitLine } = makeDesktop({
    onCommand: (cmd, { emit, list }) => {
      if (cmd.type !== 'new_session') { emit(cmd.id, {}); return { ok: true } }
      const want = String(cmd.payload?.id ?? '')
      emit(cmd.id, { session_id: want, sessions: list(), todos: [], ...cp(want) })
      return { ok: true }
    },
  })
  install(desktop)
  const mod = freshStore('s4')
  store = mod.useAppStore
  const st = await boot(store)
  assert.equal(st().activeSessionId, A)
  // A 里收到子智能体审批 → 自动聚焦
  emitLine({ event_type: 'agent_start', session_id: A, run_id: 'subA', parent_run_id: 'mainA', depth: 1, name: '子任务', task_id: 'taskA', timestamp: 4000 })
  emitLine({ event_type: 'tool_approval_requested', session_id: A, id: 'ap1', run_id: 'subA', name: 'bash', arguments: '{}', timestamp: 4001 })
  assert.equal(st().focusedAgent?.runId, 'subA', '审批自动聚焦')
  // B 也 hydrate（缓存会话）
  mod.hydrateCheckpoint(B, { schema_version: 1, blocks: [{ kind: 'user', id: 201, text: 'B 历史', ts: 1 }], run_nodes: [], turns: [], usage: {} }, 1, { revision: 1, live: false })
  st().setActiveSession(B)
  assert.equal(st().focusedAgent, undefined, '切到 B 后主会话视图')
  st().setActiveSession(A)
  assert.equal(st().focusedAgent?.runId, 'subA', '切回 A 恢复子智能体焦点（产品决策）')
  ok('焦点跨会话保留：行为与「只修 hydrated」一致')
}

// ============ S5：残缺 view 带焦点 → 切回必须重新请求，不能拿子智能体视图顶替主会话 ============
// 这是问题 3「切 Session 后子智能体与主会话共用一个窗口」的成因：某个会话的 view 里
// 有 blocks（后台事件/上次访问留下的尾部）但也残留 focusedAgent。旧判据把 blocks>0
// 当成已恢复 → 走缓存分支 → loadViewToTop 把子智能体焦点装回顶层，主会话窗口被它占住；
// 刷新后 views 重建才正常。修好后：未 hydrated → 必须重新请求，落地时 emptyView 重建、
// 焦点不残留。
console.log('S5 残缺 view 带焦点：切回重新请求，陈旧子智能体视图不接管主会话')
{
  seedProjects()
  let store
  const { desktop, calls, emitLine } = makeDesktop({
    onCommand: (cmd, { emit, list }) => {
      if (cmd.type !== 'new_session') { emit(cmd.id, {}); return { ok: true } }
      const want = String(cmd.payload?.id ?? '')
      emit(cmd.id, { session_id: want, sessions: list(), todos: [], ...cp(want) })
      return { ok: true }
    },
  })
  install(desktop)
  const mod = freshStore('s5')
  store = mod.useAppStore
  const st = await boot(store)
  assert.equal(st().activeSessionId, A)
  emitLine({ event_type: 'agent_start', session_id: A, run_id: 'subA', parent_run_id: 'mainA', depth: 1, name: '子任务', task_id: 'taskA', timestamp: 4000 })
  emitLine({ event_type: 'tool_approval_requested', session_id: A, id: 'ap5', run_id: 'subA', name: 'bash', arguments: '{}', timestamp: 4001 })
  assert.equal(st().focusedAgent?.runId, 'subA', '审批自动聚焦')
  // B 曾经 hydrate 过（有 blocks），但这次只剩残缺尾部；焦点是上次访问留下的 subA。
  mod.hydrateCheckpoint(B, { schema_version: 1, blocks: [{ kind: 'user', id: 201, text: 'B 历史', ts: 1 }], run_nodes: [], turns: [], usage: {} }, 1, { revision: 1, live: false })
  mod.useAppStore.setState({
    views: {
      ...st().views,
      [B]: { ...st().views[B], hydrated: false, focusedAgent: { nodeId: 'subA', runId: 'subA' } },
    },
  })
  const before = calls.filter((c) => c.type === 'new_session' && c.sid === B).length
  st().setActiveSession(B)
  await sleep(120)
  const after = calls.filter((c) => c.type === 'new_session' && c.sid === B).length
  assert(after > before, 'B 有 blocks 但未 hydrated → 必须重新请求 new_session')
  assert.equal(st().views[B]?.hydrated, true, 'B 完整落地后 hydrated=true')
  assert.equal(st().focusedAgent, undefined, '落地后不得残留上次访问的子智能体焦点')
  ok('残缺 view 的陈旧焦点不接管主会话；完整落地后回主会话')
}

console.log(`\n全部通过（${pass} 项断言组）`)
process.exit(0)
