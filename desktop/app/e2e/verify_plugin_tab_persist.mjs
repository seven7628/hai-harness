#!/usr/bin/env node
// `verify_plugin_tab_persist.mjs` —— 插件标签的**跨重启持久化**（PR-14c；设计 §5.8-A 的硬验收）。
//
// 验收原句：**装一个插件 → 打开它的标签 → 重启（reload）→ 标签还在、面板照常渲染**。
//
// 为什么这条必须单独一个探针（而不是塞进 verify_session_tabs_persist / verify_plugin_loader）：
//   · 前者验的是**内置四类**的恢复（那里一条 plugin: 都不能被恢复 —— 白名单口径不同）；
//   · 后者验的是加载器（index → import → apply → 注册表），它从不重启页面，
//     所以「重启后标签还在吗」在它那里是**不可测**的；
//   · 两者的**交叉点**才是本 PR 的 bug：标签在模块加载时同步恢复，而 provider 要等
//     `startPluginLoader`（异步、在 bridge running 之后）才注册 —— 恢复出来的一瞬必然是
//     `unavailable: 'missing'` 墓碑，provider 注册后必须自动变回面板。
//
// 覆盖（每条都能被实现打死）：
//   T0 自检：四个内置 provider 都在（对照基线；缺了它下面「内置不受影响」无从谈起）。
//   T1 写入：openTab('plugin:demo', {a:1}) → 真面板渲染 + 落盘形态（kind + params，版本号仍是 1）。
//   T2 重启恢复：**gate 住 surfaceIndex**（确定性复现「标签已恢复、provider 还没注册」那一瞬）
//      → 标签在、aria-selected 对、**墓碑**在；放行索引 → 墓碑消失、面板真的渲染出来、
//      params 往返（面板读到的还是 {a:1}）。
//   T3 超大 params（> 4 KiB）：那条标签**不恢复**（宁可不恢复，也不存半截参数），
//      其余标签照常恢复，零 pageerror。
//   T4 卸载后重启：标签仍在但是**墓碑**（reason=missing），**绝不**变成「空白 web 标签」。
//   T5 全程零 pageerror。
//
// 用法：cd desktop/app && node e2e/verify_plugin_tab_persist.mjs
// 退出码：全通过 0；任一断言失败 1；前置不满足（端口被占 / 探针服务没起来）2。
import { spawn } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'
import { exitCodeOf, isPrecondition, waitProbePage } from './_serve.mjs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')

// 端口按口径固定 5229（5227 = PR-12 的 ui 快照探针，5228 = PR-14 的插件加载器探针）。
const PORT = 5229
const PROBE_DIR = path.join(__dirname, '_plugintabprobe_run')
const PROBE_MARKER = '<title>plugin-tab-persist probe</title>'
const PROBE_URL = `http://localhost:${PORT}/e2e/_plugintabprobe_run/index.html`
const WS = '/mock/proj-plugin-tabs'
const LS_TABS = 'go-code.sessionTabs'
// 期望的标签顺序：探针第一次 boot 时 store 里没有标签集，RightPanel 的 syncTabsToRightTab
// 会按 rightTab 补一个默认 activity（阶段 1 的既有语义），之后的 git / plugin:demo 排其后。
const ORDER = 'activity,git,plugin:demo'

// —— 插件资产（真的静态文件：loader 的 `import()` 必须真的能拉到它们）——
//
// 与 verify_plugin_loader 同构：真 index 形状 + 真静态目录，只把 `hai-surface://` 的字节来源
// 换成同源静态目录（本机没有 Electron 主进程，见 loader.ts 的 setPluginBaseUrlForTest）。
fs.mkdirSync(PROBE_DIR, { recursive: true })
const PLUGINS = path.join(PROBE_DIR, 'plugins')
fs.rmSync(PLUGINS, { recursive: true, force: true })
fs.mkdirSync(path.join(PLUGINS, 'demo'), { recursive: true })
fs.writeFileSync(path.join(PLUGINS, 'demo', 'panel.js'), `
globalThis.__pluginRuns = globalThis.__pluginRuns || {}
globalThis.__pluginRuns.demo = (globalThis.__pluginRuns.demo || 0) + 1
export const surface = {
  kind: 'plugin:demo',
  apply({ React, surfaces }) {
    surfaces.register({
      kind: 'plugin:demo',
      group: 'plugin',
      order: 10,
      title: () => 'Demo 看板',
      // 面板把**它拿到的 params** 原样印出来：T2/T3 的「参数往返」断言读的就是这一行 ——
      // 断言恢复出来的 store 标签里有 params 是不够的（那只证明数据在，不证明面板读得到）。
      render: ({ tab }) => React.createElement(
        'div',
        { 'data-plugin': 'demo', 'data-params': JSON.stringify(tab.params ?? null) },
        'params=' + JSON.stringify(tab.params ?? null),
      ),
    })
  },
}
`)

// —— 探针页 ——
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8">'
  + `<title>plugin-tab-persist probe</title></head>`
  + '<body><div id="root" style="height:100vh"></div>'
  + '<script type="module" src="./main.tsx"></script></body></html>')

// 探针页职责（§4.7：只替 IPC 边界，store/RightPanel/注册表/loader 全是真的）：
//   · `surfaceIndex` 的内容住在 localStorage（`__probe.pluginIndex`）—— 真实世界里 index 归
//     主进程所有、**跨重启还在**；用 localStorage 是它在「同一个页面反复 reload」下的等价物。
//   · `__probe.gate` 让 surfaceIndex 返回一个**永不 resolve 的 promise**：这是「标签已经从
//     localStorage 恢复出来、但 provider 还没注册」那一瞬的确定性复现（不靠 sleep 赌时序）。
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
// ⚠️ 全部用 await import 而不是顶部 static import：store 在模块求值时就
// const transport = getTransport() 把实现定死了，而生产里 window.desktop 由 preload **先于**
// bundle 注入（详见 verify_plugin_loader.mjs 的同一段注释）。
import '../../src/index.css'

const feeds: Array<(line: string) => void> = []
const surfaceIndexCalls: Array<unknown> = []
const readIndex = (): unknown => {
  try {
    const raw = localStorage.getItem('__probe.pluginIndex')
    return raw ? JSON.parse(raw) : { version: 1, items: [] }
  } catch { return { version: 1, items: [] } }
}
let pluginIndex: unknown = readIndex()
// gate：为 true 时 surfaceIndex **永不 resolve**（= provider 还没注册的那一瞬）
let gated = localStorage.getItem('__probe.gate') === '1'
const gatedWaiters: Array<(v: unknown) => void> = []

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
  surfaceIndex: async () => {
    surfaceIndexCalls.push(pluginIndex)
    if (!gated) return pluginIndex
    return await new Promise((res) => { gatedWaiters.push(res) })
  },
  surfaceCommand: async () => ({ ok: true }),
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
const { setPluginBaseUrlForTest } = await import('../../src/surfaces/plugin/loader')
setPluginBaseUrlForTest((id) => \`/e2e/_plugintabprobe_run/plugins/\${id}\`)
const { useAppStore } = await import('../../src/store/useAppStore')

useAppStore.setState({ workspace: ${JSON.stringify(WS)} })

const scopeOf = (s: { activeSessionId?: string; workspace?: string }) => s.activeSessionId || s.workspace || ''
;(window as unknown as Record<string, unknown>).__probe = {
  store: useAppStore,
  surfaces: () => listSurfaces().map((p) => p.kind),
  has: (k: string) => !!getSurface(k as never),
  surfaceIndexCalls: () => surfaceIndexCalls.length,
  setIndex: (items: unknown[]) => {
    pluginIndex = { version: 1, items }
    localStorage.setItem('__probe.pluginIndex', JSON.stringify(pluginIndex))
  },
  setGate: (on: boolean) => { localStorage.setItem('__probe.gate', on ? '1' : '0'); gated = on },
  // 放行被 gate 住的 surfaceIndex（= 「provider 注册完成」）—— 之后 loader 会自己接着 sync。
  release: () => {
    gated = false
    localStorage.setItem('__probe.gate', '0')
    const ws = gatedWaiters.splice(0)
    for (const w of ws) w(pluginIndex)
  },
  inject: (line: string) => feeds.forEach((f) => f(line)),
  // 真 store 动作（不是 setState 造标签）：openTab 会把 params 合进实例
  open: (spec: unknown) => {
    useAppStore.setState({ rightLayout: { open: true, bias: 'even', ratio: 50 }, rightExpanded: true })
    useAppStore.getState().openTab(spec as never)
  },
  expand: (on: boolean) => useAppStore.getState().setRightExpanded(on),
  // 按 kind 激活（真动作 activateTab）：T4 用它把用户视角切到插件标签上 —— 标签条上的墓碑
  // 与内容区的墓碑是两件事，后者只在「它正在显示」时存在。
  activate: (kind: string) => {
    const s = useAppStore.getState()
    const set = s.tabsByScope[scopeOf(s)]
    const tab = (set?.tabs ?? []).find((t) => t.kind === kind)
    if (tab) s.activateTab(tab.id)
    return !!tab
  },
  tabs: () => {
    const s = useAppStore.getState()
    const set = s.tabsByScope[scopeOf(s)]
    return {
      kinds: (set?.tabs ?? []).map((t) => t.kind),
      params: Object.fromEntries((set?.tabs ?? []).map((t) => [t.kind, (t as { params?: unknown }).params ?? null])),
      activeKind: (set?.tabs ?? []).find((t) => t.id === set?.activeId)?.kind ?? '',
      ids: (set?.tabs ?? []).map((t) => t.id),
    }
  },
  flush: () => useAppStore.getState().flushSessionTabs(),
  stored: () => localStorage.getItem(${JSON.stringify(LS_TABS)}),
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

const item = (id, hash) => ({
  id,
  version: '1.0.0',
  hash: hash.padEnd(64, '0').slice(0, 64),
  enabled: true,
  kind: `plugin:${id}`,
  commands: [],
  data: ['surfaces_changed'],
  entry: 'panel.js',
})

let code = 0
try {
  await waitProbePage(PROBE_URL, { marker: PROBE_MARKER, probe: 'e2e:plugin-tab-persist', child: server })
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
  page.setDefaultTimeout(8000)
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))

  const ready = async () => {
    await page.waitForFunction(() => !!window.__probe, null, { timeout: 30000 })
  }
  const tabs = () => page.evaluate(() => window.__probe.tabs())
  const stored = () => page.evaluate(() => window.__probe.stored())
  const flush = () => page.evaluate(() => window.__probe.flush())
  const open = (spec) => page.evaluate((s) => window.__probe.open(s), spec)
  const expand = (on = true) => page.evaluate((v) => window.__probe.expand(v), on)
  const activate = (kind) => page.evaluate((k) => window.__probe.activate(k), kind)
  const setIndex = (items) => page.evaluate((it) => window.__probe.setIndex(it), items)
  const setGate = (on) => page.evaluate((v) => window.__probe.setGate(v), on)
  const release = () => page.evaluate(() => window.__probe.release())
  const injectChanged = () => page.evaluate(() => window.__probe.inject(JSON.stringify({ type: 'ui', op: 'surfaces_changed', data: { at: Date.now() } })))
  const surfaces = () => page.evaluate(() => window.__probe.surfaces())

  // 干净起点：先在页内把两份 localStorage 复位（index 归「主进程」所有 → 探针用 LS 持有它），
  // 再 reload 一次，让 store 的**模块加载期**恢复逻辑读到干净的存储（那一步不可逆）。
  const boot = async ({ index, gate }) => {
    await page.evaluate(([idx, g]) => {
      localStorage.removeItem('go-code.sessionTabs')
      localStorage.setItem('__probe.pluginIndex', JSON.stringify({ version: 1, items: idx }))
      localStorage.setItem('__probe.gate', g ? '1' : '0')
    }, [index, gate])
    await page.reload({ waitUntil: 'domcontentloaded' })
    await ready()
  }
  // 只 reload、**不动**存储（T2/T3/T4 用它：此时 localStorage 里是上一次跑出来的标签集）
  const reload = async () => {
    await page.reload({ waitUntil: 'domcontentloaded' })
    await ready()
  }

  // —— T0 自检（F45：先证明探针走的是被测那条路）——
  console.log('\nT0 内置 provider 在（对照基线）')
  await page.goto(PROBE_URL, { waitUntil: 'domcontentloaded' })
  await ready()
  await boot({ index: [], gate: false })
  const kinds0 = await surfaces()
  for (const k of ['git', 'activity', 'file', 'web']) check(`listSurfaces() 含内置 ${k}`, kinds0.includes(k), JSON.stringify(kinds0))

  // —— T1 写入：真面板 + 落盘形态 ——
  console.log("\nT1 openTab('plugin:demo', {a:1}) → 面板渲染 + 落盘")
  await setIndex([item('demo', 'h1')])
  await injectChanged()
  await page.waitForFunction(() => window.__probe.has('plugin:demo'), null, { timeout: 15000 }).catch(() => {})
  check('index 同步后 plugin:demo 进注册表（否则下面的渲染断言是空转）', await page.evaluate(() => window.__probe.has('plugin:demo')))
  await open({ kind: 'git' }) // 对照组：内置标签必须不受影响
  await open({ kind: 'plugin:demo', params: { a: 1 } })
  await page.waitForSelector('[data-plugin="demo"]', { timeout: 8000 }).catch(() => {})
  const t1tab = await tabs()
  check('标签集里有 plugin:demo（openTab 走的是真动作）', t1tab.kinds.includes('plugin:demo'), JSON.stringify(t1tab.kinds))
  check('面板渲染出来了，且它读到的 params 是 {"a":1}',
    (await page.evaluate(() => document.querySelector('[data-plugin="demo"]')?.getAttribute('data-params') ?? '')) === '{"a":1}',
    JSON.stringify(await page.evaluate(() => document.querySelector('[data-plugin="demo"]')?.textContent ?? '')))
  check('标签条上 aria-selected 指向它（激活的是插件标签）',
    (await page.evaluate(() => document.querySelector('.rtab[data-kind="plugin:demo"]')?.getAttribute('aria-selected') ?? '')) === 'true')
  await flush()
  const raw1 = JSON.parse(await stored())
  const scope = await page.evaluate(() => {
    const s = window.__probe.store.getState()
    return s.activeSessionId || s.workspace || ''
  })
  const saved1 = (raw1.scopes[scope]?.tabs ?? []).find((t) => t.kind === 'plugin:demo')
  check('落盘形态带 kind + params（plugin 分支在 default(web) 之前）',
    saved1?.kind === 'plugin:demo' && JSON.stringify(saved1?.params) === '{"a":1}', JSON.stringify(saved1))
  check('存储版本号仍是 1（加一种标签不升版本：旧数据里没有 plugin 项，天然兼容）', raw1.v === 1, String(raw1.v))
  check('内置标签照常落盘（回归基线：内置四类的条目逐字不变，plugin 排在它们之后）',
    (raw1.scopes[scope]?.tabs ?? []).map((t) => t.kind).join(',') === ORDER,
    JSON.stringify((raw1.scopes[scope]?.tabs ?? []).map((t) => t.kind)))

  // —— T2 重启：**确定性**复现「标签已恢复 / provider 还没注册」那一瞬 ——
  console.log('\nT2 reload → 标签还在 → 墓碑 →（provider 注册后）面板照常渲染')
  await setGate(true) // 下一份文档里 surfaceIndex 永不 resolve
  await reload()
  const t2tab = await tabs()
  check('重启后标签还在（kind 仍是 plugin:demo）', t2tab.kinds.join(',') === ORDER, JSON.stringify(t2tab.kinds))
  check('重启后 params 往返（store 里仍是 {"a":1}）', JSON.stringify(t2tab.params['plugin:demo']) === '{"a":1}', JSON.stringify(t2tab.params))
  check('激活的仍是它（不是回落到 git/activity）', t2tab.activeKind === 'plugin:demo', JSON.stringify(t2tab.activeKind))
  await expand(true)
  const rowTomb = await page.evaluate(() => {
    const el = document.querySelector('.rtab[data-kind="plugin:demo"]')
    return el ? { un: el.getAttribute('data-unavailable'), reason: el.getAttribute('data-unavailable-reason'), sel: el.getAttribute('aria-selected') } : null
  })
  check('标签条上它是**墓碑**（data-unavailable=1 / reason=missing）', rowTomb?.un === '1' && rowTomb?.reason === 'missing', JSON.stringify(rowTomb))
  check('墓碑态下 aria-selected 仍然正确（标签身份不丢）', rowTomb?.sel === 'true', JSON.stringify(rowTomb))
  check('内容区渲染的是墓碑（不是空白/不是 provider）',
    (await page.evaluate(() => document.querySelector('.surface-tombstone[data-reason="missing"]')?.getAttribute('data-kind') ?? '')) === 'plugin:demo')
  check('provider 还没注册 → 此刻**没有**插件面板 DOM', (await page.evaluate(() => document.querySelectorAll('[data-plugin="demo"]').length)) === 0)
  await release() // = provider 注册完成（loader 自己接着 sync + 广播）
  await page.waitForSelector('[data-plugin="demo"]', { timeout: 15000 }).catch(() => {})
  check('provider 注册后**自动**变回面板（注册表的 subscribeSurfaces 推动了重渲染，不用手点）',
    (await page.evaluate(() => document.querySelectorAll('[data-plugin="demo"]').length)) === 1)
  check('墓碑消失了（同一处内容区，不是叠加）',
    (await page.evaluate(() => document.querySelectorAll('.surface-tombstone').length)) === 0)
  check('恢复后面板读到的 params 仍是 {"a":1}（参数往返走的是真 render）',
    (await page.evaluate(() => document.querySelector('[data-plugin="demo"]')?.getAttribute('data-params') ?? '')) === '{"a":1}')
  check('标签条上墓碑标记也清掉了（data-unavailable 不再出现）',
    (await page.evaluate(() => document.querySelectorAll('.rtab[data-unavailable="1"]').length)) === 0)

  // —— T3 超大 params（> 4 KiB）：整条不持久化 ——
  console.log('\nT3 params > 4 KiB → 那条标签不恢复（其余照常），不抛错')
  await open({ kind: 'plugin:big', params: { blob: 'x'.repeat(5000) } })
  const t3before = await tabs()
  check('前置：超大 params 的标签确实开出来了（否则下面的「不恢复」是空转）', t3before.kinds.includes('plugin:big'), JSON.stringify(t3before.kinds))
  await flush()
  const raw3 = await stored()
  check('超大 params 的标签**一条字段都没落盘**（宁可不恢复，也不存半截参数）', !raw3.includes('plugin:big'))
  await reload()
  const t3after = await tabs()
  check('重启后它没被恢复（其余标签正常：activity + git + plugin:demo 都在）',
    t3after.kinds.join(',') === ORDER, JSON.stringify(t3after.kinds))
  check('超大 params 之后仍然零 pageerror', errs.length === 0, errs.join('; '))

  // —— T4 卸载后重启：墓碑，绝不退化成「空白 web 标签」 ——
  console.log('\nT4 插件被卸载后重启 → 标签仍在但是墓碑（missing），不是空白 web 标签')
  await setIndex([]) // index 去掉该插件（= 卸载），然后 reload
  await reload()
  const t4tab = await tabs()
  check('标签仍在（恢复走的是「地址」，不依赖插件活着）', t4tab.kinds.join(',') === ORDER, JSON.stringify(t4tab.kinds))
  check('kind 仍是 plugin:demo，且没有多出一个 web 标签（当成 web 存/取的话这里会是「空白网页」）',
    t4tab.kinds.includes('plugin:demo') && !t4tab.kinds.includes('web'), JSON.stringify(t4tab.kinds))
  await expand(true)
  await activate('plugin:demo') // 内容区的墓碑只在「它正在显示」时存在
  await page.waitForTimeout(150)
  const t4row = await page.evaluate(() => {
    const el = document.querySelector('.rtab[data-kind="plugin:demo"]')
    return el ? { un: el.getAttribute('data-unavailable'), reason: el.getAttribute('data-unavailable-reason') } : null
  })
  check('标签条上它是墓碑（data-unavailable=1 / reason=missing）', t4row?.un === '1' && t4row?.reason === 'missing', JSON.stringify(t4row))
  check('点它是激活项（不是点不动的一行灰字）',
    (await page.evaluate(() => document.querySelector('.rtab[data-kind="plugin:demo"]')?.getAttribute('aria-selected') ?? '')) === 'true')
  check('内容区是墓碑（reason=missing），不是空白 web 面板',
    (await page.evaluate(() => document.querySelector('.surface-tombstone[data-reason="missing"]')?.getAttribute('data-kind') ?? '')) === 'plugin:demo')
  check('mismatch：此时注册表里确实没有它（墓碑是**正确**结论，不是巧合）',
    (await page.evaluate(() => window.__probe.has('plugin:demo'))) === false)

  console.log('\nT5 全程零页面错误')
  check('全程无页面错误（卸载/超大 params/墓碑都不许抛）', errs.length === 0, errs.join('; '))
  await browser.close()
} catch (e) {
  // 前置不满足 ≠ 断言失败：退出码 2，且必须把「为什么」打出来（00 §4.6 红归因）。
  if (!isPrecondition(e)) throw e
  console.error(`\n⛔ 前置不满足（退出码 2，不是测试失败）：${e.message}`)
  code = 2
} finally {
  cleanup()
}

if (code === 0) {
  console.log(`\n${fails.length === 0 ? 'ALL PASS' : `FAILED (${fails.length}): ${fails.join('; ')}`}`)
  code = fails.length === 0 ? 0 : 1
}
// 成功路径也要显式收尾：spawn 的 vite 子进程会让 Node 事件循环永不空 → 脚本「全绿却永不退出」
process.exit(code)
