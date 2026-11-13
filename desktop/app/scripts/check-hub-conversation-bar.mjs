#!/usr/bin/env node
// 群状态条（ConversationBar，P6-10）浏览器验收。
//
// 判据（任一失败 → exit 1）：
//   · 状态条是 header 与消息区之间的独立一档（几何上夹在两者中间，不随消息滚动）；
//   · 一轮 @ 的账本读得出来：本轮 / 本链 / 队列 / pair（带自动恢复倒计时）/ 投递
//     （已入队·已回复·未回复·失败·跳过）都在，未知原因码有可读文案；
//   · 「重置本群限制」真的会重置（点击后 store 里 status=idle、pair 清零、状态条收起）；
//   · 真发一条 @all：先「已入队 N」，回复落地后变「已回复 1 · 未回复 N-1」（@all 成功
//     = 已入队，不是「已发送」）；
//   · 1280 / 640 都无横向溢出（documentElement 与状态条自身都不溢出）。
//
// 用法（dev server 用 525x，别占 5173/5180）：
//   cd desktop/app && npx vite --port 5257          # 另开一个终端
//   node scripts/check-hub-conversation-bar.mjs                 # 默认 http://localhost:5257/
//   node scripts/check-hub-conversation-bar.mjs --url http://localhost:5258/
//
// dev server 没起 → 打印怎么起 + exit 2（与「断言不达标」的 exit 1 区分开）。
import { chromium } from 'playwright'
import { mkdirSync } from 'node:fs'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const URL = arg('url', process.env.HUB_CONV_BAR_URL || 'http://localhost:5257/')
const OUT = arg('out', '/tmp/hub-conv-bar-shots')
const GROUP_NAME = '场景演示 · 六项能力'

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
  console.error('     cd desktop/app && npx vite --port 5257')
  process.exit(2)
}

mkdirSync(OUT, { recursive: true })

const browser = await chromium.launch()
const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
const shot = async (name) => {
  await page.screenshot({ path: `${OUT}/${name}.png`, fullPage: false })
  console.log(`  📷 ${OUT}/${name}.png`)
}

// 真读 Hub store（不是 DOM 猜）：main.tsx 在 **dev 构建**里把 Hub store 挂在 window.__hub
// （同 __store/__hydrate，见 shoot-hub-failures.mjs 的用法）。用它而不是动态 import：
// HMR 会给同一文件换 `?t=` URL，动态 import 可能拿到另一个模块实例（状态是空的）。
// window.__store 是 app store，没有 groupConversations。
const hubConvState = () => page.evaluate(async () => {
  const store = window.__hub
  if (!store) throw new Error('window.__hub 不存在（不是 dev 构建？）')
  const s = store.getState()
  const c = s.groupConversations[s.activeGroupId] ?? null
  return {
    gid: s.activeGroupId, status: c?.status ?? null,
    pairs: Object.keys(c?.pair_count ?? {}).length, depth: c?.depth ?? null,
    notice: s.notice ?? '',
  }
})

// 判据里要读 store，就先确认拿到的是 App 的那一份（__hub 在启动后异步挂上，可能晚一帧）；
// 拿不到就硬刷一次。
async function syncHubStore(open) {
  for (let i = 0; i < 20; i++) {
    const st = await hubConvState().catch(() => null)
    if (st?.gid) return
    await page.waitForTimeout(150)
  }
  await page.reload({ waitUntil: 'domcontentloaded' })
  await open()
  if (!(await hubConvState()).gid) throw new Error('读不到 Hub store（activeGroupId 为空）：window.__hub 没挂上')
}

const bar = page.locator('.hub-conv-bar')
const barText = async () => (await bar.innerText().catch(() => '')).replace(/\s+/g, ' ').trim()

async function openGroup() {
  // 左栏行用 isVisible()：窄窗抽屉收起/首屏没渲染时 count() 仍是 1（N1 的同一个坑）。
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

try {
  // —— 1) 护栏命中的状态（?hubFixture=guards）：一条状态条说清楚六件事 ——
  console.log('\n[1] 护栏命中的状态条（?hubFixture=guards）')
  await page.goto(`${URL}?hubFixture=guards`, { waitUntil: 'domcontentloaded' })
  await openGroup()
  await bar.waitFor({ timeout: 10000 })
  await syncHubStore(openGroup)
  const text1 = await barText()
  console.log(`      状态条：${text1}`)
  check(text1.includes('等你继续'), '状态标签 = 等你继续')
  check(/第\s*2\s*轮/.test(text1), '本轮（第 2 轮）')
  check(/本链\s*4\/4/.test(text1), '本链（4/4，深度到限）')
  check(/队列\s*2/.test(text1), '队列（2 条在排）')
  check(/2\/2/.test(text1) && text1.includes('自动恢复'), 'pair 计数 + 自动恢复倒计时')
  check(text1.includes('已入队 2') && text1.includes('已回复 1') && text1.includes('未回复 1'),
    '@ 成功 = 已入队，另显示已回复/未回复')
  check(text1.includes('失败 1') && text1.includes('跳过 1'), '失败与跳过分开显示')
  check(text1.includes('未知原因：boom'), '未知原因码有可读文案（不是裸码）')

  // 几何：状态条夹在 header 与消息区之间（滚动历史时不会消失）。
  const geo = await page.evaluate(() => {
    const q = (s) => document.querySelector(s)
    const box = (el) => { const r = el?.getBoundingClientRect(); return r ? { top: r.top, bottom: r.bottom, h: r.height } : null }
    const h = box(q('.hub-group-view .chat-header'))
    const b = box(q('.hub-group-view .hub-conv-bar'))
    const m = box(q('.hub-group-view .messages'))
    const cs = q('.hub-group-view .hub-conv-bar') ? getComputedStyle(q('.hub-group-view .hub-conv-bar')) : null
    return { h, b, m, pos: cs?.position ?? '', docOverflow: document.documentElement.scrollWidth - window.innerWidth }
  })
  check(!!geo.b && geo.b.h >= 18, `状态条有高度（${geo.b?.h ?? 0}px）`)
  check(!!geo.h && !!geo.m && geo.h.bottom - 1 <= geo.b.top && geo.b.bottom - 1 <= geo.m.top,
    '状态条夹在 header 与 messages 之间（独立一档，不随消息滚动）')
  check(geo.pos !== 'absolute' && geo.pos !== 'fixed', `状态条不是浮层（position=${geo.pos}）`)
  check(geo.docOverflow <= 0, `1280 无横向溢出（${geo.docOverflow}px）`)

  // hover 明细：每个 target 一条（谁在跑、谁跳过、为什么）。
  const detail = await bar.locator('.hub-conv-delivery').first().getAttribute('title')
  check((detail ?? '').split('\n').length === 4, `投递明细 4 行（got=${(detail ?? '').split('\n').length}）`)
  check((detail ?? '').includes('已回复') && (detail ?? '').includes('排队中'), '明细含已回复/排队中')
  await shot('conv-bar-guards-1280')

  // —— 2) 重置本群限制：真的清零（不是只改显示） ——
  console.log('\n[2] 重置本群限制')
  const before = await hubConvState()
  check(before.status === 'waiting_human' && before.depth === 4 && before.pairs === 1,
    `重置前：status=${before.status} / pair=${before.pairs} / depth=${before.depth}`)
  const resetBtn = bar.locator('.hub-conv-reset')
  check(await resetBtn.isVisible(), '状态条上有「重置本群限制」')
  await resetBtn.click()
  await page.waitForTimeout(900)
  const after = await hubConvState()
  check(after.status === 'idle' && after.pairs === 0 && after.depth === 0,
    `重置后：status=${after.status} / pair=${after.pairs} / depth=${after.depth}`)
  check((after.notice ?? '').includes('重置'), `提示透出（got=${JSON.stringify(after.notice)}）`)
  // 状态条此时可能还在：演示群有一个 dispatched/running 的任务，B7 要求「有 run 在跑就
  // 保留停止入口」。判据是**旧计数消失**，不是整条消失。
  const text2 = (await bar.isVisible().catch(() => false)) ? await barText() : ''
  check(!text2.includes('等你继续') && !text2.includes('2/2') && !/第\s*2\s*轮/.test(text2),
    `重置后状态条不再显示旧计数（got=${JSON.stringify(text2)}）`)

  // —— 3) 真发一条 @all：先「已入队」，回复落地后「已回复 1 · 未回复 N-1」 ——
  console.log('\n[3] @all 真流程')
  const ta = page.locator('.hub-group-view .composer textarea')
  await ta.fill('@all 看看这条')
  await page.locator('.hub-group-view .composer .send-btn').click()
  await page.waitForTimeout(500)
  const text3a = await barText()
  console.log(`      发送后：${text3a}`)
  check(/已入队\s*2/.test(text3a), `@all 成功 = 已入队 2（got=${JSON.stringify(text3a)}）`)
  check(/第\s*1\s*轮/.test(text3a) && !text3a.includes('2/2'),
    `重置后新一轮从第 1 轮数起、pair 归零（got=${JSON.stringify(text3a)}）`)
  check(/未回复\s*2|已回复\s*1/.test(text3a), '投递账本已生效（已回复/未回复）')
  await page.waitForTimeout(4200) // 演示群走两轮流式回复（约 2.6s）
  const text3b = await barText()
  console.log(`      回复后：${text3b}`)
  check(/已回复\s*1/.test(text3b) && /未回复\s*1/.test(text3b),
    `回复落地后：已回复 1 · 未回复 1（got=${JSON.stringify(text3b)}）`)
  await shot('conv-bar-after-all-1280')

  // —— 4) 640：状态条折行、不横向溢出 ——
  console.log('\n[4] 640 窄窗')
  await page.setViewportSize({ width: 640, height: 900 })
  await page.waitForTimeout(600)
  const narrow = await page.evaluate(() => {
    const el = document.querySelector('.hub-group-view .hub-conv-bar')
    const r = el?.getBoundingClientRect()
    return {
      visible: !!el && !!r && r.width > 0,
      w: r?.width ?? 0,
      selfOverflow: el ? el.scrollWidth - el.clientWidth : 0,
      docOverflow: document.documentElement.scrollWidth - window.innerWidth,
    }
  })
  check(narrow.visible, '640 状态条仍可见')
  check(narrow.selfOverflow <= 1, `640 状态条自身不溢出（${narrow.selfOverflow}px）`)
  check(narrow.docOverflow <= 0, `640 无横向溢出（${narrow.docOverflow}px）`)
  await shot('conv-bar-640')
} catch (e) {
  failed++
  console.error(`\n❌ 断言过程抛错：${e?.message ?? e}`)
} finally {
  await browser.close()
}

console.log(`\n${failed === 0 ? '✅' : '❌'} 群状态条：${pass} 项通过 / ${failed} 项失败`)
process.exit(failed === 0 ? 0 : 1)
