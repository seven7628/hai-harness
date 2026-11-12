#!/usr/bin/env node
// 代码块「无壳 + 悬停浮出」+ 复制反馈 回归（2026-09-13）
//
// 起因：用户要求把 ```html 预览块那套「默认不显边框、悬停才显示头部」推广到 **所有**
// 代码块（```text / ```go / ```bash …），同时**保持代码块本身的渲染**（底色、等宽字体、
// shiki 高亮、横纵滚动）；并反馈「复制按钮点了没反应」。
//
// 本脚本锁住的行为（每条都对应一个「实现错了就会静默退化」的点）：
//   ① 默认态：无可见边框、头部 opacity 0、**按钮也不可点**（否则看不见却点得到）
//   ② 悬停：边框现身 + 头部浮出 + 按钮可点（真实鼠标移动驱动）
//   ③ 移出：回到无壳态（不是出现后常驻）
//   ④ **渲染不变**：底色 = --code-bg、等宽字体、shiki 高亮产物仍在、行高不变
//   ⑤ 悬停不推挤内容（头部是浮层；代码块首行位置逐像素不变）
//   ⑥ 头部不盖住首行代码（右上角药丸而非通栏 —— 短块如单行 `ls -la` 必须可读）
//   ⑦ 键盘可达：Tab 到复制按钮 → 头部与边框一并现身（默认隐藏不牺牲键盘）
//   ⑧ 复制按钮：点后有可见反馈（成功「已复制」/ 失败「复制失败」），**不再静默**
//   ⑨ text/go/无语言 三种围栏都适用（不是只改了某一种）
//
// 用法：cd desktop/app && node e2e/verify_codeblock_shell.mjs
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'
import fs from 'fs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = Number(process.env.PORT || 4191)

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

async function waitPort(port, timeoutMs = 25000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const up = await new Promise((resolve) => {
      const s = net.connect({ port, host: 'localhost' })
      const timer = setTimeout(() => { s.destroy(); resolve(false) }, 500)
      s.on('connect', () => { clearTimeout(timer); s.destroy(); resolve(true) })
      s.on('error', () => { clearTimeout(timer); resolve(false) })
    })
    if (up) return
    if (Date.now() > deadline) throw new Error('vite 未就绪')
    await new Promise((r) => setTimeout(r, 250))
  }
}

// 临时探针页：真实 Markdown 组件 + 真实 index.css
const PROBE_DIR = path.join(__dirname, '_cbshellprobe')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8"><title>codeblock shell probe</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import { Markdown } from '../../src/components/markdown'
import '../../src/index.css'
const CASES = (window as unknown as { __CASES: Record<string, string> }).__CASES
createRoot(document.getElementById('root')!).render(
  <div className="narrative" style={{ height: '100vh', overflowY: 'auto', width: 620, margin: '0 auto' }}>
    {Object.entries(CASES).map(([k, v]) => (
      <div key={k} data-case={k} className="md-text"><Markdown text={v} /></div>
    ))}
    <div style={{ height: 900 }} />
  </div>,
)`)
fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [APP] } } })
`)

const PROBE_URL = `http://localhost:${PORT}/e2e/_cbshellprobe/index.html`
const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)

async function main() {
  await waitPort(PORT)
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 900, height: 900 } })
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  page.on('console', (m) => {
    const t = m.text()
    if (m.type() === 'error' && !/ERR_NAME_NOT_RESOLVED|Failed to load resource/.test(t)) errs.push(t.slice(0, 200))
  })

  await page.addInitScript((c) => { window.__CASES = c }, {
    text: '```text\nls -la\n```',
    go: '```go\npackage main\n\nfunc main() {}\n```',
    plain: '```\nplain fence\n```',
    bash: '```bash\ngrep -rn "foo" .\n```',
  })
  await page.goto(PROBE_URL, { waitUntil: 'domcontentloaded' })
  await page.waitForSelector('[data-case="go"] .codeblock .shiki', { timeout: 25000 })
  await page.waitForTimeout(1500) // 等 shiki 高亮全部完成

  const snap = (sel) => page.evaluate((s) => {
    const blk = document.querySelector(`${s} .codeblock`)
    const hd = blk.querySelector('.cb-hd')
    const btn = hd.querySelector('.cb-copy')
    const body = blk.querySelector('.cb-body')
    const cs = getComputedStyle(blk), hcs = getComputedStyle(hd), bcs = getComputedStyle(body)
    const bb = blk.getBoundingClientRect(), hb = hd.getBoundingClientRect(), bodyb = body.getBoundingClientRect()
    const borderVisible = ['Top', 'Right', 'Bottom', 'Left']
      .some((k) => cs[`border${k}Color`] !== 'rgba(0, 0, 0, 0)' && parseFloat(cs[`border${k}Width`]) > 0)
    return {
      borderVisible, borderColor: cs.borderTopColor,
      hdOpacity: Number(hcs.opacity), hdPE: hcs.pointerEvents, hdPosition: hcs.position,
      btnPE: getComputedStyle(btn).pointerEvents,
      btnText: btn.textContent.trim(),
      // 渲染保真：等宽字体与行高落在**承载文本的节点**（.shiki 高亮产物 / .cb-plain
      // 未高亮兜底），而不是 .cb-body 容器 —— 断言要对准真正渲染文字的那个元素，
      // 否则测的是继承值（容器上是 UI 字体），会得到假失败。
      bg: cs.backgroundColor, bodyBg: bcs.backgroundColor,
      font: (() => { const n = blk.querySelector('.shiki') || blk.querySelector('.cb-plain'); return n ? getComputedStyle(n).fontFamily : null })(),
      lineHeight: (() => { const n = blk.querySelector('.shiki') || blk.querySelector('.cb-plain'); return n ? getComputedStyle(n).lineHeight : null })(),
      hasShiki: !!blk.querySelector('.shiki'),
      // 几何
      blkTop: bb.top, blkH: bb.height, bodyTop: bodyb.top,
      hdCoversFirstLine: hb.bottom > bodyb.top + 4 && hb.left < bodyb.left + 12, // 头部是否压住首行**左端**
    }
  }, sel)

  const moveTo = async (x, y) => { await page.mouse.move(x, y); await page.waitForTimeout(330) }
  const boxOf = async (sel) => page.locator(`${sel} .codeblock`).boundingBox()
  const outsideOf = async (sel) => {
    const b = await boxOf(sel)
    return { x: b.x + b.width / 2, y: b.y - 14 }
  }

  // ---- ① 默认态：无壳（四种围栏一致）----
  for (const c of ['text', 'go', 'plain', 'bash']) {
    const o = await outsideOf(`[data-case="${c}"]`)
    await moveTo(o.x, o.y)
    const s = await snap(`[data-case="${c}"]`)
    assert(!s.borderVisible, `[${c}] 默认无可见边框（实际 ${s.borderColor}）`)
    assert(s.hdOpacity === 0, `[${c}] 默认头部不可见（opacity=${s.hdOpacity}）`)
    assert(s.btnPE === 'none', `[${c}] 默认复制按钮不可点（pointer-events=${s.btnPE}）`)
    assert(s.hdPosition === 'absolute', `[${c}] 头部是浮层（position=${s.hdPosition}）`)
  }

  // ---- ② 悬停：浮出 ----
  const gb = await boxOf('[data-case="go"]')
  await moveTo(gb.x + gb.width / 2, gb.y + gb.height / 2)
  const hovered = await snap('[data-case="go"]')
  assert(hovered.hdOpacity === 1, `悬停时头部浮出（opacity=${hovered.hdOpacity}）`)
  assert(hovered.borderVisible, '悬停时边框现身')
  assert(hovered.btnPE === 'auto', `悬停时复制按钮可点（pointer-events=${hovered.btnPE}）`)

  // ---- ③ 移出：回无壳态 ----
  const goOut = await outsideOf('[data-case="go"]')
  await moveTo(goOut.x, goOut.y)
  const away = await snap('[data-case="go"]')
  assert(away.hdOpacity === 0 && !away.borderVisible, '移出后回到无壳态（不是出现后常驻）')

  // ---- ④ 渲染保真（底色 / 等宽 / shiki / 行高）----
  assert(hovered.bg !== 'rgba(0, 0, 0, 0)' && hovered.bg !== 'rgb(255, 255, 255)',
    `代码块仍有自身底色（实际 ${hovered.bg}）—— 无壳 = 去描边，不是去底色`)
  assert(/mono|Menlo|SF Mono|Courier/i.test(hovered.font), `等宽字体保持（${hovered.font}）`)
  assert(hovered.hasShiki, 'shiki 高亮产物仍在（渲染未被破坏）')
  assert(away.bodyTop === hovered.bodyTop, '悬停不移动代码正文（头部为浮层）')
  assert(Math.abs(away.blkH - hovered.blkH) <= 0.5, `悬停不改变块高（${away.blkH} vs ${hovered.blkH}）`)

  // ---- ⑥ 头部不盖住首行左端（药丸几何）----
  assert(!hovered.hdCoversFirstLine,
    '浮出的头部不压住首行左端（右上角药丸）—— 单行短块必须可读')

  // ---- ⑦ 键盘可达 ----
  await page.evaluate(() => { const el = document.activeElement; if (el && el.blur) el.blur() })
  await page.locator('[data-case="go"] .codeblock .cb-copy').focus()
  await page.waitForTimeout(260)
  const focused = await snap('[data-case="go"]')
  assert(focused.hdOpacity === 1 && focused.borderVisible,
    `聚焦复制按钮时头部与边框现身（opacity=${focused.hdOpacity}）—— 默认隐藏不牺牲键盘可达性`)

  // ---- ⑧ 复制反馈：成功 / 失败都不能静默 ----
  // 成功路径：Chromium 无头下 clipboard 可用（需授权），断言按钮文案变「已复制」。
  const ctx = page.context()
  await ctx.grantPermissions(['clipboard-read', 'clipboard-write'])
  await page.locator('[data-case="go"] .codeblock .cb-copy').click()
  await page.waitForTimeout(400)
  const afterOk = await snap('[data-case="go"]')
  assert(afterOk.btnText !== '复制' || afterOk.btnText === '复制',
    `复制按钮点击后文案变化（实际「${afterOk.btnText}」）`)
  assert(/已复制/.test(afterOk.btnText), `复制成功显示「已复制」（实际「${afterOk.btnText}」）—— 不再静默`)
  // 剪贴板内容正确
  const clip = await page.evaluate(() => navigator.clipboard.readText())
  assert(clip.includes('package main'), `剪贴板写入正确内容（实得 ${JSON.stringify(clip.slice(0, 40))}）`)

  // 失败路径：把 writeText 打桩成抛错 → 必须显示「复制失败」
  await page.evaluate(() => {
    navigator.clipboard.writeText = () => Promise.reject(new Error('denied'))
  })
  await page.waitForTimeout(1700) // 等上一次「已复制」复位
  await page.locator('[data-case="go"] .codeblock .cb-copy').click()
  await page.waitForTimeout(400)
  const afterFail = await snap('[data-case="go"]')
  assert(/复制失败/.test(afterFail.btnText),
    `复制失败显示「复制失败」（实际「${afterFail.btnText}」）—— 失败不再被 catch{} 吞掉`)

  assert(errs.length === 0, `无页面/控制台报错${errs.length ? '：' + errs.join(' | ') : ''}`)

  await browser.close()
  cleanup()
  console.log(`\n[verify] 代码块「无壳 + 悬停浮出」+ 复制反馈全部通过（${pass} 项断言）`)
}

main().catch((e) => { console.error('FAIL', e); cleanup(); process.exit(1) })
