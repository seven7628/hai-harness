// 一次用户输入不该排出很多个 Worked 盒子（用户 2026-11 截图：一轮 4 个 Worked，
// 其中三个 0 calls、没有正文）。
//
// 形态：一次运行被「续跑 / 回执」切成多个 run。切段（turnRuns）只应该在**上一段
// 交付过东西**时发生 —— 正文结论、记录（回执/信号）、产出。没有交付的连续 run 对
// 读者是同一段活，合成一个盒子。
//
// 用法：npx vite --port 5288 之后 node lab/verify-turn-runs.mjs
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const results = []
const check = (name, ok, detail) => results.push({ name, ok, detail })
const wait = (ms) => new Promise((r) => setTimeout(r, ms))

const b = await Browser.launch({ width: 1280, height: 1000 })
const page = await b.newPage({ width: 1280, height: 1000 })
const errs = []
b.on(page.sessionId, 'Runtime.exceptionThrown', (p) => errs.push(p.exceptionDetails?.text))
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
await wait(500)
await page.eval(() => window.__store.setState((s) => ({ settings: { ...s.settings, narrative_skin: 'runtime' } })))
await wait(200)
await page.eval(() => { document.documentElement.dataset.skin = 'runtime' })
for (let i = 0; i < 3 && !(await page.eval(() => !!window.__store.getState().activeSessionId)); i++) {
  await page.eval(() => window.__store.getState().newSession())
  await waitUntil('!!window.__store.getState().activeSessionId', 8000)
}
if (!(await page.eval(() => !!window.__store.getState().activeSessionId))) throw new Error('no active session')
await wait(500)
const inject = async (blocksExpr, runEnded, turnsExpr = '[]') => {
  await page.eval(new Function(`
    const store = window.__store
    const st = store.getState()
    const sid = st.activeSessionId
    const blocks = ${blocksExpr}
    const turns = ${turnsExpr}
    const v = st.views[sid] ?? {}
    store.setState({
      blocks, turns, runEnded: ${runEnded},
      views: { ...st.views, [sid]: { ...v, blocks, runEnded: ${runEnded}, runNodes: [], streaming: false, turns,
        usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0 } } },
    })
  `))
  await wait(350)
}
const now = Date.now()
const tool = (id, run) => `{ kind: 'tool', id: ${id}, toolId: 't${id}', name: 'bash', status: 'done', runId: '${run}', parentRunId: '${run}', durMs: 1200, startedAt: ${now - 9000} }`
const think = (id, run) => `{ kind: 'assistant', id: ${id}, text: '', thinking: '想一想', streaming: false, runId: '${run}', durMs: 900 }`
const say = (id, run, text) => `{ kind: 'assistant', id: ${id}, text: '${text}', streaming: false, thinking: '', runId: '${run}', durMs: 4000 }`

// ① 四段运行、只有第一段交付了正文 → 两个盒子（第一段的结论留在段与段之间），
//    后三段 0/0/1 calls 合成**一个**盒子，不再各占一个 Worked。
await inject(`[ { kind: 'user', id: 1, text: '干活' },
  ${tool(2, 'rA')}, ${say(3, 'rA', '第一段结论')},
  ${think(4, 'rB')}, ${think(5, 'rC')}, ${tool(6, 'rD')} ]`, false, `[
  { model: 'm', run_id: 'rA', input: 1000000, output: 100, cache_read: 999000, dur_ms: 4000 },
  { model: 'm', run_id: 'rD', input: 2000000, output: 100, cache_read: 1990000, dur_ms: 1200 } ]`)
const post = await page.eval(() => {
  const turn = document.querySelector('.rt-turn')
  // 盒子有两种形态：已交卷那一段是折叠行 .rt-act，还在跑的最后一段是 .cy-live
  const boxes = [...(turn?.querySelectorAll('.rt-act, .cy-live') ?? [])]
  const answer = [...(turn?.querySelectorAll('.msg.assistant .body') ?? [])]
    .find((e) => (e.textContent || '').includes('第一段结论'))
  return {
    acts: boxes.length,
    answers: answer ? 1 : 0,
    answerInBox: !!(answer && answer.closest('.rt-work-body, .cy-live')),
    foots: [...(turn?.querySelectorAll('.rt-turn-foot') ?? [])].map((e) => e.textContent),
  }
})
check('① 四段运行只画两个盒子（第一段交付 + 其余合并）', post.acts === 2, JSON.stringify(post))
check('① 第一段的结论仍在时间线上（没被收进盒子）', post.answers === 1 && post.answerInBox === false, JSON.stringify(post))
check('① 合并段不再出现「0 calls」的账', !(post.foots || []).some((t) => (t || '').trim().startsWith('0 calls')), JSON.stringify(post.foots))

// ② 四段运行一个都没交付 → 一个盒子
await inject(`[ { kind: 'user', id: 1, text: '干活' },
  ${think(2, 'rA')}, ${tool(3, 'rA')}, ${think(4, 'rB')}, ${tool(5, 'rB')}, ${think(6, 'rC')} ]`, false, `[
  { model: 'm', run_id: 'rA', input: 1200000, output: 100, cache_read: 1190000, dur_ms: 3000 } ]`)
const one = await page.eval(() => {
  const turn = document.querySelector('.rt-turn')
  return {
    acts: turn?.querySelectorAll('.rt-act, .cy-live').length ?? -1,
    foots: [...(turn?.querySelectorAll('.rt-turn-foot') ?? [])].map((e) => e.textContent),
  }
})
check('② 一个都没交付 → 一个盒子（一次用户输入一个盒子）', one.acts === 1, JSON.stringify(one))

// ③ 对照：中间夹着一条回执 → 仍然两个盒子（记录要落在段与段之间的时间线上）
await inject(`[ { kind: 'user', id: 1, text: '干活' },
  ${tool(2, 'rA')}, { kind: 'task_delivery', id: 3, taskId: 'task-x', status: 'completed', afterRun: true },
  ${tool(4, 'rB')} ]`, false)
const two = await page.eval(() => {
  const turn = document.querySelector('.rt-turn')
  const boxes = [...(turn?.querySelectorAll('.rt-act, .cy-live') ?? [])]
  const r = turn?.querySelector('.sig, .rt-td')
  return {
    acts: boxes.length,
    recordBetween: (() => {
      if (!r || boxes.length < 2) return null
      return !!(boxes[0].compareDocumentPosition(r) & Node.DOCUMENT_POSITION_FOLLOWING) &&
        !!(r.compareDocumentPosition(boxes[1]) & Node.DOCUMENT_POSITION_FOLLOWING)
    })(),
  }
})
check('③ 段间有回执 → 仍是两个盒子', two.acts === 2, JSON.stringify(two))
check('③ 回执排在两个盒子之间（顺序没有回去）', two.recordBetween === true, JSON.stringify(two.recordBetween))

check('无未捕获异常', errs.length === 0, JSON.stringify(errs.slice(0, 3)))
await b.close()

const failed = results.filter((r) => !r.ok)
for (const r of results) console.log(`${r.ok ? 'PASS' : 'FAIL'} ${r.name}${r.ok ? '' : '  ⟵ ' + r.detail}`)
if (failed.length) {
  console.log(`\nTURN_RUNS_FAIL (${failed.length}/${results.length})`)
  process.exit(1)
}
console.log(`\nTURN_RUNS_PASS (${results.length})`)
