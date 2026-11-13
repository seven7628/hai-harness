#!/usr/bin/env node
// Hub composer 单行垂直居中 + 最多 4 行自动增高（P4-UI-14）。
//
// 前置：mock dev server（默认 5180，不要占 5173）：
//   VITE_PORT=5180 npx vite
// 运行：
//   node scripts/check-hub-composer.mjs [--url http://localhost:5180/]
//
// 断言：
//   · 单行 textarea 高 34px；左右 28/31px 控件与文本行中心同高（±0.6px）
//   · 4 行 = 100px；5 行仍 100px 且 overflow-y=auto（内部滚动，不挤消息区）
//   · 清空后回到 34px；DM / 群一致
//   · 390×844 无横向溢出，5 行仍不超过 100px
import { chromium } from 'playwright'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const BASE_URL = arg('url', process.env.HUB_COMPOSER_URL || 'http://localhost:5180/')

const failed = []
const errors = []
const check = (cond, msg) => { if (!cond) failed.push(msg) }

const browser = await chromium.launch()
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()) })
page.on('pageerror', (e) => errors.push(`pageerror ${e.message}`))

async function openHub() {
  await page.goto(BASE_URL, { waitUntil: 'domcontentloaded' })
  await page.waitForTimeout(1400)
  if (!(await page.locator('.hub-side-row').count())) {
    await page.locator('button[aria-label="智能体"]').first().click({ timeout: 8000 }).catch(() => {})
    await page.waitForTimeout(700)
  }
  await page.locator('.hub-side-row').first().waitFor({ timeout: 10000 })
}

async function openScope(kind) {
  await openHub()
  const tab = page.locator('.hub-side-tabs button').filter({ hasText: kind === 'group' ? '群组' : '智能体' }).first()
  if ((await tab.getAttribute('aria-selected')) !== 'true') await tab.click({ timeout: 6000 })
  await page.waitForTimeout(250)
  await page.locator('.hub-side-row').first().click({ timeout: 8000 })
  await page.waitForTimeout(800)
  await page.locator('.hub-demo .composer textarea').first().waitFor({ timeout: 8000 })
}

async function measure() {
  return page.evaluate(() => {
    const c = document.querySelector('.hub-demo .composer')
    const ta = c?.querySelector('textarea')
    const ic = c?.querySelector('.icon-btn')
    const sb = c?.querySelector('.send-btn')
    const rect = (e) => (e ? e.getBoundingClientRect() : null)
    const tr = rect(ta), ir = rect(ic), sr = rect(sb)
    const cy = (r) => (r ? r.top + r.height / 2 : NaN)
    return {
      taH: tr?.height ?? 0,
      taCy: cy(tr),
      iconCy: cy(ir),
      sendCy: cy(sr),
      scrollH: ta?.scrollHeight ?? 0,
      clientH: ta?.clientHeight ?? 0,
      overflowY: ta ? getComputedStyle(ta).overflowY : '',
      docW: document.documentElement.scrollWidth,
      winW: window.innerWidth,
    }
  })
}

async function checkScope(kind) {
  await openScope(kind)
  const label = kind === 'group' ? '群' : 'DM'
  const ta = page.locator('.hub-demo .composer textarea').first()
  let m = await measure()
  check(Math.abs(m.taH - 34) <= 0.6, `${label} 单行 textarea 高 34（got=${m.taH}）`)
  check(Math.abs(m.iconCy - m.taCy) <= 0.6, `${label} 左侧 28px 控件与文本行中心同高（Δ=${m.iconCy - m.taCy}）`)
  check(Math.abs(m.sendCy - m.taCy) <= 0.6, `${label} 发送按钮与文本行中心同高（Δ=${m.sendCy - m.taCy}）`)

  await ta.fill('第一行\n第二行\n第三行\n第四行')
  await page.waitForTimeout(120)
  m = await measure()
  check(Math.abs(m.taH - 100) <= 0.6, `${label} 4 行 textarea 高 100（got=${m.taH}）`)
  check(m.scrollH <= m.clientH, `${label} 4 行不出现内部滚动（sh=${m.scrollH} ch=${m.clientH}）`)

  await ta.fill('第一行\n第二行\n第三行\n第四行\n第五行')
  await page.waitForTimeout(120)
  m = await measure()
  check(Math.abs(m.taH - 100) <= 0.6, `${label} 5 行仍钉在 100（got=${m.taH}）`)
  check(m.scrollH > m.clientH && m.overflowY === 'auto', `${label} 5 行内部滚动（sh=${m.scrollH} ch=${m.clientH} oy=${m.overflowY}）`)
  await page.screenshot({ path: `/tmp/hub-composer-${kind}-5lines.png` })

  await ta.fill('')
  await page.waitForTimeout(120)
  m = await measure()
  check(Math.abs(m.taH - 34) <= 0.6, `${label} 清空后回到 34（got=${m.taH}）`)
}

await checkScope('group')
await checkScope('dm')

// 390：不横向溢出；5 行仍不超过 100。
// 先宽窗打开会话再缩窄：390 下左栏是图标条、没有 .hub-side-row。
await openScope('group')
await page.setViewportSize({ width: 390, height: 844 })
await page.waitForTimeout(250)
const ta390 = page.locator('.hub-demo .composer textarea').first()
await ta390.fill('一\n二\n三\n四\n五')
await page.waitForTimeout(150)
const m390 = await measure()
check(m390.docW <= 390, `390 无横向溢出（doc=${m390.docW}）`)
check(m390.taH <= 100.6, `390 5 行不超过 100（got=${m390.taH}）`)
await page.screenshot({ path: '/tmp/hub-composer-390.png' })

check(errors.length === 0, `无 console error / pageerror：${errors.slice(0, 3).join(' | ')}`)
await browser.close()

if (failed.length) {
  console.error(`❌ Hub composer 回归失败 ${failed.length} 条：`)
  for (const f of failed) console.error(`  · ${f}`)
  process.exit(1)
}
console.log('✅ Hub composer：单行居中、4 行增高、5 行内部滚动、DM/群/390 通过')
