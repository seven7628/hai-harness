// 并发基准：**多会话同时流式**时，前台页面到底付多少代价。
//
// 背景（store 的实现）：后台会话的事件走 setView(sid) → 只改 `views[sid]`，不碰顶层
// `blocks`；流式 chunk 还有 ~32ms 的合并缓冲。理论上前台不该重渲染 —— 这个脚本验证
// 「理论上」是否成立，并量出真实成本。
//
//   A) 只有后台在流（3 个会话 × 40 chunk）：前台 .narrative 的 DOM 变更应恒为 0
//   B) 前后台同时流：与「只有前台流」对照，看多出来的 script 时间
//   C) 每次 chunk 的成本 vs 转录长度（0 / 140 / 420 块）：判断「是否需要 memo 已结算的轮」
//
// 用法：npx vite --port 5288 & node lab/bench-concurrent.mjs
import { readFileSync } from 'node:fs'
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const CP_PATH = process.env.CP_PATH || '/tmp/real-cp-settled.json'
const base = JSON.parse(readFileSync(CP_PATH, 'utf8'))
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const DETAILED = (process.env.DENSITY || 'detailed') === 'detailed'

const scale = (cp, mult) => {
  if (mult <= 1) return cp.blocks
  const out = []
  let id = 1
  for (let k = 0; k < mult; k++) {
    for (const b of cp.blocks) {
      const c = JSON.parse(JSON.stringify(b))
      c.id = id++
      if (Array.isArray(c.items)) c.items.forEach((it) => { it.id = id++ })
      out.push(c)
    }
  }
  return out
}

const b = await Browser.launch({ width: 1280, height: 800 })
const page = await b.newPage({ width: 1280, height: 800 })
await page.goto(URL, { settleMs: 4500 })
for (let i = 0; i < 200; i++) { if (await page.eval(() => !!window.__store && !!window.__hydrate)) break; await wait(150) }
try { await page.send('Performance.enable', { timeDomain: 'timeTicks' }) } catch { /* 老 client */ }

await page.eval(() => {
  const w = window
  w.__cbench = { rafMs: 0, rafN: 0, muts: 0, added: 0 }
  const orig = w.requestAnimationFrame.bind(w)
  w.requestAnimationFrame = (cb) => orig((t) => {
    const a = performance.now()
    try { cb(t) } finally { w.__cbench.rafMs += performance.now() - a; w.__cbench.rafN++ }
  })
  w.__cReset = () => {
    w.__cbench.rafMs = 0; w.__cbench.rafN = 0; w.__cbench.muts = 0; w.__cbench.added = 0
    if (w.__cObs) w.__cObs.disconnect()
    const el = document.querySelector('.narrative')
    if (!el) return
    w.__cObs = new MutationObserver((recs) => {
      w.__cbench.muts += recs.length
      for (const r of recs) w.__cbench.added += r.addedNodes.length
    })
    w.__cObs.observe(el, { childList: true, subtree: true, characterData: true })
  }
})
const metrics = async () => {
  const m = await page.send('Performance.getMetrics')
  const map = {}
  for (const { name, value } of m.metrics) map[name] = value
  return map
}
const dScript = (a, z) => +(((z.ScriptDuration ?? 0) - (a.ScriptDuration ?? 0))).toFixed(4)
// 分段成本：script（React/JS）、style（选择器匹配与重算）、layout —— 3 倍到底花在哪一段
const dAll = (a, z) => ({
  script: +(((z.ScriptDuration ?? 0) - (a.ScriptDuration ?? 0))).toFixed(4),
  style: +(((z.RecalcStyleDuration ?? 0) - (a.RecalcStyleDuration ?? 0))).toFixed(4),
  layout: +(((z.LayoutDuration ?? 0) - (a.LayoutDuration ?? 0))).toFixed(4),
  task: +(((z.TaskDuration ?? 0) - (a.TaskDuration ?? 0))).toFixed(4),
  styles: +(((z.RecalcStyleCount ?? 0) - (a.RecalcStyleCount ?? 0))),
  layouts: +(((z.LayoutCount ?? 0) - (a.LayoutCount ?? 0))),
})

const FOREGROUND = 'fg-1'
const BG = ['bg-1', 'bg-2', 'bg-3']

// 后台视图：hydrate 但不激活（setView 的 active 为 false → 只写 views[sid]）
const hydrateBg = async (sid, blocks, skin, density) => {
  await page.eval((arg) => {
    const st = window.__store.getState()
    st.setNarrativeSkin(arg.skin)
    st.setNarrativeDensity(arg.d)
  }, { skin, d: density })
  await page.eval(({ cp, sid }) => window.__hydrate(sid, cp, undefined, { live: true }), { cp: { ...base, blocks }, sid })
  await wait(250)
}

const activate = async (sid, blocks) => {
  await page.eval(({ cp, sid }) => window.__hydrate(sid, cp, undefined, { live: true }), { cp: { ...base, blocks }, sid })
  await page.eval((sid) => window.__store.setState((s) => {
    const v = s.views[sid]
    return { activeSessionId: sid, blocks: v.blocks, blocksTotal: v.blocksTotal, blocksCursor: v.blocksCursor, blocksHasMore: v.blocksHasMore, runNodes: v.runNodes, usage: v.usage, turns: v.turns, costUsd: v.costUsd, runEnded: v.runEnded, turnsTotal: v.turnsTotal, todos: v.todos, diffs: v.diffs, agentDetached: v.agentDetached, approvals: v.approvals, workspace: '/mock/workspace' }
  }), sid)
  await wait(500)
}

// 在页面里并发跑：前台 + 后台各自按节奏发 chunk（都在页面内，避免 CDP 往返抖动）
const runStreams = async ({ fgChunks, bgChunks, bgCount, gapMs, runTag }) => {
  await page.eval(() => window.__cReset())
  const m0 = await metrics()
  const r = await page.eval(async (arg) => {
    const d = window.__dispatch
    const now = () => new Date().toISOString()
    const workers = []
    const streamOne = async (sid, n, tag) => {
      d({ event_type: 'agent_start', session_id: sid, run_id: tag, index: 0, timestamp: now() })
      for (let i = 0; i < n; i++) {
        d({ event_type: 'content_chunk', session_id: sid, request_id: tag + '-r', run_id: tag, content: `第 ${i} 段流式正文，含 \`code\` 与 **强调**。\n\n` })
        await new Promise((r) => setTimeout(r, arg.gap))
      }
      d({ event_type: 'agent_end', session_id: sid, run_id: tag, timestamp: now() })
    }
    if (arg.fg > 0) workers.push(streamOne(arg.fgSid, arg.fg, 'fg-run'))
    for (let k = 0; k < arg.bgCount; k++) workers.push(streamOne(arg.bgSids[k], arg.bg, 'bg-run-' + k))
    await Promise.all(workers)
    const c = window.__cbench
    return { muts: c.muts, added: c.added, rafMs: Math.round(c.rafMs), rafN: c.rafN }
  }, { fg: fgChunks, bg: bgChunks, bgCount, gap: gapMs, fgSid: FOREGROUND, bgSids: BG })
  const m1 = await metrics()
  return { ...r, script: dScript(m0, m1), all: dAll(m0, m1) }
}

const skin = process.env.SKIN || 'runtime'
const density = DETAILED ? 'detailed' : 'concise'
console.log(`皮肤 ${skin} / 密度 ${density} / checkpoint ${base.blocks.length} 块\n`)

// 建 3 个后台视图（先各 hydrate 一份，模拟「后台会话有历史」）
for (const sid of BG) await hydrateBg(sid, base.blocks, skin, density)
await activate(FOREGROUND, base.blocks)
await wait(800)

const skipAB = process.env.ONLY === 'C'
if (!skipAB) {
// ---- A) 只有后台在流
const A = await runStreams({ fgChunks: 0, bgChunks: 40, bgCount: 3, gapMs: 40 })
console.log(`A 只有后台流（3×40 chunk）: 前台 DOM 变更 ${A.muts} / +${A.added}  script ${A.script}s  raf ${A.rafMs}ms/${A.rafN}`)

// B1/B2 必须**各自从干净的视图开始**：前台在同一轮里连流两遍会让正文长度翻倍，
// 第二轮光是 markdown 重新解析就更贵 —— 第一次跑就是这么把「并发成本」量错了 3 倍。
// ---- B1) 只有前台流
await activate(FOREGROUND, base.blocks)
await wait(600)
const B1 = await runStreams({ fgChunks: 60, bgChunks: 0, bgCount: 0, gapMs: 25 })
console.log(`B1 只有前台流（60 chunk）:   DOM 变更 ${B1.muts} / +${B1.added}  script ${B1.script}s`)
await wait(600)

// ---- B2) 前台 + 3 个后台同时流（前台同样从干净视图开始）
await activate(FOREGROUND, base.blocks)
await wait(600)
const B2 = await runStreams({ fgChunks: 60, bgChunks: 60, bgCount: 3, gapMs: 25 })
console.log(`B2 前台+3 后台同时流:        DOM 变更 ${B2.muts} / +${B2.added}  script ${B2.script}s  raf ${B2.rafMs}ms/${B2.rafN}`)
console.log(`   → 后台带来的额外 script ${(B2.script - B1.script).toFixed(3)}s（含 3×60 个 chunk 的处理）`)
await wait(600)

}

// ---- C) 每次 chunk 的成本 vs 转录长度（只看前台，单会话）
const perChunk = []
for (const mult of [0, 1, 3]) {
  const blocks = mult === 0 ? [] : scale(base, mult)
  await activate(FOREGROUND, blocks)
  await wait(700)
  const r = await runStreams({ fgChunks: 60, bgChunks: 0, bgCount: 0, gapMs: 20 })
  const msPerChunk = (r.script * 1000) / 60
  perChunk.push({ blocks: blocks.length, script: r.script, msPerChunk: +msPerChunk.toFixed(2), muts: r.muts, all: r.all })
  const a = r.all
  console.log(`C 转录 ${String(blocks.length).padStart(3)} 块: 60 chunk 的 script ${r.script}s（${msPerChunk.toFixed(2)}ms/chunk）  `
    + `style ${a.style}s/${a.styles}×  layout ${a.layout}s/${a.layouts}×  DOM 变更 ${r.muts}`)
}
const [p0, p1, p3] = perChunk
console.log(`\n结论`)
console.log(`  分块成本随转录增长：0 块 ${p0.msPerChunk}ms/chunk → ${p1.blocks} 块 ${p1.msPerChunk}ms/chunk → ${p3.blocks} 块 ${p3.msPerChunk}ms/chunk`)
console.log(`  斜率 ≈ ${((p3.msPerChunk - p0.msPerChunk) / Math.max(p3.blocks, 1) * 1000).toFixed(2)}µs / 块 / chunk`
  + ` ⇒ ${p3.blocks} 块时每 chunk 有 ${(p3.msPerChunk - p0.msPerChunk).toFixed(2)}ms 花在「与流式无关的已有内容」上`)
if (!skipAB) {
  console.log(`  后台并发：前台 DOM 变更 ${A.muts}（0 = 后台事件没有让前台重渲染），`
    + `3 个后台各 60 chunk 额外花 ${(B2.script - B1.script).toFixed(3)}s script`)
}
await b.close?.()
