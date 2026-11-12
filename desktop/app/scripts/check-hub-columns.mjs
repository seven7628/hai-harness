#!/usr/bin/env node
// 会话区「消息内容列 / composer 内层」列对齐实测（群 + DM）。
//
// 背景：用户报「会话栏没对齐、没居中」。三个根因分别只体现在**类名被两套 CSS 同时命中**、
// 断点值不一致、以及**滚动条 gutter 只在 classic 平台占位**上：
//   1. 全局 `.composer-wrap`（index.css）带 max-width:800 + margin:0 auto，把 demo 的
//      composer 外层收成 800，再被 28px 内边距压成 744；而 .messages-inner 是 760。
//      1440 下实测 messages-inner x=278 w=760 / composer-inner x=286 w=744。
//   2. ≤900 断点里消息区 14px、composer 12px（hub.css 与 demo.css 各一对），
//      于是 641-900 之间左右各差 2px；≤640 两边都是 12，反而正常。
//   3. `.messages` 是滚动容器，classic 占位滚动条从右侧吃掉内容盒 S px，
//      `.messages-inner` 的 margin:0 auto 整体左移 S/2，而 composer 没有滚动条，
//      于是左右各差 S/2（10px 条约 5px、15px 条约 7.5px）。headless macOS 是 overlay
//      条（宽度 0），默认模式测不到 —— 所以有 --classic-scrollbar 一档。
//
// 用法（需要 dev server；默认复用 5180，不占 5173）：
//   cd desktop/app && npx vite --port 5180          # 另开一个终端
//   node scripts/check-hub-columns.mjs --classic-scrollbar   # 模拟 10px 占位条
//   node scripts/check-hub-columns.mjs              # 默认 http://localhost:5180/
//   node scripts/check-hub-columns.mjs --url http://localhost:5181/ --out /tmp/hub-columns
//
// 判据（任一失败 → exit 1）：
//   · 每个宽度 × 每个视图，messages-inner 与 composer-inner 的左右边缘差 ≤0.5px（亚像素）；
//   · 1440 下群聊两者同为 760px 且 x 相同（classic 档同样 760：内容宽 770 → 760 列）；
//   · ≤900 消息区与 composer 外层左右内边距一致（14px），≤640 保持 12px；
//   · classic 档：两侧右内边距同为 28+10（消息区由占位条吃掉、composer 由变量补偿）；
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
// classic 档：headless macOS 没有占位条，用「内容盒少 S px」等价伪造 ——
// 真占位条也是从 padding box 右侧吃掉 S（内容起点仍是 padding-left），
// 所以 `.messages` 注 padding-right: 28+S 与真滚动条对内容盒的几何完全等价。
const CLASSIC = argv.includes('--classic-scrollbar')
const BAR = Number(arg('bar', '10'))
// 1440 覆盖「宽屏 760 列」；1024 是中间档；900 正好压在断点上；783 是断点内、非 640；
// 640 压在窄屏断点上（必须保持 12px 口径）。classic 档只跑 1440（宽档 28px 留白）。
const WIDTHS = CLASSIC ? [1440] : [1440, 1024, 900, 783, 640]
const HEIGHT = 900
const KINDS = ['group', 'dm']
const TOLERANCE = 0.5
const MODE = CLASSIC ? 'classic' : 'overlay'

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
// 侧栏行用 isVisible() 判断：窄窗抽屉收起/首屏没渲染时 count() 仍是 1，只查 count
// 会走到 click 上变成 30s 裸 Playwright 超时（N1）。
async function waitVisibleSideRow(page, timeoutMs = 10000) {
  const rows = page.locator('.hub-side-row')
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    if (await rows.first().isVisible().catch(() => false)) return true
    await page.waitForTimeout(200)
  }
  return false
}

async function openConversation(page, kind) {
  if (!(await waitVisibleSideRow(page, 300))) {
    const entry = page.locator('button[aria-label="智能体"]').first()
    if (!(await entry.count())) {
      throw new Error('找不到 Hub 入口（button[aria-label="智能体"]）；Hub 没打开？')
    }
    await entry.click({ timeout: 8000 }).catch(() => {})
    if (!(await waitVisibleSideRow(page, 10000))) {
      const total = await page.locator('.hub-side-row').count()
      throw new Error(`左栏没有可见的会话行（.hub-side-row 共 ${total} 个，均不可见）；Hub 首屏没渲染？`)
    }
  }
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
    const row = page.locator('.hub-side-row').filter({ hasText: 'Researcher' }).first()
    if (!(await row.isVisible().catch(() => false))) {
      throw new Error('左栏找不到可见的「Researcher」行；DM 用例需要这个智能体')
    }
    await row.click({ timeout: 8000 })
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

// classic 档：按「占位条抢走了内容盒右侧 S px」的等价几何伪造，并把补偿量钉在
// 视图根节点上（!important 压过 hook 内联值与 .hub-center 的默认值）。
async function injectClassicGutter(page, bar) {
  await page.addStyleTag({
    content: `
      .hub-demo .messages { padding-right: calc(28px + ${bar}px) !important; }
      .hub-group-view, .hub-dm { --hub-scrollbar-w: ${bar}px !important; }
    `,
  })
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
    scrollbarVar: getComputedStyle(root).getPropertyValue('--hub-scrollbar-w').trim(),
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
try {
  for (const width of WIDTHS) {
    for (const kind of KINDS) {
      const label = `${width}-${kind}${CLASSIC ? `/${MODE}` : ''}`
      const page = await browser.newPage({ viewport: { width, height: HEIGHT } })
      const consoleErrors = []
      page.on('console', (m) => { if (m.type() === 'error') consoleErrors.push(m.text()) })
      page.on('pageerror', (e) => consoleErrors.push(`pageerror ${e.message}`))
      await page.goto(URL, { waitUntil: 'domcontentloaded' })
      await page.waitForTimeout(1500)
      try {
        await openConversation(page, kind)
        if (CLASSIC) await injectClassicGutter(page, BAR)
        await page.evaluate(() => {
          const list = document.querySelector('.hub-demo .messages')
          if (list) list.scrollTop = 0
        })
        await page.mouse.move(1, 1)
        await page.waitForTimeout(200)
      } catch (e) {
        failures.push(`${label}: ${e?.message ?? e}`)
        await page.close()
        continue
      }

      const rootSelector = kind === 'group' ? '.hub-group-view' : '.hub-dm'
      const m = await page.evaluate(measureColumns, rootSelector)
      await page.screenshot({ path: `${OUT}/${label.replace(/\//g, '-')}.png` })
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
      // classic 档内容盒少 10px（770 → 760），inner 仍是 760，不放宽阈值。
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
      // 断点口径：≤900 两侧都是 14，≤640 两侧都是 12；classic 档右内边距 = 左 + S。
      const want = width <= 640 ? 12 : width <= 900 ? 14 : 28
      const wantRight = want + (CLASSIC ? BAR : 0)
      for (const [where, pad] of [['消息区', m.messagesPadding], ['composer 外层', m.composerPadding]]) {
        if (!pad) { failures.push(`${label}: 取不到 ${where} 内边距`); continue }
        if (pad.left !== `${want}px`) failures.push(`${label}: ${where} 左内边距 ${pad.left}（应为 ${want}px）`)
        if (pad.right !== `${wantRight}px`) failures.push(`${label}: ${where} 右内边距 ${pad.right}（应为 ${wantRight}px）`)
        if (!CLASSIC && pad.left !== pad.right) {
          failures.push(`${label}: ${where} 左右内边距不等（${pad.left}/${pad.right}）`)
        }
      }
      if (CLASSIC && m.scrollbarVar !== `${BAR}px`) {
        failures.push(`${label}: --hub-scrollbar-w=${m.scrollbarVar || '(空)'}（应为 ${BAR}px）`)
      }
      if (consoleErrors.length) failures.push(`${label}: console 报错 ${consoleErrors[0]}`)
    }
  }
} finally {
  // 任何断言/异常都要收掉 Chromium，避免残留进程（N4）。
  await browser.close().catch(() => {})
}

// —— 报告 ——
const pad = (s, n) => String(s).padEnd(n)
console.log(`\n会话列对齐实测（${URL}，模式 ${MODE}${CLASSIC ? ` · 模拟 ${BAR}px 占位条` : ''}）`)
console.log(`${pad('viewport', 18)}${pad('messages-inner', 30)}${pad('composer-inner', 30)}${pad('Δleft', 8)}${pad('Δright', 8)}Δwidth`)
for (const r of rows) {
  const cell = (x) => `${x.left} → ${x.right} (w ${x.width})`
  console.log(
    `${pad(r.label, 18)}${pad(cell(r.messages), 30)}${pad(cell(r.composer), 30)}` +
    `${pad(r.dLeft, 8)}${pad(r.dRight, 8)}${r.dWidth}`
  )
}
console.log(`\n截图：${OUT}`)

if (failures.length) {
  console.error(`\n❌ ${failures.length} 项不达标：`)
  for (const f of failures) console.error(`   · ${f}`)
  process.exit(1)
}
console.log(`\n✅ ${MODE}：消息列与 composer 内层左右对齐（≤0.5px），0 横向溢出`)
