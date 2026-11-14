#!/usr/bin/env node
// 右侧栏标签条**视觉验证**（阶段 1）：把 RightPanel 单独挂到一个探针页里，开若干标签后截图。
//
// 为什么需要它：阶段 1 把「5 个固定 tab 按钮」换成标签实例列表，改的是**布局与观感**——
// Mocha/断言测不出「标签被挤扁」「关闭钮盖住文字」「菜单跑出面板」。本脚本产出的 PNG 是
// 人工复核的载体（也可作为回归对照：同一场景重跑，图应该只有预期差异）。
//
// 用法：cd desktop/app && node e2e/shots_right_tabs.mjs
// 产出：e2e/_rtabs_shots/*.png
import { spawn } from 'child_process'
import fs from 'node:fs'
import net from 'node:net'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = 5207
const OUT = path.join(__dirname, '_rtabs_shots')

async function waitPort(port, timeoutMs = 30000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const up = await new Promise((resolve) => {
      const s = net.connect({ port, host: 'localhost' })
      const timer = setTimeout(() => { s.destroy(); resolve(false) }, 500)
      s.on('connect', () => { clearTimeout(timer); s.destroy(); resolve(true) })
      s.on('error', () => { clearTimeout(timer); s.destroy(); resolve(false) })
    })
    if (up) return
    if (Date.now() > deadline) throw new Error('vite 未就绪')
    await new Promise((r) => setTimeout(r, 250))
  }
}

const PROBE_DIR = path.join(__dirname, '_rtabsprobe_run')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8"><title>rtabs probe</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
// 与 src/main.tsx 同序：**先**副作用注册内置 provider，再挂组件。
// 缺这一行探针就是个「provider 全都没注册」的假世界：RightPanel 里每处
// getSurface(k)?.render/title/icon 都会走 ?? 兜底分支 —— 2026-11（R06b）实测踩过：
// web 标签的图标因此渲染成兜底值，截图证明的是兜底而不是委派。
import '../../src/surfaces/builtin'
import RightPanel from '../../src/components/RightPanel'
import { useAppStore } from '../../src/store/useAppStore'
import '../../src/index.css'

useAppStore.setState({ workspace: '/w' })
;(window as unknown as { __store: unknown }).__store = useAppStore
void useAppStore.getState().start('/w')

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

try {
  fs.rmSync(OUT, { recursive: true, force: true })
  fs.mkdirSync(OUT, { recursive: true })
  await waitPort(PORT)
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1100, height: 860 }, deviceScaleFactor: 2 })
  page.on('pageerror', (e) => console.error('pageerror:', e.message))
  await page.goto(`http://localhost:${PORT}/e2e/_rtabsprobe_run/index.html`, { waitUntil: 'domcontentloaded' })
  await page.waitForFunction(() => !!window.__store, null, { timeout: 20000 })
  await page.waitForTimeout(600)

  // 场景 1：默认（只开了一个 链路 标签）
  await page.evaluate(() => {
    const st = () => window.__store.getState()
    window.__store.setState({
      activeSessionId: 'S1',
      workspace: '/w',
      projects: [{ path: '/w', sessions: [{ id: 'S1', title: 'A' }] }],
      rightExpanded: true,
      tabsByScope: {},
    })
    st().openTab({ kind: 'activity' })
  })
  await page.waitForTimeout(300)
  await page.locator('.right').screenshot({ path: path.join(OUT, '1-default.png') })

  // 场景 2：开满 4 类标签（含文件标签显示文件名）。
  // 2026-10：'agent'（任务）tab 已退役 —— 子智能体住左栏会话树，不再占右栏入口。
  await page.evaluate(() => {
    const st = () => window.__store.getState()
    st().openTab({ kind: 'git' })
    st().openFile('desktop/app/src/store/useAppStore.ts')
    st().openTab({ kind: 'web' })
    st().openTab({ kind: 'git' }) // 激活「变更」，让多标签状态可见
  })
  await page.waitForTimeout(300)
  await page.locator('.right').screenshot({ path: path.join(OUT, '2-all-tabs.png') })

  // 场景 3：「+」菜单展开
  await page.locator('.rtabs-add').click()
  await page.waitForTimeout(250)
  await page.locator('.right').screenshot({ path: path.join(OUT, '3-add-menu.png') })
  await page.keyboard.press('Escape')
  await page.locator('.rtabs-backdrop').click({ force: true })
  await page.waitForTimeout(200)

  // 场景 4：关闭两个标签（「⌄」菜单里应有可重开的项）
  await page.evaluate(() => {
    const st = () => window.__store.getState()
    const set = st().tabsByScope['S1']
    for (const t of set.tabs.filter((x) => x.kind === 'web')) st().closeTab(t.id)
  })
  await page.waitForTimeout(300)
  await page.locator('.rtabs-more').click()
  await page.waitForTimeout(250)
  await page.locator('.right').screenshot({ path: path.join(OUT, '4-reopen-menu.png') })
  await page.locator('.rtabs-backdrop').click({ force: true })
  await page.waitForTimeout(200)

  // 场景 5：关光所有标签 → 空态
  await page.evaluate(() => {
    const st = () => window.__store.getState()
    for (const t of [...st().tabsByScope['S1'].tabs]) st().closeTab(t.id)
  })
  await page.waitForTimeout(300)
  await page.locator('.right').screenshot({ path: path.join(OUT, '5-empty.png') })

  // 场景 6：折叠态 rail（2026-10 起 4 类固定入口：链路/变更/文件/网页）
  await page.evaluate(() => window.__store.setState({ rightExpanded: false }))
  await page.waitForTimeout(300)
  await page.locator('.right').screenshot({ path: path.join(OUT, '6-collapsed-rail.png') })

  // 场景 7/8：文件面板的两种形态（T2 的视觉对照）——
  // 整文件（「最终文件」）应**独占整宽**、不出现左侧变更列表；切「本次修改」才回到两栏。
  // 光看 DOM 断言不够：这里要肉眼确认文件没被挤在半栏里（长行不被窄列截断）。
  await page.evaluate(async () => {
    const st = () => window.__store.getState()
    window.__store.setState({ rightExpanded: true, gitFiles: [], fileFocus: undefined, filePreviews: {} })
    st().openFile('auth.go', undefined, 'source')
    await new Promise((r) => setTimeout(r, 400))
    // 活动列表**必须在 openFile 之后**注入：openFile 会经 setView 用会话视图回写顶层镜像，
    // 先注入的 blocks 会被冲掉（本探针实测踩过：DIAG blocks=[] → 只能看到 standalone 形态）。
    window.__store.setState({
      blocks: [{ kind: 'tool', id: 1, toolId: 't1', name: 'read_file', args: JSON.stringify({ path: 'auth.go' }), result: 'ok', status: 'done', startedAt: Date.now() }],
    })
    await new Promise((r) => setTimeout(r, 400))
  })
  await page.waitForTimeout(700)
  await page.locator('.right').screenshot({ path: path.join(OUT, '7-file-whole.png') })
  await page.evaluate(() => {
    const b = [...document.querySelectorAll('.file-detail-tabs button')].find((x) => (x.textContent || '').includes('本次修改'))
    b?.click()
  })
  await page.waitForTimeout(600)
  await page.locator('.right').screenshot({ path: path.join(OUT, '8-file-changes-two-col.png') })

  // 场景 9：折叠态 rail 的**未读角标**（阶段 4 的可见性兜底）——
  // 「AI/Agent 后台产出不抢视线」之后，用户唯一能知道「有东西在等我」的地方就是这条 rail，
  // 角标位置/配色错了就等于看不见（断言只能证明元素在，证明不了它没被图标盖住）。
  await page.evaluate(() => {
    const st = () => window.__store.getState()
    window.__store.setState({ rightExpanded: false, unreadViews: ['t9'] })
    window.__store.setState({
      tabsByScope: { S1: { tabs: [{ id: 'ua', kind: 'activity' }, { id: 'ub', kind: 'web', viewId: 't9' }], activeId: 'ua', closed: [] } },
    })
  })
  await page.waitForTimeout(400)
  await page.locator('.right').screenshot({ path: path.join(OUT, '9-collapsed-unread-badge.png') })

  await browser.close()
  console.log(`\n截图已写入 ${path.relative(APP, OUT)}/：`)
  for (const f of fs.readdirSync(OUT).sort()) console.log('  ·', f)
  cleanup()
  process.exit(0) // vite 子进程持有管道 → 不显式退出会挂住（首次实测被外层超时杀掉）
} catch (e) {
  console.error('截图失败:', e.message)
  cleanup()
  process.exit(1)
}
