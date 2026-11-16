// 并发归属探针：前后台同时流式时，前台 DOM 变更量是「只有前台流」的 3 倍
//（bench-concurrent B1 1864 → B2 5582）。先排除最坏的可能 —— 后台会话的正文
// 串进了前台会话。
//
// 做法：给两条流**不同的正文标记**（FG… / BG…），流完在页面里数：
//   · 前台 .narrative 里出现几次 "BG"（必须 0）
//   · views[fg].blocks / views[bg].blocks 里各自的文本（必须各归各）
// 同时量 blocks / views 的身份变化次数（判断 3 倍是「重渲染次数」还是「一次动更多节点」）。
//
// 用法：npx vite --port 5288 & node lab/probe-concurrent-bleed.mjs
import { readFileSync } from 'node:fs'
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const CP_PATH = process.env.CP_PATH || '/tmp/real-cp-settled.json'
const cp = JSON.parse(readFileSync(CP_PATH, 'utf8'))
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
let fail = 0
const ok = (m) => console.log('OK ' + m)
const bad = (m) => { fail++; console.log('!! ' + m) }

const b = await Browser.launch({ width: 1280, height: 800 })
const page = await b.newPage({ width: 1280, height: 800 })
await page.goto(URL, { settleMs: 4500 })
for (let i = 0; i < 200; i++) { if (await page.eval(() => !!window.__store && !!window.__hydrate)) break; await wait(150) }

const FG = 'bleed-fg'
const BG = 'bleed-bg'
const prep = async (sid, blocks) => {
  await page.eval(({ cp, sid }) => window.__hydrate(sid, cp, undefined, { live: true }), { cp: { ...cp, blocks }, sid })
  await wait(200)
}
await page.eval(() => {
  const st = window.__store.getState()
  st.setNarrativeDensity('detailed')
})
await prep(BG, cp.blocks)
await prep(FG, cp.blocks)
await page.eval((sid) => window.__store.setState((s) => {
  const v = s.views[sid]
  return { activeSessionId: sid, blocks: v.blocks, blocksTotal: v.blocksTotal, blocksCursor: v.blocksCursor, blocksHasMore: v.blocksHasMore, runNodes: v.runNodes, usage: v.usage, turns: v.turns, costUsd: v.costUsd, runEnded: v.runEnded, turnsTotal: v.turnsTotal, todos: v.todos, diffs: v.diffs, agentDetached: v.agentDetached, approvals: v.approvals, workspace: '/mock/workspace' }
}), FG)
await wait(700)

// 计数器：blocks / views 的身份变化（≈ 顶层渲染的触发次数）
await page.eval(() => {
  const w = window
  w.__cnt = { blocks: 0, views: 0 }
  const un = w.__store.subscribe((s, p) => {
    if (s.blocks !== p.blocks) w.__cnt.blocks++
    if (s.views !== p.views) w.__cnt.views++
  })
  w.__cntUnsub = un
  w.__mut = 0
  w.__mutObs = new MutationObserver((r) => { w.__mut += r.length })
  w.__mutObs.observe(document.querySelector('.narrative'), { childList: true, subtree: true, characterData: true })
})

const r = await page.eval(async (arg) => {
  const d = window.__dispatch
  const now = () => new Date().toISOString()
  const run = async (sid, tag, text) => {
    d({ event_type: 'agent_start', session_id: sid, run_id: tag, index: 0, timestamp: now() })
    for (let i = 0; i < arg.n; i++) {
      d({ event_type: 'content_chunk', session_id: sid, request_id: tag, run_id: tag, content: `${text} 第 ${i} 段。\n\n` })
      await new Promise((r) => setTimeout(r, arg.gap))
    }
    d({ event_type: 'agent_end', session_id: sid, run_id: tag, timestamp: now() })
  }
  await Promise.all([run(arg.fg, 'fg-run', 'FG正文'), run(arg.bg, 'bg-run', 'BG正文')])
  await new Promise((r) => setTimeout(r, 600))
  const st = window.__store.getState()
  const textOf = (sid) => (st.views[sid]?.blocks ?? []).map((x) => (x.text ?? '') + (Array.isArray(x.items) ? x.items.map((i) => i.text ?? '').join('') : '')).join('')
  const dom = document.querySelector('.narrative').textContent
  return {
    fgHasBg: (dom.match(/BG正文/g) || []).length,
    fgHasFg: (dom.match(/FG正文/g) || []).length,
    fgTextBg: (textOf(arg.fg).match(/BG正文/g) || []).length,
    fgTextFg: (textOf(arg.fg).match(/FG正文/g) || []).length,
    bgTextBg: (textOf(arg.bg).match(/BG正文/g) || []).length,
    bgTextFg: (textOf(arg.bg).match(/FG正文/g) || []).length,
    cnt: window.__cnt,
    mut: window.__mut,
  }
}, { fg: FG, bg: BG, n: 24, gap: 24 })

console.log(`前台 DOM：FG ×${r.fgHasFg}  BG ×${r.fgHasBg}（BG 必须 0）`)
console.log(`views[fg]：FG ×${r.fgTextFg}  BG ×${r.fgTextBg}    views[bg]：BG ×${r.bgTextBg}  FG ×${r.bgTextFg}`)
console.log(`触发次数：顶层 blocks 变化 ${r.cnt.blocks}   views 变化 ${r.cnt.views}   .narrative 变更记录 ${r.mut}`)

if (r.fgHasBg === 0 && r.fgTextBg === 0) ok('后台正文没有串进前台')
else bad('后台正文串进了前台会话')
if (r.bgTextFg === 0) ok('前台正文没有串进后台')
else bad('前台正文串进了后台会话')
if (r.fgTextFg >= 20) ok(`前台收齐自己的 ${r.fgTextFg} 段`)
else bad(`前台只收到 ${r.fgTextFg} 段`)
if (r.bgTextBg >= 20) ok(`后台收齐自己的 ${r.bgTextBg} 段`)
else bad(`后台只收到 ${r.bgTextBg} 段`)

console.log(fail ? `\nFAIL ${fail}` : '\nPASS 归属正确')
await b.close?.()
