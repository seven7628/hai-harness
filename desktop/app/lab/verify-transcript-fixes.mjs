// 转录的五个「归属」断言（真浏览器、真组件、真 store）。
//
//   A bash 运行中输出尾巴不得撑破叙述列（简洁档 / 详细档都一样）
//   B 轮尾账结算在 AgentEnd：下一轮开始 / 子 agent 迟到用量都不许改写它
//   C 运行中插话：同一个 run 只有一个 Working 盒子，插话在盒子内部按时间落位
//   D 摘离信号归**它自己的会话**（子 agent 的卡片 / 车道），完成通知回主会话
//   E 压缩完成的一行 signal 归对应会话（主会话块 / 子 agent 卡片项）
//
// 用法：npx vite --port 5288 之后 node lab/verify-transcript-fixes.mjs
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
await wait(700)
// 皮肤走 store 的设置（settings.narrative_skin）：App 的 effect 会在**任何**外观设置
// 变更（含 setNarrativeDensity）时重写 data-skin —— 只手改属性会在下一次切档时被盖回
// current（这个坑 verify-concise-fold.mjs 的注释里也记过）。
await page.eval(() => window.__store.setState((s) => ({ settings: { ...s.settings, narrative_skin: 'runtime' } })))
await wait(300)
await page.eval(() => { document.documentElement.dataset.skin = 'runtime' })
for (let i = 0; i < 3 && !(await page.eval(() => !!window.__store.getState().activeSessionId)); i++) {
  await page.eval(() => window.__store.getState().newSession())
  await waitUntil('!!window.__store.getState().activeSessionId', 8000)
}
if (!(await page.eval(() => !!window.__store.getState().activeSessionId))) throw new Error('no active session')
await wait(600)
const sid = await page.eval(() => window.__store.getState().activeSessionId)
const density = (d) => page.eval((x) => window.__store.getState().setNarrativeDensity(x), d)

// 直接注入视图（与 verify-concise-fold.mjs 同一套：顶层 blocks 镜像 + views[sid]）
const inject = async (blocksExpr, runEnded, turnsExpr, runsExpr) => {
  await page.eval(new Function(`
    const store = window.__store
    const st = store.getState()
    const sid = st.activeSessionId
    const blocks = ${blocksExpr}
    const turns = ${typeof turnsExpr === 'undefined' ? '[]' : turnsExpr}
    const runNodes = ${typeof runsExpr === 'undefined' ? '[]' : runsExpr}
    const v = st.views[sid] ?? {}
    store.setState({
      blocks,
      turns,
      runEnded: ${runEnded},
      views: { ...st.views, [sid]: { ...v, blocks, runEnded: ${runEnded}, runNodes, streaming: false, turns, usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0 } } },
    })
  `))
  await wait(320)
}
// store 写入 → React 提交是异步的：断言前轮询 DOM，别用固定 sleep 踩空
const poll = async (expr, timeout = 5000) => {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) {
    if (await page.eval(new Function('return (' + expr + ')'))) return true
    await wait(120)
  }
  return false
}
const run1 = (id = 'r1', status = 'running') => `[{ id: '${id}', kind: 'run', status: '${status}', label: 'run', depth: 0, startedAt: Date.now() - 4000 }]`

// ---------------------------------------------------------------- A 尾巴宽度
const LONG = 'cd /Users/admin/cybertron-gogroup/overseas-projects/metrix-voice && for s in regress-purposes regress-something-very-long-here; do echo $s; done'
const tailBlocks = `[
  { kind: 'user', id: 1, text: '跑一下' },
  { kind: 'tool', id: 2, toolId: 't2', name: 'bash', status: 'running', runId: 'r1',
    args: ${JSON.stringify(JSON.stringify({ command: LONG }))},
    liveTail: ${JSON.stringify(LONG + '\n=== regress-purposes\nPASS the list scrolls to its last row and the pager sits below, not over it (last row 887)\n')},
    liveTotal: 4096, startedAt: Date.now() - 3000 },
]`
const tailGeo = () => page.eval(() => {
  const nar = document.querySelector('.narrative')
  const tail = document.querySelector('.tool-tail')
  const pre = document.querySelector('.sh-out--live')
  if (!nar || !tail || !pre) return null
  const n = nar.getBoundingClientRect()
  const t = tail.getBoundingClientRect()
  return {
    overflows: t.right > n.right + 1 || t.left < n.left - 1,
    tailRight: Math.round(t.right),
    colRight: Math.round(n.right),
    hScroll: pre.scrollWidth - pre.clientWidth,
    wrapped: getComputedStyle(document.querySelector('.tl .tx')).whiteSpace,
  }
})
for (const d of ['concise', 'detailed']) {
  await density(d)
  await inject(tailBlocks, false, '[]', run1())
  const g = await tailGeo()
  check(`A ${d}：运行中输出尾巴不超出叙述列`, g && !g.overflows, JSON.stringify(g))
  check(`A ${d}：尾巴行在列宽内换行（没有横向裁切）`, g && g.hScroll <= 1, JSON.stringify(g))
}

// ---------------------------------------------------------------- B 轮尾账
const metric = (runId, input, output, durMs = 1000) =>
  `{ model: 'm', input: ${input}, output: ${output}, cacheRead: 0, cacheWrite: 0, reasoning: 0, durMs: ${durMs}, costUsd: 0, runId: '${runId}' }`
const ledgerTexts = () => page.eval(() =>
  [...document.querySelectorAll('.rt-turn')].map((t) => t.querySelector('.rt-turn-foot')?.textContent ?? ''),
)
const turn1 = `[
  { kind: 'user', id: 1, text: '第一问' },
  { kind: 'assistant', id: 2, text: '第一答', streaming: false, thinking: '', runId: 'r1', durMs: 2000 },
  { kind: 'agent', id: 3, runId: 'sub1', label: 'sub', status: 'running', spawnedAt: Date.now() - 5000, usage: {} },
]`
await density('concise')
await inject(turn1, true, `[${metric('r1', 1000, 100)}]`, '[]')
const led1 = (await ledgerTexts())[0] ?? ''
// 下一轮开始；同时子 agent 又完成一次调用（迟到的 metric 原本会把上一轮改写大）
await inject(
  `${turn1.slice(0, -1)}, { kind: 'user', id: 4, text: '第二问' }, { kind: 'tool', id: 5, toolId: 't5', name: 'bash', status: 'running', runId: 'r2', startedAt: Date.now() - 300 }, { kind: 'assistant', id: 6, text: '第二答', streaming: true, thinking: '', runId: 'r2' }]`,
  false,
  `[${metric('r1', 1000, 100)}, ${metric('sub1', 5000, 500)}, ${metric('sub1', 9000, 900)}]`,
  run1('r2'),
)
const led2 = (await ledgerTexts())[0] ?? ''
check('B 轮尾账在 AgentEnd 结算（有数）', /tokens/.test(led1), JSON.stringify(led1))
check('B 下一轮开始后，上一轮的账不被改写', led1 === led2, JSON.stringify({ led1, led2 }))
check('B 子 agent 的用量不重复算进这一轮', /1k tokens/.test(led1), JSON.stringify(led1))

// ---------------------------------------------------------------- C 运行中插话
await inject(
  `[
    { kind: 'user', id: 1, text: '第一句' },
    { kind: 'tool', id: 2, toolId: 't2', name: 'bash', status: 'running', runId: 'r1', startedAt: Date.now() - 2000 },
    { kind: 'user', id: 3, text: '跑的时候补一句', injected: true },
    { kind: 'assistant', id: 4, text: '正在写…', streaming: true, thinking: '', runId: 'r1' },
  ]`,
  false,
  '[]',
  run1(),
)
await poll('!!document.querySelector(".cy-live .rt-you[data-injected=\\"1\\"]")')
const mid = await page.eval(() => {
  const turns = [...document.querySelectorAll('.rt-turn')]
  const live = [...document.querySelectorAll('.cy-live')]
  const midUser = document.querySelector('.cy-live .rt-you[data-injected="1"]')
  return {
    turns: turns.length,
    live: live.length,
    inBox: !!midUser,
    label: midUser?.querySelector('.rt-who')?.textContent ?? '',
    outside: document.querySelectorAll('.rt-turn-ans .rt-you[data-injected="1"]').length,
  }
})
check('C 运行中插话不另起一轮（只有一个 turn）', mid.turns === 1, JSON.stringify(mid))
check('C 只有一个 Working 盒子', mid.live === 1, JSON.stringify(mid))
check('C 插话落在盒子内部（按事件顺序）', mid.inBox === true, JSON.stringify(mid))
check('C 插话带「插话」标记（不是这一轮的开口）', /插话/.test(mid.label), JSON.stringify(mid.label))
check('C 插话不在时间线上重复画一遍', mid.outside === 0, JSON.stringify(mid.outside))

// ---------------------------------------------------------------- D 摘离 / 完成
await page.eval((sid) => {
  const d = window.__dispatch
  window.__store.setState({ blocks: [], views: { ...window.__store.getState().views, [sid]: { ...(window.__store.getState().views[sid] ?? {}), blocks: [], runNodes: [], runEnded: false } } })
  d({ event_type: 'agent_start', session_id: sid, run_id: 'rm', index: 0 })
  d({ event_type: 'agent_start', session_id: sid, run_id: 'rs', parent_run_id: 'rm', index: 0, name: 'sub' })
  d({ event_type: 'tool_start', session_id: sid, id: 'tt1', name: 'bash', run_id: 'rs', task_id: 'tooltask-sub', arguments: '{"command":"sleep 60"}' })
  d({ event_type: 'task_promoted', session_id: sid, task_id: 'tooltask-sub', name: 'bash' })
}, sid)
await poll('!!document.querySelector(".la-head")')
await wait(300)
const promoted = await page.eval(() => {
  const lane = document.querySelector('.la-head')
  return {
    mainSigs: [...document.querySelectorAll('.rt-turn-ans .sig')].map((x) => x.textContent ?? ''),
    lane: !!lane,
    laneText: lane?.textContent ?? '',
  }
})
// 展开车道，看它的活动行里有没有那条摘离信号
await page.eval(() => { document.querySelector('.la-head')?.click() })
await wait(300)
const laneEvs = await page.eval(() => document.querySelector('.la-evs')?.textContent ?? '')
check('D 子 agent 摘离的任务：主转录里没有这一行', !promoted.mainSigs.some((t) => t.includes('已摘为后台任务')), JSON.stringify(promoted.mainSigs))
check('D 子 agent 摘离的任务：信号在它自己的会话（车道活动行）里', /已摘为后台任务/.test(laneEvs), JSON.stringify(laneEvs.slice(0, 120)))

// 完成通知回主会话，并渲染成一行 signal
await page.eval((sid) => {
  window.__dispatch({ event_type: 'task_result_delivered', session_id: sid, task_id: 'tooltask-sub', name: 'bash', result: 'done' })
}, sid)
await poll('[...document.querySelectorAll(".sig")].some((x) => /Task tooltask-sub/.test(x.textContent ?? ""))')
const done = await page.eval(() => ({
  sigs: [...document.querySelectorAll('.sig')].map((x) => x.textContent ?? ''),
  deliveries: [...document.querySelectorAll('.sig-t')].map((x) => x.textContent ?? ''),
}))
check('D 任务完成：主会话有一行 signal（Task … completed）', done.sigs.some((t) => /Task tooltask-sub/.test(t)), JSON.stringify(done.sigs.slice(0, 4)))
check('D 任务完成的行落在主会话（不是只在子 agent 里）', done.deliveries.includes('task_delivery'), JSON.stringify(done.deliveries))

// ---------------------------------------------------------------- E 压缩完成
await page.eval((sid) => {
  const d = window.__dispatch
  d({ event_type: 'compress_start', session_id: sid, run_id: 'rm', before: 40 })
  d({ event_type: 'compress_end', session_id: sid, run_id: 'rm', before: 40, after: 12, ctx_tokens: 2000 })
}, sid)
await poll('[...document.querySelectorAll(".sig")].some((x) => /上下文压缩完成/.test(x.textContent ?? ""))')
const compactMain = await page.eval(() => [...document.querySelectorAll('.sig')].map((x) => x.textContent ?? ''))
check('E 主会话压缩完成 → 一行「上下文压缩完成」', compactMain.some((t) => /上下文压缩完成/.test(t)), JSON.stringify(compactMain.slice(0, 4)))
// 子 agent 的压缩：挂在它的卡片项上（车道活动行）
await page.eval((sid) => {
  window.__dispatch({ event_type: 'compress_start', session_id: sid, run_id: 'rs', before: 30 })
  window.__dispatch({ event_type: 'compress_end', session_id: sid, run_id: 'rs', before: 30, after: 9 })
}, sid)
await poll('/上下文压缩完成/.test(document.querySelector(".la-evs")?.textContent ?? "")')
const compactSub = await page.eval(() => ({
  lane: document.querySelector('.la-evs')?.textContent ?? '',
  mainSigs: [...document.querySelectorAll('.rt-turn-ans .sig')].map((x) => x.textContent ?? ''),
}))
check('E 子 agent 压缩完成：信号在它的会话（车道活动行）里', /上下文压缩完成/.test(compactSub.lane), JSON.stringify(compactSub.lane.slice(0, 160)))
check('E 子 agent 压缩完成：不在主转录多画一行', compactSub.mainSigs.filter((t) => t.includes('上下文压缩完成')).length === 0, JSON.stringify(compactSub.mainSigs))

// ---------------------------------------------------------------- F 暖恢复保留运行态
// 用户 2026-11 报障：上下文压缩之后刷新页面（进程内的 run 其实还在跑），Working 变成
// Worked for，压缩完成那一行也没有收缩进 Working 盒子。破坏点在恢复路径：
//   · checkpoint 恢复无条件把 running 收敛成 done、runEnded 恒 true；
//   · 之后到达的记录被 afterRunNow() 判成「交卷后到达」→ 落在盒外的时间线上。
// 判据 = bridge new_session 的 live 位 + checkpoint 里顶层 run 节点的 status。
const cpBlocks = [
  { kind: 'user', id: 1, text: '这一轮的问题', ts: Date.now() - 6000 },
  { kind: 'assistant', id: 2, text: '写到一半的正文', run_id: 'rh', ts: Date.now() - 5000 },
  { kind: 'tool', id: 3, name: 'bash', args: '{}', status: 'running', run_id: 'rh', tool_id: 'th', ts: Date.now() - 4000 },
]
const hydrate = (live, blocks) => page.eval((payload) => {
  const store = window.__store
  const sid = 's-hyd'
  store.setState({ activeSessionId: sid, sessions: [{ id: sid, title: 'hyd' }], projects: [{ path: '/mock/w', sessions: [{ id: sid, title: 'hyd' }] }] })
  window.__hydrate(sid, {
    schema_version: 1,
    blocks: payload.blocks,
    run_nodes: [{ id: 'rh', kind: 'run', status: 'running', label: 'run', depth: 0, started_at: Date.now() - 6000 }],
    turns: [],
  }, undefined, { live: payload.live })
}, { live, blocks })
const compactEv = () => page.eval(() => {
  const d = window.__dispatch
  d({ event_type: 'compress_start', session_id: 's-hyd', run_id: 'rh', before: 639 })
  d({ event_type: 'compress_end', session_id: 's-hyd', run_id: 'rh', before: 639, after: 15, ctx_tokens: 2000 })
})
const heads = () => page.eval(() => ({
  live: document.querySelector('.lv-lbl')?.textContent ?? '',
  livePanel: !!document.querySelector('.cy-live'),
  fold: document.querySelector('.rt-act-k')?.textContent ?? '',
  insideSig: [...document.querySelectorAll('.cy-live .sig')].map((x) => x.textContent ?? ''),
  // 盒外的时间线 = 结论区 + 结算之后的那条记录通道（见 postRecord：交卷后到达的记录
  // 排在账与产出卡后面，形状是 .rt-turn-line）
  outsideSig: [...document.querySelectorAll('.rt-turn-ans .sig, .rt-turn-line .sig')].map((x) => x.textContent ?? ''),
  order: [...(document.querySelector('.rt-turn')?.children ?? [])].map((el) => (el.className || '').split(' ')[0]),
}))

await density('concise')
await hydrate(true, cpBlocks)
await poll('!!document.querySelector(".cy-live")')
const warm0 = await heads()
check('F 暖恢复（live=true）：在跑的 run 仍显示 Working（不许折叠成 Worked for）',
  warm0.livePanel && warm0.live === 'Working', JSON.stringify({ live: warm0.live, fold: warm0.fold, panel: warm0.livePanel }))
await compactEv()
await poll('[...document.querySelectorAll(".sig")].some((x) => /上下文压缩完成/.test(x.textContent ?? ""))')
const warm1 = await heads()
check('F 暖恢复：AgentEnd 还没来，压缩完成的 signal 收缩在 Working 盒子内部',
  warm1.insideSig.some((t) => /上下文压缩完成/.test(t)) && !warm1.outsideSig.some((t) => /上下文压缩完成/.test(t)),
  JSON.stringify({ inside: warm1.insideSig.slice(0, 2), outside: warm1.outsideSig.slice(0, 2) }))

// 恢复之后那次运行真的交卷了：AgentEnd 一到，盒子必须能折叠成 Worked for（不许卡在 Working）
await page.eval(() => {
  window.__dispatch({ event_type: 'agent_end', session_id: 's-hyd', run_id: 'rh', finish_reason: 'stop' })
})
await poll('!!document.querySelector(".rt-act-k")')
const folded = await heads()
check('F 暖恢复之后 AgentEnd 到达：盒子折叠成 Worked for（时钟停、不再 Working）',
  !folded.livePanel && /Worked/.test(folded.fold), JSON.stringify({ live: folded.live, fold: folded.fold, panel: folded.livePanel }))

await hydrate(false, cpBlocks)
await poll('!!document.querySelector(".rt-act-k")')
const cold0 = await heads()
check('F 冷恢复（live=false）：幽灵 run 收敛，显示 Worked for',
  !cold0.livePanel && /Worked/.test(cold0.fold), JSON.stringify({ live: cold0.live, fold: cold0.fold, panel: cold0.livePanel }))
await compactEv()
await poll('[...document.querySelectorAll(".rt-turn-ans .sig")].some((x) => /上下文压缩完成/.test(x.textContent ?? ""))')
const cold1 = await heads()
check('F 冷恢复：交卷之后到达的 signal 排在账/产出卡之后（不是夹在结论与账之间）',
  (() => { const o = cold1.order; const i = o.lastIndexOf('rt-turn-line'); return i >= 0 && i >= o.indexOf('rt-turn-foot') && i > o.indexOf('rt-turn-artifacts') })(),
  JSON.stringify(cold1.order))
check('F 冷恢复：交卷之后到达的 signal 落在盒外的时间线上',
  cold1.outsideSig.some((t) => /上下文压缩完成/.test(t)), JSON.stringify({ inside: cold1.insideSig.slice(0, 2), outside: cold1.outsideSig.slice(0, 2) }))

// ------------------------------------------------- F2 长运行中段到达的回执
// 用户 2026-11 截图：19 分钟的运行里 05:57 已经交付过一段正文（narration），之后 06:05
// 到达的 `Task … completed` 全被判成「交卷后抵达」，堆在 Working 盒子外面（永远在最下方），
// 盒子里只剩思考与 toolcall。破坏点：恢复出来的 record 没有 after_run 位（omitempty 会把
// false 吃掉），位置兜底又只看「轮内最后一段正文」——运行中段到达的记录必须按**邻居运行块**判。
const nowF2 = Date.now()
const cpNarr = [
  { kind: 'user', id: 1, text: '这一轮的问题', ts: nowF2 - 20000 },
  { kind: 'assistant', id: 2, text: 'D2 方案完成，先交代结论', run_id: 'rh', ts: nowF2 - 15000 },
  { kind: 'tool', id: 3, name: 'bash', args: '{"command":"x"}', status: 'done', run_id: 'rh', tool_id: 'n1', ts: nowF2 - 14000 },
  { kind: 'task_delivery', id: 4, task_id: 'tk-narr', status: 'completed', result: 'done', ts: nowF2 - 10000 },
  { kind: 'tool', id: 5, name: 'bash', args: '{"command":"y"}', status: 'done', run_id: 'rh', tool_id: 'n2', ts: nowF2 - 5000 },
  { kind: 'assistant', id: 6, text: '', thinking: '还在跑', run_id: 'rh', ts: nowF2 - 2000 },
]
await hydrate(true, cpNarr)
await poll('!!document.querySelector(".cy-live")')
const narr = await heads()
const narrOrder = await page.eval(() => {
  const box = document.querySelector('.cy-live')
  if (!box) return { ok: false, why: 'no box' }
  const sig = box.querySelector('.sig')
  const batches = [...box.querySelectorAll('.rt-batch')]
  if (!sig || batches.length < 2) return { ok: false, why: `sig=${!!sig} batches=${batches.length}` }
  const at = sig.getBoundingClientRect().top
  const a = batches[0].getBoundingClientRect().top
  const c = batches[1].getBoundingClientRect().top
  return { ok: a < at && at < c, a, at, c }
})
check('F2 旧 record（没有 after_run 位）：运行中段到达的回执收缩在 Working 盒子内部',
  narr.insideSig.some((t) => /Task tk-narr/.test(t)) && !narr.outsideSig.some((t) => /Task tk-narr/.test(t)),
  JSON.stringify({ inside: narr.insideSig, outside: narr.outsideSig }))
check('F2 回执留在**事件顺序**里（两条工具行之间，不是堆到盒子末尾）', narrOrder.ok === true, JSON.stringify(narrOrder))
await hydrate(true, cpBlocks) // 复原，后面的用例沿用 F 的夹具

// ------------------------------------------------- G 产出卡 / 轮尾账的归属交叉矩阵
// 用户 2026-11 追问三件事：
//   ① 一轮里出现**两张**「本轮产出」卡（截图）——一个 AgentEnd 应该只有一张；
//   ② 产出卡与轮尾账（calls/tokens/cache/turn）是否都按 AgentEnd 与 session 归属；
//   ③ 自动压缩 / 异步任务结果回推 / working 未收尾·已收尾 交叉时还对不对。
// 判据：ledger 与产出卡都是**结算**，只在 AgentEnd 之后出现；一轮多次运行（任务回推后续跑）
// 各有一个 AgentEnd，但卡片合并成一张（按 path 去重）。
const gSid = 's-matrix'
const reset = () => page.eval((sid) => {
  window.__store.setState({ activeSessionId: sid, sessions: [{ id: sid, title: 'm' }] })
  window.__hydrate(sid, { schema_version: 1, blocks: [], run_nodes: [], turns: [] }, undefined, { live: true })
}, gSid)
const evs = (list) => page.eval((p) => {
  const d = window.__dispatch
  for (const [t, payload] of p) d({ event_type: t, session_id: p.sid, ...payload })
}, { sid: gSid, list })
// 上面那层包了一层：改成 (sid, list) 直接派发
const fire = async (list) => {
  for (const [t, payload] of list) {
    await page.eval((p) => window.__dispatch({ event_type: p.t, session_id: p.sid, ...p.payload }), { t, sid: gSid, payload })
  }
  await wait(400)
}
const facts = () => page.eval(() => ({
  turns: document.querySelectorAll('.rt-turn').length,
  foot: document.querySelectorAll('.rt-turn-foot').length,
  cards: document.querySelectorAll('.rt-turn-artifacts').length,
  // 卡里列了几个文件（每个文件一行）
  files: [...document.querySelectorAll('.rt-turn-artifacts')].map((el) => el.querySelectorAll('[data-art-path], .art-row, .art').length),
  cardText: [...document.querySelectorAll('.rt-turn-artifacts')].map((el) => el.textContent ?? ''),
  live: !!document.querySelector('.cy-live'),
}))
const A1 = { name: 'a.md', path: 'w/a.md', kind: 'created', added: 75, removed: 0 }
const A2 = { name: 'b.md', path: 'w/b.md', kind: 'created', added: 60, removed: 0 }

// G1 单 run 正常收尾：一张卡、一行轮尾账
await reset()
await fire([
  ['agent_start', { run_id: 'g1', timestamp: new Date(Date.now() - 4000).toISOString() }],
  ['user_inputs_consumed', { user_texts: ['G1'] }],
  ['tool_run_start', { id: 'g1t', name: 'bash', arguments: '{}', run_id: 'g1' }],
  ['tool_run_end', { id: 'g1t', name: 'bash', result: 'ok', is_error: false, run_id: 'g1' }],
  // 轮尾账的数据源是 assistant 块上的 runId + llm_end 指标 —— 两个都要有
  ['llm_start', { run_id: 'g1', request_id: 'g1r', model: 'm' }],
  ['content_chunk', { run_id: 'g1', request_id: 'g1r', content: '第 g1 轮正文' }],
  ['llm_end', { run_id: 'g1', request_id: 'g1r', model: 'm', usage: { Input: 1200, Output: 300, CacheRead: 900 } }],
])
const g1Live = await facts()
check('G1 运行中：没有轮尾账、没有产出卡（两者都是结算）',
  g1Live.live && g1Live.foot === 0 && g1Live.cards === 0, JSON.stringify(g1Live))
await fire([['agent_end', { run_id: 'g1', finish_reason: 'stop', artifacts: [A1] }]])
const g1 = await facts()
check('G1 AgentEnd 之后：一行轮尾账 + 一张卡',
  g1.foot === 1 && g1.cards === 1, JSON.stringify(g1))

// G2 运行中自动压缩（compress_start/end），同一个 AgentEnd：仍然一张卡、一到一行账
await reset()
await fire([
  ['agent_start', { run_id: 'g2', timestamp: new Date(Date.now() - 6000).toISOString() }],
  ['user_inputs_consumed', { user_texts: ['G2'] }],
  ['tool_run_start', { id: 'g2t', name: 'bash', arguments: '{}', run_id: 'g2' }],
  ['tool_run_end', { id: 'g2t', name: 'bash', result: 'ok', is_error: false, run_id: 'g2' }],
  ['compress_start', { run_id: 'g2', before: 300 }],
  ['compress_end', { run_id: 'g2', before: 300, after: 20, ctx_tokens: 1000 }],
  // 轮尾账的数据源是 assistant 块上的 runId + llm_end 指标 —— 两个都要有
  ['llm_start', { run_id: 'g2', request_id: 'g2r', model: 'm' }],
  ['content_chunk', { run_id: 'g2', request_id: 'g2r', content: '第 g2 轮正文' }],
  ['llm_end', { run_id: 'g2', request_id: 'g2r', model: 'm', usage: { Input: 1500, Output: 400, CacheRead: 1200 } }],
])
const g2Live = await facts()
check('G2 自动压缩后仍在跑：没有提前结算出轮尾账/产出卡',
  g2Live.foot === 0 && g2Live.cards === 0, JSON.stringify(g2Live))
await fire([['agent_end', { run_id: 'g2', finish_reason: 'stop', artifacts: [A1] }]])
const g2 = await facts()
check('G2 压缩 + AgentEnd：仍然一行账、一张卡（压缩不产生第二张）',
  g2.foot === 1 && g2.cards === 1, JSON.stringify(g2))

// G3 一轮两次运行（后台任务结果回推 → 续跑），两个 AgentEnd 各带产出 → 只画**一张**卡
await reset()
await fire([
  ['agent_start', { run_id: 'g3a', timestamp: new Date(Date.now() - 9000).toISOString() }],
  ['user_inputs_consumed', { user_texts: ['G3'] }],
  ['tool_run_start', { id: 'g3t', name: 'bash', arguments: '{}', run_id: 'g3a' }],
  // 轮尾账的数据源是 assistant 块上的 runId + llm_end 指标 —— 两个都要有
  ['llm_start', { run_id: 'g3a', request_id: 'g3ar', model: 'm' }],
  ['content_chunk', { run_id: 'g3a', request_id: 'g3ar', content: '第 g3a 轮正文' }],
  ['llm_end', { run_id: 'g3a', request_id: 'g3ar', model: 'm', usage: { Input: 900, Output: 200, CacheRead: 700 } }],
  ['agent_end', { run_id: 'g3a', finish_reason: 'stop', artifacts: [A1] }],
  ['task_result_delivered', { task_id: 'tg3', result: 'done' }],
  ['agent_start', { run_id: 'g3b', timestamp: new Date().toISOString() }],
  // 第二次运行要做**自己的**事（新工具 + 新正文 + 一次压缩）——真实续跑就是这样；
  // 只回填上一轮那个工具 id 的话，这一段没有任何属于 g3b 的块，也就没有第二个盒子。
  ['llm_start', { run_id: 'g3b', request_id: 'g3br' , model: 'm' }],
  ['content_chunk', { run_id: 'g3b', request_id: 'g3br', content: '续跑正文' }],
  ['tool_run_start', { id: 'g3t2', name: 'bash', arguments: '{}', run_id: 'g3b' }],
  ['tool_run_end', { id: 'g3t2', name: 'bash', result: 'ok', is_error: false, run_id: 'g3b' }],
  ['compress_start', { run_id: 'g3b', before: 300 }],
  ['compress_end', { run_id: 'g3b', before: 300, after: 20, ctx_tokens: 1200 }],
  // 轮尾账的数据源是 assistant 块上的 runId + llm_end 指标 —— 两个都要有
  ['llm_start', { run_id: 'g3b', request_id: 'g3br', model: 'm' }],
  ['content_chunk', { run_id: 'g3b', request_id: 'g3br', content: '第 g3b 轮正文' }],
  ['llm_end', { run_id: 'g3b', request_id: 'g3br', model: 'm', usage: { Input: 800, Output: 150, CacheRead: 600 } }],
  ['agent_end', { run_id: 'g3b', finish_reason: 'stop', artifacts: [A2] }],
])
const g3 = await facts()
check('G3 一轮两次运行：仍然只有一行轮尾账', g3.foot === 1, JSON.stringify(g3))
check('G3 一轮两次运行：产出卡合并成一张（不是两张「本轮产出 1 个文件」）',
  g3.cards === 1, JSON.stringify({ cards: g3.cards, text: g3.cardText.map((t) => t.slice(0, 60)) }))
check('G3 合并后的卡列出两个文件', g3.cardText.length === 1 && /a\.md/.test(g3.cardText[0]) && /b\.md/.test(g3.cardText[0]),
  JSON.stringify(g3.cardText.map((t) => t.slice(0, 80))))
/* 用户 2026-11 的两个形态（简洁档）：
   情况一 = 一次运行 → 一个盒子，运行中到达的信号在盒内；
   情况二 = run1 正常 AgentEnd → 异步任务结果通知（**顶层，盒外**）→ run2 起新盒子。
   判据 = 盒子按 run 分段，段间的记录落在两个盒子之间。 */
const shape = () => page.eval(() => {
  const turn = document.querySelector('.rt-turn')
  if (!turn) return null
  return [...turn.children]
    .filter((el) => !el.classList.contains('rt-head'))
    .map((el) => {
      const c = (el.className || '').split(' ')[0]
      const t = (el.textContent || '').replace(/\s+/g, ' ').trim()
      return c === 'rt-act' || c === 'cy-live' ? c + ':' + t.slice(0, 22) : c === 'rt-turn-line' ? 'line:' + t.slice(0, 22) : c
    })
})
// 情况二：两次运行（各一个盒子）+ 盒间那条回执在**顶层**
const order2 = await shape()
check('G3 情况二：一轮两次运行画出**两个**盒子（run1 一个、run2 一个）',
  order2.filter((x) => /^(rt-act|cy-live)/.test(x)).length === 2, JSON.stringify(order2))
/* 整轮都交卷时：每段结论在盒外，账 + 合并卡在轮尾，**结算之后**才轮到交卷后到达的回执
   （用户 2026-11 口径：signal 只可能在 Working 盒子里，或者在 tokens/artifacts 后面）。 */
const at2 = (re) => order2.findIndex((x) => re.test(x))
check('G3 情况二：两段运行的结论都在盒外（各一段，不折进 Worked 盒子）',
  order2.filter((x) => /^rt-turn-ans/.test(x)).length === 2 &&
    at2(/^rt-turn-ans/) === 1 && at2(/^rt-turn-ans/) < at2(/^rt-turn-foot/),
  JSON.stringify(order2))
check('G3 情况二：交卷后到达的「Task … completed」在**结算之后**（顶层，不在盒里）',
  at2(/^line:Task /) > at2(/^rt-turn-artifacts/) && at2(/^rt-turn-artifacts/) > at2(/^rt-turn-foot/) && at2(/^rt-turn-foot/) > 0,
  JSON.stringify(order2))
// 展开第二个盒子：段内那条 compress 信号必须在盒子里（情况二的下半段）
await page.eval(() => {
  const rows = document.querySelectorAll('.rt-act')
  rows[rows.length - 1]?.click()
})
await wait(400)
const inBox2 = await page.eval(() => {
  const boxes = [...document.querySelectorAll('.rt-workbox')]
  return boxes.map((el) => ({ sigs: el.querySelectorAll('.sig').length, txt: (el.textContent || '').slice(0, 40) }))
})
check('G3 情况二：第二个盒子展开后，段内到达的压缩信号在盒子里',
  inBox2.some((b) => b.sigs > 0), JSON.stringify(inBox2))

// G4 working 未收尾时任务回执到达 + 压缩：账/卡都不许提前出现
await reset()
await fire([
  ['agent_start', { run_id: 'g4', timestamp: new Date(Date.now() - 5000).toISOString() }],
  ['user_inputs_consumed', { user_texts: ['G4'] }],
  ['tool_run_start', { id: 'g4t', name: 'bash', arguments: '{}', run_id: 'g4' }],
  ['task_result_delivered', { task_id: 'tg4', result: 'done' }],
  ['compress_start', { run_id: 'g4', before: 200 }],
  ['compress_end', { run_id: 'g4', before: 200, after: 10, ctx_tokens: 800 }],
  // 轮尾账的数据源是 assistant 块上的 runId + llm_end 指标 —— 两个都要有
  ['llm_start', { run_id: 'g4', request_id: 'g4r', model: 'm' }],
  ['content_chunk', { run_id: 'g4', request_id: 'g4r', content: '第 g4 轮正文' }],
  ['llm_end', { run_id: 'g4', request_id: 'g4r', model: 'm', usage: { Input: 700, Output: 120, CacheRead: 500 } }],
])
const g4Live = await facts()
check('G4 运行中（回执+压缩都在跑）：轮尾账/产出卡都不出现，盒子仍 Working',
  g4Live.live && g4Live.foot === 0 && g4Live.cards === 0, JSON.stringify(g4Live))
await fire([['agent_end', { run_id: 'g4', finish_reason: 'stop', artifacts: [A2] }]])
const g4 = await facts()
check('G4 收尾后：一行账 + 一张卡', g4.foot === 1 && g4.cards === 1, JSON.stringify(g4))

// G5 情况二的「下半段还在跑」：第二个盒子必须是 Working（不许被第一个盒子的 AgentEnd 带走）；
//    并且**已经交卷那一段**的三件套（正文 / 轮尾账 / 产出卡）必须留在时间线上 —— 以前它们
//    跟着整轮的 `live` 一起消失：正文被收进 Worked 盒子、账与卡整块不见（用户 2026-11 截图）。
await reset()
await fire([
  ['agent_start', { run_id: 'g5a', timestamp: new Date(Date.now() - 8000).toISOString() }],
  ['user_inputs_consumed', { user_texts: ['G5'] }],
  ['tool_run_start', { id: 'g5t', name: 'bash', arguments: '{}', run_id: 'g5a' }],
  ['tool_run_end', { id: 'g5t', name: 'bash', result: 'ok', is_error: false, run_id: 'g5a' }],
  ['llm_start', { run_id: 'g5a', request_id: 'g5ar', model: 'm' }],
  ['content_chunk', { run_id: 'g5a', request_id: 'g5ar', content: '第一段的结论' }],
  ['llm_end', { run_id: 'g5a', request_id: 'g5ar', usage: { Input: 100, Output: 20 } }],
  ['agent_end', { run_id: 'g5a', finish_reason: 'stop', artifacts: [A1] }],
  ['task_result_delivered', { task_id: 'tg5', result: '回推' }],
  ['agent_start', { run_id: 'g5b', timestamp: new Date().toISOString() }],
  ['llm_start', { run_id: 'g5b', request_id: 'g5br', model: 'm' }],
  ['reasoning_chunk', { run_id: 'g5b', request_id: 'g5br', content: '想一想' }],
  ['content_chunk', { run_id: 'g5b', request_id: 'g5br', content: '正在写' }],
])
await poll('!!document.querySelector(".cy-live")')
const order5 = await shape()
const g5 = await facts()
check('G5 情况二（下半段还在跑）：第一个盒子折叠 + 回执在中间 + 第二个盒子是 Working',
  order5.length >= 3 && /^rt-act/.test(order5[0]) && /^cy-live/.test(order5[order5.length - 1]),
  JSON.stringify(order5))
// 顺序 = 盒子 → 结论正文 → 账 → 产出卡 → 回执 → Working：通知只可能在盒子里面，
// 或者在 tokens/artifacts **后面**（用户 2026-11 的口径）
const at5 = (re) => order5.findIndex((x) => re.test(x))
check('G5 下半段在跑：上一段的结论正文仍在时间线上（没被收回 Worked 盒子）',
  at5(/^rt-turn-ans/) >= 0, JSON.stringify(order5))
check('G5 下半段在跑：上一段的轮尾账还在（不跟着整轮的 live 消失）',
  g5.foot === 1 && at5(/^rt-turn-foot/) >= 0, JSON.stringify({ foot: g5.foot, order5 }))
check('G5 下半段在跑：上一段的产出卡还在', g5.cards === 1, JSON.stringify({ cards: g5.cards }))
check('G5 顺序：正文 → 账 → 卡 → 回执 → 下一个盒子（回执排在结算后面）',
  at5(/^rt-turn-ans/) < at5(/^rt-turn-foot/) && at5(/^rt-turn-foot/) < at5(/^rt-turn-artifacts/)
    && at5(/^rt-turn-artifacts/) < at5(/^line:Task /) && at5(/^line:Task /) < at5(/^cy-live/),
  JSON.stringify(order5))
// 第一段的正文不许在盒子里（盒子里只有过程）—— 判据：Worked 盒子正文里没有那句话
const g5Box = await page.eval(() => {
  const box = document.querySelector('.rt-workbox')
  const t = box?.textContent ?? ''
  return { hasAnswer: t.includes('第一段的结论'), workboxes: document.querySelectorAll('.rt-workbox').length }
})
check('G5 上一段的结论正文不在 Worked 盒子里', !g5Box.hasAnswer, JSON.stringify(g5Box))

// G3b 整轮都交卷（两次运行）：回执只画一遍，且仍在两个盒子之间
const dup = await page.eval(() => ({
  lines: [...document.querySelectorAll('.rt-turn-line')].filter((el) => /Task /.test(el.textContent ?? '')).length,
}))
check('G3 整轮交卷后：回执只有一行（结算通道不与 plan 重复渲染）', dup.lines === 1, JSON.stringify(dup))

// G6 单 run 交卷后**迟到**的回执（没有人再续跑）：账与卡是交付，通知排在它们后面
await reset()
await fire([
  ['agent_start', { run_id: 'g6', timestamp: new Date(Date.now() - 9000).toISOString() }],
  ['user_inputs_consumed', { user_texts: ['G6'] }],
  ['tool_run_start', { id: 'g6t', name: 'bash', arguments: '{}', run_id: 'g6' }],
  ['tool_run_end', { id: 'g6t', name: 'bash', result: 'ok', is_error: false, run_id: 'g6' }],
  ['llm_start', { run_id: 'g6', request_id: 'g6r', model: 'm' }],
  ['content_chunk', { run_id: 'g6', request_id: 'g6r', content: '收尾后的结论' }],
  ['llm_end', { run_id: 'g6', request_id: 'g6r', usage: { Input: 400, Output: 90, CacheRead: 300 } }],
  ['agent_end', { run_id: 'g6', finish_reason: 'stop', artifacts: [A2] }],
  ['task_result_delivered', { task_id: 'tg6', result: 'done' }],
])
const order6 = await shape()
const at6 = (re) => order6.findIndex((x) => re.test(x))
check('G6 交卷后迟到的回执：排在账与产出卡**之后**（不在结论与账之间）',
  at6(/^rt-turn-foot/) >= 0 && at6(/^rt-turn-artifacts/) >= 0 && at6(/^line:Task /) > at6(/^rt-turn-artifacts/),
  JSON.stringify(order6))

// ------------------------------------------------- H 待答的 ask_user 跨刷新复原
// 用户 2026-11：ask_user 是工具调用，就该进 checkpoint —— 之前只活在前端，
// 刷新一次面板没了，而 run 还在等这个回答（等到超时才动）。
const hSid = 's-ask'
const askCp = {
  schema_version: 1,
  blocks: [{ kind: 'user', id: 1, text: '问你三个问题', ts: Date.now() - 2000 }],
  run_nodes: [{ id: 'ra', kind: 'run', status: 'running', label: 'run', depth: 0, started_at: Date.now() - 3000 }],
  turns: [],
  pending_questions: [{
    id: 'qb-h', run_id: 'ra', timeout_ms: 1_680_000, ts: Date.now(),
    items: [
      { id: 'h1', question: '插话在转录里长什么样？', options: [{ label: '盒内按时间', preview: 'a' }, { label: '时间线单独一行' }] },
      { id: 'h2', question: '第二问？', options: [{ label: 'A' }] },
      { id: 'h3', question: '第三问？', options: [{ label: 'A' }] },
    ],
  }],
}
await page.eval((cp) => {
  const store = window.__store
  store.setState({ activeSessionId: 's-ask', sessions: [{ id: 's-ask', title: 'ask' }] })
  window.__hydrate('s-ask', cp, undefined, { live: true })
}, askCp)
await poll('!!document.querySelector(".ask")')
const asked = await page.eval(() => ({
  q: document.querySelector('.ask-q')?.textContent ?? '',
  step: document.querySelector('.ask-step')?.textContent ?? '',
  countdown: document.querySelector('.dock-countdown')?.textContent ?? '',
  options: [...document.querySelectorAll('.ask-opt')].length,
}))
check('H 刷新后待答的 ask_user 面板复原（题干/分步/倒计时/选项都在）',
  /插话在转录里长什么样/.test(asked.q) && /第 1 问/.test(asked.step) && /剩余/.test(asked.countdown) && asked.options === 2,
  JSON.stringify(asked))
await page.eval(() => window.__dispatch({ event_type: 'tool_run_end', session_id: 's-ask', id: 'aq-h', name: 'ask_user', run_id: 'ra', result: '盒内按时间', is_error: false }))
await poll('!document.querySelector(".ask")')
check('H 回答之后（tool_run_end）面板消失', !(await page.eval(() => !!document.querySelector('.ask'))), '')
await page.eval((cp) => {
  const store = window.__store
  window.__hydrate('s-ask', cp, undefined, { live: false })
}, askCp)
await wait(600)
check('H 冷恢复：不画「等你回答」的假面板', !(await page.eval(() => !!document.querySelector('.ask'))), '')

// ------------------------------------------------- I 工具行：摘离后不许被控件撑成两行
// 用户 2026-11 截图：运行中的 bash 被摘成后台任务后，行头里的「停止 + 后台运行中」把六列网格
// 挤到隐式第二行，时长 `· 2m43s…` 掉到下面单独一行（看起来像样式坏了）。行头是设计给的
// 六列（14·14·56·1fr·auto·52），控件必须合成**一个**单元格。
const rowSid = 's-row'
const rowFire = async (list) => {
  for (const [t, payload] of list) {
    await page.eval((p) => window.__dispatch({ event_type: p.t, session_id: p.sid, ...p.payload }), { t, sid: rowSid, payload })
    await wait(110)
  }
  await wait(450)
}
const rowGeo = () => page.eval(() => {
  const hd = document.querySelector('.tool-row-hd')
  if (!hd) return null
  const kids = [...hd.children].filter((el) => el.getBoundingClientRect().width > 0).map((el) => ({
    cls: String(el.className || '').split(' ')[0],
    top: Math.round(el.getBoundingClientRect().top),
  }))
  // 同一行的子项 top 会差几像素（字形基线与按钮高度不同）→ 6px 内算同一行
  const clusters = []
  for (const k of kids.map((x) => x.top).sort((a, b) => a - b)) {
    if (!clusters.length || k - clusters[clusters.length - 1] > 6) clusters.push(k)
  }
  return {
    hdH: Math.round(hd.getBoundingClientRect().height),
    rows: clusters.length,
    ctl: hd.querySelectorAll('.tool-ctl').length,
    dur: kids.find((k) => k.cls === 'dur')?.top ?? null,
    maxOther: Math.max(...kids.filter((k) => k.cls !== 'dur').map((k) => k.top)),
  }
})
await page.eval((sid) => {
  window.__store.setState({ activeSessionId: sid, sessions: [{ id: sid, title: 'row' }] })
  window.__hydrate(sid, { schema_version: 1, blocks: [], run_nodes: [], turns: [] }, undefined, { live: true })
}, rowSid)
await rowFire([
  ['agent_start', { run_id: 'rr', timestamp: new Date(Date.now() - 170000).toISOString() }],
  ['user_inputs_consumed', { user_texts: ['跑一个长命令'] }],
  ['tool_run_start', { id: 'xr', name: 'bash', arguments: JSON.stringify({ command: 'ls ~/.go-code/events | head -5; echo ---' }), run_id: 'rr', task_id: 'tooltask-row' }],
  ['tool_output', { id: 'xr', run_id: 'rr', tail: '$ ls\na\nb\n', total_bytes: 4096, seq: 1 }],
])
const rowPlain = await rowGeo()
check('I 运行中的 bash 行：行头只有一行（时长不掉到第二行）',
  !!rowPlain && rowPlain.hdH <= 36 && rowPlain.rows === 1, JSON.stringify(rowPlain))
await rowFire([['task_promoted', { session_id: rowSid, task_id: 'tooltask-row', name: 'bash', kind: 'tool' }]])
const rowPromoted = await rowGeo()
check('I 摘成后台任务后：停止 + 后台运行中 合成一个单元格，行头仍是一行',
  !!rowPromoted && rowPromoted.rows === 1 && rowPromoted.ctl === 1, JSON.stringify(rowPromoted))
check('I 摘离前后行头高度不变（32px 一档）',
  !!rowPlain && !!rowPromoted && rowPromoted.hdH === rowPlain.hdH, JSON.stringify({ plain: rowPlain?.hdH, promoted: rowPromoted?.hdH }))
check('I 时长与行内其它项同排（不再单独一行）',
  !!rowPromoted && rowPromoted.dur != null && Math.abs(rowPromoted.dur - rowPromoted.maxOther) <= 12, JSON.stringify(rowPromoted))

// 同一个症状的**第二个来源**：恢复路径上 running 块在 checkpoint 里带着部分输出（Go Block.Output），
// 于是度量 `.m` 也有内容 → 行头多出第七格，时长又一次掉到第二行（用户 2026-11 截图 2，
// 那条 bash 是刷新/重连后从 checkpoint 复原的）。尾簇收进一个单元格后两种来源都只有六格。
await page.eval(() => {
  const store = window.__store
  const st = store.getState()
  const sid = st.activeSessionId
  const startedAt = Date.now() - 163000
  const blocks = [
    { kind: 'user', id: 1, text: '跑一个长命令' },
    { kind: 'tool', id: 2, toolId: 'xr', name: 'bash', status: 'running', runId: 'rr',
      args: JSON.stringify({ command: 'ls ~/.go-code/events | head -5; echo ---' }),
      result: 'Users_admin_a\nUsers_admin_b\n', startedAt,
      promoted: true, taskId: 'tooltask-row', taskStatus: 'running' },
  ]
  const v = st.views[sid] ?? {}
  store.setState({
    blocks,
    views: { ...st.views, [sid]: { ...v, blocks, runEnded: false,
      runNodes: [{ id: 'rr', kind: 'run', status: 'running', label: 'run', depth: 0, startedAt }],
      streaming: true, turns: [], usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0 } } },
  })
})
await wait(600)
const rowRestored = await rowGeo()
const rowRestoredMetric = await page.eval(() => {
  const hd = document.querySelector('.tool-row-hd')
  const m = hd?.querySelector('.m')
  return m ? (m.textContent || '').trim() : null
})
check('I 恢复路径的 running 块：行头仍是一行（部分输出不占第七格）',
  !!rowRestored && rowRestored.rows === 1 && rowRestored.hdH <= 36,
  JSON.stringify({ ...rowRestored, metric: rowRestoredMetric }))
check('I 运行中不画「结果度量」（部分输出的大小不是结果）',
  rowRestoredMetric === null, JSON.stringify(rowRestoredMetric))
check('I 度量与控件同格，时长仍在同排',
  !!rowRestored && rowRestored.dur != null && Math.abs(rowRestored.dur - rowRestored.maxOther) <= 12,
  JSON.stringify(rowRestored))

check('无未捕获异常', errs.length === 0, JSON.stringify(errs.slice(0, 2)))

await b.close()
const bad = results.filter((r) => !r.ok)
for (const r of results) console.log(`${r.ok ? 'PASS' : 'FAIL'} ${r.name}${r.ok ? '' : '  → ' + r.detail}`)
console.log(bad.length ? `TRANSCRIPT_FIXES_FAIL (${bad.length}/${results.length})` : `TRANSCRIPT_FIXES_PASS (${results.length})`)
process.exit(bad.length ? 1 : 0)
