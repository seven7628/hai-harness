// bridge 启动闸门回归（2026-11-15）：
//
//   T1 bridge **从未启动过**（bridgeStatus='idle'）时的「bridge 未运行」⇒
//      不得 latch 成 error、不得出错误 toast。
//      场景：首次启动的开屏页——用户还没选工作区，bridge 压根没起；此时
//      SkillsPage 挂载就发 skills/market_list，老实现直接回「bridge 未运行」
//      ⇒ 前端 looksBridgeDown() 命中 ⇒ bridgeStatus 被 latch 成 error
//      ⇒ 开屏页红字显示「bridge 未运行」（用户视角：还没做任何事就报 bridge 挂了）。
//      主进程侧已改成「排队而不是回错」（electron/main.ts 的 bridge 生命周期闸门），
//      这里钉的是**前端兜底**：即使还有别的路径漏进来，idle 期间也不标红。
//
//   T2 bridge **已经跑起来**后失败 ⇒ 仍必须 latch 成 error + toast。
//      （防「把真故障也一起吞掉」的回归：bridge 真死时用户必须看得见。）
//
//   T3 bridgeStatus='starting'（正在 spawn）⇒ 不 latch（老实现已有，钉住不回归）。
//
// 用法：npm run test:bridge-boot-gate
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

const dir = mkdtempSync(join(tmpdir(), 'bridge-boot-gate-'))
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

const WS = '/ws-gate'
// makeDesktop：desktop 宿主替身。failTypes 里的命令类型回「bridge 未运行」，
// 其余正常回成功（boot 期间的 list 必须成功，否则 boot 自己就会把 bridge 标红）。
function makeDesktop({ failTypes = [] } = {}) {
  const fail = new Set(failTypes)
  const F = { cb: null }
  const emitLine = (obj) => { if (F.cb) F.cb(JSON.stringify(obj)) }
  const calls = []
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
      calls.push({ type: cmd.type, id: cmd.id })
      if (fail.has(cmd.type)) return { ok: false, error: 'bridge 未运行' }
      if (cmd.type === 'list') {
        emitLine({ event_type: 'command_response', id: cmd.id, ok: true, data: { sessions: [] }, workspace: WS })
        return { ok: true }
      }
      emitLine({ event_type: 'command_response', id: cmd.id, ok: true, data: {}, workspace: WS })
      return { ok: true }
    },
    onEvent: (cb) => { F.cb = cb; return () => { F.cb = null } },
    onExit: () => () => {},
  }
  return { desktop, F, calls }
}
function install(desktop) {
  global.window = {
    localStorage: global.localStorage,
    matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
    addEventListener() {}, removeEventListener() {}, desktop,
  }
}
function seedProjects() {
  LS.clear()
  LS.set('go-code.projects', JSON.stringify({ paths: [WS], active: WS, lastSession: { path: WS, id: 'sA' } }))
}
let pass = 0
function ok(label) { pass++; console.log(`  ✅ ${label}`) }

// ============ T1：idle（bridge 从未启动）⇒ 不 latch ============
console.log('T1 bridge 未启动过（idle）时的「bridge 未运行」不得 latch 成 error')
{
  seedProjects()
  const { desktop } = makeDesktop({ failTypes: ["market_list"] })
  install(desktop)
  const store = freshStore('t1').useAppStore
  const st = () => store.getState()
  assert.equal(st().bridgeStatus, 'idle', '初始应为 idle')
  // 起一个命令（开屏时 SkillsPage 会发）——bridge 未运行 ⇒ 合成一条失败回执。
  st().fetchMarkets()
  await sleep(200)
  assert.notEqual(st().bridgeStatus, 'error', 'idle 期间不得 latch 成 error')
  assert.equal(st().bridgeError, undefined, 'idle 期间不得写 bridgeError')
  assert.notEqual(st().toast?.kind, 'error', 'idle 期间不得弹错误 toast')
  ok('bridge 未启动过 ⇒ 「bridge 未运行」不进错误态、不出红字')
}

// ============ T2：running 后失败 ⇒ 仍要 latch ============
console.log('T2 bridge 已经跑起来后失败 ⇒ 必须 latch 成 error（真故障不能被吞）')
{
  seedProjects()
  const { desktop } = makeDesktop({ failTypes: ["market_list"] })
  install(desktop)
  const store = freshStore('t2').useAppStore
  const st = () => store.getState()
  await st().start()
  await sleep(150)
  assert.equal(st().bridgeStatus, 'running', 'boot 后应为 running')
  st().fetchMarkets()
  await sleep(200)
  assert.equal(st().bridgeStatus, 'error', 'running 期间的 bridge 失败必须 latch 成 error')
  assert.equal(st().toast?.kind, 'error', '应有错误 toast')
  assert(String(st().toast?.msg).includes('bridge'), 'toast 文案包含 bridge')
  ok('bridge 真挂了 ⇒ 仍然可见（横幅 + toast），没被一起吞掉')
}

// ============ T3：starting ⇒ 不 latch ============
console.log('T3 bridgeStatus=starting ⇒ 不 latch')
{
  seedProjects()
  const { desktop } = makeDesktop({ failTypes: ["market_list"] })
  install(desktop)
  const store = freshStore('t3').useAppStore
  const st = () => store.getState()
  await st().start()
  await sleep(150)
  // 模拟「还在 starting 时命令失败」的窗口：手动把状态打回 starting 再发命令。
  store.setState({ bridgeStatus: 'starting', bridgeError: undefined })
  st().fetchMarkets()
  await sleep(200)
  assert.notEqual(st().bridgeStatus, 'error', 'starting 期间不得 latch 成 error')
  ok('starting 期间不标红（老行为保持）')
}

console.log(`\n全部通过（${pass} 项断言组）`)
