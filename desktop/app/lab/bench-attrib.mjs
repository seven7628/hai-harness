// 归因基准：把 bench-runtime 的「流式 script 时间 +45%」拆成两半，看是哪一半。
//
//   A) **空会话**里流式 120 段 —— 量「流式这条路径本身」（markdown 增量解析 + 盒子里
//      的正文重渲染），与预置的 140 块静态内容无关。
//   B) **420 块**（--blocks=3）的挂载 —— 量「长转录的规模曲线」，content-visibility
//      真正该起作用的地方。
//
// 用法：npx vite --port 5288 & node lab/bench-attrib.mjs [--blocks=3]
import { readFileSync } from 'node:fs'
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const CP_PATH = process.env.CP_PATH || '/tmp/real-cp-settled.json'
const MULT = Number((process.argv.find((a) => a.startsWith('--blocks=')) || '').split('=')[1] || 3)
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const base = JSON.parse(readFileSync(CP_PATH, 'utf8'))

function scale(cp, mult) {
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
  return { ...cp, blocks: out }
}

const b = await Browser.launch({ width: 1280, height: 800 })
const page = await b.newPage({ width: 1280, height: 800 })
await page.goto(URL, { settleMs: 4500 })
for (let i = 0; i < 200; i++) { if (await page.eval(() => !!window.__store && !!window.__hydrate)) break; await wait(150) }
try { await page.send('Performance.enable', { timeDomain: 'timeTicks' }) } catch { /* 老 client */ }

await page.eval(() => {
  const w = window
  w.__bench = { long: [], rafMs: 0, rafN: 0, muts: 0, added: 0 }
  try {
    new PerformanceObserver((list) => { for (const e of list.getEntries()) w.__bench.long.push(Math.round(e.duration)) }).observe({ entryTypes: ['longtask'] })
  } catch { /* 没有 longtask */ }
  const orig = w.requestAnimationFrame.bind(w)
  w.requestAnimationFrame = (cb) => orig((t) => {
    const a = performance.now()
    try { cb(t) } finally { w.__bench.rafMs += performance.now() - a; w.__bench.rafN++ }
  })
  w.__benchReset = () => {
    w.__bench.long = []; w.__bench.rafMs = 0; w.__bench.rafN = 0; w.__bench.muts = 0; w.__bench.added = 0
    const el = document.querySelector('.narrative') || document.querySelector('#root') || document.body
    if (w.__benchObs) w.__benchObs.disconnect()
    if (!el) return
    w.__benchObs = new MutationObserver((recs) => {
      w.__bench.muts += recs.length
      for (const r of recs) w.__bench.added += r.addedNodes.length
    })
    w.__benchObs.observe(el, { childList: true, subtree: true, characterData: true })
  }
})
const metrics = async () => {
  const m = await page.send('Performance.getMetrics')
  const map = {}
  for (const { name, value } of m.metrics) map[name] = value
  return map
}
const d = (a, z, k) => +(((z[k] ?? 0) - (a[k] ?? 0))).toFixed(4)

const SID = 'attr-1'
const prep = async (skin, density, blocks) => {
  await page.eval((arg) => {
    const st = window.__store.getState()
    const views = { ...st.views }
    delete views[arg.sid]
    window.__store.setState({ views, activeSessionId: undefined, blocks: [], runNodes: [], runEnded: true })
    st.setNarrativeSkin(arg.skin)
  }, { sid: SID, skin })
  await wait(150)
  await page.eval((x) => window.__store.getState().setNarrativeDensity(x), density)
  await wait(200)
  const cp = { ...base, blocks }
  await page.eval(({ cp, sid }) => window.__hydrate(sid, cp, undefined, { live: true }), { cp, sid: SID })
  await page.eval(() => window.__benchReset())
  const m0 = await metrics()
  const t0 = await page.eval((sid) => {
    const a = performance.now()
    window.__store.setState((s) => {
      const v = s.views[sid]
      return { activeSessionId: sid, blocks: v.blocks, blocksTotal: v.blocksTotal, blocksCursor: v.blocksCursor, blocksHasMore: v.blocksHasMore, runNodes: v.runNodes, usage: v.usage, turns: v.turns, costUsd: v.costUsd, runEnded: v.runEnded, turnsTotal: v.turnsTotal, todos: v.todos, diffs: v.diffs, agentDetached: v.agentDetached, approvals: v.approvals, workspace: '/mock/workspace' }
    })
    return a
  }, SID)
  const paint = await page.eval((t0) => new Promise((res) => {
    requestAnimationFrame(() => requestAnimationFrame(() => requestAnimationFrame(() => res(performance.now() - t0))))
  }), t0)
  const m1 = await metrics()
  const skinNow = await page.eval(() => document.documentElement.getAttribute('data-skin'))
  if (skinNow !== skin) console.log(`  !! 皮肤没生效：想要 ${skin}，实际 ${skinNow}`)
  return { paint: Math.round(paint), task: d(m0, m1, 'TaskDuration'), script: d(m0, m1, 'ScriptDuration'), nodes: await page.eval(() => document.querySelectorAll('.narrative *').length) }
}

const stream = async (n = 120) => {
  await page.eval(() => window.__benchReset())
  const m0 = await metrics()
  const r = await page.eval(async (arg) => {
    const d = window.__dispatch
    d({ event_type: 'agent_start', session_id: arg.sid, run_id: 'attr-run', index: 0, timestamp: new Date().toISOString() })
    for (let i = 0; i < arg.n; i++) {
      d({ event_type: 'content_chunk', session_id: arg.sid, request_id: 'attr-r', run_id: 'attr-run', content: `第 ${i} 段流式正文，含 \`code\` 与 **强调**。\n\n` })
      await new Promise((r) => setTimeout(r, 16))
    }
    const bb = window.__bench
    return { rafMs: Math.round(bb.rafMs), rafN: bb.rafN, muts: bb.muts, added: bb.added, long: bb.long.slice() }
  }, { sid: SID, n })
  const m1 = await metrics()
  return { ...r, script: d(m0, m1, 'ScriptDuration') }
}

const SKINS = (process.env.SKINS || 'current,runtime').split(',')
const rows = []
for (const skin of SKINS) {
  for (const density of ['concise', 'detailed']) {
    for (let rep = 0; rep < 3; rep++) {
      if (rep === 0) { await prep(skin, density, base.blocks); await wait(200); continue } // 热身
      // A) 空会话流式
      const empty = await prep(skin, density, [])
      await wait(300)
      const s = await stream(120)
      // B) 大转录挂载
      const big = await prep(skin, density, scale(base, MULT).blocks)
      rows.push({ skin, density, stream: s, big, empty })
      console.log(`[${skin}/${density}] 空会话流式: script ${s.script}s mut ${s.muts} +${s.added} raf ${s.rafMs}ms/${s.rafN}  |  420块挂载: ${big.paint}ms script ${big.script}s task ${big.task}s nodes ${big.nodes}  (空 ${empty.nodes} nodes)`)
    }
  }
}

const med = (xs) => { const a = xs.slice().sort((x, y) => x - y); return a.length ? a[Math.floor((a.length - 1) / 2)] : 0 }
const pick = (skin, density) => {
  const r = rows.filter((x) => x.skin === skin && x.density === density)
  if (!r.length) return null
  return {
    sScript: med(r.map((x) => x.stream.script)), sMut: med(r.map((x) => x.stream.muts)), sAdded: med(r.map((x) => x.stream.added)),
    bMount: med(r.map((x) => x.big.paint)), bScript: med(r.map((x) => x.big.script)), bTask: med(r.map((x) => x.big.task)), bNodes: med(r.map((x) => x.big.nodes)),
  }
}
console.log('\n=== 归因（A 空会话流式 / B 大转录挂载） ===')
for (const density of ['concise', 'detailed']) {
  const c = pick('current', density)
  const t = pick('runtime', density)
  if (!c || !t) continue
  const pc = (a, b) => `${((b / (a || 1) - 1) * 100).toFixed(0)}%`
  console.log(`${density}  A 流式 script ${c.sScript}s → ${t.sScript}s (${pc(c.sScript, t.sScript)})  mut ${c.sMut} → ${t.sMut}  +nodes ${c.sAdded} → ${t.sAdded}`)
  console.log(`${density}  B 挂载 ${c.bMount}ms → ${t.bMount}ms (${pc(c.bMount, t.bMount)})  script ${c.bScript}s → ${t.bScript}s (${pc(c.bScript, t.bScript)})  task ${c.bTask}s → ${t.bTask}s  nodes ${c.bNodes} → ${t.bNodes}`)
}
await b.close?.()
