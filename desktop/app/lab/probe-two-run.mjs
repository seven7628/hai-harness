// 探针：情况二「下半段还在跑」时，上一段运行（已 AgentEnd）的**结算三件套**还在不在：
//   · 结论正文（llm_content）
//   · 产出卡（artifacts）
//   · 轮尾账（tokens）
// 用户 2026-11：run1 交了卷、异步任务回执到达、run2 起新盒子 —— 结果上一段的正文被收进
// Worked 盒子里（看不见），产出卡与轮尾账整块消失。
// 用法：LAB_URL=http://localhost:5288/ node lab/probe-two-run.mjs
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const b = await Browser.launch({ width: 1280, height: 1000 })
const page = await b.newPage({ width: 1280, height: 1000 })
await page.goto(URL, { settleMs: 4000 })
const waitUntil = async (expr, timeout = 20000) => {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) {
    if (await page.eval(new Function('return (' + expr + ')'))) return true
    await wait(250)
  }
  return false
}
if (!(await waitUntil('!!window.__store && !!window.__dispatch'))) throw new Error('store/dispatch never appeared')
await page.eval(() => window.__store.getState().setNarrativeDensity('concise'))
await wait(400)
await page.eval(() => window.__store.setState((s) => ({ settings: { ...s.settings, narrative_skin: 'runtime' } })))
await page.eval(() => { document.documentElement.dataset.skin = 'runtime' })
await wait(200)
for (let i = 0; i < 3 && !(await page.eval(() => !!window.__store.getState().activeSessionId)); i++) {
  await page.eval(() => window.__store.getState().newSession())
  await wait(900)
}
const sid = await page.eval(() => window.__store.getState().activeSessionId)
const fire = async (list) => {
  for (const [t, payload] of list) {
    await page.eval((p) => window.__dispatch({ event_type: p.t, session_id: p.sid, ...p.payload }), { t, sid, payload })
    await wait(80)
  }
  await wait(600)
}
const A1 = { name: 'a.md', path: 'w/a.md', kind: 'created', added: 75, removed: 0 }

// run1：一次工具调用 + 结论正文 + llm_end（账的数据源）+ AgentEnd（带产出）
await fire([
  ['agent_start', { run_id: 'p1', timestamp: new Date(Date.now() - 20000).toISOString() }],
  ['user_inputs_consumed', { user_texts: ['探针：两段运行'] }],
  ['tool_run_start', { id: 'pt1', name: 'bash', arguments: '{}', run_id: 'p1' }],
  ['tool_run_end', { id: 'pt1', name: 'bash', result: 'ok', is_error: false, run_id: 'p1' }],
  ['llm_start', { run_id: 'p1', request_id: 'p1r', model: 'm' }],
  ['content_chunk', { run_id: 'p1', request_id: 'p1r', content: '第一段的结论正文' }],
  ['llm_end', { run_id: 'p1', request_id: 'p1r', model: 'm', usage: { Input: 900, Output: 200, CacheRead: 700 } }],
  ['agent_end', { run_id: 'p1', finish_reason: 'stop', artifacts: [A1] }],
])
const snap = (name) => page.eval((n) => {
  const turn = document.querySelector('.rt-turn')
  if (!turn) return { n, missing: true }
  const outline = [...turn.children]
    .filter((el) => !el.classList.contains('rt-head'))
    .map((el) => {
      const c = (el.className || '').split(' ')[0]
      const t = (el.textContent || '').replace(/\s+/g, ' ').trim().slice(0, 26)
      return `${c}|${t}`
    })
  const inBox = [...turn.querySelectorAll('.rt-workbox')].map((el) => (el.textContent || '').replace(/\s+/g, ' ').slice(0, 60))
  const inBody = [...turn.querySelectorAll('.rt-turn-in')].map((el) => (el.textContent || '').replace(/\s+/g, ' ').slice(0, 40))
  return {
    n,
    answerOutside: [...turn.querySelectorAll('.rt-turn-ans')].some((el) => el.textContent.includes('第一段的结论正文')),
    answerAnywhere: (turn.textContent || '').includes('第一段的结论正文'),
    artifacts: turn.querySelectorAll('.rt-turn-artifacts').length,
    foot: turn.querySelectorAll('.rt-turn-foot').length,
    boxes: turn.querySelectorAll('.rt-workbox').length,
    outline, inBox, inBody,
  }
}, name)

console.log('### run1 交卷后\n' + JSON.stringify(await snap('after-agentend'), null, 2))

// 情况二：回执到达 + run2 起新盒子（还在跑，正文在流）
await fire([
  ['task_result_delivered', { task_id: 'tp1', result: 'done' }],
  ['agent_start', { run_id: 'p2', timestamp: new Date().toISOString() }],
  ['llm_start', { run_id: 'p2', request_id: 'p2r', model: 'm' }],
  ['content_chunk', { run_id: 'p2', request_id: 'p2r', content: '第二段正在写' }],
  ['tool_run_start', { id: 'pt2', name: 'bash', arguments: '{}', run_id: 'p2' }],
])
await wait(800)
console.log('\n### 回执 + run2 在跑\n' + JSON.stringify(await snap('run2-live'), null, 2))
await page.eval(() => document.querySelector('.rt-turn')?.scrollIntoView({ block: 'start' }))
await wait(300)
await page.screenshot({ path: '/tmp/probe-two-run-live.png' })
console.log('shot /tmp/probe-two-run-live.png')

// 单 run 交卷 + 迟到的回执（没有人再续跑）：账与卡是交付，通知排在它们**后面**
await page.eval(() => window.__store.getState().newSession())
await wait(1200)
const sid2 = await page.eval(() => window.__store.getState().activeSessionId)
const fire2 = async (list) => {
  for (const [t, payload] of list) {
    await page.eval((p) => window.__dispatch({ event_type: p.t, session_id: p.sid, ...p.payload }), { t, sid: sid2, payload })
    await wait(80)
  }
  await wait(600)
}
await fire2([
  ['agent_start', { run_id: 'q1', timestamp: new Date(Date.now() - 9000).toISOString() }],
  ['user_inputs_consumed', { user_texts: ['探针：收尾后迟到回执'] }],
  ['tool_run_start', { id: 'qt1', name: 'bash', arguments: '{}', run_id: 'q1' }],
  ['tool_run_end', { id: 'qt1', name: 'bash', result: 'ok', is_error: false, run_id: 'q1' }],
  ['llm_start', { run_id: 'q1', request_id: 'q1r', model: 'm' }],
  ['content_chunk', { run_id: 'q1', request_id: 'q1r', content: '收尾后的结论' }],
  ['llm_end', { run_id: 'q1', request_id: 'q1r', model: 'm', usage: { Input: 400, Output: 90, CacheRead: 300 } }],
  ['agent_end', { run_id: 'q1', finish_reason: 'stop', artifacts: [{ name: 'z.md', path: 'w/z.md', kind: 'created', added: 9, removed: 1 }] }],
  ['task_result_delivered', { task_id: 'tq1', result: 'done' }],
])
console.log('\n### 单 run 收尾 + 迟到回执\n' + JSON.stringify(await snap('single-late-record'), null, 2))

// 两段运行**都交卷**（用户刷新后的形态）：上一段的正文必须在盒外，账/卡在轮尾，
// 交卷后到达的回执排在账与卡之后
await page.eval(() => window.__store.getState().newSession())
await wait(1200)
const sid3 = await page.eval(() => window.__store.getState().activeSessionId)
const fire3 = async (list) => {
  for (const [t, payload] of list) {
    await page.eval((p) => window.__dispatch({ event_type: p.t, session_id: p.sid, ...p.payload }), { t, sid: sid3, payload })
    await wait(80)
  }
  await wait(700)
}
await fire3([
  ['agent_start', { run_id: 'k1', timestamp: new Date(Date.now() - 30000).toISOString() }],
  ['user_inputs_consumed', { user_texts: ['探针：两段都交卷'] }],
  ['tool_run_start', { id: 'kt1', name: 'bash', arguments: '{}', run_id: 'k1' }],
  ['tool_run_end', { id: 'kt1', name: 'bash', result: 'ok', is_error: false, run_id: 'k1' }],
  ['llm_start', { run_id: 'k1', request_id: 'k1r', model: 'm' }],
  ['content_chunk', { run_id: 'k1', request_id: 'k1r', content: '第一段交付的正文' }],
  ['llm_end', { run_id: 'k1', request_id: 'k1r', model: 'm', usage: { Input: 900, Output: 200, CacheRead: 700 } }],
  ['agent_end', { run_id: 'k1', finish_reason: 'stop', artifacts: [A1] }],
  ['task_result_delivered', { task_id: 'tk3', result: 'done' }],
  ['agent_start', { run_id: 'k2', timestamp: new Date().toISOString() }],
  ['llm_start', { run_id: 'k2', request_id: 'k2r', model: 'm' }],
  ['reasoning_chunk', { run_id: 'k2', request_id: 'k2r', content: '第二段想一想' }],
  ['content_chunk', { run_id: 'k2', request_id: 'k2r', content: '第二段交付的正文' }],
  ['llm_end', { run_id: 'k2', request_id: 'k2r', model: 'm', usage: { Input: 120, Output: 30 } }],
  ['agent_end', { run_id: 'k2', finish_reason: 'stop' }],
])
const settled = await snap('two-runs-settled')
const box1 = await page.eval(() => {
  const box = document.querySelector('.rt-workbox')
  return { text: (box?.textContent ?? '').slice(0, 120), hasAns1: (box?.textContent ?? '').includes('第一段交付的正文') }
})
console.log('\n### 两段都交卷\n' + JSON.stringify({ ...settled, box1 }, null, 2))

await page.eval(() => document.querySelector('.rt-turn')?.scrollIntoView({ block: 'center' }))
await wait(300)
await page.screenshot({ path: '/tmp/probe-two-run.png' })
console.log('\nshot /tmp/probe-two-run.png')
await b.close()
