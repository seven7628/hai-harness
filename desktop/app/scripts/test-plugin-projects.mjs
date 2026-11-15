// 虚拟 Project（插件）的 store 级回归 —— 真实 store + mock transport，Node 直跑（无浏览器）。
//
// 纯函数单测（src/lib/pluginProjects-test.ts、ownerPath-test.ts）钉住判据；本脚本钉住**接线**：
// 判据接错位置照样出问题，而症状全是「不报错的错」——
//   · plugin_projects_list 回执没并进 projects → 左栏插件分组永远空；
//   · 会话归属没写进 sessionOwnerPath → 插件会话的消息投到当时打开的真实项目；
//   · LS_PROJECTS 里留下虚拟路径 → 重启后把它当工作区水合（后端硬拒）；
//   · setActiveWorkspace 没挡虚拟路径 → 对插件 home 发 workspace_add/list/skills/git；
//   · 删会话没清归属 → 会话 id 复用后继承了别人的路径。
//
// 用法：cd desktop/app && npm run test:plugin-projects-store
// 退出码：全过 0；任一断言失败 1。
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

// 1) 浏览器 shim
const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
}
global.document = { documentElement: { classList: { add() {}, remove() {} } } }

// 2) 打包 store（含真实 mock transport）
const dir = mkdtempSync(join(tmpdir(), 'plugin-projects-'))
const outfile = join(dir, 'store.cjs')
// mock transport import 了带 ?raw / ?url 后缀的静态资源，esbuild 不认识 → stub 掉
const assetPlugin = {
  name: 'asset-suffix',
  setup(b) {
    b.onResolve({ filter: /\?(raw|url|no-inline)$/ }, () => ({ path: 'asset', namespace: 'asset-stub' }))
    b.onLoad({ filter: /.*/, namespace: 'asset-stub' }, () => ({ contents: 'module.exports = ""', loader: 'js' }))
  },
}
await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile,
  logLevel: 'silent',
  external: ['electron'],
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' },
  plugins: [assetPlugin],
})
const { useAppStore, __traceSpy } = require(outfile)
// 归属注册表（sessionOwnerPath）是模块级 Map，store 不对外导出（生产代码不该有写入口）。
// 所以这里**只用行为断言**：send() 取到哪个 workspace 就是归属是否落地的唯一可见证据。

let pass = 0
function ok(label) {
  pass++
  console.log(`  ✓ ${label}`)
}

const REAL = '/real/project'
// 每个用例用**独立的插件 home**：mock transport 的会话表按 path 存且跨用例保留，
// 复用同一个 path 会让上一个用例的会话漏进下一个（假绿/假红都来自这里）。
const pluginHome = (tag) => `/u/.go-code/plugin-sessions/${tag}/home`
let PLUG = pluginHome('c1')
const sent = []
const resetSent = () => { sent.length = 0; __traceSpy.sent = sent }

// 起一次 mock bridge：wire() 在这里注册 transport.onEvent 监听 —— 不调用它，mock 的
// command_response 没有人收（症状是「命令发了、状态永远不变」）。之后每个用例只改 state。
await useAppStore.getState().start(REAL)
resetSent()
const typesSent = () => sent.map((c) => c.type)
const lastOf = (type) => [...sent].reverse().find((c) => c.type === type)

function seed({ pluginProjects, sessions } = {}) {
  mem.clear()
  if (pluginProjects !== undefined) {
    mem.set('mock.pluginProjects', typeof pluginProjects === 'string' ? pluginProjects : JSON.stringify(pluginProjects))
  }
  useAppStore.setState({
    workspace: REAL,
    activeSessionId: 'r1',
    bridgeStatus: 'running',
    sessions: sessions ?? [{ id: 'r1', title: 'real session' }],
    projects: [{ path: REAL, sessions: sessions ?? [{ id: 'r1', title: 'real session' }] }],
    views: {},
    recentSessions: [],
    skills: [],
    titleOverrides: {},
    tabsByScope: {},
    streaming: false,
  })
  resetSent()
}

// ===== 1) loadPluginProjects：回执并入项目树 + 逐个 list 水合会话 =====
{
  PLUG = pluginHome('c1')
  seed({
    pluginProjects: [{
      path: PLUG, plugin_id: 'alpha', name: 'PDF 阅读器', icon: '📕',
      sessions: [{ id: 'ps-1', title: '翻译草稿' }],
    }],
  })
  useAppStore.getState().loadPluginProjects()
  await sleep(50)
  const st = useAppStore.getState()
  const proj = st.projects.find((p) => p.path === PLUG)
  assert.ok(proj, 'plugin_projects_list 回执应并入 projects')
  assert.equal(proj.virtual, true, '条目必须标 virtual（否则左栏把它当工作区画）')
  assert.equal(proj.pluginId, 'alpha')
  assert.equal(proj.name, 'PDF 阅读器')
  assert.equal(proj.icon, '📕')
  assert.deepEqual(proj.sessions.map((s) => s.id), ['ps-1'], '回执之后应发 list 把会话水合进来')
  assert.ok(typesSent().includes('list'), '应对虚拟 path 发 list')
  ok('loadPluginProjects：并入项目树 + list 水合会话')
}

// ===== 2) 打开插件会话：切项目指针 + 归属落地（不 refreshGit/skills/git）=====
{
  PLUG = pluginHome('c2')
  seed({
    pluginProjects: [{ path: PLUG, plugin_id: 'alpha', name: 'PDF 阅读器', sessions: [{ id: 'ps-1', title: '翻译草稿' }] }],
  })
  useAppStore.getState().loadPluginProjects()
  await sleep(30)
  resetSent()
  useAppStore.getState().setActiveSession('ps-1', PLUG)
  await sleep(30)
  const st = useAppStore.getState()
  assert.equal(st.workspace, PLUG, '打开插件会话 = 当前项目指针切到虚拟 Project')
  assert.equal(st.activeSessionId, 'ps-1')
  assert.ok(!typesSent().includes('git_snapshot'), '不再对插件 home 拉 git（§4.6 第 5 条）')
  assert.ok(!typesSent().includes('skills'), '不再对插件 home 拉项目层技能（§4.6 第 8 条）')
  assert.equal(st.skills.length, 0, '项目层技能不叠进插件会话')
  const ns = lastOf('new_session')
  assert.ok(ns, '应发 new_session')
  assert.equal(ns.payload.workspace, PLUG, 'new_session 必须显式带虚拟 workspace')
  ok('setActiveSession(plugin)：切项目 + new_session 带 workspace + 跳过 git/skills')
}

// ===== 3) send 归属：UI 切回真实项目后，插件会话的命令仍回插件 home =====
{
  PLUG = pluginHome('c3')
  seed({
    pluginProjects: [{ path: PLUG, plugin_id: 'alpha', sessions: [{ id: 'ps-1', title: 't' }] }],
  })
  useAppStore.getState().loadPluginProjects()
  await sleep(30)
  useAppStore.getState().setActiveSession('ps-1', PLUG)
  await sleep(30)
  // 模拟「用户切回真实项目」：workspace 变了，但活跃会话仍是插件的那个
  useAppStore.setState({ workspace: REAL })
  resetSent()
  // 用 switch_model 作探针：它是**带 session_id 的同步命令**（没有 busy 门控），
  // 命令落在哪个 workspace 完全由 send() 的归属取值决定。
  useAppStore.getState().switchModel('probe-model')
  await sleep(30)
  const cmd = lastOf('switch_model')
  assert.ok(cmd, '应发出 switch_model')
  assert.equal(cmd.payload.session_id, 'ps-1')
  assert.equal(cmd.payload.workspace, PLUG, '归属优先于当前活跃工作区：命令必须回到插件 home')
  ok('send 归属：插件会话消息回插件 home（不跟随 state.workspace）')
}

// ===== 4) setActiveWorkspace(虚拟 path)：空转 + 打开首会话，绝不 workspace_add/list/skills/git =====
{
  PLUG = pluginHome('c4')
  seed({
    pluginProjects: [{ path: PLUG, plugin_id: 'alpha', sessions: [{ id: 'ps-1', title: 't' }] }],
  })
  useAppStore.getState().loadPluginProjects()
  await sleep(30)
  resetSent()
  useAppStore.getState().setActiveWorkspace(PLUG)
  await sleep(30)
  const st = useAppStore.getState()
  for (const bad of ['workspace_add', 'workspace_sync', 'skills', 'git_snapshot', 'git_history']) {
    assert.ok(!typesSent().includes(bad), `虚拟 path 不得触发 ${bad}（§4.6 第 4/11 条）`)
  }
  assert.equal(st.workspace, PLUG)
  assert.equal(st.activeSessionId, 'ps-1', '转成「打开该插件项目的第一个会话」')
  ok('setActiveWorkspace(plugin)：不发工作区命令，转打开首会话')
}

// ===== 5) 持久化过滤：LS_PROJECTS 里没有虚拟 path / active / lastSession / recent =====
{
  PLUG = pluginHome('c5')
  seed({
    pluginProjects: [{ path: PLUG, plugin_id: 'alpha', sessions: [{ id: 'ps-1', title: 't' }] }],
  })
  useAppStore.getState().loadPluginProjects()
  await sleep(30)
  useAppStore.getState().setActiveSession('ps-1', PLUG)
  await sleep(30)
  const saved = JSON.parse(mem.get('go-code.projects') ?? '{}')
  assert.ok(!(saved.paths ?? []).includes(PLUG), 'paths 不得含虚拟 path（§4.6 第 1 条）')
  assert.equal(saved.active, REAL, 'active 回落真实工作区')
  // lastSession：插件会话不写；保留上一个真实会话（没有就当空 —— 下一次真实会话会补上）
  assert.ok(!saved.lastSession || saved.lastSession.path === REAL, 'lastSession 不得指向虚拟 path')
  assert.ok(!(saved.recent ?? []).some((r) => r.path === PLUG), 'recent 不得含虚拟 path')
  ok('persistProjects：虚拟 path 不进 localStorage（重启后由 plugin_projects_list 水合）')
}

// ===== 6) 虚拟项目新建会话：new_session 带虚拟 workspace，回执登记归属 =====
{
  PLUG = pluginHome('c6')
  seed({ pluginProjects: [{ path: PLUG, plugin_id: 'alpha', name: 'Alpha' }] })
  useAppStore.getState().loadPluginProjects()
  await sleep(30)
  resetSent()
  useAppStore.getState().createSessionIn(PLUG)
  await sleep(60)
  const st = useAppStore.getState()
  const ns = lastOf('new_session')
  assert.ok(ns, '应发 new_session')
  assert.equal(ns.payload.workspace, PLUG, '新建会话必须落在虚拟 Project 下')
  const proj = st.projects.find((p) => p.path === PLUG)
  assert.ok(proj.sessions.length === 1, '回执后虚拟项目下应有 1 个会话')
  const sid = proj.sessions[0].id
  assert.equal(st.activeSessionId, sid, '新会话应被激活')
  // 归属：切回真实工作区后，这个新会话的命令仍回插件 home
  useAppStore.setState({ workspace: REAL })
  resetSent()
  useAppStore.getState().switchModel('probe-model')
  await sleep(30)
  const cmd = lastOf('switch_model')
  assert.equal(cmd?.payload?.workspace, PLUG, '新会话的归属必须已登记（send 同步读）')
  ok('createSessionIn(plugin)：new_session 落虚拟 Project + 回执登记归属')
}

// ===== 7) 已卸载插件只读：不给新建会话 =====
{
  PLUG = pluginHome('c7')
  seed({ pluginProjects: [{ path: PLUG, plugin_id: 'alpha', installed: false, sessions: [{ id: 'ps-9', title: 't' }] }] })
  useAppStore.getState().loadPluginProjects()
  await sleep(30)
  const proj = useAppStore.getState().projects.find((p) => p.path === PLUG)
  assert.equal(proj.installed, false, 'installed:false 应透传')
  resetSent()
  useAppStore.getState().createSessionIn(PLUG)
  await sleep(30)
  assert.ok(!typesSent().includes('new_session'), '已卸载插件不建会话（只读）')
  // 但会话仍可打开
  useAppStore.getState().setActiveSession('ps-9', PLUG)
  await sleep(30)
  assert.equal(useAppStore.getState().activeSessionId, 'ps-9', '已卸载插件的会话仍可打开')
  ok('已卸载插件：只读（不新建），会话可打开')
}

// ===== 8) 删除虚拟项目最后一个会话：项目保留（D1），活跃工作区回落真实项目 =====
{
  PLUG = pluginHome('c8')
  seed({ pluginProjects: [{ path: PLUG, plugin_id: 'alpha', sessions: [{ id: 'ps-1', title: 't' }] }] })
  useAppStore.getState().loadPluginProjects()
  await sleep(30)
  useAppStore.getState().setActiveSession('ps-1', PLUG)
  await sleep(30)
  resetSent()
  useAppStore.getState().deleteSession(PLUG, 'ps-1')
  await sleep(50)
  const st = useAppStore.getState()
  const del = lastOf('delete_session')
  assert.equal(del?.payload?.workspace, PLUG, 'delete_session 带虚拟 workspace（后端按 runtime 解析）')
  const proj = st.projects.find((p) => p.path === PLUG)
  assert.ok(proj, '会话删空后虚拟项目条目保留（D1：还能继续新建/删除）')
  assert.equal(proj.sessions.length, 0, '该会话已从项目里移除')
  assert.equal(st.workspace, REAL, '活跃工作区回落到真实项目（不停在空壳插件 home 上）')
  ok('deleteSession(plugin)：条目保留 + 回落真实项目')
}

// ===== 9) 回执为空数组 = 插件全没了：有会话的转已卸载，空壳摘掉 =====
{
  PLUG = pluginHome('c9')
  seed({ pluginProjects: [{ path: PLUG, plugin_id: 'alpha', installed: true, sessions: [{ id: 'ps-1', title: 't' }] }] })
  useAppStore.getState().loadPluginProjects()
  await sleep(30)
  assert.ok(useAppStore.getState().projects.some((p) => p.path === PLUG))
  // 换成空清单再拉一次
  mem.set('mock.pluginProjects', JSON.stringify([]))
  useAppStore.getState().loadPluginProjects()
  await sleep(30)
  const proj = useAppStore.getState().projects.find((p) => p.path === PLUG)
  assert.ok(proj, '有会话的虚拟项目不因一次空回执消失')
  assert.equal(proj.installed, false, '转为已卸载（只读可删）')
  ok('空回执：有会话转已卸载，不整组消失')
}

// ===== 10) schedulePluginProjectsRefresh：150ms 合并一次，且不会自我循环 =====
{
  PLUG = pluginHome('c10')
  seed({ pluginProjects: [{ path: PLUG, plugin_id: 'alpha', sessions: [{ id: 'ps-1', title: 't' }] }] })
  const countList = () => typesSent().filter((t) => t === 'plugin_projects_list').length

  // 立刻发的那条（启动/手动路径）不许被去抖拖住：左栏第一帧就要有插件分组。
  useAppStore.getState().loadPluginProjects()
  assert.equal(countList(), 1, 'loadPluginProjects 必须同步发（不去抖）')
  await sleep(30)
  resetSent()

  // 插件连开三个会话 → 合并成一次列表（三次回执会把左栏刷三遍，还会互相覆盖标题）
  useAppStore.getState().schedulePluginProjectsRefresh()
  useAppStore.getState().schedulePluginProjectsRefresh()
  useAppStore.getState().schedulePluginProjectsRefresh()
  assert.equal(countList(), 0, '150ms 内不该立刻发（合并窗口）')
  await sleep(220)
  assert.equal(countList(), 1, '三次请求合并成一次')

  // 回执只更新项目树，**不再**触发本 action：没有任何新请求发出去（否则左栏 60Hz 自刷）
  const proj = useAppStore.getState().projects.find((p) => p.path === PLUG)
  assert.ok(proj, '合并后的那一次真的水合了（不是空转）')
  await sleep(400)
  assert.equal(countList(), 1, '回执不许再触发一次刷新（无自发自收环）')
  ok('schedulePluginProjectsRefresh：同步路径不去抖 + 150ms 合并 + 无自循环')
}

console.log(`\n全部通过（${pass} 项断言组）`)
