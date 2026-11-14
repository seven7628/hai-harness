#!/usr/bin/env node
// verify_host_boundary.mjs —— L0（D6 一次性交接）的边界门禁。
//
// 来源：docs/creator-mode-v0.1/09-agent-bridge.md §6.1、L0 定稿（用户 2026-11-14）。
// 这条门禁只钉一件事：**插件世界里不存在 window.desktop**，宿主能力只经一次交接进宿主内部。
// 它不测 P1 的东西（agent.run / quote / 虚拟 Project / not_declared 错误码名）。
//
// 三个阶段，全部在 5262（跑前 `lsof -i :5262` 确认没人占；脚本自己再 bind 一次自检）：
//
//   A 静态边界（不需要浏览器，纯读字节）
//     A1 electron/preload.ts 不再 `exposeInMainWorld('desktop'`；只暴露 `__haiClaimHostApi`，
//        且第二次调用返回 null（`if (claimed) return null`）。
//     A2 src/** 里 `window.desktop` 只允许出现在 src/lib/bridge.ts（探针回退）与 src/i18n.ts（文案）。
//        —— 任何一处「宿主直接读 window.desktop」都会在这里红。
//     A3 生产产物 dist/assets/*.js **不含** `__haiTestBridge`（生产不许有探针口），但**含** claim。
//        —— 这条同时钉住「回退分支没被 dead-code 掉」和「探针口没被挂到生产」。
//     A4 dist 是新的（比 src/electron 的任何源文件都新）；过期就 exit 2 让人先 `npx vite build`。
//
//   B 生产产物（真 dist，普通 Chromium 里跑）
//     注入 fake claim → 启动应用：
//     B1 window.desktop === undefined（不是隐藏，是不存在）
//     B2 __haiClaimHostApi() 已经交接出去 → 第二次调用返回 null
//     B3 window.__haiTestBridge === undefined（生产没有探针口）
//     B4 另一个上下文：claim 返回 null 且 window.desktop 被注入 fake →
//        生产**完全不碰**它（还在窗口上），也没有 __haiTestBridge。
//        这条是「回退分支跑到生产也生效」那条变异的判据。
//     B5 全程 pageerror = 0
//
//   C 开发档（vite dev server，真 loader 装一个探针插件）
//     注入 fake claim → 真 loader.syncPlugins → 探针插件在 apply 里自己断言：
//     C1 插件里 typeof window.desktop === 'undefined'
//     C2 host.command 是函数；事件入口是函数（**L0 保持 host.events.on**；§6.3 的
//        host.onEvent 是 P1 改名，本轮不改 loader 的 host API 形态 —— 这里只如实打印）
//     C3 宿主没有把承载这两个函数的对象交给插件：host 上没有 api 对象本身；
//        页面上也没有任何属性指向 api（除 dev-only 的 __haiTestBridge，见下）
//     C4 未声明命令被 **loader** 拒：返回对象、ok:false（现在的 reason 是 denied；
//        §6.5 的 not_declared 是 P1 的错误码改名）→ 两种都接受，但必须 ok:false 且不抛
//     C5 已声明命令真的走到了 IPC 边界（claim 的 surfaceCommand 收到调用）
//     C6 dev 档里 __haiTestBridge === 交接出去的那个 api（探针读取口，生产没有）
//     C7 全程 pageerror = 0
//
// ⚠️ 已知的 dev-only 口子（设计如此，§6.1 的探针兼容）：dev 构建把交接到的 api 挂到
//    window.__haiTestBridge 供 e2e 读取；生产构建没有这一行（A3 钉住）。插件世界里
//    仍然没有 window.desktop —— 这一点在生产档由 B1 + 真 Electron 冒烟共同钉住。
//
// 用法：cd desktop/app && npx vite build && node e2e/verify_host_boundary.mjs
// 退出码：全通过 0；任一断言失败 1；前置不满足（dist 过期 / 端口被占 / vite 没起来）2。
import { spawn } from 'node:child_process'
import fs from 'node:fs'
import http from 'node:http'
import net from 'node:net'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = 5262
const PROBE_DIR = path.join(__dirname, '_hostboundary_run')
const DIST = path.join(APP, 'dist')

const fails = []
const notes = []
function check(label, cond, detail = '') {
  if (cond) console.log(`  ✅ ${label}`)
  else { fails.push(label); console.log(`  ❌ ${label}${detail ? ' —— ' + detail : ''}`) }
}
function precondition(msg) {
  console.error(`\n❌ 前置不满足：${msg}`)
  cleanupAll()
  process.exit(2)
}

let staticServer = null
let vite = null
let browser = null
function cleanupAll() {
  try { staticServer?.close() } catch { /* 已关闭 */ }
  try { staticServer?.closeAllConnections?.() } catch { /* 老 node */ }
  try { vite?.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}

// —— 端口自检：5262 必须真的能 bind（被并行探针占住时后面的失败会伪装成「功能坏了」）——
async function bindSelfCheck() {
  await new Promise((resolve, reject) => {
    const s = net.createServer()
    s.once('error', (e) => reject(e))
    s.listen(PORT, '127.0.0.1', () => s.close(() => resolve()))
  }).catch((e) => precondition(`端口 ${PORT} 不可 bind（${e.code || e}）：先 lsof -i :${PORT}`))
}

// ===================== A 静态边界 =====================
function newestMtime(dir) {
  let newest = 0
  for (const ent of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, ent.name)
    if (ent.isDirectory()) newest = Math.max(newest, newestMtime(p))
    else { try { newest = Math.max(newest, fs.statSync(p).mtimeMs) } catch { /* 忽略 */ } }
  }
  return newest
}

function phaseA() {
  console.log('\n=== A 静态边界 ===')
  const preload = fs.readFileSync(path.join(APP, 'electron/preload.ts'), 'utf8')
  check("A1a preload 不再 exposeInMainWorld('desktop'", !/exposeInMainWorld\(\s*'desktop'/.test(preload))
  check('A1b preload 只暴露 __haiClaimHostApi', /exposeInMainWorld\(\s*'__haiClaimHostApi'/.test(preload))
  check('A1c 交接是一次性的（第二次返回 null）', /if \(claimed\) return null/.test(preload))

  const offenders = []
  const walk = (dir) => {
    for (const ent of fs.readdirSync(dir, { withFileTypes: true })) {
      const p = path.join(dir, ent.name)
      if (ent.isDirectory()) { walk(p); continue }
      if (!/\.(ts|tsx)$/.test(ent.name)) continue
      const rel = path.relative(APP, p).replaceAll(path.sep, '/')
      if (rel === 'src/lib/bridge.ts' || rel === 'src/i18n.ts') continue // 探针回退口 / 文案
      const text = fs.readFileSync(p, 'utf8')
      if (text.includes('window.desktop')) offenders.push(rel)
    }
  }
  walk(path.join(APP, 'src'))
  check('A2 src 里只有 bridge.ts / i18n.ts 还提 window.desktop', offenders.length === 0, offenders.join(', '))

  const indexHtml = path.join(DIST, 'index.html')
  if (!fs.existsSync(indexHtml)) precondition('dist/index.html 不存在 —— 先跑 npx vite build')
  const assetsDir = path.join(DIST, 'assets')
  const js = fs.existsSync(assetsDir)
    ? fs.readdirSync(assetsDir).filter((f) => f.endsWith('.js')).map((f) => fs.readFileSync(path.join(assetsDir, f), 'utf8')).join('\n')
    : ''
  check('A3a 生产产物不含 __haiTestBridge', !js.includes('__haiTestBridge'))
  check('A3b 生产产物含 __haiClaimHostApi（交接真的进包了）', js.includes('__haiClaimHostApi'))

  const distMtime = fs.statSync(indexHtml).mtimeMs
  const srcMtime = Math.max(newestMtime(path.join(APP, 'src')), newestMtime(path.join(APP, 'electron')))
  if (distMtime < srcMtime) precondition('dist 比 src/electron 旧（改过源码没重建）—— 先跑 npx vite build')
  check('A4 dist 是新的（不旧于 src/electron）', distMtime >= srcMtime)
}

// ===================== 共用的 fake claim =====================
// 只替 preload 那一次交接：面子上给足（Proxy 兜底 = 任何未知 IPC 返回 {ok:true}），
// 好让真应用照样 boot；凡是断言用到的（surfaceIndex / surfaceCommand / startupMark …）
// 都显式记账。
function fakeClaimScript() {
  return `(() => {
    const calls = []
    window.__fakeCalls = calls
    let claimed = 0
    const impl = {
      startupMark: () => Promise.resolve(true),
      startBridge: () => Promise.resolve({ ok: true }),
      command: async (cmd) => {
        calls.push({ t: 'command', cmd })
        const type = cmd && cmd.type
        const data = type === 'workspace_list' ? { workspaces: [] }
          : type === 'session_list' ? { sessions: [] }
          : type === 'surface_index' ? { version: 1, items: [] }
          : {}
        return { ok: true, data }
      },
      onEvent: () => () => {},
      onExit: () => () => {},
      settingsGet: async () => ({}),
      settingsSet: async () => ({ ok: true }),
      surfaceIndex: async () => ({ version: 1, items: [] }),
      surfaceCommand: async (p) => { calls.push({ t: 'surfaceCommand', p }); return { ok: true, data: {} } },
      // 真 preload 里返回**数组**的几条：Proxy 兜底回的是 {ok:true}，真应用 boot 时会
      // for (const x of await api.xxx()) ⇒ 不是数组就 TypeError（那是 fake 不全，不是 L0 的错）。
      hubTakeDeepLinks: async () => [],
    }
    const api = new Proxy(impl, {
      get(t, k) {
        if (k in t) return t[k]
        return () => Promise.resolve({ ok: true })
      },
    })
    window.__fakeApi = api
    window.__haiClaimHostApi = () => { claimed += 1; return claimed === 1 ? api : null }
    window.__fakeClaimCalls = () => claimed
  })()`
}

// ===================== B 生产产物 =====================
function serveDist() {
  const MIME = {
    '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8',
    '.css': 'text/css; charset=utf-8', '.json': 'application/json; charset=utf-8',
    '.svg': 'image/svg+xml', '.png': 'image/png', '.jpg': 'image/jpeg', '.webp': 'image/webp',
    '.woff': 'font/woff', '.woff2': 'font/woff2', '.ttf': 'font/ttf', '.wasm': 'application/wasm',
    '.md': 'text/markdown; charset=utf-8', '.txt': 'text/plain; charset=utf-8',
  }
  return new Promise((resolve, reject) => {
    const srv = http.createServer((req, res) => {
      const url = decodeURIComponent((req.url || '/').split('?')[0])
      let file = path.join(DIST, url === '/' ? 'index.html' : url)
      if (!file.startsWith(DIST)) { res.writeHead(403); res.end('nope'); return }
      if (!fs.existsSync(file) || fs.statSync(file).isDirectory()) file = path.join(DIST, 'index.html') // SPA 回退
      try {
        res.writeHead(200, { 'content-type': MIME[path.extname(file)] || 'application/octet-stream' })
        fs.createReadStream(file).pipe(res)
      } catch (e) { res.writeHead(500); res.end(String(e)) }
    })
    srv.once('error', reject)
    srv.listen(PORT, '127.0.0.1', () => { staticServer = srv; resolve() })
  })
}

async function phaseB() {
  console.log('\n=== B 生产产物（dist + 普通 Chromium）===')
  await serveDist()
  const ctx = await browser.newContext()
  const errors = []
  const page = await ctx.newPage()
  page.on('pageerror', (e) => errors.push(String(e)))
  await page.addInitScript(fakeClaimScript())
  await page.goto(`http://127.0.0.1:${PORT}/index.html`, { waitUntil: 'load', timeout: 30000 })
  // 等应用真的开机（React 树挂上 + 交接发生）
  await page.waitForFunction(() => (window.__fakeClaimCalls?.() ?? 0) >= 1, null, { timeout: 30000 })
  const b1 = await page.evaluate(() => ({
    desktopType: typeof window.desktop,
    hasDesktopKey: Object.prototype.hasOwnProperty.call(window, 'desktop'),
    secondClaim: window.__haiClaimHostApi(),
    testBridge: typeof window.__haiTestBridge,
  }))
  check('B1 window.desktop === undefined', b1.desktopType === 'undefined' && b1.hasDesktopKey === false, JSON.stringify(b1))
  check('B2 claim 已交接出去 → 第二次调用返回 null', b1.secondClaim === null, String(b1.secondClaim))
  check('B3 生产没有 __haiTestBridge', b1.testBridge === 'undefined', b1.testBridge)

  // B4：claim 返回 null + window.desktop 被注入 fake → 生产不许碰它（也没有探针口）
  const ctx2 = await browser.newContext()
  const errors2 = []
  const page2 = await ctx2.newPage()
  page2.on('pageerror', (e) => errors2.push(String(e)))
  await page2.addInitScript(`(() => {
    window.__haiClaimHostApi = () => null
    window.desktop = new Proxy({ __fake: true }, { get(t, k) { if (k in t) return t[k]; return () => Promise.resolve({ ok: true }) } })
  })()`)
  await page2.goto(`http://127.0.0.1:${PORT}/index.html`, { waitUntil: 'load', timeout: 30000 })
  await page2.waitForTimeout(1500)
  const b4 = await page2.evaluate(() => ({
    desktopType: typeof window.desktop,
    stillThere: !!window.desktop?.__fake,
    testBridge: typeof window.__haiTestBridge,
  }))
  check('B4 生产不消费注入的 window.desktop（回退分支不在产物里）', b4.desktopType === 'object' && b4.stillThere === true, JSON.stringify(b4))
  check('B4b 生产档没有 __haiTestBridge（同上）', b4.testBridge === 'undefined', b4.testBridge)

  check('B5 pageerror = 0（应用档）', errors.length === 0, errors.slice(0, 3).join(' | '))
  check('B5b pageerror = 0（注入 fake desktop 档）', errors2.length === 0, errors2.slice(0, 3).join(' | '))
  await ctx.close()
  await ctx2.close()
  staticServer.close()
  staticServer = null
}

// ===================== C 开发档：真 loader 装探针插件 =====================
function writeProbe() {
  fs.rmSync(PROBE_DIR, { recursive: true, force: true })
  fs.mkdirSync(path.join(PROBE_DIR, 'plugins', 'hb'), { recursive: true })
  fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
    '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8">'
    + '<title>host boundary probe</title></head>'
    + '<body><div id="root" style="height:100vh"></div>'
    + '<script type="module" src="./main.tsx"></script></body></html>')

  // 探针插件：**在插件自己的代码里**断言（不是宿主替它断言）。
  fs.writeFileSync(path.join(PROBE_DIR, 'plugins/hb/panel.js'), `
globalThis.__pluginProbe = globalThis.__pluginProbe || {}
globalThis.__pluginProbe.runs = (globalThis.__pluginProbe.runs || 0) + 1
export const surface = {
  kind: 'plugin:hb',
  apply({ host, React, surfaces }) {
    const P = globalThis.__pluginProbe
    P.desktopType = typeof window.desktop
    P.hasDesktopKey = Object.prototype.hasOwnProperty.call(window, 'desktop')
    P.commandType = typeof host.command
    P.onEventType = typeof host.onEvent
    P.eventsOnType = typeof (host.events && host.events.on)
    P.hostKeys = Object.keys(host).sort()
    // 宿主有没有「顺手」把承载这两个函数的对象塞进来？（L0 的核心契约）
    let leaked = null
    for (const [k, v] of Object.entries(host)) {
      if (v && typeof v === 'object' && typeof v.command === 'function') leaked = k
    }
    P.leakedHostKey = leaked
    // C4 未声明命令：loader 侧就拒（P1 改名 not_declared；L0 是 denied）
    P.undeclared = null
    host.command('definitely_not_declared', {}).then((r) => { P.undeclared = r }).catch((e) => { P.undeclared = { threw: String(e) } })
    // C5 已声明命令：真的经 claim 的 surfaceCommand 出去
    P.declared = null
    host.command('ping', { n: 1 }).then((r) => { P.declared = r }).catch((e) => { P.declared = { threw: String(e) } })
    surfaces.register({
      kind: 'plugin:hb', group: 'plugin', order: 50,
      title: () => 'Host Boundary', render: () => React.createElement('div', { 'data-plugin': 'hb' }, 'host boundary probe'),
    })
  },
}
`)

  fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
// 探针页（dev 构建）：**只**注入 preload 那一次交接（fake claim），其余全是真的
// —— 真 loader、真注册表、真插件 import()。
import '../../src/index.css'

${fakeClaimScript()}

const { setPluginBaseUrlForTest, syncPlugins } = await import('../../src/surfaces/plugin/loader')
setPluginBaseUrlForTest((id: string) => \`/e2e/_hostboundary_run/plugins/\${id}\`)

const item = {
  id: 'hb', version: '1.0.0', hash: 'h'.padEnd(64, '0'), enabled: true, kind: 'plugin:hb',
  commands: ['ping'], data: [], entry: 'panel.js',
}
const report = await syncPlugins({ version: 1, items: [item] } as never, undefined as never, {}, { authoritativeIndex: true })

;(window as unknown as Record<string, unknown>).__hb = {
  report,
  plugin: () => (globalThis as unknown as { __pluginProbe?: unknown }).__pluginProbe ?? null,
  desktopType: () => typeof (window as unknown as { desktop?: unknown }).desktop,
  testBridgeIsApi: () => (window as unknown as { __haiTestBridge?: unknown }).__haiTestBridge === (window as unknown as { __fakeApi?: unknown }).__fakeApi,
  testBridgeType: () => typeof (window as unknown as { __haiTestBridge?: unknown }).__haiTestBridge,
  secondClaim: () => (window as unknown as { __haiClaimHostApi: () => unknown }).__haiClaimHostApi(),
  calls: () => (window as unknown as { __fakeCalls: unknown[] }).__fakeCalls,
  // 宿主（本页）有没有把 api 挂回 window：这里逐个查「除 __haiTestBridge 外」的属性
  windowApiRefs: () => Object.keys(window).filter((k) => {
    const v = (window as unknown as Record<string, unknown>)[k]
    return v && typeof v === 'object' && typeof (v as { surfaceCommand?: unknown }).surfaceCommand === 'function'
  }).sort(),
}
`)
}

async function phaseC() {
  console.log('\n=== C 开发档（真 loader + 探针插件）===')
  writeProbe()
  await new Promise((resolve, reject) => {
    vite = spawn('node', [path.join(APP, 'node_modules/vite/bin/vite.js'),
      '--port', String(PORT), '--strictPort', '--host', '127.0.0.1'],
      { cwd: APP, stdio: 'pipe', env: { ...process.env, VITE_PORT: String(PORT) } })
    const timer = setTimeout(() => reject(new Error('vite 未就绪（30s）')), 30000)
    vite.stdout.on('data', (d) => { if (/ready in|Local:/.test(String(d))) { clearTimeout(timer); resolve() } })
    vite.stderr.on('data', (d) => { const s = String(d); if (!/ready in|Local:/.test(s)) process.stderr.write(s) })
    vite.once('exit', (code) => { clearTimeout(timer); reject(new Error(`vite 退出（code=${code}）`)) })
  }).catch((e) => precondition(String(e.message || e)))

  const ctx = await browser.newContext()
  const errors = []
  const page = await ctx.newPage()
  page.on('pageerror', (e) => errors.push(String(e)))
  page.on('console', (m) => { if (m.type() === 'error') errors.push('console: ' + m.text()) })
  await page.goto(`http://127.0.0.1:${PORT}/e2e/_hostboundary_run/index.html`, { waitUntil: 'load', timeout: 30000 })
  await page.waitForFunction(() => {
    const hb = window.__hb
    return !!hb && !!(hb.plugin()?.undeclared) && !!(hb.plugin()?.declared)
  }, null, { timeout: 30000 })

  const c = await page.evaluate(() => {
    const hb = window.__hb
    return {
      scriptDesktop: hb.desktopType(),
      plugin: hb.plugin(),
      report: hb.report,
      secondClaim: hb.secondClaim(),
      testBridgeIsApi: hb.testBridgeIsApi(),
      testBridgeType: hb.testBridgeType(),
      windowApiRefs: hb.windowApiRefs(),
      surfaceCommandCalls: hb.calls().filter((x) => x.t === 'surfaceCommand').length,
    }
  })
  const p = c.plugin
  check('C0 插件真的被 import() 并 apply 了', p && p.runs >= 1 && Array.isArray(c.report?.loaded) === true,
    JSON.stringify({ runs: p?.runs, report: c.report }))
  check('C1 插件里 window.desktop === undefined', p.desktopType === 'undefined' && p.hasDesktopKey === false, JSON.stringify(p))
  check('C2 host.command 是函数', p.commandType === 'function', p.commandType)
  check('C2b 事件入口是函数（L0 = host.events.on）', p.eventsOnType === 'function', p.eventsOnType)
  notes.push(`host.onEvent = ${p.onEventType}（§6.3 的改名是 P1；L0 保持 host.events.on）`)
  notes.push(`插件看到的 host keys = ${JSON.stringify(p.hostKeys)}`)
  check('C3 host 上没有承载 command 的对象（插件拿不到 api 本身）', p.leakedHostKey === null, String(p.leakedHostKey))
  // 页面上只允许两个「像 api」的对象：探针自己注入的 fake（用来比对身份）与 dev-only 的读取口。
  check('C3b 页面上没有别的 api 引用（只有探针 fake + dev-only __haiTestBridge）',
    JSON.stringify(c.windowApiRefs) === JSON.stringify(['__fakeApi', '__haiTestBridge'].sort()), JSON.stringify(c.windowApiRefs))
  check('C4 未声明命令被 loader 拒（对象 + ok:false，不抛）',
    p.undeclared && p.undeclared.ok === false && !p.undeclared.threw
      && ['denied', 'not_declared'].includes(p.undeclared.reason),
    JSON.stringify(p.undeclared))
  if (p.undeclared?.reason === 'denied') notes.push('未声明命令的 reason 目前是 denied（§6.5 的 not_declared 是 P1）')
  check('C4b 已声明命令走通了（reply ok:true）', p.declared?.ok === true, JSON.stringify(p.declared))
  check('C5 claim 的 surfaceCommand 真的被调用一次', c.surfaceCommandCalls === 1, String(c.surfaceCommandCalls))
  check('C6 dev 档 __haiTestBridge === 交接出去的 api', c.testBridgeIsApi === true, c.testBridgeType)
  check('C6b 第二次 claim 返回 null', c.secondClaim === null, String(c.secondClaim))
  check('C7 pageerror = 0', errors.length === 0, errors.slice(0, 3).join(' | '))
  await ctx.close()
}

// ===================== 主流程 =====================
await bindSelfCheck()
phaseA()
browser = await chromium.launch()
try {
  await phaseB()
  await phaseC()
} finally {
  try { await browser?.close() } catch { /* 忽略 */ }
  cleanupAll()
}

console.log('')
for (const n of notes) console.log(`  ℹ️  ${n}`)
if (fails.length) {
  console.log(`\n❌ host boundary：${fails.length} 条断言失败`)
  for (const f of fails) console.log(`   · ${f}`)
  process.exit(1)
}
console.log('\n✅ host boundary：全部通过（插件世界里没有 window.desktop；能力只经一次交接）')
process.exit(0)
