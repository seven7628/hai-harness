#!/usr/bin/env node
// verify_external_surface_electron.mjs —— 外部插件在**真 Electron**里装得上、点得开（R16b 冒烟）。
//
// 前两条工具（go test -run TestSurfaceInstallExternal / verify_external_surface.mjs）验的是
// 「安装器」与「loader + ＋菜单」，各自把另一半替换成了替身。这一条**两边都不用替身**：
//   1. 用真安装器把 --dir 装进一个临时 HOME（HAI_DATA_DIR 的布局：<HOME>/.go-code/surfaces）；
//   2. 以 HOME=HAI_DATA_DIR=<同一个目录> 启动**真的**应用（真桥进程、真 index IPC、
//      真 hai-surface:// 协议、真 RightPanel）；
//   3. 断言：＋ 菜单里出现 manifest 声明的入口（标题同源）、点第一个入口渲染出非空内容。
//
// 用法：cd desktop/app && node e2e/verify_external_surface_electron.mjs --dir=../../templates/surface
// 退出码：0 通过 / 1 断言失败 / 2 前置不满足（缺 dist / --dir 不可用 / 装不上）。
//
// 多实例：HAI_DATA_DIR 会让 Electron 跳过单实例锁（本机可能正跑着用户自己的实例，
// 不加会被 requestSingleInstanceLock 直接踢掉，表现为 launch 后立刻退出）。
import { _electron } from 'playwright'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const REPO = path.resolve(APP, '../..')
const ELECTRON = path.join(APP, 'node_modules', '.bin', 'electron')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const ARGV = process.argv.slice(2)
const dirArg = (() => {
  const eq = ARGV.find((a) => a.startsWith('--dir='))
  if (eq) return eq.slice('--dir='.length)
  const i = ARGV.indexOf('--dir')
  if (i >= 0) return ARGV[i + 1] ?? ''
  return ''
})().trim() || '../../templates/surface'
const PLUGIN_DIR = path.resolve(process.cwd(), dirArg)

class Precondition extends Error {
  constructor(msg) { super(msg); this.name = 'Precondition'; this.exitCode = 2 }
}
const fail = (msg) => { throw new Precondition(msg) }
const fails = []
function check(label, cond, detail = '') {
  if (cond) console.log(`  ✅ ${label}`)
  else { fails.push(label); console.log(`  ❌ ${label}${detail ? ' —— ' + detail : ''}`) }
}
async function waitFor(fn, { timeout = 30000, label = '条件' } = {}) {
  const end = Date.now() + timeout
  while (Date.now() < end) {
    try { const v = await fn(); if (v) return v } catch { /* 继续等 */ }
    await sleep(250)
  }
  throw new Error(`等待超时：${label}`)
}

let code = 0
let app = null
let HOME = ''
process.on('exit', () => { try { app?.close() } catch { /* 已退出 */ } })

try {
  console.log(`\nT0 前置：${PLUGIN_DIR}`)
  if (!fs.existsSync(PLUGIN_DIR)) fail(`--dir 不存在：${PLUGIN_DIR}`)
  if (!fs.existsSync(path.join(PLUGIN_DIR, 'surface.json'))) fail(`缺 surface.json：${PLUGIN_DIR}`)
  const manifest = JSON.parse(fs.readFileSync(path.join(PLUGIN_DIR, 'surface.json'), 'utf8'))
  const entries = Array.isArray(manifest.entries) ? manifest.entries : []
  const titleText = (t, lang) => {
    if (typeof t === 'string') return t.trim()
    if (!t || typeof t !== 'object') return ''
    const zh = String(t.zh || '').trim()
    const en = String(t.en || '').trim()
    return lang === 'en' ? en || zh : zh || en
  }
  const expectTitles = entries.length ? entries.map((e) => titleText(e.title, 'zh')) : [manifest.name?.zh || manifest.id]

  // 应用要真跑起来：缺 dist / dist-electron / 桥二进制就地构建（与 e2e/ego_browser_panel.mjs 同一条）。
  const index = path.join(APP, 'dist', 'index.html')
  const mainJs = path.join(APP, 'dist-electron', 'main.js')
  const bridge = path.join(APP, 'dist', 'hai-bridge')
  if (!fs.existsSync(mainJs)) {
    console.log('  构建 dist-electron（npm run build:electron）…')
    const b = spawnSync('npm', ['run', 'build:electron'], { cwd: APP, encoding: 'utf8' })
    if (b.status !== 0) fail(`build:electron 失败：\n${b.stdout}\n${b.stderr}`)
  }
  if (!fs.existsSync(index)) {
    console.log('  构建渲染层（npx vite build）…')
    const b = spawnSync('npx', ['vite', 'build'], { cwd: APP, encoding: 'utf8' })
    if (b.status !== 0) fail(`vite build 失败：\n${b.stdout}\n${b.stderr}`)
  }
  if (!fs.existsSync(bridge)) {
    console.log('  构建桥（go build ./desktop/bridge）…')
    const b = spawnSync('go', ['build', '-o', 'desktop/app/dist/hai-bridge', './desktop/bridge'], { cwd: REPO, encoding: 'utf8' })
    if (b.status !== 0) fail(`go build 失败：\n${b.stdout}\n${b.stderr}`)
  }
  check('三个前置产物齐（index.html / dist-electron/main.js / hai-bridge）',
    fs.existsSync(index) && fs.existsSync(mainJs) && fs.existsSync(bridge))

  // ---- T1：真安装器装进「应用会去读的那一份」HOME ----
  HOME = fs.mkdtempSync(path.join(os.tmpdir(), 'ext-surface-electron-'))
  fs.mkdirSync(path.join(HOME, '.go-code'), { recursive: true })
  fs.writeFileSync(path.join(HOME, '.go-code', 'settings.json'),
    JSON.stringify({ theme: 'dark', lang: 'zh' }, null, 2))
  console.log(`\nT1 真安装器 → ${HOME}（= HAI_DATA_DIR）`)
  const inst = spawnSync('go', ['test', './desktop/bridge', '-run', 'TestSurfaceInstallExternal',
    '-count=1', '-v', '-args', `-dir=${PLUGIN_DIR}`, `-home=${HOME}`], { cwd: REPO, encoding: 'utf8' })
  const instOut = (inst.stdout || '') + (inst.stderr || '')
  const receipt = instOut.match(/\{[\s\S]*?"status": "applied"[\s\S]*?\n\s*\}/)
  if (receipt) console.log(receipt[0].split('\n').map((l) => '  ' + l.trim()).join('\n'))
  check('安装器回执 status=applied', /"status": "applied"/.test(instOut), instOut.split('\n').slice(-4).join(' | '))
  check('回执带 entries 条数', new RegExp(`"entries": ${expectTitles.length}`).test(instOut))
  const indexPath = path.join(HOME, '.go-code', 'surfaces', 'index.json')
  check('index.json 落在应用会读的位置（<HOME>/.go-code/surfaces）', fs.existsSync(indexPath), indexPath)
  const indexItems = fs.existsSync(indexPath) ? JSON.parse(fs.readFileSync(indexPath, 'utf8')).items : []
  check('index 里恰好这一条插件（enabled）',
    indexItems.length === 1 && indexItems[0].id === manifest.id && indexItems[0].enabled === true,
    JSON.stringify(indexItems))

  // ---- T2：启动真应用 → ＋ 菜单里出现入口 ----
  console.log('\nT2 启动真 Electron（HAI_DATA_DIR 多实例模式）→ ＋ 菜单')
  app = await _electron.launch({
    executablePath: ELECTRON, args: ['.'], cwd: APP,
    env: { ...process.env, HOME, HAI_DATA_DIR: HOME },
  })
  const win = await app.firstWindow()
  const errs = []
  win.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  await win.waitForLoadState('domcontentloaded')
  const WS = fs.mkdtempSync(path.join(os.tmpdir(), 'ext-surface-ws-'))
  await win.evaluate(([ws]) => {
    localStorage.setItem('go-code.projects', JSON.stringify({ paths: [ws], active: ws }))
  }, [WS])
  await win.reload()
  await waitFor(() => win.locator('button', { hasText: '设置' }).first().isVisible(), { label: '主界面就绪' })

  // ---- T2a：L0（D6 一次性交接）在**生产档真 preload**里成立 ----
  // 这一段不是可选的装饰：上面 T2 的菜单能出来，正是靠 bridge.surfaceIndex 走通了真 claim
  // —— 但「菜单出来了」也可能来自别的通路，所以把边界自己钉一遍。
  console.log('\nT2a 生产档的宿主边界（真 preload + 真 claim）')
  const boundary = await win.evaluate(() => ({
    desktopType: typeof window.desktop,
    hasDesktopKey: Object.prototype.hasOwnProperty.call(window, 'desktop'),
    claimType: typeof window.__haiClaimHostApi,
    secondClaim: typeof window.__haiClaimHostApi === 'function' ? window.__haiClaimHostApi() : 'missing',
    testBridge: typeof window.__haiTestBridge,
  }))
  check('claim 函数是 preload 唯一暴露面',
    boundary.claimType === 'function', JSON.stringify(boundary))
  check('window.desktop 在生产档不存在（不是隐藏，是没有这个属性）',
    boundary.desktopType === 'undefined' && boundary.hasDesktopKey === false, JSON.stringify(boundary))
  check('宿主已交接 → 第二次 claim 返回 null', boundary.secondClaim === null, String(boundary.secondClaim))
  check('生产档没有 __haiTestBridge（探针口只在非生产构建）', boundary.testBridge === 'undefined', boundary.testBridge)

  // 展开右栏：折叠态下 ＋ 在图标条里看不到，点一个标签图标即可展开（与 ego 冒烟同一条路）。
  const activityBtn = win.getByRole('button', { name: '链路', exact: true }).first()
  await waitFor(() => activityBtn.count().then((n) => n > 0), { label: '右栏图标条' })
  await activityBtn.click({ timeout: 5000 }).catch(() => {})
  await waitFor(() => win.locator('.rtabs-add').count().then((n) => n > 0), { label: '＋ 按钮' })
  await win.locator('.rtabs-add').click()
  await waitFor(() => win.locator('[data-plugin-entry]').count().then((n) => n > 0), { label: '＋ 菜单里的插件入口' })
  const items = await win.locator('[data-plugin-entry]').evaluateAll((ns) => ns.map((n) => ({
    key: n.getAttribute('data-plugin-entry'),
    text: (n.textContent || '').trim(),
  })))
  console.log(`  菜单里的插件入口：${JSON.stringify(items)}`)
  check(`＋ 菜单里 ${expectTitles.length} 个入口全在（真桥 → 真 index → 真菜单）`,
    items.length === expectTitles.length, JSON.stringify(items))
  check('标题 = manifest 声明的 title（菜单/标签同源）',
    JSON.stringify(items.map((i) => i.text)) === JSON.stringify(expectTitles), JSON.stringify(items.map((i) => i.text)))

  // ---- T3：点开 → 面板渲染出非空内容（真 hai-surface:// 协议加载 panel.js）----
  console.log('\nT3 点第一个入口 → 面板渲染')
  await win.locator('[data-plugin-entry]').first().click()
  await sleep(1200) // 真协议的 import + 面板异步取数
  const facts = await win.evaluate(() => {
    const body = document.querySelector('.right-body')
    if (!body) return { ok: false }
    const text = (body.innerText || '').trim()
    return {
      ok: true, text, textLen: text.length,
      crash: !!body.querySelector('.surface-crash'),
      tombstone: !!body.querySelector('.surface-tombstone'),
      reason: body.querySelector('.surface-tombstone')?.getAttribute('data-reason') || '',
      crashText: body.querySelector('.surface-crash-msg')?.textContent || '',
    }
  })
  console.log(`  面板文本：${JSON.stringify((facts.text || '').slice(0, 120))}`)
  check('面板渲染出非空内容（真协议把 panel.js 送进来了）', facts.textLen > 0, JSON.stringify(facts).slice(0, 200))
  check('不是墓碑（hai-surface:// 协议与 kind 都对）', !facts.tombstone, facts.reason)
  check('不是崩溃卡', !facts.crash, facts.crashText)
  check('全程零 pageerror', errs.length === 0, errs.join(' | '))
  await app.close()
  app = null
} catch (e) {
  if (e instanceof Precondition) {
    console.error(`\n前置不满足：${e.message}`)
    code = e.exitCode
  } else {
    console.error('\n冒烟异常：', e)
    code = 1
  }
}

console.log('')
if (fails.length) {
  console.log(`❌ ${fails.length} 项失败：\n  - ${fails.join('\n  - ')}`)
  code = code || 1
} else if (code === 0) {
  console.log('✅ 全部通过（verify_external_surface_electron）')
}
try { await app?.close() } catch { /* 已退出 */ }
process.exit(code)
