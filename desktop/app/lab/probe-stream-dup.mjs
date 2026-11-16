// 追查「runtime 皮肤流式期间 DOM 变更量 ≈ current 的 2 倍」（bench-runtime 实测
// muts 10007 vs 4920、added 5120 vs 2576，静置节点数却只差 2%）。
//
// 假设：流式正文在 DOM 里被渲染了两份（例如「盒子里一份 + 时间线/测量用一份」），
// 或者每个 chunk 都要重建整段 markdown 的两套包装。
//
// 判据（计算值，不看截图）：
//   A. 同一段正文的字符串在文档里出现几次（textContent 命中数 / 元素数）
//   B. 每来一个 chunk 新增多少元素（按 class 归类），是 1 套还是 2 套
//   C. .narrative 下的文本总长度 vs 期望长度（重复即翻倍）
//
// 用法：npx vite --port 5288 & node lab/probe-stream-dup.mjs
import { readFileSync } from 'node:fs'
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const CP_PATH = process.env.CP_PATH || '/tmp/real-cp-settled.json'
const cp = JSON.parse(readFileSync(CP_PATH, 'utf8'))
const SID = 'dup-1'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
let fail = 0
const ok = (m) => console.log('OK ' + m)
const bad = (m) => { fail++; console.log('!! ' + m) }

const b = await Browser.launch({ width: 1280, height: 800 })
const page = await b.newPage({ width: 1280, height: 800 })
await page.goto(URL, { settleMs: 4500 })
for (let i = 0; i < 200; i++) {
  if (await page.eval(() => !!window.__store && !!window.__hydrate)) break
  await wait(150)
}

const setup = async (skin) => {
  await page.eval((arg) => {
    const st = window.__store.getState()
    const views = { ...st.views }
    delete views[arg.sid]
    window.__store.setState({ views, activeSessionId: undefined, blocks: [], runNodes: [], runEnded: true })
    st.setNarrativeDensity('concise')
  }, { sid: SID, skin })
  await wait(200)
  await page.eval(({ cp, sid }) => window.__hydrate(sid, cp, undefined, { live: true }), { cp, sid: SID })
  await page.eval((sid) => window.__store.setState((s) => {
    const v = s.views[sid]
    return { activeSessionId: sid, blocks: v.blocks, blocksTotal: v.blocksTotal, blocksCursor: v.blocksCursor, blocksHasMore: v.blocksHasMore, runNodes: v.runNodes, usage: v.usage, turns: v.turns, costUsd: v.costUsd, runEnded: v.runEnded, turnsTotal: v.turnsTotal, todos: v.todos, diffs: v.diffs, agentDetached: v.agentDetached, approvals: v.approvals, workspace: '/mock/workspace' }
  }), SID)
  await wait(600)
  return page.eval(() => document.documentElement.getAttribute('data-skin'))
}

// 每来一个 chunk：新增元素按 class 归类（只看新加的元素，不看总数）
const streamOne = (n) => page.eval(async (arg) => {
  const root = document.querySelector('.narrative')
  const added = []
  const obs = new MutationObserver((recs) => {
    for (const r of recs) for (const el of r.addedNodes) if (el.nodeType === 1) added.push(el)
  })
  obs.observe(root, { childList: true, subtree: true })
  window.__dispatch({
    event_type: 'content_chunk', session_id: arg.sid, request_id: 'dup-r', run_id: 'dup-run',
    content: `段落 ${arg.n}：含 \`code\` 与 **强调**，还有一点长一点的正文用来触发换行与增量 markdown 解析。\n\n`,
  })
  await new Promise((r) => setTimeout(r, 120))
  obs.disconnect()
  // 归类：取新元素自身的 class（忽略纯文本包装的重复计数）
  const tally = {}
  for (const el of added) {
    const k = '.' + (el.className || el.tagName.toLowerCase()).toString().split(/\s+/).slice(0, 2).join('.')
    tally[k] = (tally[k] || 0) + 1
  }
  return { n: added.length, tally }
}, { sid: SID, n })

const snap = () => page.eval(() => {
  const root = document.querySelector('.narrative')
  const txt = root ? root.textContent : ''
  // 每个「段落 N：」在文档里出现几次（正文重复即 > 1）
  const hits = {}
  for (const m of txt.matchAll(/段落 (\d+)：/g)) hits[m[1]] = (hits[m[1]] || 0) + 1
  const dup = Object.entries(hits).filter(([, c]) => c > 1)
  const cls = {}
  for (const el of root ? root.querySelectorAll('*') : []) {
    const k = (el.className || el.tagName).toString().split(/\s+/)[0]
    cls[k] = (cls[k] || 0) + 1
  }
  const top = Object.entries(cls).sort((a, b) => b[1] - a[1]).slice(0, 14)
  return { len: txt.length, dupCount: dup.length, dupSample: dup.slice(0, 5), textLen: txt.length, top }
})

for (const skin of ['current', 'runtime']) {
  const now = await setup(skin)
  if (now !== skin) bad(`皮肤没生效：想要 ${skin}，实际 ${now}`)
  await page.eval((sid) => window.__dispatch({ event_type: 'agent_start', session_id: sid, run_id: 'dup-run', index: 0, timestamp: new Date().toISOString() }), SID)
  await wait(200)
  const per = []
  const t0 = performance.now()
  for (let n = 1; n <= 6; n++) per.push(await streamOne(n))
  const s = await snap()
  console.log(`\n[${skin}] 6 个 chunk 共新增 ${per.reduce((a, p) => a + p.n, 0)} 个元素（每 chunk ${per.map((p) => p.n).join('/')}）  ${Math.round(performance.now() - t0)}ms`)
  console.log(`  正文长度 ${s.textLen}  重复段落 ${s.dupCount} ${s.dupCount ? JSON.stringify(s.dupSample) : ''}`)
  console.log(`  新元素归类（最后一个 chunk）：${JSON.stringify(per[per.length - 1].tally).slice(0, 300)}`)
  console.log(`  DOM top：${JSON.stringify(s.top)}`)
  // 判据：同一段正文出现 2 次以上 = 渲染了两份
  if (s.dupCount > 0) bad(`${skin}: 正文在文档里重复（${s.dupCount} 段）`)
}

console.log(fail ? `\nFAIL ${fail}` : '\nPASS 无重复渲染')
await b.close?.()
