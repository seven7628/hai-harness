#!/usr/bin/env node
// `verify_plugin_entries.mjs` —— 插件入口闭环（R16；规格 docs/creator-mode-v0.1/07-plugin-entries.md）。
//
// 验收原句（D-2 的硬约束）：**任何已安装且 enabled 的插件，必须至少有一个可见入口** ——
// 不允许「加载成功但界面上什么都看不见」。
//
// 为什么这条必须单独一个探针：它测的既不是加载器（verify_plugin_loader 从不点菜单），
// 也不是持久化（verify_plugin_tab_persist 只开一个默认标签），而是三者之间的**入口闭环**：
// manifest.entries → 宿主三处入口 → openTab(params.entry) → 一插件多标签 → 禁用/卸载的可见性。
//
// 覆盖（每条都能被实现打死）：
//   T0 对照基线：四个内置 provider 与 ＋ 菜单的三个内置项都在；没插件时**没有**插件分组。
//   T1 ＋ 菜单：真模板（templates/surface，3 个 entries）→ 分组「面板插件」+ 三个标题
//     取自 manifest（总览/课表/资源，**不是** id）+ data-entry 正确 + 多插件时插件名小标。
//   T2 空态能力面板（tabs 为空时那块）：同一份入口也在；点「课表」→ 标签打开、aria-selected、
//      标题=课表、params.entry=schedule、面板画的是课表页。
//   T3 第二个入口：同一 kind 的第二个标签**并存**（身份含 params.entry），标题与页面各自正确。
//   T4 reload：两个标签都恢复，标题、entry、页面内容都对。
//   T5 零 entries 的插件：仍有一个默认入口（标题取 name；没有 name 时退到 id），能打开，
//      params.entry === 'default'。
//   T6 禁用：三处入口消失（菜单 + 空态），已开标签**不自动关**而是变墓碑（reason=missing，
//      标题仍是 manifest 的「课表」）。
//   T7 重新启用：入口回来、同一入口能再打开并渲染。
//   T8 设置页：「打开」按钮 → store.openTab({kind, params:{entry:'overview'}})（第一个 entry）；
//      每行有「入口在右栏 ＋ → 面板插件」提示；停用后按钮置灰、提示换成启用说明。
//   T8b 标签身份契约（PR-00 的 dedupeKey）：同 entry 同 params = 1 个标签；同 entry 不同 params
//      = 2 个；键序不同 = 同一个（稳定序列化）；provider 声明 dedupeKey → 按它的键合并。
//   T9 全程零 pageerror。
//
// 用法：cd desktop/app && node e2e/verify_plugin_entries.mjs
// 退出码：全通过 0；任一断言失败 1；前置不满足（端口被占 / 探针服务没起来）2。
//
// 端口 5231（5229 = PR-14c 跨重启、5230 = PR-14b 设置页）。
import { spawn } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'
import { exitCodeOf, isPrecondition, waitProbePage } from './_serve.mjs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const TEMPLATE_DIR = path.resolve(APP, '../../templates/surface')

const PORT = 5231
const PROBE_DIR = path.join(__dirname, '_pluginentriesprobe_run')
const PROBE_MARKER = '<title>plugin-entries probe</title>'
const PROBE_URL = `http://localhost:${PORT}/e2e/_pluginentriesprobe_run/index.html`
const REL_PROBE = 'e2e/_pluginentriesprobe_run' // 同源静态插件目录（相对 APP，供 vite 直接吃）
const WS = '/mock/proj-plugin-entries'

// —— 插件资产（真的静态文件：loader 的 `import()` 必须真的能拉到它们）——
//
// 三个插件刻意是三种形状，缺一个都测不到 R16 的契约：
//   · cost-board = **仓里的真模板**（3 个 entries）：测「manifest → 三处入口 → 一插件多标签」；
//   · plain      = 零 entries、**不声明 dedupeKey**：测「零入口必有一个默认入口」+ 缺省身份键
//                  （entry + 其余 params 的稳定序列化）；
//   · ctx        = 零 entries、**声明 dedupeKey**：测「provider 说了算」——PR-00 就写在契约里、
//                  R16 第一版把它忽略掉的字段。
const PLUGINS = path.join(PROBE_DIR, 'plugins')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.rmSync(PLUGINS, { recursive: true, force: true })
const tplManifest = JSON.parse(fs.readFileSync(path.join(TEMPLATE_DIR, 'surface.json'), 'utf8'))
fs.mkdirSync(path.join(PLUGINS, tplManifest.id), { recursive: true })
for (const f of fs.readdirSync(TEMPLATE_DIR)) {
  if (f === 'surface.json') continue
  fs.copyFileSync(path.join(TEMPLATE_DIR, f), path.join(PLUGINS, tplManifest.id, f))
}
fs.mkdirSync(path.join(PLUGINS, 'ctx'), { recursive: true })
fs.writeFileSync(path.join(PLUGINS, 'ctx', 'panel.js'), `
// 声明了 dedupeKey 的插件：标签身份归 provider（PR-00 的契约）。
// 同一 courseId = 同一份上下文 → 合并成一个标签；换 courseId 才开新标签。
export const surface = {
  kind: 'plugin:ctx',
  apply({ React, surfaces }) {
    surfaces.register({
      kind: 'plugin:ctx',
      group: 'plugin',
      order: 30,
      title: () => '上下文面板',
      dedupeKey: (tab) => String((tab.params || {}).courseId || ''),
      render: ({ tab }) => React.createElement(
        'div',
        { 'data-surface': 'ctx', 'data-params': JSON.stringify(tab.params ?? null) },
        'params=' + JSON.stringify(tab.params ?? null),
      ),
    })
  },
}
`, { recursive: true })
fs.mkdirSync(path.join(PLUGINS, 'plain'), { recursive: true })
fs.writeFileSync(path.join(PLUGINS, 'plain', 'panel.js'), `
// 零 entries 的插件：宿主必须给它一个默认入口（标题取 name → id），
// params.entry === 'default'。面板把 params 原样印出来 —— 断言「入口真的传了 entry」。
export const surface = {
  kind: 'plugin:plain',
  apply({ React, surfaces }) {
    surfaces.register({
      kind: 'plugin:plain',
      group: 'plugin',
      order: 20,
      title: () => '零入口面板',
      render: ({ tab }) => React.createElement(
        'div',
        { 'data-surface': 'plain', 'data-params': JSON.stringify(tab.params ?? null) },
        'params=' + JSON.stringify(tab.params ?? null),
      ),
    })
  },
}
`, { recursive: true })

// —— 探针页 ——
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8">'
  + `<title>plugin-entries probe</title></head>`
  + '<body><div id="root" style="height:100vh"></div>'
  + '<div id="settings-root" style="max-height:60vh;overflow:auto"></div>'
  + '<script type="module" src="./main.tsx"></script></body></html>')

// 探针页职责（§4.7：只替 IPC 边界，store/RightPanel/注册表/loader/SurfacesPage 全是真的）：
//   · `surfaceIndex` 的内容住在 localStorage（跨 reload 还在，等价于「index 归主进程所有」）；
//   · `surfaceSetEnabled` 真的翻那一份 index（设置页点开关后重拉能看到新值）；
//   · `surfaces_changed` 由测试显式注入（桥在动作后推它，探针没有那条通道）。
//   · 设置页挂在第二个 root 上：它和右栏是同一棵树里的两块 UI，共用同一个 store。
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
// ⚠️ 全部用 await import：store 在模块求值时就 const transport = getTransport() 把实现定死了，
// 而生产里 window.desktop 由 preload **先于** bundle 注入（见 verify_plugin_loader.mjs 同一段）。
import '../../src/index.css'

const feeds: Array<(line: string) => void> = []
const openCalls: Array<unknown> = []
const readIndex = (): unknown => {
  try {
    const raw = localStorage.getItem('__probe.pluginIndex')
    return raw ? JSON.parse(raw) : { version: 1, items: [] }
  } catch { return { version: 1, items: [] } }
}
let pluginIndex: { version: number; items: Array<Record<string, unknown>> } = readIndex() as { version: number; items: Array<Record<string, unknown>> }
const writeIndex = () => localStorage.setItem('__probe.pluginIndex', JSON.stringify(pluginIndex))

const stub: Record<string, unknown> = {
  startBridge: async () => ({ ok: true }),
  command: async () => ({ ok: true }),
  onEvent: (cb: (line: string) => void) => {
    feeds.push(cb)
    return () => { const i = feeds.indexOf(cb); if (i >= 0) feeds.splice(i, 1) }
  },
  onExit: () => () => {},
  settingsGet: async () => ({}),
  settingsSet: async () => ({ ok: true }),
  startupMark: () => {},
  // ↓ 本探针的 IPC 边界（只替 surface 这几条）
  surfaceIndex: async () => pluginIndex,
  surfaceSetEnabled: async (id: string, enabled: boolean) => {
    pluginIndex = { ...pluginIndex, items: pluginIndex.items.map((i) => (i.id === id ? { ...i, enabled } : i)) }
    writeIndex()
    return { ok: true, id, enabled }
  },
  surfaceRemove: async (id: string) => {
    pluginIndex = { ...pluginIndex, items: pluginIndex.items.filter((i) => i.id !== id) }
    writeIndex()
    return { ok: true, id, status: 'applied' }
  },
  surfaceAudit: async () => ({ ok: true, lines: [] }),
  surfaceCommand: async () => ({ ok: true, data: { workspaces: [{ path: '/mock', title: 'mock' }] } }),
}
;(window as unknown as { desktop: unknown }).desktop = new Proxy(stub, {
  get(t, k) {
    if (k in t) return t[k as string]
    return () => Promise.resolve({ ok: true })
  },
})

const { createRoot } = await import('react-dom/client')
const [{ default: RightPanel }, { default: SurfacesPage }] = await Promise.all([
  import('../../src/components/RightPanel'),
  import('../../src/components/SurfacesPage'),
])
await import('../../src/surfaces/builtin') // 副作用注册（F45）
const { getSurface, listSurfaces } = await import('../../src/surfaces/registry')
const { setPluginBaseUrlForTest } = await import('../../src/surfaces/plugin/loader')
setPluginBaseUrlForTest((id) => \`/${REL_PROBE}/plugins/\${id}\`)
const { useAppStore } = await import('../../src/store/useAppStore')

const WS = ${JSON.stringify(WS)}
useAppStore.setState({ workspace: WS })
// 右栏默认收着（生产里由用户点开）：本探针要看的正是右栏里的入口，先展开。
useAppStore.setState({ rightLayout: { open: true, bias: 'even', ratio: 50 }, rightExpanded: true })

const w = window as unknown as Record<string, unknown>
const scope = () => {
  const s = useAppStore.getState()
  return s.activeSessionId || s.workspace || ''
}
const tabs = () => {
  const s = useAppStore.getState()
  return (s.tabsByScope[scope()]?.tabs ?? []).map((t) => ({
    kind: (t as { kind: string }).kind,
    entry: ((t as { params?: Record<string, unknown> }).params ?? {}).entry,
    params: (t as { params?: unknown }).params,
    active: (t as { id: string }).id === s.tabsByScope[scope()]?.activeId,
  }))
}
w.__probe = {
  store: useAppStore,
  setIndex: (items: unknown[]) => { pluginIndex = { version: 1, items: items as Array<Record<string, unknown>> }; writeIndex() },
  inject: (line: string) => feeds.forEach((f) => f(line)),
  surfaces: { list: () => listSurfaces().map((p) => p.kind), has: (k: string) => !!getSurface(k as never) },
  tabs,
  activeKind: () => tabs().find((t) => t.active)?.kind ?? null,
  closeAll: () => {
    const s = useAppStore.getState()
    for (const t of (s.tabsByScope[scope()]?.tabs ?? [])) s.closeTab((t as { id: string }).id)
  },
  // 设置页的「打开」按钮走 store.openTab：spy 一下拿到**真实参数**（断言的是发出去的 spec，
  // 不是「标签出现了」这一个副作用 —— 后者在参数丢了的时候也可能因为别处写错而绿）。
  spyOpenTab: () => {
    const real = useAppStore.getState().openTab
    useAppStore.setState({
      openTab: (spec: unknown, opts?: { activate?: boolean }) => {
        openCalls.push(spec)
        return real(spec as never, opts)
      },
    })
  },
  openCalls: () => openCalls.map((c) => JSON.parse(JSON.stringify(c))),
  resetOpenCalls: () => { openCalls.length = 0 },
  // 直接按 spec 打开（T8b 要带**其它** params，菜单只给 entry）——
  // 走的仍是生产动作 openTab，不 setState 造标签。
  openSpec: (spec: unknown) => useAppStore.getState().openTab(spec as never),
}

void useAppStore.getState().start(WS)
createRoot(document.getElementById('root')!).render(<RightPanel />)
createRoot(document.getElementById('settings-root')!).render(<SurfacesPage />)
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

// —— 索引条目的两种形状 ——
// tpl：**从真 manifest 派生**（id/kind/entry/icon/commands/data/name/entries 全部照抄），
// 只有 hash 是探针给的（生产里由安装器算 sha256）。
const tplItem = (extra = {}) => ({
  id: tplManifest.id,
  version: tplManifest.version,
  hash: 'htpl'.padEnd(64, '1'),
  enabled: true,
  kind: tplManifest.kind,
  name: tplManifest.name?.zh ?? '',
  commands: tplManifest.commands || [],
  data: tplManifest.data || [],
  entry: tplManifest.entry || 'panel.js',
  ...(tplManifest.icon ? { icon: tplManifest.icon } : {}),
  entries: tplManifest.entries || [],
  ...extra,
})
const plainItem = (extra = {}) => ({
  id: 'plain',
  version: '1.0.0',
  hash: 'hpln'.padEnd(64, '2'),
  enabled: true,
  kind: 'plugin:plain',
  name: '零入口面板',
  commands: [],
  data: [],
  entry: 'panel.js',
  entries: [],
  ...extra,
})
const ctxItem = (extra = {}) => ({
  id: 'ctx',
  version: '1.0.0',
  hash: 'hctx'.padEnd(64, '3'),
  enabled: true,
  kind: 'plugin:ctx',
  name: '上下文面板',
  commands: [],
  data: [],
  entry: 'panel.js',
  entries: [],
  ...extra,
})
const TPL_KIND = `plugin:${tplManifest.id}`
const TPL_TITLES = (tplManifest.entries || []).map((e) => e.title)

let code = 0
try {
  await waitProbePage(PROBE_URL, { marker: PROBE_MARKER, probe: 'e2e:plugin-entries', child: server })
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } })
  page.setDefaultTimeout(5000)
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))

  // 右栏展开：store.start() 会用 settings 里的布局覆盖模块初值（生产里这是用户的偏好），
  // 所以每次 boot/reload 之后都要显式展开一次 —— 收起态只有一条 rail，看不到任何入口。
  const expand = () => page.evaluate(() => window.__probe.store.setState({
    rightLayout: { open: true, bias: 'even', ratio: 50 },
    rightExpanded: true,
  }))
  const surfaces = () => page.evaluate(() => window.__probe.surfaces.list())
  const hasKind = (k) => page.evaluate((kind) => window.__probe.surfaces.has(kind), k)
  const setIndex = (items) => page.evaluate((it) => window.__probe.setIndex(it), items)
  const injectChanged = () => page.evaluate(() => window.__probe.inject(JSON.stringify({ type: 'ui', op: 'surfaces_changed', data: { at: Date.now() } })))
  const probeTabs = () => page.evaluate(() => window.__probe.tabs())
  const closeAllTabs = () => page.evaluate(() => window.__probe.closeAll())
  const waitKindGone = (k) => page.waitForFunction((kind) => !window.__probe.surfaces.has(kind), k, { timeout: 8000 }).catch(() => {})

  // ＋ 菜单的开关与读法（菜单是**唯一**的读点：断言的是人看到的那一份 DOM）
  const menuItems = () => page.locator('.rtabs-menu.rtabs-menu-right [data-plugin-entry]')
  const menuItem = (key) => page.locator(`.rtabs-menu.rtabs-menu-right [data-plugin-entry="${key}"]`)
  const emptyItems = () => page.locator('.rtabs-empty-plugin [data-plugin-entry]')
  const emptyItem = (key) => page.locator(`.rtabs-empty-plugin [data-plugin-entry="${key}"]`)
  const openMenu = async () => {
    if (await page.locator('.rtabs-menu.rtabs-menu-right').count()) return
    await page.locator('.rtabs-add').click()
    await page.waitForSelector('.rtabs-menu.rtabs-menu-right')
  }
  const closeMenu = async () => {
    if (!(await page.locator('.rtabs-menu.rtabs-menu-right').count())) return
    await page.locator('.rtabs-backdrop').click({ force: true })
    await page.waitForSelector('.rtabs-menu.rtabs-menu-right', { state: 'detached' }).catch(() => {})
  }
  const tabOf = (entry) => page.locator(`.rtab[data-kind="${TPL_KIND}"][data-entry="${entry}"]`)
  // reload = 真的重启渲染进程那一份状态（标签集与索引都从 localStorage 恢复）
  const reload = async () => {
    await page.reload({ waitUntil: 'domcontentloaded' })
    await page.waitForFunction(() => !!window.__probe, null, { timeout: 30000 })
    await expand()
    await page.waitForFunction((kinds) => kinds.every((k) => window.__probe.surfaces.has(k)), [TPL_KIND, 'plugin:plain', 'plugin:ctx'], { timeout: 8000 }).catch(() => {})
  }

  // 干净起点：**不用** addInitScript 清 localStorage（那会在每次 reload 时把标签集与索引一起抹掉，
  // T4 的「重启恢复」就永远测不到）。改成页内清一次 + reload 一次，让 store 的模块加载期恢复
  // 读到干净存储（与 verify_plugin_tab_persist.mjs 的 boot 同一条口径）。
  await page.goto(PROBE_URL, { waitUntil: 'domcontentloaded' })
  await page.waitForFunction(() => !!window.__probe, null, { timeout: 30000 })
  await page.evaluate(() => { try { localStorage.clear() } catch { /* 忽略 */ } })
  await page.reload({ waitUntil: 'domcontentloaded' })
  await page.waitForFunction(() => !!window.__probe, null, { timeout: 30000 })
  await expand()
  await page.waitForSelector('.rtabs-add')

  // ---- T0 对照基线：内置 provider / 内置菜单项 / 没有插件分组 ----
  console.log('\nT0 对照基线（内置四类都在，没装插件时没有插件分组）')
  const baseKinds = await surfaces()
  check('四个内置 provider 都注册了',
    ['activity', 'git', 'file', 'web'].every((k) => baseKinds.includes(k)), JSON.stringify(baseKinds))
  await openMenu()
  check('＋ 菜单内置项仍是 3 个（ADDABLE_KINDS 语义不变）',
    (await page.locator('.rtabs-menu.rtabs-menu-right > .rtabs-mi[data-kind]:not([data-plugin-entry])').count()) === 3,
    String(await page.locator('.rtabs-menu.rtabs-menu-right > .rtabs-mi[data-kind]:not([data-plugin-entry])').count()))
  check('没装插件时没有「面板插件」分组', (await page.locator('[data-plugin-entries]').count()) === 0)
  await closeMenu()

  // ---- T1 ＋ 菜单：manifest.entries → 分组里的三条 ----
  console.log('\nT1 ＋ 菜单：真模板的 3 个 entries + 零入口插件的默认入口')
  await setIndex([tplItem(), plainItem(), ctxItem()])
  await injectChanged()
  await page.waitForFunction((kinds) => kinds.every((k) => window.__probe.surfaces.has(k)), [TPL_KIND, 'plugin:plain', 'plugin:ctx'], { timeout: 8000 }).catch(() => {})
  check('三个插件都进了注册表', (await hasKind(TPL_KIND)) && (await hasKind('plugin:plain')) && (await hasKind('plugin:ctx')))
  await openMenu()
  check('＋ 菜单出现「面板插件」分组',
    (await page.locator('.rtabs-menu.rtabs-menu-right [data-plugin-entries]').count()) === 1)
  check('分组名 = 面板插件',
    (await page.locator('.rtabs-menu.rtabs-menu-right [data-plugin-entries]').innerText()).trim() === '面板插件',
    await page.locator('.rtabs-menu.rtabs-menu-right [data-plugin-entries]').innerText())
  const itemCount = await menuItems().count()
  check('入口 5 个（模板 3 + 两个零入口插件各 1）', itemCount === 5, String(itemCount))
  const menuTitles = (await menuItems().allInnerTexts()).map((s) => s.trim())
  check('前三个标题取自 manifest（总览/课表/资源）',
    JSON.stringify(menuTitles.slice(0, 3)) === JSON.stringify(TPL_TITLES), JSON.stringify(menuTitles))
  const menuKeys = await menuItems().evaluateAll((ns) => ns.map((n) => n.getAttribute('data-plugin-entry')))
  check('data-plugin-entry 顺序 = 模板三项 + plain/default + ctx/default',
    JSON.stringify(menuKeys) === JSON.stringify([`${tplManifest.id}/overview`, `${tplManifest.id}/schedule`, `${tplManifest.id}/resources`, 'plain/default', 'ctx/default']),
    JSON.stringify(menuKeys))
  const menuEntries = await menuItems().evaluateAll((ns) => ns.map((n) => n.getAttribute('data-entry')))
  check('data-entry 来自 manifest.entries[].id（零入口插件是 default）',
    JSON.stringify(menuEntries) === JSON.stringify(['overview', 'schedule', 'resources', 'default', 'default']),
    JSON.stringify(menuEntries))
  check('多插件时有插件名小标（3 个）',
    (await page.locator('.rtabs-menu.rtabs-menu-right [data-plugin-group]').count()) === 3)
  const groupLabels = await page.locator('.rtabs-menu.rtabs-menu-right [data-plugin-group]').allInnerTexts()
  check('小标 = 插件显示名（成本看板 / 零入口面板 / 上下文面板）',
    JSON.stringify(groupLabels.map((s) => s.trim())) === JSON.stringify(['成本看板', '零入口面板', '上下文面板']),
    JSON.stringify(groupLabels))

  // ---- T2 空态能力面板：同一份入口 + 从这里点开「课表」 ----
  console.log('\nT2 空态能力面板：入口也在；点「课表」→ 标签打开、标题/entry/页面都对')
  await closeMenu()
  await closeAllTabs()
  await page.waitForSelector('.rtabs-empty-plugin')
  check('空态能力面板出现插件分组', (await page.locator('.rtabs-empty-plugin[data-plugin-entries]').count()) === 1)
  check('空态里也是 5 个入口', (await emptyItems().count()) === 5, String(await emptyItems().count()))
  const emptyTitles = (await emptyItems().allInnerTexts()).map((s) => s.trim())
  check('空态标题与菜单同源（总览/课表/资源/零入口面板/上下文面板）',
    JSON.stringify(emptyTitles) === JSON.stringify([...TPL_TITLES, '零入口面板', '上下文面板']), JSON.stringify(emptyTitles))
  await emptyItem(`${tplManifest.id}/schedule`).click()
  await page.waitForSelector(`.rtab[data-kind="${TPL_KIND}"][data-entry="schedule"]`)
  const schTab = tabOf('schedule')
  check('点「课表」→ 标签打开', (await schTab.count()) === 1)
  check('该标签 aria-selected = true', (await schTab.getAttribute('aria-selected')) === 'true')
  check('标签标题 = 课表（manifest 的 title，不是 entry id）',
    (await schTab.locator('.rtab-t').innerText()).trim() === '课表',
    await schTab.locator('.rtab-t').innerText())
  const schTabState = (await probeTabs()).find((t) => t.kind === TPL_KIND)
  check('store 里 params.entry = schedule', schTabState?.entry === 'schedule', JSON.stringify(schTabState))
  await page.waitForSelector('[data-surface="cost-board"][data-entry="schedule"]')
  check('面板画的是课表页（tab.params.entry 分发）',
    (await page.locator('[data-surface="cost-board"][data-entry="schedule"]').innerText()).includes('本学期课表'))

  // ---- T3 第二个入口 = 第二个标签（同一 kind 并存）----
  console.log('\nT3 点「资源」→ 同一插件的第二个标签并存')
  await openMenu()
  await menuItem(`${tplManifest.id}/resources`).click()
  await page.waitForSelector('[data-surface="cost-board"][data-entry="resources"]')
  check('同一 kind 有两个插件标签', (await page.locator(`.rtab[data-kind="${TPL_KIND}"]`).count()) === 2,
    String(await page.locator(`.rtab[data-kind="${TPL_KIND}"]`).count()))
  check('两个标签的标题分别 = 课表 / 资源',
    JSON.stringify((await page.locator(`.rtab[data-kind="${TPL_KIND}"] .rtab-t`).allInnerTexts()).map((s) => s.trim()))
      === JSON.stringify(['课表', '资源']),
    JSON.stringify(await page.locator(`.rtab[data-kind="${TPL_KIND}"] .rtab-t`).allInnerTexts()))
  const twoTabs = (await probeTabs()).filter((t) => t.kind === TPL_KIND)
  check('store 里两个标签的 entry = schedule / resources',
    JSON.stringify(twoTabs.map((t) => t.entry)) === JSON.stringify(['schedule', 'resources']), JSON.stringify(twoTabs))
  check('资源页内容 = 课程资源',
    (await page.locator('[data-surface="cost-board"][data-entry="resources"]').innerText()).includes('课程资源'))

  // ---- T4 reload：两个标签都恢复 ----
  console.log('\nT4 reload：两个标签都恢复（标题 / entry / 页面）')
  await reload()
  const after = (await probeTabs()).filter((t) => t.kind === TPL_KIND)
  check('reload 后两个插件标签都恢复', after.length === 2, JSON.stringify(after))
  check('恢复的 entry 仍是 schedule / resources',
    JSON.stringify(after.map((t) => t.entry)) === JSON.stringify(['schedule', 'resources']), JSON.stringify(after))
  check('恢复的标签标题仍是 课表 / 资源',
    JSON.stringify((await page.locator(`.rtab[data-kind="${TPL_KIND}"] .rtab-t`).allInnerTexts()).map((s) => s.trim()))
      === JSON.stringify(['课表', '资源']),
    JSON.stringify(await page.locator(`.rtab[data-kind="${TPL_KIND}"] .rtab-t`).allInnerTexts()))
  await tabOf('schedule').click()
  await page.waitForSelector('[data-surface="cost-board"][data-entry="schedule"]')
  check('恢复后点「课表」标签，画的是课表页',
    (await page.locator('[data-surface="cost-board"][data-entry="schedule"]').innerText()).includes('本学期课表'))

  // ---- T5 零 entries：默认入口 ----
  console.log('\nT5 零 entries 的插件：默认入口（标题 name → id，params.entry=default）')
  await openMenu()
  const plainLabel = (await menuItem('plain/default').innerText()).trim()
  check('零入口插件有一个默认入口，标题 = manifest.name', plainLabel === '零入口面板', plainLabel)
  await menuItem('plain/default').click()
  await page.waitForSelector('[data-surface="plain"]')
  const plainTabs = (await probeTabs()).filter((t) => t.kind === 'plugin:plain')
  check('默认入口能打开且 params.entry = default',
    plainTabs.length === 1 && plainTabs[0].entry === 'default', JSON.stringify(plainTabs))
  check('默认入口的面板真的渲染了（且读到了 params）',
    (await page.locator('[data-surface="plain"]').getAttribute('data-params')) === JSON.stringify({ entry: 'default' }),
    await page.locator('[data-surface="plain"]').getAttribute('data-params'))
  // 没有 name 时标题退到 id：同一个插件换一份没有 name 的 index 即可
  await setIndex([tplItem(), plainItem({ name: '' }), ctxItem()])
  await injectChanged()
  await openMenu()
  // 有界软等待：快照更新是 loader 那一轮 sync 的副作用（readIndex → 微任务），
  // 菜单在同一毫秒内打开时可能读到旧快照；这里等「标题真的变了」，但超时不抛。
  await page.waitForFunction(
    (sel) => document.querySelector(sel)?.textContent?.trim() === 'plain',
    '.rtabs-menu.rtabs-menu-right [data-plugin-entry="plain/default"]',
    { timeout: 4000 },
  ).catch(() => {})
  const idLabel = (await menuItem('plain/default').innerText()).trim()
  check('没有 name 时默认入口标题退到 id（plain）', idLabel === 'plain', idLabel)
  await closeMenu()

  // ---- T6 禁用：三处入口消失，标签变墓碑（不自动关）----
  console.log('\nT6 禁用插件：入口立刻消失，已开标签走既有墓碑机制')
  await setIndex([tplItem({ enabled: false }), plainItem({ name: '' }), ctxItem()])
  await injectChanged()
  await waitKindGone(TPL_KIND)
  check('禁用后 provider 已卸下（入口消失的来源）', !(await hasKind(TPL_KIND)))
  await openMenu()
  check('菜单里该插件的三个入口都没了',
    (await page.locator(`.rtabs-menu.rtabs-menu-right [data-plugin-entry^="${tplManifest.id}/"]`).count()) === 0)
  check('其它插件的入口还在（只黑自己）', (await menuItem('plain/default').count()) === 1)
  check('已开标签没有被另外关掉（仍是 2 个）',
    (await page.locator(`.rtab[data-kind="${TPL_KIND}"]`).count()) === 2)
  await closeMenu()
  await tabOf('schedule').click()
  await page.waitForSelector(`.surface-tombstone[data-kind="${TPL_KIND}"]`)
  const tomb = page.locator(`.surface-tombstone[data-kind="${TPL_KIND}"]`)
  check('禁用后标签变墓碑（reason = missing）', (await tomb.getAttribute('data-reason')) === 'missing',
    await tomb.getAttribute('data-reason'))
  check('墓碑标题仍是 manifest 的「课表」（不是 kind 字面量）',
    (await tomb.locator('.surface-tombstone-title').innerText()).trim() === '课表',
    await tomb.locator('.surface-tombstone-title').innerText())
  await closeAllTabs()
  await page.waitForSelector('.rtabs-empty-plugin')
  check('空态能力面板里该插件的入口也消失了',
    (await page.locator(`.rtabs-empty-plugin [data-plugin-entry^="${tplManifest.id}/"]`).count()) === 0)
  check('空态里仍留着其它零入口插件的默认入口', (await emptyItem('plain/default').count()) === 1)

  // ---- T7 重新启用：入口回来 ----
  console.log('\nT7 重新启用：入口回来，同一入口能再打开')
  await setIndex([tplItem(), plainItem({ name: '' }), ctxItem()])
  await injectChanged()
  await page.waitForFunction((k) => window.__probe.surfaces.has(k), TPL_KIND, { timeout: 8000 }).catch(() => {})
  await openMenu()
  check('重新启用后三个入口都回来',
    (await page.locator(`.rtabs-menu.rtabs-menu-right [data-plugin-entry^="${tplManifest.id}/"]`).count()) === 3)
  await menuItem(`${tplManifest.id}/schedule`).click()
  await page.waitForSelector('[data-surface="cost-board"][data-entry="schedule"]')
  check('重新启用后「课表」能再打开并渲染',
    (await page.locator('[data-surface="cost-board"][data-entry="schedule"]').innerText()).includes('本学期课表'))

  // ---- T8 设置页：「打开」按钮 = 第一个 entry + 入口提示 ----
  console.log('\nT8 设置页：每行「打开」发到 store（第一个 entry）+ 入口提示')
  await reload()
  await page.waitForSelector('[data-surface-id]')
  check('设置页列出三个插件', (await page.locator('[data-surface-id]').count()) === 3,
    String(await page.locator('[data-surface-id]').count()))
  check('每行都有「打开」按钮', (await page.locator('[data-surface-open]').count()) === 3)
  check('行内提示 = 入口在右栏 ＋ → 面板插件',
    (await page.locator(`[data-surface-id="${tplManifest.id}"] [data-surface-open-hint]`).innerText())
      .replace(/\s+/g, ' ').trim() === '入口在右栏 ＋ → 面板插件',
    await page.locator(`[data-surface-id="${tplManifest.id}"] [data-surface-open-hint]`).innerText())
  await page.evaluate(() => window.__probe.resetOpenCalls())
  await page.evaluate(() => window.__probe.spyOpenTab())
  await page.locator(`[data-surface-open="${tplManifest.id}"]`).click()
  const calls = await page.evaluate(() => window.__probe.openCalls())
  check('「打开」发给 store 的 spec = { kind, params: { entry: overview } }',
    calls.length === 1
      && calls[0]?.kind === TPL_KIND
      && JSON.stringify(calls[0]?.params) === JSON.stringify({ entry: 'overview' }),
    JSON.stringify(calls))
  const opened = (await probeTabs()).filter((t) => t.kind === TPL_KIND)
  check('store 里真的出现了 entry=overview 的标签', opened.some((t) => t.entry === 'overview'), JSON.stringify(opened))
  await page.waitForSelector('[data-surface="cost-board"][data-entry="overview"]')
  check('总览页渲染了（模板的总览页 = 成本看板）',
    (await page.locator('[data-surface="cost-board"][data-entry="overview"]').innerText()).includes('成本看板'))
  // 设置页自己停用 → 按钮置灰、提示换成启用说明（入口这时已经从右栏消失）
  await page.locator(`[data-surface-id="${tplManifest.id}"] .switch`).click()
  await page.waitForFunction((id) => document.querySelector(`[data-surface-id="${id}"]`)?.getAttribute('data-enabled') === '0', tplManifest.id, { timeout: 8000 }).catch(() => {})
  await injectChanged()
  check('停用后「打开」按钮置灰',
    await page.locator(`[data-surface-open="${tplManifest.id}"]`).isDisabled())
  check('停用后提示改成「启用后入口才会回到右栏」',
    (await page.locator(`[data-surface-id="${tplManifest.id}"] [data-surface-open-hint]`).innerText()).includes('启用后入口才会回到右栏'),
    await page.locator(`[data-surface-id="${tplManifest.id}"] [data-surface-open-hint]`).innerText())

  // ---- T8b 同一入口的不同上下文：PR-00 的 dedupeKey 契约 ----
  console.log('\nT8b 标签身份：provider.dedupeKey 优先；缺省 = entry + 其余 params 的稳定序列化')
  const openSpec = (spec) => page.evaluate((s) => window.__probe.openSpec(s), spec)
  const kindTabs = (kind) => page.evaluate((k) => window.__probe.tabs().filter((t) => t.kind === k), kind)
  // (1) 同一 entry、同一份 params → 只有一个标签（幂等键命中）
  await openSpec({ kind: 'plugin:plain', params: { entry: 'default', fileId: 42 } })
  await openSpec({ kind: 'plugin:plain', params: { entry: 'default', fileId: 42 } })
  let plainCtx = await kindTabs('plugin:plain')
  check('同一 entry、同 params → 1 个标签（幂等）', plainCtx.length === 1, JSON.stringify(plainCtx))
  // (2) 同一 entry、fileId 不同 → 2 个（R16 第一版只看 entry，这条是**被修掉的 bug**）
  await openSpec({ kind: 'plugin:plain', params: { entry: 'default', fileId: 7 } })
  plainCtx = await kindTabs('plugin:plain')
  check('同一 entry、fileId 不同 → 2 个标签（缺省身份含其余 params）', plainCtx.length === 2, JSON.stringify(plainCtx))
  check('两个标签各自保留自己的 params（42 / 7）',
    JSON.stringify(plainCtx.map((t) => t.params?.fileId).sort()) === JSON.stringify([42, 7]),
    JSON.stringify(plainCtx.map((t) => t.params)))
  // (3) 键序不影响身份：{a:1,b:2} 与 {b:2,a:1} 是同一份数据
  await openSpec({ kind: 'plugin:plain', params: { entry: 'default', a: 1, b: 2 } })
  await openSpec({ kind: 'plugin:plain', params: { entry: 'default', b: 2, a: 1 } })
  plainCtx = await kindTabs('plugin:plain')
  check('params 键序不同 → 仍是 3 个标签（稳定序列化，不是 4 个）', plainCtx.length === 3, JSON.stringify(plainCtx))
  // (4) provider 声明 dedupeKey：按 courseId 合并，fileId 不进身份
  await openSpec({ kind: 'plugin:ctx', params: { entry: 'default', courseId: 'c1', fileId: 1 } })
  await openSpec({ kind: 'plugin:ctx', params: { entry: 'default', courseId: 'c1', fileId: 2 } })
  let ctxTabs = await kindTabs('plugin:ctx')
  check('provider.dedupeKey 生效：同 courseId、不同 fileId → 1 个标签', ctxTabs.length === 1, JSON.stringify(ctxTabs))
  check('合并后标签的 params 是最新那次（fileId=2）', ctxTabs[0]?.params?.fileId === 2, JSON.stringify(ctxTabs))
  await openSpec({ kind: 'plugin:ctx', params: { entry: 'default', courseId: 'c2', fileId: 3 } })
  ctxTabs = await kindTabs('plugin:ctx')
  check('dedupeKey 不同（courseId 变了）→ 第二个标签', ctxTabs.length === 2, JSON.stringify(ctxTabs))

  // ---- T9 全程零 pageerror ----
  console.log('\nT9 全程零 pageerror')
  check('pageerror = 0', errs.length === 0, errs.join(' | '))

  await browser.close()
} catch (e) {
  if (isPrecondition(e)) {
    console.error(`\n前置不满足：${e.message}`)
    code = exitCodeOf(e)
  } else {
    console.error('\n探针异常：', e)
    code = 1
  }
}

console.log('')
if (fails.length) {
  console.log(`❌ ${fails.length} 项失败：\n  - ${fails.join('\n  - ')}`)
  code = code || 1
} else if (code === 0) {
  console.log('✅ 全部通过（verify_plugin_entries）')
}
cleanup()
process.exit(code)
