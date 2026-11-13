#!/usr/bin/env node
// Hub 会话行头像的性能回归（浏览器）：一条长会话里有上百颗 52px Luna 头像，
// 引擎 30fps 无条件遍历 scene.faces 并写 transform + d，agent 一运行所有历史行还会
// 同时跑 CSS 呼吸动画。本脚本守住三条不再回退的约束：
//
//   1. 消息行头像 `data-live="off"`（宿主 span 与内部 svg 都写），header 头像保持默认 live；
//   2. `data-live=off` 的头像不跑呼吸动画（即使 data-state=working），header 仍跑；
//   3. 离屏头像被 IntersectionObserver 置 `svg.__lunaFace.visible=false`，滚回变 true；
//   4. 390 宽不横向溢出；console error / pageerror 0。
//
// 用法（默认自起 dev server；5180，不占 5173）：
//   node scripts/check-hub-avatar-perf.mjs
//   node scripts/check-hub-avatar-perf.mjs --url http://localhost:5201/   # 复用已起的
//   node scripts/check-hub-avatar-perf.mjs --no-spawn --port 5180
import { chromium } from 'playwright'
import { spawn } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const PORT = Number(arg('port', process.env.HUB_AVATAR_PORT || '5180'))
const URL_ARG = arg('url', '')
const BASE = URL_ARG || `http://localhost:${PORT}/`
const NO_SPAWN = argv.includes('--no-spawn')

const failed = []
const check = (cond, msg) => { if (!cond) failed.push(msg) }

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

let server = null
async function ensureServer() {
  if (!(await preflight(BASE))) return
  if (NO_SPAWN) {
    console.error(`❌ dev server 不可用：${BASE}`)
    console.error(`   先起：cd desktop/app && npx vite --port ${PORT} --strictPort`)
    process.exit(2)
  }
  const appDir = fileURLToPath(new URL('..', import.meta.url))
  // 直接跑 node_modules/.bin/vite：经 npx 会多一层 npm exec 进程，SIGTERM 只杀到壳。
  server = spawn(join(appDir, 'node_modules', '.bin', 'vite'), ['--port', String(PORT), '--strictPort'], {
    cwd: appDir, stdio: 'ignore',
  })
  for (let i = 0; i < 120; i++) {
    await new Promise((r) => setTimeout(r, 500))
    if (!(await preflight(BASE))) return
  }
  console.error(`❌ 自起 dev server 超时（${BASE}）`)
  process.exit(2)
}

await ensureServer()
const stopServer = () => { try { server?.kill('SIGTERM') } catch { /* 已退出 */ } }
process.on('exit', stopServer)
process.on('SIGINT', () => { stopServer(); process.exit(130) })

const errors = []
const browser = await chromium.launch()
const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()) })
page.on('pageerror', (e) => errors.push(`pageerror ${e.message}`))

// —— 打开 App → Hub → 左栏「智能体」页签 → 第一行 DM ——
async function openApp() {
  for (let i = 0; i < 4; i++) {
    await page.goto(`${BASE}?hubFixture=dmstream`, { waitUntil: 'domcontentloaded' })
    await page.waitForTimeout(1500)
    if (await page.locator('.hub-side-row').count()) return
    if (!(await page.locator('button[aria-label="智能体"]').count())) continue
    await page.locator('button[aria-label="智能体"]').first().click({ timeout: 8000 }).catch(() => {})
    await page.waitForTimeout(700)
    if (await page.locator('.hub-side-row').count()) return
  }
  throw new Error('页面没挂载出 Hub 左栏（dev server 未就绪 / 504 Outdated Optimize Dep）')
}

await openApp()
await page.locator('.hub-side-row').first().waitFor({ timeout: 10000 })
const tab = page.locator('.hub-side-tabs button').filter({ hasText: '智能体' }).first()
if ((await tab.getAttribute('aria-selected')) !== 'true') await tab.click({ timeout: 6000 })
await page.waitForTimeout(250)
await page.locator('.hub-side-row').first().click({ timeout: 8000 })
await page.waitForTimeout(800)
const composer = page.locator('.hub-demo .composer textarea').first()
await composer.waitFor({ timeout: 8000 })
await page.locator('.hub-dm .messages').first().waitFor({ timeout: 8000 })

// —— 1) data-live：消息行 off / header 保持 live（宿主 + 内部 svg + 引擎 face）——
const attrs = () => page.evaluate(() => {
  const rows = Array.from(document.querySelectorAll('.hub-dm .msg-row.agent .luna-avatar'))
  const head = document.querySelector('.chat-header .luna-avatar')
  const probe = (el) => {
    if (!el) return null
    const svg = el.querySelector('svg[data-luna-face]')
    return {
      host: el.getAttribute('data-live'),
      svg: svg?.getAttribute('data-live') ?? null,
      face: svg?.__lunaFace ? svg.__lunaFace.live === true : null,
    }
  }
  return { rows: rows.map(probe), header: probe(head), rowCount: rows.length }
})

const a0 = await attrs()
check(a0.rowCount > 0, `会话里有 agent 消息行（got=${a0.rowCount}）`)
const badHost = a0.rows.filter((r) => r.host !== 'off').length
const badSvg = a0.rows.filter((r) => r.svg !== 'off').length
const badFace = a0.rows.filter((r) => r.face !== false).length
check(badHost === 0, `消息行头像宿主都带 data-live="off"（${badHost}/${a0.rowCount} 不合规）`)
check(badSvg === 0, `消息行头像内部 svg 都带 data-live="off"（${badSvg}/${a0.rowCount} 不合规）`)
// 引擎在 makeFace 里读 svg dataset：off 必须落到 face.live=false（否则随机换风格还在跑）。
check(badFace === 0, `引擎 face.live=false（${badFace}/${a0.rowCount} 不合规）`)
check(a0.header?.host === null, `header 头像不带 data-live（got=${a0.header?.host}）`)
check(a0.header?.svg === null, `header 头像内部 svg 不带 data-live（got=${a0.header?.svg}）`)
check(a0.header?.face === true, `header 头像引擎侧仍 live（got=${a0.header?.face}）`)

// —— 2) CSS：off 头像不跑呼吸动画，header 照跑（全局规则没被改坏）——
// 用克隆探针强制 data-state=working：只验级联，不动引擎、不碰 React 管的节点。
const anim = await page.evaluate(() => {
  const name = (el) => {
    const svg = el?.querySelector('svg[data-luna-face]')
    return svg ? getComputedStyle(svg).animationName : null
  }
  const row = document.querySelector('.hub-dm .msg-row.agent .luna-avatar')
  const head = document.querySelector('.chat-header .luna-avatar')
  const force = (el) => {
    if (!el || !el.parentElement) return null
    const probe = el.cloneNode(true)
    probe.dataset.state = 'working'
    el.parentElement.appendChild(probe)
    const v = name(probe)
    probe.remove()
    return v
  }
  return { rowWorking: force(row), headWorking: force(head), rowIdle: name(row) }
})
check(anim.rowWorking === 'none', `data-live=off + working 的头像不跑 CSS 动画（animationName=${anim.rowWorking}）`)
check(anim.headWorking === 'av-work', `header 头像 working 仍跑 av-work（animationName=${anim.headWorking}）`)
check(anim.rowIdle === 'none', `data-live=off 头像默认无动画（animationName=${anim.rowIdle}）`)

// —— 3) 流式期间：草稿行头像同样是 off 且不动画（bug 的原始现场：agent 一运行）——
await composer.fill('跑一下价格对比')
await composer.press('Enter')
let streaming = null
const streamDeadline = Date.now() + 15000
while (Date.now() < streamDeadline) {
  const s = await page.evaluate(() => {
    const row = document.querySelector('.hub-dm .msg-row.agent.streaming .luna-avatar')
    const svg = row?.querySelector('svg[data-luna-face]')
    return row
      ? {
        host: row.getAttribute('data-live'),
        svg: svg?.getAttribute('data-live') ?? null,
        state: row.getAttribute('data-state'),
        anim: svg ? getComputedStyle(svg).animationName : null,
      }
      : null
  })
  if (s && s.anim) { streaming = s; break }
  await page.waitForTimeout(50)
}
check(!!streaming, '流式草稿行出现了（用于采样运行中的头像）')
if (streaming) {
  check(streaming.host === 'off' && streaming.svg === 'off',
    `流式行头像 data-live=off（host=${streaming.host} svg=${streaming.svg}）`)
  check(streaming.anim === 'none',
    `流式行头像不跑 CSS 动画（state=${streaming.state} animationName=${streaming.anim}）`)
}

// 等本轮两轮回复落地（dmstream 每轮一条气泡）。
const settleDeadline = Date.now() + 20000
while (Date.now() < settleDeadline) {
  const streamingNow = await page.locator('.hub-dm .msg-row.agent.streaming').count()
  if (streamingNow === 0) break
  await page.waitForTimeout(100)
}

// 再发两条，把行数堆到能真的滚出 rootMargin（200px）之外。
for (let i = 0; i < 2; i++) {
  await composer.fill(`继续 ${i + 2}`)
  await composer.press('Enter')
  const d = Date.now() + 20000
  while (Date.now() < d) {
    if ((await page.locator('.hub-dm .msg-row.agent.streaming').count()) === 0) break
    await page.waitForTimeout(100)
  }
  await page.waitForTimeout(300)
}

// —— 4) IntersectionObserver：离屏 visible=false，滚回 true ——
// 把会话区压到 240px 高，行数远超一屏，早期行就会滚出「240 + rootMargin 200」。
const faces = () => page.evaluate(() => {
  const list = document.querySelector('.hub-dm .messages')
  const box = list.getBoundingClientRect()
  const rows = Array.from(list.querySelectorAll('.msg-row.agent'))
  return {
    scrollTop: Math.round(list.scrollTop),
    scrollHeight: Math.round(list.scrollHeight),
    rows: rows.map((r) => {
      const svg = r.querySelector('svg[data-luna-face]')
      const rect = r.getBoundingClientRect()
      return {
        top: Math.round(rect.top - box.top),
        hasFace: !!svg?.__lunaFace,
        visible: svg?.__lunaFace?.visible ?? null,
      }
    }),
  }
})

await page.evaluate(() => {
  const list = document.querySelector('.hub-dm .messages')
  list.style.flex = '0 0 auto'
  list.style.height = '240px'
  list.scrollTop = 0
})
await page.waitForTimeout(400)
const top = await faces()
check(top.rows.length >= 6, `会话里至少 6 条 agent 行（got=${top.rows.length}）`)
check(top.rows.every((r) => r.hasFace), `每条 agent 行都拿到了引擎 face（缺 ${top.rows.filter((r) => !r.hasFace).length} 个）`)
check(top.scrollHeight > 600, `会话内容比一屏高，能真的滚出去（scrollHeight=${top.scrollHeight}）`)
const offAtTop = top.rows.filter((r) => r.top > 240 + 200 + 40)
check(offAtTop.length > 0, `顶部位置存在超出 rootMargin 的行（got=${offAtTop.length}）`)
check(offAtTop.every((r) => r.visible === false),
  `离屏头像 visible=false（不合规 ${offAtTop.filter((r) => r.visible !== false).length}/${offAtTop.length}）`)
const onAtTop = top.rows.filter((r) => r.top >= 0 && r.top < 200)
check(onAtTop.length > 0, `顶部位置有屏内行（got=${onAtTop.length}）`)
check(onAtTop.every((r) => r.visible === true),
  `屏内头像 visible=true（不合规 ${onAtTop.filter((r) => r.visible !== true).length}/${onAtTop.length}）`)

await page.evaluate(() => {
  const list = document.querySelector('.hub-dm .messages')
  list.scrollTop = list.scrollHeight
})
await page.waitForTimeout(400)
const bottom = await faces()
const nowOn = bottom.rows.filter((r) => r.top >= 0 && r.top < 200)
const nowOff = bottom.rows.filter((r) => r.top < -240)
check(nowOn.length > 0, `滚到底后屏内有行（got=${nowOn.length}）`)
check(nowOn.every((r) => r.visible === true),
  `滚回来的头像 visible=true（不合规 ${nowOn.filter((r) => r.visible !== true).length}/${nowOn.length}）`)
check(nowOff.length > 0, `滚到底后存在离屏行（got=${nowOff.length}）`)
check(nowOff.every((r) => r.visible === false),
  `滚走后头像 visible=false（不合规 ${nowOff.filter((r) => r.visible !== false).length}/${nowOff.length}）`)

// 复原内联样式，别把压扁的会话区带进 390 视口检查。
await page.evaluate(() => {
  const list = document.querySelector('.hub-dm .messages')
  list.style.removeProperty('flex')
  list.style.removeProperty('height')
  list.scrollTop = list.scrollHeight
})

// —— 5) 390 宽：不横向溢出 ——
await page.setViewportSize({ width: 390, height: 844 })
await page.waitForTimeout(500)
const narrow = await page.evaluate(() => ({
  scrollWidth: document.documentElement.scrollWidth,
  viewport: document.documentElement.clientWidth,
  rowOverflow: Array.from(document.querySelectorAll('.hub-dm .msg-row')).filter((r) => {
    const b = r.getBoundingClientRect()
    return b.right > 390 + 1 || b.left < -1
  }).length,
}))
check(narrow.scrollWidth <= narrow.viewport,
  `390 宽无横向溢出（scrollWidth=${narrow.scrollWidth} > viewport=${narrow.viewport}）`)
check(narrow.rowOverflow === 0, `390 宽消息行没有跑出视口（${narrow.rowOverflow} 行）`)
await page.setViewportSize({ width: 1280, height: 900 })

// —— 6) 全程 console error / pageerror 0 ——
check(errors.length === 0, `无 console error / pageerror：${errors.slice(0, 3).join(' | ')}`)

await browser.close()
stopServer()

console.log(`\n采样：rows=${a0.rowCount} anim(row/head)=${anim.rowWorking}/${anim.headWorking}`
  + ` visible(top/off)=${top.rows.filter((r) => r.visible === true).length}/${top.rows.filter((r) => r.visible === false).length}`
  + ` scrollHeight=${top.scrollHeight} 390 scrollWidth=${narrow.scrollWidth}`)

if (failed.length) {
  console.error(`\n❌ Hub 头像性能：${failed.length} 项不达标`)
  for (const f of failed) console.error(`   · ${f}`)
  process.exit(1)
}
console.log('✅ Hub 头像性能：全部通过')
