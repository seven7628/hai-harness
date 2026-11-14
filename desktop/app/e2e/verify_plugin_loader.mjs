#!/usr/bin/env node
// `verify_plugin_loader.mjs` —— 渲染层插件加载器（PR-14 C）的端到端验证。
//
// 来源：docs/creator-mode-v0.1/02-pr-plan.md PR-14、03-code-plan.md §3.6、
//       01-design.md §5.8（E 场景：坏插件只黑自己）、00-conventions §4.7（测试必须走生产入口）。
//
// 这个脚本按 §4.7 钉住三件容易测成别的东西的事：
//   1. **import() 真的跑了**：panel.js 在模块顶层写 `globalThis.__pluginRuns.demo++`，
//      断言它 ≥1。只断言「provider 存在」证明不了模块被求值（那也可能是假的 provider）。
//   2. **loader 从生产入口被启动**：`useAppStore.getState().start()` → startPluginLoader，
//      索引来自 `bridge.surfaceIndex`（探针只替这个 IPC 边界）。探针不自己调
//      syncPlugins —— 那会把「store → loader」的接线（最容易漏的一段）测成不存在。
//   3. **UI 是真的渲染**：真 RightPanel + 真注册表，断言 DOM（面板内容 / 墓碑 / 图标 URL），
//      而不是读注册表里的字符串。
//
// 覆盖（每条都能被实现打死）：
//   T0 注册表自检：四个内置 provider 都在（对照基线；缺了它下面「内置不受影响」无从谈起）。
//   T1 启动同步：demo 进注册表、listSurfaces 含它、模块求值 1 次、apply 1 次。
//   T2 标签渲染：openTab('plugin:demo') → DOM 出现面板内容 + 快照 tabs 里 kind/标题正确。
//   T3 图标：icon.svg 存在 → provider.icon 是协议 URL（DOM 里能查到那个 src）。
//   T4 坏模块隔离：kind 不符 / apply 抛错 → 都没进注册表；demo 与内置照旧；零 pageerror。
//   T5 失败可见：快照 surfaces[] 里有 plugin:bad 且带 error（ui_inspect 能看见）。
//   T6 卸载：index 去掉 → provider 消失、标签变墓碑（PR-10 机制）、事件订阅被清理。
//   T7 变更/重装：hash 变 → 重新 import（模块求值 +1）；标签恢复渲染。
//   T8 host 形状 = {command, onEvent}（没有 events/openTab/toast）；未声明 → not_declared
//      且**不抛**；D11 四条声明了也 denied；agent.* → surfaceAgent（带 loader 领的 token）。
//   T9 全程零 pageerror。
//   T10 **真模板**（templates/surface）经 loader 注册并在 RightPanel 里渲染，且走通新口子：
//      问 Agent（session → run）→ agent.delta/done 事件路由（别人的 surface_id 不进）→
//      composer.quote 只投 uiBus、不产生 IPC。
//   T11 index 移除插件 → 该 id 前缀（`<id>:`）下的 localStorage 键全清 + 健康记录一起清（R17a）。
//   T12 **禁用不清理**：enabled:false 的插件离开 index 的 alive 集合，但数据一个都不能少。
//   T13 **R-20**：同 hash 继续累计；hash 一变 → 整条健康记录清空，标签恢复渲染。
//   T14 known-ids 跨启动：首次（无记录）只记不清；第二次启动 index 少一个 id → 清那一个。
//   T16 **手动禁用不被版本变更复活**：换 hash 保留记录（disabled 仍 true）只把这一版的账归零。
//   T15 **非权威 index**（浏览器版 / 没有 preload）：不清理、不写 known-ids —— 缺省即不权威。
//
// 用法：cd desktop/app && node e2e/verify_plugin_loader.mjs
// 退出码：全通过 0；任一断言失败 1；前置不满足（端口被占 / 探针服务没起来）2。
import { spawn } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'
import { exitCodeOf, isPrecondition, waitProbePage } from './_serve.mjs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')

// 端口按口径固定 5251（PR-14 起原为 5228；5227 是 PR-12 的 ui 快照探针）。
// R17a 迁移 = **并行轮次避让**：5225–5232 那一段正被并行 R17b 的五个探针占着，
// 同一时刻启动会互相顶掉 vite（--strictPort 下后起的那个直接 exit 2，看起来像「前置不满足」）。
const PORT = 5251
const PROBE_DIR = path.join(__dirname, '_pluginprobe_run')
const PROBE_MARKER = '<title>plugin-loader probe</title>'
const PROBE_URL = `http://localhost:${PORT}/e2e/_pluginprobe_run/index.html`
const WS = '/mock/proj-plugin-loader'

// —— 插件资产（真的静态文件：`import()` 必须真的能拉到它们）——
//
// 用探针静态文件而不是 data: URL 是刻意的：data: URL 走的是另一条模块解析路径，
// 测不出 `hai-surface://<id>/panel.js?v=<hash>` 这条真实链路里的「同源 + 相对子资产
// （icon.svg）」形态。fake index 与真实 index 的字段逐字相同（读的是 surface-index.ts
// 的契约），差别只有「谁提供字节」。
fs.mkdirSync(PROBE_DIR, { recursive: true })
const PLUGINS = path.join(PROBE_DIR, 'plugins')
fs.rmSync(PLUGINS, { recursive: true, force: true })
fs.mkdirSync(PLUGINS, { recursive: true })

function writePlugin(id, panelJs, extra = {}) {
  const dir = path.join(PLUGINS, id)
  fs.mkdirSync(dir, { recursive: true })
  fs.writeFileSync(path.join(dir, 'panel.js'), panelJs)
  for (const [name, body] of Object.entries(extra)) fs.writeFileSync(path.join(dir, name), body)
}

// demo：正常插件。模块顶层计数证明 import 真的求值；apply 里注册 provider；
// 订阅一个**已声明**的 op（T6 用它证明卸载后订阅被清掉）。
writePlugin('demo', `
globalThis.__pluginRuns = globalThis.__pluginRuns || {}
globalThis.__pluginRuns.demo = (globalThis.__pluginRuns.demo || 0) + 1
export const surface = {
  kind: 'plugin:demo',
  apply({ host, React, surfaces }) {
    globalThis.__applied = globalThis.__applied || {}
    globalThis.__applied.demo = (globalThis.__applied.demo || 0) + 1
    globalThis.__hostDemo = host
    // P1 的形态：host.onEvent(type, fn) → disposer（没有 host.events 这个对象了）
    host.onEvent('surfaces_changed', () => {
      globalThis.__hostEvents = (globalThis.__hostEvents || 0) + 1
    })
    surfaces.register({
      kind: 'plugin:demo',
      group: 'plugin',
      order: 10,
      title: () => 'Demo 看板',
      render: () => React.createElement('div', { 'data-plugin': 'demo' }, 'hello from demo'),
    })
  },
}
`, { 'icon.svg': '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 12 12"><rect width="12" height="12"/></svg>\n' })

// bad：manifest 说 plugin:bad，panel.js 自称 plugin:other → kind 不符，必须拒绝注册。
writePlugin('bad', `
globalThis.__applied = globalThis.__applied || {}
export const surface = {
  kind: 'plugin:other',
  apply({ surfaces, React }) {
    globalThis.__applied.bad = true
    surfaces.register({ kind: 'plugin:bad', group: 'plugin', title: () => 'Bad', render: () => React.createElement('div') })
  },
}
`)

// crash：apply 里**先注册再抛** → 必须回滚（否则注册表里留一个半死的 provider）。
writePlugin('crash', `
export const surface = {
  kind: 'plugin:crash',
  apply({ surfaces, React }) {
    surfaces.register({ kind: 'plugin:crash', group: 'plugin', title: () => 'Crash', render: () => React.createElement('div') })
    throw new Error('apply 故意抛错（e2e 的 fixture）')
  },
}
`)

// cost-board：**仓里的真模板**（templates/surface）。跟上面三个 fixture 不同 ——
// 它是用户按 skill 拷走的那一份，所以「它能被 loader 装上并渲染」就是模板本身可用。
const TEMPLATE_DIR = path.resolve(APP, '..', '..', 'templates', 'surface')
const tplManifest = JSON.parse(fs.readFileSync(path.join(TEMPLATE_DIR, 'surface.json'), 'utf8'))
fs.mkdirSync(path.join(PLUGINS, tplManifest.id), { recursive: true })
for (const f of ['panel.js', 'icon.svg']) {
  fs.copyFileSync(path.join(TEMPLATE_DIR, f), path.join(PLUGINS, tplManifest.id, f))
}

// —— 探针页 ——
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8">'
  + `<title>plugin-loader probe</title></head>`
  + '<body><div id="root" style="height:100vh"></div>'
  + '<script type="module" src="./main.tsx"></script></body></html>')

fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
// 探针页：真 store + 真 RightPanel + 真注册表 + 真插件加载器。
//
// ⚠️ 全部用 await import 而不是顶部 static import：store 在模块求值时就
// const transport = getTransport() 把实现定死了，而 fake **必须**在任何 src 模块求值之前
// 就在 window 上（L0：桥在模块求值时一次性消费 window.desktop）。static import 会被 hoist
// 到赋值之前，探针就会静默跑在 mock
// transport 上（PR-12 踩过同一条，注释照抄）。
import '../../src/index.css'

const sent: Array<Record<string, unknown>> = []
const feeds: Array<(line: string) => void> = []
const surfaceIndexCalls: Array<unknown> = []
let surfaceCommandCalls: Array<Record<string, unknown>> = []
let surfaceCommandReply: unknown = { ok: true, data: { workspaces: [{ path: '/mock', title: 'mock' }] } }
// 插件 Agent 通道（P1b2）：token 与 surfaceAgent 的**假 main**；事件由测试显式 push。
// 这里只验证「形状 / 闸门 / 过滤」——真 run 要 Go 侧 P1c，探针不假装它成功了。
const agentCalls: Array<Record<string, unknown>> = []
const agentFeeds: Array<(p: Record<string, unknown>) => void> = []
let bootSettings: unknown = null
// 插件索引（**可变**：T6/T7 通过改它 + 注入 surfaces_changed 模拟安装/卸载/升级）。
// 初值可以预置（\`localStorage['__probe.index']\`）：T14 的「第二次启动」靠它模拟
// 「上一次会话里 index 里还有谁」—— 探针页 reload 一次就是一次冷启动。
let pluginIndex: unknown = (() => {
  try {
    const raw = localStorage.getItem('__probe.index')
    return raw ? JSON.parse(raw) : { version: 1, items: [] }
  } catch {
    return { version: 1, items: [] }
  }
})()

const stub: Record<string, unknown> = {
  startBridge: async () => ({ ok: true }),
  command: async (cmd: unknown) => {
    sent.push({ ...(cmd as Record<string, unknown>), __at: performance.now() })
    return { ok: true }
  },
  onEvent: (cb: (line: string) => void) => {
    feeds.push(cb)
    return () => {
      const i = feeds.indexOf(cb)
      if (i >= 0) feeds.splice(i, 1)
    }
  },
  onExit: () => () => {},
  settingsGet: async () => bootSettings ?? {},
  settingsSet: async () => ({ ok: true }),
  startupMark: () => {},
  // ↓ 本轮的 IPC 边界（只替这两个；loader/store/registry/RightPanel 全是真的）
  surfaceIndex: async () => {
    surfaceIndexCalls.push(pluginIndex)
    return pluginIndex
  },
  surfaceCommand: async (p: Record<string, unknown>) => {
    surfaceCommandCalls.push(p)
    return surfaceCommandReply
  },
  surfaceAgentToken: async (id: string) => ({ ok: true, token: 'probe-token-' + id }),
  surfaceAgent: async (p: Record<string, unknown>) => {
    agentCalls.push(p)
    // 回执形状照 wire（§6.6）：值都在 data 里，loader 再归一给插件
    if (p.name === 'agent.session') return { ok: true, data: { session_id: 'sp-1' } }
    if (p.name === 'agent.run') return { ok: true, data: { run_id: 'sr-1', session_id: 'sp-1' } }
    return { ok: true, data: {} }
  },
  onSurfaceAgentEvent: (cb: (p: Record<string, unknown>) => void) => {
    agentFeeds.push(cb)
    return () => {
      const i = agentFeeds.indexOf(cb)
      if (i >= 0) agentFeeds.splice(i, 1)
    }
  },
}
;(window as unknown as { desktop: unknown }).desktop = new Proxy(stub, {
  get(t, k) {
    if (k in t) return t[k as string]
    return () => Promise.resolve({ ok: true })
  },
})

const { createRoot } = await import('react-dom/client')
const [{ default: RightPanel }] = await Promise.all([import('../../src/components/RightPanel')])
await import('../../src/surfaces/builtin') // 副作用注册（F45）
const { getSurface, listSurfaces } = await import('../../src/surfaces/registry')
const { listSurfaceLoadErrors } = await import('../../src/surfaces/plugin/status')
// 本机没有 Electron 主进程 → hai-surface:// 未注册。保留 store → loader 这条接线，
// 只把字节来源换成同源静态目录（见 loader.ts 的 setPluginBaseUrlForTest 注释）。
const { setPluginBaseUrlForTest, syncPlugins: syncPluginsForTest } = await import('../../src/surfaces/plugin/loader')
setPluginBaseUrlForTest((id) => \`/e2e/_pluginprobe_run/plugins/\${id}\`)
const health = await import('../../src/surfaces/health')
const { subscribeUI } = await import('../../src/lib/uiBus')
// composer.quote 是**渲染层本地**命令：投递走 uiBus，不经过任何 IPC（§6.2）。
const quotePayloads: Array<Record<string, unknown>> = []
subscribeUI('composer_quote', (d: Record<string, unknown>) => quotePayloads.push(d))
const { useAppStore } = await import('../../src/store/useAppStore')

useAppStore.setState({ workspace: ${JSON.stringify(WS)} })
bootSettings = useAppStore.getState().settings

const w = window as unknown as Record<string, unknown>
w.__probe = {
  sent,
  store: useAppStore,
  surfaceIndexCalls: () => surfaceIndexCalls.length,
  setIndex: (items: unknown[]) => {
    pluginIndex = { version: 1, items }
    // 落盘只是为了「下一次冷启动也看到这份 index」（T14）；生产里这份数据在 Go 的 surfaceStore。
    try { localStorage.setItem('__probe.index', JSON.stringify(pluginIndex)) } catch { /* 忽略 */ }
  },
  // 健康度（R17a 的 R-20 判据）：探针只调生产函数，不自己拼 surface.disabled 的 JSON。
  health: {
    fail: (k: string) => health.recordMountFailure(k as never, new Error('mount boom（e2e 的 fixture）')),
    record: (k: string) => health.getSurfaceHealth(k as never) ?? null,
    disabled: () => health.listDisabled(),
    // CrashCard 的「禁用此面板」走的就是 disableSurface（auto:false = 手动禁用）。
    disable: (k: string) => health.disableSurface(k as never),
    enable: (k: string) => health.enableSurface(k as never),
  },
  // T15 专用：直接调**生产入口** syncPlugins 演「不权威 index」那一档。
  // store 的 start() 只能走权威那条（探针注入的 surfaceIndex 是真的），
  // 而这一档要证的恰恰是「拿不到权威 index 时什么都不许动」。
  sync: (items: unknown[], authoritative: boolean | 'default') =>
    authoritative === 'default'
      ? syncPluginsForTest({ version: 1, items } as never, undefined as never)
      : syncPluginsForTest({ version: 1, items } as never, undefined as never, {}, { authoritativeIndex: authoritative }),
  storageKeys: () => {
    const out: string[] = []
    try {
      for (let i = 0; i < localStorage.length; i += 1) {
        const k = localStorage.key(i)
        if (k && k !== '__probe.index') out.push(k)
      }
    } catch { /* 忽略 */ }
    return out.sort()
  },
  knownIds: () => {
    try {
      const raw = localStorage.getItem('hai-surface.known-ids')
      return raw ? JSON.parse(raw) : null
    } catch { return null }
  },
  inject: (line: string) => feeds.forEach((f) => f(line)),
  // 快照类的读：只从 sent 里挑 ui 信封（断言不接触内部实现）。
  snapshots: () =>
    sent
      .filter((c) => c.type === 'ui' && (c.payload as Record<string, unknown> | undefined)?.op === 'snapshot')
      .map((c) => (c.payload as Record<string, unknown>).data as Record<string, unknown>),
  surfaces: {
    list: () => listSurfaces().map((p) => p.kind),
    has: (k: string) => !!getSurface(k as never),
    iconSrc: (k: string) => {
      const node = getSurface(k as never)?.icon?.() as { props?: { src?: string } } | undefined
      return node?.props?.src ?? ''
    },
  },
  loadErrors: () => listSurfaceLoadErrors(),
  // 把右栏打开并开一个插件标签（真 store 动作，不是 setState 造标签）
  open: (kind: string) => {
    useAppStore.setState({ rightLayout: { open: true, bias: 'even', ratio: 50 }, rightExpanded: true })
    useAppStore.getState().openTab({ kind } as never)
  },
  tabKinds: () => {
    const s = useAppStore.getState()
    const scope = s.activeSessionId || s.workspace || ''
    return (s.tabsByScope[scope]?.tabs ?? []).map((t) => t.kind)
  },
  command: (name: string, payload?: unknown) => globalThis.__hostDemo ? globalThis.__hostDemo.command(name, payload ?? {}) : Promise.resolve({ ok: false, reason: 'no_host' }),
  setCommandReply: (r: unknown) => { surfaceCommandReply = r },
  commandCalls: () => surfaceCommandCalls,
  // —— 插件 Agent 通道（P1b2）——
  agentCalls: () => agentCalls,
  // 事件从「main → 渲染层」这一跳注入：**含别人的事件**（过滤是 loader 的责任）
  pushAgentEvent: (p: Record<string, unknown>) => agentFeeds.forEach((f) => f(p)),
  // host 的形状：新形态 = command + onEvent，且**没有** events/openTab/toast
  hostShape: () => {
    const host = globalThis.__hostDemo
    if (!host) return null
    return {
      keys: Object.keys(host).sort(),
      command: typeof host.command,
      onEvent: typeof host.onEvent,
      events: host.events === undefined ? 'undefined' : typeof host.events,
      openTab: typeof host.openTab,
      toast: typeof host.toast,
      apiLeak: ['surfaceAgent', 'surfaceAgentToken', 'onSurfaceAgentEvent'].filter((k) => host[k] !== undefined),
    }
  },
  // manifest.data 闸门：未声明的事件类型必须抛（apply 期间就能发现写错了）
  subscribeUndeclared: () => { try { globalThis.__hostDemo.onEvent('not_declared', () => {}); return 'no-throw' } catch (e) { return String(e && e.message || e) } },
  // composer.quote 的投递（真 uiBus；本探针没挂 Composer，所以只看通道里到了什么）
  quotes: () => quotePayloads,
  moduleRuns: (id: string) => (globalThis.__pluginRuns || {})[id] || 0,
  applied: (id: string) => (globalThis.__applied || {})[id] || 0,
  eventHits: () => globalThis.__hostEvents || 0,
}

void useAppStore.getState().start(${JSON.stringify(WS)})
createRoot(document.getElementById('root')!).render(<RightPanel />)
`)

fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [APP] } } })
`)

const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
server.stderr.on('data', (d) => { const s = String(d); if (!/ready in|Local:/.test(s)) process.stderr.write(s) })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)

const fails = []
function check(label, cond, detail = '') {
  if (cond) console.log(`  ✅ ${label}`)
  else { fails.push(label); console.log(`  ❌ ${label}${detail ? ' —— ' + detail : ''}`) }
}

const BASE = (id) => `/e2e/_pluginprobe_run/plugins/${id}`
const item = (id, hash, extra = {}) => ({
  id,
  version: '1.0.0',
  hash: hash.padEnd(64, '0').slice(0, 64),
  enabled: true,
  kind: `plugin:${id}`,
  // workspace_list 是**故意**留着的：D11 说它已禁用，声明了也必须 denied（双保险）
  commands: ['workspace_list', 'agent.session', 'agent.run'],
  data: ['surfaces_changed'],
  entry: 'panel.js',
  ...(id === 'demo' ? { icon: 'icon.svg' } : {}),
  ...extra,
})

let code = 0
try {
  await waitProbePage(PROBE_URL, { marker: PROBE_MARKER, probe: 'e2e:plugin-loader', child: server })
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
  page.setDefaultTimeout(5000)
  // ⚠️ 只在**第一次**导航时清 localStorage：T14 的「跨启动」要 reload 同一页面，
  // 每次都清等于把 known-ids 记录一起清掉，那条断言就永远测不到东西。
  // 同时预置 `ghost:keep`（一个**从未**出现在 index 里的 id 的数据）：首次升级
  // （没有 known-ids 记录）时它必须原样留着 —— 那是「不清理」的反向证据。
  await page.addInitScript(() => {
    try {
      if (sessionStorage.getItem('__probe_boot')) return
      sessionStorage.setItem('__probe_boot', '1')
      localStorage.clear()
      localStorage.setItem('ghost:keep', '1')
    } catch { /* 忽略 */ }
  })
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))

  await page.goto(PROBE_URL, { waitUntil: 'domcontentloaded' })
  await page.waitForFunction(() => !!window.__probe, null, { timeout: 30000 })

  const surfaces = () => page.evaluate(() => window.__probe.surfaces.list())
  const snapshots = () => page.evaluate(() => window.__probe.snapshots())
  const loadErrors = () => page.evaluate(() => window.__probe.loadErrors())
  const injectChanged = () => page.evaluate(() => window.__probe.inject(JSON.stringify({ type: 'ui', op: 'surfaces_changed', data: { at: Date.now() } })))
  const setIndex = (items) => page.evaluate((it) => window.__probe.setIndex(it), items)

  // ---- T0 注册表自检（F45：先证明探针走的是被测那条路）----
  console.log('\nT0 内置 provider 都在（对照基线）')
  const kinds0 = await surfaces()
  for (const k of ['git', 'activity', 'file', 'web']) check(`listSurfaces() 含内置 ${k}`, kinds0.includes(k), JSON.stringify(kinds0))
  check('启动时一个插件都没装（index 为空）', !kinds0.some((k) => k.startsWith('plugin:')), JSON.stringify(kinds0))

  // ---- T14a 首次启动（没有 known-ids 记录）→ 一个键都不清 ----
  console.log('\nT14a 首次启动：没有 known-ids 记录 → 不清任何键，只记录当前 index')
  await page.waitForFunction(() => localStorage.getItem('hai-surface.known-ids') !== null, null, { timeout: 5000 }).catch(() => {})
  const bootKnown = await page.evaluate(() => window.__probe.knownIds())
  check('启动后就写下了 known-ids 记录（不是等第一次变更）', !!bootKnown, JSON.stringify(bootKnown))
  check('记录带格式版本字段（v1）', bootKnown?.version === 1, JSON.stringify(bootKnown))
  check('首次启动记录的内容 = 当前 index（空数组）', Array.isArray(bootKnown?.ids) && bootKnown.ids.length === 0, JSON.stringify(bootKnown?.ids))
  // 反向证据：ghost 从未出现在 index 里，首次升级时**必须**原样留着。
  // 一个「把 localStorage 里所有前缀都当孤儿」的实现会在这一步把用户已装插件的数据删光。
  check('首次升级不清数据：索引外的 ghost:keep 原样还在', (await page.evaluate(() => localStorage.getItem('ghost:keep'))) === '1')

  // ---- T1 启动同步：demo 真的被 import + apply ----
  console.log('\nT1 启动同步（index → import → apply → 注册表）')
  await setIndex([item('demo', 'h1'), item('bad', 'hb'), item('crash', 'hc')])
  await page.evaluate(() => window.__probe.inject(JSON.stringify({ type: 'ui', op: 'surfaces_changed', data: {} })))
  await page.waitForFunction(() => window.__probe.surfaces.has('plugin:demo'), null, { timeout: 8000 }).catch(() => {})
  const kinds1 = await surfaces()
  check('getSurface("plugin:demo") 存在', await page.evaluate(() => window.__probe.surfaces.has('plugin:demo')),
    JSON.stringify(await loadErrors()))
  check('listSurfaces() 里有 plugin:demo', kinds1.includes('plugin:demo'), JSON.stringify(kinds1))
  check('panel.js 模块被真的求值了 1 次', (await page.evaluate(() => window.__probe.moduleRuns('demo'))) === 1,
    String(await page.evaluate(() => window.__probe.moduleRuns('demo'))))
  check('surface.apply 被调用 1 次', (await page.evaluate(() => window.__probe.applied('demo'))) === 1)

  // ---- T2 标签渲染：真 RightPanel 把面板内容画出来了 ----
  console.log('\nT2 openTab(plugin:demo) → 标签 + 面板内容')
  await page.evaluate(() => window.__probe.open('plugin:demo'))
  await page.waitForSelector('[data-plugin="demo"]', { timeout: 5000 }).catch(() => {})
  const panelText = await page.evaluate(() => document.querySelector('[data-plugin="demo"]')?.textContent ?? '')
  check('DOM 里出现插件面板内容', panelText.includes('hello from demo'), JSON.stringify(panelText))
  check('标签条里有这个 kind', (await page.evaluate(() => window.__probe.tabKinds())).includes('plugin:demo'))
  // 快照（真 startUISnapshot → transport.send）里也要有它：ui_inspect 读的就是这份
  let snap = (await snapshots()).at(-1)
  for (let i = 0; i < 30 && !(snap?.tabs?.tabs ?? []).some((t) => t.kind === 'plugin:demo'); i++) {
    await page.waitForTimeout(200)
    snap = (await snapshots()).at(-1)
  }
  const tabEntry = (snap?.tabs?.tabs ?? []).find((t) => t.kind === 'plugin:demo')
  check('快照 tabs 里有 plugin:demo', !!tabEntry, JSON.stringify(snap?.tabs?.tabs ?? []))
  check('快照里的标题来自 provider.title（Demo 看板）', tabEntry?.title === 'Demo 看板', JSON.stringify(tabEntry?.title))
  check('快照 surfaces 里有 plugin:demo 且未禁用', (snap?.surfaces ?? []).some((s) => s.kind === 'plugin:demo' && s.disabled === false),
    JSON.stringify((snap?.surfaces ?? []).map((s) => s.kind)))

  // ---- T3 图标：icon.svg → 协议/静态 URL 作为 provider.icon ----
  console.log('\nT3 icon.svg 存在 → provider.icon 指向它')
  const iconSrc = await page.evaluate(() => window.__probe.surfaces.iconSrc('plugin:demo'))
  check(`provider.icon 的 src 指向 icon.svg（实得 ${JSON.stringify(iconSrc)}）`, iconSrc.endsWith('/plugins/demo/icon.svg'))
  const domIcon = await page.evaluate(() => !!document.querySelector('img[src*="plugins/demo/icon.svg"]'))
  check('标签条上真的画了那个图标', domIcon)

  // ---- T4 坏模块隔离（E 场景）----
  console.log('\nT4 坏模块只黑自己：kind 不符 / apply 抛错')
  const kinds2 = await surfaces()
  check('kind 不符的插件没进注册表（plugin:bad）', !kinds2.includes('plugin:bad'), JSON.stringify(kinds2))
  check('apply 抛错的插件也没进注册表（plugin:crash，注册已回滚）', !kinds2.includes('plugin:crash'), JSON.stringify(kinds2))
  check('demo 不受影响', kinds2.includes('plugin:demo'))
  check('四个内置 provider 仍全在', ['git', 'activity', 'file', 'web'].every((k) => kinds2.includes(k)), JSON.stringify(kinds2))
  check('坏插件的 apply 连第一行都没跑（plugin:bad）', (await page.evaluate(() => window.__probe.applied('bad'))) === 0)
  check('此时页面零 pageerror', errs.length === 0, errs.join(' | '))

  // ---- T5 失败可见：ui_inspect 的 surfaces[] 带 error ----
  console.log('\nT5 加载失败进快照（ui_inspect 能看见 disabled+error）')
  let errSnap = (await snapshots()).at(-1)
  for (let i = 0; i < 30 && !(errSnap?.surfaces ?? []).some((s) => s.kind === 'plugin:bad' && s.error); i++) {
    await page.waitForTimeout(200)
    errSnap = (await snapshots()).at(-1)
  }
  const badEntry = (errSnap?.surfaces ?? []).find((s) => s.kind === 'plugin:bad')
  check('快照 surfaces 里有 plugin:bad', !!badEntry, JSON.stringify(errSnap?.surfaces ?? []))
  check('plugin:bad 带 disabled:true + error', badEntry?.disabled === true && typeof badEntry?.error === 'string' && badEntry.error.length > 0,
    JSON.stringify(badEntry))
  const errsList = await loadErrors()
  check('status 清单里有两个失败（bad + crash）', errsList.length === 2, JSON.stringify(errsList.map((e) => e.kind)))

  // ---- T6 卸载：provider 消失 + 标签变墓碑 + 订阅清掉 ----
  console.log('\nT6 index 去掉 demo → provider 消失、标签变墓碑')
  const hitsBefore = await page.evaluate(() => window.__probe.eventHits())
  await injectChanged() // 先让 loader 的订阅被触发一次（证明订阅真的挂上了）
  await page.waitForTimeout(300)
  check('已声明的 op 订阅真的收到事件', (await page.evaluate(() => window.__probe.eventHits())) > hitsBefore,
    `before=${hitsBefore} after=${await page.evaluate(() => window.__probe.eventHits())}`)

  await setIndex([item('bad', 'hb'), item('crash', 'hc')]) // demo 从 index 消失
  await injectChanged()
  await page.waitForFunction(() => !window.__probe.surfaces.has('plugin:demo'), null, { timeout: 5000 }).catch(() => {})
  check('provider 已注销', !(await page.evaluate(() => window.__probe.surfaces.has('plugin:demo'))))
  check('listSurfaces() 里也没了', !(await surfaces()).includes('plugin:demo'))
  await page.waitForSelector('.surface-tombstone[data-kind="plugin:demo"]', { timeout: 5000 }).catch(() => {})
  const tomb = await page.evaluate(() => {
    const el = document.querySelector('.surface-tombstone[data-kind="plugin:demo"]')
    return el ? { reason: el.getAttribute('data-reason'), text: el.textContent } : null
  })
  check('标签变墓碑（PR-10 机制，reason=missing）', tomb?.reason === 'missing', JSON.stringify(tomb))
  check('墓碑文案非空', !!tomb?.text && tomb.text.length > 0)
  check('标签本身还在（不删用户的标签）', (await page.evaluate(() => window.__probe.tabKinds())).includes('plugin:demo'))
  const hitsAfterUnload = await page.evaluate(() => window.__probe.eventHits())
  await injectChanged()
  await page.waitForTimeout(300)
  check('卸载后 host 的事件订阅被清理（僵尸监听 = 0）', (await page.evaluate(() => window.__probe.eventHits())) === hitsAfterUnload,
    `after=${hitsAfterUnload} now=${await page.evaluate(() => window.__probe.eventHits())}`)

  // ---- T7 hash 变 → 重新 import；标签恢复渲染 ----
  console.log('\nT7 升级（hash 变）→ 重新 import，标签恢复')
  await setIndex([item('demo', 'h2'), item('bad', 'hb'), item('crash', 'hc')])
  await injectChanged()
  await page.waitForFunction(() => window.__probe.surfaces.has('plugin:demo'), null, { timeout: 5000 }).catch(() => {})
  check('provider 回来了', await page.evaluate(() => window.__probe.surfaces.has('plugin:demo')))
  check('hash 变了 → 模块被重新求值（第 2 次）', (await page.evaluate(() => window.__probe.moduleRuns('demo'))) === 2,
    String(await page.evaluate(() => window.__probe.moduleRuns('demo'))))
  await page.waitForSelector('[data-plugin="demo"]', { timeout: 5000 }).catch(() => {})
  check('墓碑消失、面板内容回来了', (await page.evaluate(() => !!document.querySelector('[data-plugin="demo"]'))))
  // 同一份 hash 再同步一次：不许重复求值（缓存键生效）
  await injectChanged()
  await page.waitForTimeout(400)
  check('hash 未变 → 不重复 import（仍是 2 次）', (await page.evaluate(() => window.__probe.moduleRuns('demo'))) === 2,
    String(await page.evaluate(() => window.__probe.moduleRuns('demo'))))

  // ---- T8 host 的形状 + command 闸门（P1b2：command/onEvent 两个口子）----
  console.log('\nT8 host 形状 = {command,onEvent}；未声明 → not_declared；D11 → denied')
  const shape = await page.evaluate(() => window.__probe.hostShape())
  check('host.command 是函数', shape?.command === 'function', JSON.stringify(shape))
  check('host.onEvent 是函数', shape?.onEvent === 'function', JSON.stringify(shape))
  check('没有 host.events（D5 之前的形态）', shape?.events === 'undefined', JSON.stringify(shape))
  check('没有 host.openTab / host.toast（收进 ui.* command）',
    shape?.openTab === 'undefined' && shape?.toast === 'undefined', JSON.stringify(shape))
  check('没有把承载 IPC 的对象交给插件', Array.isArray(shape?.apiLeak) && shape.apiLeak.length === 0, JSON.stringify(shape?.apiLeak))

  const beforeCalls = (await page.evaluate(() => window.__probe.commandCalls())).length
  const beforeAgent = (await page.evaluate(() => window.__probe.agentCalls())).length
  const denied = await page.evaluate(() => window.__probe.command('not.declared'))
  check('未声明命令回 {ok:false, reason:"not_declared"}', denied?.ok === false && denied?.reason === 'not_declared', JSON.stringify(denied))
  check('未声明命令没有白走一次 IPC', (await page.evaluate(() => window.__probe.commandCalls())).length === beforeCalls)

  const d11 = await page.evaluate(() => window.__probe.command('workspace_list'))
  check('D11 的 workspace_list 声明了也 denied', d11?.ok === false && d11?.reason === 'denied', JSON.stringify(d11))
  check('D11 没有白走一次 IPC', (await page.evaluate(() => window.__probe.commandCalls())).length === beforeCalls)

  // agent.* → 走 surfaceAgent（不是 surface_command），token 由 loader 闭包持有
  const agentReply = await page.evaluate(() => window.__probe.command('agent.run', { input: '你好' }))
  check('agent.run 回执把 data 摊到顶层（§5.1）',
    agentReply?.ok === true && agentReply?.run_id === 'sr-1' && agentReply?.data?.run_id === 'sr-1', JSON.stringify(agentReply))
  const agentCalls = await page.evaluate(() => window.__probe.agentCalls())
  const runCall = agentCalls[agentCalls.length - 1]
  check('surfaceAgent 收到 name/args/token',
    runCall?.name === 'agent.run' && runCall?.token === 'probe-token-demo' && JSON.stringify(runCall?.args) === JSON.stringify({ input: '你好' }),
    JSON.stringify(runCall))
  check('agent.* 没有误走 surface_command', (await page.evaluate(() => window.__probe.commandCalls())).length === beforeCalls,
    `before=${beforeCalls} now=${(await page.evaluate(() => window.__probe.commandCalls())).length}`)
  check('agentCalls 只多了一条（没重复投递）', agentCalls.length === beforeAgent + 1, `${beforeAgent} → ${agentCalls.length}`)

  const undeclaredOp = await page.evaluate(() => window.__probe.subscribeUndeclared())
  check('订阅未声明的事件类型会抛（不是静默）', undeclaredOp.includes('未在 manifest.data 中声明'), JSON.stringify(undeclaredOp))
  // 拒绝也要是「值」不是「异常」：再看一眼 pageerror（T9 会整场复查）
  check('T8 之后仍然零 pageerror', errs.length === 0, errs.join(' | '))

  // ---- T10 真模板端到端：templates/surface 这个「用户拷走的那份」能装能渲染 ----
  console.log('\nT10 真模板（templates/surface）经 loader 注册、渲染，并走通新口子')
  // 索引条目**从真 manifest 派生**（id/kind/entry/commands/data/icon 全部照抄），
  // 只有 hash 是探针给的（生产里由安装器算 sha256）。
  const tplItem = {
    id: tplManifest.id,
    version: tplManifest.version,
    hash: 'htpl'.padEnd(64, '1'),
    enabled: true,
    kind: tplManifest.kind,
    commands: tplManifest.commands || [],
    data: tplManifest.data || [],
    entry: tplManifest.entry || 'panel.js',
    ...(tplManifest.icon ? { icon: tplManifest.icon } : {}),
  }
  check(`模板 id/kind 自洽（${tplManifest.id} / ${tplManifest.kind}）`, tplManifest.kind === `plugin:${tplManifest.id}`)
  check('模板 manifest 不再声明 D11 的旧命令',
    !(tplManifest.commands || []).some((c) => ['workspace_list', 'session_list', 'git_snapshot', 'ui_inspect'].includes(c)),
    JSON.stringify(tplManifest.commands))
  await setIndex([item('demo', 'h2'), tplItem]) // 去掉两个坏插件，避免它们的 warn 淹没
  await injectChanged()
  await page.waitForFunction((k) => window.__probe.surfaces.has(k), tplItem.kind, { timeout: 8000 }).catch(() => {})
  check('模板的 provider 进了注册表', await page.evaluate((k) => window.__probe.surfaces.has(k), tplItem.kind))
  await page.evaluate((k) => window.__probe.open(k), tplItem.kind)
  await page.waitForSelector('[data-surface="cost-board"]', { timeout: 5000 }).catch(() => {})
  const tplText = await page.evaluate(() => document.querySelector('[data-surface="cost-board"]')?.textContent ?? '')
  check('模板面板在真 RightPanel 里渲染了', tplText.includes('成本看板'), JSON.stringify(tplText))

  // ① 问 Agent：session → run（都是 surfaceAgent，不是 surface_command）
  const tplCallsBefore = (await page.evaluate(() => window.__probe.commandCalls())).length
  const tplAgentBefore = (await page.evaluate(() => window.__probe.agentCalls())).length
  await page.click('[data-template-ask]')
  await page.waitForFunction((n) => window.__probe.agentCalls().length >= n + 2, tplAgentBefore, { timeout: 5000 }).catch(() => {})
  const tplAgent = (await page.evaluate(() => window.__probe.agentCalls())).slice(tplAgentBefore)
  check('模板先建会话', tplAgent[0]?.name === 'agent.session' && tplAgent[0]?.token === `probe-token-${tplManifest.id}`, JSON.stringify(tplAgent[0]))
  check('模板再发起 run（带 session_id + input）',
    tplAgent[1]?.name === 'agent.run' && tplAgent[1]?.args?.session_id === 'sp-1' && typeof tplAgent[1]?.args?.input === 'string',
    JSON.stringify(tplAgent[1]))
  check('模板的 agent.* 没有误走 surface_command', (await page.evaluate(() => window.__probe.commandCalls())).length === tplCallsBefore)

  // ② 事件路由：自己的 delta 进面板、别人的 delta 不进、done 收尾
  await page.evaluate((id) => {
    window.__probe.pushAgentEvent({ surface_id: 'someone-else', event: { type: 'agent.delta', seq: 1, run_id: 'sr-x', data: { text: '别人的输出' } } })
    window.__probe.pushAgentEvent({ surface_id: id, event: { type: 'agent.delta', seq: 2, run_id: 'sr-1', session_id: 'sp-1', data: { text: '流式文本' } } })
  }, tplManifest.id)
  await page.waitForSelector('[data-template-answer]', { timeout: 5000 }).catch(() => {})
  const answer = await page.evaluate(() => document.querySelector('[data-template-answer]')?.textContent ?? '')
  check('agent.delta 流进了面板', answer.includes('流式文本'), JSON.stringify(answer))
  check('别的 surface 的事件没进面板', !answer.includes('别人的输出'), JSON.stringify(answer))
  await page.evaluate((id) => window.__probe.pushAgentEvent({ surface_id: id, event: { type: 'agent.done', seq: 3, run_id: 'sr-1', data: { text: '流式文本' } } }), tplManifest.id)
  await page.waitForFunction(() => (document.querySelector('[data-template-status]')?.textContent || '') === '完成', null, { timeout: 3000 }).catch(() => {})
  check('agent.done 把面板收尾（状态 = 完成）',
    (await page.evaluate(() => document.querySelector('[data-template-status]')?.textContent ?? '')) === '完成')

  // ③ composer.quote：**本层**处理，不产生任何 IPC。
  //   探针页没有活动会话（没调 newSession），所以这里拿到的正是 §5.1 的 no_session ——
  //   「成功插入草稿 + chip + 去重」是 verify_composer_quote.mjs 的事（那边有真 Composer 与真会话）。
  //   这一条只钉「它是本地命令」：既不进 surface_command，也不进 surface_agent。
  const tplQuotes = (await page.evaluate(() => window.__probe.quotes())).length
  await page.click('[data-template-quote]')
  await page.waitForFunction(() => (document.querySelector('[data-template-status]')?.textContent || '').includes('引用失败'), null, { timeout: 3000 }).catch(() => {})
  const quoteStatus = await page.evaluate(() => document.querySelector('[data-template-status]')?.textContent ?? '')
  check('无活动会话 → 面板如实显示 no_session', quoteStatus === '引用失败：no_session', JSON.stringify(quoteStatus))
  check('no_session 不投递（uiBus 一条都没多）', (await page.evaluate(() => window.__probe.quotes())).length === tplQuotes)
  check('composer.quote 没有走任何 IPC',
    (await page.evaluate(() => window.__probe.commandCalls())).length === tplCallsBefore &&
    (await page.evaluate(() => window.__probe.agentCalls())).length === tplAgentBefore + 2,
    `cmd=${(await page.evaluate(() => window.__probe.commandCalls())).length} agent=${(await page.evaluate(() => window.__probe.agentCalls())).length}`)
  check('模板没有引入页面错误', errs.length === 0, errs.join(' | '))

  // ---- T11 卸载即清理（运行中）----
  console.log('\nT11 运行中卸载 → 该 id 前缀的键 + surface.disabled 记录一起清（R17a）')
  await page.evaluate(() => {
    localStorage.setItem('demo:progress:v1', '3/10')
    localStorage.setItem('demo:last-tab', 'board')
    // 前缀**相似但不是同一个 id**：'notdemo:' 不属于 'demo:'，不许被前缀清理带走。
    localStorage.setItem('notdemo:keep', '1')
  })
  await page.evaluate(() => { for (let i = 0; i < 3; i += 1) window.__probe.health.fail('plugin:demo') })
  const rec11 = await page.evaluate(() => window.__probe.health.record('plugin:demo'))
  const keys11 = await page.evaluate(() => window.__probe.storageKeys())
  check('基线：3 次挂载失败 → plugin:demo 自动禁用', rec11?.disabled === true && rec11?.count === 3, JSON.stringify(rec11))
  check('基线：demo: 前缀下 2 个键', keys11.filter((k) => k.startsWith('demo:')).length === 2, JSON.stringify(keys11))
  // 记录出生时版本表里已经写着 h2 —— 所以这条记录**盖得上戳**（跨重启的升级才比得出来）。
  check('基线：记录带上了当前版本 hash（来自 surface.versions）',
    typeof rec11?.hash === 'string' && rec11.hash.length === 64, JSON.stringify(rec11?.hash))

  await setIndex([tplItem]) // demo 从 index 消失 = 运行中卸载
  await injectChanged()
  await page.waitForFunction(() => !window.__probe.surfaces.has('plugin:demo'), null, { timeout: 5000 }).catch(() => {})
  const keysAfter11 = await page.evaluate(() => window.__probe.storageKeys())
  check('demo: 前缀的键全没了', !keysAfter11.some((k) => k.startsWith('demo:')), JSON.stringify(keysAfter11))
  check('别的 id 的键没被误删（notdemo:keep 还在）', keysAfter11.includes('notdemo:keep'), JSON.stringify(keysAfter11))
  check('surface.disabled 里 plugin:demo 的记录也没了（kind 都没了，禁用记录没有意义）',
    (await page.evaluate(() => window.__probe.health.record('plugin:demo'))) === null)

  // ---- T12 禁用不清理 ----
  console.log('\nT12 禁用不清理：enabled:false 离开 alive 集合，但数据一个都不能少')
  await setIndex([item('demo', 'h3'), tplItem])
  await injectChanged()
  await page.waitForFunction(() => window.__probe.surfaces.has('plugin:demo'), null, { timeout: 8000 }).catch(() => {})
  await page.evaluate(() => { localStorage.setItem('demo:progress:v1', '7/10') })
  await setIndex([item('demo', 'h3', { enabled: false }), tplItem])
  await injectChanged()
  await page.waitForFunction(() => !window.__probe.surfaces.has('plugin:demo'), null, { timeout: 5000 }).catch(() => {})
  check('停用后 provider 已卸下（不是「没装」）', !(await page.evaluate(() => window.__probe.surfaces.has('plugin:demo'))))
  check('停用**不**清数据（demo:progress:v1 还在）',
    (await page.evaluate(() => localStorage.getItem('demo:progress:v1'))) === '7/10')
  // 用户补的那条口径：known-ids 按 index.items 的**全部** id 记（含 enabled:false），
  // 否则「停用 → 下次启动」会被当成「消失」而把数据清掉。
  check('known-ids 里仍有 demo（停用的插件不算消失）',
    ((await page.evaluate(() => window.__probe.knownIds()?.ids)) ?? []).includes('demo'))

  // ---- T13 R-20：按版本重置 ----
  console.log('\nT13 R-20：同 hash 继续累计，hash 一变整条清空')
  await setIndex([item('demo', 'h3'), tplItem]) // 重新启用（hash 未变）
  await injectChanged()
  await page.waitForFunction(() => window.__probe.surfaces.has('plugin:demo'), null, { timeout: 8000 }).catch(() => {})
  await page.evaluate(() => { for (let i = 0; i < 3; i += 1) window.__probe.health.fail('plugin:demo') })
  const rec13a = await page.evaluate(() => window.__probe.health.record('plugin:demo'))
  check('3 次失败 → count=3 且 disabled:true', rec13a?.count === 3 && rec13a?.disabled === true, JSON.stringify(rec13a))
  check('记录盖的是 h3', typeof rec13a?.hash === 'string' && rec13a.hash.startsWith('h3'), JSON.stringify(rec13a?.hash))
  // 同 hash 的**重新应用**（停用 → 启用：provider 真的走一次 unload + apply）：
  // 版本没变 → 记录与禁用态都不许动。这条是「换版本才清」的另一半，也是变异
  // 「hash 相同也清」的杀手（那种实现会在这里把整条记录删掉，面板凭空复活）。
  // 注：只发一次 surfaces_changed 不够 —— 同 hash 会走 skipped 分支，根本不进
  // noteSurfaceVersion，测不到那一行（第一次写这条断言时就踩了这个假绿）。
  await setIndex([{ ...item('demo', 'h3'), enabled: false }, tplItem])
  await injectChanged()
  await page.waitForFunction(() => !window.__probe.surfaces.has('plugin:demo'), null, { timeout: 5000 }).catch(() => {})
  await setIndex([item('demo', 'h3'), tplItem])
  await injectChanged()
  await page.waitForFunction(() => window.__probe.surfaces.has('plugin:demo'), null, { timeout: 8000 }).catch(() => {})
  await page.waitForTimeout(300)
  const rec13b = await page.evaluate(() => window.__probe.health.record('plugin:demo'))
  check('同 hash 重新应用（停用→启用）：记录还在、仍是禁用态',
    rec13b?.disabled === true && typeof rec13b?.hash === 'string' && rec13b.hash.startsWith('h3'), JSON.stringify(rec13b))

  const runsBefore13 = await page.evaluate(() => window.__probe.moduleRuns('demo'))
  await setIndex([item('demo', 'h4'), tplItem]) // 换版本
  await injectChanged()
  await page.waitForFunction((n) => window.__probe.moduleRuns('demo') > n, runsBefore13, { timeout: 8000 }).catch(() => {})
  check('新版本真的被 import 了（模块求值 +1）',
    (await page.evaluate(() => window.__probe.moduleRuns('demo'))) > runsBefore13,
    `before=${runsBefore13} after=${await page.evaluate(() => window.__probe.moduleRuns('demo'))}`)
  const rec13c = await page.evaluate(() => window.__probe.health.record('plugin:demo'))
  check('换 hash → 整条记录被清（count/disabled/lastError 一起）', rec13c === null, JSON.stringify(rec13c))
  check('也不再出现在禁用集里',
    !((await page.evaluate(() => window.__probe.health.disabled())) ?? []).includes('plugin:demo'))
  await page.evaluate(() => window.__probe.open('plugin:demo'))
  await page.waitForSelector('[data-plugin="demo"]', { timeout: 5000 }).catch(() => {})
  check('新版可挂载：面板内容真的画出来了（不是墓碑 / 禁用卡）',
    await page.evaluate(() => !!document.querySelector('[data-plugin="demo"]')))

  // ---- T16 手动禁用不被版本变更复活 ----
  console.log('\nT16 手动禁用（CrashCard「禁用此面板」）不因换版本被自动复活')
  // 先把 demo 换到 h5：让「手动禁用」这条记录**带着版本戳**（disableSurface 会沿用 prev.hash）。
  // 不带戳的记录走的是「老记录只补值」那条分支，测不到「换版本」的判据（第一版用例就踩了这个假绿）。
  const runsBefore16 = await page.evaluate(() => window.__probe.moduleRuns('demo'))
  await setIndex([item('demo', 'h5'), tplItem])
  await injectChanged()
  await page.waitForFunction((n) => window.__probe.moduleRuns('demo') > n, runsBefore16, { timeout: 8000 }).catch(() => {})
  // 2 次失败 → 记录带 h5 的戳 + 非空 lastError（换版本后这两样都必须变），
  // 再由「禁用此面板」把它变成手动禁用。三项都在同一次 evaluate 里做完（跨 task 中间有
  // SurfaceBoundary 的 didUpdate → recordMountSuccess 会把 count 归零）。
  const manual16 = await page.evaluate(() => {
    const h = window.__probe.health
    h.fail('plugin:demo')
    h.fail('plugin:demo')
    const beforeDisable = h.record('plugin:demo')
    h.disable('plugin:demo')
    const manual = h.record('plugin:demo')
    return { beforeDisable, manual }
  })
  check('基线：禁用前记录带 h5 的戳 + count=2',
    manual16.beforeDisable?.count === 2 && typeof manual16.beforeDisable?.hash === 'string'
      && manual16.beforeDisable.hash.startsWith('h5'), JSON.stringify(manual16.beforeDisable))
  check('基线：手动禁用 = disabled:true 且 auto 不是 true（沿用那枚 h5 戳）',
    manual16.manual?.disabled === true && manual16.manual?.auto !== true
      && typeof manual16.manual?.hash === 'string' && manual16.manual.hash.startsWith('h5'),
    JSON.stringify(manual16.manual))
  const runsBefore16b = await page.evaluate(() => window.__probe.moduleRuns('demo'))
  await setIndex([item('demo', 'h6'), tplItem]) // 换版本
  await injectChanged()
  await page.waitForFunction((n) => window.__probe.moduleRuns('demo') > n, runsBefore16b, { timeout: 8000 }).catch(() => {})
  const rec16 = await page.evaluate(() => window.__probe.health.record('plugin:demo'))
  check('换 hash 后：记录**仍在**且仍是手动禁用（不替用户撤销决定）',
    rec16?.disabled === true && rec16?.auto !== true, JSON.stringify(rec16))
  check('换 hash 后：这一版的账归零 + hash 换新 + lastError 清空',
    rec16?.count === 0 && typeof rec16?.hash === 'string' && rec16.hash.startsWith('h6') && rec16?.lastError === '',
    JSON.stringify(rec16))
  // 用户在墓碑上点「重新启用」之后：必须重新攒满 3 次才自动禁用（= 计数真的归零过，
  // 不是拿换版本前的旧账往上加）。三次全在同一次 evaluate 里做完。
  const seq16 = await page.evaluate(() => {
    const h = window.__probe.health
    h.enable('plugin:demo')
    const afterEnable = h.record('plugin:demo')
    h.fail('plugin:demo')
    h.fail('plugin:demo')
    const two = h.record('plugin:demo')
    h.fail('plugin:demo')
    const three = h.record('plugin:demo')
    return { afterEnable, two, three }
  })
  check('enableSurface 后记录被清空', seq16.afterEnable === null, JSON.stringify(seq16.afterEnable))
  check('连续失败 2 次**不**自动禁用（count=2, disabled:false）',
    seq16.two?.count === 2 && seq16.two?.disabled === false, JSON.stringify(seq16.two))
  check('第 3 次才自动禁用（count=3, disabled:true）',
    seq16.three?.count === 3 && seq16.three?.disabled === true, JSON.stringify(seq16.three))

  // ---- T14b known-ids 跨启动 ----
  console.log('\nT14b 第二次启动：known-ids 里少一个 id → 只清那一个')
  await page.evaluate((tplId) => {
    localStorage.setItem('demo:keep', '1')
    localStorage.setItem(`${tplId}:gone`, '1')
  }, tplManifest.id)
  const knownBefore14 = (await page.evaluate(() => window.__probe.knownIds()?.ids)) ?? []
  check('重启前 known-ids 同时含 demo 与模板 id',
    knownBefore14.includes('demo') && knownBefore14.includes(tplManifest.id), JSON.stringify(knownBefore14))
  // 「上一次会话」的 index：模板被卸载了、demo 还在。
  await page.evaluate((it) => localStorage.setItem('__probe.index', JSON.stringify({ version: 1, items: it })), [item('demo', 'h4')])
  await page.reload({ waitUntil: 'domcontentloaded' })
  await page.waitForFunction(() => !!window.__probe, null, { timeout: 30000 })
  await page.waitForFunction(() => window.__probe.surfaces.has('plugin:demo'), null, { timeout: 8000 }).catch(() => {})
  check('重启后 demo 被重新加载（对照：这一轮 sync 真的跑了）',
    await page.evaluate(() => window.__probe.surfaces.has('plugin:demo')))
  check('没消失的 id 不清：demo:keep 还在',
    (await page.evaluate(() => localStorage.getItem('demo:keep'))) === '1')
  check('消失的 id 被清（跨重启卸载的数据回收）',
    (await page.evaluate((tplId) => localStorage.getItem(`${tplId}:gone`), tplManifest.id)) === null)
  const knownAfter14 = (await page.evaluate(() => window.__probe.knownIds()?.ids)) ?? []
  check('known-ids 更新成当前 index（只剩 demo）', knownAfter14.length === 1 && knownAfter14[0] === 'demo', JSON.stringify(knownAfter14))

  // ---- T15 非权威 index（浏览器版）→ 一个字节都不动 ----
  console.log('\nT15 非权威 index（vite 直开 / 没有 preload）→ 不清理、不写 known-ids')
  await page.evaluate(() => {
    localStorage.setItem('demo:nonauth', '1')
    localStorage.setItem('ghost3:nonauth', '1') // 从没进过 known-ids：任何实现都不该碰它
  })
  const knownRaw15 = await page.evaluate(() => localStorage.getItem('hai-surface.known-ids'))
  // 缺省（不传 opts）必须是不权威 —— 这是唯一安全的缺省值。
  await page.evaluate(() => window.__probe.sync([], 'default'))
  check('缺省 opts + 空 index：demo 前缀的数据一个都没少',
    (await page.evaluate(() => localStorage.getItem('demo:nonauth'))) === '1')
  check('缺省 opts：known-ids 没被覆盖成空', (await page.evaluate(() => localStorage.getItem('hai-surface.known-ids'))) === knownRaw15,
    `before=${knownRaw15} after=${await page.evaluate(() => localStorage.getItem('hai-surface.known-ids'))}`)
  await page.evaluate(() => window.__probe.sync([], false))
  check('显式 authoritative:false：仍然不动数据',
    (await page.evaluate(() => localStorage.getItem('demo:nonauth'))) === '1')
  check('显式 false：known-ids 仍然没变', (await page.evaluate(() => localStorage.getItem('hai-surface.known-ids'))) === knownRaw15)
  // 对照：权威来源 + **空 index** 必须照常清理 —— 「卸载最后一个插件」就是这一档，
  // 所以这里不能用「items 非空」当权威性的替身。
  await page.evaluate(() => window.__probe.sync([], true))
  check('对照：权威 + 空 index 照常清（卸载最后一个插件）',
    (await page.evaluate(() => localStorage.getItem('demo:nonauth'))) === null)
  check('对照：从没进过 known-ids 的键照旧不动（ghost3:nonauth）',
    (await page.evaluate(() => localStorage.getItem('ghost3:nonauth'))) === '1')
  const knownAfter15 = (await page.evaluate(() => window.__probe.knownIds()?.ids)) ?? null
  check('对照：权威轮把 known-ids 更新成空（记账真的发生了）',
    Array.isArray(knownAfter15) && knownAfter15.length === 0, JSON.stringify(knownAfter15))

  // ---- T9 全程零 pageerror ----
  console.log('\nT9 全程零 pageerror')
  check('pageerror = 0', errs.length === 0, errs.join(' | '))

  await browser.close()
} catch (e) {
  if (isPrecondition(e)) {
    console.error(`\n[前置不满足] ${e.message}`)
    code = exitCodeOf(e)
  } else {
    console.error('\n[探针异常]', e)
    code = 1
  }
  cleanup()
  process.exit(code)
}
cleanup()
if (fails.length) {
  console.log(`\n❌ ${fails.length} 条断言失败：\n  - ${fails.join('\n  - ')}`)
  process.exit(1)
}
console.log('\n✅ 插件加载器全部通过')
process.exit(0)
