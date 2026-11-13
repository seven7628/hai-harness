#!/usr/bin/env node
// 任务卡产物预览的真浏览器回归（P4-UI-12）。
//
// 前置：mock dev server 已启动（默认 5180，不要占 5173）：
//   VITE_PORT=5180 npx vite
// 运行：
//   node scripts/check-hub-artifacts.mjs [--url http://localhost:5180/]
//
// 断言：
//   · 群聊任务卡 3 个产物都是可聚焦的 <button>（aria-haspopup=dialog）
//   · md：弹窗里是真 Markdown（标题 + 表格）；csv：真表格行；png：<img> naturalWidth=120
//   · Esc / 遮罩都能关闭，焦点回到触发按钮
//   · 390px 下弹窗不产生文档级横向溢出
//   · 无 console error / pageerror
import { chromium } from 'playwright'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const URL = arg('url', process.env.HUB_ARTIFACT_URL || 'http://localhost:5180/')

const failed = []
const errors = []
const check = (cond, msg) => { if (!cond) failed.push(msg) }

const browser = await chromium.launch()
const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
page.on('console', (m) => {
  if (m.type() === 'error') errors.push(m.text())
})
page.on('pageerror', (e) => errors.push(`pageerror ${e.message}`))

async function openGroupConversation() {
  await page.goto(URL, { waitUntil: 'domcontentloaded' })
  await page.waitForTimeout(1500)
  if (!(await page.locator('.hub-side-row').count())) {
    await page.locator('button[aria-label="智能体"]').first().click({ timeout: 8000 }).catch(() => {})
    await page.waitForTimeout(600)
  }
  await page.locator('.hub-side-row').first().waitFor({ timeout: 10000 })
  const tab = page.locator('.hub-side-tabs button').filter({ hasText: '群组' }).first()
  if ((await tab.getAttribute('aria-selected')) !== 'true') await tab.click({ timeout: 6000 })
  await page.waitForTimeout(250)
  await page.locator('.hub-side-row').first().click({ timeout: 8000 })
  await page.waitForTimeout(900)
}

async function artifactButton(name) {
  return page.locator('.event-card--task button.artifact-line--action').filter({ hasText: name }).first()
}

async function openArtifact(name) {
  const btn = await artifactButton(name)
  await btn.waitFor({ timeout: 8000 })
  await btn.click()
  await page.locator('.hub-artifact-modal').waitFor({ timeout: 8000 })
  await page.waitForTimeout(120)
}

async function closeAndAssertFocus(method) {
  if (method === 'esc') await page.keyboard.press('Escape')
  else await page.mouse.click(4, 4)
  await page.locator('.hub-artifact-modal').waitFor({ state: 'detached', timeout: 4000 })
  await page.waitForTimeout(120)
  const focus = await page.evaluate(() => {
    const el = document.activeElement
    return {
      tag: el?.tagName ?? '',
      cls: el?.className ?? '',
      text: (el?.textContent ?? '').trim(),
      dialog: Boolean(el?.closest?.('.hub-artifact-modal')),
    }
  })
  check(!focus.dialog, `${method} 关闭后焦点仍留在弹窗`)
  check(focus.tag === 'BUTTON' && String(focus.cls).includes('artifact-line--action'),
    `${method} 关闭后焦点没有回到触发按钮（${focus.tag}.${focus.cls}）`)
  return focus
}

try {
  await openGroupConversation()

  const buttons = page.locator('.event-card--task button.artifact-line--action')
  const count = await buttons.count()
  check(count === 3, `任务卡可预览产物按钮 = ${count}（应为 3）`)
  const aria = await buttons.first().getAttribute('aria-haspopup')
  check(aria === 'dialog', `产物按钮 aria-haspopup=${aria}（应为 dialog）`)

  // —— Markdown ——
  await openArtifact('pricing-v3.md')
  check(await page.evaluate(() => Boolean(document.querySelector('.hub-artifact-modal')?.contains(document.activeElement))),
    '弹窗打开后焦点不在弹窗内')
  const md = await page.evaluate(() => {
    const modal = document.querySelector('.hub-artifact-modal')
    return {
      text: modal?.querySelector('.hub-artifact-doc')?.textContent ?? '',
      headings: modal?.querySelectorAll('.hub-artifact-doc h1, .hub-artifact-doc h2').length ?? 0,
      rows: modal?.querySelectorAll('.hub-artifact-doc table tbody tr').length ?? 0,
      title: modal?.querySelector('#hub-artifact-title')?.textContent ?? '',
    }
  })
  check(md.text.includes('三家价格对比'), 'Markdown 弹窗缺少标题正文')
  check(md.headings >= 2, `Markdown 标题数 = ${md.headings}（应 ≥2）`)
  check(md.rows >= 3, `Markdown 表格行 = ${md.rows}（应 ≥3）`)
  check(md.title === 'pricing-v3.md', `弹窗标题 = ${md.title}`)
  const focusAfterEsc = await closeAndAssertFocus('esc')
  check(focusAfterEsc.text.includes('pricing-v3.md'), 'Esc 关闭后焦点按钮不是刚打开的 md 产物')

  // —— 遮罩关闭（按钮 → 弹窗 → 点 scrim 左上角）——
  await openArtifact('pricing-v3.md')
  await closeAndAssertFocus('scrim')

  // —— CSV ——
  await openArtifact('pricing-v3.csv')
  const csv = await page.evaluate(() => {
    const modal = document.querySelector('.hub-artifact-modal')
    return {
      rows: modal?.querySelectorAll('.csv-preview table tbody tr').length ?? 0,
      text: modal?.querySelector('.csv-preview')?.textContent ?? '',
    }
  })
  check(csv.rows >= 4, `CSV 表格行 = ${csv.rows}（应 ≥4）`)
  check(csv.text.includes('Globex'), 'CSV 表格缺少 Globex 行')
  await closeAndAssertFocus('esc')

  // —— 图片 ——
  await openArtifact('cost-curve.png')
  const img = await page.evaluate(() => {
    const el = document.querySelector('.hub-artifact-image img')
    return el ? { w: el.naturalWidth, h: el.naturalHeight, src: el.getAttribute('src')?.slice(0, 20) } : null
  })
  check(Boolean(img), 'PNG 弹窗里没有 <img>')
  check(img?.w === 120 && img?.h === 40, `图片尺寸 = ${img?.w}×${img?.h}（应为 120×40）`)
  check(String(img?.src ?? '').startsWith('data:image/'), '图片 src 不是 data URL')
  await closeAndAssertFocus('esc')

  // —— 390px：无文档级横向溢出 ——
  await page.setViewportSize({ width: 390, height: 900 })
  await page.waitForTimeout(250)
  await openArtifact('pricing-v3.md')
  const narrow = await page.evaluate(() => {
    const modal = document.querySelector('.hub-artifact-modal')
    const b = modal?.getBoundingClientRect()
    return {
      scrollWidth: document.documentElement.scrollWidth,
      innerWidth: window.innerWidth,
      modalRight: b ? Math.round(b.right) : null,
      modalLeft: b ? Math.round(b.left) : null,
      tableOverflow: (() => {
        const wrap = modal?.querySelector('.md-table-wrap')
        return wrap ? getComputedStyle(wrap).overflowX : null
      })(),
    }
  })
  check(narrow.scrollWidth <= narrow.innerWidth, `390px 文档横向溢出 ${narrow.scrollWidth} > ${narrow.innerWidth}`)
  check(narrow.modalLeft >= 0 && narrow.modalRight <= narrow.innerWidth,
    `390px 弹窗越界 ${narrow.modalLeft}..${narrow.modalRight}`)
  check(narrow.tableOverflow === 'auto', `390px 表格容器 overflow-x=${narrow.tableOverflow}（应为 auto）`)
  await page.keyboard.press('Escape')
} catch (e) {
  failed.push(`运行时异常：${e?.message ?? e}`)
} finally {
  await browser.close()
}

if (errors.length) failed.push(...errors.map((e) => `console/pageerror: ${e}`))
if (failed.length) {
  console.error(`✗ check:hub-artifacts 失败 (${failed.length})`)
  for (const f of failed) console.error(`  · ${f}`)
  process.exit(1)
}
console.log('✓ check:hub-artifacts：md/csv/png 弹窗、Esc/遮罩关闭、焦点返回、390 无横向溢出')
