#!/usr/bin/env node
// 头部「更多操作」菜单的层级与交互（P6-16）。
//
// 前置（**不要用 5173/5180**）：
//   VITE_PORT=5221 npx vite
// 运行：
//   node scripts/check-hub-menu-layer.mjs [--url http://localhost:5221/]
//
// 为什么要单独一组浏览器断言：菜单被消息区盖住是**绘制顺序**问题，jsdom/Node
// 测不出来 —— 只有真浏览器 + 真 CSS 的 elementFromPoint 能判定「点下去落在谁身上」。
//
// 断言（每种调色板 × 1280/640）：
//   · .chat-header 显式成层：position:relative、z-index 非 auto、isolation:isolate
//   · 菜单确实溢出头部（菜单底 > header 底），且溢出部分的命中测试落在菜单内
//     —— 这正是「下层表格/消息文字透出」的复现条件
//   · 菜单几何不塌（宽高 > 0）、不被裁切
//   · Esc 关闭并把焦点还给触发按钮；外点关闭；方向键在菜单项间移动；Enter 能激活
import { chromium } from 'playwright'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const BASE_URL = arg('url', process.env.HUB_MENU_URL || 'http://localhost:5221/')

const failed = []
const errors = []
const check = (cond, msg) => { if (!cond) failed.push(msg) }
const log = (m) => console.log(`  ✅ ${m}`)

// 三套调色板：index.css 的三条根规则（:root 默认 / [data-theme=ink] / [data-theme=paper]）。
// 应用里 theme 偏好 dark→ink、light→paper、system→跟随系统解析成两者之一，
// 所以把「无属性（默认）/ink/paper」都跑一遍 = 覆盖全部绘制路径。
const PALETTES = [
  { name: 'default', attr: null },
  { name: 'ink', attr: 'ink' },
  { name: 'paper', attr: 'paper' },
]

const browser = await chromium.launch()
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()) })
page.on('pageerror', (e) => errors.push(`pageerror ${e.message}`))

async function setPalette(attr) {
  await page.evaluate((theme) => {
    if (theme) document.documentElement.setAttribute('data-theme', theme)
    else document.documentElement.removeAttribute('data-theme')
  }, attr)
  await page.waitForTimeout(80)
}

async function openHub() {
  await page.goto(BASE_URL, { waitUntil: 'domcontentloaded' })
  await page.waitForTimeout(1400)
  if (!(await page.locator('.hub-demo').count())) {
    await page.locator('button[aria-label="智能体"]').first().click({ timeout: 8000 }).catch(() => {})
    await page.waitForTimeout(700)
  }
  await page.locator('.hub-side-row').first().waitFor({ timeout: 10000 })
}

// 两种头部都要验：群聊（GroupView）与智能体单聊（HubSurface）各有一份「更多操作」，
// 层级与交互必须一致（它们共用 .chat-header 的层与 useHeaderMenu 的行为）。
async function openScope(kind) {
  await openHub()
  const tab = page.locator('.hub-side-tabs button').filter({ hasText: kind === 'group' ? '群组' : '智能体' }).first()
  if ((await tab.getAttribute('aria-selected')) !== 'true') await tab.click({ timeout: 6000 })
  await page.waitForTimeout(250)
  await page.locator('.hub-side-row').first().click({ timeout: 8000 })
  await page.waitForTimeout(700)
  await page.locator('.hub-demo .chat-header').first().waitFor({ timeout: 8000 })
}

const trigger = () => page.locator('.hub-demo .chat-header button[aria-label="更多操作"]').first()
const menu = () => page.locator('.hub-demo .header-menu').first()

async function openMenu() {
  await trigger().click({ timeout: 6000 })
  await menu().waitFor({ timeout: 4000 })
  await page.waitForTimeout(120)
}

// 几何 + 命中测试：菜单下半部分（与消息区重叠的位置）必须命中菜单自己。
async function probeLayer() {
  return page.evaluate(() => {
    const header = document.querySelector('.hub-demo .chat-header')
    const el = document.querySelector('.hub-demo .header-menu')
    const messages = document.querySelector('.hub-demo .messages')
    if (!header || !el) return { missing: true }
    const hs = getComputedStyle(header)
    const hr = header.getBoundingClientRect()
    const mr = el.getBoundingClientRect()
    const probes = []
    // 取菜单横向三个位置 × 纵向（顶部 / 中部 / 底部下方 4px），底部那批就是原先被盖住的地方。
    for (const fx of [0.2, 0.5, 0.8]) {
      for (const fy of [0.1, 0.5, 0.95]) {
        const x = Math.round(mr.left + mr.width * fx)
        const y = Math.round(mr.top + mr.height * fy)
        if (x < 0 || y < 0 || x >= window.innerWidth || y >= window.innerHeight) continue
        const hit = document.elementFromPoint(x, y)
        probes.push({
          x, y,
          inMenu: !!hit?.closest('.header-menu'),
          hit: hit ? `${hit.tagName.toLowerCase()}.${String(hit.className || '').split(' ')[0]}` : 'null',
        })
      }
    }
    return {
      headerPosition: hs.position,
      headerZ: hs.zIndex,
      headerIsolation: hs.isolation,
      headerBottom: Math.round(hr.bottom),
      menuTop: Math.round(mr.top),
      menuBottom: Math.round(mr.bottom),
      menuWidth: Math.round(mr.width),
      menuHeight: Math.round(mr.height),
      menuItems: el.querySelectorAll('[role="menuitem"],[role="menuitemradio"]').length,
      messagesHasText: (messages?.textContent || '').trim().length > 0,
      viewport: { w: window.innerWidth, h: window.innerHeight },
      probes,
    }
  })
}

const KINDS = [
  { kind: 'group', runSelector: '.hub-demo .hub-run' },
  { kind: 'dm', runSelector: '.hub-demo .messages.dm.run' },
]

for (const { kind, runSelector } of KINDS) {
  for (const width of [1280, 640]) {
    for (const palette of PALETTES) {
    const label = `${kind}:${palette.name}@${width}`
    await page.setViewportSize({ width, height: width === 640 ? 760 : 900 })
    await openScope(kind)
    await setPalette(palette.attr)
    await openMenu()

    const g = await probeLayer()
    check(!g.missing, `[${label}] 头部/菜单都存在`)
    if (g.missing) continue

    // 1) header 显式成层（不是靠把菜单 z-index 调大碰运气）
    check(g.headerPosition === 'relative' || g.headerPosition === 'absolute', `[${label}] header 有定位上下文（${g.headerPosition}）`)
    check(g.headerZ !== 'auto' && g.headerZ !== '', `[${label}] header z-index 非 auto（${g.headerZ}）`)
    check(g.headerIsolation === 'isolate', `[${label}] header isolation=isolate（${g.headerIsolation}）`)

    // 2) 菜单几何：溢出头部且不塌
    check(g.menuWidth > 100 && g.menuHeight > 60, `[${label}] 菜单几何正常（${g.menuWidth}×${g.menuHeight}）`)
    // 菜单锚在触发按钮下方：起点落在 header 内（top:38px + header-actions 居中），
    // 底部溢出到消息区 —— 那一段正是原先被消息/表格盖住的地方。
    check(g.menuTop > 0 && g.menuTop < g.headerBottom, `[${label}] 菜单锚在 header 内（menuTop=${g.menuTop} headerBottom=${g.headerBottom}）`)
    check(g.menuBottom > g.headerBottom, `[${label}] 菜单确实溢出到消息区（复现条件）`)
    check(g.menuItems >= 3, `[${label}] 菜单项渲染完整（${g.menuItems} 项）`)

    // 3) 命中测试：菜单覆盖的每个点都必须落在菜单内（下层消息/表格不能透出）
    check(g.probes.length >= 6, `[${label}] 取样点足够（${g.probes.length}）`)
    for (const p of g.probes) {
      check(p.inMenu, `[${label}] (${p.x},${p.y}) 命中菜单而不是下层内容（got ${p.hit}）`)
    }
    check(g.messagesHasText, `[${label}] 消息区有内容（层级断言才有意义）`)
    log(`${label}: header z=${g.headerZ} menu ${g.menuTop}-${g.menuBottom} (header 底 ${g.headerBottom}) 命中 ${g.probes.length} 点`)

    // 4) Esc 关闭 + 焦点回到触发按钮
    await page.keyboard.press('Escape')
    await page.waitForTimeout(150)
    check((await page.locator('.hub-demo .header-menu').count()) === 0, `[${label}] Esc 关闭菜单`)
    const focusLabel = await page.evaluate(() => document.activeElement?.getAttribute?.('aria-label') || '')
    check(focusLabel === '更多操作', `[${label}] Esc 后焦点回到触发按钮（got ${focusLabel || 'none'}）`)

    // 5) 外点关闭
    await openMenu()
    await page.locator('.hub-demo .messages').first().click({ position: { x: 6, y: 6 }, timeout: 4000 }).catch(() => {})
    await page.waitForTimeout(200)
    check((await page.locator('.hub-demo .header-menu').count()) === 0, `[${label}] 点菜单外面关闭`)

    // 6) 键盘：打开即聚焦首项，方向键移动，Enter 激活（切到 Run 视图）
    await openMenu()
    const inMenu = await page.evaluate(() => !!document.activeElement?.closest?.('.header-menu'))
    check(inMenu, `[${label}] 打开后焦点进入菜单`)
    const firstText = await page.evaluate(() => document.activeElement?.textContent || '')
    await page.keyboard.press('ArrowDown')
    const secondText = await page.evaluate(() => document.activeElement?.textContent || '')
    check(!!secondText && secondText !== firstText, `[${label}] ArrowDown 移动焦点（${firstText} → ${secondText}）`)
    // 回到第一项（Chat）并 Enter 激活：视图切到 Run 再切回来。
    await page.keyboard.press('Home')
    await page.keyboard.press('ArrowDown')
    await page.keyboard.press('Enter')
    await page.waitForTimeout(300)
    const runView = await page.locator(runSelector).count()
    check(runView > 0, `[${label}] Enter 激活菜单项（切到 Run 视图）`)
    check((await page.locator('.hub-demo .header-menu').count()) === 0, `[${label}] 激活后菜单关闭`)
    // 复位到 Chat，避免影响下一轮。
    await openMenu()
    await page.keyboard.press('Home')
    await page.keyboard.press('Enter')
    await page.waitForTimeout(200)
    }
  }
}

await browser.close()

if (errors.length) failed.push(`console/page errors: ${errors.slice(0, 3).join(' | ')}`)
if (failed.length) {
  console.error(`\n❌ HUB_MENU_LAYER_TEST_FAIL (${failed.length})`)
  for (const f of failed) console.error(`   - ${f}`)
  process.exit(1)
}
console.log('\nHUB_MENU_LAYER_TEST_PASS')
