// turnCache 失效诊断：为什么每条事件要重算 82 轮（65.8ms JS、0 个 DOM 变化）。
//
// turnCache 的复用判据（Narrative.tsx:1348-1352）：
//   turnState  = [open, live, multiRun, ti, turns.length]
//   turnGlobals= [lang, density, runEnded, waiting, workspace, openFile,
//                 turnMetrics, openTurns, livePages, animOn, runStartedAt,
//                 mainStartedAt, mainRunId, mainEnded, transcriptKey]
// 任一元素**引用变了** ⇒ 该轮缓存失效 ⇒ 整轮元素重建（React 再靠 memo 兜住 DOM）。
//
// 本探针在真 store 上监听这些输入的引用身份，逐条事件报告「谁变了」。
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
await wait(1500)

const report = await page.eval(async (n) => {
  const store = window.__store
  const d = window.__dispatch
  const sid = 's-real'
  // 记录「每个候选全局量」的引用身份
  const snap = () => {
    const s = store.getState()
    const v = s.views[sid] ?? {}
    return {
      turns: v.turns, turnsLen: (v.turns ?? []).length,
      blocks: v.blocks, blocksLen: (v.blocks ?? []).length,
      runNodes: v.runNodes,
      activeSessionId: s.activeSessionId,
      workspace: s.workspace,
      runEnded: v.runEnded, streaming: v.streaming,
      usage: v.usage, costUsd: v.costUsd,
      todos: v.todos, diffs: v.diffs, agentDetached: v.agentDetached,
    }
  }
  const changes = []
  let prev = snap()
  for (let i = 0; i < n; i++) {
    d({ event_type: 'tool_run_start', session_id: sid, run_id: 'rq', id: 'pz' + i, name: 'bash', arguments: '{"command":"ls"}', timestamp: new Date().toISOString() })
    await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)))
    const cur = snap()
    const diff = []
    for (const k of Object.keys(cur)) {
      if (k.endsWith('Len')) { if (cur[k] !== prev[k]) diff.push(`${k}: ${prev[k]}→${cur[k]}`); continue }
      if (cur[k] !== prev[k]) diff.push(`${k}: 引用变了`)
    }
    changes.push({ i, diff })
    prev = cur
  }
  return changes
}, 8)

console.log('每条事件后变化的 store 输入（turnCache 的复用判据看的就是这些的引用身份）：\n')
for (const c of report) {
  console.log(`  事件 ${c.i}: ${c.diff.length ? c.diff.join('  |  ') : '（无变化）'}`)
}
const seen = new Map()
for (const c of report) for (const d of c.diff) { const k = d.split(':')[0]; seen.set(k, (seen.get(k) ?? 0) + 1) }
console.log('\n汇总（出现次数 / 8 条事件）：')
for (const [k, v] of [...seen].sort((a, z) => z[1] - a[1])) console.log(`  ${k}: ${v}`)
await b.close?.()
srv.close()
