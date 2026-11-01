// 简洁档「时间线 + Working 盒」的结构断言（真浏览器、真组件、真 store）。
//
// 判据是 DOM 结构与文本（不是截图）：哪块在盒子里、哪块在时间线上。四个形态：
//   A 纯对话轮运行中  → 没有盒子，正文必须在时间线上（旧实现整轮不渲染）
//   B 工具轮运行中    → 有盒子，正文在盒子里，外面只有记录
//   C AgentEnd + 后台任务仍 running → 折叠行是 Worked（不是 Working），盒子不再出现
//   D AgentEnd 之后才到的后台任务回执 → 在**时间线**上（盒子外面）
//   E 运行中到达的回执 / 一行信号 → 在**盒子里**（按事件顺序，不是时间线上的记录）
//
// 用法：npx vite --port 5288 之后 node lab/verify-concise-fold.mjs
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
if (!(await waitUntil('!!window.__store'))) throw new Error('window.__store never appeared')
// 皮肤/密度走 store 动作（手工改 data-skin 会被 App 的 effect 盖回）
await page.eval(() => window.__store.getState().setNarrativeDensity('concise'))
// 皮肤：直接改属性（组件通过 MutationObserver 读它）。要在 boot 完成后设 ——
// 任何一次 settings 变更都会让 App 的 effect 重新 apply，把属性盖回 current。
await wait(800)
await page.eval(() => { document.documentElement.dataset.skin = 'runtime' })
for (let i = 0; i < 3 && !(await page.eval(() => !!window.__store.getState().activeSessionId)); i++) {
  await page.eval(() => window.__store.getState().newSession())
  await waitUntil('!!window.__store.getState().activeSessionId', 8000)
}
if (!(await page.eval(() => !!window.__store.getState().activeSessionId))) throw new Error('no active session')
await wait(600)

// 注入一段确定的块序列（等价于后端事件流落库后的 view）。
// Narrative 读顶层 blocks 镜像 + views[sid].runEnded —— 两处都要写。
const inject = async (blocksExpr, runEnded, turnsExpr, runsExpr) => {
  // 注意：交给 page.eval 的必须是**会执行**的函数体（new Function(body) 而非
  // new Function('() => {...}') —— 后者只是把箭头函数当表达式求值，什么都不会发生）
  await page.eval(new Function(`
    const store = window.__store
    const st = store.getState()
    const sid = st.activeSessionId
    const blocks = ${blocksExpr}
    const v = st.views[sid] ?? {}
    const turns = ${typeof turnsExpr === 'undefined' ? '[]' : turnsExpr}
    const runNodes = ${typeof runsExpr === 'undefined' ? '[]' : runsExpr}
    store.setState({
      blocks,
      turns,
      runEnded: ${runEnded},
      views: { ...st.views, [sid]: { ...v, blocks, runEnded: ${runEnded}, runNodes, streaming: false, turns, usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0 } } },
    })
  `))
  await wait(260)
}
const snap = () =>
  page.eval(() => {
    const turns = [...document.querySelectorAll('.rt-turn')]
    const t = turns[turns.length - 1]
    const q = (sel) => (t ? t.querySelectorAll(sel).length : 0)
    return {
      turns: turns.length,
      live: q('.cy-live'),
      liveText: t?.querySelector('.cy-live')?.textContent ?? '',
      fold: t?.querySelector('.rt-act')?.textContent ?? '',
      workbox: q('.rt-workbox'),
      ans: t?.querySelector('.rt-turn-ans')?.textContent ?? '',
      ansSignals: [...(t?.querySelectorAll('.rt-turn-ans .sig, .rt-turn-ans .sig-list') ?? [])].map((x) => x.textContent ?? ''),
      ansText: t?.querySelector('.rt-turn-ans')?.textContent ?? '',
      foot: t?.querySelector('.rt-turn-foot')?.textContent ?? '',
      footFlag: t?.getAttribute('data-foot') ?? '',
      foldText: t?.querySelector('.rt-act')?.textContent ?? '',
      signalsInBox: [...(t?.querySelectorAll('.rt-workbox .sig') ?? [])].length,
      // 每一轮的「还在跑吗」：G 用例要同时看上一轮与当前轮
      all: turns.map((x) => ({
        live: !!x.querySelector('.cy-live'),
        act: x.querySelector('.rt-act')?.textContent ?? '',
      })),
    }
  })

const user = (id, text) => `{ kind: 'user', id: ${id}, text: ${JSON.stringify(text)} }`
const asst = (id, text, streaming) => `{ kind: 'assistant', id: ${id}, text: ${JSON.stringify(text)}, streaming: ${streaming}, thinking: '' }`
const tool = (id, status) => `{ kind: 'tool', id: ${id}, toolId: 't${id}', name: 'bash', status: '${status}' }`
const agentBlock = (id) => `{ kind: 'agent', id: ${id}, runId: 'sub-1', label: 'subagent · go test', status: 'running', spawnedAt: Date.now() - 5000, usage: {} }`
const delivery = (id) => `{ kind: 'task_delivery', id: ${id}, taskId: 'task-9', status: 'completed', result: 'sleep done (30s)' }`

const requireTurn = (label, n) => check(`${label} 轮已渲染`, n >= 1, `turns=${n}`)

// ---- A 纯对话轮（运行中）：没有盒子，正文必须在时间线上 ---------------------
await inject(`[${user(1, '你好')}, ${asst(2, '正在写…', true)}]`, false)
let s = await snap()
requireTurn('A', s.turns)
check('A 纯对话轮不画 Working 盒', s.live === 0, `live=${s.live}`)
check('A 流式正文在时间线上（不再整轮空白）', s.ans.includes('正在写'), JSON.stringify(s.ans.slice(0, 60)))

// ---- B 工具轮（运行中）：正文在盒子里，外面只有记录 -------------------------
await inject(`[${user(1, '跑一下')}, ${tool(2, 'running')}, ${asst(3, '正在写…', true)}]`, false)
s = await snap()
requireTurn('B', s.turns)
check('B 工具轮有 Working 盒', s.live === 1, `live=${s.live}`)
check('B 正文在盒子里', s.liveText.includes('正在写'), JSON.stringify(s.liveText.slice(0, 60)))
check('B 外面没有正文', !s.ans.includes('正在写'), JSON.stringify(s.ans.slice(0, 60)))

// ---- C AgentEnd + 后台子 agent 仍 running：折叠行是 Worked -------------------
await inject(`[${user(1, '开个后台任务')}, ${tool(2, 'done')}, ${agentBlock(3)}, ${asst(4, '主 run 已交卷', false)}]`, true)
s = await snap()
requireTurn('C', s.turns)
check('C 交卷后不再显示 Working 盒', s.live === 0, `live=${s.live}`)
check('C 折叠行是 Worked（不是 Working）', /Worked/.test(s.fold) && !/Working/.test(s.fold), JSON.stringify(s.fold.slice(0, 80)))
check('C 结论在外面', s.ans.includes('主 run 已交卷'), JSON.stringify(s.ans.slice(0, 60)))

// ---- D AgentEnd 之后才到的后台任务回执：在时间线上 ---------------------------
await inject(`[${user(1, '开个后台任务')}, ${tool(2, 'done')}, ${agentBlock(3)}, ${asst(4, '主 run 已交卷', false)}, ${delivery(5)}]`, true)
s = await snap()
requireTurn('D', s.turns)
check('D 回执渲染在时间线（.rt-turn-ans 里）', s.ansText.includes('task-9'), JSON.stringify(s.ansText.slice(-80)))
check('D 回执不在折叠行/盒子里', !s.foldText.includes('task-9') && s.signalsInBox === 0, `inFold=${s.foldText.includes('task-9')} signalsInBox=${s.signalsInBox}`)

// ---- E 运行中到达的回执：属于这次运行的过程 → 在 Working 面板里（盒内）-------
await inject(`[${user(1, '跑一下')}, ${tool(2, 'running')}, ${delivery(3)}, ${asst(4, '正在写…', true)}]`, false)
s = await snap()
requireTurn('E', s.turns)
check('E 运行中回执在 Working 面板里', s.liveText.includes('task-9'), JSON.stringify(s.liveText.slice(-60)))
check('E 回执不在时间线上（不重复渲染）', !s.ansText.includes('task-9') && s.ansSignals.length === 0, JSON.stringify(s.ansText.slice(-60)))

// ---- E2 运行中到达的一行信号（task_promoted）：同样在盒子里，且不打乱顺序 ------
const promote = (id) => `{ kind: 'signal', id: ${id}, fact: { sig: 'promote', etype: 'task_promoted', name: 'bash', message: 'task-9' } }`
const namedTool = (id, name) => `{ kind: 'tool', id: ${id}, toolId: 'x${id}', name: ${JSON.stringify(name)}, status: 'running' }`
await inject(
  `[${user(1, '跑一下')}, ${namedTool(2, 'alpha')}, ${promote(3)}, ${namedTool(4, 'beta')}, ${asst(5, '正在写…', true)}]`,
  false,
)
s = await snap()
requireTurn('E2', s.turns)
check('E2 信号在 Working 面板里', s.liveText.includes('task_promoted'), JSON.stringify(s.liveText.slice(-80)))
check('E2 信号不在时间线上', !s.ansText.includes('task_promoted'), JSON.stringify(s.ansText.slice(-80)))
// 顺序：promote 行必须夹在两条工具行之间（alpha < sig < beta），否则就是「没按时间顺序」
check(
  'E2 信号按事件顺序夹在工作行之间',
  (() => {
    // 工具名渲染时首字母大写（Alpha/Beta），比较前统一小写
    const text = s.liveText.toLowerCase()
    const ia = text.indexOf('alpha')
    const isig = text.indexOf('task_promoted')
    const ib = text.indexOf('beta')
    return ia >= 0 && isig > ia && ib > isig
  })(),
  JSON.stringify(s.liveText.slice(0, 240)),
)

// ---- F abort 那一轮：轮尾账要出现（此前没有 llm_end → 账缺失）---------------
await inject(
  `[${user(1, '中途停一下')}, { kind: 'assistant', id: 2, text: '半截回复', streaming: false, thinking: '', runId: 'r-abort' }]`,
  true,
  `[{ model: 'm1', input: 120, output: 12, cacheRead: 100, cacheWrite: 0, reasoning: 0, durMs: 1500, costUsd: 3e-6, runId: 'r-abort' }]`,
)
s = await snap()
requireTurn('F', s.turns)
check('F 被中断那轮在轮尾账里（有 foot 行）', s.footFlag === '1' && s.foot.length > 0, JSON.stringify(s.foot.slice(0, 80)))
check('F 轮尾账含该轮 tokens', /120|132|12/.test(s.foot), JSON.stringify(s.foot.slice(0, 80)))

// ---- G 上一轮已交卷、用户又发一条：新 run 不得把上一轮的 Working 重新点亮 -------
// 用户 2026-10 报障：上一轮 AgentEnd 之后 Worked for 已经出来了，用户紧接着发下一条，
// 上一轮的 Working 与时钟又转起来（还带得动滚动位置）。根因是判据只看视图级 runEnded，
// 而上一轮里还挂着 status='running' 的后台块。修法：轮上的 runId 必须等于正在跑的 run。
await inject(
  `[
    ${user(1, '第一问')},
    { kind: 'tool', id: 2, toolId: 't2', name: 'bash', status: 'running', runId: 'run-1', taskId: 'task-bg', promoted: true },
    { kind: 'assistant', id: 3, text: '第一答', streaming: false, thinking: '', runId: 'run-1' },
    ${user(4, '第二问')},
    { kind: 'tool', id: 5, toolId: 't5', name: 'bash', status: 'running', runId: 'run-2' },
  ]`,
  false,
  '[]',
  `[{ id: 'run-2', kind: 'run', status: 'running', label: 'run', depth: 0, startedAt: Date.now() - 1200 }]`,
)
s = await snap()
requireTurn('G', s.turns)
check('G 上一轮不因新 run 重新变 Working', s.all[0]?.live === false, JSON.stringify(s.all[0]))
check('G 上一轮仍是 Worked 折叠行', /Worked/.test(s.all[0]?.act ?? ''), JSON.stringify(s.all[0]?.act))
check('G 当前轮才是 Working', s.all[1]?.live === true, JSON.stringify(s.all[1]))

if (errs.length) check('无运行期异常', false, errs.join(' | '))
else check('无运行期异常', true, '')

for (const r of results) console.log(`${r.ok ? '✔' : '✘'} ${r.name}${r.detail ? '  [' + r.detail + ']' : ''}`)
const bad = results.filter((r) => !r.ok).length
console.log(`\n${results.length - bad}/${results.length} passed`)
await b.close()
process.exit(bad ? 1 : 0)
