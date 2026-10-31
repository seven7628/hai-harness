// 恢复/激活相关 store 行为回归测试（真实 store + esbuild 打包，Node 直接跑，无需浏览器/vitest）。
//
// 覆盖（2026-08-22 三个问题）：
//   T1 思考计时结算：纯「思考→工具调用」轮（无 content_chunk）在 llm_end 后 thinkMs 必须落地
//      —— 否则 Thinking 组件 `thinkStart!=null && thinkMs==null` 判据恒真，计时器永不停止。
//   T2 截断日志恢复：快照以 reasoning_chunk 结尾（会话中断）→ 归一化强制结算思考计时
//      （防恢复后「思考 · Ns…」幽灵计时器），且流式态复位。
//   T3 lastSession 单调守卫：实例 B 新建会话后，实例 A 的旧状态持久化不得把 lastSession
//      写回旧会话（跨实例/过期回写回归 —— 重启后恢复错会话、活跃标记错位的根因）。
//   T4 客户端新建会话：激活标记落在新会话（activeSessionId==新 id、出现在项目列表、
//      lastSession 同步更新）。
//   T5 启动恢复：恢复的会话被激活且在列表内（标记有行可亮）。
//
// 用法：npm run test:fixes
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync, copyFileSync, writeFileSync } from 'node:fs'
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

// —— 假 bridge（列表/快照/新建协议；可注入延迟）——
const F = { sessions: new Map(), events: new Map(), ws: '/ws-x', cb: null, delayFor: null, seq: 0 }
function emitResp(id, data) {
  if (F.cb) F.cb(JSON.stringify({ event_type: 'command_response', id, ok: true, data, workspace: F.ws }))
}
function seedSession(sid, events) {
  F.sessions.set(sid, { id: sid, title: sid.toUpperCase(), updated_at: Date.now() })
  F.events.set(sid, events)
}
function seedEvents(sid) {
  const evs = []
  const push = (o) => evs.push({ session_id: sid, timestamp: '2026-08-22T12:00:00', ...o })
  push({ event_type: 'agent_start', run_id: 'r1' })
  push({ event_type: 'llm_start', run_id: 'r1', model: 'm1', request_id: 'req1' })
  for (let i = 0; i < 3; i++) push({ event_type: 'reasoning_chunk', run_id: 'r1', request_id: 'req1', content: `t${i} ` })
  push({ event_type: 'llm_end', run_id: 'r1', request_id: 'req1', model: 'm1', finish_reason: 'tool_call', tool_calls: [{ id: 'c1', name: 'bash', arguments: '{}' }], usage: { Input: 1, Output: 1 } })
  push({ event_type: 'tool_run_start', run_id: 'r1', request_id: 'req1', id: 'c1', name: 'bash', arguments: 'ls' })
  push({ event_type: 'tool_run_end', run_id: 'r1', request_id: 'req1', id: 'c1', name: 'bash', arguments: 'ls', result: 'ok' })
  evs.push({ event_type: 'agent_end', run_id: 'r1', finish_reason: 'stop', duration_ms: 1000, timestamp: '2026-08-22T12:00:00', session_id: sid })
  return evs
}
function makeDesktop(tag) {
  return {
    startBridge: async () => ({ ok: true }),
    pickWorkspace: async () => null, homeDir: async () => '/tmp', createWorkspaceFolder: async () => ({ ok: false }),
    settingsGet: async () => null, settingsSet: async () => null,
    externalSkillsStatus: async () => null, importExternalSkills: async () => ({ ok: true }),
    pickApp: async () => null, providerSave: async () => ({ ok: true }), providerRemove: async () => ({ ok: true }), providerKeyStatus: async () => false, mcpSave: async () => ({ ok: true }),
    fsList: async () => ({ ok: true, entries: [] }), fsReadRef: async () => ({ ok: true, content: '' }),
    pickFsFile: async () => null, pickFsDir: async () => null,
    command: async (cmd) => {
      const delay = F.delayFor ? F.delayFor(cmd) : 0
      const respond = () => {
        if (cmd.type === 'list') emitResp(cmd.id, { sessions: [...F.sessions.values()] })
        else if (cmd.type === 'new_session') {
          const want = String(cmd.payload?.id ?? '')
          const existing = want ? F.sessions.get(want) : undefined
          if (existing) {
            const snapEvents = (F.events.get(want) || []).map(({ session_id, ...rest }) => rest)
            emitResp(cmd.id, { session_id: want, sessions: [...F.sessions.values()], todos: [], live: false, restored: 'snapshot', snapshot: { events: snapEvents } })
          } else {
            const sid = want || `${tag}-new-${++F.seq}`
            F.sessions.set(sid, { id: sid, title: '新会话', updated_at: Date.now() })
            F.events.set(sid, [])
            emitResp(cmd.id, { session_id: sid, sessions: [...F.sessions.values()], todos: [], live: false })
          }
        } else if (cmd.type === 'skills') emitResp(cmd.id, { skills: [] })
        else emitResp(cmd.id, {})
      }
      if (delay > 0) setTimeout(respond, delay)
      else respond()
      return { ok: true }
    },
    onEvent: (cb) => { F.cb = cb; return () => { F.cb = null } },
    onExit: () => () => {},
  }
}

// —— 打包 store（两份独立副本，模拟两个「实例」共享 localStorage）——
const dir = mkdtempSync(join(tmpdir(), 'store-fixes-'))
async function bundleTo(name) {
  const outfile = join(dir, name)
  await build({
    entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
    bundle: true, format: 'cjs', platform: 'node', outfile, logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' }, // mock.ts 的 ?raw/?url 资源

  })
  return outfile
}
const bundleA = await bundleTo('store-a.cjs')
const bundleB = await bundleTo('store-b.cjs')
// 每个场景用独立模块副本（require 缓存会让第二个 start() 直接跳过 —— 模拟真实刷新重置）
let freshSeq = 0
function freshStore(tag, src = bundleA) {
  const out = join(dir, `store-${tag}-${++freshSeq}.cjs`)
  copyFileSync(src, out)
  return require(out).useAppStore
}

global.window = {
  localStorage: global.localStorage,
  matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
  addEventListener() {}, removeEventListener() {},
  desktop: makeDesktop('instA'),
}
global.document = { documentElement: { classList: { add() {}, remove() {} } } }

function bootSeed() {
  LS.set('go-code.projects', JSON.stringify({ paths: [F.ws], active: F.ws, lastSession: { path: F.ws, id: 'sA' } }))
}
function markerInfo(st) {
  const proj = st.projects.find((p) => p.path === st.workspace)
  return {
    activeSessionId: st.activeSessionId,
    markedInList: (proj?.sessions ?? []).some((x) => x.id === st.activeSessionId),
    listIds: (proj?.sessions ?? []).map((x) => x.id),
  }
}
const persisted = () => JSON.parse(LS.get('go-code.projects'))

let pass = 0
function ok(label) { pass++; console.log(`  ✅ ${label}`) }

// ============ T1：纯思考→工具轮 thinkMs 落地 ============
console.log('T1 纯「思考→工具调用」轮（无正文）思考计时结算')
{
  F.sessions.clear(); F.events.clear(); F.seq = 0
  seedSession('sA', seedEvents('sA'))
  bootSeed()
  const storeA = freshStore('t1')
  await storeA.getState().start()
  await sleep(30)
  const st = storeA.getState()
  const v = st.views['sA']
  assert(v, 'sA 视图应已恢复')
  const pureTool = v.blocks.find((b) => b.kind === 'assistant' && b.thinking && !b.text)
  assert(pureTool, '应存在纯思考块（思考→工具轮）')
  assert(pureTool.thinkMs != null, `纯工具轮 thinkMs 应已结算，got ${pureTool.thinkMs}`)
  assert(pureTool.thinkStart != null, 'thinkStart 应有')
  assert.strictEqual(pureTool.streaming, false)
  ok(`thinkMs=${pureTool.thinkMs}ms 已结算（timer 停止，不再显示 · Ns…）`)
}

// ============ T2：截断日志（reasoning 结尾）恢复归一化 ============
console.log('T2 截断/中断会话恢复：思考计时强制落地 + 流式态复位')
{
  F.sessions.clear(); F.events.clear(); F.seq = 0
  const sid = 's-cut'
  const evs = []
  const push = (o) => evs.push({ session_id: sid, timestamp: '2026-08-22T12:00:00', ...o })
  push({ event_type: 'agent_start', run_id: 'r1' })
  push({ event_type: 'llm_start', run_id: 'r1', model: 'm1', request_id: 'req1' })
  push({ event_type: 'reasoning_chunk', run_id: 'r1', request_id: 'req1', content: 'cut thinking ' })
  // 无 llm_end —— 会话在流式中被关闭（截断日志）
  seedSession(sid, evs)
  bootSeed()
  LS.set('go-code.projects', JSON.stringify({ paths: [F.ws], active: F.ws, lastSession: { path: F.ws, id: sid } }))
  const storeA = freshStore('t2')
  await storeA.getState().start()
  await sleep(30)
  const v = storeA.getState().views[sid]
  assert(v, '截断会话视图应已恢复')
  const b = v.blocks.find((x) => x.kind === 'assistant')
  assert(b, '截断会话应有 assistant 块')
  assert.strictEqual(b.streaming, false, '恢复后 streaming 应复位')
  assert(b.thinkMs != null, `截断思考应被归一化结算，got ${b.thinkMs}`)
  ok(`thinkMs=${b.thinkMs}ms（无 llm_end 也停止计时）`)
}

// ============ T3：lastSession 单调守卫（跨实例回归） ============
console.log('T3 lastSession 单调守卫：旧实例持久化不得覆盖更新的最近会话')
{
  F.sessions.clear(); F.events.clear(); F.seq = 0
  seedSession('sA', seedEvents('sA'))
  F.sessions.set('sB', { id: 'sB', title: 'SB', updated_at: 1000 })
  F.events.set('sB', [])
  bootSeed()
  // 实例 A：启动恢复 sA → 新建会话 N1
  global.window.desktop = makeDesktop('instA')
  const storeA = freshStore('t3a')
  await storeA.getState().start()
  await sleep(20)
  await storeA.getState().newSession()
  await sleep(30)
  const n1 = storeA.getState().activeSessionId
  assert(n1 && n1.endsWith('-1'), `实例 A 应激活新会话，got ${n1}`)
  // 实例 B：启动（恢复 N1）→ 新建会话 N2（更新的 lastSession）
  global.window.desktop = makeDesktop('instB')
  const storeB = freshStore('t3b', bundleB)
  await storeB.getState().start()
  await sleep(20)
  await storeB.getState().newSession()
  await sleep(30)
  const n2 = storeB.getState().activeSessionId
  assert(n2 && n2.endsWith('-2'), `实例 B 应激活新会话，got ${n2}`)
  const before = persisted()
  assert.strictEqual(before.lastSession.id, n2, 'B 的新会话应写入 lastSession')
  assert(typeof before.lastSessionAt === 'number', 'lastSessionAt 应写入')
  // 实例 A（其活跃仍是较旧的 N1）做一次持久化触发的操作（置顶）
  storeA.getState().toggleWorkspacePin(F.ws)
  await sleep(20)
  const after = persisted()
  assert.strictEqual(after.lastSession.id, n2, `过期状态不得把 lastSession 写回旧会话，got ${after.lastSession.id}`)
  assert.strictEqual(after.lastSession.id, before.lastSession.id, 'lastSession 保持 B 的新会话')
  ok(`回归防护生效：lastSession 保持 ${n2}（实例 A 的旧激活 ${n1} 未覆盖）`)
}

// ============ T4：客户端新建会话 → 活跃标记 ============
console.log('T4 客户端新建会话：标记落在新会话且持久化同步')
{
  F.sessions.clear(); F.events.clear(); F.seq = 0
  seedSession('sA', seedEvents('sA'))
  bootSeed()
  global.window.desktop = makeDesktop('instA')
  const storeA = freshStore('t4')
  await storeA.getState().start()
  await sleep(20)
  const before = markerInfo(storeA.getState())
  assert.strictEqual(before.activeSessionId, 'sA')
  assert(before.markedInList, '恢复后标记应有行可亮（sA 在列表中）')
  await storeA.getState().newSession()
  await sleep(30)
  const after = markerInfo(storeA.getState())
  assert.strictEqual(after.activeSessionId, 'instA-new-1')
  assert(after.markedInList, `新会话应被标记，markedInList=${after.markedInList}`)
  assert(after.listIds.includes('instA-new-1'), '新会话应出现在项目列表')
  assert.strictEqual(persisted().lastSession.id, 'instA-new-1', 'lastSession 应同步到新会话')
  ok(`新会话标记 + lastSession 同步（activeSessionId=${after.activeSessionId}）`)
}

// ============ T5：启动自动恢复 ============
console.log('T5 启动自动恢复最近会话')
{
  F.sessions.clear(); F.events.clear(); F.seq = 0
  seedSession('sA', seedEvents('sA'))
  seedSession('sB', [])
  bootSeed()
  global.window.desktop = makeDesktop('instA')
  const storeA = freshStore('t5')
  await storeA.getState().start()
  await sleep(30)
  const st = storeA.getState()
  assert.strictEqual(st.activeSessionId, 'sA', '应自动恢复 lastSession 指向的会话')
  assert(markerInfo(st).markedInList, '恢复后活跃标记可亮')
  assert(st.views['sA']?.blocks.length > 0, '恢复视图应有内容')
  ok(`自动恢复 ${st.activeSessionId}（blocks=${st.views['sA']?.blocks.length}）`)
}

console.log(`\n全部通过（${pass} 项断言组）`)
