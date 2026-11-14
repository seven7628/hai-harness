#!/usr/bin/env node
// verify_external_surface.mjs —— 把「一个外部插件目录」装进来、点开、看有没有渲染（R16b / Creator Mode 验收工具）。
//
// 它验的是**别人的插件**，不是我们的：使用者（另一个 agent / 用户）照 skills/hai-surface-dev
// 写出来的目录，交给这一条命令，回答「装进来之后界面上点得开吗」。
//
//   node e2e/verify_external_surface.mjs --dir=../../templates/surface
//   npm run e2e:external-surface                        （缺省就是 templates/surface）
//   SURFACE_DIR=/abs/plugin/dir npm run e2e:external-surface
//
// 这一条**不重写插件的 UI**，也不认任何插件内部约定（路由 / 组件名 / data-* 属性）：
//   1. 读 --dir 的 surface.json（真声明）→ 派生一条 index 项，hash = sha256(入口文件)，
//      与 Go 安装器同一条口径（同一份目录，两条工具算出的 hash 必须一样）；
//   2. 读 --dir 的入口文件（manifest.entry，缺省 panel.js）**原文** → 写进探针页,
//      由页面用 Blob 造一个 module URL 喂 setPluginBaseUrlForTest —— 加载的就是那份字节；
//   3. 断言：entries 全部出现在 ＋ 菜单、点第一个入口渲染出**非空内容**、pageerror=0；
//   4. 顺带验宿主的 ＋ 菜单键盘（R16b：↑↓ / Enter / Esc 回焦）与 `{zh,en}` 标题按语言选。
//      这两条与插件无关（是宿主 UI），但这条命令本来就是「装进来 + 点得开」的验收，
//      而「点得开」在键盘用户那里就是 ↑↓ + Enter。
//
// 为什么不落一份插件副本到磁盘（verify_plugin_entries 用的是那种）：那种做法需要把
// `<dir>` 拷进 dev server 的静态目录，等于「装的是副本」；这里用 Blob 直接喂字节，
// 目录只读。代价：**多文件插件**（panel.js 里再 import './x.js'）不在本工具的支持范围 ——
// 那需要一条真协议（生产是 hai-surface://）。单文件零依赖是 SKILL.md 的硬约定，
// 违反时这里会以「import 失败 / 注册表里没有这个 kind」红给你看，不会静默通过。
//
// 已知不覆盖：图标（Blob 源没有同源资产目录，PluginEntryIcon 会退回 kind 图标）、
// 重启后仍在（那一条归 index/磁盘，Go 侧 TestSurfaceInstallExternal 已经验过 index 落盘）。
//
// 端口写死 5232（5231 = verify_plugin_entries）。启动前自检端口空闲；被占则 exit 2。
// 退出码：全通过 0；断言失败 1；前置不满足（端口被占 / 探针没起来 / --dir 不可用）2。
import { spawn } from 'node:child_process'
import crypto from 'node:crypto'
import fs from 'node:fs'
import net from 'node:net'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'
import { exitCodeOf, isPrecondition, waitProbePage } from './_serve.mjs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = 5232
const PROBE_DIR = path.join(__dirname, '_externalsurfaceprobe_run')
const REL_PROBE = 'e2e/_externalsurfaceprobe_run'
const PROBE_MARKER = '<title>external surface probe</title>'
const PROBE_URL = `http://localhost:${PORT}/${REL_PROBE}/index.html`
const WS = '/mock/proj-external-surface'

// ---- 参数：--dir=<插件目录> 或 --dir <插件目录>（缺省 = 仓里的模板）----
const ARGV = process.argv.slice(2)
const dirArg = (() => {
  const eq = ARGV.find((a) => a.startsWith('--dir='))
  if (eq) return eq.slice('--dir='.length)
  const i = ARGV.indexOf('--dir')
  if (i >= 0) return ARGV[i + 1] ?? ''
  return ''
})().trim() || '../../templates/surface'
const PLUGIN_DIR = path.resolve(process.cwd(), dirArg)

const fails = []
function check(label, cond, detail = '') {
  if (cond) console.log(`  ✅ ${label}`)
  else {
    fails.push(label)
    console.log(`  ❌ ${label}${detail ? ' —— ' + detail : ''}`)
  }
}
// 前置错误统一走 exit 2（与 _serve.mjs 的契约同码），断言失败才是 1。
class Precondition extends Error {
  constructor(message) {
    super(message)
    this.name = 'Precondition'
    this.exitCode = 2
  }
}
const fail = (msg) => {
  throw new Precondition(msg)
}

// 端口自检：本机有别的会话在跑一堆 vite，撞上端口时要在**起服务之前**说清楚，
// 别让 waitProbePage 的超时被读成「探针坏了」。
async function assertPortFree(port) {
  await new Promise((resolve, reject) => {
    const s = net.createServer()
    s.once('error', (e) => reject(new Precondition(`端口 ${port} 已被占用（${e.code}）：先关掉占它的进程，或改脚本里的 PORT`)))
    s.once('listening', () => s.close(resolve))
    s.listen(port, '127.0.0.1')
  })
}

let code = 0
let server = null
const cleanup = () => {
  try { server?.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)

try {
  // ---- T0 前置：目录/声明/入口文件 ----
  console.log(`\nT0 前置：${PLUGIN_DIR}`)
  if (!fs.existsSync(PLUGIN_DIR) || !fs.statSync(PLUGIN_DIR).isDirectory()) {
    fail(`--dir 不是目录：${PLUGIN_DIR}`)
  }
  const manifestPath = path.join(PLUGIN_DIR, 'surface.json')
  if (!fs.existsSync(manifestPath)) fail(`缺 surface.json：${manifestPath}`)
  let manifest
  try {
    manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'))
  } catch (e) {
    fail(`surface.json 不是合法 JSON：${e.message}`)
  }
  const entryRel = String(manifest.entry || 'panel.js').trim() || 'panel.js'
  const entryPath = path.join(PLUGIN_DIR, entryRel)
  if (!fs.existsSync(entryPath)) fail(`manifest.entry 指向的文件不存在：${entryRel}`)
  const source = fs.readFileSync(entryPath, 'utf8')
  const hash = crypto.createHash('sha256').update(fs.readFileSync(entryPath)).digest('hex')
  check('surface.json 可读', true)
  check(`入口文件可读（${entryRel}，${source.length} 字节）`, source.length > 0)
  check('id / kind 形状像插件（id 合法、kind = plugin:<id>）',
    /^[a-z][a-z0-9-]{1,40}$/.test(String(manifest.id || ''))
      && String(manifest.kind || '') === `plugin:${manifest.id}`,
    `id=${manifest.id} kind=${manifest.kind}`)

  // 真声明 → index 项（**不补、不改**：entries 就是 manifest 里那几条）
  const entries = Array.isArray(manifest.entries) ? manifest.entries : []
  const item = {
    id: manifest.id,
    version: manifest.version || '0.0.0',
    hash,
    enabled: true,
    kind: manifest.kind,
    name: typeof manifest.name === 'string' ? manifest.name : (manifest.name?.zh || manifest.name?.en || ''),
    commands: manifest.commands || [],
    data: manifest.data || [],
    entry: entryRel,
    ...(manifest.icon ? { icon: manifest.icon } : {}),
    entries,
  }
  const expectIds = entries.length ? entries.map((e) => String(e.id)) : ['default']
  const titleText = (t, lang) => {
    if (typeof t === 'string') return t.trim()
    if (!t || typeof t !== 'object') return ''
    const zh = String(t.zh || '').trim()
    const en = String(t.en || '').trim()
    return lang === 'en' ? en || zh : zh || en
  }
  const expectTitles = entries.length
    ? entries.map((e) => titleText(e.title, 'zh'))
    : [item.name || item.id]
  console.log(`  插件 ${item.id} v${item.version} · hash ${hash.slice(0, 12)}… · ${expectIds.length} 个入口：${expectIds.join(', ')}`)

  // ---- 探针页（只替 IPC 边界与 module 字节来源；store/RightPanel/注册表/loader 都是真的）----
  await assertPortFree(PORT)
  fs.mkdirSync(PROBE_DIR, { recursive: true })
  fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
    '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8">'
    + `<title>external surface probe</title></head>`
    + '<body><div id="root" style="height:100vh"></div>'
    + '<script type="module" src="./main.tsx"></script></body></html>')

  fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import '../../src/index.css'

const feeds: Array<(line: string) => void> = []
const readIndex = (): { version: number; items: Array<Record<string, unknown>> } => {
  try {
    const raw = localStorage.getItem('__probe.pluginIndex')
    return raw ? JSON.parse(raw) : { version: 1, items: [] }
  } catch { return { version: 1, items: [] } }
}
let pluginIndex = readIndex()
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
  surfaceIndex: async () => pluginIndex,
  surfaceSetEnabled: async (id: string, enabled: boolean) => {
    pluginIndex = { ...pluginIndex, items: pluginIndex.items.map((i) => (i.id === id ? { ...i, enabled } : i)) }
    localStorage.setItem('__probe.pluginIndex', JSON.stringify(pluginIndex))
    return { ok: true, id, enabled }
  },
  surfaceRemove: async () => ({ ok: true, status: 'applied' }),
  surfaceAudit: async () => ({ ok: true, lines: [] }),
  // 插件声明的命令：真值由 --dir 的 manifest 决定，探针只给一份「有数据」的回执。
  surfaceCommand: async () => ({ ok: true, data: { workspaces: [{ path: '/mock', title: 'mock' }] } }),
  // P1b2：插件 Agent 通道（真 main 的三条；探针不发起 run，只保证形状对得上）
  surfaceAgentToken: async (id: string) => ({ ok: true, token: 'probe-token-' + id }),
  surfaceAgent: async () => ({ ok: true, data: {} }),
  onSurfaceAgentEvent: () => () => {},
}
;(window as unknown as { desktop: unknown }).desktop = new Proxy(stub, {
  get(t, k) { return k in t ? t[k as string] : () => Promise.resolve({ ok: true }) },
})

const { createRoot } = await import('react-dom/client')
const { setPluginBaseUrlForTest } = await import('../../src/surfaces/plugin/loader')

// ★ module 字节的来源：--dir 里那份入口文件的**原文**（探针在 Node 侧读好，写进 localStorage）。
//   base = 'blob:<uuid>#' —— loader 会拼 $\\{base}/$\\{entry}?v=<hash>，而 blob: 带查询串在
//   Chromium 里解析失败（Failed to fetch dynamically imported module）。fragment 不参与
//   blob 查找，所以 'blob:…uuid#/panel.js?v=h' 拿到的正是这个 blob，且 hash 变了 URL 也变
//   （loader 的缓存键语义照旧：换 hash = 换模块实例）。
const panelSource = localStorage.getItem('__probe.panelSource')
let blobUrl = ''
if (panelSource) {
  blobUrl = URL.createObjectURL(new Blob([panelSource], { type: 'text/javascript' }))
  setPluginBaseUrlForTest(() => blobUrl + '#')
}

const [{ default: RightPanel }, { default: SurfacesPage }] = await Promise.all([
  import('../../src/components/RightPanel'),
  import('../../src/components/SurfacesPage'),
])
await import('../../src/surfaces/builtin')
const { getSurface, listSurfaces } = await import('../../src/surfaces/registry')
const { useAppStore } = await import('../../src/store/useAppStore')
const { HostLangProvider } = await import('../../src/host/lang')

const WS = ${JSON.stringify(WS)}
useAppStore.setState({ workspace: WS })
useAppStore.setState({ rightLayout: { open: true, bias: 'even', ratio: 50 }, rightExpanded: true })

const w = window as unknown as Record<string, unknown>
const scope = () => {
  const s = useAppStore.getState()
  return s.activeSessionId || s.workspace || ''
}
w.__probe = {
  store: useAppStore,
  blobUrl: () => blobUrl,
  setIndex: (items: unknown[]) => {
    pluginIndex = { version: 1, items: items as Array<Record<string, unknown>> }
    localStorage.setItem('__probe.pluginIndex', JSON.stringify(pluginIndex))
  },
  inject: (line: string) => feeds.forEach((f) => f(line)),
  surfaces: { list: () => listSurfaces().map((p) => p.kind), has: (k: string) => !!getSurface(k as never) },
  tabs: () => {
    const s = useAppStore.getState()
    return (s.tabsByScope[scope()]?.tabs ?? []).map((t) => {
      const tab = t as { id: string; kind: string; params?: Record<string, unknown> }
      return {
        id: tab.id,
        kind: tab.kind,
        params: tab.params ?? null,
        entry: (tab.params ?? {}).entry,
        active: tab.id === s.tabsByScope[scope()]?.activeId,
      }
    })
  },
  closeAll: () => {
    const s = useAppStore.getState()
    for (const t of (s.tabsByScope[scope()]?.tabs ?? [])) s.closeTab((t as { id: string }).id)
  },
  setLang: (lang: string) => {
    const s = useAppStore.getState()
    useAppStore.setState({ settings: { ...s.settings, lang } })
  },
  // 「面板有没有画出东西」：只看 DOM 事实，不认插件的内部约定。
  panelFacts: () => {
    const body = document.querySelector('.right-body')
    if (!body) return { ok: false, why: 'no .right-body' }
    const sized = [...body.querySelectorAll('*')].filter((n) => {
      const r = n.getBoundingClientRect()
      return r.width > 0 && r.height > 0
    })
    const text = (body.innerText || '').trim()
    const pic = ['svg', 'canvas', 'img'].some((sel) =>
      [...body.querySelectorAll(sel)].some((n) => {
        const r = n.getBoundingClientRect()
        return r.width > 0 && r.height > 0
      }))
    return {
      ok: true,
      text,
      textLen: text.length,
      sized: sized.length,
      pic,
      crash: !!body.querySelector('.surface-crash'),
      tombstone: !!body.querySelector('.surface-tombstone'),
      crashText: body.querySelector('.surface-crash-msg')?.textContent || '',
      html: body.innerHTML.slice(0, 200),
    }
  },
}

// 生产里这层 provider 在 main.tsx 的根上（host/panels-desktop.tsx）：语言的唯一来源。
function Probe() {
  const lang = useAppStore((s) => s.settings?.lang)
  return <HostLangProvider lang={lang}><RightPanel /></HostLangProvider>
}

void useAppStore.getState().start(WS)
createRoot(document.getElementById('root')!).render(<Probe />)
`)

  fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [APP] } } })
`)

  server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT),
    '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
  server.stderr.on('data', (d) => { const s = String(d); if (!/ready in|Local:/.test(s)) process.stderr.write(s) })

  await waitProbePage(PROBE_URL, { marker: PROBE_MARKER, probe: 'e2e:external-surface', child: server })
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } })
  page.setDefaultTimeout(6000)
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))

  // 干净起点：先把「装好的索引」与「入口文件原文」写进 localStorage，再首次加载 ——
  // 这样 boot 顺序与生产一致（preload/主进程先给数据，渲染层再 import 插件模块），
  // reload 也自动走同一条路（blob URL 不能跨文档复用，必须每次 boot 重建）。
  const seed = async () => page.evaluate(([src, it]) => {
    localStorage.setItem('__probe.pluginSource', '1')
    localStorage.setItem('__probe.panelSource', src)
    localStorage.setItem('__probe.pluginIndex', JSON.stringify({ version: 1, items: [it] }))
  }, [source, item])
  const boot = async () => {
    await page.goto(PROBE_URL, { waitUntil: 'domcontentloaded' })
    await page.waitForFunction(() => !!window.__probe, null, { timeout: 30000 })
    await page.evaluate(() => window.__probe.store.setState({
      rightLayout: { open: true, bias: 'even', ratio: 50 }, rightExpanded: true,
    }))
    await page.waitForSelector('.rtabs-add')
  }

  await page.goto(PROBE_URL, { waitUntil: 'domcontentloaded' })
  await page.waitForFunction(() => !!window.__probe, null, { timeout: 30000 })
  await page.evaluate(() => { try { localStorage.clear() } catch { /* 忽略 */ } })
  await seed()
  await boot()

  const KIND = `plugin:${item.id}`
  const hasKind = (k) => page.evaluate((kind) => window.__probe.surfaces.has(kind), k)
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
  const pluginItems = () => page.locator('.rtabs-menu.rtabs-menu-right [data-plugin-entry]')
  const menuOrder = () => pluginItems().evaluateAll((ns) => ns.map((n) => ({
    key: n.getAttribute('data-plugin-entry'),
    entry: n.getAttribute('data-entry'),
    text: (n.textContent || '').trim(),
  })))
  const activeMenu = () => page.evaluate(() => {
    const el = document.activeElement
    if (!el || !el.closest?.('.rtabs-menu.rtabs-menu-right')) return null
    return { entry: el.getAttribute('data-entry'), kind: el.getAttribute('data-kind'), text: (el.textContent || '').trim() }
  })
  const waitRegistered = () => page.waitForFunction((k) => window.__probe.surfaces.has(k), KIND, { timeout: 10000 }).catch(() => {})

  // ---- T1：真文件 import + apply 成功（loader 真的把这份字节当模块求值了）----
  console.log('\nT1 加载真 panel.js：注册表里有这个 kind')
  await waitRegistered()
  check(`loader 注册了 ${KIND}（${entryRel} 被真的 import + apply）`, await hasKind(KIND),
    `注册表 = ${JSON.stringify(await page.evaluate(() => window.__probe.surfaces.list()))}`)
  check('页面拿到了 blob 源（就是 --dir 那份字节）',
    (await page.evaluate(() => window.__probe.blobUrl())).startsWith('blob:'),
    await page.evaluate(() => window.__probe.blobUrl()))

  // ---- T2：＋ 菜单里出现**全部** entries，标题取自 manifest ----
  console.log('\nT2 ＋ 菜单：entries 全部出现，标题与声明一致')
  await openMenu()
  const order = await menuOrder()
  check(`＋ 菜单里 ${expectIds.length} 个入口全在（${expectIds.join(', ')}）`,
    JSON.stringify(order.map((o) => o.entry)) === JSON.stringify(expectIds),
    JSON.stringify(order.map((o) => o.entry)))
  check('标题 = manifest 的 title（zh）',
    JSON.stringify(order.map((o) => o.text)) === JSON.stringify(expectTitles),
    JSON.stringify(order.map((o) => o.text)))
  check('data-plugin-entry = <id>/<entryId>（宿主按声明拼，不是插件自己注入的）',
    JSON.stringify(order.map((o) => o.key)) === JSON.stringify(expectIds.map((id) => `${item.id}/${id}`)),
    JSON.stringify(order.map((o) => o.key)))

  // ---- T3：R16b ＋ 菜单键盘（↑↓ 移焦点 / Enter 激活 / Esc 关闭并把焦点还给 ＋）----
  console.log('\nT3 ＋ 菜单键盘：打开即聚焦第一项 → ↑↓ 移焦点 → ↓ 到底回绕 → Enter 激活')
  // 菜单里内置 kind 在前、插件分组在后：第一项是内置项，插件入口要从上面走下去才到。
  const focusAt = () => page.evaluate(() => {
    const ns = [...document.querySelectorAll('.rtabs-menu.rtabs-menu-right .rtabs-mi')]
    return { at: ns.indexOf(document.activeElement), len: ns.length }
  })
  const first = await focusAt()
  check('打开菜单后焦点就在菜单里（第一项），不用先按 Tab', first.at === 0, JSON.stringify(first))
  const menuLen = first.len
  await page.keyboard.press('ArrowDown')
  const secondAt = await focusAt()
  check('↑↓：ArrowDown 焦点到第二项', secondAt.at === 1, JSON.stringify(secondAt))
  await page.keyboard.press('ArrowUp')
  await page.keyboard.press('ArrowUp')
  const wrapped = await focusAt()
  check('↑↓：首项再 ArrowUp 回绕到末项（可以一直按，不会卡在边界）',
    wrapped.at === wrapped.len - 1, JSON.stringify(wrapped))
  // 键盘走到第一个**插件**入口（内置项在前，插件分组在后）
  let steps = 0
  while (steps < menuLen + 1) {
    const cur = await activeMenu()
    if (cur && cur.entry) break
    await page.keyboard.press('ArrowDown')
    steps += 1
  }
  const atPlugin = await activeMenu()
  check('↑↓ 能走到插件入口（分组标题不是焦点陷阱）',
    !!atPlugin?.entry && expectIds.includes(atPlugin.entry), JSON.stringify(atPlugin))
  const kbEntry = atPlugin?.entry
  await page.keyboard.press('Enter')
  await page.waitForSelector(`.rtab[data-kind="${KIND}"][data-entry="${kbEntry}"]`, { timeout: 8000 }).catch(() => {})
  check('Enter 激活：标签真的开了（与鼠标点击同一条 onClick）',
    (await page.locator(`.rtab[data-kind="${KIND}"][data-entry="${kbEntry}"]`).count()) === 1)
  check('Enter 之后菜单关闭', (await page.locator('.rtabs-menu.rtabs-menu-right').count()) === 0)

  // ---- T4：Esc 关闭 + 焦点回到 ＋ ----
  console.log('\nT4 Esc：关闭菜单，焦点还给 ＋ 按钮')
  await openMenu()
  await page.keyboard.press('Escape')
  await page.waitForSelector('.rtabs-menu.rtabs-menu-right', { state: 'detached' }).catch(() => {})
  const focusBack = await page.evaluate(() => {
    const el = document.activeElement
    return { cls: el?.className || '', isAdd: !!el?.classList?.contains('rtabs-add') }
  })
  check('Esc 关闭菜单', (await page.locator('.rtabs-menu.rtabs-menu-right').count()) === 0)
  check('Esc 之后焦点回到 ＋ 按钮（不是掉到 body）', focusBack.isAdd, JSON.stringify(focusBack))

  // ---- T5：点第一个入口 → 渲染出非空内容（这一条是本工具的主断言）----
  console.log('\nT5 点入口：面板渲染出非空内容（不看插件的内部结构）')
  await page.evaluate(() => window.__probe.closeAll())
  await openMenu()
  await pluginItems().first().click()
  await page.waitForSelector(`.rtab[data-kind="${KIND}"][data-entry="${expectIds[0]}"]`, { timeout: 8000 }).catch(() => {})
  // 面板内容可能异步（插件用 host.command 取数），给一个短的稳定窗口再读。
  await page.waitForTimeout(400)
  const facts = await page.evaluate(() => window.__probe.panelFacts())
  console.log(`  面板文本：${JSON.stringify((facts.text || '').slice(0, 120))}`)
  check('面板里至少有一个有尺寸的元素', facts.ok && facts.sized > 0, JSON.stringify(facts).slice(0, 300))
  check('面板渲染出非空内容（文本非空，或画了 svg/canvas/img）',
    facts.textLen > 0 || facts.pic, `textLen=${facts.textLen} pic=${facts.pic} html=${(facts.html || '').slice(0, 160)}`)
  check('不是崩溃卡（SurfaceBoundary 没接住异常）', !facts.crash, facts.crashText || '')
  check('不是墓碑（provider 已注册且 tab 的 kind 对得上）', !facts.tombstone)
  const activeTab = (await page.evaluate(() => window.__probe.tabs())).find((t) => t.active)
  check('打开的是这个入口（params.entry = 声明里的 id）', activeTab?.entry === expectIds[0], JSON.stringify(activeTab))
  const tabLabel = await page.locator(`.rtab[data-kind="${KIND}"][data-entry="${expectIds[0]}"]`).getAttribute('title')
  check('标签标题 = 该入口的 manifest 标题（菜单/标签同源）', tabLabel === expectTitles[0], JSON.stringify(tabLabel))

  // ---- T6：第二个入口（若声明了）→ 同一 kind 的第二个标签并存 ----
  if (expectIds.length > 1) {
    console.log('\nT6 第二个入口：同一 kind 的第二个标签并存（一插件多入口）')
    await openMenu()
    await pluginItems().nth(1).click()
    await page.waitForSelector(`.rtab[data-kind="${KIND}"][data-entry="${expectIds[1]}"]`, { timeout: 8000 }).catch(() => {})
    const tabs = (await page.evaluate(() => window.__probe.tabs())).filter((t) => t.kind === KIND)
    check('两个入口各自一个标签', tabs.length === 2, JSON.stringify(tabs.map((t) => t.entry)))
    check('两个标签的 params.entry 各自正确',
      JSON.stringify(tabs.map((t) => t.entry).sort()) === JSON.stringify([...expectIds].slice(0, 2).sort()),
      JSON.stringify(tabs.map((t) => t.entry)))
  } else {
    console.log('\nT6（跳过）manifest 没有声明第二个入口')
  }

  // ---- T7：宿主侧 {zh,en} 标题按语言选（同一份真面板，只换声明）----
  console.log('\nT7 entry 标题 {zh,en}：按当前语言选；单字符串两种语言都一样')
  await page.evaluate(([it, ids]) => window.__probe.setIndex([{
    ...it,
    // 声明换成两条：一条对象写法、一条老的单字符串。面板还是 --dir 那份真文件。
    entries: [
      { id: ids[0], title: { zh: '中文标题', en: 'English title' } },
      { id: 'string-form', title: '固定标题' },
    ],
  }]), [item, expectIds])
  await page.evaluate(() => window.__probe.inject(JSON.stringify({ type: 'ui', op: 'surfaces_changed', data: { at: Date.now() } })))
  await page.waitForTimeout(400)
  await openMenu()
  let zhOrder = await menuOrder()
  check('zh：对象标题取 zh，字符串标题原样',
    JSON.stringify(zhOrder.map((o) => o.text)) === JSON.stringify(['中文标题', '固定标题']),
    JSON.stringify(zhOrder.map((o) => o.text)))
  await closeMenu()
  await page.evaluate(() => window.__probe.setLang('en'))
  await page.waitForTimeout(200)
  await openMenu()
  const enOrder = await menuOrder()
  check('en：对象标题取 en，字符串标题**不变**（老写法继续可用）',
    JSON.stringify(enOrder.map((o) => o.text)) === JSON.stringify(['English title', '固定标题']),
    JSON.stringify(enOrder.map((o) => o.text)))
  // 标签标题与菜单同源（declaredEntryTitle 也吃对象写法）：en 下点开必须是 en 那一句。
  // 没有这一条，「标签标题只在 zh 下对」这种半对状态不会被发现（模板的 provider.title
  // 恰好与 zh 标题同字，看不出差别）。
  await page.evaluate(() => window.__probe.closeAll())
  await openMenu()
  await pluginItems().first().click()
  await page.waitForTimeout(200)
  const enTabTitle = await page.locator(`.rtab[data-kind="${KIND}"][data-entry="${expectIds[0]}"]`).getAttribute('title')
  check('en：标签标题也走同一份声明（对象标题取 en）', enTabTitle === 'English title', JSON.stringify(enTabTitle))
  await page.evaluate(() => window.__probe.setLang('zh'))
  await page.evaluate(() => window.__probe.closeAll())

  // ---- T8：reload 之后入口还在、还点得开（index 与声明都来自「主进程」那一份）----
  console.log('\nT8 reload：入口还在，点开仍渲染（blob 源每次 boot 重建）')
  await page.evaluate(([it]) => window.__probe.setIndex([it]), [item])
  await page.evaluate(() => window.__probe.inject(JSON.stringify({ type: 'ui', op: 'surfaces_changed', data: { at: Date.now() } })))
  await page.waitForTimeout(300)
  await boot()
  await waitRegistered()
  check('reload 后 kind 仍注册（真文件被重新 import）', await hasKind(KIND))
  await openMenu()
  check('reload 后 ＋ 菜单里入口仍在', (await pluginItems().count()) === expectIds.length,
    String(await pluginItems().count()))
  await pluginItems().first().click()
  await page.waitForTimeout(400)
  const facts2 = await page.evaluate(() => window.__probe.panelFacts())
  check('reload 后点开仍渲染出非空内容', facts2.textLen > 0 || facts2.pic, JSON.stringify(facts2).slice(0, 200))

  // ---- T9：全程零 pageerror ----
  console.log('\nT9 全程零 pageerror')
  check('pageerror = 0', errs.length === 0, errs.join(' | '))

  await browser.close()
} catch (e) {
  if (e instanceof Precondition || isPrecondition(e)) {
    console.error(`\n前置不满足：${e.message}`)
    code = e instanceof Precondition ? e.exitCode : exitCodeOf(e)
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
  console.log('✅ 全部通过（verify_external_surface）')
}
cleanup()
process.exit(code)
