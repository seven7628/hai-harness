// 「干掉常驻 tick 的布局读」能省多少？—— 直接把布局属性 getter 换成缓存值来模拟。
//
// 对照组：正常跑（tick 每帧读 scrollHeight/clientHeight/scrollTop）
// 实验组：这些 getter 返回**首次读到的缓存值** ⇒ tick 的读不再触发强制布局
//
// 同时统计：这些 getter 在空闲 2 秒里被读了多少次、读的是谁（按元素统计）。
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'
import { readFileSync } from 'node:fs'
import { createServer } from 'node:http'

const FILE = process.argv[2] ?? '/tmp/real-blocks-46m.json'
const URL = process.argv[3] ?? 'http://localhost:5288/'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const raw = readFileSync(FILE, 'utf8')
const srv = createServer((_req, res) => {
  const buf = Buffer.from(raw, 'utf8')
  res.writeHead(200, { 'content-type': 'application/json', 'content-length': buf.length, 'access-control-allow-origin': '*' })
  res.end(buf)
})
await new Promise((r) => srv.listen(0, '127.0.0.1', r))
const DATA_URL = `http://127.0.0.1:${srv.address().port}/blocks.json`

const b = await Browser.launch({ width: 1440, height: 900 })
const page = await b.newPage({ width: 1440, height: 900 })
await page.goto(URL, { settleMs: 5000 })
for (let i = 0; i < 200; i++) { if (await page.eval(() => !!window.__store && !!window.__dispatch)) break; await wait(150) }
try { await page.send('Performance.enable', { timeDomain: 'timeTicks' }) } catch { /* 老 client */ }

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
    activeSessionId: sid, blocks: data.blocks, runNodes: data.runNodes ?? [], turns: data.turns ?? [],
  })
}, { url: DATA_URL })
await wait(9000)   // 等「铺开」动画放完（每帧 25 块 ⇒ 4140 块要 ~3s），否则 revealStart 每帧变 ⇒ effect 每帧重跑 ⇒ 布局读每帧都发生

// —— 统计布局属性读 ——
await page.eval(() => {
  const proto = Element.prototype
  const cache = new WeakMap()
  window.__reads = { scrollHeight: 0, clientHeight: 0, scrollTop: 0, byEl: new Map() }
  for (const prop of ['scrollHeight', 'clientHeight', 'scrollTop']) {
    const desc = Object.getOwnPropertyDescriptor(proto, prop)
    if (!desc || !desc.get) continue
    Object.defineProperty(proto, prop, {
      configurable: true,
      get() {
        window.__reads[prop]++
        const key = this.className && typeof this.className === 'string' ? '.' + this.className.split(' ')[0] : (this.tagName || '?')
        window.__reads.byEl.set(key, (window.__reads.byEl.get(key) ?? 0) + 1)
        return desc.get.call(this)
      },
      set(v) { desc.set.call(this, v) },
    })
  }
  window.__scrollN = 0
  const el0 = document.querySelector('.narrative')
  if (el0) el0.addEventListener('scroll', () => { window.__scrollN++ }, { passive: true })
  window.__readsReset = () => {
    window.__scrollN = 0
    window.__reads.scrollHeight = 0
    window.__reads.clientHeight = 0
    window.__reads.scrollTop = 0
    window.__reads.byEl = new Map()
  }
  void cache
})

const metrics = async () => {
  const m = await page.send('Performance.getMetrics')
  const map = {}
  for (const { name, value } of m.metrics) map[name] = value
  return map
}
const D = (a, z) => {
  const o = {}
  for (const k of ['TaskDuration', 'ScriptDuration', 'LayoutDuration', 'RecalcStyleDuration', 'LayoutCount', 'RecalcStyleCount'])
    o[k] = +((z[k] ?? 0) - (a[k] ?? 0)).toFixed(4)
  return o
}
const idle = async (label) => {
  await page.eval(() => window.__readsReset())
  const m0 = await metrics()
  await wait(2000)
  const m1 = await metrics()
  const d = D(m0, m1)
  const r = await page.eval(() => ({
    sh: window.__reads.scrollHeight, ch: window.__reads.clientHeight, st: window.__reads.scrollTop,
    byEl: [...window.__reads.byEl].sort((a, z) => z[1] - a[1]).slice(0, 5),
    scrollN: window.__scrollN,
  }))
  console.log(`  ${label}`)
  console.log(`     layout ${d.LayoutCount} 次 ｜ style ${d.RecalcStyleCount} 次 ｜ layout ${d.LayoutDuration}s ｜ style ${d.RecalcStyleDuration}s ｜ task ${d.TaskDuration}s`)
  console.log(`     布局属性读：scrollHeight ${r.sh} ｜ clientHeight ${r.ch} ｜ scrollTop ${r.st}`)
  console.log(`     scroll 事件：${r.scrollN} 次
     读在谁身上：${r.byEl.map(([k, v]) => `${k}×${v}`).join('  ') || '（无）'}`)
  return d
}

console.log('空闲 2 秒：\n')
await idle('① 现状（无事件）')

// ② 先打 12 条事件，再测空闲 ⇒ 看事件是否把脏标记永久置位
await page.eval(async () => {
  const d = window.__dispatch
  for (let i = 0; i < 12; i++) {
    d({ event_type: 'tool_run_start', session_id: 's-real', run_id: 'rq', id: 'pz' + i, name: 'bash', arguments: '{"command":"ls"}', timestamp: new Date().toISOString() })
    await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)))
  }
})
await wait(1500)
await idle('② 打 12 条事件后的空闲')

// 实验组：布局属性 getter 返回缓存值 ⇒ 读不再触发强制布局
await page.eval(() => {
  const proto = Element.prototype
  const cache = new WeakMap()
  for (const prop of ['scrollHeight', 'clientHeight', 'scrollTop']) {
    const desc = Object.getOwnPropertyDescriptor(proto, prop)
    if (!desc || !desc.get) continue
    Object.defineProperty(proto, prop, {
      configurable: true,
      get() {
        let v = cache.get(this)
        if (v === undefined) { v = desc.get.call(this); cache.set(this, v) }
        return v
      },
      set(v) { cache.set(this, v); desc.set.call(this, v) },
    })
  }
})
await wait(600)
await idle('② 布局属性 getter 改成缓存值（≈ tick 不再触发强制布局）')

await b.close?.()
srv.close()
