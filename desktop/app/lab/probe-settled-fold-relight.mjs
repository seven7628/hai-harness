// 探针：主 run 已 AgentEnd（Worked 已收起）之后，一个**新的顶层 run** 起跑时，
// 已经结算的那一轮会不会被重新展开成 Working（= 页面上下抖 2~4s 的形态）。
// 判据：最后一轮的高度 / data-open / 有没有长出 cy-live。
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const b = await Browser.launch({ width: 1280, height: 720 })
const page = await b.newPage({ width: 1280, height: 720 })
await page.goto('http://localhost:5288/', { settleMs: 4500 })
const waitUntil = async (expr, timeout = 30000, step = 150) => {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) { if (await page.eval(new Function('return (' + expr + ')'))) return true; await wait(step) }
  return false
}
await waitUntil('!!window.__store')
await page.eval(() => window.__store.getState().setNarrativeDensity('concise'))
await page.eval(() => { document.documentElement.dataset.skin = 'runtime' })
await wait(200)
for (let i = 0; i < 3 && !(await page.eval(() => !!window.__store.getState().activeSessionId)); i++) {
  await page.eval(() => window.__store.getState().newSession()); await wait(900)
}
const sid = await page.eval(() => window.__store.getState().activeSessionId)
// 一轮：工具 + 正文 + AgentEnd
await page.eval((sid) => {
  const d = window.__dispatch, ts = () => new Date().toISOString()
  // 块上**不带 run_id**（恢复/旧快照形态）→ turn.run 为空 → sameRun 恒 true，
  // 这是「结算后被 live 重新点亮」最容易复现的一条路。
  d({ event_type: 'user_inputs_consumed', session_id: sid, user_texts: ['跑一下这一轮'], timestamp: ts() })
  d({ event_type: 'agent_start', session_id: sid, run_id: 'R1', index: 0, content: 'main', timestamp: ts() })
  d({ event_type: 'tool_run_start', session_id: sid, id: 't1', name: 'bash', arguments: '{"command":"echo hi"}', timestamp: ts() })
  d({ event_type: 'tool_run_end', session_id: sid, id: 't1', name: 'bash', result: 'hi', is_error: false, timestamp: ts() })
  d({ event_type: 'llm_start', session_id: sid, request_id: 'q1', model: 'm', timestamp: ts() })
  d({ event_type: 'content_chunk', session_id: sid, request_id: 'q1', content: '结论正文。\n\n' + '一段话。\n\n'.repeat(10) })
  d({ event_type: 'llm_end', session_id: sid, request_id: 'q1', model: 'm', finish_reason: 'stop', usage: { Input: 100, Output: 50, CacheRead: 80 }, timestamp: ts() })
  d({ event_type: 'agent_end', session_id: sid, run_id: 'R1', index: 1, content: 'done', finish_reason: 'end', model: 'm', timestamp: ts() })
}, sid)
await wait(1200)
const snap = (label) => page.eval((label) => {
  const el = document.querySelector('.narrative')
  const turns = [...document.querySelectorAll('.rt-turn')]
  const last = turns.find((t) => t.querySelector('.cy-live')) || turns.at(-1)
  return {
    label,
    h: Math.round(el.scrollHeight),
    open: last?.getAttribute('data-open') || null,
    live: !!last?.querySelector('.cy-live'),
    kids: last ? [...last.children].map((c) => `${c.className.split(' ')[0]}:${Math.round(c.getBoundingClientRect().height)}`) : [],
  }
}, label)
const settled = await snap('settled')
console.log(JSON.stringify(settled))
// 新的顶层 run 起跑（后台/续跑型的「agent_start」），没有任何新块落进这一轮
await page.eval((sid) => {
  const d = window.__dispatch, ts = () => new Date().toISOString()
  d({ event_type: 'agent_start', session_id: sid, run_id: 'R2', index: 0, content: 'background', timestamp: ts() })
}, sid)
await wait(900)
const relit = await snap('relit')
console.log(JSON.stringify(relit))
await page.eval((sid) => window.__dispatch({ event_type: 'agent_end', session_id: sid, run_id: 'R2', index: 1, content: 'done', finish_reason: 'end', model: 'm', timestamp: new Date().toISOString() }), sid)
await wait(900)
console.log(JSON.stringify(await snap('settled-again')))
console.log(relit.open === settled.open && Math.abs(relit.h - settled.h) <= 6 ? 'OK 后台 run 不把已结算的轮重新展开' : '!! 被重新展开了')
// 变体：后台 run **带新块**（缺 run id）→ 应该切出新段，上半段保持折叠
await page.eval((sid) => {
  const d = window.__dispatch, ts = () => new Date().toISOString()
  d({ event_type: 'agent_start', session_id: sid, run_id: 'R4', index: 0, content: 'bg with blocks', timestamp: ts() })
  d({ event_type: 'tool_run_start', session_id: sid, id: 't4', name: 'bash', arguments: '{"command":"echo hi"}', timestamp: ts() })
  d({ event_type: 'content_chunk', session_id: sid, request_id: 'q4', content: '后台任务自己的输出。\n\n' })
}, sid)
await wait(900)
const bgBlocks = await page.eval(() => {
  const el = document.querySelector('.narrative')
  const turns = [...document.querySelectorAll('.rt-turn')]
  const last = turns.at(-1)
  const kids = last ? [...last.children].map((c) => `${c.className.split(' ')[0]}${c.className.includes('cy-live') ? '(live)' : ''}:${Math.round(c.getBoundingClientRect().height)}`) : []
  return { h: Math.round(el.scrollHeight), kids }
})
console.log(JSON.stringify(bgBlocks))
console.log(bgBlocks.kids[0]?.startsWith('rt-head') && bgBlocks.kids.some((k) => k.startsWith('rt-act')) ? 'OK 上半段仍折叠（Worked 行还在），后台新活另起一段' : '!! 上半段被重展开了')
// 正例：用户真的发一条 → 新 run 必须点亮 Working（回归防线：别把正常路径一起关掉）
await page.eval((sid) => {
  const d = window.__dispatch, ts = () => new Date().toISOString()
  d({ event_type: 'user_inputs_consumed', session_id: sid, user_texts: ['用户又发了一条'], timestamp: ts() })
  d({ event_type: 'agent_start', session_id: sid, run_id: 'R3', index: 0, content: 'new turn', timestamp: ts() })
  d({ event_type: 'llm_start', session_id: sid, request_id: 'q3', run_id: 'R3', model: 'm', timestamp: ts() })
  d({ event_type: 'content_chunk', session_id: sid, request_id: 'q3', run_id: 'R3', content: '新一轮正在写……' })
  d({ event_type: 'tool_run_start', session_id: sid, id: 't3', name: 'bash', arguments: '{"command":"echo hi"}', run_id: 'R3', timestamp: ts() })
}, sid)
await wait(900)
const userRun = await snap('user-run')
console.log(JSON.stringify(userRun))
console.log(userRun.open === '1' && userRun.live === true ? 'OK 用户发起的 run 仍算在跑（open=1 / Working）' : '!! 正常路径被关掉了')
await b.close?.()
