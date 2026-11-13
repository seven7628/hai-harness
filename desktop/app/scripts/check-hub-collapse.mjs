#!/usr/bin/env node
// 长正文折叠 + 完整内容弹窗的浏览器验收（src/hub/CollapsibleMarkdown.tsx）。
//
// 与 tsc/test:p4 的分工：那些是纯逻辑与源码接线；这个跑 DOM —— 真 CSS、真 React、
// 真布局、真 ResizeObserver、真 iframe（visual-ui 的 html 预览）。
//
// 用法（默认自起 dev server；5226 端口，不占 5173/5180）：
//   node scripts/check-hub-collapse.mjs
//   node scripts/check-hub-collapse.mjs --url http://localhost:5180/   # 复用已起的
//   node scripts/check-hub-collapse.mjs --no-spawn                      # 不自己起 server
//
// 判据（任一失败 → exit 1）：
//   0. 静态：内层 <Markdown> 必须透传 streaming（弹窗里的不传）、阈值比较不许带 +1 容差；
//   1. 阈值边界：内层 419 不折 / 421、422 折；折叠态 max-height 恰好 420；
//   2. RO 双向：折叠后把内层削短 → 自动取消折叠（不需要重新挂载）；
//   3. streaming=true 不折、无按钮，且 html 围栏走「正在生成」骨架（证明 streaming 真的
//      传到了 Markdown —— 这条是 8af7753 被吞掉 streaming 的回归锁）；
//   4. visual-ui：折叠态与弹窗里都能读到 iframe（探针上报了高度，不是白板）；
//   5. 弹窗：role/aria-modal/标题、打开聚焦关闭按钮、锁背景滚动、Esc/遮罩关闭、关闭回焦；
//   6. 空正文不渲染壳；console error / pageerror 0。
import { chromium } from 'playwright'
import { spawn } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'
import { openHubHarness, APP_DIR } from './hub-harness.mjs'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const PORT = Number(arg('port', process.env.HUB_COLLAPSE_PORT || '5226'))
const URL_ARG = arg('url', '')
const BASE = URL_ARG || `http://localhost:${PORT}/`
const NO_SPAWN = argv.includes('--no-spawn')

const failed = []
const check = (cond, msg) => {
  if (!cond) failed.push(msg)
  console.log(`  ${cond ? '✅' : '❌'} ${msg}`)
}

// —— 0) 静态接线（不需要浏览器）：先跑，源码被改回去时不必等浏览器 ——
{
  const src = readFileSync(join(APP_DIR, 'src', 'hub', 'CollapsibleMarkdown.tsx'), 'utf8')
  const code = src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '')
  check(code.includes('<Markdown text={text} streaming={streaming}'), '内层 <Markdown> 透传 streaming')
  check(code.split('streaming={streaming}').length - 1 === 1, '只有折叠壳那一处传 streaming（弹窗里的不传）')
  check(code.includes('> COLLAPSE_HEIGHT') && !code.includes('COLLAPSE_HEIGHT + 1'), '阈值比较是 > COLLAPSE_HEIGHT（无 +1 容差）')
}

// —— dev server（与 check-hub-inline-todos.mjs 同一套：自起 / 带走）——
async function preflight(url) {
  try {
    const res = await fetch(url, { signal: AbortSignal.timeout(4000) })
    if (!res.ok) return `HTTP ${res.status}`
    const html = await res.text()
    if (!html.includes('id="root"')) return '响应里没有 #root（不是 vite dev 页面？）'
    return null
  } catch (e) {
    return e?.cause?.code || e?.message || String(e)
  }
}

let server = null
async function ensureServer() {
  if (!(await preflight(BASE))) return
  if (NO_SPAWN) {
    console.error(`❌ dev server 不可用：${BASE}`)
    console.error(`   先起：cd desktop/app && npx vite --port ${PORT} --strictPort`)
    process.exit(2)
  }
  // 直接跑 node_modules/.bin/vite：经 npx 会多一层 npm exec 进程，SIGTERM 只杀到壳。
  server = spawn(join(APP_DIR, 'node_modules', '.bin', 'vite'), ['--port', String(PORT), '--strictPort'], {
    cwd: APP_DIR, stdio: 'ignore',
  })
  for (let i = 0; i < 120; i++) {
    await new Promise((r) => setTimeout(r, 500))
    if (!(await preflight(BASE))) return
  }
  console.error(`❌ 自起 dev server 超时（${BASE}）`)
  process.exit(2)
}

await ensureServer()
const stopServer = () => { try { server?.kill('SIGTERM') } catch { /* 已退出 */ } }
process.on('exit', stopServer)
process.on('SIGINT', () => { stopServer(); process.exit(130) })

// —— 夹具：五条消息（长 / 长-用户气泡 / 短 / 流式 / 空）+ 一条边界用 ——
// openHubHarness 的 <link> 列表固定是 index/demo/hub 三份 CSS；组件自己的 CSS 由入口
// 同步插一个 <style>（读的就是磁盘上那份文件，不是副本）。不用 <link>：page.setContent
// 里 <link> 一进文档就出现在 document.styleSheets，但 load 事件要等真正的抓取 —— 等它
// 就把「能不能渲染」绑在了 dev server 的响应上，夹具会变成偶发超时。
const CSS_TEXT = readFileSync(join(APP_DIR, 'src', 'hub', 'CollapsibleMarkdown.css'), 'utf8')
const FENCE = '`'.repeat(3)
const ENTRY = [
  "import React from 'react'",
  "import { createRoot } from 'react-dom/client'",
  "import CollapsibleMarkdown from './CollapsibleMarkdown.tsx'",
  '',
  `const CSS_TEXT = ${JSON.stringify(CSS_TEXT)}`,
  "const P = '这是一段用于验证折叠的长正文。它必须真的超过阈值高度，而不是按字符数猜出来的。'",
  `const HTML_FENCE = ${JSON.stringify(`${FENCE}html\n<div style="font:14px system-ui;padding:14px">visual-ui 预览：<b>你好</b></div>\n${FENCE}`)}`,
  "const LONG = ['# 三家定价页勘察', P, '## 结论', '- 条目一\\n- 条目二\\n- 条目三', HTML_FENCE, P, P, P, P, P, P, P, P, P, P, '尾巴：这一行只应该在完整内容里看到。'].join('\\n\\n')",
  "const SHORT = '短消息：不该折叠。'",
  "const MID = ['# 边界用例', P, P].join('\\n\\n')",
  '',
  "const row = (tag, cls, text, props) => React.createElement('div', { className: 'msg-row ' + cls, 'data-test': tag },",
  "  React.createElement('div', { className: 'msg-main' },",
  "    React.createElement('div', { className: cls === 'user' ? 'user-bubble' : 'agent-text' },",
  "      React.createElement(CollapsibleMarkdown, Object.assign({ text }, props || {})))))",
  '',
  'function App() {',
  "  return React.createElement('div', { className: 'messages' }, React.createElement('div', { className: 'messages-inner' },",
  "    row('long', 'agent', LONG, { ariaLabel: 'Alice' }),",
  "    row('long-user', 'user', LONG, { ariaLabel: '你' }),",
  "    row('short', 'agent', SHORT),",
  "    row('stream', 'agent', LONG, { streaming: true, ariaLabel: 'Alice' }),",
  "    row('empty', 'agent', '   '),",
  "    row('boundary', 'agent', MID),",
  '  ))',
  '}',
  '',
  "const style = document.createElement('style')",
  'style.textContent = CSS_TEXT',
  'document.head.appendChild(style)',
  "createRoot(document.getElementById('hub-root')).render(React.createElement(App))",
  'setTimeout(() => { window.__harnessReady = true }, 50)',
].join('\n')

const browser = await chromium.launch()
const page = await openHubHarness(browser, { contents: ENTRY, baseURL: BASE, width: 1280, height: 900 })
await page.waitForTimeout(700)

const q = (sel) => page.locator(sel)
const count = (sel) => q(sel).count()
const clipBox = async (sel) => (await q(sel).first().boundingBox()) ?? { height: -1 }
const frameHeight = (sel) => page.evaluate((s) => {
  const f = document.querySelector(s)
  return f ? Number((f.style.height || '0').replace('px', '')) : -1
}, sel)
const frameDoc = (sel) => page.evaluate((s) => document.querySelector(s)?.getAttribute('srcdoc') ?? '', sel)
const forceInner = async (px) => {
  await page.evaluate((h) => {
    const el = document.querySelector('[data-test=boundary] .cm-inner')
    if (el) el.style.height = `${h}px`
  }, px)
  // ResizeObserver 是异步投递的：给它一帧多一点。
  await page.waitForTimeout(150)
}

// —— 1) 阈值边界（419 不折 / 421、422 折 / 削短自动取消）——
await forceInner(419)
check(await count('[data-test=boundary] .cm-clip.is-clipped') === 0, '内层 419px → 不折叠')
check(await count('[data-test=boundary] .cm-more-btn') === 0, '内层 419px → 无按钮')

await forceInner(421)
check(await count('[data-test=boundary] .cm-clip.is-clipped') === 1, '内层 421px → 折叠')
check(await count('[data-test=boundary] .cm-more-btn') === 1, '内层 421px → 出现「查看完整详情」')
{
  const b = await clipBox('[data-test=boundary] .cm-clip')
  check(Math.abs(b.height - 420) < 1.5, `折叠态 max-height = 420（实测 ${b.height.toFixed(1)}）`)
  const maxH = await page.evaluate(() => getComputedStyle(document.querySelector('[data-test=boundary] .cm-clip')).maxHeight)
  check(maxH === '420px', `折叠态 computed max-height = ${maxH}`)
  // mask 只可能来自 CollapsibleMarkdown.css：这条顺带证明组件样式表真的生效了
  // （max-height 是内联的，单看它证明不了 CSS 加载成功）。
  const mask = await page.evaluate(() => getComputedStyle(document.querySelector('[data-test=boundary] .cm-clip')).maskImage)
  check(mask && mask !== 'none', `折叠态底部渐隐生效（mask-image=${String(mask).slice(0, 40)}…）`)
}

await forceInner(422)
check(await count('[data-test=boundary] .cm-clip.is-clipped') === 1, '内层 422px → 仍折叠')

await forceInner(300)
check(await count('[data-test=boundary] .cm-clip.is-clipped') === 0, '削短内层到 300px → RO 自动取消折叠（无需重挂载）')
check(await count('[data-test=boundary] .cm-more-btn') === 0, '取消折叠后按钮消失')

// —— 2) 真实内容：长文折叠、短/流式/空不折 ——
check(await count('[data-test=long] .cm-clip.is-clipped') === 1, '长正文（agent 无气泡）折叠')
check(await count('[data-test=long-user] .cm-clip.is-clipped') === 1, '长正文（user 暖色气泡）折叠')
check(await count('[data-test=short] .cm-more-btn') === 0, '短正文不折叠')
check(await count('[data-test=stream] .cm-clip.is-clipped') === 0, 'streaming=true 不折叠')
check(await count('[data-test=stream] .cm-more-btn') === 0, 'streaming=true 无按钮')
check(await count('[data-test=empty] .cm') === 0, '空正文不渲染壳')
check((await q('[data-test=long] .cm-more-btn').innerText()) === '查看完整详情', '按钮文案 = 查看完整详情')
check((await q('[data-test=long] .cm-more-btn').getAttribute('aria-label')) === '查看完整详情：Alice', '按钮 aria-label 带作者名')

// streaming 真的传到了 Markdown：html 围栏在流式态只出骨架，不挂 iframe。
check(await count('[data-test=stream] .htmlblock-streaming') === 1, 'streaming 的 html 围栏走「正在生成」骨架')
check(await count('[data-test=stream] iframe') === 0, 'streaming 的 html 围栏不挂 iframe（半截 HTML 不执行）')

// —— 3) visual-ui：折叠态可读 ——
await page.waitForTimeout(900) // 等 iframe 内探针上报高度
check(await count('[data-test=long] .htmlblock') === 1, '折叠态走 HtmlPreviewBlock（.htmlblock）')
check(await count('[data-test=long] iframe.htmlblock-frame') === 1, '折叠态有 html 预览 iframe')
{
  const h = await frameHeight('[data-test=long] iframe.htmlblock-frame')
  check(h > 40, `折叠态 iframe 高度 = ${h}（探针已上报 → 真的渲染）`)
  const doc = await frameDoc('[data-test=long] iframe.htmlblock-frame')
  check(doc.includes('visual-ui 预览'), '折叠态 iframe 文档里就是这段 HTML')
}

await page.screenshot({ path: '/tmp/hub-collapse-collapsed.png', fullPage: true })

// —— 4) 弹窗 ——
const trigger = q('[data-test=long] .cm-more-btn')
await trigger.click()
await page.waitForSelector('.cm-dialog')
check(await q('.cm-dialog').getAttribute('role') === 'dialog', 'role=dialog')
check(await q('.cm-dialog').getAttribute('aria-modal') === 'true', 'aria-modal=true')
check((await q('.cm-dialog-title').innerText()) === '完整内容', '标题 = 完整内容')
check(await page.evaluate(() => document.documentElement.style.overflow) === 'hidden', '打开时锁背景滚动')
{
  const focused = await page.evaluate(() => document.activeElement?.className || '')
  check(focused.includes('cm-dialog-close'), `打开即聚焦关闭按钮（${focused}）`)
}
check((await q('.cm-dialog .cm-dialog-body').innerText()).includes('尾巴：这一行只应该在完整内容里看到。'), '弹窗里是被折叠掉的完整正文')
await page.waitForTimeout(900)
check(await count('.cm-dialog .htmlblock') === 1, '弹窗里也走 HtmlPreviewBlock')
{
  const h = await frameHeight('.cm-dialog iframe.htmlblock-frame')
  check(h > 40, `弹窗内 iframe 高度 = ${h}（探针已上报 → 真的渲染）`)
  const doc = await frameDoc('.cm-dialog iframe.htmlblock-frame')
  check(doc.includes('visual-ui 预览'), '弹窗内 iframe 文档里就是这段 HTML')
}
await page.screenshot({ path: '/tmp/hub-collapse-dialog.png' })

// —— 5) Esc 关闭 + 回焦 + 解锁 ——
await page.keyboard.press('Escape')
await page.waitForSelector('.cm-dialog', { state: 'detached' })
check(await count('.cm-dialog') === 0, 'Esc 关闭')
check(await page.evaluate(() => document.documentElement.style.overflow) !== 'hidden', '关闭后解锁背景滚动')
check(await page.evaluate(() => document.activeElement?.className?.includes?.('cm-more-btn')), '关闭后焦点回到触发按钮')

// —— 6) 遮罩关闭 ——
await trigger.click()
await page.waitForSelector('.cm-dialog')
await page.mouse.click(20, 20)
await page.waitForSelector('.cm-dialog', { state: 'detached' })
check(await count('.cm-dialog') === 0, '点遮罩关闭')

// —— 7) console / pageerror ——
{
  const errs = await page.evaluate(() => window.__hubErrors || [])
  check(errs.length === 0, `console error / pageerror = ${errs.length}${errs.length ? ' → ' + errs.slice(0, 3).join(' | ') : ''}`)
}

await browser.close()
if (failed.length) {
  console.error(`\n❌ ${failed.length} 项失败：`)
  for (const f of failed) console.error(`   · ${f}`)
  process.exit(1)
}
console.log('\nHUB_COLLAPSE_CHECK_PASS')
