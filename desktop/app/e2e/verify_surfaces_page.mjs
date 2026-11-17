// verify_surfaces_page.mjs —— 设置 →「面板插件」页（PR-14b）。
//
// 探针只替**一条边界**：fake host api 的四个 surface 方法（00-conventions §4.7）。
// 真代码路径全部保留：`SurfacesPage` 组件本体、`useT`（i18n 真表）、`ConfirmModal`、
// `surfaces/manage.ts` 的 reason 映射、以及「动作后重拉 index + audit」的刷新逻辑。
//
// 钉住的七条（每条都能独立变红）：
//   T1 列表：3 条（启用 / 停用 / 带 error），version + hash 前 8 位 + 声明 chip 都在；
//   T2 开关：点开 → IPC 收到 {id, enabled:false}（**目标值**，不是反转反了），成功后列表刷新；
//   T3 卸载：先弹确认框（✗ 不发 IPC），取消 = 什么都不发生，确认才发 surfaceRemove(id)；
//   T4 审计：3 行表格（最新在上）+ 坏行不炸 + 空态一句话；
//   T5 失败：ok:false 的可读原因贴在那一行上，**不 alert**；
//   T6 白名单说明：四条只读命令以 chip 形式出现在页面顶部；
//   T9 **清除数据**（R17a）：每行一个按钮、先二次确认、确认后 `<id>:` 前缀的键消失 +
//      toast 出现；**卸载路径同样清**（两条路径都不许只依赖 surfaces_changed）。
//   T7 全程 pageerror = 0（含坏行刷新那一步）。
//
// 端口 5252（5227 = PR-12 的 ui 快照、5228 = PR-14 的插件加载器、5229 = PR-14c 的跨重启）。
// R17a 迁移 = **并行轮次避让**：5225–5232 那一段正被并行 R17b 的探针占着，同一时刻
// 启动会互相顶掉 vite（--strictPort 下后起的那个直接 exit 2）。
import fs from 'node:fs'
import path from 'node:path'
import { spawn } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'
import { exitCodeOf, isPrecondition, waitProbePage } from './_serve.mjs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')

const PORT = 5252
const PROBE_DIR = path.join(__dirname, '_surfacespageprobe_run')
const PROBE_MARKER = '<title>surfaces-page probe</title>'
const PROBE_URL = `http://localhost:${PORT}/e2e/_surfacespageprobe_run/index.html`

// —— 探针页 ——
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8">'
  + `<title>surfaces-page probe</title></head>`
  + '<body><div id="root" style="height:100vh"></div>'
  + '<script type="module" src="./main.tsx"></script></body></html>')

// 假 host api 的四条方法，语义按 bridge 的真实语义写：
//   · surfaceSetEnabled 真的翻那一份 index（页面 refresh 之后必须看到新值，否则 T2 假绿）；
//   · surfaceRemove 真的把条目去掉（T3 数行数靠它）；
//   · surfaceAudit 原样回 lines（坏行由调用方注入，页面自己不许炸）；
//   · 每次调用都记进 __probe.calls —— T2/T3 断言的是「IPC 收到了什么」而不是「UI 变了」。
// 另外把 window.alert 计数（T5 证明失败路径不弹窗）。
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import '../../src/index.css'

type Call = { fn: string; args: unknown[] }
const calls: Call[] = []
let alerts = 0
;(window as any).alert = () => { alerts++ }

const ITEMS = [
  {
    id: 'alpha', version: '1.2.0', hash: 'a'.repeat(64), enabled: true, kind: 'plugin:alpha',
    commands: ['git_snapshot', 'workspace_list'], data: ['surfaces_changed'], entry: 'panel.js',
  },
  {
    id: 'beta', version: '0.3.1', hash: 'b'.repeat(64), enabled: false, kind: 'plugin:beta',
    commands: [], data: [], entry: 'panel.js',
  },
  {
    id: 'gamma', version: '0.0.9', hash: 'c'.repeat(64), enabled: true, kind: 'plugin:gamma',
    commands: ['ui_inspect'], data: [], entry: 'panel.js',
    error: 'surface.json 缺少 kind',
  },
]
// audit.jsonl 是「旧 → 新」的文件顺序；页面上应当**最新在上**（T4 断言第一行是最后这条）。
const AUDIT = [
  { id: 'alpha', action: 'install', ok: true, ts: '2026-11-12T09:59:00.000000000Z' },
  { id: 'alpha', action: 'command', cmd: 'git_snapshot', ok: true, ts: '2026-11-12T10:00:00.000000000Z' },
  { id: 'alpha', action: 'command', cmd: 'file_save', ok: false, ts: '2026-11-12T10:00:05.000000000Z', reason: '未在 manifest.commands 中声明' },
]

let items = ITEMS.map((i) => ({ ...i }))
let audit: unknown[] = AUDIT.map((a) => ({ ...a }))
const failNext: Record<string, string> = {}

const log = (fn: string, ...args: unknown[]) => { calls.push({ fn, args }) }

let settings: Record<string, unknown> = { theme: 'dark', lang: 'zh' }
const stub = {
  settingsSet: async (patch: Record<string, unknown>) => {
    log('settingsSet', patch)
    settings = { ...settings, ...patch }
    return { ok: true, settings }
  },
  command: async (cmd: Record<string, unknown>) => { log('send', cmd); return { ok: true } },
  surfaceIndex: async () => {
    log('surfaceIndex')
    return { version: 1, items: items.map((i) => ({ ...i })) }
  },
  surfaceSetEnabled: async (id: string, enabled: boolean) => {
    log('surfaceSetEnabled', id, enabled)
    if (failNext.surfaceSetEnabled) return { ok: false, reason: failNext.surfaceSetEnabled }
    items = items.map((i) => (i.id === id ? { ...i, enabled } : i))
    return { ok: true, id, enabled }
  },
  surfaceRemove: async (id: string) => {
    log('surfaceRemove', id)
    if (failNext.surfaceRemove) return { ok: false, reason: failNext.surfaceRemove }
    items = items.filter((i) => i.id !== id)
    return { ok: true, id, status: 'applied' }
  },
  surfaceAudit: async (limit?: number) => {
    log('surfaceAudit', limit)
    if (failNext.surfaceAudit) return { ok: false, reason: failNext.surfaceAudit, lines: [] }
    return { ok: true, lines: audit }
  },
}
;(window as any).desktop = stub

;(window as any).__probe = {
  calls,
  reset: () => { calls.length = 0 },
  alerts: () => alerts,
  setAudit: (next: unknown[]) => { audit = next },
  fail: (fn: string, reason: string) => { failNext[fn] = reason },
  clearFail: () => { for (const k of Object.keys(failNext)) delete failNext[k] },
  items: () => items.map((i) => ({ id: i.id, enabled: i.enabled })),
}

// 动态 import：fake 必须先于组件模块求值就位（i18n → store 在模块加载期
// 就会用 getTransport() 分流；静态 import 会被提升到桩赋值之前，与生产里 preload
// 先于 bundle 的顺序不一致 —— verify_plugin_loader.mjs 同一段注释）。
void (async () => {
  const [{ createRoot }, { default: SurfacesPage }, { SurfaceTombstone }, { useAppStore }] = await Promise.all([
    import('react-dom/client'),
    import('../../src/components/SurfacesPage'),
    import('../../src/surfaces/SurfaceBoundary'),
    import('../../src/store/useAppStore'),
  ])
  // toast 的真实产出点：SurfacesPage 调 store.showToast（ToastView 挂在 App 壳里，
  // 这个探针只渲染设置页 —— 所以断言读的是**生产状态**，不是探针自造的 DOM）。
  ;(window as any).__probe.toast = () => useAppStore.getState().toast
  createRoot(document.getElementById('root')!).render(<SurfacesPage />)
  // 第二个容器：一块 plugin: 的墓碑（右栏「重新启用」那条路；右栏本身在
  // verify_surface_boundary.mjs 里测，这里只钉「plugin: 会先去写 index」）。
  //
  // reason='disabled' 才会出「重新启用」按钮 —— 这正是 health 的本地禁用集那一档。
  const tombHost = document.createElement('div')
  tombHost.id = 'tomb'
  document.body.appendChild(tombHost)
  createRoot(tombHost).render(
    <SurfaceTombstone kind="plugin:alpha" title="Alpha 面板" reason="disabled" onRemove={() => {}} />,
  )
  ;(window as any).__probe.mounted = true
})()
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

let code = 0
// 页内错误收集器放在 try 外面：catch 里要把它打出来（只报「超时」等于没说）。
let page = null
const errs = []          // pageerror（唯一的硬判据：全程必须 0）
const consoleErrors = [] // console.error（诊断用，不参与断言：favicon 404 也在这里）
try {
  await waitProbePage(PROBE_URL, { marker: PROBE_MARKER, probe: 'e2e:surfaces-page', child: server })
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  page = await browser.newPage({ viewport: { width: 1100, height: 900 } })
  page.setDefaultTimeout(8000)
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  // 探针自身的 console.error 也要看得见：组件在渲染期抛错时 pageerror 只有一行，
  // 真正的原因（import 失败 / 缺 hook）常常只出现在 console 里。
  // console 错误单独收：favicon 404 之类的噪音不该被算成「页面炸了」，但排查时要看得见。
  page.on('console', (m) => { if (m.type() === 'error') consoleErrors.push(m.text().slice(0, 200)) })

  const calls = () => page.evaluate(() => window.__probe.calls)
  const reset = () => page.evaluate(() => window.__probe.reset())
  const callsOf = (fn) => calls().then((all) => all.filter((c) => c.fn === fn))
  const rows = () => page.locator('[data-surface-id]')
  const row = (id) => page.locator(`[data-surface-id="${id}"]`)
  const auditRows = () => page.locator('[data-surface-audit]')
  const stateOf = (id) => row(id).locator('[data-surface-state]').innerText()
  // 软等待：超时**不抛**。硬等待会把一次断言失败变成一行「locator timeout」，
  // 那既看不出哪条判据坏了，也会让「变异」实验只留下一个栈（R05 的教训）。
  const softWait = (fn, timeout = 3000) => page.waitForFunction(fn, null, { timeout }).catch(() => {})
  const softSelector = (sel, timeout = 3000) => page.waitForSelector(sel, { timeout }).catch(() => {})
  const textOf = async (loc) => (await loc.innerText()).trim()

  // ================= T1 列表渲染 =================
  console.log('\nT1 列表：3 条（启用 / 停用 / error）+ version + 短 hash + 声明 chip')
  // 先真的打开探针页（waitProbePage 只是 HTTP 就绪 + 归属自检，不开浏览器）：
  // 少了这一行，页面里的一切断言都会以「超时」的形式失败。
  await page.goto(PROBE_URL, { waitUntil: 'domcontentloaded' })
  await page.waitForFunction(() => window.__probe?.mounted, null, { timeout: 30000 })
  await page.waitForSelector('[data-surface-id="alpha"]')

  check('标题 = 面板', (await textOf(page.locator('.settings-sec-title').first())) === '面板')
  check('顶部说明非空', (await textOf(page.locator('.set-desc').first())).length > 20)
  check('列表 3 条', (await rows().count()) === 3, String(await rows().count()))

  check('alpha 显示「已启用」', (await stateOf('alpha')) === '已启用', await stateOf('alpha'))
  check('beta 显示「已停用」', (await stateOf('beta')) === '已停用', await stateOf('beta'))
  check('alpha 版本 1.2.0', (await textOf(row('alpha'))).includes('1.2.0'))
  check('alpha 短 hash = 前 8 位', (await textOf(row('alpha'))).includes('aaaaaaaa'), await textOf(row('alpha')))
  check('alpha 的 hash 只显示 8 位（不是 64 位）', !(await textOf(row('alpha'))).includes('a'.repeat(9)))

  const alphaCmds = await row('alpha').locator('[data-surface-command]').allInnerTexts()
  check('alpha 的 commands 声明 = git_snapshot + workspace_list',
    JSON.stringify(alphaCmds.map((s) => s.trim())) === JSON.stringify(['git_snapshot', 'workspace_list']), JSON.stringify(alphaCmds))
  const alphaData = await row('alpha').locator('[data-surface-data]').allInnerTexts()
  check('alpha 的 data 声明 = surfaces_changed', JSON.stringify(alphaData.map((s) => s.trim())) === JSON.stringify(['surfaces_changed']))
  check('beta 没有声明命令 → 显示「无」', (await textOf(row('beta'))).includes('无'))

  check('gamma 有 error 徽标', (await row('gamma').locator('[data-surface-error]').count()) === 1)
  check('gamma 的 error 原因可见', (await textOf(row('gamma').locator('[data-surface-error-msg]'))) === 'surface.json 缺少 kind')

  check('alpha 的开关是开着的', (await row('alpha').locator('.switch').getAttribute('aria-checked')) === 'true')
  check('beta 的开关是关着的', (await row('beta').locator('.switch').getAttribute('aria-checked')) === 'false')

  // ================= T6 白名单说明 =================
  console.log('\nT6 顶部白名单：四条只读命令都在（与 surfaces_command.go 同一集合）')
  const chips = await page.locator('.hook-note .mcp-type').allInnerTexts()
  check('四条只读命令都以 chip 出现',
    JSON.stringify(chips.map((s) => s.trim())) === JSON.stringify(['git_snapshot', 'workspace_list', 'session_list', 'ui_inspect']),
    JSON.stringify(chips))

  // ================= T6b 功能开关（surfaceTools）=================
  console.log('\nT6b 功能开关：默认关 → 点击写 settings:set，不误发 surface 动作')
  const toolsBox = page.locator('[data-surface-tools]')
  check('surfaceTools 默认关（settings.features 缺省）',
    (await toolsBox.getAttribute('data-surface-tools-on')) === '0')
  check('默认显示「已关闭」', (await textOf(toolsBox.locator('[data-surface-tools-state]'))) === '已关闭')
  await reset()
  await toolsBox.locator('.switch').click()
  await softWait(() => window.__probe.calls.some((c) => c.fn === 'settingsSet'))
  const toolsSet = (await callsOf('settingsSet'))[0]
  check('settings:set 只写 features.surfaceTools=true',
    JSON.stringify(toolsSet?.args) === JSON.stringify([{ features: { surfaceTools: true } }]),
    JSON.stringify(toolsSet?.args))
  check('开关 UI 变为已开启', (await toolsBox.getAttribute('data-surface-tools-on')) === '1')
  check('功能开关不误发 surfaceSetEnabled', (await callsOf('surfaceSetEnabled')).length === 0)
  check('功能开关会发 reload_settings（loop 重建）',
    (await callsOf('send')).some((c) => c.args[0]?.type === 'reload_settings'), JSON.stringify(await callsOf('send')))

  // ================= T2 开关（传的是目标值）=================
  console.log('\nT2 开关：点 alpha（开着）→ IPC {id:alpha, enabled:false}，成功后列表刷新')
  await reset()
  await row('alpha').locator('.switch').click()
  await softWait(() => window.__probe.calls.some((c) => c.fn === 'surfaceSetEnabled'))
  await softWait(() => document.querySelector('[data-surface-id="alpha"]')?.getAttribute('data-enabled') === '0')
  const setCalls = await callsOf('surfaceSetEnabled')
  check('surfaceSetEnabled 只调了一次', setCalls.length === 1, JSON.stringify(setCalls))
  check('IPC 参数 = [alpha, false]（是目标值，不是传反）',
    JSON.stringify(setCalls[0]?.args) === JSON.stringify(['alpha', false]), JSON.stringify(setCalls[0]?.args))
  check('开关 UI 变成「已停用」', (await stateOf('alpha')) === '已停用', await stateOf('alpha'))
  const idxCalls = await callsOf('surfaceIndex')
  check('动作后重拉了 index（≥1 次）', idxCalls.length >= 1, String(idxCalls.length))
  check('动作后重拉了 audit', (await callsOf('surfaceAudit')).length >= 1)
  check('开关没有误发 surfaceRemove', (await callsOf('surfaceRemove')).length === 0)

  // ================= T3 卸载（确认框）=================
  console.log('\nT3 卸载：先确认；取消不发 IPC；确认才发 surfaceRemove(beta)')
  await reset()
  await row('beta').locator('.surface-uninstall').click()
  await softSelector('.confirm-card', 2000)
  // 这两条是「不确认就不许发」的核心判据：弹框有没有出现 + 点完按钮 IPC 还是空的。
  check('点卸载先弹确认框', (await page.locator('.confirm-card').count()) === 1)
  check('点卸载不发 IPC（必须先确认）', (await callsOf('surfaceRemove')).length === 0, JSON.stringify(await calls()))
  const modalText = (await page.locator('.confirm-card').count()) ? await textOf(page.locator('.confirm-card')) : ''
  check('确认框里点名了 beta', modalText.includes('beta'), modalText)

  await page.locator('.confirm-card .acts button').nth(0).click().catch(() => {})
  await page.waitForSelector('.confirm-card', { state: 'detached' }).catch(() => {})
  check('取消后确认框消失', (await page.locator('.confirm-card').count()) === 0)
  check('取消不发 surfaceRemove', (await callsOf('surfaceRemove')).length === 0, JSON.stringify(await calls()))
  check('取消后 beta 还在', (await row('beta').count()) === 1)

  await row('beta').locator('.surface-uninstall').click()
  await softSelector('.confirm-card', 2000)
  await page.locator('.confirm-card .acts button').nth(1).click().catch(() => {})
  await softWait(() => !document.querySelector('[data-surface-id="beta"]'))
  const rmCalls = await callsOf('surfaceRemove')
  check('确认后发 surfaceRemove(["beta"])',
    rmCalls.length === 1 && JSON.stringify(rmCalls[0]?.args) === JSON.stringify(['beta']), JSON.stringify(rmCalls))
  check('卸载后列表 2 条', (await rows().count()) === 2, String(await rows().count()))
  check('卸载后确认框已关', (await page.locator('.confirm-card').count()) === 0)

  // ================= T4 审计表 =================
  console.log('\nT4 审计：3 行表格（最新在上）/ 坏行不炸 / 空态')
  check('审计表 3 行', (await auditRows().count()) === 3, String(await auditRows().count()))
  const firstCells = await auditRows().nth(0).locator('td').allInnerTexts()
  check('第一行是**最新**那条（file_save，10:00:05）',
    firstCells[1]?.trim() === '调用' && firstCells[2]?.trim() === 'file_save', JSON.stringify(firstCells))
  check('时间列截成「日期 时分秒」', (firstCells[4] ?? '').trim() === '2026-11-12 10:00:05', firstCells[4])
  const lastCells = await auditRows().nth(2).locator('td').allInnerTexts()
  check('最后一行是最旧的 install', lastCells[1]?.trim() === '安装', JSON.stringify(lastCells))
  const okCells = await auditRows().evaluateAll((trs) => trs.map((tr) => tr.children[3]?.textContent?.trim()))
  check('ok 列：失败那行是 ✕，其余 ✓', JSON.stringify(okCells) === JSON.stringify(['✕', '✓', '✓']), JSON.stringify(okCells))

  // 坏行：null / 缺字段的对象夹在好行里 —— 页面必须照常渲染，pageerror 不许涨。
  const errsBefore = errs.length
  await page.evaluate(() => {
    ;window.__probe.setAudit([
      { id: 'alpha', action: 'command', cmd: 'git_snapshot', ok: true, ts: '2026-11-12T10:00:00.000000000Z' },
      null,
      { id: 'ghost', action: 'command', ok: false },
      { id: 'alpha', action: 'enable', ok: true, ts: '2026-11-12T10:01:00.000000000Z' },
    ])
  })
  await page.locator('.settings-sec-title', { hasText: '调用记录' }).locator('button').click()
  await page.waitForFunction(
    () => document.querySelectorAll('[data-surface-audit]').length >= 2,
    null, { timeout: 8000 },
  )
  check('坏行刷新后没有 pageerror', errs.length === errsBefore, errs.slice(errsBefore).join(' | '))
  // 4 条输入里：null 被丢掉（不是对象），剩下的 3 条照常渲染 —— 缺字段的那条降级成「—」，
  // 既不让整张表消失、也不在单元格里打出 undefined。
  check('坏行被跳过，好行照常渲染（3 行）', (await auditRows().count()) === 3, String(await auditRows().count()))
  const partial = await auditRows().nth(1).locator('td').allInnerTexts()
  check('缺字段的行降级成「—」而不是 undefined',
    partial[2]?.trim() === '—' && partial[4]?.trim() === '—', JSON.stringify(partial))

  await page.evaluate(() => window.__probe.setAudit([]))
  await page.locator('.settings-sec-title', { hasText: '调用记录' }).locator('button').click()
  await page.waitForFunction(
    () => !document.querySelector('[data-surface-audit]'),
    null, { timeout: 8000 },
  )
  check('空 audit 显示空态一句话', (await page.locator('.page-empty').last().innerText()).includes('还没有调用记录'))

  // ================= T5 失败：可读原因，不 alert =================
  console.log('\nT5 失败路径：ok:false 的 reason 贴在那一行上，不弹 alert')
  await reset()
  await page.evaluate(() => window.__probe.fail('surfaceSetEnabled', 'unknown_id'))
  await row('alpha').locator('.switch').click()
  await softSelector('[data-surface-row-error]', 2000)
  check('开关失败给出可读原因',
    (await textOf(row('alpha').locator('[data-surface-row-error]'))) === '面板清单里没有这个 id（可能已被卸载）。',
    await textOf(row('alpha').locator('[data-surface-row-error]')))
  check('开关失败没有推进 UI（仍是已停用）', (await stateOf('alpha')) === '已停用', await stateOf('alpha'))
  check('失败不弹 alert', (await page.evaluate(() => window.__probe.alerts())) === 0)

  await page.evaluate(() => { window.__probe.clearFail(); window.__probe.fail('surfaceRemove', 'error') })
  await row('alpha').locator('.surface-uninstall').click()
  await softSelector('.confirm-card', 2000)
  await page.locator('.confirm-card .acts button').nth(1).click().catch(() => {})
  await softWait(() => !!document.querySelector('[data-surface-id="alpha"] [data-surface-row-error]'))
  check('卸载失败给出兜底原因',
    (await textOf(row('alpha').locator('[data-surface-row-error]'))) === '操作失败，请重试。',
    await textOf(row('alpha').locator('[data-surface-row-error]')))
  check('卸载失败后 alpha 还在', (await row('alpha').count()) === 1)
  check('两次失败都没有 alert', (await page.evaluate(() => window.__probe.alerts())) === 0)

  // ================= T8 墓碑的「重新启用」（plugin: 先去写 index）=================
  console.log('\nT8 plugin: 墓碑点「重新启用」→ 先 surfaceSetEnabled(id,true)')
  await reset()
  // 先把 plugin:alpha 真的放进 health 的禁用集（key 见 src/surfaces/health.ts）：
  // 「重新启用」的第二半就是清它 —— 不预置的话这条断言会永远为真（假绿）。
  await page.evaluate(() => {
    localStorage.setItem('surface.disabled', JSON.stringify({
      'plugin:alpha': { count: 3, lastError: 'boom', at: Date.now(), disabled: true, auto: true },
    }))
  })
  await page.locator('.surface-tombstone-enable').click()
  await softWait(() => window.__probe.calls.some((c) => c.fn === 'surfaceSetEnabled'))
  const tombCalls = await callsOf('surfaceSetEnabled')
  check('墓碑调了 surfaceSetEnabled(["alpha", true])（先写 index，再清本地禁用集）',
    tombCalls.length === 1 && JSON.stringify(tombCalls[0].args) === JSON.stringify(['alpha', true]), JSON.stringify(tombCalls))
  check('墓碑没有报错（顺序对了，桥回了 ok）', (await page.locator('[data-tombstone-error]').count()) === 0)
  // 本地禁用集确实被清了（enableSurface 的副作用：localStorage['surface.disabled'] 里那条没了）
  check('本地禁用集已清空', (await page.evaluate(() => {
    const raw = localStorage.getItem('surface.disabled') ?? '{}'
    return raw.includes('plugin:alpha')
  })) === false)

  // ================= T9 清除数据（R17a）=================
  console.log('\nT9 清除数据：每行一个按钮 → 二次确认 → 前缀键消失 + toast；卸载路径同样清')
  // T5 故意把 surfaceRemove 设成了失败，这里必须复位 —— 否则下面那条「卸载也清数据」的
  // 断言会因为卸载根本没成功而红，看起来像清理没实现（假红）。
  await page.evaluate(() => window.__probe.clearFail())
  // 此时列表是 alpha + gamma（beta 已在 T3 卸载）。
  const clearBtns = await page.locator('[data-surface-clear]').count()
  check('每一行都有「清除数据」按钮', clearBtns === (await rows().count()) && clearBtns === 2,
    `buttons=${clearBtns} rows=${await rows().count()}`)
  check('按钮文案 = 清除数据',
    (await textOf(row('alpha').locator('[data-surface-clear]'))) === '清除数据',
    await textOf(row('alpha').locator('[data-surface-clear]')))

  await page.evaluate(() => {
    localStorage.setItem('alpha:progress:v1', '5/10')
    localStorage.setItem('alphax:keep', '1') // 前缀陷阱：'alphax:' 不属于 'alpha:'
    localStorage.setItem('gamma:note', 'x')
  })
  await reset()
  await row('alpha').locator('[data-surface-clear]').click()
  await softSelector('.confirm-card', 2000)
  check('点「清除数据」先弹确认框', (await page.locator('.confirm-card').count()) === 1)
  check('确认框点名了 alpha', (await textOf(page.locator('.confirm-card'))).includes('alpha'))
  // 不确认就什么都不许删（这条是「二次确认」的实质判据）。
  check('未确认时数据还在', (await page.evaluate(() => localStorage.getItem('alpha:progress:v1'))) === '5/10')
  check('未确认时不发任何 IPC（清数据是纯渲染侧动作）',
    (await callsOf('surfaceRemove')).length === 0 && (await callsOf('surfaceSetEnabled')).length === 0,
    JSON.stringify(await calls()))

  await page.locator('.confirm-card .acts button').nth(1).click().catch(() => {})
  await softWait(() => localStorage.getItem('alpha:progress:v1') === null)
  check('确认后 alpha 前缀的键消失', (await page.evaluate(() => localStorage.getItem('alpha:progress:v1'))) === null)
  check('前缀不同的键没被误删（alphax:keep）', (await page.evaluate(() => localStorage.getItem('alphax:keep'))) === '1')
  const toastAfterClear = await page.evaluate(() => window.__probe.toast())
  check('出现了 toast（store.showToast 真的被调）', !!toastAfterClear && typeof toastAfterClear.msg === 'string', JSON.stringify(toastAfterClear))
  check('toast 文案点名了 id 与键数',
    typeof toastAfterClear?.msg === 'string' && toastAfterClear.msg.includes('alpha') && toastAfterClear.msg.includes('个键'),
    JSON.stringify(toastAfterClear?.msg))
  check('清除数据不动 index（该行还在）', (await row('alpha').count()) === 1)
  check('清除数据的确认框已关', (await page.locator('.confirm-card').count()) === 0)

  // 卸载路径：**卸载成功就立刻清**，不等 surfaces_changed（那条事件可能已经没人听）。
  await page.evaluate(() => { localStorage.setItem('gamma:note', 'x') })
  check('基线：gamma 前缀的键在', (await page.evaluate(() => localStorage.getItem('gamma:note'))) === 'x')
  await reset()
  await row('gamma').locator('.surface-uninstall').click()
  await softSelector('.confirm-card', 2000)
  await page.locator('.confirm-card .acts button').nth(1).click().catch(() => {})
  await softWait(() => !document.querySelector('[data-surface-id="gamma"]'))
  check('卸载后 gamma 前缀的键也被清了（不依赖 sync）',
    (await page.evaluate(() => localStorage.getItem('gamma:note'))) === null)
  check('卸载后列表少一行', (await rows().count()) === 1, String(await rows().count()))

  // ================= T7 pageerror =================
  console.log('\nT7 全程 pageerror = 0')
  check('pageerror 0 条', errs.length === 0, errs.join(' | '))

  await browser.close()
} catch (e) {
  if (isPrecondition(e)) {
    console.error('\n[前置不满足] ' + e.message)
  } else {
    console.error('\n[探针异常] ' + (e instanceof Error ? e.stack : String(e)))
  }
  if (errs.length) console.error('[页内错误]\n  ' + errs.join('\n  '))
  if (consoleErrors.length) console.error('[console 错误（仅供参考）]\n  ' + consoleErrors.join('\n  '))
  code = exitCodeOf(e)
}

console.log('')
if (fails.length) {
  console.log(`❌ ${fails.length} 项未通过：`)
  for (const f of fails) console.log('   · ' + f)
  code = 1
} else if (!code) {
  console.log('✅ 面板插件页全绿')
}
cleanup()
process.exit(code)
