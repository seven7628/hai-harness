#!/usr/bin/env node
// verify_web_server_surface.mjs —— 带 server 的 URL 型插件：安装/自动起服务/满幅/多标签/重启。
//
// 这条探针刻意把「应用已经启动」作为前置：插件是在应用运行**之后**由真安装器写进
// HOME 的。它验的是完整链路：
//   1. bridge 的 index watcher 发现外部安装 → surfaces_changed → loader 重读索引（不重启应用）；
//   2. ＋ 菜单出现入口；
//   3. 点入口 → main 探活/ spawn `node server.mjs` → 就绪后 loadURL（页面不是连接拒绝页）；
//   4. 页面满幅（right-body.full-mode + host 矩形 = 内容区）；
//   5. 两个 entry = 两个标签/两个原生视图，切回来 DOM 状态还在；
//   6. kill server → 宿主错误态 + 重试 → 重新 spawn；
//   7. 关标签不杀服务；关应用杀服务。
//
// 用法：cd desktop/app && npm run e2e:web-server-surface
// 退出码：0 通过 / 1 断言失败 / 2 前置不满足（缺产物 / 端口 / 安装失败）。
import { _electron } from 'playwright'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { freePort, ProbePrecondition } from './_serve.mjs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const REPO = path.resolve(APP, '../..')
const ELECTRON = path.join(APP, 'node_modules', '.bin', 'electron')
const TEMPLATE = path.resolve(APP, '../../templates/surface-web-server')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const fails = []
const check = (label, cond, detail = '') => {
  console.log(`${cond ? '  ✅' : '  ❌'} ${label}${cond ? '' : detail ? ` —— ${detail}` : ''}`)
  if (!cond) fails.push(label)
}
async function waitFor(fn, { timeout = 30000, label = '条件' } = {}) {
  const end = Date.now() + timeout
  let last
  while (Date.now() < end) {
    try { const v = await fn(); if (v) return v; last = v } catch (e) { last = e }
    await sleep(250)
  }
  throw new Error(`等待超时：${label}${last ? `（最后：${String(last).slice(0, 300)}）` : ''}`)
}
function precondition(msg) { throw new ProbePrecondition(msg) }
async function health(port) {
  try {
    const r = await fetch(`http://127.0.0.1:${port}/health`, { signal: AbortSignal.timeout(1000) })
    if (!r.ok) return null
    return await r.json()
  } catch {
    return null
  }
}
function processAlive(pid) {
  try { process.kill(pid, 0); return true } catch { return false }
}

let app = null
let home = ''
let pluginDir = ''
let port = 0
let code = 0
try {
  console.log('\nT0 前置')
  if (!fs.existsSync(path.join(TEMPLATE, 'surface.json'))) precondition(`模板不存在：${TEMPLATE}`)
  const index = path.join(APP, 'dist', 'index.html')
  const mainJs = path.join(APP, 'dist-electron', 'main.js')
  const bridge = path.join(APP, 'dist', 'hai-bridge')
  if (!fs.existsSync(index) || !fs.existsSync(mainJs) || !fs.existsSync(bridge)) {
    precondition('缺产物：先 npm run build:electron && npx vite build && go build -o dist/hai-bridge ./desktop/bridge')
  }
  port = await freePort()
  pluginDir = fs.mkdtempSync(path.join(os.tmpdir(), 'web-server-plugin-'))
  fs.cpSync(TEMPLATE, pluginDir, { recursive: true })
  const manifestPath = path.join(pluginDir, 'surface.json')
  const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'))
  manifest.web.url = `http://127.0.0.1:${port}`
  manifest.web.start.env = { ...(manifest.web.start.env || {}), PORT: String(port) }
  fs.writeFileSync(manifestPath, JSON.stringify(manifest, null, 2))
  console.log(`  模板：${pluginDir}（port ${port}）`)

  home = fs.mkdtempSync(path.join(os.tmpdir(), 'web-server-home-'))
  fs.mkdirSync(path.join(home, '.go-code'), { recursive: true })
  fs.writeFileSync(path.join(home, '.go-code', 'settings.json'), JSON.stringify({ theme: 'dark', lang: 'zh' }, null, 2))

  console.log('\nT1 启动应用（此时插件还没装、服务也没起）')
  check('安装前端口没有服务', (await health(port)) === null)
  app = await _electron.launch({
    executablePath: ELECTRON, args: ['.'], cwd: APP,
    env: { ...process.env, HOME: home, HAI_DATA_DIR: home, VITE_DEV_SERVER_URL: '', HAI_MULTI: '', ELECTRON_RUN_AS_NODE: '' },
  })
  const win = await app.firstWindow()
  const errs = []
  win.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  await win.waitForLoadState('domcontentloaded')
  const ws = fs.mkdtempSync(path.join(os.tmpdir(), 'web-server-ws-'))
  await win.evaluate(([p]) => localStorage.setItem('go-code.projects', JSON.stringify({ paths: [p], active: p })), [ws])
  await win.reload()
  await waitFor(() => win.locator('button', { hasText: '设置' }).first().isVisible(), { label: '主界面就绪' })
  const activity = win.locator('[data-kind="activity"]').first()
  await waitFor(() => activity.count().then((n) => n > 0), { label: '右栏图标条' })
  await activity.click({ timeout: 5000 }).catch(() => {})
  await waitFor(() => win.locator('.rtabs-add').count().then((n) => n > 0), { label: '＋ 按钮' })

  console.log('\nT2 应用运行中安装插件 → index watcher → surfaces_changed → loader')
  const inst = spawnSync('go', ['test', './desktop/bridge', '-run', 'TestSurfaceInstallExternal', '-count=1', '-v',
    '-args', `-dir=${pluginDir}`, `-home=${home}`, '-dev=1'], { cwd: REPO, encoding: 'utf8' })
  const instOut = (inst.stdout || '') + (inst.stderr || '')
  check('真安装器 applied（dev 软链）', /"status": "applied"/.test(instOut), instOut.slice(-1200))
  // 不 reload、不重启：等 bridge 的 index watcher 把 surfaces_changed 推给 loader。
  await win.locator('.rtabs-add').click().catch(() => {})
  await waitFor(() => win.locator('[data-plugin-entry]').count().then((n) => n >= 2), { label: '安装后菜单入口（无需重启）', timeout: 15000 })
  const items = await win.locator('[data-plugin-entry]').evaluateAll((ns) => ns.map((n) => (n.textContent || '').trim()))
  check('入口 = 首页/后台', JSON.stringify(items) === JSON.stringify(['首页', '后台']), JSON.stringify(items))

  const wcList = () => app.evaluate(({ webContents }) => webContents.getAllWebContents().map((w) => ({ id: w.id, url: w.getURL(), type: w.getType() })))
  const viewList = async () => (await wcList()).filter((w) => w.url.startsWith(`http://127.0.0.1:${port}`))
  const inView = async (exactUrl, js) => app.evaluate(async ({ webContents }, [u, src]) => {
    const wc = webContents.getAllWebContents().find((w) => w.getURL() === u)
    if (!wc) return null
    return await wc.executeJavaScript(src, true)
  }, [exactUrl, js])

  console.log('\nT3 点首页 → 宿主自动起 server → loadURL')
  check('点开前服务仍然没起', (await health(port)) === null)
  await win.locator('[data-plugin-entry]').nth(0).click()
  const homeUrl = `http://127.0.0.1:${port}/`
  await waitFor(async () => (await viewList()).some((w) => w.url === homeUrl), { label: '首页原生视图', timeout: 20000 })
  const h1 = await waitFor(() => health(port), { label: 'server /health 就绪', timeout: 20000 })
  check('宿主自动启动了服务（health.pid 存在）', !!h1 && typeof h1.pid === 'number', JSON.stringify(h1))
  check('页面 URL 是 web.url + /', (await viewList()).some((w) => w.url === homeUrl), JSON.stringify(await viewList()))
  const probe = await waitFor(() => inView(homeUrl, `({
    host: typeof window.haiHost,
    pluginId: window.haiHost && window.haiHost.pluginId,
    title: document.title,
    text: (document.body.innerText || '').slice(0, 120)
  })`), { label: '页面脚本', timeout: 15000 })
  check('window.haiHost 注入', probe?.host === 'object' || probe?.host === 'function', JSON.stringify(probe))
  check('pluginId = web-server-demo', probe?.pluginId === 'web-server-demo', JSON.stringify(probe))
  const sess = await waitFor(() => inView(homeUrl, `(async () => {
    const r = await window.haiHost.command('agent.session', {})
    return r
  })()`), { label: 'agent.session', timeout: 15000 })
  check('agent.session 回执 ok 且带顶层 session_id', sess?.ok === true && typeof sess?.session_id === 'string', JSON.stringify(sess))

  console.log('\nT4 满幅')
  const rect = await win.evaluate(() => {
    const body = document.querySelector('.right-body')
    const host = document.querySelector('.plugin-app')
    if (!body || !host) return null
    const b = body.getBoundingClientRect()
    const h = host.getBoundingClientRect()
    return { cls: body.className, padding: getComputedStyle(body).padding, overflow: getComputedStyle(body).overflow,
      bw: b.width, bh: b.height, hw: h.width, hh: h.height, dx: h.left - b.left, dy: h.top - b.top }
  })
  check('right-body.full-mode', !!rect && /\bfull-mode\b/.test(rect.cls), JSON.stringify(rect))
  check('padding=0 / overflow=hidden', rect?.padding === '0px' && rect?.overflow === 'hidden', JSON.stringify(rect))
  check('host 铺满（误差 ≤2px）', !!rect && Math.abs(rect.dx) <= 2 && Math.abs(rect.dy) <= 2 && Math.abs(rect.hw - rect.bw) <= 2 && Math.abs(rect.hh - rect.bh) <= 2, JSON.stringify(rect))

  console.log('\nT5 第二个入口 → 第二个标签；切回来状态还在')
  await inView(homeUrl, `window.__keep = 'home-state'`)
  await win.locator('.rtabs-add').click()
  await waitFor(() => win.locator('[data-plugin-entry]').count().then((n) => n >= 2), { label: '菜单再次打开' })
  await win.locator('[data-plugin-entry]').nth(1).click()
  const adminUrl = `http://127.0.0.1:${port}/admin`
  await waitFor(async () => (await viewList()).some((w) => w.url === adminUrl), { label: '后台原生视图', timeout: 20000 })
  check('两个原生视图并存', (await viewList()).length === 2, JSON.stringify(await viewList()))
  const hAdmin = await health(port)
  check('两个标签共享同一个服务进程', !!hAdmin && hAdmin.pid === h1.pid, `${h1?.pid} -> ${hAdmin?.pid}`)
  await win.locator('.rtab').filter({ hasText: '首页' }).click()
  await sleep(500)
  check('切回首页仍是同一个页面实例', (await inView(homeUrl, 'window.__keep')) === 'home-state')
  const hBack = await health(port)
  check('切标签不重启服务', !!hBack && hBack.pid === h1.pid, `${h1?.pid} -> ${hBack?.pid}`)
  const tabs = await win.locator('.rtab .rtab-t').allTextContents()
  check('标签标题 = 入口标题', tabs.includes('首页') && tabs.includes('后台'), JSON.stringify(tabs))

  console.log('\nT6 kill server → 宿主错误态 + 重试 → 重新 spawn')
  const pid1 = h1.pid
  process.kill(pid1, 'SIGTERM')
  await waitFor(() => health(port).then((h) => h === null), { label: '服务已停', timeout: 10000 })
  const errState = await waitFor(() => win.evaluate(() => {
    const el = document.querySelector('.plugin-app-state[data-state="error"]')
    return el ? { text: (el.textContent || '').trim(), logs: !!el.querySelector('.plugin-app-state-logs') } : null
  }), { label: '宿主错误态', timeout: 15000 })
  check('停服后显示宿主错误态', !!errState && /启动|服务|失败/.test(errState.text || ''), JSON.stringify(errState))
  await win.locator('.plugin-app-state[data-state="error"] button').first().click()
  const h2 = await waitFor(() => health(port), { label: '重试后服务就绪', timeout: 20000 })
  check('重试重新 spawn 了新进程', !!h2 && h2.pid !== pid1, `${pid1} -> ${JSON.stringify(h2)}`)

  console.log('\nT7 关标签不杀服务；关应用才杀')
  await win.locator('.rtab').filter({ hasText: '首页' }).locator('.rtab-close').click({ force: true })
  await waitFor(async () => !(await viewList()).some((w) => w.url === homeUrl), { label: '首页视图回收', timeout: 10000 })
  check('首页视图已销毁', !(await viewList()).some((w) => w.url === homeUrl), JSON.stringify(await viewList()))
  check('关标签后 server 仍在跑', !!processAlive(h2.pid) && !!(await health(port)), JSON.stringify(h2))
  check('全程主窗口零 pageerror', errs.length === 0, errs.join(' | '))

  await app.close(); app = null
  await waitFor(async () => (await health(port)) === null, { label: '应用退出后服务停止', timeout: 10000 })
  check('应用退出后服务停止', (await health(port)) === null)
  check('服务进程已回收', !processAlive(h2.pid), String(h2.pid))
} catch (e) {
  if (e instanceof ProbePrecondition) {
    console.error(`\n前置不满足：${e.message}`)
    code = 2
  } else {
    console.error('\n探针异常：', e)
    code = 1
  }
} finally {
  try { await app?.close() } catch { /* 已退出 */ }
  try { fs.rmSync(pluginDir, { recursive: true, force: true }) } catch { /* 忽略 */ }
  try { fs.rmSync(home, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
console.log('')
if (fails.length) { console.log(`❌ ${fails.length} 项失败：\n  - ${fails.join('\n  - ')}`); code = code || 1 }
else if (code === 0) console.log('✅ 全部通过（web server surface）')
process.exit(code)
