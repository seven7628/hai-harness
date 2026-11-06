// 探针 v7：抖动检测器 —— 逐帧量叙述区子树里每个元素的高度，找出「上下来回变」的元素。
// 场景用 mock 的「live output」钩子（tail -f 式运行中输出 + tool_output 节流帧）。
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'
const URL = process.env.LAB_URL || 'http://localhost:5288/'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const b = await Browser.launch({ width: 1280, height: 720 })
const page = await b.newPage({ width: 1280, height: 720 })
await page.goto(URL, { settleMs: 4500 })
const waitUntil = async (expr, timeout = 40000, step = 120) => {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) {
    if (await page.eval(new Function('return (' + expr + ')'))) return true
    await wait(step)
  }
  return false
}
await waitUntil('!!window.__store')
await page.eval(() => window.__store.getState().setNarrativeDensity('detailed'))
await page.eval(() => { document.documentElement.dataset.skin = 'runtime' })
await wait(300)
for (let i = 0; i < 3 && !(await page.eval(() => !!window.__store.getState().activeSessionId)); i++) {
  await page.eval(() => window.__store.getState().newSession())
  await wait(900)
}
const sid = await page.eval(() => window.__store.getState().activeSessionId)
await page.eval(() => window.__store.getState().ask('live output'))
console.log('sid', sid, 'asked live output')

// 采样 12s：逐帧记录叙述区 top/max/ch，以及子树里每个元素的高度（按路径索引）
const res = await page.eval(async (ms) => {
  const root = () => document.querySelector('.narrative')
  const rows = []
  const t0 = performance.now()
  const keyOf = (el, idx) => idx
  await new Promise((resolve) => {
    const tick = () => {
      const e = root()
      const all = [...e.querySelectorAll('*')]
      const hs = all.map((x) => Math.round(x.getBoundingClientRect().height))
      const cls = all.map((x) => x.tagName.toLowerCase() + '.' + (x.className || '').toString().split(' ')[0])
      rows.push({ t: Math.round(performance.now() - t0), top: Math.round(e.scrollTop), max: Math.round(e.scrollHeight - e.clientHeight), ch: Math.round(e.clientHeight), hs, cls })
      if (performance.now() - t0 < ms) requestAnimationFrame(tick)
      else resolve()
    }
    requestAnimationFrame(tick)
  })
  return { rows, keyOf: '' }
}, 12000)
const { rows } = res
const flips = (arr) => { let f = 0; for (let i = 2; i < arr.length; i++) { const d1 = arr[i - 1] - arr[i - 2], d2 = arr[i] - arr[i - 1]; if (d1 && d2 && Math.sign(d1) !== Math.sign(d2)) f++ } return f }
const tops = rows.map((r) => r.top), maxs = rows.map((r) => r.max)
console.log(`frames=${rows.length}`)
console.log(`top ${Math.min(...tops)}..${Math.max(...tops)} range=${Math.max(...tops) - Math.min(...tops)} flip=${flips(tops)}`)
console.log(`max ${Math.min(...maxs)}..${Math.max(...maxs)} range=${Math.max(...maxs) - Math.min(...maxs)} flip=${flips(maxs)}`)
// 逐元素：高度翻转次数最多且 range 明显
const n = Math.max(...rows.map((r) => r.hs.length))
const out = []
for (let i = 0; i < n; i++) {
  const vals = rows.map((r) => r.hs[i]).filter((v) => v != null)
  if (vals.length < rows.length * 0.6) continue
  const f = flips(vals)
  const lo = Math.min(...vals), hi = Math.max(...vals)
  if (f >= 3 && hi - lo >= 2) out.push({ i, f, lo, hi, cls: rows.find((r) => r.cls[i])?.cls[i] })
}
out.sort((a, b) => b.f - a.f)
console.log('抖动元素（flip>=3）:')
for (const o of out.slice(0, 14)) console.log(`  [${o.i}] ${o.cls} h ${o.lo}..${o.hi} flip=${o.f}`)
await b.close?.()
