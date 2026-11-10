#!/usr/bin/env node
// 会话区（群 / DM）真浏览器截图 + DOM 断言。
//
// 用法：先起 dev server（不要占 5173-5175），再跑
//   VITE_PORT=5177 npx vite
//   node scripts/shoot-hub-conv.mjs [--url http://localhost:5177] [--out /tmp/hub-conv-shots]
//
// 断言（任一失败 → 退出码 1）：
//   · documentElement.scrollWidth <= 视口宽（0 横向溢出）
//   · 用户气泡右边缘与 760px 会话列右边缘对齐（右对齐）
//   · agent 消息背景透明（无气泡）且有作者头
//   · composer meta 行存在
//   · 无 console error / pageerror
//
// 不造假数据：截图全部来自 mock 的真实 fixture（2 人群 / 两条 DM）。
// 空态需要 mock 里没有消息的 agent，本脚本不覆盖（见交付说明里的临时 mock 截图）。
import { chromium } from 'playwright'
import { mkdirSync } from 'node:fs'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const URL = arg('url', process.env.HUB_CONV_URL || 'http://localhost:5177/')
const OUT = arg('out', '/tmp/hub-conv-shots')
const VIEWPORTS = [
  [1440, 900],
  [1280, 900],
  [640, 900],
  [390, 900],
]
const THEMES = ['ink', 'paper']

mkdirSync(OUT, { recursive: true })
const browser = await chromium.launch()
const errors = []
const rows = []

async function openConversation(page, kind) {
  if (kind === 'writer') {
    if (!(await page.locator('.hub-side-row').count())) {
      await page.locator('button[aria-label="智能体"]').first().click({ timeout: 8000 }).catch(() => {})
      await page.waitForTimeout(600)
    }
    await page.locator('.hub-side-row').filter({ hasText: 'Writer' }).first().click({ timeout: 8000 })
    await page.waitForTimeout(700)
    return
  }
  if (!(await page.locator('.hub-side-row').count())) {
    await page.locator('button[aria-label="智能体"]').first().click({ timeout: 8000 }).catch(() => {})
    await page.waitForTimeout(600)
  }
  await page.locator('.hub-side-row').first().waitFor({ timeout: 10000 })
  if (kind === 'group') {
    const tab = page.locator('.hub-side-tabs button').filter({ hasText: '群组' }).first()
    if ((await tab.getAttribute('aria-selected')) !== 'true') await tab.click({ timeout: 6000 })
    await page.waitForTimeout(250)
    await page.locator('.hub-side-row').first().click({ timeout: 8000 })
  } else {
    const tab = page.locator('.hub-side-tabs button').filter({ hasText: '智能体' }).first()
    if (await tab.count()) {
      if ((await tab.getAttribute('aria-selected')) !== 'true') await tab.click()
      await page.waitForTimeout(250)
    }
    await page.locator('.hub-side-row').filter({ hasText: 'Researcher' }).first().click({ timeout: 8000 })
  }
  await page.waitForTimeout(700)
  // 窄屏：左栏是抽屉，选完收起；右栏只保留图标条（聚焦会话区截图）。
  const collapseLeft = page.locator('button[aria-label="左栏收缩 / ⌘B"]').first()
  if (await collapseLeft.count() && await page.locator('.left.expanded').count()) {
    await collapseLeft.click({ timeout: 4000 }).catch(() => {})
    await page.waitForTimeout(300)
  }
  await page.evaluate(() => {
    const store = window.__store
    store?.getState?.()?.setRightExpanded?.(false)
  })
  await page.waitForTimeout(250)
}

async function shot(width, height, theme, kind) {
  const name = `${width}-${kind}-${theme}`
  const page = await browser.newPage({ viewport: { width, height } })
  page.on('console', (m) => {
    if (m.type() === 'error') errors.push(`${name}: ${m.text()}`)
  })
  page.on('pageerror', (e) => errors.push(`${name}: pageerror ${e.message}`))
  await page.goto(URL, { waitUntil: 'domcontentloaded' })
  await page.waitForTimeout(1500)
  if (theme === 'paper') {
    await page.evaluate(() => window.__store?.getState?.().setTheme?.('light'))
  }
  await page.waitForTimeout(250)
  await openConversation(page, kind)
  await page.evaluate(() => {
    const list = document.querySelector('.hub-list')
    if (list) list.scrollTop = 0
  })
  await page.waitForTimeout(250)

  const m = await page.evaluate(() => {
    const q = (s) => document.querySelector(s)
    const rect = (el) => {
      if (!el) return null
      const b = el.getBoundingClientRect()
      return { left: Math.round(b.left), right: Math.round(b.right), width: Math.round(b.width) }
    }
    const thread = rect(q('.hub-thread'))
    const bubble = rect(q('.hub-user-bubble'))
    const agent = q('.hub-msg.agent')
    return {
      scrollWidth: document.documentElement.scrollWidth,
      thread,
      bubble,
      bubbleRightGap: bubble && thread ? Math.round(thread.right - bubble.right) : null,
      agentBackground: agent ? getComputedStyle(agent).backgroundColor : null,
      agentAuthor: q('.hub-msg.agent .hub-msg-name')?.textContent ?? null,
      composerMeta: q('.hub-composer-meta')?.innerText.replace(/\n/g, ' | ') ?? null,
      headerHeight: Math.round(q('.hub-conv-head')?.getBoundingClientRect().height ?? 0),
      dateDivider: q('.hub-day-divider')?.textContent ?? null,
    }
  })
  const failed = []
  if (m.scrollWidth > width) failed.push(`横向溢出 ${m.scrollWidth} > ${width}`)
  if (m.bubbleRightGap !== 0) failed.push(`用户气泡未右对齐（gap=${m.bubbleRightGap}）`)
  if (m.agentBackground !== 'rgba(0, 0, 0, 0)') failed.push(`agent 消息有气泡底色（${m.agentBackground}）`)
  if (!m.agentAuthor) failed.push('agent 消息缺少作者头')
  if (!m.composerMeta) failed.push('composer 缺少 meta 行')
  if (failed.length) errors.push(`${name}: ${failed.join('；')}`)

  await page.screenshot({ path: `${OUT}/${name}.png` })
  rows.push({ name, ...m, failed })
  await page.close()
}

for (const [width, height] of VIEWPORTS) {
  for (const theme of THEMES) {
    for (const kind of ['group', 'dm']) {
      await shot(width, height, theme, kind)
    }
  }
}
// 有任务卡的群聊：滚到任务卡，单独一张（1440/390 × ink/paper）。
for (const [width, height] of [[1440, 900], [390, 900]]) {
  for (const theme of THEMES) {
    const page = await browser.newPage({ viewport: { width, height } })
    const name = `${width}-group-task-${theme}`
    page.on('console', (m) => { if (m.type() === 'error') errors.push(`${name}: ${m.text()}`) })
    page.on('pageerror', (e) => errors.push(`${name}: pageerror ${e.message}`))
    await page.goto(URL, { waitUntil: 'domcontentloaded' })
    await page.waitForTimeout(1500)
    if (theme === 'paper') await page.evaluate(() => window.__store?.getState?.().setTheme?.('light'))
    await page.waitForTimeout(250)
    await openConversation(page, 'group')
    await page.evaluate(() => {
      document.querySelector('.hub-event--task')?.scrollIntoView({ block: 'center' })
    })
    await page.waitForTimeout(350)
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth)
    if (overflow > width) errors.push(`${name}: 横向溢出 ${overflow} > ${width}`)
    await page.screenshot({ path: `${OUT}/${name}.png` })
    rows.push({ name, scrollWidth: overflow })
    await page.close()
  }
}
// 次要路径：Writer 的 DM（同版式、不同身份色/模型）。
await shot(1440, 900, 'ink', 'writer')

await browser.close()
console.log(JSON.stringify({ out: OUT, rows, errors }, null, 2))
if (errors.length) {
  console.error(`\nHUB_CONV_SHOTS_FAIL (${errors.length})`)
  process.exit(1)
}
console.log(`\nHUB_CONV_SHOTS_PASS → ${OUT}`)
