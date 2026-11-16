// 交卷后手动 /compact 的落位断言（真浏览器、真组件、真 store）。
//
// 场景（用户 2026-11 截图）：一轮已经 AgentEnd，用户点了压缩。命令经 inbox 注入，
// 空闲会话为消费它**新开一次 run**（AgentStart → CompressStart/End → AgentEnd），
// 这次 run 里只有压缩块 + 一行 compress_end 信号。按 run 切段会多出一个
// 「Worked · 1 compress」盒子（时长为空）。期望：**不画盒子**，两个通知走时间线，
// 且数值口径（轮尾账）与压缩前完全一致。
//
// 用法：npx vite --port 5288 之后 node lab/verify-compact-notice.mjs
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
await wait(200)
await page.eval(() => { document.documentElement.dataset.skin = 'runtime' })
for (let i = 0; i < 3 && !(await page.eval(() => !!window.__store.getState().activeSessionId)); i++) {
  await page.eval(() => window.__store.getState().newSession())
  await waitUntil('!!window.__store.getState().activeSessionId', 8000)
}
if (!(await page.eval(() => !!window.__store.getState().activeSessionId))) throw new Error('no active session')
await wait(500)
const poll = async (expr, timeout = 5000) => {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) {
    if (await page.eval(new Function('return (' + expr + ')'))) return true
    await wait(120)
  }
  return false
}
const inject = async (blocksExpr, runEnded, turnsExpr = '[]', runsExpr = '[]') => {
  await page.eval(new Function(`
    const store = window.__store
    const st = store.getState()
    const sid = st.activeSessionId
    const blocks = ${blocksExpr}
    const turns = ${turnsExpr}
    const runNodes = ${runsExpr}
    const v = st.views[sid] ?? {}
    store.setState({
      blocks, turns, runEnded: ${runEnded},
      views: { ...st.views, [sid]: { ...v, blocks, runEnded: ${runEnded}, runNodes, streaming: false, turns,
        usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0 } } },
    })
  `))
  await wait(350)
}
const now = Date.now()
const toolRow = `{ kind: 'tool', id: 2, toolId: 't2', name: 'bash', status: 'done', runId: 'r1', durMs: 3000, startedAt: ${now - 9000} }`
const answer = `{ kind: 'assistant', id: 3, text: '干完了', streaming: false, thinking: '', runId: 'r1', durMs: 4000, agentDurMs: 60000 }`
const compression = `{ kind: 'compression', id: 4, before: 457, after: 16, ctxTokens: 27330, ctxTokensBefore: 298657, model: 'm', durMs: 196748, runId: 'rc' }`
const compactSig = `{ kind: 'signal', id: 5, fact: { sig: 'compact', etype: 'compress_end', before: 457, after: 16 }, afterRun: false }`

// ① 交卷后 compact：两个通知在时间线，不另起 Worked 盒子
await inject(`[ { kind: 'user', id: 1, text: '干活' }, ${toolRow}, ${answer}, ${compression}, ${compactSig} ]`, true)
await poll('!!document.querySelector(".rt-turn-line .rt-cp")')
const post = await page.eval(() => {
  const turn = document.querySelector('.rt-turn')
  if (!turn) return null
  const cp = turn.querySelector('.rt-cp')
  const sig = turn.querySelector('.sig[data-kind="compact"]')
  const inBox = (el) => !!(el && el.closest('.rt-work-body, .rt-workbox'))
  return {
    acts: turn.querySelectorAll('.rt-act').length,
    cpInBox: inBox(cp),
    sigInBox: inBox(sig),
    cpLine: !!(cp && cp.closest('.rt-turn-line')),
    sigLine: !!(sig && sig.closest('.rt-turn-line')),
    order: !!(cp && sig && (cp.compareDocumentPosition(sig) & Node.DOCUMENT_POSITION_FOLLOWING)),
    foot: turn.querySelector('.rt-turn-foot')?.textContent ?? '',
  }
})
check('① 只画一个 Worked 盒子（压缩不再独占一段）', post?.acts === 1, JSON.stringify(post?.acts))
check('① 压缩行在时间线上（不在任何盒子里）', post?.cpLine === true && post?.cpInBox === false, JSON.stringify(post))
check('① compress_end 信号在时间线上（不在任何盒子里）', post?.sigLine === true && post?.sigInBox === false, JSON.stringify(post))
check('① 顺序：压缩行 → 信号行', post?.order === true, JSON.stringify(post))

// ② 对照：同一轮去掉压缩，轮尾账必须逐字相同（worked/turn 不受压缩影响）
await inject(`[ { kind: 'user', id: 1, text: '干活' }, ${toolRow}, ${answer} ]`, true)
await wait(300)
const ctrl = await page.eval(() => {
  const turn = document.querySelector('.rt-turn')
  return { acts: turn?.querySelectorAll('.rt-act').length, foot: turn?.querySelector('.rt-turn-foot')?.textContent ?? '' }
})
check('② 对照（无压缩）：同样只有一个 Worked 盒子', ctrl.acts === 1, JSON.stringify(ctrl.acts))
check('② 轮尾账逐字相同（压缩不改变 calls / turn）', !!post && post.foot === ctrl.foot, JSON.stringify({ withCompact: post?.foot, control: ctrl.foot }))

// ③ 运行中的自动压缩（同一个 run 里有工具）仍在盒子里 —— 只有「只做了压缩的那次运行」才算通知
await inject(`[ { kind: 'user', id: 1, text: '干活' }, ${toolRow},
  { kind: 'compression', id: 4, before: 457, after: 16, runId: 'r1' },
  { kind: 'signal', id: 5, fact: { sig: 'compact', etype: 'compress_end', before: 457, after: 16 }, afterRun: false },
  ${answer} ]`, true)
await wait(300)
await page.eval(() => { document.querySelector('.rt-act')?.click() })
await wait(300)
const mid = await page.eval(() => {
  const turn = document.querySelector('.rt-turn')
  const cp = turn?.querySelector('.rt-cp')
  return {
    acts: turn?.querySelectorAll('.rt-act').length,
    cpInBox: !!(cp && cp.closest('.rt-work-body')),
    boxOpen: !!turn?.querySelector('.rt-work-body'),
  }
})
check('③ 过程内的压缩仍在盒子里（盒子展开后可见）', mid.boxOpen === true && mid.cpInBox === true, JSON.stringify(mid))
check('③ 过程内压缩不额外多一个盒子', mid.acts === 1, JSON.stringify(mid.acts))

check('无未捕获异常', errs.length === 0, JSON.stringify(errs.slice(0, 3)))
await b.close()

const failed = results.filter((r) => !r.ok)
for (const r of results) console.log(`${r.ok ? 'PASS' : 'FAIL'} ${r.name}${r.ok ? '' : '  ⟵ ' + r.detail}`)
if (failed.length) {
  console.log(`\nCOMPACT_NOTICE_FAIL (${failed.length}/${results.length})`)
  process.exit(1)
}
console.log(`\nCOMPACT_NOTICE_PASS (${results.length})`)
