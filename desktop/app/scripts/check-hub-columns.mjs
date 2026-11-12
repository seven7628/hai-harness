#!/usr/bin/env node
// 会话区「消息内容列 / composer 内层」列对齐实测（群 + DM，5 个宽度）。
//
// 背景：用户报「会话栏没对齐、没居中」。两个根因都只体现在**类名被两套 CSS 同时命中**上：
//   1. 全局 `.composer-wrap`（index.css）带 max-width:800 + margin:0 auto，把 demo 的
//      composer 外层收成 800，再被 28px 内边距压成 744；而 .messages-inner 是 760。
//      1440 下实测 messages-inner x=278 w=760 / composer-inner x=286 w=744。
//   2. ≤900 断点里消息区 14px、composer 12px（hub.css 与 demo.css 各一对），
//      于是 641-900 之间左右各差 2px；≤640 两边都是 12，反而正常。
// DOM 结构完全正常，所以 e2e 的选择器断言全绿 —— 只有真浏览器量矩形才看得见。
//
// 用法（需要 dev server；默认复用 5180，不占 5173）：
//   cd desktop/app && npx vite --port 5180          # 另开一个终端
//   node scripts/check-hub-columns.mjs              # 默认 http://localhost:5180/
//   node scripts/check-hub-columns.mjs --url http://localhost:5181/ --out /tmp/hub-columns
//
// 判据（任一失败 → exit 1）：
//   · 每个宽度 × 每个视图，messages-inner 与 composer-inner 的左右边缘差 ≤0.5px（亚像素）；
//   · 1440 下群聊两者同为 760px 且 x 相同；
//   · ≤900 消息区与 composer 外层左右内边距一致（14px），≤640 保持 12px；
//   · documentElement.scrollWidth <= 视口宽（0 横向溢出）。
// 截图落到 --out（默认 /tmp），不进仓库。
//
// dev server 没起 → 打印怎么起 + exit 2（与「对齐不达标」的 exit 1 区分开）。
import { chromium } from 'playwright'
import { mkdirSync } from 'node:fs'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const URL = arg('url', process.env.HUB_COLUMNS_URL || 'http://localhost:5180/')
const OUT = arg('out', '/tmp/hub-columns-shots')
// 1440 覆盖「宽屏 760 列」；1024 是中间档；900 正好压在断点上；783 是断点内、非 640；
// 640 压在窄屏断点上（必须保持 12px 口径）。
const WIDTHS = [1440, 1024, 900, 783, 640]
const HEIGHT = 900
const KINDS = ['group', 'dm']
const TOLERANCE = 0.5

// —— dev server preflight ——
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

const down = await preflight(URL)
if (down) {
  console.error(`❌ dev server 不可用：${URL}（${down}）`)
  console.error('   先另开一个终端起 dev server（不要占 5173）：')
  console.error('     cd desktop/app && npx vite --port 5180')
  console.error('   或换端口后重跑：')
  console.error('     node scripts/check-hub-columns.mjs --url http://localhost:5181/')
  process.exit(2)
}

// —— 会话切换（与 shoot-hub-conv.mjs 同一套真实现：左栏行 → 群组/智能体页签）——
async function openConversation(page, kind) {
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
  // 窄屏左栏是抽屉：选完收起，让会话区拿到干净宽度（量的是列对齐，不是抽屉）。
  const collapseLeft = page.locator('button[aria-label="左栏收缩 / ⌘B"]').first()
  if ((await collapseLeft.count()) && (await page.locator('.left.expanded').count())) {
    await collapseLeft.click({ timeout: 4000 }).catch(() => {})
    await page.waitForTimeout(300)
  }
  await page.evaluate(() => {
    window.__store?.getState?.()?.setRightExpanded?.(false)
  })
  await page.waitForTimeout(250)
}

// 只量矩形，不猜样式：左右边缘差就是用户看到的「没对齐」。
function measureColumns(rootSelector) {
  const root = document.querySelector(rootSelector)
  if (!root) return { missing: rootSelector }
  const thread = root.querySelector('.messages-inner')
  const composer = root.querySelector('.composer-inner')
  const messages = root.querySelector('.messages')
  const wrap = root.querySelector('.composer-wrap')
  const px = (v) => Math.round(v * 10) / 10
  const rect = (el) => {
    if (!el) return null
    const b = el.getBoundingClientRect()
    return { left: px(b.left), right: px(b.right), width: px(b.width) }
  }
  const padding = (el) => {
    if (!el) return null
    const st = getComputedStyle(el)
    return { left: st.paddingLeft, right: st.paddingRight }
  }
  const threadRect = rect(thread)
  const composerRect = rect(composer)
  return {
    messages: threadRect,
    composer: composerRect,
    dLeft: threadRect && composerRect ? px(composerRect.left - threadRect.left) : null,
    dRight: threadRect && composerRect ? px(composerRect.right - threadRect.right) : null,
    dWidth: threadRect && composerRect ? px(composerRect.width - threadRect.width) : null,
    messagesPadding: padding(messages),
    composerPadding: padding(wrap),
    overflow: {
      scrollWidth: document.documentElement.scrollWidth,
      viewport: window.innerWidth,
    },
    composerWrapMaxWidth: wrap ? getComputedStyle(wrap).maxWidth : null,
  }
}

mkdirSync(OUT, { recursive: true })
const browser = await chromium.launch()
const failures = []
const rows = []

for (const width of WIDTHS) {
  for (const kind of KINDS) {
    const label = `${width}-${kind}`
    const page = await browser.newPage({ viewport: { width, height: HEIGHT } })
    const consoleErrors = []
    page.on('console', (m) => { if (m.type() === 'error') consoleErrors.push(m.text()) })
    page.on('pageerror', (e) => consoleErrors.push(`pageerror ${e.message}`))
    await page.goto(URL, { waitUntil: 'domcontentloaded' })
    await page.waitForTimeout(1500)
    await openConversation(page, kind)
    await page.evaluate(() => {
      const list = document.querySelector('.hub-demo .messages')
      if (list) list.scrollTop = 0
    })
    await page.mouse.move(1, 1)
    await page.waitForTimeout(200)

    const rootSelector = kind === 'group' ? '.hub-group-view' : '.hub-dm'
    const m = await page.evaluate(measureColumns, rootSelector)
    await page.screenshot({ path: `${OUT}/${label}.png` })
    await page.close()

    if (m.missing) {
      failures.push(`${label}: 找不到 ${m.missing}`)
      continue
    }
    if (!m.messages || !m.composer) {
      failures.push(`${label}: 缺少 ${m.messages ? '.composer-inner' : '.messages-inner'}`)
      continue
    }
    rows.push({ label, ...m })

    if (Math.abs(m.dLeft) > TOLERANCE) failures.push(`${label}: 左边缘差 ${m.dLeft}px（>${TOLERANCE}）`)
    if (Math.abs(m.dRight) > TOLERANCE) failures.push(`${label}: 右边缘差 ${m.dRight}px（>${TOLERANCE}）`)
    if (m.overflow.scrollWidth > m.overflow.viewport) {
      failures.push(`${label}: 横向溢出 ${m.overflow.scrollWidth} > ${m.overflow.viewport}`)
    }
    // 1440：两者同为 760 且左缘相同（用户说的「群聊 composer 与消息列等宽」）。
    if (width === 1440) {
      if (m.messages.width !== 760) failures.push(`${label}: messages-inner 应为 760px（${m.messages.width}）`)
      if (m.composer.width !== 760) failures.push(`${label}: composer-inner 应为 760px（${m.composer.width}）`)
      if (m.messages.left !== m.composer.left) {
        failures.push(`${label}: 1440 左缘 x 应相同（${m.messages.left} vs ${m.composer.left}）`)
      }
      if (m.composerWrapMaxWidth !== 'none') {
        failures.push(`${label}: composer-wrap max-width=${m.composerWrapMaxWidth}（全局 800 未解除）`)
      }
    }
    // 断点口径：≤900 两侧都是 14，≤640 两侧都是 12。
    const want = width <= 640 ? '12px' : width <= 900 ? '14px' : '28px'
    for (const [where, pad] of [['消息区', m.messagesPadding], ['composer 外层', m.composerPadding]]) {
      if (!pad) { failures.push(`${label}: 取不到 ${where} 内边距`); continue }
      if (pad.left !== pad.right) failures.push(`${label}: ${where} 左右内边距不等（${pad.left}/${pad.right}）`)
      if (pad.left !== want) failures.push(`${label}: ${where} 左右内边距 ${pad.left}（应为 ${want}）`)
    }
    if (consoleErrors.length) failures.push(`${label}: console 报错 ${consoleErrors[0]}`)
  }
}

await browser.close()

// —— 报告 ——
const pad = (s, n) => String(s).padEnd(n)
console.log(`\n会话列对齐实测（${URL}）`)
console.log(`${pad('viewport', 10)}${pad('messages-inner', 30)}${pad('composer-inner', 30)}${pad('Δleft', 8)}${pad('Δright', 8)}Δwidth`)
for (const r of rows) {
  const cell = (x) => `${x.left} → ${x.right} (w ${x.width})`
  console.log(
    `${pad(r.label, 10)}${pad(cell(r.messages), 30)}${pad(cell(r.composer), 30)}` +
    `${pad(r.dLeft, 8)}${pad(r.dRight, 8)}${r.dWidth}`
  )
}
console.log(`\n截图：${OUT}`)

if (failures.length) {
  console.error(`\n❌ ${failures.length} 项不达标：`)
  for (const f of failures) console.error(`   · ${f}`)
  process.exit(1)
}
console.log('\n✅ 所有宽度：消息列与 composer 内层左右对齐（≤0.5px），0 横向溢出')
