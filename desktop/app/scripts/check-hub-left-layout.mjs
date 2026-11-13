#!/usr/bin/env node
// 左栏高度预算：Project 展开出长会话列表后，Hub 段（智能体/群组）不能被挤没。
//
// 用户报的现象：工作区展开一个很长的会话列表后，左栏「智能体/群组」只剩标题、
// 搜索框、搜索范围 tabs，连「智能体 N / 群组 N」两个 tab 都被裁掉，智能体行完全
// 看不见，而工作区列表反而占了大量高度。
//
// 根因（src/index.css 的 .proj-list）：flex-basis 原本是 auto，取的是**内容高度**。
// Project 展开后内容几千 px，负空间按 basis 分摊，本项触到 min-height:96px 后剩余
// 负空间全压给 .hub-side（flex:0 1 auto; min-height:0）→ Hub 段塌到连 tabs 都放不下。
// 修法：.proj-list 用 flex:1 1 0（basis 为 0，从 0 长起），超长时**滚动发生在这里**。
//
// 用法（默认复用 5180，不占 5173；另开终端起 dev server）：
//   cd desktop/app && npx vite --port 5180
//   node scripts/check-hub-left-layout.mjs
//   node scripts/check-hub-left-layout.mjs --url http://localhost:5192/
//   node scripts/check-hub-left-layout.mjs --no-shot        # 不写截图（默认落 /tmp）
//
// 判据（任一失败 → 打印实测值并 exit 1）：
//   1280×900：.hub-side-list ≥ 60、.hub-side-tabs ≥ 20、.proj-list 在滚动（scrollHeight > clientHeight）；
//   1280×620：.hub-side-list ≥ 40、.hub-side-tabs ≥ 18、.proj-list 仍在滚动；
//   两档都要求 .left 无横向溢出。
//
// 为什么注入 40 行而不等 mock 数据：mock 会话数受 mock.ts 控制（并行改动热点文件），
// 数据量一变这个挤压就复现不出来。这里直接把 40 个 .session-item 塞进 .ws-body，
// 只依赖布局（真 CSS + 真 flex 计算），跟数据源解耦、稳定复现。
import { chromium } from 'playwright'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const URL = arg('url', process.env.HUB_LEFT_URL || 'http://localhost:5180/')
const SHOT = !argv.includes('--no-shot')
const ROWS = 40
// 1280 是用户截图那档；620 是左栏最紧的短窗（Hub 段与工作区树同时要空间）。
const VIEWPORTS = [
  { width: 1280, height: 900, minList: 60, minTabs: 20 },
  { width: 1280, height: 620, minList: 40, minTabs: 18 },
]

// —— dev server preflight：没起 → exit 2（与「布局不达标」的 exit 1 区分开）——
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
  console.error('     node scripts/check-hub-left-layout.mjs --url http://localhost:5181/')
  process.exit(2)
}

// 等元素**可见**（不是 count>0）：左栏抽屉收起时 count 仍是 1，只查 count 会变成裸超时。
async function waitVisible(page, selector, timeoutMs = 15000) {
  const loc = page.locator(selector).first()
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    if (await loc.isVisible().catch(() => false)) return true
    await page.waitForTimeout(150)
  }
  return false
}

// 工作区树默认可能是收起的（.ws-body 只在 .ws-group.open 里渲染）：先展开再注入。
async function ensureWorkspaceOpen(page) {
  if (await page.locator('.ws-body').count()) return
  const head = page.locator('.ws-head').first()
  if (!(await head.count())) throw new Error('左栏没有 .ws-head（工作区列表没渲染？）')
  await head.click({ timeout: 8000 })
  await page.waitForTimeout(400)
}

// 注入长会话列表：克隆真行（.ws-body .session-item 带 16px 缩进），没有真行就造纯 div。
// 只加节点、不动 React 的 props，所以不会被下一次 render 认领回去。
const injectRows = (n) => {
  const body = document.querySelector('.ws-body')
  if (!body) return 0
  const proto = body.querySelector('.session-item')
  for (let i = 0; i < n; i++) {
    const row = proto ? proto.cloneNode(true) : document.createElement('div')
    row.className = 'session-item'
    row.removeAttribute('id')
    row.textContent = `注入的会话行 ${i + 1}`
    body.appendChild(row)
  }
  return body.querySelectorAll('.session-item').length
}

const measure = () => {
  const px = (v) => Math.round(v * 10) / 10
  const h = (sel) => {
    const el = document.querySelector(sel)
    return el ? px(el.getBoundingClientRect().height) : null
  }
  const proj = document.querySelector('.proj-list')
  const left = document.querySelector('.left')
  return {
    hubList: h('.hub-side-list'),
    hubTabs: h('.hub-side-tabs'),
    hubSide: h('.hub-side'),
    projList: h('.proj-list'),
    projScroll: proj ? { scrollHeight: proj.scrollHeight, clientHeight: proj.clientHeight } : null,
    rows: document.querySelectorAll('.ws-body .session-item').length,
    leftOverflow: left ? left.scrollWidth - left.clientWidth : null,
    docOverflow: document.documentElement.scrollWidth - window.innerWidth,
  }
}

const failures = []
const fmt = (m) =>
  `hub-list=${m.hubList} hub-tabs=${m.hubTabs} hub-side=${m.hubSide} ` +
  `proj-list=${m.projList} proj-scroll=${m.projScroll?.scrollHeight}/${m.projScroll?.clientHeight} ` +
  `rows=${m.rows} left-overflow=${m.leftOverflow}`

const browser = await chromium.launch()
const report = []
try {
  for (const vp of VIEWPORTS) {
    const page = await browser.newPage({ viewport: { width: vp.width, height: vp.height } })
    await page.goto(URL, { waitUntil: 'domcontentloaded' })
    await page.waitForTimeout(1800)
    if (await page.locator('.left.collapsed').count()) {
      await page.locator('button[aria-label="左栏收缩 / ⌘B"]').first().click({ timeout: 5000 }).catch(() => {})
      await page.waitForTimeout(300)
    }
    if (!(await waitVisible(page, '.hub-side'))) throw new Error('等不到 .hub-side（左栏 Hub 段没渲染）')
    await ensureWorkspaceOpen(page)
    if (!(await waitVisible(page, '.ws-body'))) throw new Error('等不到 .ws-body（工作区展开后仍没有会话列表）')

    // 注入 → 量。mock 偶尔会在注入后推一次会话列表把节点冲掉：重试到 40 行齐为止。
    let m = null
    for (let attempt = 0; attempt < 3; attempt++) {
      await page.evaluate(injectRows, ROWS)
      await page.waitForTimeout(350)
      m = await page.evaluate(measure)
      if (m.rows >= ROWS) break
    }
    report.push(`${vp.width}×${vp.height} ${fmt(m)}`)

    const tag = `${vp.width}×${vp.height}`
    if (m.rows < ROWS) failures.push(`${tag} 注入后只有 ${m.rows} 行（<${ROWS}），挤压没复现出来`)
    if (m.hubList === null || m.hubList < vp.minList) failures.push(`${tag} .hub-side-list 高度 ${m.hubList} < ${vp.minList}`)
    if (m.hubTabs === null || m.hubTabs < vp.minTabs) failures.push(`${tag} .hub-side-tabs 高度 ${m.hubTabs} < ${vp.minTabs}`)
    if (!m.projScroll || m.projScroll.scrollHeight <= m.projScroll.clientHeight) {
      failures.push(`${tag} .proj-list 没在滚动（scrollHeight ${m.projScroll?.scrollHeight} <= clientHeight ${m.projScroll?.clientHeight}）`)
    }
    if (m.leftOverflow === null || m.leftOverflow > 1) failures.push(`${tag} .left 横向溢出 ${m.leftOverflow}px`)
    if (m.docOverflow > 1) failures.push(`${tag} 文档横向溢出 ${m.docOverflow}px`)

    if (SHOT) await page.screenshot({ path: `/tmp/hub-left-layout-${vp.height}.png` }).catch(() => {})
    await page.close()
  }
} catch (e) {
  failures.push(`异常：${e?.message || e}`)
} finally {
  await browser.close()
}

if (failures.length) {
  console.error(`❌ 左栏 Hub 段被挤（${URL}）`)
  for (const line of report) console.error(`   实测 ${line}`)
  for (const f of failures) console.error(`   · ${f}`)
  process.exit(1)
}

console.log(`✅ 左栏高度预算正常（${URL}）：${report.join(' | ')}${SHOT ? '（截图 /tmp/hub-left-layout-*.png）' : ''}`)
