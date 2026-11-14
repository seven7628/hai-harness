#!/usr/bin/env node
// html 类文件「默认用浏览器展示」的链内探针（2026-11，用户报的真实回归）
//
// 用户口径原话：「html 类文件默认用浏览器展示，能直接看到效果」。实测回归：文件树右键「预览」
// 打开 .html → 面板里是**源码**（shiki 渲染的 `.fp-code` 代码面），看到的是一坨 `<h1>` 文本，
// 不是页面。根因不在「打开」（openFile → 内嵌浏览器那条没坏），在 R08b 新增的「预览」入口：
// openPreviewTab 对任何路径都建 `file{mode:'preview'}` 标签，而预览注册表里**没有 html 渲染器**
// → weight 10 的 text 兜底接走 → 源码。
//
// 本探针钉住四件事（每条都对应一个「实现错了就静默退化」的点）：
//   [1] **产品入口**：`openPreviewTab('/ws/demo.html')` 之后面板里**没有源码块**、右栏落在
//       web（内嵌浏览器）通道。这一条在修之前必须是红的（红=就是用户看到的那一幕）。
//   [2] **渲染器**：仍然落进 `mode='preview'` 的那些标签（旧版本持久化恢复出来的、直接
//       openTab 的调用方）由 html 渲染器接：iframe 里出现渲染后的 `<h1>标题</h1>`，
//       面板里**没有** `.fp-code`。
//   [3] **安全**：渲染器不开 allow-scripts / allow-same-origin —— 内容里的 `<script>` 与
//       onerror=/onload= 都**不执行**（在 iframe 自己的 window 上查，不查父页面：沙箱里即使
//       脚本跑了也只能写自己的 window，查父页面会假绿）。另有一条**反向对照**：把沙箱放开
//       一档后同一个文件确实会执行 —— 证明前面的「没执行」不是因为内容里没东西可执行。
//   [4] **默认通道未变**：`openFile('/ws/demo.html', undefined, 'source')` 仍开 web 标签、
//       不建 file 标签（T1 口径），以及「在内嵌浏览器中打开（可交互）」这条出口真的回到 web。
//
// 用法：node e2e/verify_html_file_preview.mjs
// 退出码：全通过 0；断言失败 1；前置不满足（探针页起不来/端口被占）2 —— 不许静默 skip。
import { spawn } from 'child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'
import { freePort, isPrecondition, waitProbePage } from './_serve.mjs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

// —— 探针页 ——
// 与 e2e/verify_preview_renderers.mjs 同一条口径：**真实** store + **真实** RightPanel
// + 真实 surface 的副作用注册，走 mock transport（不注入 window.desktop）。冻结一份
// 「面板长什么样」的复刻件证明不了接线，那正是 04-review F45 的形状。
const PROBE_MARKER = '<title>html file preview probe</title>'
const PROBE_DIR = path.join(__dirname, '_htmlfileprobe_run')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8">'
  + PROBE_MARKER
  + '</head><body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import RightPanel from '../../src/components/RightPanel'
// ⚠️ 与 src/main.tsx 一致的副作用注册（04-review F45）：少了它 getSurface() 全 undefined，
// 面板静默走 ?? 兜底 —— 兜底恰好也是 FilePreviewTab，看起来「差不多」。
import '../../src/surfaces/builtin'
import * as previewRegistry from '../../src/surfaces/preview/registry'
import { getSurface } from '../../src/surfaces/registry'
import { useAppStore } from '../../src/store/useAppStore'
import '../../src/index.css'

useAppStore.setState({ workspace: '/mock/workspace' })
;(window as unknown as { __store: unknown }).__store = useAppStore
;(window as unknown as { __preview: unknown }).__preview = { registry: previewRegistry }
;(window as unknown as { __surfaces: unknown }).__surfaces = {
  file: !!getSurface('file'),
  renderers: previewRegistry.listRenderers().map((r) => r.id),
}
void useAppStore.getState().start('/mock/workspace')

createRoot(document.getElementById('root')!).render(
  <div style={{ height: '100vh', display: 'flex', minWidth: 0 }}>
    <div style={{ flex: '1 1 auto', minWidth: 0 }} />
    <div id="rp" style={{ width: 860, height: '100vh', minWidth: 0, borderLeft: '1px solid var(--border)' }}>
      <RightPanel />
    </div>
  </div>,
)
`)
fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [APP] } } })
`)

const port = await freePort()
const PROBE_URL = `http://localhost:${port}/e2e/_htmlfileprobe_run/index.html`
const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(port), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)

const DEMO = '/ws/demo.html'
const INJECT = '/ws/inject.html'

// 当前右栏落点：激活标签的身份 + 面板里到底有没有源码块。**所有断言都读这张快照**，
// 不读组件内部状态 —— 用户看到的只有 DOM 与标签，判据也应该是这两个。
const snapshot = (page) => page.evaluate(() => {
  const st = window.__store.getState()
  const sets = Object.values(st.tabsByScope)
  const tabs = sets.flatMap((x) => (x?.tabs ?? []))
  const active = sets.flatMap((x) => (x?.tabs ?? []).filter((t) => t.id === x.activeId))[0]
  const body = document.querySelector('.right-body')
  return {
    activeKind: active?.kind ?? '',
    activeMode: active?.mode ?? '',
    activePath: active?.path ?? '',
    fileTabs: tabs.filter((t) => t.kind === 'file').length,
    // 源码块 = 用户报的「被当纯文本显示」那一幕的 DOM 指纹
    fpCode: document.querySelectorAll('.right-body .fp-code').length,
    renderer: body?.querySelector('[data-renderer]')?.getAttribute('data-renderer') ?? '',
    bodyText: (body?.textContent ?? '').trim().slice(0, 160),
  }
})

const settle = (page, ms = 400) => page.evaluate((n) => new Promise((r) => setTimeout(r, n)), ms)

// iframe 自己的 window / document —— 沙箱里脚本即使跑起来也只能写自己的 window，
// 查父页面会**假绿**（这正是「测试验的不是生产跑的那个」的变体）。
async function frameProbe(page, selector) {
  const handle = await page.locator(selector).first().elementHandle({ timeout: 20000 })
  if (!handle) throw new Error(`找不到 iframe：${selector}`)
  const frame = await handle.contentFrame()
  if (!frame) throw new Error(`iframe 没有内容框架：${selector}`)
  await frame.waitForSelector('h1', { timeout: 20000 })
  return frame.evaluate(() => ({
    heading: (document.querySelector('h1')?.textContent ?? '').trim(),
    title: document.title,
    flags: {
      pwned: typeof window.__pwned,
      onerror: typeof window.__onerror,
      onload: typeof window.__onload,
    },
  }))
}

async function main() {
  await waitProbePage(PROBE_URL, { marker: PROBE_MARKER, probe: 'e2e:html-file-preview', child: server })
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1280, height: 860 } })
  // 页面错误照常收集；唯独「Blocked script execution」要另放一处 —— 那是 Chromium 在替我们
  // 说「脚本被沙箱拦了」（见 [3]），是期望行为，不是错误。
  const errs = []
  const blocked = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  page.on('console', (m) => {
    const txt = m.text()
    if (/Blocked script execution/.test(txt)) { blocked.push(txt); return }
    if (m.type() === 'error' && !/favicon/.test(m.location()?.url || '')) errs.push(txt.slice(0, 200))
  })

  await page.goto(PROBE_URL, { waitUntil: 'domcontentloaded' })
  await page.waitForSelector('aside.right.collapsed', { timeout: 30000 })
  await page.waitForFunction(() => !!window.__store && !!window.__preview, { timeout: 20000 })

  // —— [0] 仪器自检 ——
  console.log('\n[0] 仪器自检：真实 surface provider 在场')
  const boot = await page.evaluate(() => window.__surfaces)
  assert(boot?.file === true, `builtin file provider 已注册（否则验的是 ?? 兜底；实得 ${JSON.stringify(boot)}）`)
  // 注册表清单只**打印**不在这里断言：html 缺失正是本轮要修的事，在这里拦住会让 [1] 那条
  // 真正对应「用户看到的一幕」的红失去出场机会（红要红在症状上，不是红在体检项上）。
  console.log(`  · 在册渲染器：${JSON.stringify(boot?.renderers)}`)

  // —— [1] 用户报的那一幕：点「预览」打开 .html ——
  console.log('\n[1] 产品入口：openPreviewTab(.html) 之后面板里不许是源码')
  await page.evaluate((p) => window.__store.getState().openPreviewTab(p), DEMO)
  await settle(page)
  const prev = await snapshot(page)
  // 断言顺序：先断言**用户看到的那一幕**（源码块），再断言通道 —— 红的时候最该被看见的是症状。
  assert(prev.fpCode === 0, `面板里没有 .fp-code 源码块（实得 ${prev.fpCode} 块；面板文本=${JSON.stringify(prev.bodyText)}）`)
  assert(prev.activeKind === 'web', `落在内嵌浏览器（web）通道（实得标签=${prev.activeKind}/${prev.activeMode}）`)

  // —— [2] 仍然落进 preview 标签的路径：由 html 渲染器接，且真的渲染出页面 ——
  console.log('\n[2] 渲染器：mode=preview 的 .html 由 html 渲染器接（旧的持久化标签/直接 openTab 的调用方）')
  // 生产动作（openTab）而不是 setState 伪造终态：这条标签是 store 自己建的。
  await page.evaluate((p) => window.__store.getState().openTab({ kind: 'file', path: p, mode: 'preview' }), DEMO)
  await settle(page, 600)
  const pre = await snapshot(page)
  // 先断言「谁接的」（快、红的时候一眼看出是 text 抢走的），再断言「渲染出了什么」。
  assert(pre.renderer === 'html', `preview 标签由 html 渲染器接（实得 ${pre.renderer || '<无渲染器>'}；面板文本=${JSON.stringify(pre.bodyText)}）`)
  await page.waitForSelector('.right-body [data-renderer="html"] .pv-html-frame', { timeout: 30000 })
  ok('html 渲染器接住了（data-renderer="html"）')
  const rendered = await frameProbe(page, '.pv-html-frame')
  assert(rendered.heading === '标题', `iframe 里是渲染后的 <h1>标题</h1>（实得 ${JSON.stringify(rendered.heading)}）`)
  const panel = await snapshot(page)
  assert(panel.fpCode === 0, `面板里没有 .fp-code 源码块（实得 ${panel.fpCode} 块）`)
  assert(panel.activeMode === 'preview' && /demo\.html$/.test(panel.activePath), `激活的是 demo.html 的 preview 标签（${JSON.stringify(panel.activePath)}/${panel.activeMode}）`)

  // —— [3] 安全：脚本与世界都关死 ——
  console.log('\n[3] 安全：<script> 与 onerror=/onload= 都不执行')
  const sandboxAttr = await page.evaluate(() => document.querySelector('.pv-html-frame')?.getAttribute('sandbox'))
  assert(sandboxAttr !== null && !/allow-scripts/.test(sandboxAttr), `sandbox 不含 allow-scripts（实得 ${JSON.stringify(sandboxAttr)}）`)
  assert(!/allow-same-origin/.test(sandboxAttr ?? ''), `sandbox 不含 allow-same-origin（实得 ${JSON.stringify(sandboxAttr)}）`)
  assert(rendered.flags.pwned === 'undefined' && rendered.title !== 'pwned',
    `demo.html 的 <script> 没执行（window.__pwned=${rendered.flags.pwned}、document.title=${JSON.stringify(rendered.title)}）`)
  const parentPwned = await page.evaluate(() => typeof window.__pwned)
  assert(parentPwned === 'undefined', '父页面（应用本体）也没有被写脏')

  await page.evaluate((p) => window.__store.getState().openTab({ kind: 'file', path: p, mode: 'preview' }), INJECT)
  await page.waitForSelector('.right-body [data-renderer="html"]', { timeout: 30000 })
  const injected = await frameProbe(page, '.pv-html-frame')
  assert(injected.heading === '注入样例', `注入样例也渲染出来了（实得 ${JSON.stringify(injected.heading)}）`)
  assert(injected.flags.onerror === 'undefined' && injected.flags.onload === 'undefined',
    `onerror= / onload= 都没执行（${JSON.stringify(injected.flags)}）`)
  // 只打印不断言：判据是上面的 window 标记（Chromium 的告警文案会随版本变，拿它当断言会
  // 在升级浏览器时红成噪音）。它在这里的价值是「第三方佐证」——浏览器自己说它拦了。
  console.log(`  · 佐证：Chromium 报了 ${blocked.length} 次「Blocked script execution in 'about:srcdoc'」`)
  assert(blocked.length > 0, 'Chromium 至少报告过一次脚本被沙箱拦下（与 window 标记互为佐证）')

  // 反向对照：同一个文件、只把沙箱放开一档 → 脚本必须真的跑起来。
  // 没有这一条，「没执行」可能只是「内容里根本没东西可执行」（04-review 的假绿形状）。
  // 两个坑都在这里踩过，写进注释免得下次重踩：
  //   ① 改 sandbox 属性**不会**重新解析已有文档（沙箱标记在导航时快照）→ 必须重设 srcdoc
  //      触发一次真导航，否则对照组永远等不到 __pwned（假红）；
  //   ② 对照组必须拿**含 <script> 的 demo.html**：inject.html 里只有 onerror=/onload=，
  //      查 __pwned 永远等不到（那是查错了对象，不是沙箱生效）。
  console.log('\n[3b] 反向对照：放开 allow-scripts 后同一份内容确实会执行 —— 证明上面的绿不是空的')
  await page.evaluate((p) => window.__store.getState().openTab({ kind: 'file', path: p, mode: 'preview' }), DEMO)
  await page.waitForSelector('.right-body [data-renderer="html"] .pv-html-frame', { timeout: 30000 })
  await frameProbe(page, '.pv-html-frame') // 等 h1 落地 = 这份 iframe 里确实是 demo.html
  await page.evaluate(() => {
    const el = document.querySelector('.pv-html-frame')
    const html = el?.getAttribute('srcdoc') ?? ''
    if (!el || !html) throw new Error('拿不到 iframe 的 srcdoc')
    el.setAttribute('sandbox', 'allow-scripts')
    el.srcdoc = html + '\n<!-- control: 只放开脚本，不放开同源 -->'
  })
  const t0 = Date.now()
  let woke = false
  while (Date.now() - t0 < 8000) {
    const f = await page.locator('.pv-html-frame').first().elementHandle()
    const fr = await f?.contentFrame()
    const flags = await fr?.evaluate(() => ({ pwned: typeof window.__pwned })).catch(() => undefined)
    if (flags?.pwned === 'number') { woke = true; break }
    await new Promise((r) => setTimeout(r, 120))
  }
  assert(woke, '放开沙箱后 window.__pwned 出现（对照组成立：前面的「没执行」是沙箱挡的，不是内容为空）')
  // 还原：沙箱收回 + 再导航一次（否则后面的断言/截图跑在那个放开过的文档上）
  await page.evaluate(() => {
    const el = document.querySelector('.pv-html-frame')
    if (!el) return
    el.setAttribute('sandbox', '')
    el.srcdoc = (el.getAttribute('srcdoc') ?? '') + '\n'
  })
  await settle(page, 300)

  // —— [4] 出口与默认通道 ——
  console.log('\n[4] 出口「在内嵌浏览器中打开（可交互）」+ 默认通道仍是 web 标签')
  await page.evaluate((p) => window.__store.getState().openTab({ kind: 'file', path: p, mode: 'preview' }), DEMO)
  await page.waitForSelector('.right-body [data-renderer="html"] .pv-html-open', { timeout: 30000 })
  await page.locator('.right-body [data-renderer="html"] .pv-html-open').click()
  await settle(page)
  const afterExit = await snapshot(page)
  assert(afterExit.activeKind === 'web',
    `出口把人送进内嵌浏览器（实得激活标签 kind=${afterExit.activeKind}）`)
  const before = await page.evaluate(() => {
    const st = window.__store.getState()
    return Object.values(st.tabsByScope).flatMap((x) => (x?.tabs ?? [])).filter((t) => t.kind === 'file').length
  })
  await page.evaluate((p) => window.__store.getState().openFile(p, undefined, 'source'), DEMO)
  await settle(page)
  const opened = await snapshot(page)
  assert(opened.fileTabs === before, `openFile(html, 'source') 不新建 file 标签（${before} → ${opened.fileTabs}）`)
  assert(opened.activeKind === 'web',
    `html 的「打开」仍落 web 标签（T1 口径未变；实得激活标签 kind=${opened.activeKind}）`)
  assert(opened.fpCode === 0, `默认通道里也没有源码块（实得 ${opened.fpCode}）`)

  // —— [5] 截断如实提示 ——
  console.log('\n[5] truncated 时如实提示（bridge 的 1 MiB 闸）')
  await page.evaluate(() => window.__store.getState().openTab({ kind: 'file', path: '/ws/huge.html', mode: 'preview' }))
  await page.waitForSelector('.right-body [data-renderer="html"] [data-html-truncated="1"]', { timeout: 30000 })
  ok('截断横幅出现（data-html-truncated="1"）')

  // —— [6] 变异（判据）：把 html 的 weight 压到 text 以下 → 又变回源码 ——
  // 没有这一条，「渲染出来了」可能只是巧合（比如哪天 text 被改成专杀 html）。权重是这条绿的
  // 唯一支点（要求：html 必须压过 text），所以必须能被变异打红。
  console.log('\n[6] 变异：html 的 weight 压到 5（低于 text 的 10）→ 面板又变回源码')
  await page.evaluate(() => {
    const R = window.__preview.registry
    const orig = R.listRenderers().find((r) => r.id === 'html')
    window.__htmlOrig = orig
    R.unregisterRenderer('html')
    R.registerRenderer({ ...orig, weight: 5 })
  })
  await page.evaluate((p) => window.__store.getState().openTab({ kind: 'file', path: p, mode: 'preview' }), DEMO)
  await page.waitForSelector('.right-body [data-renderer="text"] .fp-code', { timeout: 30000 })
  const mutated = await snapshot(page)
  assert(mutated.renderer === 'text' && mutated.fpCode === 1,
    `权重压下去后由 text 接管（实得 renderer=${mutated.renderer}、源码块=${mutated.fpCode}）`)
  // 还原：html 回到注册表原位（weight 70），后面的断言才验的是产品配置
  await page.evaluate(() => {
    const R = window.__preview.registry
    R.unregisterRenderer('html')
    R.registerRenderer(window.__htmlOrig)
  })
  const restored = await page.evaluate(() => window.__preview.registry.pickRenderer(window.__preview.registry.fileRefOf('/ws/demo.html'))?.id ?? '')
  assert(restored === 'html', `还原后 pickRenderer(.html) 仍是 html（实得 ${restored}）`)

  assert(errs.length === 0, `无页面错误（${errs.length ? errs.join(' | ') : 'clean'}）`)
  await browser.close()
  console.log(`\n✅ html 文件预览：${pass} 项全部通过`)
}

let code = 0
try {
  await main()
} catch (e) {
  if (!isPrecondition(e)) {
    console.error('\n❌ ' + (e?.message ?? e))
    code = 1
  } else {
    // 前置不满足 ≠ 断言失败（00 §4.6）：退出码 2
    console.error(`\n⛔ 前置不满足（退出码 2，不是测试失败）：${e.message}`)
    code = 2
  }
} finally {
  cleanup()
}
process.exit(code)
