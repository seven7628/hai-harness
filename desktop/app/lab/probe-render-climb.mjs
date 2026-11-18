// 渲染成本分项探针（爬山用）：把「每条事件 83ms」拆成 JS / 布局 / 样式 / markdown 解析。
//
// 用真实会话导出的 blocks 灌进真 store（与 probe-real-transcript-render.mjs 同一条路），
// 量四组数：
//   ① 挂载：DOM 节点、轮数、首帧耗时、LayoutCount/RecalcStyleCount
//   ② 每条事件：ScriptDuration / LayoutDuration / RecalcStyleDuration 三者分账 + rAF 占用
//   ③ 空闲 2s：常驻 rAF 循环每帧读 scrollHeight 的代价（LayoutCount 涨不涨）
//   ④ 堆：usedJSHeapSize vs totalJSHeapSize（**已提交 vs 活对象**，预算轴现在只看前者）
//
// 另外每轮都读 __mdCache.stats()：缓存条目数是 0 ⇒ markdown 每次都重解析。
//
// 用法：node lab/probe-render-climb.mjs <blocks.json> [url]
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'
import { readFileSync } from 'node:fs'
import { createServer } from 'node:http'

const FILE = process.argv[2] ?? '/tmp/real-blocks-46m.json'
const URL = process.argv[3] ?? 'http://localhost:5288/'
const EVENTS = Number(process.env.EVENTS || 12)
const wait = (ms) => new Promise((r) => setTimeout(r, ms))

const raw = readFileSync(FILE, 'utf8')
const srv = createServer((_req, res) => {
  const buf = Buffer.from(raw, 'utf8')
  res.writeHead(200, {
    'content-type': 'application/json',
    'content-length': buf.length,
    'access-control-allow-origin': '*',
  })
  res.end(buf)
})
await new Promise((r) => srv.listen(0, '127.0.0.1', r))
const DATA_URL = `http://127.0.0.1:${srv.address().port}/blocks.json`

const b = await Browser.launch({ width: 1440, height: 900 })
const page = await b.newPage({ width: 1440, height: 900 })
await page.goto(URL, { settleMs: 5000 })
for (let i = 0; i < 200; i++) { if (await page.eval(() => !!window.__store && !!window.__dispatch)) break; await wait(150) }
try { await page.send('Performance.enable', { timeDomain: 'timeTicks' }) } catch { /* 老 client */ }

// —— 测量脚手架：rAF 回调占用 + MutationObserver ——
await page.eval(() => {
  const w = window
  w.__c = { rafMs: 0, rafN: 0, muts: 0, added: 0 }
  const orig = w.requestAnimationFrame.bind(w)
  w.requestAnimationFrame = (cb) => orig((t) => {
    const a = performance.now()
    try { cb(t) } finally { w.__c.rafMs += performance.now() - a; w.__c.rafN++ }
  })
  w.__cReset = () => { w.__c.rafMs = 0; w.__c.rafN = 0; w.__c.muts = 0; w.__c.added = 0 }
})
const metrics = async () => {
  const m = await page.send('Performance.getMetrics')
  const map = {}
  for (const { name, value } of m.metrics) map[name] = value
  return map
}
const D = (a, z) => {
  const keys = ['TaskDuration', 'ScriptDuration', 'LayoutDuration', 'RecalcStyleDuration', 'LayoutCount', 'RecalcStyleCount', 'JSHeapUsedSize']
  const o = {}
  for (const k of keys) o[k] = +((z[k] ?? 0) - (a[k] ?? 0)).toFixed(4)
  return o
}
const heap = () => page.eval(() => {
  const m = performance.memory
  return m ? { used: Math.round(m.usedJSHeapSize / 1048576), total: Math.round(m.totalJSHeapSize / 1048576), limit: Math.round(m.jsHeapSizeLimit / 1048576) } : null
})
const mdStats = () => page.eval(() => (window.__mdCache ? window.__mdCache.stats() : null))

// —— 灌真实 blocks ——
console.log(`数据：${FILE}（${(raw.length / 1048576).toFixed(1)} MB）`)
const t0 = Date.now()
await page.eval(async (arg) => {
  const data = await (await fetch(arg.url)).json()
  const sid = 's-real'
  const st = window.__store.getState()
  const view = {
    ...(st.views[sid] ?? {}),
    blocks: data.blocks, runNodes: data.runNodes ?? [], turns: data.turns ?? [],
    traceEvents: [], approvals: [], questions: [], todos: [], diffs: [], agentDetached: [],
    pendingQueue: [], toolStartTs: {}, reqBlock: {}, thinkStartTs: {}, turnThinkMs: {},
    usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0 },
    git: { is_repo: false, ahead: 0, behind: 0, staged: 0, modified: 0, untracked: 0, conflicted: 0, clean: true },
    gitHasMore: false, gitHistLoading: false, gitLoading: false,
    runEnded: false, aborted: false, compressing: false, streaming: false,
  }
  window.__store.setState({
    views: { ...st.views, [sid]: view },
    activeSessionId: sid,
    blocks: data.blocks, runNodes: data.runNodes ?? [], turns: data.turns ?? [],
  })
}, { url: DATA_URL })
console.log(`灌入 store：${Date.now() - t0} ms`)
await wait(9000)   // 等「铺开」动画放完（每帧 25 块 ⇒ 4140 块要 ~3s）—— 否则测的是「铺开期」不是稳态

const m1 = await page.eval(() => ({
  dom: document.querySelectorAll('*').length,
  turns: document.querySelectorAll('.rt-turn').length,
  msgs: document.querySelectorAll('.narrative .msg').length,
  text: document.querySelector('.narrative')?.innerText?.length ?? 0,
}))
const h1 = await heap()
const md1 = await mdStats()
console.log(`挂载后：DOM ${m1.dom} 节点 / ${m1.turns} 轮 / ${m1.msgs} 条消息 / 正文 ${m1.text} 字符`)
console.log(`堆：活对象 ${h1?.used}MB / 已提交 ${h1?.total}MB（上限 ${h1?.limit}MB）  ⇒  **已提交/活对象 = ${h1 ? (h1.total / Math.max(h1.used, 1)).toFixed(2) : '?'}×**`)
console.log(`md 缓存：${md1 ? `${md1.entries} 条 / ${md1.chars} 字符` : '不可用'}`)

// —— ② 每条事件 ——
await page.eval(() => window.__cReset())
const m0 = await metrics()
const ev = await page.eval(async (arg) => {
  const d = window.__dispatch
  const times = []
  for (let i = 0; i < arg.n; i++) {
    const s = performance.now()
    d({
      event_type: 'tool_run_start', session_id: 's-real', run_id: 'rq', id: 'pz' + i, name: 'bash',
      arguments: '{"command":"ls"}', timestamp: new Date().toISOString(),
    })
    await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)))
    times.push(+(performance.now() - s).toFixed(1))
  }
  const c = window.__c
  return { times, rafMs: Math.round(c.rafMs), rafN: c.rafN, muts: c.muts, added: c.added }
}, { n: EVENTS })
const m2 = await metrics()
const d2 = D(m0, m2)
const md2 = await mdStats()
const avg = ev.times.reduce((a, x) => a + x, 0) / ev.times.length
console.log('')
console.log(`—— 每条事件（${EVENTS} 条，真实数据）——`)
console.log(`  墙钟：${ev.times.join('ms, ')}ms  平均 **${avg.toFixed(1)}ms**`)
console.log(`  分账：script ${d2.ScriptDuration}s / layout ${d2.LayoutDuration}s / style ${d2.RecalcStyleDuration}s`)
console.log(`        layout ${d2.LayoutCount} 次 / style ${d2.RecalcStyleCount} 次 / task ${d2.TaskDuration}s`)
console.log(`  rAF：${ev.rafMs}ms / ${ev.rafN} 次（平均 ${(ev.rafMs / Math.max(ev.rafN, 1)).toFixed(2)}ms/帧）`)
console.log(`  DOM 变更：mut ${ev.muts} / +${ev.added} 节点`)
console.log(`  md 缓存：${md2 ? `${md2.entries} 条 / ${md2.chars} 字符` : '不可用'}` + (md1 && md2 && md2.entries === 0 ? '  ← **0 条 ⇒ 每次渲染都重解析**' : ''))

// —— ③ 空闲 2s ——
await page.eval(() => window.__cReset())
const m3 = await metrics()
await wait(2000)
const m4 = await metrics()
const d4 = D(m3, m4)
const idle = await page.eval(() => ({ rafMs: Math.round(window.__c.rafMs), rafN: window.__c.rafN, muts: window.__c.muts, added: window.__c.added }))
console.log('')
console.log('—— 空闲 2s（无事件）——')
console.log(`  rAF：${idle.rafMs}ms / ${idle.rafN} 次（平均 ${(idle.rafMs / Math.max(idle.rafN, 1)).toFixed(2)}ms/帧）`)
console.log(`  分账：script ${d4.ScriptDuration}s / layout ${d4.LayoutDuration}s / style ${d4.RecalcStyleDuration}s`)
console.log(`        layout ${d4.LayoutCount} 次 / style ${d4.RecalcStyleCount} 次 / task ${d4.TaskDuration}s  ← **空闲时的布局次数就是常驻 rAF 读 scrollHeight 的代价**`)
console.log(`  DOM 变更：mut ${idle.muts} / +${idle.added}`)

// —— ④ 堆 ——
const h2 = await heap()
console.log('')
console.log(`—— 堆 ——`)
console.log(`  活对象 ${h1?.used} → ${h2?.used}MB ｜ 已提交 ${h1?.total} → ${h2?.total}MB`)
console.log(`  **预算轴现在读的是 usedJSHeapSize（活对象）** ⇒ 已提交 ${h2?.total}MB 对它不可见`)

await b.close?.()
srv.close()
