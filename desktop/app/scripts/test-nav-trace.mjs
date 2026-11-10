// 导航代际丢弃 trace 回归（FE-03）。
//
// 背景：延迟响应按导航代际丢弃是**正确**的（否则用户切走后旧工作区的会话会覆盖当前界面），
// 但丢弃一直是静默的 —— 用户看到的是「历史不出来 / 骨架屏超时后空白」，日志里查不到一条
// 「这条响应被丢了、属于哪一代、和哪一端对不上」。而且同一个判定布尔把两类完全不同的事
// 混在一起：**过期**（有更新的导航取代了它，正常）与**不一致**（代际对得上但 workspace /
// session_id 与发起时不符，是串台/身份错配的真异常）。
//
// 覆盖：
//   T1 纯判定 classifyNavDiscard：stale vs inconsistent 的顺序与措辞（含 attempt / 无登记）
//   T2 store 现场 stale：扣住恢复响应 → 另一次导航 → 迟到响应按 stale 丢弃并记录；
//      行为不变（不重建视图、仍走恢复重试，不把丢弃变成用户可见错误）
//   T3 store 现场 inconsistent：代际对得上但响应 workspace 与当前不符 → 记成 inconsistent
//   T4 正常响应不产生任何丢弃记录（trace 不能污染 happy path）
//   T5 环形缓冲有界（不随会话长跑无界增长）
//
// 用法：npm run test:nav-trace
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

const dir = mkdtempSync(join(tmpdir(), 'nav-trace-'))
let pass = 0

// ============ T1：纯判定（不启动 store）============
console.log('T1 classifyNavDiscard：stale 与 inconsistent 必须可分')
{
  const out = join(dir, 'navTrace.cjs')
  await build({
    entryPoints: [join(__dirname, '../src/store/navTrace.ts')],
    bundle: true, format: 'cjs', platform: 'node', outfile: out, logLevel: 'silent',
  })
  const { classifyNavDiscard, recordNavDiscard, navDiscards, clearNavDiscards, NAV_DISCARD_LIMIT } = require(out)

  const base = { workspace: '/ws/a', generation: 5, sessionId: 's1' }

  // 当前导航：允许应用
  assert.deepEqual(classifyNavDiscard({ ...base, metaGeneration: 5 }), { current: true })
  assert.deepEqual(
    classifyNavDiscard({ ...base, metaGeneration: 5, responseWorkspace: '/ws/a', expectedSession: 's1' }),
    { current: true },
  )

  // 代际被取代 = stale（正常现象）
  const stale = classifyNavDiscard({ ...base, metaGeneration: 3, responseWorkspace: '/ws/b' })
  assert.equal(stale.current, false)
  assert.equal(stale.kind, 'stale', '代际不符必须是 stale（先判取代，再判身份）')
  assert.match(stale.reason, /过期\(stale\).*3 → 5/)

  // 陈旧 attempt = stale（恢复协议的第二个取代来源）
  const attempt = classifyNavDiscard({ ...base, metaGeneration: 5, staleAttempt: true })
  assert.equal(attempt.kind, 'stale')
  assert.match(attempt.reason, /attempt/)

  // 代际对得上、workspace 不符 = inconsistent（真异常）
  const wsMismatch = classifyNavDiscard({ ...base, metaGeneration: 5, responseWorkspace: '/ws/b' })
  assert.equal(wsMismatch.kind, 'inconsistent')
  assert.match(wsMismatch.reason, /不一致\(inconsistent\).*workspace=\/ws\/b.*\/ws\/a/)

  // 代际对得上、session 不符 = inconsistent
  const sidMismatch = classifyNavDiscard({ ...base, metaGeneration: 5, expectedSession: 's0' })
  assert.equal(sidMismatch.kind, 'inconsistent')
  assert.match(sidMismatch.reason, /session_id=s1.*s0/)

  // 没有在途登记 = inconsistent（无从证明它属于当前导航）
  const orphan = classifyNavDiscard({ ...base })
  assert.equal(orphan.kind, 'inconsistent')
  assert.match(orphan.reason, /在途登记/)

  // 环形缓冲有界：写超上限后只留最近 NAV_DISCARD_LIMIT 条，且是最近的
  clearNavDiscards()
  const origDebug = console.debug
  console.debug = () => {} // 这一段只为验证封顶，不必刷屏
  for (let i = 0; i < NAV_DISCARD_LIMIT + 10; i++) {
    recordNavDiscard({ at: i, kind: 'stale', type: 't', reason: 'r', sid: `s${i}`, generation: i })
  }
  console.debug = origDebug
  const buf = navDiscards()
  assert.equal(buf.length, NAV_DISCARD_LIMIT, '记录必须封顶（诊断不能无界增长）')
  assert.equal(buf[buf.length - 1].sid, `s${NAV_DISCARD_LIMIT + 9}`, '保留的应是最近若干条')
  pass++
  console.log(`  ✅ 判定四态 + 缓冲上限（${NAV_DISCARD_LIMIT}）`)
}

// ============ store 现场 ============
const LS = new Map()
global.localStorage = {
  getItem: (k) => (LS.has(k) ? LS.get(k) : null),
  setItem: (k, v) => LS.set(k, String(v)),
  removeItem: (k) => LS.delete(k),
}
global.document = { documentElement: { classList: { add() {}, remove() {} } } }

const WS = '/ws-nav-trace'
const SID = 'sNav'

const storeBundle = join(dir, 'store.cjs')
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true, format: 'cjs', platform: 'node', outfile: storeBundle, logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' },
})
let seq = 0
function freshStore(tag) {
  const out = join(dir, `store-${tag}-${++seq}.cjs`)
  copyFileSync(storeBundle, out)
  return require(out)
}

// 一段最小历史（3 轮）—— 用来证明「丢弃后仍能自动重试落地」（行为未变）。
function seedEvents(sid) {
  const evs = []
  const push = (o) => evs.push({ session_id: sid, timestamp: '2026-09-21T21:53:00', ...o })
  push({ event_type: 'agent_start', run_id: 'r1' })
  for (let i = 0; i < 3; i++) {
    push({ event_type: 'llm_start', run_id: 'r1', model: 'm1', request_id: `req${i}` })
    push({
      event_type: 'llm_end', run_id: 'r1', request_id: `req${i}`, model: 'm1',
      finish_reason: 'stop', content: `ok${i}`,
      usage: { Input: 1000, Output: 10, CacheRead: 0, CacheWrite: 0, Reasoning: 0 },
    })
  }
  push({ event_type: 'agent_end', run_id: 'r1', finish_reason: 'stop', duration_ms: 1000 })
  return evs
}

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
          if (scenario.dropBareNewSession) return { ok: true }
          const sid = `${tag}-new-${++F.restoreCalls}`
          F.sessions.set(sid, { id: sid, title: '新', updated_at: Date.now() })
          F.events.set(sid, [])
          emit(cmd.id, { session_id: sid, sessions: list(), todos: [], live: false }, ws)
          return { ok: true }
        }
        F.restoreCalls++
        const ctx = {
          call: F.restoreCalls,
          // respondWs：响应回显的 workspace（T3 用它制造「代际对得上但 workspace 不符」）
          respondSnapshot: (respondWs = ws) =>
            emit(cmd.id, {
              session_id: want, sessions: list(), todos: [], live: false, restored: 'snapshot',
              snapshot: { events: (F.events.get(want) ?? []).map(({ session_id, ...rest }) => rest) },
            }, respondWs),
          respondEmpty: () => emit(cmd.id, { session_id: want, sessions: list(), todos: [], live: false }, ws),
        }
        scenario.onNewSession(ctx)
        return { ok: true }
      }
      emit(cmd.id, {}, ws)
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
function installWindow(desktop) {
  global.window = {
    localStorage: global.localStorage,
    matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
    addEventListener() {}, removeEventListener() {}, desktop,
  }
}

const ok = (label) => { pass++; console.log(`  ✅ ${label}`) }

// ============ T2：stale（代际被取代）============
console.log('T2 stale：迟到响应按代际丢弃并记 trace（行为不变）')
{
  LS.clear()
  let held = null
  const { desktop, F } = makeDesktop('t2', {
    dropBareNewSession: true, // 制造「代际已变、另一条 new_session 永不落地」的竞态
    onNewSession: (c) => { if (c.call === 1) held = c; else c.respondSnapshot() },
  })
  F.sessions.set(SID, { id: SID, title: 'R', updated_at: Date.now() })
  F.events.set(SID, seedEvents(SID))
  installWindow(desktop)
  bootSeed()
  const mod = freshStore('t2')
  const store = mod.useAppStore
  await store.getState().start()
  await sleep(60)
  assert(held, '首个恢复响应应被扣住')

  store.getState().newSession() // 另一次导航 → 代际 +1
  await sleep(20)
  held.respondSnapshot() // 迟到响应：按陈旧丢弃
  await sleep(60)

  const recs = mod.__navDiscards()
  assert.equal(recs.length >= 1, true, '陈旧响应必须留下 trace（旧实现完全静默）')
  const stale = recs.find((r) => r.sid === SID && r.kind === 'stale')
  assert(stale, `应记一条 stale（实际：${JSON.stringify(recs)}）`)
  assert.equal(stale.type, 'new_session')
  assert.match(stale.reason, /过期\(stale\)/)
  assert.equal(typeof stale.metaGeneration, 'number', 'trace 必须带发起代际')
  assert.equal(typeof stale.generation, 'number', 'trace 必须带当前代际')
  assert.equal(stale.metaGeneration < stale.generation, true, '发起代际应小于当前代际')

  // 行为不变 1：丢弃不得重建视图
  const dropped = store.getState().views[SID]
  assert(!dropped || dropped.blocks.length === 0, '陈旧响应不得重建视图')
  // 行为不变 2：不把丢弃变成用户可见错误
  assert.equal(store.getState().toast, null, 'stale 丢弃不得弹 toast')
  // 行为不变 3：仍走恢复重试（scheduleRestoreRetry 的既有语义）
  await sleep(1200)
  const v = store.getState().views[SID]
  assert(v && v.blocks.length > 0, '陈旧丢弃后应自动重试并落地')
  assert.equal(v.turns.length, 3, '重试后回合数应恢复')
  ok('stale 记录齐全（kind/reason/代际/sid），视图未被覆盖、无 toast、重试照旧')
}

// ============ T3：inconsistent（代际对得上但 workspace 不符）============
console.log('T3 inconsistent：代际一致但响应 workspace 不符 → 记为不一致')
{
  LS.clear()
  const { desktop, F } = makeDesktop('t3', {
    onNewSession: (c) => (c.call === 1 ? c.respondSnapshot('/ws-OTHER') : c.respondSnapshot()),
  })
  F.sessions.set(SID, { id: SID, title: 'R', updated_at: Date.now() })
  F.events.set(SID, seedEvents(SID))
  installWindow(desktop)
  bootSeed()
  const mod = freshStore('t3')
  const store = mod.useAppStore
  await store.getState().start()
  await sleep(120)
  const recs = mod.__navDiscards()
  const bad = recs.find((r) => r.kind === 'inconsistent')
  assert(bad, `workspace 不符必须记成 inconsistent（实际：${JSON.stringify(recs)}）`)
  assert.match(bad.reason, /不一致\(inconsistent\)/)
  assert.equal(bad.responseWorkspace, '/ws-OTHER', 'trace 必须带响应端 workspace')
  assert.equal(bad.workspace, WS, 'trace 必须带当前 workspace')
  assert.equal(bad.metaGeneration, bad.generation, '不一致的判据正是「代际相同却对不上」')
  assert.equal(store.getState().toast, null, '不一致也不升级成用户可见错误（保持原有行为）')
  ok('inconsistent 与 stale 在同一缓冲里可区分（代际相同、workspace 不同）')
}

// ============ T4：正常响应不产生 trace ============
console.log('T4 正常恢复：不产生任何丢弃记录')
{
  LS.clear()
  const { desktop, F } = makeDesktop('t4', { onNewSession: (c) => c.respondSnapshot() })
  F.sessions.set(SID, { id: SID, title: 'R', updated_at: Date.now() })
  F.events.set(SID, seedEvents(SID))
  installWindow(desktop)
  bootSeed()
  const mod = freshStore('t4')
  const store = mod.useAppStore
  await store.getState().start()
  await sleep(120)
  const v = store.getState().views[SID]
  assert(v && v.turns.length === 3, '正常恢复应落地 3 轮')
  assert.deepEqual(mod.__navDiscards(), [], 'happy path 不得写 trace')
  ok('正常恢复：无丢弃记录（trace 零污染）')
}

console.log(`\n全部通过：${pass} 组`)
