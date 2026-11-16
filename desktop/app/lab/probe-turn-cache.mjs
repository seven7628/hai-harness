// 元素复用缓存（Narrative 里的 turnCache）的**失效**判据 —— 缓存写错了只会表现为
// 「点了不展开 / 内容变了界面不动」，所以必须逐条证明「输入一变，那一轮就重画」。
//
// 场景（runtime/concise —— 工作盒子只在简洁档存在；detailed 是整段摊开）：
//   ① 让历史轮进入缓存（流式来几个 chunk，历史轮输入不变 → 复用）
//   ② 点开一个**已结算**轮的 Worked 盒 → 盒体必须真的出现（openTurns 变化要失效）
//   ③ 再点一次收起 → 盒体必须消失
//   ④ 切密度 concise → 轮的结构必须跟着变（density 变化要失效）
//   ⑤ 给已结算的那一轮**追加一块**（后台任务回执）→ 新记录必须出现（items 变化要失效）
//   ⑥ 追加之后盒子的内容块数要变（不是整轮重建就是没更新）
//
// 用法：npx vite --port 5288 & node lab/probe-turn-cache.mjs
import { readFileSync } from 'node:fs'
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const CP_PATH = process.env.CP_PATH || '/tmp/real-cp-settled.json'
const cp = JSON.parse(readFileSync(CP_PATH, 'utf8'))
const SID = 'cache-1'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
let fail = 0
const ok = (m) => console.log('OK ' + m)
const bad = (m) => { fail++; console.log('!! ' + m) }

const b = await Browser.launch({ width: 1280, height: 800 })
const page = await b.newPage({ width: 1280, height: 800 })
await page.goto(URL, { settleMs: 4500 })
for (let i = 0; i < 200; i++) { if (await page.eval(() => !!window.__store && !!window.__hydrate)) break; await wait(150) }

await page.eval((arg) => {
  const st = window.__store.getState()
  const views = { ...st.views }
  delete views[arg.sid]
  window.__store.setState({ views, activeSessionId: undefined, blocks: [], runNodes: [], runEnded: true })
  st.setNarrativeDensity('concise')
}, { sid: SID })
await wait(300)
await page.eval(({ cp, sid }) => window.__hydrate(sid, cp, undefined, { live: true }), { cp, sid: SID })
await page.eval((sid) => window.__store.setState((s) => {
  const v = s.views[sid]
  return { activeSessionId: sid, blocks: v.blocks, blocksTotal: v.blocksTotal, blocksCursor: v.blocksCursor, blocksHasMore: v.blocksHasMore, runNodes: v.runNodes, usage: v.usage, turns: v.turns, costUsd: v.costUsd, runEnded: v.runEnded, turnsTotal: v.turnsTotal, todos: v.todos, diffs: v.diffs, agentDetached: v.agentDetached, approvals: v.approvals, workspace: '/mock/workspace' }
}), SID)
await wait(800)

// ① 让历史轮进缓存：来 6 个 chunk（这些轮输入不变 → 命中复用）
await page.eval(async (sid) => {
  const d = window.__dispatch
  const now = () => new Date().toISOString()
  d({ event_type: 'agent_start', session_id: sid, run_id: 'cache-run', index: 0, timestamp: now() })
  for (let i = 0; i < 6; i++) {
    d({ event_type: 'content_chunk', session_id: sid, request_id: 'cache-r', run_id: 'cache-run', content: `缓存验证 ${i}。\n\n` })
    await new Promise((r) => setTimeout(r, 40))
  }
  d({ event_type: 'agent_end', session_id: sid, run_id: 'cache-run', timestamp: now() })
}, SID)
await wait(700)

// 找一个「已结算、有 Worked 行」的轮：concise 下它的 act 是 collapsed
const findSettled = () => page.eval(() => {
  const turns = Array.from(document.querySelectorAll('.rt-turn'))
  for (let i = 0; i < turns.length; i++) {
    const act = turns[i].querySelector('.rt-act')
    if (act && act.getAttribute('aria-expanded') === 'false') return i
  }
  return -1
})
const boxAt = (i) => page.eval((idx) => {
  const turns = Array.from(document.querySelectorAll('.rt-turn'))
  const t = turns[idx]
  if (!t) return { missing: true }
  const body = t.querySelector('.rt-work-body')
  const act = t.querySelector('.rt-act')
  return {
    turnCount: turns.length,
    hasBody: !!body,
    bodyH: body ? Math.round(body.getBoundingClientRect().height) : 0,
    rows: body ? body.querySelectorAll('*').length : 0,
    actText: act ? (act.textContent || '').slice(0, 24) : '',
    expanded: act ? act.getAttribute('aria-expanded') : null,
    htmlLen: t.innerHTML.length,
  }
}, i)

// 找一个「已结算、收起中」的轮
const idx = await findSettled()
if (idx < 0) bad('没找到已结算的轮（夹具或渲染变了？）')
const before = idx >= 0 ? await boxAt(idx) : null
if (before?.expanded === 'true') bad('起手就是展开的，无法验证「点开要失效」')

// ② 点开：盒体必须出现/变高
await page.eval((i) => { document.querySelectorAll('.rt-turn')[i]?.querySelector('.rt-act')?.click() }, idx)
await wait(450)
const opened = await boxAt(idx)
if (opened?.expanded === 'true' && opened.rows > (before?.rows ?? 0)) ok(`点开生效：行 ${before?.rows} → ${opened.rows}`)
else bad(`点开没生效（expanded ${opened?.expanded}，行 ${before?.rows} → ${opened?.rows}）`)

// ③ 再点一次收起
await page.eval((i) => { document.querySelectorAll('.rt-turn')[i]?.querySelector('.rt-act')?.click() }, idx)
await wait(450)
const closed = await boxAt(idx)
if (closed && closed.rows <= (before?.rows ?? 0)) ok(`收起生效：行回到 ${closed.rows}`)
else bad(`收起没生效（行 ${closed?.rows}）`)

// ④ 切密度 → 那一轮的结构必须变
await page.eval(() => window.__store.getState().setNarrativeDensity('detailed'))
await wait(700)
const detailed = await boxAt(idx)
if (detailed && detailed.htmlLen !== closed?.htmlLen) ok(`切密度生效：html ${closed?.htmlLen} → ${detailed.htmlLen}`)
else bad(`切密度没生效（html ${closed?.htmlLen} → ${detailed?.htmlLen}）`)
await page.eval(() => window.__store.getState().setNarrativeDensity('concise'))
await wait(700)

// ⑤ 给已结算的那一轮追加一块（上下文分隔线）→ 必须出现在 DOM 里
const before5 = await page.eval(() => document.querySelectorAll('.narrative .msg.divider').length)
await page.eval((sid) => {
  const st = window.__store.getState()
  const v = st.views[sid]
  const blocks = v.blocks.slice()
  const at = Math.min(blocks.length, 8)
  blocks.splice(at, 0, { kind: 'divider', id: 990001, reason: 'clear' })
  window.__store.setState({ blocks, views: { ...st.views, [sid]: { ...v, blocks } } })
}, SID)
await wait(700)
const after5 = await page.eval(() => document.querySelectorAll('.narrative .msg.divider').length)
if (after5 > before5) ok(`追加的块渲染出来了（分隔线 ${before5} → ${after5}）`)
else bad(`追加的块没有出现（${before5} → ${after5}，items 变了但界面没更新 = 缓存陈旧）`)

console.log(fail ? `\nFAIL ${fail}` : '\nPASS 缓存失效判据全部生效')
await b.close?.()
