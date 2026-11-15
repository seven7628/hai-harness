// 插件宿主能力面（host.command / host.onEvent）的 Node 级回归 —— 09-agent-bridge §5/§6。
//
// 为什么要有这一层：loader 是「插件能做什么」的**唯一**闸门，而它的几种失败都是「不报错的错」——
//   · manifest 闸门写错 → 插件少用一个口子没感觉，多用一个就成了越权；
//   · token 每次调用都重新领 → 表面正常，但 main 的 run 归属是按 token 解析的，换 token =
//     换个身份（跨插件劫持的第一块砖）；
//   · 事件不按 surface_id 过滤 → 插件收到**别的插件**的 run，这在产品里是事故，不是 bug；
//   · disposer 没登记 → 卸载后回调还在跑，闭包还引用着上一条 host（僵尸监听）。
// 这些都能在没有 Electron、没有浏览器的 Node 里钉住，所以别再靠 e2e 的页面时序去撞。
//
// 用法：cd desktop/app && npm run test:surface-host
// 退出码：全过 0；任一断言失败 1。
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)

// 1) 浏览器 shim：loader → registry/health/storage 在模块求值时就会碰这两个全局
const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
  key: (i) => [...mem.keys()][i] ?? null,
  get length() { return mem.size },
}
global.document = { documentElement: { classList: { add() {}, remove() {} } } }

// 2) 打包一个**单一实例**的入口：createHost 与本脚本订阅的 uiBus 必须是同一份模块实例
//    （分别 bundle 会各自持有一张订阅表，emit 到了也没人听见 —— 这种假绿最费时间）。
const dir = mkdtempSync(join(tmpdir(), 'surface-host-'))
const entry = join(dir, 'entry.ts')
writeFileSync(entry, `
export { createHost } from ${JSON.stringify(join(__dirname, '../src/surfaces/plugin/loader.ts'))}
export { subscribeUI, emitUI, resetUIBusForTest } from ${JSON.stringify(join(__dirname, '../src/lib/uiBus.ts'))}
`)
const outfile = join(dir, 'host.cjs')
// mock transport / 静态资源后缀（?raw / ?url）esbuild 不认 → stub 掉（与其它 Node 单测同一条）
const assetPlugin = {
  name: 'asset-suffix',
  setup(b) {
    b.onResolve({ filter: /\?(raw|url|no-inline)$/ }, () => ({ path: 'asset', namespace: 'asset-stub' }))
    b.onLoad({ filter: /.*/, namespace: 'asset-stub' }, () => ({ contents: 'module.exports = ""', loader: 'js' }))
  },
}
await build({
  entryPoints: [entry],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile,
  logLevel: 'silent',
  external: ['electron'],
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' },
  plugins: [assetPlugin],
})
const { createHost, subscribeUI, emitUI, resetUIBusForTest } = require(outfile)

let pass = 0
function ok(label, cond, detail = '') {
  if (cond) {
    pass += 1
    console.log(`  ✅ ${label}`)
    return
  }
  console.log(`  ❌ ${label}${detail ? ' —— ' + detail : ''}`)
  process.exitCode = 1
  throw new Error(`断言失败：${label}${detail ? ' —— ' + detail : ''}`)
}
const eq = (label, actual, expected) => ok(label, JSON.stringify(actual) === JSON.stringify(expected), `实得 ${JSON.stringify(actual)}`)

// —— 假宿主（= main 那三条 IPC 的渲染层替身；记录每一次跨进程边界上的调用）——
function fakeAgent({ token = 'tok-demo', failToken = false, rejectAgent = false, rejectCommand = false } = {}) {
  const subs = new Set()
  const calls = { token: [], agent: [], command: [] }
  // 白名单在「main」那一侧：这份假实现只回一份有数据的回执（形状照 §6.6：值都在 data 里）
  const replies = {
    'agent.session': { ok: true, data: { session_id: 's-1' } },
    'agent.run': { ok: true, data: { run_id: 'r-1', session_id: 's-1' } },
  }
  return {
    subs,
    calls,
    surfaceAgentToken: async (id) => {
      calls.token.push(id)
      return failToken ? { ok: false, reason: 'denied' } : { ok: true, token }
    },
    surfaceAgent: async (p) => {
      calls.agent.push(p)
      if (rejectAgent) throw new Error('ipc boom（单测的 fixture）')
      return replies[p.name] ?? { ok: true, data: {} }
    },
    surfaceCommand: async (p) => {
      calls.command.push(p)
      if (rejectCommand) throw new Error('ipc boom（单测的 fixture）')
      return { ok: true, data: { echo: p.name } }
    },
    onSurfaceAgentEvent: (cb) => {
      subs.add(cb)
      return () => subs.delete(cb)
    },
    // 模拟 main 把 bridge stdout 的 data 转发到渲染层（**含别人的事件**：过滤是 loader 的责任）
    push: (payload) => {
      for (const cb of [...subs]) cb(payload)
    },
  }
}

const ITEM = {
  id: 'demo',
  version: '1.0.0',
  hash: 'h'.padEnd(64, '0'),
  enabled: true,
  kind: 'plugin:demo',
  name: 'Demo',
  entries: [],
  // 刻意声明两个「不该放行」的名字：D11 的四条之一 + 一个走旧通道的普通名字
  commands: ['agent.session', 'agent.run', 'composer.quote', 'ui.open_tab', 'ui.toast', 'workspace_list', 'board.refresh'],
  data: ['agent.delta', 'agent.done', 'surfaces_changed', 'surfaces.changed'],
  entry: 'panel.js',
}
const makeDeps = (over = {}) => {
  const calls = { openTab: [], toast: [] }
  const deps = {
    openTab: (spec) => calls.openTab.push(spec),
    toast: (msg) => calls.toast.push(msg),
    getWorkspace: () => '/mock/proj',
    hasActiveSession: () => true,
    ...over,
  }
  return { deps, calls }
}

// =====================================================================
console.log('\nT1 host 的形状 = 只有 command + onEvent（没有 events/openTab/toast/agent）')
{
  const { deps } = makeDeps()
  const host = createHost(ITEM, deps, [], fakeAgent())
  eq('keys', Object.keys(host).sort(), ['React', 'command', 'onEvent', 't', 'theme'])
  ok('command 是函数', typeof host.command === 'function')
  ok('onEvent 是函数', typeof host.onEvent === 'function')
  ok('没有 host.events（D5 之前的形态）', host.events === undefined)
  ok('没有 host.openTab / host.toast（收进 ui.* command）', host.openTab === undefined && host.toast === undefined)
  ok('没有把承载 IPC 的对象交给插件',
    ['surfaceAgent', 'surfaceAgentToken', 'onSurfaceAgentEvent', 'api', 'bridge'].every((k) => host[k] === undefined),
    JSON.stringify(Object.keys(host)))
}

// =====================================================================
console.log('\nT2 manifest.commands 闸门：未声明 → not_declared（且不产生任何 IPC）')
{
  const agent = fakeAgent()
  const { deps } = makeDeps()
  const host = createHost(ITEM, deps, [], agent)
  const r = await host.command('not.declared')
  eq('回执', r, { ok: false, reason: 'not_declared' })
  eq('没有走 surfaceAgent', agent.calls.agent.length, 0)
  eq('也没有走 surfaceCommand', agent.calls.command.length, 0)
  const r2 = await host.command(123)
  eq('非字符串名字也收敛成 not_declared（永不抛）', r2, { ok: false, reason: 'not_declared' })
}

// =====================================================================
console.log('\nT3 token：加载时领**一次**，之后每次 agent.* 复用（身份不漂）')
{
  const agent = fakeAgent()
  const { deps } = makeDeps()
  const host = createHost(ITEM, deps, [], agent)
  const a = await host.command('agent.session', { title: '看板' })
  const b = await host.command('agent.run', { input: '你好' })
  eq('session 回执把 data 摊到顶层（§5.1 的 {ok, session_id}）', { ok: a.ok, session_id: a.session_id, flat: a.data?.session_id }, { ok: true, session_id: 's-1', flat: 's-1' })
  eq('run 回执 run_id', { ok: b.ok, run_id: b.run_id, session_id: b.session_id }, { ok: true, run_id: 'r-1', session_id: 's-1' })
  eq('token 只领了一次', agent.calls.token, ['demo'])
  eq('两次调用带同一个 token', agent.calls.agent.map((c) => c.token), ['tok-demo', 'tok-demo'])
  eq('payload 只含 name/args（插件看不到 id/token 之外的注入点）',
    Object.keys(agent.calls.agent[1]).sort(), ['args', 'name', 'token'])
  eq('args 原样透传', agent.calls.agent[1].args, { input: '你好' })
}

// =====================================================================
console.log('\nT4 D11 四条：声明了也 denied，且不进 IPC')
for (const name of ['workspace_list', 'session_list', 'git_snapshot', 'ui_inspect']) {
  const agent = fakeAgent()
  const { deps } = makeDeps()
  const item = { ...ITEM, commands: [...ITEM.commands, name] }
  const host = createHost(item, deps, [], agent)
  const r = await host.command(name, {})
  eq(`${name} → denied`, r, { ok: false, reason: 'denied' })
  eq(`${name} 没有产生 IPC`, agent.calls.agent.length + agent.calls.command.length, 0)
}

// =====================================================================
console.log('\nT5 其余名字仍走 surface_command（P1c 收紧前的兼容路径）')
{
  const agent = fakeAgent()
  const { deps } = makeDeps()
  const host = createHost(ITEM, deps, [], agent)
  const r = await host.command('board.refresh', { at: 1 })
  eq('回执透传', { ok: r.ok, data: r.data }, { ok: true, data: { echo: 'board.refresh' } })
  eq('IPC 形状', agent.calls.command[0], { id: 'demo', name: 'board.refresh', args: { at: 1 }, workspace: '/mock/proj' })
}

// =====================================================================
console.log('\nT6 传输异常收敛成固定 reason（插件永远拿到对象，不接拒绝）')
{
  const agent = fakeAgent({ rejectAgent: true, rejectCommand: true })
  const { deps } = makeDeps()
  const host = createHost(ITEM, deps, [], agent)
  eq('agent.* 抛错 → error', await host.command('agent.run', { input: 'x' }), { ok: false, reason: 'error' })
  eq('surface_command 抛错 → error', await host.command('board.refresh'), { ok: false, reason: 'error' })
}

// =====================================================================
console.log('\nT7 token 领不到：agent.* denied，但 onEvent / composer.quote 照常可用')
{
  const agent = fakeAgent({ failToken: true })
  const { deps } = makeDeps()
  const host = createHost(ITEM, deps, [], agent)
  eq('agent.run → denied', await host.command('agent.run', { input: 'x' }), { ok: false, reason: 'denied' })
  eq('没有白发一次 IPC', agent.calls.agent.length, 0)
  const events = []
  const off = host.onEvent('surfaces_changed', (e) => events.push(e))
  emitUI('surfaces_changed', { at: 1 })
  eq('本地事件不依赖 token', events.length, 1)
  eq('事件形状 = {type, data}', events[0], { type: 'surfaces_changed', data: { at: 1 } })
  off()
  const quoted = await host.command('composer.quote', { text: 'hi' })
  eq('composer.quote 不依赖 token', quoted, { ok: true })
}

// =====================================================================
console.log('\nT8 事件：manifest.data 闸门 + 按 surface_id 过滤 + 按 type 分发')
{
  const agent = fakeAgent()
  const { deps } = makeDeps()
  const host = createHost(ITEM, deps, [], agent)

  let threw = ''
  try {
    host.onEvent('agent.usage', () => {}) // 没在 data 里声明
  } catch (e) {
    threw = String(e?.message ?? e)
  }
  ok('未声明的事件类型 → 抛（不是静默）', threw.includes('未在 manifest.data 中声明'), threw)

  const got = []
  const off = host.onEvent('agent.delta', (ev) => got.push(ev))
  agent.push({ surface_id: 'other', event: { type: 'agent.delta', seq: 1, data: { text: '别人的' } } })
  eq('别的插件的事件不进监听器', got.length, 0)
  agent.push({ surface_id: 'demo', event: { type: 'agent.done', seq: 1, data: { text: 'x' } } })
  eq('订阅的是 agent.delta，agent.done 不投递', got.length, 0)
  agent.push({ surface_id: 'demo', event: { type: 'agent.delta', seq: 2, run_id: 'r-1', session_id: 's-1', data: { text: '你' } } })
  eq('自己的 agent.delta 投递一次', got.length, 1)
  eq('投的是**事件信封**（不是 data）', got[0], { type: 'agent.delta', seq: 2, run_id: 'r-1', session_id: 's-1', data: { text: '你' } })
  agent.push({ surface_id: undefined, event: { type: 'agent.delta', data: { text: '无主' } } })
  eq('没有 surface_id 的事件不投递', got.length, 1)

  off()
  agent.push({ surface_id: 'demo', event: { type: 'agent.delta', data: { text: '之后' } } })
  eq('off() 之后不再回调', got.length, 1)

  // 两种拼写映射到同一个本地 op（§5.2 写 surfaces.changed，uiBus 的 op 叫 surfaces_changed）
  const local = []
  host.onEvent('surfaces.changed', (e) => local.push(e.type))
  emitUI('surfaces_changed', {})
  eq('surfaces.changed 也能收到（同一事件的两种拼法）', local, ['surfaces.changed'])
}

// =====================================================================
console.log('\nT9 disposer：全部登记进 disposers，卸载（逆序执行）后一个都不剩')
{
  const agent = fakeAgent()
  const { deps } = makeDeps()
  const disposers = []
  const host = createHost(ITEM, deps, disposers, agent)
  const got = []
  host.onEvent('agent.delta', () => got.push('delta'))
  host.onEvent('surfaces_changed', () => got.push('changed'))
  eq('两次订阅 = 两个 disposer', disposers.length, 2)
  agent.push({ surface_id: 'demo', event: { type: 'agent.delta', data: { text: 'a' } } })
  emitUI('surfaces_changed', {})
  eq('卸载前两条都活', got, ['delta', 'changed'])
  for (const d of [...disposers].reverse()) d()
  agent.push({ surface_id: 'demo', event: { type: 'agent.delta', data: { text: 'b' } } })
  emitUI('surfaces_changed', {})
  eq('卸载后一条都不回', got, ['delta', 'changed'])
  eq('agent 通道的订阅也退干净了', agent.subs.size, 0)
}

// =====================================================================
console.log('\nT10 onEvent 第二个参数必须是函数（写错了在 apply 时炸，不留半个订阅）')
{
  const { deps } = makeDeps()
  const disposers = []
  const host = createHost(ITEM, deps, disposers, fakeAgent())
  let msg = ''
  try {
    host.onEvent('agent.delta', null)
  } catch (e) {
    msg = String(e?.message ?? e)
  }
  ok('非函数 → 抛', msg.includes('必须是函数'), msg)
  eq('没有留下半个订阅', disposers.length, 0)
}

// =====================================================================
console.log('\nT11 composer.quote：只投 uiBus、不代发、8 KiB / 空文本 / 无会话')
{
  const { deps } = makeDeps()
  const host = createHost(ITEM, deps, [], fakeAgent())
  const seen = []
  const off = subscribeUI('composer_quote', (d) => seen.push(d))

  eq('成功回执', await host.command('composer.quote', { text: '  选中文本  ', source: { id: 'sel-1', title: '讲义' } }), { ok: true })
  eq('投递的载荷（text 已 trim）', seen, [{ text: '选中文本', source: { id: 'sel-1', title: '讲义' } }])

  eq('source 允许纯字符串', await host.command('composer.quote', { text: 'a', source: '来源' }), { ok: true })
  eq('字符串 source 归一成 {title}', seen[1], { text: 'a', source: { title: '来源' } })

  eq('空文本 → invalid_input', await host.command('composer.quote', { text: '   ' }), { ok: false, reason: 'invalid_input' })
  eq('缺 text → invalid_input', await host.command('composer.quote', {}), { ok: false, reason: 'invalid_input' })
  eq('非字符串 text → invalid_input', await host.command('composer.quote', { text: 42 }), { ok: false, reason: 'invalid_input' })
  eq('8 KiB 整（按字节）→ ok', await host.command('composer.quote', { text: 'x'.repeat(8192) }), { ok: true })
  eq('8 KiB + 1 字节 → invalid_input', await host.command('composer.quote', { text: 'x'.repeat(8193) }), { ok: false, reason: 'invalid_input' })
  // 中文 3 字节：2731 字 = 8193 字节 → invalid_input（用 length 判就会放过它）
  eq('按字节而不是字符', await host.command('composer.quote', { text: '中'.repeat(2731) }), { ok: false, reason: 'invalid_input' })
  eq('失败的投递一条都没进 uiBus', seen.length, 3)

  // 无活动会话：草稿是会话作用域的，宁可回 no_session 也不静默写进另一份草稿
  const noSession = createHost(ITEM, makeDeps({ hasActiveSession: () => false }).deps, [], fakeAgent())
  eq('无活动会话 → no_session', await noSession.command('composer.quote', { text: 'hi' }), { ok: false, reason: 'no_session' })
  eq('no_session 也不投递', seen.length, 3)
  off()
}

// =====================================================================
console.log('\nT12 ui.open_tab 只能开自己的 kind；ui.toast 走宿主')
{
  const { deps, calls } = makeDeps()
  const host = createHost(ITEM, deps, [], fakeAgent())
  eq('自己的 kind → ok', await host.command('ui.open_tab', { kind: 'plugin:demo', params: { entry: 'x' } }), { ok: true })
  eq('openTab 收到 kind + params', calls.openTab, [{ kind: 'plugin:demo', params: { entry: 'x' } }])
  eq('别人的 kind → denied', await host.command('ui.open_tab', { kind: 'plugin:other' }), { ok: false, reason: 'denied' })
  eq('缺 kind → invalid_input', await host.command('ui.open_tab', {}), { ok: false, reason: 'invalid_input' })
  eq('别人的 kind 没有开标签', calls.openTab.length, 1)

  eq('ui.toast → ok', await host.command('ui.toast', { message: ' 你好 ' }), { ok: true })
  eq('toast 收到 trim 后的文案', calls.toast, ['你好'])
  eq('空提示 → invalid_input', await host.command('ui.toast', { message: '  ' }), { ok: false, reason: 'invalid_input' })

  // 宿主没给能力时是 denied（不是静默成功：插件得知道它没生效）
  const bare = createHost(ITEM, { hasActiveSession: () => true }, [], fakeAgent())
  eq('宿主无 openTab 能力 → denied', await bare.command('ui.open_tab', { kind: 'plugin:demo' }), { ok: false, reason: 'denied' })
  eq('宿主无 toast 能力 → denied', await bare.command('ui.toast', { message: 'x' }), { ok: false, reason: 'denied' })
}

console.log(`\n${process.exitCode ? '❌ 有失败' : '✅ 全通过'}（${pass} 项断言）`)
