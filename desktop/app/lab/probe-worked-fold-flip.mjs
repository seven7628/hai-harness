// 探针：主 run 已 AgentEnd（Worked 已折叠）之后，**后台/被 promote 的 run** 起跑
// 会不会把上一轮重新展开（高度 1fr ⇄ 0fr 来回）—— 用户 2026-11：抖动持续 2~4s 就停。
import { readFileSync } from 'node:fs'
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'
const URL = process.env.LAB_URL || 'http://localhost:5288/'
const CP_PATH = process.env.CP_PATH || '/tmp/real-cp-settled.json'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const cp = JSON.parse(readFileSync(CP_PATH, 'utf8'))
const b = await Browser.launch({ width: 1280, height: 720 })
const page = await b.newPage({ width: 1280, height: 720 })
await page.goto(URL, { settleMs: 4500 })
const waitUntil = async (expr, timeout = 30000, step = 150) => {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) { if (await page.eval(new Function('return (' + expr + ')'))) return true; await wait(step) }
  return false
}
await waitUntil('!!window.__store && !!window.__hydrate')
await page.eval(() => window.__store.getState().setNarrativeDensity('concise'))
await page.eval(() => { document.documentElement.dataset.skin = 'runtime' })
await wait(300)
await page.eval((cp) => window.__hydrate('real-2', cp, undefined, { live: true }), cp)
await wait(400)
await page.eval(() => {
  window.__store.setState((s) => {
    const v = s.views['real-2']
    return { activeSessionId: 'real-2', blocks: v.blocks, blocksTotal: v.blocksTotal, blocksCursor: v.blocksCursor, blocksHasMore: v.blocksHasMore, runNodes: v.runNodes, usage: v.usage, turns: v.turns, costUsd: v.costUsd, runEnded: v.runEnded, turnsTotal: v.turnsTotal, todos: v.todos, diffs: v.diffs, agentDetached: v.agentDetached, approvals: v.approvals, workspace: '/mock/workspace' }
  })
})
await wait(2000)
const before = await page.eval(() => {
  const el = document.querySelector('.narrative')
  const turns = [...document.querySelectorAll('.rt-turn')]
  const last = turns.at(-1)
  const body = last?.querySelector('.rt-turn-body')
  return { top: Math.round(el.scrollTop), max: Math.round(el.scrollHeight - el.clientHeight), h: Math.round(el.scrollHeight), turns: turns.length, lastOpen: last?.getAttribute('data-open'), bodyH: body ? Math.round(body.getBoundingClientRect().height) : null, lastH: last ? Math.round(last.getBoundingClientRect().height) : null }
})
console.log('起点（已结算）', JSON.stringify(before))

// 后台/被 promote 的顶层 run：agent_start **不带 parent_run_id**（promote 后的形态）
const res = await page.eval(async ({ ms }) => {
  const d = window.__dispatch
  const sid = 'real-2'
  const ts = () => new Date().toISOString()
  const rows = []
  let i = 0
  const t0 = performance.now()
  // 每 700ms 起停一次顶层 run（模拟 2~4s 内多个后台任务收尾/起跑）
  const tid = setInterval(() => {
    i++
    const rid = `promo-${i}`
    d({ event_type: 'agent_start', session_id: sid, run_id: rid, index: 0, content: `promoted task ${i}`, timestamp: ts() })
    d({ event_type: 'llm_start', session_id: sid, request_id: `pr${i}`, run_id: rid, model: 'm', timestamp: ts() })
    d({ event_type: 'content_chunk', session_id: sid, request_id: `pr${i}`, run_id: rid, content: `后台任务 ${i} 的输出\n` })
    d({ event_type: 'tool_run_start', session_id: sid, id: `pt${i}`, name: 'bash', arguments: '{"command":"echo hi"}', run_id: rid, timestamp: ts() })
    d({ event_type: 'tool_run_end', session_id: sid, id: `pt${i}`, name: 'bash', result: 'hi', is_error: false, run_id: rid, timestamp: ts() })
    d({ event_type: 'llm_end', session_id: sid, request_id: `pr${i}`, run_id: rid, model: 'm', finish_reason: 'stop', usage: { Input: 10, Output: 5, CacheRead: 0 }, timestamp: ts() })
    d({ event_type: 'agent_end', session_id: sid, run_id: rid, index: 1, content: 'done', finish_reason: 'end', model: 'm', timestamp: ts() })
  }, 700)
  await new Promise((resolve) => {
    const tick = () => {
      const el = document.querySelector('.narrative')
      const turns = [...document.querySelectorAll('.rt-turn')]
      const last = turns.at(-1)
      const body = last?.querySelector('.rt-turn-body')
      rows.push({
        t: Math.round(performance.now() - t0),
        top: Math.round(el.scrollTop), max: Math.round(el.scrollHeight - el.clientHeight), h: Math.round(el.scrollHeight),
        open: last?.getAttribute('data-open') || null,
        bodyH: body ? Math.round(body.getBoundingClientRect().height) : null,
        lastH: last ? Math.round(last.getBoundingClientRect().height) : null,
      })
      if (performance.now() - t0 < ms) requestAnimationFrame(tick)
      else resolve()
    }
    requestAnimationFrame(tick)
  })
  clearInterval(tid)
  return rows
}, { ms: 6000 })
const flips = (arr) => { let f = 0; for (let i = 2; i < arr.length; i++) { const d1 = arr[i - 1] - arr[i - 2], d2 = arr[i] - arr[i - 1]; if (d1 && d2 && Math.sign(d1) !== Math.sign(d2)) f++ } return f }
const hs = res.map((r) => r.h), tops = res.map((r) => r.top)
console.log(`frames=${res.length} h ${Math.min(...hs)}..${Math.max(...hs)} flip=${flips(hs)}   top ${Math.min(...tops)}..${Math.max(...tops)}`)
const opens = res.filter((r, i) => i === 0 || r.open !== res[i - 1].open)
console.log('data-open 变化:', opens.map((r) => `${r.t}:${r.open} bodyH=${r.bodyH} lastH=${r.lastH}`).join(' '))
await b.close?.()
