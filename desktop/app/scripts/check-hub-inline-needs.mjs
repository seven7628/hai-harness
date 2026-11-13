#!/usr/bin/env node
// Hub 单聊/群内联「任务等待你」卡（P4-UI-15）。
//
// 前置：mock dev server（默认 5180，不要占 5173）。
// 运行：node scripts/check-hub-inline-needs.mjs [--url http://localhost:5180/]
//
// 断言：
//   · ?hubFixture=needs 群内出现审批内联卡，带 Researcher / workspace / Project session / 子智能体名
//   · 批准后卡片消失（走 hub_need_resolve，不写 hub_message）
//   · 有 group_id 的 need 不出现在 DM 内联卡（作用域隔离）
//   · 390 无横向溢出；无 console error
import { chromium } from 'playwright'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const BASE_URL = arg('url', process.env.HUB_INLINE_NEEDS_URL || 'http://localhost:5180/')
const FIXTURE_URL = new URL(BASE_URL)
FIXTURE_URL.searchParams.set('hubFixture', 'needs')

const failed = []
const errors = []
const check = (cond, msg) => { if (!cond) failed.push(msg) }

const browser = await chromium.launch()
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()) })
page.on('pageerror', (e) => errors.push(`pageerror ${e.message}`))

async function openHub(url = FIXTURE_URL.toString()) {
  await page.goto(url, { waitUntil: 'domcontentloaded' })
  await page.waitForTimeout(1500)
  if (!(await page.locator('.hub-side-row').count())) {
    await page.locator('button[aria-label="智能体"]').first().click({ timeout: 8000 }).catch(() => {})
    await page.waitForTimeout(700)
  }
  await page.locator('.hub-side-row').first().waitFor({ timeout: 10000 })
}

async function openGroup() {
  await openHub()
  const tab = page.locator('.hub-side-tabs button').filter({ hasText: '群组' }).first()
  if ((await tab.getAttribute('aria-selected')) !== 'true') await tab.click({ timeout: 6000 })
  await page.waitForTimeout(250)
  await page.locator('.hub-side-row').first().click({ timeout: 8000 })
  await page.waitForTimeout(900)
  await page.locator('.hub-demo .composer textarea').first().waitFor({ timeout: 8000 })
}

async function openDM() {
  await openHub()
  const tab = page.locator('.hub-side-tabs button').filter({ hasText: '智能体' }).first()
  if ((await tab.getAttribute('aria-selected')) !== 'true') await tab.click({ timeout: 6000 })
  await page.waitForTimeout(250)
  await page.locator('.hub-side-row').first().click({ timeout: 8000 })
  await page.waitForTimeout(900)
  await page.locator('.hub-demo .composer textarea').first().waitFor({ timeout: 8000 })
}

// 1) 群内审批内联卡 + 归属
await openGroup()
const card = page.locator('.hub-inline-need--approval').first()
await card.waitFor({ timeout: 10000 })
const text = await card.textContent()
check(String(text).includes('Researcher'), `群内联卡显示发起智能体（got=${text}）`)
check(String(text).includes('workspace'), `群内联卡显示 Project workspace（got=${text}）`)
check(String(text).includes('s_mock_project'), `群内联卡显示 Project session（got=${text}）`)
check(String(text).includes('Researcher-sub'), `群内联卡显示子智能体名（got=${text}）`)
check(await page.locator('.hub-inline-need--question').count() >= 1, '群内联提问卡也出现')
await page.screenshot({ path: '/tmp/hub-inline-needs-group.png' })

// 批准：卡片消失，且没有新增 hub 消息（只走 need resolve）
const beforeMsgs = await page.locator('.hub-demo .msg-row').count()
await card.locator('button').filter({ hasText: '批准' }).first().click()
await page.waitForTimeout(900)
check(await page.locator('.hub-inline-need--approval').count() === 0, '批准后审批内联卡消失')
check(await page.locator('.hub-demo .msg-row').count() === beforeMsgs, '内联卡不写 hub 消息（只展示）')

// 2) 群 need 不出现在 DM
await openDM()
await page.waitForTimeout(600)
check(await page.locator('.hub-inline-need').count() === 0, '带 group_id 的 need 不泄漏到 DM 内联卡')

// 3) 390 无横向溢出
await openGroup()
await page.setViewportSize({ width: 390, height: 844 })
await page.waitForTimeout(300)
check(await page.locator('.hub-inline-need').count() >= 1, '390 下内联卡仍渲染')
const docW = await page.evaluate(() => document.documentElement.scrollWidth)
check(docW <= 390, `390 无横向溢出（doc=${docW}）`)
await page.screenshot({ path: '/tmp/hub-inline-needs-390.png' })

check(errors.length === 0, `无 console error / pageerror：${errors.slice(0, 3).join(' | ')}`)
await browser.close()

if (failed.length) {
  console.error(`❌ Hub 内联 need 回归失败 ${failed.length} 条：`)
  for (const f of failed) console.error(`  · ${f}`)
  process.exit(1)
}
console.log('✅ Hub 内联 need：群归属、审批、隔离、390 通过')
