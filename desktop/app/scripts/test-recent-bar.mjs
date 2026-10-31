// 顶部快捷栏（recentSessions）行为回归测试（真实 store + esbuild 打包，Node 直接跑，无需浏览器/vitest）。
//
// 覆盖：
//   R1 空栏新建会话 → 新会话入栏（此前新建不走 setActiveSession，never 入栏）。
//   R2 满栏（10 条）新建会话 → 追加到队尾 + 挤掉 index 0（最久未看），长度仍为 10。
//   R3 点击/快捷键切换会话 → 入栏；重复切换已存在项位置不变（不换位）。
//   R4 关闭（removeRecentSession）→ 仅摘掉快捷项，队列其余顺序不变。
//   R5 删除会话/删除工作区 → 对应快捷项清理（不留悬空项）。
//   R6 持久化 recent 随状态同步（重启后恢复同一队列）。
//
// 用法：npm run test:recent
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

// 1) shim localStorage + window（store 模块加载时读取项目树/偏好；mock transport 走浏览器分支）
const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
}
global.window = {
  localStorage: global.localStorage,
  matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
  addEventListener() {},
  removeEventListener() {},
  desktop: undefined, // 无 Electron → mock transport（browser 分支）
}
global.document = { documentElement: { classList: { add() {}, remove() {} } } }

// 2) esbuild 打包 store（含真实 mock transport）到临时 CJS
const dir = mkdtempSync(join(tmpdir(), 'recent-bar-'))
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
const { useAppStore } = require(outfile)

const WS = '/mock/w'
const ids = () => useAppStore.getState().recentSessions.map((r) => r.id)
const persisted = () => JSON.parse(global.localStorage.getItem('go-code.projects') ?? '{}')
let pass = 0
function ok(label) { pass++; console.log(`  ✅ ${label}`) }

// 每个场景重置到干净状态（mock transport 的 workspace 会话池在 start() 后可用）
async function fresh() {
  useAppStore.setState({
    workspace: WS,
    activeSessionId: undefined,
    sessions: [],
    projects: [{ path: WS, sessions: [] }],
    views: {},
    recentSessions: [],
    titleOverrides: {},
  })
  global.localStorage.removeItem('go-code.projects')
  await useAppStore.getState().start()
  await sleep(40)
}
// 建会话：走真实 newSession()（mock transport 建新会话 + 回 new_session 响应）
async function newSession() {
  await useAppStore.getState().newSession()
  await sleep(80)
  return useAppStore.getState().activeSessionId
}

// ============ R1：空栏新建会话 → 入栏 ============
console.log('R1 空快捷栏：新建会话即入栏')
{
  await fresh()
  const sid = await newSession()
  assert(sid, '新建会话应有 session id')
  assert.deepStrictEqual(ids(), [sid], `新会话应入栏，got ${ids().join(',') || '(空)'}`)
  ok(`新建会话 ${sid} 已入栏`)
}

// ============ R2：满栏新建会话 → 追加队尾 + 挤掉 index 0 ============
console.log('R2 满栏（10 条）：新建会话追加队尾并挤掉 index 0')
{
  await fresh()
  const full = Array.from({ length: 10 }, (_, i) => ({ path: WS, id: `old-${i}` }))
  useAppStore.setState({ recentSessions: full })
  const sid = await newSession()
  const cur = ids()
  assert.strictEqual(cur.length, 10, `队列应保持上限 10，got ${cur.length}`)
  assert.strictEqual(cur[9], sid, `新会话应在队尾，got ${cur[9]}`)
  assert.ok(!cur.includes('old-0'), 'index 0（最久未看）应被挤出')
  assert.deepStrictEqual(cur.slice(0, 9), full.slice(1, 10).map((r) => r.id), '其余项应前移且顺序不变')
  ok(`new=${sid} 入队尾，old-0 挤出（${cur.join(',')}）`)
}

// ============ R3：切换会话入栏 + 重复切换不换位 ============
console.log('R3 切换会话：入栏；重复切换已存在项位置不变')
{
  await fresh()
  const a = await newSession()
  const b = await newSession()
  // 新建后：a 在 index0、b 在 index1（都在栏内）
  assert.deepStrictEqual(ids(), [a, b], `两条新会话应依序入栏，got ${ids().join(',')}`)
  // 切回 a（已存在）→ 位置不变，不因重复切换而换位
  useAppStore.getState().setActiveSession(a, WS)
  await sleep(30)
  assert.deepStrictEqual(ids(), [a, b], `重复切换不得改变队列顺序，got ${ids().join(',')}`)
  assert.strictEqual(useAppStore.getState().activeSessionId, a)
  ok(`切换 ${a} 后顺序保持 [${ids().join(',')}]`)
}

// ============ R4：关闭快捷项 → 仅摘该项 ============
console.log('R4 关闭快捷项：仅移除该项，其余顺序不变')
{
  await fresh()
  const full = Array.from({ length: 4 }, (_, i) => ({ path: WS, id: `s-${i}` }))
  useAppStore.setState({ recentSessions: full })
  useAppStore.getState().removeRecentSession(WS, 's-1')
  assert.deepStrictEqual(ids(), ['s-0', 's-2', 's-3'], `应仅移除 s-1，got ${ids().join(',')}`)
  // 不存在的项 → 幂等（不改变状态）
  useAppStore.getState().removeRecentSession(WS, 'nope')
  assert.deepStrictEqual(ids(), ['s-0', 's-2', 's-3'], '移除不存在项应幂等')
  ok('关闭 s-1 后 [s-0,s-2,s-3]；不存在项幂等')
}

// ============ R5：删除会话/工作区 → 快捷项清理 ============
console.log('R5 删除会话/工作区：快捷栏不留悬空项')
{
  await fresh()
  const keep = await newSession() // 先建一个会保留的会话
  const drop = await newSession() // 待删除
  assert.ok(ids().includes(drop), '待删会话应在栏内')
  useAppStore.getState().deleteSession(WS, drop)
  await sleep(40)
  assert.ok(!ids().includes(drop), `删除会话后不应残留快捷项，got ${ids().join(',')}`)
  assert.ok(ids().includes(keep), '其余会话应保留')
  // 删除工作区 → 同工作区快捷项全部清理
  useAppStore.setState({ recentSessions: [{ path: WS, id: keep }, { path: '/other', id: 'x1' }] })
  useAppStore.getState().deleteWorkspace(WS)
  await sleep(40)
  assert.deepStrictEqual(ids(), ['x1'], `删工作区后仅留其它工作区项，got ${ids().join(',')}`)
  ok('删除会话/工作区后快捷项同步清理')
}

// ============ R6：持久化 recent 同步 ============
console.log('R6 持久化：recent 随状态写入 localStorage（重启恢复同一队列）')
{
  await fresh()
  const a = await newSession()
  const b = await newSession()
  assert.deepStrictEqual(persisted().recent?.map((r) => r.id), [a, b], `持久化的 recent 应为 [${a},${b}]`)
  // 上限内截断：写满 12 条只保留最后 10 条（与 RECENT_MAX 一致）
  const many = Array.from({ length: 12 }, (_, i) => ({ path: WS, id: `m-${i}` }))
  useAppStore.setState({ recentSessions: many })
  useAppStore.getState().removeRecentSession(WS, 'm-99') // 触发一次 persistProjects（不存在项提前返回）
  useAppStore.getState().removeRecentSession(WS, 'm-0') // 真实移除 → 持久化
  const rec = persisted().recent ?? []
  assert.ok(rec.length <= 10, `持久化 recent 不应超过上限 10，got ${rec.length}`)
  ok(`持久化 recent = [${(persisted().recent ?? []).map((r) => r.id).join(',')}]`)
}

console.log(`\n全部通过（${pass} 项断言组）`)
console.log('RECENT_BAR_TEST_PASS')
process.exit(0)
