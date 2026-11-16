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
// 密度走 store 动作；皮肤是静态的（index.html 的 <html data-skin="runtime">），不用设
await page.eval(() => window.__store.getState().setNarrativeDensity('concise'))
await wait(800)
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
      // 交卷之后到达的记录走在**结算后面**（用户 2026-11 口径：通知只可能在盒子里，
      // 或者在账/产出卡之后），形状是 .rt-turn-line —— 不再是 .rt-turn-ans 的一段
      tails: [...(t?.querySelectorAll('.rt-turn-line') ?? [])].map((x) => x.textContent ?? '').join('\n'),
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
check('D 回执渲染在时间线上（结算之后的那条通道）', s.tails.includes('task-9'), JSON.stringify({ tails: s.tails.slice(-80), ans: s.ansText.slice(-60) }))
check('D 回执不在折叠行/盒子里', !s.foldText.includes('task-9') && s.signalsInBox === 0, `inFold=${s.foldText.includes('task-9')} signalsInBox=${s.signalsInBox}`)

// ---- E 运行中到达的回执：属于这次运行的过程 → 在 Working 面板里（盒内）-------
await inject(`[${user(1, '跑一下')}, ${tool(2, 'running')}, ${delivery(3)}, ${asst(4, '正在写…', true)}]`, false)
s = await snap()
requireTurn('E', s.turns)
check('E 运行中回执在 Working 面板里', s.liveText.includes('task-9'), JSON.stringify(s.liveText.slice(-60)))
check('E 回执不在时间线上（不重复渲染）', !s.ansText.includes('task-9') && !s.tails.includes('task-9') && s.ansSignals.length === 0, JSON.stringify({ ans: s.ansText.slice(-40), tails: s.tails.slice(-40) }))

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

// ---- H 运行中能上滑看历史（盒子不能吃掉 wheel），并能一键回到底部 -----------
// 用户 2026-10：简洁档 Working 运行中、详细档流式输出时「上滑不了去翻历史」，
// 也看不到「回到最新」。根因：工作盒子的 .rt-work-body 是内层滚动容器
//（max-height 46vh + overscroll-behavior: contain），wheel 被它吞掉且不再向外交付。
const long = []
let hid = 1
for (let i = 0; i < 6; i++) {
  long.push(user(hid++, '旧问题 ' + i))
  long.push(asst(hid++, ('旧回答 ' + i + ' ').repeat(8), false))
}
for (let i = 0; i < 12; i++) long.push(`{ kind: 'tool', id: ${hid++}, toolId: 'old${i}', name: 'bash', args: '{"command":"echo ${i}"}', status: 'done', durMs: 30, result: 'x'.repeat(200) }`)
long.push(user(hid++, '现在的问题'))
for (let i = 0; i < 25; i++) long.push(`{ kind: 'tool', id: ${hid++}, toolId: 'live${i}', name: 'bash', args: '{"command":"echo run ${i}"}', status: 'running' }`)
await inject(`[${long.join(',')}]`, false, '[]', `[{ id: 'run-live', kind: 'run', status: 'running', label: 'run', depth: 0, startedAt: Date.now() - 5000 }]`)
await wait(600)
const geo = () =>
  page.eval(() => {
    const n = document.querySelector('.narrative')
    const body = document.querySelector('.rt-work-body')
    return {
      top: n.scrollTop,
      max: n.scrollHeight - n.clientHeight,
      jump: !!document.querySelector('.narrative-jump'),
      contain: body ? getComputedStyle(body).overscrollBehaviorY : '',
    }
  })
let g = await geo()
check('H 起始贴在底部', g.max - g.top < 120, JSON.stringify(g))
check('H 盒子不允许 contain（wheel 要能交付给叙述区）', g.contain !== 'contain', g.contain)
// 真实滚轮：指针落在工作盒子上，向上滚
const pt = await page.eval(() => {
  const box = document.querySelector('.rt-work-body') || document.querySelector('.narrative')
  const r = box.getBoundingClientRect()
  return { x: Math.round(r.left + r.width / 2), y: Math.round(r.top + Math.min(r.height - 20, 120)) }
})
await page.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: pt.x, y: pt.y })
await wait(120)
await page.wheel(-1500, { step: 300, pauseMs: 60 })
await wait(400)
g = await geo()
check('H 上滑能看到历史（叙述区真的动了）', g.max - g.top > 200, JSON.stringify(g))
check('H 出现「回到最新」', g.jump, JSON.stringify(g))
// 点它回到底部并恢复跟随
await page.eval(() => document.querySelector('.narrative-jump')?.click())
await wait(500)
g = await geo()
check('H 点「回到最新」回到最底', g.max - g.top < 6, JSON.stringify(g))
check('H 回到底部后按钮消失', !g.jump, JSON.stringify(g))

// ---- I 子会话页：子 agent 在跑 → 最后一轮是 Working（不是 Worked）-----------
// 用户 2026-10 报障：左栏点进一个正在跑的子智能体，顶部写着「执行中 5m37s」，正文第一屏
// 却是「Worked for 4m47s」。根因：Narrative 的 runEnded / runStartedAt / activeRunId
// 一律读**主会话**视图，而子会话页渲染的是**子 agent** 的转录 —— 主 agent 交卷（后台
// 子 agent 还在跑）就把子会话整段判成已结算。修法：AgentSessionView 传 runState
//（node.status / startedAt），走 lib/turnSettle.resolveRunState。
const subCard = `{ kind: 'agent', id: 3, runId: 'sub-1', label: 'subagent', status: 'running', spawnedAt: Date.now() - 4000,
  items: [{ kind: 'tool', id: 'i1', toolId: 'x1', name: 'bash', status: 'running' }, { kind: 'text', id: 'i2', text: '正在跑…' }] }`
await inject(
  `[${user(1, '跑一下')}, ${subCard}]`,
  true,
  '[]',
  `[{ id: 'root', kind: 'run', status: 'done', label: 'run', depth: 0, startedAt: Date.now() - 600000 }, { id: 'sub-1', kind: 'run', parentId: 'root', status: 'running', label: 'subagent', depth: 1, startedAt: Date.now() - 4000 }]`,
)
await page.eval(() => window.__store.getState().focusAgent({ nodeId: 'sub-1', runId: 'sub-1' }))
await wait(700)
const sub = await page.eval(() => {
  const t = [...document.querySelectorAll('.rt-turn')].pop()
  return {
    turns: document.querySelectorAll('.rt-turn').length,
    live: !!t?.querySelector('.cy-live'),
    liveText: t?.querySelector('.cy-live')?.textContent ?? '',
    act: t?.querySelector('.rt-act')?.textContent ?? '',
  }
})
check('I 子会话页已切进去', sub.turns >= 1, `turns=${sub.turns}`)
check('I 子 agent 在跑 → 有 Working 盒', sub.live, `live=${sub.live} act=${JSON.stringify(sub.act.slice(0, 40))}`)
check('I 子会话不显示 Worked for', !/Worked/.test(sub.act), JSON.stringify(sub.act.slice(0, 60)))
// 子 agent 交卷（status 收敛）→ 同一页应结算成 Worked
await page.eval(() => {
  const st = window.__store.getState()
  st.focusAgent({ nodeId: 'sub-1', runId: 'sub-1' })
})
await page.eval(() => {
  const st = window.__store.getState()
  const sid = st.activeSessionId
  const v = st.views[sid]
  const runNodes = (v.runNodes ?? []).map((n) => (n.id === 'sub-1' ? { ...n, status: 'done' } : n))
  const blocks = v.blocks.map((b) => (b.kind === 'agent' ? { ...b, status: 'done', durMs: 4700 } : b))
  window.__store.setState({ blocks, views: { ...st.views, [sid]: { ...v, blocks, runNodes } } })
})
await wait(600)
const subDone = await page.eval(() => {
  const t = [...document.querySelectorAll('.rt-turn')].pop()
  return { live: !!t?.querySelector('.cy-live'), act: t?.querySelector('.rt-act')?.textContent ?? '' }
})
check('I 子 agent 交卷后结算成 Worked', !subDone.live && /Worked/.test(subDone.act), JSON.stringify(subDone))
await page.eval(() => window.__store.getState().focusAgent(undefined))
await wait(400)

// ---- J 子会话之间切换：滚动容器按**转录身份**重建，落回最新 -------------------
// 用户 2026-10：子会话页的容器 key 退化成主会话 id，于是左栏从子 agent A 切到 B 时
// 容器不重建 —— 转录换了、滚动位置还是 A 那一段的，A 的上滑意图也带给 B。
// 正文与工具交替：连续的同类工具会被 groupRounds 折成一行「Ran N commands」，
// 那样转录撑不高，滚动断言就测不到（case J 第一版 max=0 的原因）。
const manyTools = (p) => {
  const out = []
  for (let i = 0; i < 14; i++) {
    out.push(`{ kind: 'text', id: '${p}t${i}', text: '${p} 第 ${i} 段正文：${'这是一段用来撑高转录的说明文字。'.repeat(4)}' }`)
    out.push(`{ kind: 'tool', id: '${p}${i}', toolId: '${p}${i}', name: 'bash', status: 'done', args: '{"command":"echo ${i}"}', result: 'done' }`)
  }
  return `[${out.join(',')}]`
}
const subCardJ = (id, runId) => `{ kind: 'agent', id: ${id}, runId: '${runId}', label: '${runId}', status: 'running', spawnedAt: Date.now() - 5000, items: ${manyTools(runId)} }`
await inject(
  `[${user(1, '跑一下')}, ${subCardJ(2, 'sub-a')}, ${subCardJ(3, 'sub-b')}]`,
  true,
  '[]',
  `[{ id: 'root', kind: 'run', status: 'done', label: 'run', depth: 0, startedAt: Date.now() - 600000 }, { id: 'sub-a', kind: 'run', parentId: 'root', status: 'running', label: 'sub-a', depth: 1, startedAt: Date.now() - 5000 }, { id: 'sub-b', kind: 'run', parentId: 'root', status: 'running', label: 'sub-b', depth: 1, startedAt: Date.now() - 5000 }]`,
)
// 详细档：子转录整段摊在流里（简洁档全在 46vh 的盒子里，外层叙述区不滚，断言会空转）
await page.eval(() => window.__store.getState().setNarrativeDensity('detailed'))
await wait(300)
await page.eval(() => window.__store.getState().focusAgent({ nodeId: 'sub-a', runId: 'sub-a' }))
await wait(800)
// 模拟用户在子会话 A 里上滑看到顶部（并给这个 DOM 节点打个标记，用于识别「是否重建」）
const j0 = await page.eval(() => {
  const n = document.querySelector('.narrative')
  n.scrollTop = 0
  n.dataset.mark = 'A'
  return { max: n.scrollHeight - n.clientHeight }
})
await wait(300)
await page.eval(() => window.__store.getState().focusAgent({ nodeId: 'sub-b', runId: 'sub-b' }))
await wait(900)
const j1 = await page.eval(() => {
  const n = document.querySelector('.narrative')
  return { mark: n.dataset.mark ?? '', top: n.scrollTop, max: n.scrollHeight - n.clientHeight }
})
check('J 子会话页面可滚动（用例前提）', j0.max > 100, JSON.stringify(j0))
check('J 切到另一个子会话：容器重建（旧节点被替换）', j1.mark === '', `mark=${j1.mark}`)
check('J 切到另一个子会话：落回最新而不是沿用上一段的滚动位置', j1.max - j1.top < 120, JSON.stringify(j1))
await page.eval(() => window.__store.getState().focusAgent(undefined))
await page.eval(() => window.__store.getState().setNarrativeDensity('concise'))
await wait(400)

if (errs.length) check('无运行期异常', false, errs.join(' | '))
else check('无运行期异常', true, '')

for (const r of results) console.log(`${r.ok ? '✔' : '✘'} ${r.name}${r.detail ? '  [' + r.detail + ']' : ''}`)
const bad = results.filter((r) => !r.ok).length
console.log(`\n${results.length - bad}/${results.length} passed`)
await b.close()
process.exit(bad ? 1 : 0)
