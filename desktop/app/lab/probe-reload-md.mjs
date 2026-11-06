// 「重载（刷新 / 切会话回来）要不要一起改？」—— 先量一遍它的账。
//
// 流式是 O(n²)（每 chunk 重解析整段），重载是 O(n)（每次挂载每个消息解析一次）。
// 但「切会话来回切」会把同一条长回答**反复挂载**，所以要看两个数：
//   ① 一屏 10 条 6k 字的结构化长回答，挂载要多久、其中 markdown 解析占多少
//      （对照：同样字数但纯叙述 —— 差值就是结构化解析 + 表格/代码块的渲染）
//   ② 切走再切回来，第二次挂载还是不是同样的价格（= 解析结果没有跨挂载复用）
//
// 用法：npx vite --port 5288 & node lab/probe-reload-md.mjs
import { readFileSync } from 'node:fs'
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const CP_PATH = process.env.CP_PATH || '/tmp/real-cp-settled.json'
const base = JSON.parse(readFileSync(CP_PATH, 'utf8'))
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const MSGS = 10
const CHARS = 6000

const richBlock = (i) => {
  const unit = [
    `- 第 ${i} 项：**要点** 与 \`inline code\`，还有一点正文。\n- 第二项：普通文本。\n`,
    `| 指标 | 值 |\n|---|---|\n| 第 ${i} 行 | 123 |\n`,
    '```js\nconst x = 1\nconsole.log(x)\n```\n',
    `### 小标题 ${i}\n\n再写一两句正文，长度与普通段落相当，用来把字数顶上去。\n`,
  ]
  let s = ''
  while (s.length < CHARS) s += '\n' + unit[Math.floor(s.length / 200) % unit.length]
  return s.slice(0, CHARS)
}
const plainBlock = (i) => {
  let s = ''
  while (s.length < CHARS) s += `这是第 ${i} 条回答的正文段落，没有特殊结构，用来做同字数的对照。`
  return s.slice(0, CHARS)
}

const b = await Browser.launch({ width: 1280, height: 900 })
const page = await b.newPage({ width: 1280, height: 900 })
await page.goto(URL, { settleMs: 4500 })
for (let i = 0; i < 200; i++) { if (await page.eval(() => !!window.__store && !!window.__hydrate)) break; await wait(150) }
try { await page.send('Performance.enable', { timeDomain: 'timeTicks' }) } catch { /* 老 client */ }
const metrics = async () => {
  const m = await page.send('Performance.getMetrics')
  const map = {}
  for (const { name, value } of m.metrics) map[name] = value
  return map
}
const d = (a, z, k) => +(((z[k] ?? 0) - (a[k] ?? 0))).toFixed(3)

const buildFixture = (variant) => {
  const blocks = JSON.parse(JSON.stringify(base.blocks))
  const targets = blocks.filter((x) => x.kind === 'assistant').slice(0, MSGS)
  targets.forEach((x, i) => {
    x.text = variant === 'rich' ? richBlock(i) : plainBlock(i)
    x.streaming = false
    delete x.thinking
  })
  return blocks
}

const mount = async (sid, blocks, skin = 'runtime') => {
  await page.eval((arg) => {
    const st = window.__store.getState()
    const views = { ...st.views }
    delete views[arg.sid]
    window.__store.setState({ views, activeSessionId: undefined, blocks: [], turns: [], runNodes: [], runEnded: true })
    st.setNarrativeSkin('runtime')
    st.setNarrativeDensity('detailed')
    void arg.skin
  }, { sid, skin })
  await wait(250)
  await page.eval(({ cp, sid }) => window.__hydrate(sid, cp, undefined, { live: true }), { cp: { ...base, blocks }, sid })
  const m0 = await metrics()
  await page.eval((sid) => window.__store.setState((s) => {
    const v = s.views[sid]
    return { activeSessionId: sid, blocks: v.blocks, blocksTotal: v.blocksTotal, blocksCursor: v.blocksCursor, blocksHasMore: v.blocksHasMore, runNodes: v.runNodes, usage: v.usage, turns: v.turns, costUsd: v.costUsd, runEnded: v.runEnded, turnsTotal: v.turnsTotal, todos: v.todos, diffs: v.diffs, agentDetached: v.agentDetached, approvals: v.approvals, workspace: '/mock/workspace' }
  }), sid)
  await wait(1500)
  const m1 = await metrics()
  const dom = await page.eval(() => ({ nodes: document.querySelectorAll('.narrative *').length, h: Math.round(document.querySelector('.narrative')?.scrollHeight ?? 0) }))
  return { script: d(m0, m1, 'ScriptDuration'), style: d(m0, m1, 'RecalcStyleDuration'), layout: d(m0, m1, 'LayoutDuration'), ...dom }
}

const rows = {}
for (const variant of ['plain', 'rich']) {
  const blocks = buildFixture(variant)
  // 第一次挂载
  const a = await mount(`${variant}-1`, blocks)
  // 切走去一个空会话，再切回来（= 切会话的真实形态：组件卸载后重新挂载）
  await page.eval(() => window.__store.setState({ activeSessionId: 'other', blocks: [], turns: [], runNodes: [], runEnded: true }))
  await wait(500)
  const b2 = await mount(`${variant}-1`, blocks)
  rows[variant] = { first: a, again: b2 }
  console.log(`[${variant}] ${MSGS} 条 × ${CHARS} 字`)
  console.log(`   首次挂载 script ${a.script}s / style ${a.style}s / layout ${a.layout}s / ${a.nodes} nodes`)
  console.log(`   切走再回来 script ${b2.script}s / style ${b2.style}s / layout ${b2.layout}s / ${b2.nodes} nodes`)
}

console.log('\n=== 结论 ===')
const share = (1 - rows.plain.first.script / rows.rich.first.script) * 100
console.log(`同字数对照：plain 挂载 ${rows.plain.first.script}s → rich 挂载 ${rows.rich.first.script}s（结构化多花 ${(rows.rich.first.script - rows.plain.first.script).toFixed(3)}s，占 rich 的 ${share.toFixed(0)}%）`)
console.log(`切走再回来：rich ${rows.rich.first.script}s → ${rows.rich.again.script}s（缓存命中的话第二次应当明显更便宜）`)
console.log(`一条 6k 字 rich 回答的挂载成本 ≈ ${((rows.rich.first.script * 1000 - rows.plain.first.script * 1000) / MSGS).toFixed(1)}ms`)
await b.close?.()
