// 追查「420 块 detailed 挂载：runtime 的 layout/style 约是旧皮肤的 5 倍」
// （bench-attrib：task 0.31s/script 0.15s vs task 0.16s/script 0.13s）。
//
// 最可能的解释：`content-visibility: auto` 在 runtime 皮肤里没真正生效 —— 即
// 「屏幕外的消息跳过布局与样式」这条**唯一的规模杠杆**丢了。判据用浏览器自己的
// `checkVisibility({ contentVisibilityAuto: true })`：返回 false 的元素其内容被跳过。
//
// 同时量：通过/未通过的门槛分布（`.msg` 上 content-visibility 的计算值、以及是否
// 被祖先的 `contain`/`content-visibility: visible` 挡住）。
//
// 用法：npx vite --port 5288 & node lab/probe-cv.mjs [--blocks=3]
import { readFileSync } from 'node:fs'
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const CP_PATH = process.env.CP_PATH || '/tmp/real-cp-settled.json'
const MULT = Number((process.argv.find((a) => a.startsWith('--blocks=')) || '').split('=')[1] || 3)
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const base = JSON.parse(readFileSync(CP_PATH, 'utf8'))
const blocks = []
let id = 1
for (let k = 0; k < MULT; k++) {
  for (const b of base.blocks) {
    const c = JSON.parse(JSON.stringify(b))
    c.id = id++
    if (Array.isArray(c.items)) c.items.forEach((it) => { it.id = id++ })
    blocks.push(c)
  }
}

const b = await Browser.launch({ width: 1280, height: 800 })
const page = await b.newPage({ width: 1280, height: 800 })
await page.goto(URL, { settleMs: 4500 })
for (let i = 0; i < 200; i++) { if (await page.eval(() => !!window.__store && !!window.__hydrate)) break; await wait(150) }

const SID = 'cv-1'
const run = async (skin, density) => {
  await page.eval((arg) => {
    const st = window.__store.getState()
    const views = { ...st.views }
    delete views[arg.sid]
    window.__store.setState({ views, activeSessionId: undefined, blocks: [], runNodes: [], runEnded: true })
    st.setNarrativeDensity(arg.d)
  }, { sid: SID, skin, d: density })
  await wait(350)
  await page.eval(({ cp, sid }) => window.__hydrate(sid, cp, undefined, { live: true }), { cp: { ...base, blocks }, sid: SID })
  await page.eval((sid) => window.__store.setState((s) => {
    const v = s.views[sid]
    return { activeSessionId: sid, blocks: v.blocks, blocksTotal: v.blocksTotal, blocksCursor: v.blocksCursor, blocksHasMore: v.blocksHasMore, runNodes: v.runNodes, usage: v.usage, turns: v.turns, costUsd: v.costUsd, runEnded: v.runEnded, turnsTotal: v.turnsTotal, todos: v.todos, diffs: v.diffs, agentDetached: v.agentDetached, approvals: v.approvals, workspace: '/mock/workspace' }
  }), SID)
  await wait(900)
  return page.eval(() => {
    const root = document.querySelector('.narrative')
    const all = Array.from(root.querySelectorAll('*'))
    const msgs = Array.from(root.querySelectorAll('.msg'))
    const cvAuto = all.filter((el) => getComputedStyle(el).contentVisibility === 'auto')
    const skipped = cvAuto.filter((el) => !el.checkVisibility({ contentVisibilityAuto: true }))
    // 视口内 vs 视口外的消息：跳过数应当都来自视口外
    const inView = (el) => { const r = el.getBoundingClientRect(); return r.bottom > 0 && r.top < innerHeight }
    const offscreen = cvAuto.filter((el) => !inView(el))
    const offSkipped = offscreen.filter((el) => !el.checkVisibility({ contentVisibilityAuto: true }))
    const msgCv = {}
    for (const m of msgs) { const v = getComputedStyle(m).contentVisibility; msgCv[v] = (msgCv[v] || 0) + 1 }
    const styleTpl = {}
    for (const m of msgs.slice(0, 3)) styleTpl[m.className] = { cv: getComputedStyle(m).contentVisibility, contain: getComputedStyle(m).contain, cis: getComputedStyle(m).containIntrinsicSize }
    return {
      nodes: all.length,
      msgs: msgs.length,
      msgCv,
      cvAutoCount: cvAuto.length,
      skippedCount: skipped.length,
      offscreenAuto: offscreen.length,
      offscreenSkipped: offSkipped.length,
      scrollH: Math.round(root.scrollHeight),
      styleTpl,
      // 实际渲染高度：一次完整布局的代价代理
      laidOutHeight: Math.round(all.slice(0, 400).reduce((n, el) => n + (el.getBoundingClientRect().height || 0), 0)),
    }
  })
}

for (const density of ['concise', 'detailed']) {
  for (const skin of ['runtime']) { // 2026-11：current/v3 已删除
    const r = await run(skin, density)
    console.log(`\n[${density} / ${skin}] nodes ${r.nodes}  msgs ${r.msgs}  scrollH ${r.scrollH}`)
    console.log(`  .msg content-visibility 分布：${JSON.stringify(r.msgCv)}`)
    console.log(`  content-visibility:auto 元素 ${r.cvAutoCount}，其中内容被跳过 ${r.skippedCount}；视口外 ${r.offscreenAuto} → 跳过 ${r.offscreenSkipped}`)
    console.log(`  样例：${JSON.stringify(r.styleTpl).slice(0, 260)}`)
  }
}
await b.close?.()
