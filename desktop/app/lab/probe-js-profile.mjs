// JS CPU Profile 归因：每条事件 65.8ms 的 script 到底花在哪个函数上。
//
// 用 CDP 的 Profiler 域抓真实事件流期间的**自耗时**排名（按函数名聚合），
// 直接回答「65ms 的 JS 是谁干的」。
//
// 用法：node lab/probe-js-profile.mjs <blocks.json> [url] [events]
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'
import { readFileSync } from 'node:fs'
import { createServer } from 'node:http'

const FILE = process.argv[2] ?? '/tmp/real-blocks-46m.json'
const URL = process.argv[3] ?? 'http://localhost:5288/'
const EVENTS = Number(process.argv[4] || 10)
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
await wait(2000)

// —— 抓 CPU profile ——
await page.send('Profiler.enable')
await page.send('Profiler.setSamplingInterval', { interval: 100 })   // 0.1ms
await page.send('Profiler.start')
const wall = await page.eval(async (n) => {
  const d = window.__dispatch
  const t0 = performance.now()
  for (let i = 0; i < n; i++) {
    d({ event_type: 'tool_run_start', session_id: 's-real', run_id: 'rq', id: 'pz' + i, name: 'bash', arguments: '{"command":"ls"}', timestamp: new Date().toISOString() })
    await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)))
  }
  return performance.now() - t0
}, EVENTS)
const { profile } = await page.send('Profiler.stop')

// —— 聚合自耗时 ——
const byId = new Map()
for (const n of profile.nodes) byId.set(n.id, n)
const self = new Map()
const counts = new Map()
for (const id of profile.samples ?? []) counts.set(id, (counts.get(id) ?? 0) + 1)
const totalSamples = (profile.samples ?? []).length
const dt = profile.timeDeltas ?? []
// 用 timeDeltas 累加每个采样点的耗时
let acc = 0
const selfUs = new Map()
for (let i = 0; i < (profile.samples ?? []).length; i++) {
  const d = dt[i] ?? 0
  acc += d
  const id = profile.samples[i]
  selfUs.set(id, (selfUs.get(id) ?? 0) + d)
}
for (const [id, us] of selfUs) {
  const n = byId.get(id)
  if (!n) continue
  const f = n.callFrame
  const key = `${f.functionName || '(anonymous)'}  ${(f.url || '').replace(/^https?:\/\/[^/]+/, '')}:${f.lineNumber + 1}`
  self.set(key, (self.get(key) ?? 0) + us)
}
const rows = [...self].sort((a, z) => z[1] - a[1]).slice(0, 30)
const totalUs = rows.reduce((s, r) => s + r[1], 0)
console.log(`事件 ${EVENTS} 条，墙钟 ${wall.toFixed(0)}ms，采样 ${totalSamples} 个 / 覆盖 ${(acc / 1000).toFixed(0)}ms`)
console.log('')
console.log('自耗时 Top 30（按函数）：')
for (const [k, us] of rows) {
  console.log(`  ${(us / 1000).toFixed(1).padStart(7)}ms  ${((us / acc) * 100).toFixed(1).padStart(5)}%  ${k}`)
}
await b.close?.()
srv.close()
