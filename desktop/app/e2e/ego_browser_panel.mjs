// ego 引擎下右侧浏览器面板接线验证（真实 Electron + Go bridge）。
//
// 背景（2026-09 报障）：Browser Use 引擎切到 ego lite 后，右栏 web tab 显示「未启用」，
// 且无法用 file:// / HTML 预览。根因是 BrowserTab 的原生内嵌视图路径此前整体挂在
// 「插件 enabled」上：ego 与 Browser Use 互斥（bridge 切 ego 时停用插件），于是
//   - 头部状态显示「未启用」（看不到“ego 模式”）
//   - 容器/工具条不渲染 → openHtmlInBrowser 的 embedShow/embedNavigate 无宿主
// 修复后 ego 模式仍走 Electron 原生内嵌视图（不依赖插件），本地预览与导航可用。
//
// 覆盖：
//   T1 settings.json browser_engine=ego → 头部状态显示「ego 模式」（不再是「未启用」）
//   T2 工具条（地址栏）存在 → 可自主导航 file:///外部网址
//   T3 openHtmlInBrowser 接线：写一个 .html → 调 store 动作 → 原生视图真的加载 file://
//   T4 切回 mcp → 头部不再显示 ego 模式（互斥开关双向生效）
//
// 用法：node e2e/ego_browser_panel.mjs
import { _electron } from 'playwright'
import { spawnSync } from 'child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const APP = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const REPO = path.resolve(APP, '../..')
const ELECTRON = path.join(APP, 'node_modules', '.bin', 'electron')
const HOME = fs.mkdtempSync(path.join(os.tmpdir(), 'ego-e2e-home-'))
const WS = fs.mkdtempSync(path.join(os.tmpdir(), 'ego-e2e-ws-'))
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const index = path.join(APP, 'dist', 'index.html')
const mainJs = path.join(APP, 'dist-electron', 'main.js')
if (!fs.existsSync(index) || !fs.existsSync(mainJs)) {
  const b = spawnSync('npm', ['run', 'build'], { cwd: APP, encoding: 'utf8' })
  if (b.status !== 0) throw new Error(`npm run build 失败:\n${b.stdout}\n${b.stderr}`)
}
spawnSync('go', ['build', '-o', 'desktop/app/dist/hai-bridge', './desktop/bridge'], { cwd: REPO, encoding: 'utf8' })

// 预置 settings.json：浏览器引擎 = ego（复现报障条件；HOME 隔离，不动真实配置）
fs.mkdirSync(path.join(HOME, '.go-code'), { recursive: true })
fs.writeFileSync(path.join(HOME, '.go-code', 'settings.json'), JSON.stringify({
  theme: 'dark', lang: 'zh', browser_engine: 'ego',
}, null, 2))

async function waitFor(fn, { timeout = 30000, label = '条件' } = {}) {
  const end = Date.now() + timeout
  while (Date.now() < end) {
    try { const v = await fn(); if (v) return v } catch { /* 继续 */ }
    await sleep(250)
  }
  throw new Error(`等待超时: ${label}`)
}

const fails = []
function check(label, cond, detail = '') {
  if (cond) console.log(`  ✅ ${label}`)
  else { fails.push(label); console.log(`  ❌ ${label} ${detail}`) }
}

// HAI_DATA_DIR：多实例模式（跳过单实例锁）。本机可能正在跑用户的 HAI 实例，
// 不加这个会被 requestSingleInstanceLock 直接踢掉（表现为 launch 后立刻退出）。
const app = await _electron.launch({
  executablePath: ELECTRON, args: ['.'], cwd: APP,
  env: { ...process.env, HOME, HAI_DATA_DIR: HOME },
})
try {
  const win = await app.firstWindow()
  win.on('console', (m) => { if (m.type() === 'error' && !/favicon/.test(m.text())) console.log('[renderer-err]', m.text()) })
  await win.waitForLoadState('domcontentloaded')
  await win.evaluate(([ws]) => { localStorage.setItem('go-code.projects', JSON.stringify({ paths: [ws], active: ws })) }, [WS])
  await win.reload()
  await waitFor(() => win.locator('button', { hasText: '设置' }).first().isVisible(), { label: '主界面就绪' })

  // —— T1：引擎读成 ego，且右栏头部显示「ego 模式」而非「未启用」——
  console.log('T1 ego 引擎下右栏头部状态')
  // 打开右栏并切到 web tab：tab 按钮用 aria-label（= i18n 的 rp.tab.web，中文「网页」），
  // 点击即展开右栏 + 切 tab（见 RightPanel 的 rbtn onClick）。收起态下按钮在右侧图标条里。
  // 点击可能落在启动竞态里 → 只点一次，然后轮询 DOM 存在性。
  // 注意不能用 Playwright 的 isVisible()：网页 tab 是沉浸模式，头部 chrome
  // （含 .fp-hd/.mcp-state）被 display:none 收起（把空间让给内嵌页面），
  // 元素在 DOM 里但 computed 不可见——用存在性 + 文本断言才对。
  const webTab = win.getByRole('button', { name: '网页', exact: true }).first()
  await waitFor(() => webTab.count().then((n) => n > 0), { label: 'web tab 图标存在' })
  await webTab.click({ timeout: 5000 }).catch(() => {})
  const rendered = await waitFor(
    () => win.evaluate(() => !!document.querySelector('.file-preview .mcp-state')),
    { label: 'web tab 面板渲染' },
  ).catch(() => false)
  if (!rendered) throw new Error('web tab 面板始终未渲染')
  await sleep(600)

  const stateText = await win.evaluate(() => {
    const el = document.querySelector('.file-preview .mcp-state')
    return el ? el.textContent.trim() : null
  })
  check('右栏 web tab 头部状态可读', stateText != null, `stateText=${stateText}`)
  check('ego 下不显示「浏览器未启用」', stateText !== '浏览器未启用', `实际=${stateText}`)

  // —— T2：ego 下工具条（地址栏）存在 ——
  console.log('T2 ego 下本地预览工具条')
  const hasUrlBar = await win.locator('.browser-toolbar .bt-url').count()
  check('地址栏存在（可自主导航 file://）', hasUrlBar > 0, `count=${hasUrlBar}`)

  // —— T3：openHtmlInBrowser → 原生视图加载 file:// ——
  console.log('T3 file:// HTML 预览接线')
  const htmlPath = path.join(WS, 'preview.html')
  fs.writeFileSync(htmlPath, '<html><body><h1 id="ego-preview-ok">EGO-PREVIEW-OK</h1></body></html>')
  const loaded = await win.evaluate(async ([p]) => {
    // store 动作经 window 暴露？未暴露则走 preload 的 embedShow+embedNavigate 等价路径
    const d = window.desktop
    if (!d?.embedShow || !d?.embedNavigate) return { skipped: 'no embed api' }
    const shown = await d.embedShow({})
    if (!shown?.ok) return { error: shown?.error ?? 'embedShow failed' }
    const fileUrl = 'file://' + p.split('/').map((seg) => encodeURIComponent(seg)).join('/')
    const r = await d.embedNavigate(fileUrl)
    return { ok: !!r?.ok, error: r?.error, fileUrl }
  }, [htmlPath])
  check('embedShow + embedNavigate(file://) 成功', loaded?.ok === true, JSON.stringify(loaded))

  // 直连 CDP 读页面 DOM：确认原生视图里真的是我们的 HTML（而非白屏/旧页）。
  // 这是「file:// 能用」的实证——只验 embedNavigate 返回 ok 不足以排除白屏。
  const probe = await win.evaluate(async () => {
    const d = window.desktop
    if (!d?.embedDebuggerProxy) return null
    return d.embedDebuggerProxy()
  })
  check('调试代理可用（原生视图已 attach）', probe?.ok === true, JSON.stringify(probe))

  let domOk = false
  let domDetail = ''
  for (let i = 0; i < 20; i++) {
    const r = await win.evaluate(async () => {
      const d = window.desktop
      if (!d?.embedCommand) return null
      const res = await d.embedCommand({
        method: 'Runtime.evaluate',
        params: { expression: 'document.getElementById("ego-preview-ok")?.textContent ?? document.body?.innerText?.slice(0,80) ?? ""', returnByValue: true },
      })
      // 响应形状：{ok, result} → result 由 embed.ts 按协议解包，可能是
      // {result:{value}} / {value} / 字符串。全试一遍，避免形状假设导致误判。
      const shapes = [
        res?.result?.result?.value,
        res?.result?.value,
        typeof res?.result === 'string' ? res.result : undefined,
      ]
      const val = shapes.find((v) => typeof v === 'string')
      return { ok: !!res?.ok, val: val ?? null, raw: JSON.stringify(res)?.slice(0, 300) }
    })
    domDetail = JSON.stringify(r).slice(0, 400)
    if (r?.ok && typeof r.val === 'string' && r.val.includes('EGO-PREVIEW-OK')) { domOk = true; break }
    await sleep(300)
  }
  check('原生视图真的渲染了该 file:// 页面（DOM 实测）', domOk, domDetail)

  // —— T4：切回 mcp → 头部不再显示 ego 模式 ——
  // 走与 UI 同一条路径：先落 settings.json（渲染层读它水合），再发 browser_engine_set
  // （bridge 热应用）。只发命令不改 settings 会不同步——真实 switchEngine 也是两步。
  console.log('T4 切回 mcp（互斥双向）')
  const switched = await win.evaluate(async () => {
    const d = window.desktop
    if (!d?.settingsSet || !d?.command) return null
    const saved = await d.settingsSet({ browser_engine: 'mcp' })
    if (!saved?.ok) return { ok: false, step: 'settingsSet', error: saved?.error }
    const r = await d.command({ type: 'browser_engine_set', id: Date.now(), payload: { engine: 'mcp' } })
    return { ok: !!r?.ok, step: 'command', error: r?.error }
  })
  check('落盘 + browser_engine_set(mcp) 均成功', switched?.ok === true, JSON.stringify(switched))
  // 引擎状态由 store 持有 → 需触发一次设置水合（真实路径是切 tab/重进设置）。
  // 这里直接 reload，等价于用户重启客户端后读盘。
  await win.reload()
  await waitFor(() => win.locator('button', { hasText: '设置' }).first().isVisible(), { label: 'reload 后主界面' })
  await win.getByRole('button', { name: '网页', exact: true }).first().click({ timeout: 5000 }).catch(() => {})
  await waitFor(() => win.evaluate(() => !!document.querySelector('.file-preview .mcp-state')), { label: 'reload 后面板渲染' })
  const after = await win.evaluate(() => {
    const el = document.querySelector('.file-preview .mcp-state')
    return el ? el.textContent.trim() : null
  })
  check('切回 mcp 后不再是 ego 模式文案（重读 settings 生效）', after !== 'ego 模式', `实际=${after}`)
} finally {
  await app.close()
}

console.log(`\n${fails.length === 0 ? 'ALL PASS' : `FAILED (${fails.length}): ${fails.join('; ')}`}`)
process.exit(fails.length === 0 ? 0 : 1)
