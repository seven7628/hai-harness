#!/usr/bin/env node
// 会话区 / 右栏色板统一（P6-14）浏览器验收。
//
// 背景：demo.css 自带一套暖色板，会话区/header/composer 吃 --chat（深色 #0d0d0c），
// 右栏 .hub-inspector 吃 app 的 --paper-2（#231d1a）—— 中间比右栏暗一整档，浅色也
// 一样（#f8f4eb vs #ffffff）。「拼起来的界面」只有真浏览器 + 真 CSS 能判定：这里读的是
// getComputedStyle 的最终颜色，不是源码里的字符串。
//
// 判据（三套调色板 default/ink/paper × 1280/640，任一失败 → exit 1）：
//   · 会话区底色 === 右栏底色（同一个 token：--paper-2）；
//   · header 底 = 同色半透明（滚动时仍模糊，但不再是另一套颜色），header 分隔线 = --ink-line；
//   · composer 外层 === 会话区底色；composer 输入框 = --paper-3（比底亮一档，看得见）；
//   · 正文文字 = --ink（不再是 demo 的暖白）；
//   · 无横向溢出（documentElement 与状态条自身）；
//   · P6-16 的头部层级没被新增的状态条破坏（header 仍是 position:relative + z-index
//     非 auto + isolation:isolate，且「更多操作」菜单溢出到状态条/消息区的位置仍命中菜单）；
//   · 状态条只有一条（信息密度与 P6-10 合并，群头不重复轮次文案）。
//
// 用法（dev server 用 525x，别占 5173/5180）：
//   cd desktop/app && npx vite --port 5258          # 另开一个终端
//   node scripts/check-hub-theme-palette.mjs                    # 默认 http://localhost:5258/
//   node scripts/check-hub-theme-palette.mjs --url http://localhost:5257/
//
// dev server 没起 → exit 2（与「色板不达标」的 exit 1 区分开）。
import { chromium } from 'playwright'
import { mkdirSync } from 'node:fs'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const URL = arg('url', process.env.HUB_PALETTE_URL || 'http://localhost:5258/')
const OUT = arg('out', '/tmp/hub-theme-palette-shots')
const GROUP_NAME = '场景演示 · 六项能力'

// 三套调色板 = index.css 的三条根规则。应用里 dark→ink、light→paper、system→跟随系统
// 解析成 ink/paper，所以「无属性（默认）/ink/paper」跑一遍就覆盖全部绘制路径。
const PALETTES = [
  { name: 'default', attr: null },
  { name: 'ink', attr: 'ink' },
  { name: 'paper', attr: 'paper' },
]
const WIDTHS = [1280, 640]

let pass = 0
let failed = 0
const check = (ok, msg) => {
  if (ok) { pass++; console.log(`  ✅ ${msg}`) } else { failed++; console.log(`  ❌ ${msg}`) }
}

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
  console.error('   先另开一个终端起 dev server（不要占 5173/5180）：')
  console.error('     cd desktop/app && npx vite --port 5258')
  process.exit(2)
}

mkdirSync(OUT, { recursive: true })

const browser = await chromium.launch()
const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
const shot = async (name) => {
  await page.screenshot({ path: `${OUT}/${name}.png`, fullPage: false })
  console.log(`  📷 ${OUT}/${name}.png`)
}

async function setPalette(attr) {
  await page.evaluate((theme) => {
    if (theme) document.documentElement.setAttribute('data-theme', theme)
    else document.documentElement.removeAttribute('data-theme')
  }, attr)
  await page.waitForTimeout(150)
}

async function openGroup() {
  const rows = page.locator('.hub-side-row')
  const deadline = Date.now() + 15000
  while (Date.now() < deadline && !(await rows.first().isVisible().catch(() => false))) {
    await page.waitForTimeout(200)
  }
  const tab = page.locator('.hub-side-tabs button').filter({ hasText: '群组' }).first()
  if (await tab.count()) {
    if ((await tab.getAttribute('aria-selected')) !== 'true') await tab.click({ timeout: 6000 })
    await page.waitForTimeout(250)
  }
  const row = page.locator('.hub-side-row').filter({ hasText: GROUP_NAME }).first()
  await row.waitFor({ timeout: 10000 })
  await row.click({ timeout: 8000 })
  await page.locator('.hub-group-view').waitFor({ timeout: 10000 })
  await page.waitForTimeout(700)
}

// 右栏展开到「概览/上下文」：色板判据要拿 .hub-inspector 的底色做基准。
async function expandInspector() {
  await page.evaluate(async () => {
    const app = window.__store
    app?.getState?.()?.setRightExpanded?.(true)
    const m = await import('/src/hub/useHubStore.ts')
    m.useHubStore.getState().setInspector?.('context')
  })
  await page.waitForTimeout(500)
}

// 把 CSS 变量的最终值解析成 rgb（用探针元素，避免自己解析 color-mix/var 链）。
const resolveColors = () => page.evaluate(() => {
  const probe = document.createElement('div')
  probe.style.cssText = 'position:absolute;width:0;height:0;pointer-events:none'
  document.body.appendChild(probe)
  const resolve = (value) => {
    probe.style.color = ''
    probe.style.color = value
    return getComputedStyle(probe).color
  }
  const out = {
    paper: resolve('var(--paper)'),
    paper2: resolve('var(--paper-2)'),
    paper3: resolve('var(--paper-3)'),
    ink: resolve('var(--ink)'),
    inkLine: resolve('var(--ink-line)'),
  }
  probe.remove()
  return out
})

const measure = () => page.evaluate(() => {
  const bg = (sel) => {
    const el = document.querySelector(sel)
    return el ? getComputedStyle(el).backgroundColor : null
  }
  const header = document.querySelector('.hub-group-view .chat-header')
  const hs = header ? getComputedStyle(header) : null
  const bar = document.querySelector('.hub-group-view .hub-conv-bar')
  const chatSub = document.querySelector('.hub-group-view .chat-sub')
  return {
    chat: bg('.hub-group-view'),
    header: bg('.hub-group-view .chat-header'),
    headerBorder: hs ? hs.borderBottomColor : null,
    headerPosition: hs?.position ?? '',
    headerZ: hs?.zIndex ?? '',
    headerIsolation: hs?.isolation ?? '',
    composerWrap: bg('.hub-group-view .composer-wrap'),
    composer: bg('.hub-group-view .composer'),
    textColor: (() => { const el = document.querySelector('.hub-group-view'); return el ? getComputedStyle(el).color : null })(),
    inspector: bg('.hub-inspector'),
    inspectorCollapsed: !!document.querySelector('.hub-detail-rail'),
    barCount: document.querySelectorAll('.hub-group-view .hub-conv-bar').length,
    chatSubText: (chatSub?.textContent ?? '').trim(),
    docOverflow: document.documentElement.scrollWidth - window.innerWidth,
    barOverflow: bar ? bar.scrollWidth - bar.clientWidth : 0,
    demoTheme: document.querySelector('.hub-group-view')?.getAttribute('data-theme') ?? null,
  }
})

// rgba(...) / color(srgb r g b / a) 都归一成 [r,g,b,a]（0-255 + 0-1），用来比「同色半透明」。
function parseColor(value) {
  if (!value) return null
  const rgba = value.match(/rgba?\(([^)]+)\)/)
  if (rgba) {
    const parts = rgba[1].split(/[,/]/).map((x) => parseFloat(x.trim()))
    return [parts[0], parts[1], parts[2], parts.length > 3 ? parts[3] : 1]
  }
  const srgb = value.match(/color\(srgb\s+([^)]+)\)/)
  if (srgb) {
    const parts = srgb[1].split(/[\s/]+/).filter(Boolean).map(Number)
    return [Math.round(parts[0] * 255), Math.round(parts[1] * 255), Math.round(parts[2] * 255), parts.length > 3 ? parts[3] : 1]
  }
  return null
}
const sameRGB = (a, b) => {
  const x = parseColor(a)
  const y = parseColor(b)
  if (!x || !y) return false
  return Math.abs(x[0] - y[0]) <= 1 && Math.abs(x[1] - y[1]) <= 1 && Math.abs(x[2] - y[2]) <= 1
}

try {
  await page.goto(URL, { waitUntil: 'domcontentloaded' })
  await openGroup()
  await expandInspector()

  for (const width of WIDTHS) {
    await page.setViewportSize({ width, height: width === 640 ? 760 : 900 })
    await page.waitForTimeout(300)
    await expandInspector()
    for (const palette of PALETTES) {
      const label = `${palette.name}@${width}`
      console.log(`\n[${label}]`)
      await setPalette(palette.attr)
      const tokens = await resolveColors()
      const m = await measure()
      const conv = m.chat
      const inspector = m.inspector

      // 1) 会话区与右栏同一个 token（--paper-2）
      check(sameRGB(conv, tokens.paper2), `会话区底 = --paper-2（got=${conv} / paper2=${tokens.paper2}）`)
      if (inspector) {
        check(sameRGB(conv, inspector), `会话区底 === 右栏 .hub-inspector 底（${conv} vs ${inspector}）`)
      } else {
        check(false, `右栏未展开（.hub-inspector 不存在，collapsed=${m.inspectorCollapsed}）`)
      }

      // 2) header：同色半透明 + app 发丝线
      const hdr = parseColor(m.header)
      check(!!hdr && sameRGB(m.header, conv) && hdr[3] > 0.5 && hdr[3] < 1,
        `header 底 = 会话区同色半透明（got=${m.header} alpha=${hdr?.[3]}）`)
      check(sameRGB(m.headerBorder, tokens.inkLine), `header 分隔线 = --ink-line（got=${m.headerBorder}）`)

      // 3) composer：外层跟会话区同色；输入框比底亮一档（--paper-3），不能糊成一片
      check(sameRGB(m.composerWrap, conv), `composer 外层 = 会话区底（got=${m.composerWrap}）`)
      check(sameRGB(m.composer, tokens.paper3), `composer 输入框 = --paper-3（got=${m.composer}）`)
      check(!sameRGB(m.composer, conv), 'composer 输入框与底不同色（有边界，不是一片平底）')

      // 4) 文字：app 的 --ink（不是 demo 的暖白）
      check(sameRGB(m.textColor, tokens.ink), `正文文字 = --ink（got=${m.textColor}）`)

      // 5) 布局：无横向溢出
      check(m.docOverflow <= 0, `无横向溢出（${m.docOverflow}px）`)
      check(m.barOverflow <= 1, `状态条自身不溢出（${m.barOverflow}px）`)

      // 6) P6-16：头部层级没被新增的状态条破坏
      check(m.headerPosition === 'relative' || m.headerPosition === 'absolute', `header 有定位上下文（${m.headerPosition}）`)
      check(m.headerZ !== 'auto' && m.headerZ !== '', `header z-index 非 auto（${m.headerZ}）`)
      check(m.headerIsolation === 'isolate', `header isolation=isolate（${m.headerIsolation}）`)

      // 7) 信息只有一套：状态条一条、群头不再重复轮次文案
      check(m.barCount === 1, `状态条只有一条（got=${m.barCount}）`)
      check(!/第\s*\d+\s*轮/.test(m.chatSubText), `群头不重复轮次文案（got=${JSON.stringify(m.chatSubText)}）`)

      // 8) 「更多操作」菜单溢出到状态条/消息区的位置仍命中菜单（新增兄弟节点的回归点）
      await page.locator('.hub-group-view .chat-header button[aria-label="更多操作"]').first().click({ timeout: 6000 })
      await page.locator('.hub-group-view .header-menu').first().waitFor({ timeout: 4000 })
      await page.waitForTimeout(120)
      const hit = await page.evaluate(() => {
        const el = document.querySelector('.hub-group-view .header-menu')
        if (!el) return { ok: false, why: 'no menu' }
        const r = el.getBoundingClientRect()
        const x = Math.round(r.left + r.width * 0.5)
        const y = Math.round(r.top + r.height * 0.95)
        const top = document.elementFromPoint(x, y)
        return { ok: !!top?.closest('.header-menu'), why: top ? `${top.tagName.toLowerCase()}.${String(top.className || '').split(' ')[0]}` : 'null' }
      })
      check(hit.ok, `菜单溢出部分仍命中菜单（got=${hit.why}）`)
      await page.keyboard.press('Escape')
      await page.waitForTimeout(150)

      await shot(`conv-palette-${palette.name}-${width}`)
    }
  }
  // —— 5) system 偏好：解析成 ink/paper 后，会话区必须跟着换（不能停在启动时的值）——
  console.log('\n[system → ink/paper]')
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.waitForTimeout(300)
  await page.evaluate(() => document.documentElement.removeAttribute('data-theme'))
  await page.evaluate(() => window.__store?.getState?.()?.setTheme?.('system'))
  for (const scheme of ['light', 'dark']) {
    await page.emulateMedia({ colorScheme: scheme })
    await page.waitForTimeout(350)
    const st = await page.evaluate(() => ({
      demo: document.querySelector('.hub-group-view')?.getAttribute('data-theme') ?? null,
      root: document.documentElement.getAttribute('data-theme') ?? null,
    }))
    const want = scheme === 'light' ? 'paper' : 'ink'
    check(st.root === want && st.demo === (want === 'paper' ? 'light' : 'dark'),
      `system + prefers-color-scheme:${scheme} → data-theme=${want}（root=${st.root} demo=${st.demo}）`)
    const tokens = await resolveColors()
    const m = await measure()
    check(sameRGB(m.chat, tokens.paper2), `system/${scheme}：会话区底 = --paper-2（got=${m.chat}）`)
    if (m.inspector) check(sameRGB(m.chat, m.inspector), `system/${scheme}：与右栏同色（${m.chat} vs ${m.inspector}）`)
    await shot(`conv-palette-system-${scheme}`)
  }
  // 收尾：把偏好还原成默认（不影响下一次运行，只是别把 dev 页面留在 system）。
  await page.evaluate(() => window.__store?.getState?.()?.setTheme?.('dark'))
} catch (e) {
  failed++
  console.error(`\n❌ 断言过程抛错：${e?.message ?? e}`)
} finally {
  await browser.close()
}

console.log(`\n${failed === 0 ? '✅' : '❌'} 会话区/右栏色板：${pass} 项通过 / ${failed} 项失败`)
process.exit(failed === 0 ? 0 : 1)
