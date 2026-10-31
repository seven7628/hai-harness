// 会话恢复落地重试回归（2026-09-21）。
//
// 背景（真实故障，逐位对账过）：刷新/重启后恢复响应没被应用时，会话视图会从「空」重建 ——
// 历史与累计（回合 / in-out / 缓存 / cost）只剩重载之后的部分，且全程静默。
// 用户实测：280 轮 / 80M / $0.51 的会话在渲染进程重载后 footbar 只显示最后 4 轮
// （in 1.9M / out 3k / 缓存 99.89% / cost $0.0081），而 record 里数据完好。
//
// 覆盖（store 侧修复：scheduleRestoreRetry）：
//   T1 响应「无历史数据」→ 800ms 后自动重试，第二次落地（blocks/turns/usage 齐全，无误报 toast）
//   T2 响应按陈旧导航丢弃（刷新竞态：代际已变但目标仍是该会话）→ 自动重试并落地
//   T3 hydrate 抛错 → 重试到上限后**明示**（console.error + toast），不再静默留空视图
//   T4 正常恢复（checkpoint/snapshot 落地）→ 不重试、不误报
//
// 用法：npm run test:restore-retry
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

// —— 共享 localStorage（跨「实例」= 跨模块副本）——
const LS = new Map()
global.localStorage = {
  getItem: (k) => (LS.has(k) ? LS.get(k) : null),
  setItem: (k, v) => LS.set(k, String(v)),
  removeItem: (k) => LS.delete(k),
}

const WS = '/ws-restore-retry'
const SID = 'sR'

// seedEvents 一段最小真实历史：3 轮 llm_end（带 usage）→ 恢复后 turns/usage 都应有值。
function seedEvents(sid) {
  const evs = []
  const push = (o) => evs.push({ session_id: sid, timestamp: '2026-09-21T21:53:00', ...o })
  push({ event_type: 'agent_start', run_id: 'r1' })
  for (let i = 0; i < 3; i++) {
    push({ event_type: 'llm_start', run_id: 'r1', model: 'm1', request_id: `req${i}` })
    push({
      event_type: 'llm_end', run_id: 'r1', request_id: `req${i}`, model: 'm1',
      finish_reason: 'stop', content: `ok${i}`,
      usage: { Input: 1000 + i, Output: 10 + i, CacheRead: 900, CacheWrite: 0, Reasoning: 0 },
    })
  }
  push({ event_type: 'agent_end', run_id: 'r1', finish_reason: 'stop', duration_ms: 1000 })
  return evs
}

// —— 打包 store（每个场景一份独立副本：清掉模块级状态，等价于一次真实刷新）——
const dir = mkdtempSync(join(tmpdir(), 'store-restore-retry-'))
const bundle = join(dir, 'store.cjs')
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true, format: 'cjs', platform: 'node', outfile: bundle, logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' }, // mock.ts 的 ?raw/?url 资源

})
let freshSeq = 0
function freshStore(tag) {
  const out = join(dir, `store-${tag}-${++freshSeq}.cjs`)
  copyFileSync(bundle, out)
  return require(out).useAppStore
}

// —— 假 bridge：new_session 的响应由场景脚本决定（丢弃/无数据/坏 checkpoint/正常）——
// scenario.onNewSession(call, ctx)：call = 第几次带 id 的恢复请求；ctx.respond/ctx.hold/ctx.drop
function makeDesktop(tag, scenario) {
  const F = { cb: null, sessions: new Map(), events: new Map(), restoreCalls: 0 }
  const emit = (id, data, workspace) => {
    if (F.cb) F.cb(JSON.stringify({ event_type: 'command_response', id, ok: true, data, workspace }))
  }
  const list = () => [...F.sessions.values()]
  const desktop = {
    startBridge: async () => ({ ok: true }),
    pickWorkspace: async () => null, homeDir: async () => '/tmp', createWorkspaceFolder: async () => ({ ok: false }),
    settingsGet: async () => null, settingsSet: async () => null,
    externalSkillsStatus: async () => null, importExternalSkills: async () => ({ ok: true }),
    pickApp: async () => null, providerSave: async () => ({ ok: true }), providerRemove: async () => ({ ok: true }),
    providerKeyStatus: async () => false, mcpSave: async () => ({ ok: true }),
    fsList: async () => ({ ok: true, entries: [] }), fsReadRef: async () => ({ ok: true, content: '' }),
    pickFsFile: async () => null, pickFsDir: async () => null,
    command: async (cmd) => {
      const ws = String(cmd.payload?.workspace ?? WS)
      if (cmd.type === 'list') {
        emit(cmd.id, { sessions: list() }, ws)
        return { ok: true }
      }
      if (cmd.type === 'new_session') {
        const want = String(cmd.payload?.id ?? '')
        if (!want) {
          // 不带 id = 真新建：本测试里只用于 T2 制造「代际已变但请求永不落地」的竞态
          if (scenario.dropBareNewSession) return { ok: true }
          const sid = `${tag}-new-${++F.restoreCalls}`
          F.sessions.set(sid, { id: sid, title: '新会话', updated_at: Date.now() })
          F.events.set(sid, [])
          emit(cmd.id, { session_id: sid, sessions: list(), todos: [], live: false }, ws)
          return { ok: true }
        }
        F.restoreCalls++
        const ctx = {
          call: F.restoreCalls,
          respond: (extra = {}) => emit(cmd.id, { session_id: want, sessions: list(), todos: [], live: false, ...extra }, ws),
          respondSnapshot: () =>
            emit(cmd.id, {
              session_id: want, sessions: list(), todos: [], live: false, restored: 'snapshot',
              snapshot: { events: (F.events.get(want) ?? []).map(({ session_id, ...rest }) => rest) },
            }, ws),
          respondEmpty: () => emit(cmd.id, { session_id: want, sessions: list(), todos: [], live: false }, ws),
          // blocks 缺失 → hydrateCheckpoint 抛「checkpoint 结构非法」
          respondBadCheckpoint: () =>
            emit(cmd.id, {
              session_id: want, sessions: list(), todos: [], live: false, restored: 'checkpoint',
              degraded: false, revision: 1, view_checkpoint: { schema_version: 1, turns: [], usage: {} },
            }, ws),
        }
        scenario.onNewSession(ctx)
        return { ok: true }
      }
      emit(cmd.id, {}, ws) // skills / ws_git / 其它：空响应即可
      return { ok: true }
    },
    onEvent: (cb) => { F.cb = cb; return () => { F.cb = null } },
    onExit: () => () => {},
  }
  return { desktop, F }
}

function bootSeed() {
  LS.set('go-code.projects', JSON.stringify({ paths: [WS], active: WS, lastSession: { path: WS, id: SID } }))
}
global.document = { documentElement: { classList: { add() {}, remove() {} } } }

let pass = 0
function ok(label) { pass++; console.log(`  ✅ ${label}`) }

// ============ T1：响应无历史数据 → 自动重试落地 ============
console.log('T1 恢复响应无历史数据（bridge 无 record/事件）→ 自动重试')
{
  LS.clear()
  const scenario = {
    onNewSession: (c) => (c.call === 1 ? c.respondEmpty() : c.respondSnapshot()),
  }
  const { desktop, F } = makeDesktop('t1', scenario)
  F.sessions.set(SID, { id: SID, title: 'R', updated_at: Date.now() })
  F.events.set(SID, seedEvents(SID))
  global.window = { localStorage: global.localStorage, matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }), addEventListener() {}, removeEventListener() {}, desktop }
  bootSeed()
  const store = freshStore('t1')
  await store.getState().start()
  await sleep(60)
  const before = store.getState().views[SID]
  assert(!before || before.blocks.length === 0, '首次响应无数据：视图应为空')
  assert.equal(F.restoreCalls, 1, '首次只发一次恢复请求')
  await sleep(1200) // 等 800ms 重试窗口
  const v = store.getState().views[SID]
  assert(v && v.blocks.length > 0, '重试后视图应落地（blocks）')
  assert.equal(v.turns.length, 3, '重试后回合数应恢复（3 轮）')
  assert.equal(store.getState().usage.input, 3003, '重试后累计 usage 应恢复')
  assert.equal(F.restoreCalls, 2, '应恰好重试一次')
  assert.equal(store.getState().toast, null, '无数据场景不应弹 toast（避免空会话误报）')
  ok('无历史数据 → 800ms 内自动重试并完整落地（blocks/回合/usage）')
}

// ============ T2：响应按陈旧导航丢弃（刷新竞态）→ 自动重试落地 ============
console.log('T2 恢复响应按陈旧导航丢弃（刷新竞态）→ 自动重试')
{
  LS.clear()
  let held = null
  const scenario = {
    dropBareNewSession: true, // 竞态制造：代际已变，但另一个 new_session 请求永不落地
    onNewSession: (c) => {
      if (c.call === 1) held = c // 扣住首个恢复响应
      else c.respondSnapshot()
    },
  }
  const { desktop, F } = makeDesktop('t2', scenario)
  F.sessions.set(SID, { id: SID, title: 'R', updated_at: Date.now() })
  F.events.set(SID, seedEvents(SID))
  global.window = { localStorage: global.localStorage, matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }), addEventListener() {}, removeEventListener() {}, desktop }
  bootSeed()
  const store = freshStore('t2')
  await store.getState().start()
  await sleep(60)
  assert(held, '首个恢复响应应已被扣住')
  store.getState().newSession() // 递增导航代际（模拟真实刷新竞态里的另一次导航）
  await sleep(20)
  held.respondSnapshot() // 迟到响应：按陈旧丢弃
  await sleep(60)
  const dropped = store.getState().views[SID]
  assert(!dropped || dropped.blocks.length === 0, '陈旧响应不得重建视图')
  await sleep(1200)
  const v = store.getState().views[SID]
  assert(v && v.blocks.length > 0, '陈旧丢弃后应自动重试并落地')
  assert.equal(v.turns.length, 3, '重试后回合数应恢复')
  assert.equal(F.restoreCalls, 2, '应恰好重试一次')
  ok('陈旧丢弃 → 自动重试，历史与累计落地（不再静默从零开始）')
}

// ============ T3：hydrate 抛错 → 重试到上限后明示 ============
console.log('T3 hydrate 抛错 → 重试上限 + 可见提示')
{
  LS.clear()
  const scenario = { onNewSession: (c) => c.respondBadCheckpoint() }
  const { desktop, F } = makeDesktop('t3', scenario)
  F.sessions.set(SID, { id: SID, title: 'R', updated_at: Date.now() })
  F.events.set(SID, seedEvents(SID))
  global.window = { localStorage: global.localStorage, matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }), addEventListener() {}, removeEventListener() {}, desktop }
  bootSeed()
  const store = freshStore('t3')
  await store.getState().start()
  await sleep(4200) // 800 + 2500 两次重试走完
  assert.equal(F.restoreCalls, 3, '应是「首次 + 2 次重试」共 3 次')
  const t = store.getState().toast
  assert(t && t.kind === 'error' && String(t.msg).includes('恢复'), '达上限必须明示（toast）')
  ok('hydrate 异常 → 有限重试（3 次）后 toast 明示，不再静默留空视图')
}

// ============ T4：正常恢复不重试（回归保护）============
console.log('T4 正常恢复 → 不重试、不误报')
{
  LS.clear()
  const scenario = { onNewSession: (c) => c.respondSnapshot() }
  const { desktop, F } = makeDesktop('t4', scenario)
  F.sessions.set(SID, { id: SID, title: 'R', updated_at: Date.now() })
  F.events.set(SID, seedEvents(SID))
  global.window = { localStorage: global.localStorage, matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }), addEventListener() {}, removeEventListener() {}, desktop }
  bootSeed()
  const store = freshStore('t4')
  await store.getState().start()
  await sleep(1500)
  const v = store.getState().views[SID]
  assert(v && v.turns.length === 3, '正常恢复应完整落地')
  assert.equal(F.restoreCalls, 1, '落地成功不得再发恢复请求')
  assert.equal(store.getState().toast, null, '正常路径不得弹 toast')
  ok('正常恢复 → 单次请求、无重试、无 toast')
}

console.log(`\n全部通过（${pass} 项断言组）`)
