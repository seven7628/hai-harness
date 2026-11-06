// 性能基准：新会话运行时（data-skin=runtime）vs 旧皮肤（current）/ v3。
//
// 同一份**真实 checkpoint**（/tmp/real-cp-settled.json：140 块真实转录，含工具输出、
// 产出卡、子 agent 卡、真实长度的 markdown），比较四件事：
//   ① 挂载（hydrate → 激活 → 首帧后两帧）的耗时与浏览器侧计数（脚本/布局/样式重算）
//   ② 流式追加 120 段正文期间的长任务与 DOM 变更量
//   ③ 空闲 2s 里 **rAF 回调占用**（新实现有常驻校准循环，量一下它到底多贵）
//   ④ DOM 节点数与内存水位
//
// 用法：npx vite --port 5288 &  node lab/bench-runtime.mjs [--blocks=420]
import { readFileSync } from 'node:fs'
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const CP_PATH = process.env.CP_PATH || '/tmp/real-cp-settled.json'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const base = JSON.parse(readFileSync(CP_PATH, 'utf8'))
const MULT = Number((process.argv.find((a) => a.startsWith('--blocks=')) || '').split('=')[1] || 1) // 1 = 141 块；3 ≈ 420 块

// 复制 n 份（id 递增），保持每块的形状不变 —— 用来量「块数增长时的成本曲线」
function scale(cp, mult) {
  if (mult <= 1) return cp
  const out = []
  let id = 1
  for (let k = 0; k < mult; k++) {
    for (const b of cp.blocks) {
      const c = JSON.parse(JSON.stringify(b))
      c.id = id++
      if (Array.isArray(c.items)) c.items.forEach((it, i) => { it.id = id++ })
      out.push(c)
    }
  }
  return { ...cp, blocks: out }
}
const cp = scale(base, MULT)
console.log(`checkpoint: ${cp.blocks.length} blocks (×${MULT})`)

const b = await Browser.launch({ width: 1280, height: 800 })
const page = await b.newPage({ width: 1280, height: 800 })
await page.goto(URL, { settleMs: 4500 })
const waitUntil = async (expr, timeout = 30000, step = 150) => {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) { if (await page.eval(new Function('return (' + expr + ')'))) return true; await wait(step) }
  return false
}
await waitUntil('!!window.__store && !!window.__hydrate')
// Performance 域不开启时 getMetrics 全是 0（实测踩到）
try { await page.send('Performance.enable', { timeDomain: 'timeTicks' }) } catch (e) { console.log('Performance.enable 失败：', String(e).slice(0, 120)) }

// 测量脚手架：长任务 + rAF 回调占用 + MutationObserver
await page.eval(() => {
  const w = window
  w.__bench = { long: [], rafMs: 0, rafN: 0, muts: 0, added: 0 }
  try {
    new PerformanceObserver((list) => {
      for (const e of list.getEntries()) w.__bench.long.push(Math.round(e.duration))
    }).observe({ entryTypes: ['longtask'] })
  } catch { /* 老浏览器没有 longtask */ }
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

const perfMetrics = async () => {
  const m = await page.send('Performance.getMetrics')
  const map = {}
  for (const { name, value } of m.metrics) map[name] = value
  return map
}
const delta = (a, b) => {
  const keys = ['TaskDuration', 'ScriptDuration', 'LayoutDuration', 'RecalcStyleDuration', 'LayoutCount', 'RecalcStyleCount', 'JSHeapUsedSize']
  const out = {}
  for (const k of keys) out[k] = +((b[k] ?? 0) - (a[k] ?? 0)).toFixed(4)
  return out
}

const SID = 'bench-1'
const results = []
const REPS = Number(process.env.REPS || 2)
for (const skin of (process.env.SKINS || 'current,runtime').split(',')) {
  for (const density of ['concise', 'detailed']) {
    for (let rep = 0; rep < REPS + 1; rep++) {
    // 每轮都从干净的页面状态开始：清 view + 重置 store 镜像
    // 皮肤必须走 store 的动作：App.tsx 的 effect 会按 settings.narrative_skin 把
    // <html data-skin> 写回去（直接改属性会在下一次 store 更新时被覆盖 —— 实测踩到，
    // 六个组合全跑成了 current，数据看起来「三种皮肤一模一样」）。
    await page.eval((arg) => {
      const st = window.__store.getState()
      const views = { ...st.views }
      delete views[arg.sid]
      window.__store.setState({ views, activeSessionId: undefined, blocks: [], runNodes: [], runEnded: true })
      st.setNarrativeSkin(arg.skin)
    }, { sid: SID, skin })
    await wait(150)
    const skinNow = await page.eval(() => document.documentElement.getAttribute('data-skin'))
    if (skinNow !== skin) console.log(`  !! 皮肤没生效：想要 ${skin}，实际 ${skinNow}`)
    await page.eval((d) => window.__store.getState().setNarrativeDensity(d), density)
    await wait(250)
    await page.eval(({ cp, sid }) => window.__hydrate(sid, cp, undefined, { live: true }), { cp, sid: SID })
    await page.eval(() => window.__benchReset())
    const m0 = await perfMetrics()
    // ---- ① 挂载：激活 → 首帧 → 再两帧
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
    const m1 = await perfMetrics()
    const mount = delta(m0, m1)
    const mountMut = await page.eval(() => ({ muts: window.__bench.muts, added: window.__bench.added }))
    const domAfterMount = await page.eval(() => ({
      nodes: document.querySelectorAll('.narrative *').length,
      turns: document.querySelectorAll('.rt-turn').length,
      msgs: document.querySelectorAll('.narrative .msg').length,
      h: Math.round(document.querySelector('.narrative').scrollHeight),
    }))

    // ---- ② 流式：120 段正文（每段 ~16ms）
    await page.eval(() => window.__benchReset())
    const m2 = await perfMetrics()
    const stream = await page.eval(async (sid) => {
      const d = window.__dispatch
      d({ event_type: 'agent_start', session_id: sid, run_id: 'bench-run', index: 0, timestamp: new Date().toISOString() })
      d({ event_type: 'llm_start', session_id: sid, request_id: 'bench-r', run_id: 'bench-run', model: 'm', timestamp: new Date().toISOString() })
      for (let i = 0; i < 120; i++) {
        d({ event_type: 'content_chunk', session_id: sid, request_id: 'bench-r', run_id: 'bench-run', content: `第 ${i} 段流式正文，含 \`code\` 与 **强调**。\n\n` })
        await new Promise((r) => setTimeout(r, 16))
      }
      const bb = window.__bench
      return { rafMs: Math.round(bb.rafMs), rafN: bb.rafN, muts: bb.muts, added: bb.added, long: bb.long.slice() }
    }, SID)
    const m3 = await perfMetrics()
    const streamPerf = delta(m2, m3)

    // ---- ③ 空闲 2s：常驻 rAF 循环的成本
    await page.eval(() => window.__benchReset())
    await wait(2000)
    const idle = await page.eval(() => ({ rafMs: Math.round(window.__bench.rafMs), rafN: window.__bench.rafN, long: window.__bench.long.slice(), muts: window.__bench.muts }))
    try { await page.send('HeapProfiler.collectGarbage') } catch { /* 老 client 没有 */ }
    const mem = await page.eval(() => (performance.memory ? Math.round(performance.memory.usedJSHeapSize / 1048576) : null))

    if (rep === 0) { await wait(300); continue } // 第 0 次只热身（JIT / 样式表 / 图片解码）
    results.push({ skin, density, mountMs: Math.round(paint), mount, mountMut, dom: domAfterMount, streamMs: stream, streamPerf, idle, memMB: mem })
    console.log(`\n[${skin} / ${density}] mount ${Math.round(paint)}ms  dom ${domAfterMount.nodes} nodes / ${domAfterMount.turns} turns / h ${domAfterMount.h}`)
    console.log(`  mount  perf: task ${mount.TaskDuration}s script ${mount.ScriptDuration}s layout ${mount.LayoutCount}× style ${mount.RecalcStyleCount}× mut ${mountMut.muts} +${mountMut.added} nodes`)
    console.log(`  stream 120: raf ${stream.rafMs}ms/${stream.rafN} 次, mut ${stream.muts}, +${stream.added} nodes, long ${stream.long.length} (max ${stream.long.length ? Math.max(...stream.long) : 0}ms), script ${streamPerf.ScriptDuration}s`)
    console.log(`  idle 2s:   raf ${idle.rafMs}ms/${idle.rafN} 次, long ${idle.long.length}, heap ${mem}MB`)
    }
  }
}

// ---- 汇总：runtime vs current（同密度比较）
const med = (xs) => { const a = xs.slice().sort((x, y) => x - y); return a.length ? a[Math.floor((a.length - 1) / 2)] : 0 }
const pick = (skin, density) => {
  const rows = results.filter((r) => r.skin === skin && r.density === density)
  if (!rows.length) return null
  return {
    mountMs: med(rows.map((r) => r.mountMs)),
    dom: { nodes: med(rows.map((r) => r.dom.nodes)) },
    streamPerf: { ScriptDuration: med(rows.map((r) => r.streamPerf.ScriptDuration)), LayoutCount: med(rows.map((r) => r.streamPerf.LayoutCount)) },
    idle: { rafMs: med(rows.map((r) => r.idle.rafMs)) },
    memMB: med(rows.map((r) => r.memMB ?? 0)),
  }
}
console.log('\n=== 汇总（runtime 相对 current） ===')
for (const density of ['concise', 'detailed']) {
  const cur = pick('current', density)
  const rt = pick('runtime', density)
  if (!cur || !rt) continue
  const pct = (a, b) => (cur[a] ? `${((rt[a] / cur[a] - 1) * 100).toFixed(0)}%` : '—')
  console.log(`${density}: 挂载 ${cur.mountMs}ms → ${rt.mountMs}ms (${pct('mountMs')})  ` +
    `节点 ${cur.dom.nodes} → ${rt.dom.nodes} (${((rt.dom.nodes / cur.dom.nodes - 1) * 100).toFixed(0)}%)  ` +
    `流式 script ${cur.streamPerf.ScriptDuration}s → ${rt.streamPerf.ScriptDuration}s (${((rt.streamPerf.ScriptDuration / (cur.streamPerf.ScriptDuration || 1) - 1) * 100).toFixed(0)}%)  ` +
    `空闲 raf ${cur.idle.rafMs}ms → ${rt.idle.rafMs}ms  堆 ${cur.memMB}MB → ${rt.memMB}MB`)
}
await b.close?.()
