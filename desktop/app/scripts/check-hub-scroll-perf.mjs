#!/usr/bin/env node
// Hub 长历史滚动/贴底性能验收（对应三条修复：贴底依赖 liveChars、onScroll rAF 合并、
// .msg-row 的 content-visibility）。
//
// 验的四件事：
//   ① content-visibility：已结算的 .msg-row 计算值是 auto，离屏行的实测高度回落到
//      contain-intrinsic-size 的 120px 占位（= 真的跳过了布局/绘制，不只是「写了条规则」）；
//      正在流式的行必须是 visible —— 它每个 chunk 都在长高，跳过会让 scrollHeight 滞后，
//      贴底就一顿一顿；
//   ② onScroll 的 rAF 合并：滚到顶确实会请求 loadOlder；同一帧里打 30 个 scroll 只请求
//      一次；已经有一页在飞（loadingOlder）时不再请求（否则顶部会刷屏式翻页）；
//   ③ 源码静态口径：贴底 effect 的依赖是 liveChars 而不是裸 live，onScroll 走 rAF，
//      GroupView 的贴底依赖带上草稿正文长度；
//   ④ 1280 与 390×844 都没有横向溢出。
//
// 用法（本脚本不自起 dev server：没有就 exit 2）：
//   node scripts/check-hub-scroll-perf.mjs                        # 复用 localhost:5180
//   node scripts/check-hub-scroll-perf.mjs --url http://localhost:5203/
//   node scripts/check-hub-scroll-perf.mjs --port 5180 --out /tmp/hub-scroll-perf
//
// 判据（任一失败 → exit 1，并打印实测值）：
//   · 已结算行 content-visibility:auto / 流式行 visible；
//   · 离屏行高度 ≈ 120px（占位生效）；
//   · documentElement 与 .messages 均无横向溢出（1280 / 390×844）；
//   · 滚到顶触发 1 次 loadOlder；同帧 30 个 scroll 仍只 1 次；loadingOlder 时 0 次；
//   · 无 console error / pageerror。
import { chromium } from 'playwright'
import { mkdirSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const APP_DIR = fileURLToPath(new URL('..', import.meta.url))
const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const PORT = Number(arg('port', process.env.HUB_SCROLL_PERF_PORT || '5180'))
const BASE = arg('url', '') || `http://localhost:${PORT}/`
const OUT = arg('out', '/tmp/hub-scroll-perf')
// 注入多少条历史行：80 条足够让列表远超视口（每行 ~70px → ~5.6 屏），
// 又不会让脚本跑太久。
const INJECT = 80
const INTRINSIC = 120 // 与 hub.css 的 contain-intrinsic-size 一致

const failed = []
const errors = []
const measured = {}
const check = (cond, msg) => { if (!cond) failed.push(msg) }

mkdirSync(OUT, { recursive: true })

// —— 前置：dev server 必须已在跑（不自起，避免和并发会话抢端口）——
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
const dead = await preflight(BASE)
if (dead) {
  console.error(`❌ dev server 不可用：${BASE}（${dead}）`)
  console.error('   先起：cd desktop/app && npx vite --port 5203 --strictPort')
  console.error('   再用 --url http://localhost:5203/ 指过来。')
  process.exit(2)
}

// —— ③ 静态口径：先查源码，失败也继续跑浏览器（一次把问题都报出来）——
const hubSrc = readFileSync(join(APP_DIR, 'src/hub', 'HubSurface.tsx'), 'utf8')
const groupSrc = readFileSync(join(APP_DIR, 'src/hub', 'GroupView.tsx'), 'utf8')
const cssSrc = readFileSync(join(APP_DIR, 'src/hub', 'hub.css'), 'utf8')

// 贴底 effect 的依赖数组：靠 messages.length 认出它（onScroll 里没有依赖数组）。
const depsMatch = hubSrc.match(/\},\s*\[([^\]]*messages\.length[^\]]*)\]\s*\)/)
const stickyDeps = depsMatch ? depsMatch[1].replace(/\s+/g, ' ').trim() : ''
check(!!depsMatch, 'HubSurface：找到贴底 effect 的依赖数组')
check(/\bliveChars\b/.test(stickyDeps), `HubSurface：贴底 deps 用 liveChars（实测：${stickyDeps || '未找到'}）`)
check(!/(^|,)\s*live\s*(,|$)/.test(stickyDeps), `HubSurface：贴底 deps 不再含裸 live（实测：${stickyDeps || '未找到'}）`)
const onScrollBody = (hubSrc.match(/function onScroll\(\): void \{([\s\S]*?)\n {2}\}/) || ['', ''])[1]
check(/requestAnimationFrame/.test(onScrollBody), 'HubSurface：onScroll 用 requestAnimationFrame 合并')
check(/scrollRaf\.current/.test(onScrollBody), 'HubSurface：onScroll 用 scrollRaf 句柄判 pending')
check(/cancelAnimationFrame/.test(hubSrc), 'HubSurface：卸载时 cancelAnimationFrame')
const groupDepsMatch = groupSrc.match(/\},\s*\[([^\]]*messages\.length[^\]]*)\]\s*\)/)
const groupDeps = groupDepsMatch ? groupDepsMatch[1].replace(/\s+/g, ' ').trim() : ''
check(/groupDraftChars/.test(groupDeps), `GroupView：贴底 deps 含草稿正文长度（实测：${groupDeps || '未找到'}）`)
check(/\.hub-demo \.msg-row \{ content-visibility: auto/.test(cssSrc), 'hub.css：.msg-row 有 content-visibility:auto')
check(/\.hub-demo \.msg-row\.streaming \{ content-visibility: visible/.test(cssSrc), 'hub.css：.msg-row.streaming 是 visible')

// —— 浏览器 ——
const browser = await chromium.launch()
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()) })
page.on('pageerror', (e) => errors.push(`pageerror ${e.message}`))

// 冷启动的 vite 可能先返回 504（依赖重优化）：重试到 React 挂载出 Hub 左栏。
async function openApp() {
  for (let i = 0; i < 4; i++) {
    await page.goto(BASE, { waitUntil: 'domcontentloaded' })
    await page.waitForTimeout(1500)
    if (await page.locator('.hub-side-row').count()) return
    if (!(await page.locator('button[aria-label="智能体"]').count())) continue
    await page.locator('button[aria-label="智能体"]').first().click({ timeout: 8000 }).catch(() => {})
    await page.waitForTimeout(700)
    if (await page.locator('.hub-side-row').count()) return
  }
  throw new Error('页面没挂载出 Hub 左栏（dev server 未就绪 / 504 Outdated Optimize Dep）')
}

let crashed = null
try {
  await openApp()
  await page.locator('.hub-side-row').first().waitFor({ timeout: 10000 })
  const tab = page.locator('.hub-side-tabs button').filter({ hasText: '智能体' }).first()
  if ((await tab.getAttribute('aria-selected')) !== 'true') await tab.click({ timeout: 6000 })
  await page.waitForTimeout(250)
  await page.locator('.hub-side-row').first().click({ timeout: 8000 })
  await page.waitForTimeout(800)
  await page.locator('.hub-dm .composer textarea').first().waitFor({ timeout: 8000 })

  // 注入前的真实行数：用来确认 page 里 import 到的 store 就是应用那一份。
  measured.domRowsBefore = await page.locator('.hub-dm .msg-row').count()

  // —— ① 注入 80 条历史行 + 1 条流式行（cloneNode：真 DOM、真 CSS）——
  const injected = await page.evaluate((n) => {
    const inner = document.querySelector('.hub-dm .messages-inner')
    const tpl = inner?.querySelector('.msg-row.agent:not(.streaming)')
    if (!inner || !tpl) return { error: '找不到 .messages-inner 或可复制的 .msg-row' }
    const clone = (label) => {
      const c = tpl.cloneNode(true)
      c.setAttribute('data-injected', label)
      // 假行不能抢深链/跳转的目标（scrollToSeq 靠 [data-seq]）。
      c.querySelectorAll('[data-seq]').forEach((e) => e.removeAttribute('data-seq'))
      const txt = c.querySelector('.agent-text')
      if (txt) {
        txt.replaceChildren(document.createTextNode(
          `注入行 ${label}：长历史滚动/布局验收。` + '正文 '.repeat(20),
        ))
      }
      return c
    }
    for (let i = 0; i < n; i++) inner.appendChild(clone(String(i)))
    const streaming = clone('streaming')
    streaming.classList.add('streaming')
    inner.appendChild(streaming)
    const list = document.querySelector('.hub-dm .messages')
    return { rows: inner.querySelectorAll('.msg-row').length, scrollHeight: list.scrollHeight, clientHeight: list.clientHeight }
  }, INJECT)
  measured.injected = injected
  check(!injected.error, `注入 ${INJECT} 条 .msg-row 成功（${injected.error ?? 'ok'}）`)

  // 滚到顶再量：顶部那批行必然离屏 → 只可能拿到占位高度。
  const cv = await page.evaluate(() => {
    const list = document.querySelector('.hub-dm .messages')
    list.scrollTop = 0
    const rows = Array.from(document.querySelectorAll('.hub-dm .msg-row'))
    const settled = rows.filter((r) => !r.classList.contains('streaming'))
    const streaming = rows.filter((r) => r.classList.contains('streaming'))
    const heightOf = (r) => (r ? r.offsetHeight : -1)
    return {
      total: rows.length,
      settledCount: settled.length,
      // 计算值就是「规则有没有生效」：auto 是贴上去的那条，visible 是流式例外。
      settledValues: Array.from(new Set(settled.map((r) => getComputedStyle(r).contentVisibility))),
      streamingValues: Array.from(new Set(streaming.map((r) => getComputedStyle(r).contentVisibility))),
      streamingIntrinsic: Array.from(new Set(streaming.map((r) => getComputedStyle(r).containIntrinsicSize))),
      // 实测高度：视口内的行是真高，远行回落到 120px 占位 = 真的跳过了布局。
      // 远行要挑**已结算**的最后一行：最后一条是我们注入的 .streaming（visible，永远真高）。
      nearH: heightOf(rows[0]),
      farH: heightOf(settled[settled.length - 1]),
      streamingH: heightOf(rows[rows.length - 1]),
      docOverflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
      listOverflow: list.scrollWidth - list.clientWidth,
      scrollHeight: list.scrollHeight,
      clientHeight: list.clientHeight,
    }
  })
  measured.contentVisibility = cv
  await page.screenshot({ path: join(OUT, 'hub-scroll-1280.png') })

  check(cv.total >= INJECT + 1, `.messages-inner 里有 ${INJECT}+1 条行（got=${cv.total}）`)
  check(cv.settledValues.length === 1 && cv.settledValues[0] === 'auto',
    `已结算行 content-visibility === 'auto'（got=${JSON.stringify(cv.settledValues)}）`)
  check(cv.streamingValues.length === 1 && cv.streamingValues[0] === 'visible',
    `.streaming 行 content-visibility === 'visible'（got=${JSON.stringify(cv.streamingValues)}）`)
  check(cv.streamingIntrinsic.every((v) => v === 'auto none'),
    `.streaming 行 contain-intrinsic-size === 'auto none'（got=${JSON.stringify(cv.streamingIntrinsic)}）`)
  check(Math.abs(cv.farH - INTRINSIC) <= 8,
    `离屏行回落到 ${INTRINSIC}px 占位（实测 ${cv.farH}px；近处行 ${cv.nearH}px）`)
  check(cv.docOverflow <= 0, `1280 下 documentElement 无横向溢出（实测 ${cv.docOverflow}px）`)
  check(cv.listOverflow <= 0, `1280 下 .messages 无横向溢出（实测 ${cv.listOverflow}px）`)

  // —— ② 滚到顶触发 loadOlder，且不刷屏 ——
  // 直接替换 store 的 loadOlder 计数：HubSurface 的 rAF 回调读的是 getState()，
  // 所以这份 spy 一定被调到（也就顺带证明「rAF 里读的是现值」）。
  const scroll = await page.evaluate(async () => {
    const mod = await import('/src/hub/useHubStore.ts')
    const store = mod.useHubStore
    const state = store.getState()
    const list = document.querySelector('.hub-dm .messages')
    const calls = []
    // spy 两种模式：
    //   gated=false —— 只计数，不立 loadingOlder。这是**唯一**能测出 rAF 合并的形态：
    //     没有合并时 30 个事件 = 30 次调用；有合并时 = 1 次。
    //   gated=true  —— 模拟真实 action 的闸门（请求一开始就立 loadingOlder 并保持一会儿），
    //     用来测「惯性滚动连续 25 帧不会一帧翻一页」。
    let gated = false
    let gate = 0
    store.setState({
      hasMore: true,
      loadingOlder: false,
      loadOlder: async () => {
        calls.push(performance.now())
        if (!gated) return
        gate += 1
        store.setState({ loadingOlder: true })
        setTimeout(() => {
          gate -= 1
          if (gate === 0) store.setState({ loadingOlder: false })
        }, 600)
      },
    })
    const settle = (ms) => new Promise((r) => setTimeout(r, ms))
    const frame = () => new Promise((r) => requestAnimationFrame(r))

    // 先离开顶部，让「滚到顶」是真的产生一次 scroll 事件。
    list.scrollTop = list.scrollHeight
    await settle(150)
    calls.length = 0
    list.scrollTop = 0
    await settle(200)
    const afterScrollToTop = calls.length

    // 同一帧 30 个 scroll：rAF 合并后只该有 1 次（无合并 → 30 次）。
    calls.length = 0
    gated = false
    store.setState({ loadingOlder: false })
    for (let i = 0; i < 30; i++) list.dispatchEvent(new Event('scroll'))
    await settle(200)
    const afterSameFrameBurst = calls.length

    // 已有一页在飞：一个都不该再发。
    calls.length = 0
    store.setState({ loadingOlder: true })
    list.dispatchEvent(new Event('scroll'))
    await settle(200)
    const whileLoading = calls.length

    // 连续 25 帧惯性滚动：闸门保持时只该有 1 次（不会每帧翻一页）。
    calls.length = 0
    gated = true
    store.setState({ loadingOlder: false })
    for (let f = 0; f < 25; f++) {
      list.dispatchEvent(new Event('scroll'))
      await frame()
    }
    await settle(200)
    const overFrames = calls.length

    return {
      storeMsgCount: state.messages.length,
      afterScrollToTop,
      afterSameFrameBurst,
      whileLoading,
      overFrames,
      scrollTop: list.scrollTop,
      scrollHeight: list.scrollHeight,
    }
  })
  measured.loadOlder = scroll
  // 注入前的 DOM 行数 ≈ store 的消息数：对不上说明 import 到的不是应用那份 store，
  // 后面那些 0/1 的计数就不可信了，必须点出来。
  check(scroll.storeMsgCount > 0 && measured.domRowsBefore >= scroll.storeMsgCount,
    `页面 import 到的是应用同一份 store（store ${scroll.storeMsgCount} 条 / 注入前 DOM ${measured.domRowsBefore} 行）`)
  check(scroll.afterScrollToTop === 1,
    `滚到顶触发 1 次 loadOlder（got=${scroll.afterScrollToTop}）`)
  check(scroll.afterSameFrameBurst === 1,
    `同一帧 30 个 scroll 只触发 1 次（rAF 合并；got=${scroll.afterSameFrameBurst}）`)
  check(scroll.whileLoading === 0,
    `loadingOlder 期间不再请求（got=${scroll.whileLoading}）`)
  check(scroll.overFrames === 1,
    `连续 25 帧滚动只请求 1 次（got=${scroll.overFrames}）`)

  // —— ④ 390×844：窄屏不许横向溢出 ——
  await page.setViewportSize({ width: 390, height: 844 })
  await page.waitForTimeout(500)
  const narrow = await page.evaluate(() => {
    const list = document.querySelector('.hub-dm .messages')
    const rows = Array.from(document.querySelectorAll('.hub-dm .msg-row'))
    const settled = rows.filter((r) => !r.classList.contains('streaming'))
    return {
      docOverflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
      listOverflow: list ? list.scrollWidth - list.clientWidth : -1,
      settledValues: Array.from(new Set(settled.map((r) => getComputedStyle(r).contentVisibility))),
      streamingValues: Array.from(new Set(rows.filter((r) => r.classList.contains('streaming'))
        .map((r) => getComputedStyle(r).contentVisibility))),
    }
  })
  measured.narrow = narrow
  await page.screenshot({ path: join(OUT, 'hub-scroll-390.png') })
  check(narrow.docOverflow <= 0, `390×844 documentElement 无横向溢出（实测 ${narrow.docOverflow}px）`)
  check(narrow.listOverflow <= 0, `390×844 .messages 无横向溢出（实测 ${narrow.listOverflow}px）`)
  check(narrow.settledValues.length === 1 && narrow.settledValues[0] === 'auto',
    `390×844 下已结算行仍是 auto（got=${JSON.stringify(narrow.settledValues)}）`)
  check(narrow.streamingValues.length === 1 && narrow.streamingValues[0] === 'visible',
    `390×844 下 .streaming 仍是 visible（got=${JSON.stringify(narrow.streamingValues)}）`)

  // 滚动过程中不许有异常（rAF 回调读 listRef、store 现值都在这条路径上）。
  check(errors.length === 0, `无 console error / pageerror：${errors.slice(0, 3).join(' | ')}`)
} catch (e) {
  crashed = e
} finally {
  await browser.close().catch(() => {})
}

if (crashed) {
  console.error(`\n❌ Hub 滚动性能验收：脚本中断 —— ${crashed.message}`)
  console.error(`   实测：${JSON.stringify(measured, null, 1)}`)
  process.exit(1)
}

console.log('实测：')
console.log(`  · .msg-row 计算值：${JSON.stringify(measured.contentVisibility?.settledValues ?? [])}` +
  `（${measured.contentVisibility?.settledCount ?? 0} 行）· .streaming：${JSON.stringify(measured.contentVisibility?.streamingValues ?? [])}`)
console.log(`  · 离屏行占位高度：${measured.contentVisibility?.farH}px（近处行 ${measured.contentVisibility?.nearH}px，` +
  `流式行真高 ${measured.contentVisibility?.streamingH}px，列表 ${measured.contentVisibility?.scrollHeight}/${measured.contentVisibility?.clientHeight}px）`)
console.log(`  · loadOlder 调用：滚到顶 ${measured.loadOlder?.afterScrollToTop} · 同帧 30 事件 ${measured.loadOlder?.afterSameFrameBurst}` +
  ` · loadingOlder 时 ${measured.loadOlder?.whileLoading} · 连续 25 帧 ${measured.loadOlder?.overFrames}`)
console.log(`  · 横向溢出：1280 ${measured.contentVisibility?.docOverflow}px / 390 ${measured.narrow?.docOverflow}px`)

if (failed.length) {
  console.error(`\n❌ Hub 滚动性能验收：${failed.length} 项不达标`)
  for (const f of failed) console.error(`   · ${f}`)
  console.error(`   截图：${OUT}`)
  process.exit(1)
}
console.log(`\n✅ Hub 滚动性能验收：全部通过（截图见 ${OUT}）`)
