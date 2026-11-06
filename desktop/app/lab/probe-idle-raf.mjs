// 空闲时的常驻 rAF 校准循环到底花在哪：是不是只花主线程时间片，还是也在真的布局/绘制。
//
// 量四件事（每种皮肤 + 一个「会话页没挂载」的对照组）：
//   ① rAF 回调占用（我们自己的开销）与次数（= 每秒唤醒多少次）
//   ② TaskDuration（主线程总忙时）—— 换算成「每秒占用多少 %」
//   ③ LayoutCount / RecalcStyleCount —— 0 就说明没脏、没重排，只是白跑一遍 JS
//   ④ 热身后的稳定值（前 500ms 丢掉：样式表/字体/首帧）
//
// 用法：npx vite --port 5288 & node lab/probe-idle-raf.mjs
import { readFileSync } from 'node:fs'
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const CP_PATH = process.env.CP_PATH || '/tmp/real-cp-settled.json'
const cp = JSON.parse(readFileSync(CP_PATH, 'utf8'))
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const IDLE = 3000

const b = await Browser.launch({ width: 1280, height: 800 })
const page = await b.newPage({ width: 1280, height: 800 })
await page.goto(URL, { settleMs: 4500 })
for (let i = 0; i < 200; i++) { if (await page.eval(() => !!window.__store && !!window.__hydrate)) break; await wait(150) }
try { await page.send('Performance.enable', { timeDomain: 'timeTicks' }) } catch { /* 老 client */ }

await page.eval(() => {
  const w = window
  w.__raf = { ms: 0, n: 0 }
  const orig = w.requestAnimationFrame.bind(w)
  w.requestAnimationFrame = (cb) => orig((t) => {
    const a = performance.now()
    try { cb(t) } finally { w.__raf.ms += performance.now() - a; w.__raf.n++ }
  })
  w.__rafReset = () => { w.__raf.ms = 0; w.__raf.n = 0 }
})
const metrics = async () => {
  const m = await page.send('Performance.getMetrics')
  const map = {}
  for (const { name, value } of m.metrics) map[name] = value
  return map
}
const d = (a, z, k) => +(((z[k] ?? 0) - (a[k] ?? 0))).toFixed(4)

const SID = 'idle-1'
const mount = async (skin) => {
  await page.eval((arg) => {
    const st = window.__store.getState()
    const views = { ...st.views }
    delete views[arg.sid]
    window.__store.setState({ views, activeSessionId: undefined, blocks: [], runNodes: [], runEnded: true })
    st.setNarrativeSkin(arg.skin)
    st.setNarrativeDensity('detailed')
  }, { sid: SID, skin })
  await wait(250)
  await page.eval(({ cp, sid }) => window.__hydrate(sid, cp, undefined, { live: true }), { cp, sid: SID })
  await page.eval((sid) => window.__store.setState((s) => {
    const v = s.views[sid]
    return { activeSessionId: sid, blocks: v.blocks, blocksTotal: v.blocksTotal, blocksCursor: v.blocksCursor, blocksHasMore: v.blocksHasMore, runNodes: v.runNodes, usage: v.usage, turns: v.turns, costUsd: v.costUsd, runEnded: v.runEnded, turnsTotal: v.turnsTotal, todos: v.todos, diffs: v.diffs, agentDetached: v.agentDetached, approvals: v.approvals, workspace: '/mock/workspace' }
  }), SID)
  await wait(1200)
}
const unmount = async () => {
  await page.eval(() => window.__store.setState({ activeSessionId: undefined, blocks: [], turns: [], runNodes: [] }))
  await wait(1200)
}

const measure = async (label) => {
  await page.eval(() => window.__rafReset())
  const m0 = await metrics()
  await wait(IDLE)
  const m1 = await metrics()
  const raf = await page.eval(() => ({ ms: window.__raf.ms, n: window.__raf.n }))
  const task = d(m0, m1, 'TaskDuration')
  const rows = {
    label,
    fps: +(raf.n / (IDLE / 1000)).toFixed(1),
    rafMsPerSec: +(raf.ms / (IDLE / 1000)).toFixed(2),
    rafMsPerFrame: +(raf.ms / Math.max(raf.n, 1)).toFixed(3),
    taskMsPerSec: +(task / (IDLE / 1000) * 1000).toFixed(2),
    busyPct: +(task / (IDLE / 1000) * 100).toFixed(1),
    layouts: d(m0, m1, 'LayoutCount'),
    styles: d(m0, m1, 'RecalcStyleCount'),
    gc: d(m0, m1, 'MajorGCCount') + d(m0, m1, 'MinorGCCount'),
  }
  console.log(`${label.padEnd(26)} 唤醒 ${String(rows.fps).padStart(5)}/s  rAF ${String(rows.rafMsPerSec).padStart(6)}ms/s (${rows.rafMsPerFrame}ms/帧)  ` +
    `主线程忙 ${String(rows.taskMsPerSec).padStart(6)}ms/s = ${String(rows.busyPct).padStart(4)}%  layout ${rows.layouts} style ${rows.styles} gc ${rows.gc}`)
  return rows
}

const all = []
all.push(await measure('对照：会话页未挂载'))
await mount('current'); all.push(await measure('旧皮肤（current/detailed）'))
await mount('runtime'); all.push(await measure('新皮肤（runtime/detailed）'))
// 再测一次新皮肤：确认是稳态而不是一次性（缓存/首帧尾巴）
all.push(await measure('新皮肤（runtime/detailed）#2'))
await page.eval(() => document.documentElement.setAttribute('data-skin', 'current'))
await wait(800)
all.push(await measure('旧皮肤（current/detailed）#2'))

const rt = all.filter((r) => r.label.startsWith('新皮肤'))
const cu = all.filter((r) => r.label.startsWith('旧皮肤'))
const base = all[0]
const avg = (a, k) => a.reduce((n, r) => n + r[k], 0) / a.length
console.log('\n=== 结论 ===')
console.log(`常驻 rAF 循环的自有开销 ≈ ${(avg(rt, 'rafMsPerFrame') - 0.002).toFixed(3)}ms/帧，`
  + `唤醒 ${avg(rt, 'fps')}/s；相对未挂载时主线程多忙 ${(avg(rt, 'busyPct') - base.busyPct).toFixed(1)} 个百分点`)
console.log(`帧预算占比（60Hz 16.67ms）：${((avg(rt, 'rafMsPerFrame') / 16.67) * 100).toFixed(1)}%`
  + `  120Hz（8.33ms）：${((avg(rt, 'rafMsPerFrame') / 8.33) * 100).toFixed(1)}%`)
console.log(`是否造成重排/重绘：layout ${avg(rt, 'layouts')} 次、style ${avg(rt, 'styles')} 次（每 ${IDLE / 1000}s）`)
await b.close?.()
