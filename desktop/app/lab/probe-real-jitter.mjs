// 探针：用**真实会话 checkpoint**（~/.go-code/sessions/*.record.json 的 view_checkpoint）
// 复现「主 run 已 AgentEnd、子 agent 还在跑时，底部内容原地上下颤」。
//
// 与 mock 造的场景不同，这里用的是真实块形状（1223 块会话里截取的 140 块，含工具输出、
// 产出卡、子 agent 卡、真实长度的 markdown）。
//
// 用法：
//   1) python3 生成 /tmp/real-cp.json（见 lab/README-real-cp.md 里的片段）
//   2) npx vite --port 5288 &
//   3) node lab/probe-real-jitter.mjs
import { readFileSync } from 'node:fs'
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const CP_PATH = process.env.CP_PATH || '/tmp/real-cp.json'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const cp = JSON.parse(readFileSync(CP_PATH, 'utf8'))
const b = await Browser.launch({ width: 1280, height: 720 })
const page = await b.newPage({ width: 1280, height: 720 })
await page.goto(URL, { settleMs: 4500 })
const waitUntil = async (expr, timeout = 30000, step = 150) => {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) {
    if (await page.eval(new Function('return (' + expr + ')'))) return true
    await wait(step)
  }
  return false
}
await waitUntil('!!window.__store && !!window.__hydrate')
const DENSITY = process.env.DENSITY === 'detailed' ? 'detailed' : 'concise'
await page.eval((d) => window.__store.getState().setNarrativeDensity(d), DENSITY)
await page.eval(() => { document.documentElement.dataset.skin = 'runtime' })
await wait(300)

const SID = 'real-cp-1'
await page.eval((cp) => {
  window.__hydrate('real-cp-1', cp, undefined, { live: true })
}, cp)
await wait(600)
const hydrated = await page.eval(() => {
  const s = window.__store.getState()
  const v = s.views['real-cp-1']
  return v ? { blocks: v.blocks.length, runEnded: v.runEnded, runs: v.runNodes.map((n) => [n.id, n.status, n.parentId || null]) } : null
})
console.log('hydrated', JSON.stringify(hydrated))
// 直接激活这份视图（不走 setActiveSession：mock 不认识这个 sid）
await page.eval(() => {
  window.__store.setState((s) => {
    const sid = 'real-cp-1'
    const v = s.views[sid]
    return {
      activeSessionId: sid,
      blocks: v.blocks, blocksTotal: v.blocksTotal, blocksCursor: v.blocksCursor, blocksHasMore: v.blocksHasMore,
      runNodes: v.runNodes, usage: v.usage, turns: v.turns, costUsd: v.costUsd,
      runEnded: v.runEnded, turnsTotal: v.turnsTotal, todos: v.todos, diffs: v.diffs,
      agentDetached: v.agentDetached, approvals: v.approvals, enteredAt: v.enteredAt,
      workspace: '/mock/workspace',
    }
  })
})
await wait(2500)
const st = await page.eval(() => {
  const el = document.querySelector('.narrative')
  return { top: Math.round(el.scrollTop), max: Math.round(el.scrollHeight - el.clientHeight), ch: Math.round(el.clientHeight), kids: el.children.length }
})
console.log('activated', JSON.stringify(st), 'density', DENSITY)

// 逐帧采样：跟踪每个**同一个 DOM 元素**的高度，找出「反复上下变」的
const SUB = JSON.parse(readFileSync(CP_PATH, 'utf8')).run_nodes.find((n) => n.parent_id)?.id
const res = await page.eval(async ({ sid, sub, ms }) => {
  const root = () => document.querySelector('.narrative')
  const series = {}
  const rows = []
  let i = 0
  const t0 = performance.now()
  const tid = setInterval(() => {
    i++
    window.__dispatch({ event_type: 'llm_start', session_id: sid, request_id: `r${i}`, run_id: sub, parent_run_id: 'main-real-1', model: 'm', timestamp: new Date().toISOString() })
    window.__dispatch({ event_type: 'content_chunk', session_id: sid, request_id: `r${i}`, run_id: sub, parent_run_id: 'main-real-1', content: `子 agent 第 ${i} 步：检查 ${i % 5 + 1} 处调用点\n` })
    window.__dispatch({ event_type: 'llm_end', session_id: sid, request_id: `r${i}`, run_id: sub, parent_run_id: 'main-real-1', model: 'm', finish_reason: 'tool_call', usage: { Input: 10, Output: 5, CacheRead: 0 }, timestamp: new Date().toISOString() })
    window.__dispatch({ event_type: 'tool_run_start', session_id: sid, id: `t${i}`, name: 'bash', arguments: '{"command":"grep -rn os.UserHomeDir"}', run_id: sub, parent_run_id: 'main-real-1', timestamp: new Date().toISOString() })
    window.__dispatch({ event_type: 'tool_run_end', session_id: sid, id: `t${i}`, name: 'bash', result: 'session/restore.go:142\n'.repeat(i % 3 + 1), is_error: false, run_id: sub, parent_run_id: 'main-real-1', timestamp: new Date().toISOString() })
  }, 260)
  await new Promise((resolve) => {
    const tick = () => {
      const el = root()
      if (!el) { requestAnimationFrame(tick); return }
      // 按**路径**（兄弟序号链）跟踪：元素被重建（remount）时路径不变，
      // 才能看见「入场动画被重播」那种抖动（按节点身份跟踪会漏掉）。
      const walk = (node, path) => {
        const r = node.getBoundingClientRect()
        let rec = series[path]
        if (!rec) {
          rec = series[path] = { h: [], top: [], cls: (node.tagName.toLowerCase() + (node.className ? '.' + String(node.className).split(/\s+/).filter(Boolean)[0] : '')).slice(0, 60) }
        }
        rec.h.push(Math.round(r.height * 10) / 10)
        rec.top.push(Math.round(r.top * 10) / 10)
        const kids = node.children
        for (let ci = 0; ci < kids.length; ci++) walk(kids[ci], path + '/' + ci)
      }
      const kids = el.children
      for (let ci = 0; ci < kids.length; ci++) walk(kids[ci], String(ci))
      const last = el.lastElementChild
      rows.push({ t: Math.round(performance.now() - t0), top: Math.round(el.scrollTop), max: Math.round(el.scrollHeight - el.clientHeight), h: Math.round(el.scrollHeight), lastH: last ? Math.round(last.getBoundingClientRect().height) : null, lastCls: last ? String(last.className).slice(0, 40) : '' })
      if (performance.now() - t0 < ms) requestAnimationFrame(tick)
      else resolve()
    }
    requestAnimationFrame(tick)
  })
  clearInterval(tid)
  const flips = (arr) => { let f = 0; for (let k = 2; k < arr.length; k++) { const d1 = arr[k - 1] - arr[k - 2], d2 = arr[k] - arr[k - 1]; if (d1 && d2 && Math.sign(d1) !== Math.sign(d2)) f++ } return f }
  const out = []
  for (const path of Object.keys(series)) {
    const rec = series[path]
    if (!rec || rec.h.length < rows.length * 0.5) continue
    const hf = flips(rec.h), tf = flips(rec.top)
    const hLo = Math.min(...rec.h), hHi = Math.max(...rec.h)
    const tLo = Math.min(...rec.top), tHi = Math.max(...rec.top)
    if (hf >= 3 || (tf >= 3 && tHi - tLo >= 1)) {
      out.push({ path, cls: rec.cls, hf, tf, hLo, hHi, tLo, tHi, n: rec.h.length })
    }
  }
  out.sort((x, y) => Math.max(y.hf, y.tf) - Math.max(x.hf, x.tf))
  return { rows, out: out.slice(0, 24), tracked: Object.keys(series).length }
}, { sid: SID, sub: SUB, ms: 8000 })
const flips = (arr) => { let f = 0; for (let i = 2; i < arr.length; i++) { const d1 = arr[i - 1] - arr[i - 2], d2 = arr[i] - arr[i - 1]; if (d1 && d2 && Math.sign(d1) !== Math.sign(d2)) f++ } return f }
const tops = res.rows.map((r) => r.top), hs = res.rows.map((r) => r.h)
const lastHs = res.rows.map((r) => r.lastH).filter((v) => v != null)
console.log(`frames=${res.rows.length} tracked=${res.tracked}`)
console.log(`scrollTop ${Math.min(...tops)}..${Math.max(...tops)} (flip=${flips(tops)})  scrollHeight ${Math.min(...hs)}..${Math.max(...hs)} (flip=${flips(hs)})`)
if (lastHs.length) console.log(`最后一个子元素 ${Math.min(...lastHs)}..${Math.max(...lastHs)} (flip=${flips(lastHs)}) ${res.rows[0].lastCls}`)
console.log('抖动元素（高度或 top 方向翻转 ≥3 次）:')
for (const o of res.out) console.log(`  hflip=${o.hf} tflip=${o.tf} h=${o.hLo}..${o.hHi} top=${o.tLo}..${o.tHi}  ${o.cls}  @${o.path.slice(0, 40)}`)
await b.close?.()
