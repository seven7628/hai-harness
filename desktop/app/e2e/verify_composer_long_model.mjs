#!/usr/bin/env node
// 回归验证：长 Provider + 模型名时，Composer 底部行不得「整簇换行 → 右簇靠左、右侧留空白、发送钮不在最右」。
//
// 背景（2026-09-10）：模型名很长时右簇比「模式 chip + 右簇」可用宽度还宽，
// .row2 换行后第二行靠左（justify-content: space-between 单元素落到行首），
// 发送钮停在行中间、右侧留出上百 px 空白 —— 真机截图里即「样式异常」。
// 现约定：底部行恒单行，空间不足由模型 chip 的 label 省略号收敛。
//
// 用法：cd desktop/app && node e2e/verify_composer_long_model.mjs
//   可选 URL_BASE=http://localhost:5173/ 复用已运行的 dev server；
//   否则自动起一个 vite dev server（无需 build）。
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import fs from 'fs'
import os from 'os'
import path from 'path'
import net from 'net'
import { fileURLToPath } from 'url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = Number(process.env.PORT || 4188)

let pass = 0, fail = 0
const ok = (m) => { pass++; console.log(`   ✅ ${m}`) }
const bad = (m) => { fail++; console.log(`   ❌ ${m}`) }

async function portOpen(port) {
  return new Promise((resolve) => {
    const s = net.connect({ port, host: 'localhost' })
    const t = setTimeout(() => { s.destroy(); resolve(false) }, 500)
    s.on('connect', () => { clearTimeout(t); s.destroy(); resolve(true) })
    s.on('error', () => { clearTimeout(t); resolve(false) })
  })
}

async function waitPort(port, timeoutMs = 30000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    if (await portOpen(port)) return
    if (Date.now() > deadline) throw new Error(`dev server 未就绪 (port ${port})`)
    await new Promise((r) => setTimeout(r, 250))
  }
}

// 模型名长度矩阵（含用户上报的 deepseek-v4.1-flash-expires-on-0910）
const NAMES = [
  ['短', 'deepseek-v4-flash'],
  ['中', 'deepseek-v4.1-flash-expires-on-0910'],
  ['长', 'deepseek-v4.1-flash-expires-on-0910-ultra-preview-2026-q4'],
  ['超长', 'deepseek-v4.1-flash-expires-on-0910-ultra-preview-2026-q4-extended-reasoning-variant-with-a-very-long-suffix-0123456789'],
]
const WIDTHS = (process.env.WIDTHS || '1600,1440,1280,1120,1000,900,820,760,700,620,540').split(',').map(Number)

let server = null
let URL_BASE = process.env.URL_BASE
if (!URL_BASE) {
  const alive = await portOpen(PORT)
  if (!alive) {
    server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort'], { cwd: APP, stdio: 'ignore' })
    process.on('exit', () => { try { server.kill('SIGTERM') } catch { /* ignore */ } })
  }
  URL_BASE = `http://localhost:${PORT}/`
}
await waitPort(Number(new URL(URL_BASE).port || 80))

// 种子 settings：真实 ~/.go-code/settings.json（含 19 家 provider 与真实模型表），
// 每个用例再覆写 deepseek 的模型表 —— 只在首次注入，避免 reload 时被覆盖回原值。
const seed = fs.existsSync(path.join(os.homedir(), '.go-code/settings.json'))
  ? JSON.parse(fs.readFileSync(path.join(os.homedir(), '.go-code/settings.json'), 'utf8'))
  : { provider: { active: 'deepseek', deepseek: { base_url: '', models: {}, max_tokens: {}, protocol: 'chat_completions', input_types: {} } } }

const browser = await chromium.launch({ headless: true, channel: 'chrome' })
const page = await browser.newPage({ viewport: { width: WIDTHS[0], height: 900 } })
page.on('pageerror', (e) => console.error('[pageerror]', e.message))
await page.addInitScript(({ s }) => {
  if (!localStorage.getItem('go-code.projects')) {
    localStorage.setItem('go-code.projects', JSON.stringify({ paths: ['/mock/proj-auth'], active: '/mock/proj-auth' }))
  }
  if (!localStorage.getItem('go-code.settings')) localStorage.setItem('go-code.settings', JSON.stringify(s))
}, { s: seed })
await page.goto(URL_BASE, { waitUntil: 'domcontentloaded' })
await page.waitForSelector('textarea[placeholder*="输入消息"]', { timeout: 30000 })
const nb = page.getByRole('button', { name: '新建会话' }).first()
if (await nb.count()) { await nb.click(); await page.waitForTimeout(500) }

const MODEL_CHIP = '.composer .row2-right .model-chip:not(.effort-chip)' // effort chip 也带 model-chip 类

for (const [tag, model] of NAMES) {
  await page.evaluate((m) => {
    const s = JSON.parse(localStorage.getItem('go-code.settings'))
    const d = s.provider.deepseek
    d.models = { [m]: 1000000 }
    d.max_tokens = { [m]: 384000 }
    d.input_types = { [m]: ['text', 'image'] } // 带图片输入 → 行内含 attach 按钮（最挤的情形）
    s.provider.active = 'deepseek'
    localStorage.setItem('go-code.settings', JSON.stringify(s))
    localStorage.setItem('go-code.prefs', JSON.stringify({ mode: 'full-access', effort: 'max', model: m, provider: 'deepseek' }))
  }, model)
  await page.reload({ waitUntil: 'domcontentloaded' })
  await page.waitForSelector('textarea[placeholder*="输入消息"]', { timeout: 30000 })
  await page.waitForTimeout(400)
  console.log(`\n########## 模型名=${tag}（${model.length} 字符）##########`)

  for (const w of WIDTHS) {
    await page.setViewportSize({ width: w, height: 900 })
    await page.waitForTimeout(320)
    const r = await page.evaluate((chipSel) => {
      const R = (el) => { if (!el) return null; const b = el.getBoundingClientRect(); return { x: +b.x.toFixed(1), right: +b.right.toFixed(1), y: +b.y.toFixed(1), bottom: +b.bottom.toFixed(1), w: +b.width.toFixed(1) } }
      const composer = document.querySelector('.composer')
      const row2 = document.querySelector('.composer .row2')
      const right = document.querySelector('.composer .row2-right')
      const chip = document.querySelector(chipSel)
      const label = chip?.querySelector('.model-chip-label')
      const mode = document.querySelector('.composer .mode-chip')
      const send = document.querySelector('.composer .send-btn')
      const items = [mode, document.querySelector('.composer .effort-chip'), document.querySelector('.composer .ctx'), document.querySelector('.composer .attach-btn'), send].filter(Boolean)
      const boxes = items.map((el) => ({ cls: el.className.split(' ')[1] || el.className, r: R(el) }))
      const collide = []
      for (let i = 0; i < boxes.length; i++) for (let j = i + 1; j < boxes.length; j++) {
        const a = boxes[i].r, b = boxes[j].r
        if (a.y < b.bottom - 0.5 && b.y < a.bottom - 0.5 && a.x < b.right - 0.5 && b.x < a.right - 0.5) collide.push(`${boxes[i].cls}∩${boxes[j].cls}`)
      }
      const cRect = composer.getBoundingClientRect()
      const r2cs = getComputedStyle(row2)
      return {
        composer: { x: +cRect.x.toFixed(1), right: +cRect.right.toFixed(1), w: +cRect.width.toFixed(1) },
        padR: parseFloat(r2cs.paddingRight), padL: parseFloat(r2cs.paddingLeft),
        mode: R(mode), right: R(right), chip: R(chip), label: R(label),
        labelText: label?.textContent,
        labelScrollW: label?.scrollWidth, labelClientW: label?.clientWidth,
        labelStyle: label ? `${getComputedStyle(label).overflow}/${getComputedStyle(label).textOverflow}` : null,
        send: R(send), chipOverflow: chip ? getComputedStyle(chip).overflow : null,
        rightScrollW: right.scrollWidth, rightClientW: right.clientWidth,
        chipScrollW: chip?.scrollWidth, chipClientW: chip?.clientWidth,
        collide,
        docScrollW: document.documentElement.scrollWidth, viewportW: window.innerWidth,
      }
    }, MODEL_CHIP)

    const sameLine = Math.abs(r.mode.y - r.right.y) < 3
    const sendPinned = Math.abs(r.send.right - (r.composer.right - r.padR)) < 1.5
    const noOverflow = r.rightScrollW <= r.rightClientW + 0.5 && r.chipScrollW <= r.chipClientW + 0.5
    // 非紧凑态（composer > 430）：模式 chip 恒最左；紧凑态整行右对齐（既有设计）
    const modeLeft = r.composer.w <= 430 || Math.abs(r.mode.x - (r.composer.x + r.padL + 1)) < 1.5
    const truncated = r.labelScrollW > r.labelClientW
    const ellipsisOk = !truncated || r.labelStyle === 'hidden/ellipsis'
    const problems = []
    if (!sameLine) problems.push(`模式 chip 与右簇不同行 (Δy=${(r.right.y - r.mode.y).toFixed(0)})`)
    if (!sendPinned) problems.push(`发送钮未贴右 (send.right=${r.send.right} vs ${(r.composer.right - r.padR).toFixed(1)})`)
    if (!noOverflow) problems.push(`内容溢出 (右簇 ${r.rightScrollW - r.rightClientW}px / chip ${r.chipScrollW - r.chipClientW}px)`)
    if (!modeLeft) problems.push(`模式 chip 不在最左 (x=${r.mode.x}，期望 ${(r.composer.x + r.padL + 1).toFixed(1)})`)
    if (!ellipsisOk) problems.push(`label 溢出但未省略号截断 (${r.labelStyle})`)
    if (r.collide.length) problems.push(`控件重叠 ${r.collide.join(',')}`)
    if (r.docScrollW > r.viewportW) problems.push('页面横向溢出')
    if (r.chipOverflow !== 'visible') problems.push(`chip overflow=${r.chipOverflow}（下拉菜单会被裁掉）`)
    const head = `w=${String(w).padEnd(5)} composer=${String(r.composer.w).padEnd(6)}`
    if (problems.length) bad(`${head} ${problems.join('; ')}`)
    else ok(`${head} 单行·贴右·无溢出${truncated ? ` · 省略号截断 ${r.labelScrollW - r.labelClientW}px` : ' · label 完整'}`)
  }
}

// 下拉菜单回归：chip 必须保持 overflow:visible，菜单可见且可点选
await page.setViewportSize({ width: 1280, height: 900 })
await page.waitForTimeout(300)
console.log('\n########## 下拉菜单回归 ##########')
await page.locator(MODEL_CHIP).click()
await page.waitForTimeout(300)
const popup = await page.evaluate((sel) => {
  const el = document.querySelector(`${sel} .popup.open`)
  if (!el) return { exists: false }
  const b = el.getBoundingClientRect()
  return { exists: true, w: +b.width.toFixed(1), h: +b.height.toFixed(1), items: el.querySelectorAll('.mi').length }
}, MODEL_CHIP)
if (popup.exists && popup.w > 100 && popup.h > 50 && popup.items > 0) ok(`模型菜单可见（${popup.w}×${popup.h}，${popup.items} 项）`)
else bad(`模型菜单异常: ${JSON.stringify(popup)}`)
const before = await page.locator(`${MODEL_CHIP} .model-chip-label`).textContent()
const item = page.locator(`${MODEL_CHIP} .popup.open .mi`).nth(1)
if (await item.count()) {
  await item.click()
  await page.waitForTimeout(400)
  const after = await page.locator(`${MODEL_CHIP} .model-chip-label`).textContent()
  if (after !== before) ok(`菜单可点选切换模型（"${before.slice(0, 30)}…" → "${after.slice(0, 30)}…"）`)
  else bad('菜单点击未生效（模型未切换）')
} else bad('菜单项缺失')
await page.evaluate(() => document.querySelector('.composer .effort-chip')?.click())
await page.waitForTimeout(250)
const ep = await page.evaluate(() => {
  const el = document.querySelector('.composer .effort-chip .popup.open')
  if (!el) return null
  const b = el.getBoundingClientRect()
  return { w: +b.width.toFixed(1), h: +b.height.toFixed(1) }
})
if (ep && ep.w > 50 && ep.h > 30) ok(`Effort 菜单可见（${ep.w}×${ep.h}）`)
else bad(`Effort 菜单异常: ${JSON.stringify(ep)}`)

await browser.close()
if (server) { try { server.kill('SIGTERM') } catch { /* ignore */ } }
console.log(`\n[verify] 通过 ${pass} 项，失败 ${fail} 项`)
process.exit(fail ? 1 : 0)
