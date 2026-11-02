// Ask 面板 hover 布局抖动断言（用户 2026-11 报障「hover 选项时闪屏」）。
//
// 现场：短预览 164px / 26 行长预览 491px —— 卡片高度 337 ↔ 560（撞上 .ask 的 max-height），
// 底栏 +223px、转录可视高度 −224px，并且跟随到底把 scrollTop 推了 +224px。上下扫一遍就是
// 整个下半屏反复泵送。修法：预览是**固定高度的窗口**（.ask-pv-pre height:13em + overflow:auto），
// 卡片高度只由选项列决定。
//
// 断言：hover 任一选项、以及在相邻选项之间来回移动时，
//   ① 卡片 / 底栏 / 转录可视高度 / scrollTop 全部不变；
//   ② 长预览仍然读得到（固定窗口内部可滚，内容不丢）。
//
// 用法：npx vite --port 5288 之后 node lab/verify-ask-hover.mjs
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const results = []
const check = (name, ok, detail) => results.push({ name, ok, detail })

const b = await Browser.launch({ width: 1280, height: 1000 })
const page = await b.newPage({ width: 1280, height: 1000 })
await page.goto(URL, { settleMs: 4000})
const waitUntil = async (expr, timeout = 20000) => {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) {
    if (await page.eval(new Function('return (' + expr + ')'))) return true
    await wait(250)
  }
  return false
}
if (!(await waitUntil('!!window.__store && !!window.__dispatch'))) throw new Error('store never appeared')
await page.eval(() => window.__store.getState().setNarrativeDensity('concise'))
await page.eval(() => window.__store.setState((s) => ({ settings: { ...s.settings, narrative_skin: 'runtime' } })))
await wait(400)
await page.eval(() => { document.documentElement.dataset.skin = 'runtime' })
if (!(await page.eval(() => !!window.__store.getState().activeSessionId))) {
  await page.eval(() => window.__store.getState().newSession())
  await waitUntil('!!window.__store.getState().activeSessionId', 8000)
}
await wait(600)

// 先灌一段长转录：面板高度一变，滚动区/跟随到底的行为才是真实现场
await page.eval(() => {
  const sid = window.__store.getState().activeSessionId
  const blocks = []
  for (let i = 0; i < 12; i++) {
    blocks.push({ kind: 'user', id: 1000 + i * 3, text: '第 ' + (i + 1) + ' 问：' + 'x'.repeat(60) })
    blocks.push({ kind: 'assistant', id: 1001 + i * 3, text: '回答 ' + (i + 1) + '\n\n' + 'y'.repeat(400), streaming: false, thinking: '', runId: 'r' + i })
    blocks.push({ kind: 'tool', id: 1002 + i * 3, toolId: 't' + i, name: 'bash', status: 'done', runId: 'r' + i, durMs: 120 })
  }
  window.__store.setState((st) => ({
    blocks,
    views: { ...st.views, [sid]: { ...(st.views[sid] ?? {}), blocks, runEnded: true, runNodes: [], streaming: false, turns: [] } },
  }))
})
await wait(600)

// 三问、每题三选项、都带 preview（与用户截图里的形态同构）
const questions = [
  {
    id: 'q1', question: '插话在转录里长什么样？',
    options: [
      { label: '盒内按时间（当前实现）：一条「你 · 插话」，夹在工具行之间', preview: '【待发送 1】已插话 · 下一轮送达 ✓\n（转录里不出现这一行）\n✓ 转录最干净，不干扰「一轮 = 一条」的语义\n✗ 事后回看不知道当时补过什么' },
      { label: '时间线单独一行（不新开 Working），但位置只能在盒子之后', preview: Array.from({ length: 26 }, (_, k) => '第 ' + k + ' 行：不进转录，只在输入区给回执（模型仍然收到）').join('\n') },
      { label: '不进转录，只在输入区给回执（模型仍然收到）', preview: Array.from({ length: 8 }, (_, k) => '很短的第 ' + k + ' 行').join('\n') },
    ],
  },
  { id: 'q2', question: '第二问？', options: [{ label: 'A', preview: 'a' }, { label: 'B', preview: 'b' }] },
  { id: 'q3', question: '第三问？', options: [{ label: 'A' }, { label: 'B' }] },
]
await page.eval((qs) => {
  window.__dispatch({ event_type: 'ask_user_question', session_id: window.__store.getState().activeSessionId, run_id: 'rq', id: 'q-batch', questions: qs, timeout_ms: 1680000 })
}, questions)
await waitUntil('!!document.querySelector(".ask-opt")')
await wait(500)

const geo = () => page.eval(() => {
  const card = document.querySelector('.ask')
  const body = document.querySelector('.ask-body')
  const pv = document.querySelector('.ask-pv')
  const pre = document.querySelector('.ask-pv-pre')
  const r = (el) => (el ? { t: Math.round(el.getBoundingClientRect().top), h: Math.round(el.getBoundingClientRect().height), w: Math.round(el.getBoundingClientRect().width) } : null)
  return {
    form: card?.getAttribute('data-form'),
    card: r(card), body: r(body), pv: r(pv), pre: r(pre),
    bodyScroll: body ? { c: body.clientHeight, s: body.scrollHeight, w: body.clientWidth } : null,
    opts: [...document.querySelectorAll('.ask-opt')].map((el) => r(el)),
    dockH: Math.round(document.querySelector('.dock')?.getBoundingClientRect().height ?? 0),
    docH: document.documentElement.scrollHeight,
    nar: (() => { const n = document.querySelector('.narrative'); return n ? { top: Math.round(n.getBoundingClientRect().top), h: Math.round(n.clientHeight), sc: Math.round(n.scrollTop), sh: n.scrollHeight } : null })(),
    win: Math.round(window.scrollY),
  }
})

const optCenter = (i) => page.eval((idx) => {
  const el = document.querySelectorAll('.ask-opt')[idx]
  if (!el) return null
  const r = el.getBoundingClientRect()
  return { x: Math.round(r.left + r.width / 2), y: Math.round(r.top + r.height / 2) }
}, i)

const moveTo = async (pt) => {
  if (!pt) return
  await page.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: pt.x, y: pt.y, button: 'none', buttons: 0 })
}

// 先落到面板外，读基线
await moveTo({ x: 5, y: 5 })
await wait(300)
const base = await geo()
console.log('baseline', JSON.stringify(base))

for (let i = 0; i < 3; i++) {
  const pt = await optCenter(i)
  if (!pt) continue
  await moveTo(pt)
  // 采样 800ms：面板/选项/预览是否在抖
  const samples = []
  for (let k = 0; k < 16; k++) { await wait(50); samples.push(await geo()) }
  const sig = (g) => JSON.stringify([g.form, g.card?.t, g.card?.h, g.pv?.h, g.bodyScroll?.c, g.bodyScroll?.s, g.nar?.h, g.nar?.sc, g.win, ...g.opts.map((o) => o && o.t)])
  const uniq = [...new Set(samples.map(sig))]
  const dockUniq = [...new Set(samples.map((s) => s.dockH))]
  const docUniq = [...new Set(samples.map((s) => s.docH))]
  check(`hover 选项 ${i}：卡片/底栏/转录几何不动`, uniq.length === 1 && dockUniq.length === 1 && docUniq.length === 1,
    JSON.stringify({ layouts: uniq, dockH: dockUniq }))
}

// 长预览必须仍然读得到：固定窗口 → 内部滚动（内容不丢，只是要滚）
await moveTo(await optCenter(1))
await wait(260)
const scrollable = await page.eval(() => {
  const pre = document.querySelector('.ask-pv-pre')
  return pre ? { c: pre.clientHeight, s: pre.scrollHeight, oy: getComputedStyle(pre).overflowY } : null
})
check('长预览在固定窗口里仍可读（内部滚动，内容不丢）',
  !!scrollable && scrollable.s > scrollable.c && /auto|scroll/.test(scrollable.oy), JSON.stringify(scrollable))

// 上下扫一遍（用户在相邻两个选项之间来回移动时的真实路径）
await moveTo(await optCenter(0))
await wait(120)
const sweep = []
for (let k = 0; k < 40; k++) {
  const pt = await optCenter(k % 2)
  await moveTo(pt)
  await wait(40)
  sweep.push(await geo())
}
const sweepSig = (g) => JSON.stringify([g.card?.t, g.card?.h, g.pv?.h, ...g.opts.map((o) => o && o.t)])
check('相邻选项之间来回扫 40 步：布局一次都不变', new Set(sweep.map(sweepSig)).size === 1,
  JSON.stringify([...new Set(sweep.map(sweepSig))].slice(0, 4)))

await b.close()
const bad = results.filter((r) => !r.ok)
for (const r of results) console.log(`${r.ok ? 'PASS' : 'FAIL'} ${r.name}${r.ok ? '' : '  → ' + r.detail}`)
console.log(bad.length ? `ASK_HOVER_FAIL (${bad.length}/${results.length})` : `ASK_HOVER_PASS (${results.length})`)
process.exit(bad.length ? 1 : 0)
