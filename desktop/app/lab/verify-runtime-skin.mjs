// 运行时皮肤的可交互断言（I 系列）——在真浏览器里跑真应用（vite dev + mock 传输层）。
// 用法：先 `npm run dev:vite`（或 `npx vite --port 5173`），再
//   node lab/verify-runtime-skin.mjs
// 判据一律是**计算值/属性**，不是截图：每条纹样的数值来自 design/agent-runtime/v2。
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

// 别用 5173：那是开发者自己的 dev 实例（另一个检出）。这里默认起在 5288。
const URL = process.env.LAB_URL || 'http://localhost:5288/'
const results = []
const check = (name, ok, detail) => results.push({ name, ok, detail })

const b = await Browser.launch({ width: 1440, height: 1200 })
const page = await b.newPage({ width: 1440, height: 1200 })
const errs = []
b.on(page.sessionId, 'Runtime.exceptionThrown', (p) => errs.push(p.exceptionDetails?.text))

const say = async (text) => {
  await page.eval((t) => {
    const ta = document.querySelector('textarea')
    const set = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set
    set.call(ta, t)
    ta.dispatchEvent(new Event('input', { bubbles: true }))
    ta.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true }))
  }, text)
}
const wait = (ms) => new Promise((r) => setTimeout(r, ms))

// 等某块真的出现（mock 是定时推送的，固定 sleep 在慢机器上会踩空）
// 等一个条件成真（比固定 sleep 稳：mock 的每一轮长度随内容变）
const waitUntil = async (expr, timeout = 45000) => {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) {
    if (await page.eval(new Function('return (' + expr + ')'))) return true
    await wait(400)
  }
  return false
}

const waitFor = async (selector, timeout = 30000) => {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) {
    if (await page.eval((s) => !!document.querySelector(s), selector)) return true
    await wait(400)
  }
  return false
}

await page.goto(URL, { settleMs: 4000 })
// 走 store 的动作，不手写 data-skin：任何一次 settings 变更都会让 App 的 effect
// 重新 applyNarrativeSkin(settings.narrative_skin)，手工属性会被静默盖回 current。
await page.eval(() => {
  const store = window.__store
  if (!store) throw new Error('lab hook missing: window.__store（main.tsx 的 dev 钩子）')
  const st = store.getState()
  st.setNarrativeSkin('runtime')
  // 工具行/思考折叠这一段的断言要能看见工作行：简洁档运行中它们收在
  // Working 面板里（这是它该有的样子），详细档才是摊开的
  st.setNarrativeDensity('detailed')
})
await wait(900)

// ---- 一轮带工具/思考的回答 -------------------------------------------------
await say('运行时样例')
await wait(13000)

const t1 = await page.eval(() => {
  const cs = (el, ...p) => (el ? Object.fromEntries(p.map((k) => [k, getComputedStyle(el)[k]])) : null)
  const hd = document.querySelector('.tool-row .tool-row-hd')
  return {
    rows: document.querySelectorAll('.tool-row').length,
    grid: cs(hd, 'gridTemplateColumns')?.gridTemplateColumns,
    minH: cs(hd, 'minHeight')?.minHeight,
    hdClickable: !!hd && (hd.getAttribute('role') === 'button' || hd.tagName === 'BUTTON'),
    hdExpanded: hd?.getAttribute('aria-expanded'),
    think: !!document.querySelector('.thinking-toggle'),
    thinkExpanded: document.querySelector('.thinking-toggle')?.getAttribute('aria-expanded'),
  }
})
check('tool rows render', t1.rows > 0, t1.rows)
check('I1 tool row is a button', t1.hdClickable, t1.hdClickable)
check('tool row six columns', (t1.grid || '').split(' ').length === 6, t1.grid)

// I1：真的点一下工具行，看它展开
if (t1.hdClickable) {
  await page.eval(() => document.querySelector('.tool-row .tool-row-hd')?.click())
  await wait(600)
  const after = await page.eval(() => {
    const hd = document.querySelector('.tool-row .tool-row-hd')
    return { open: hd?.getAttribute('aria-expanded') }
  })
  check('I1 clicking a tool row expands it', after.open !== t1.hdExpanded, `${t1.hdExpanded} -> ${after.open}`)
}

// I2：思考折叠
if (t1.think) {
  await page.eval(() => document.querySelector('.thinking-toggle')?.click())
  await wait(600)
  const exp = await page.eval(() => document.querySelector('.thinking-toggle')?.getAttribute('aria-expanded'))
  check('I2 thinking toggle flips aria-expanded', exp !== t1.thinkExpanded, `${t1.thinkExpanded} -> ${exp}`)
}

// ---- 提问卡 ---------------------------------------------------------------
await say('修复登录接口返回 401')
if (!(await waitFor('.dock .ask'))) {
  console.log('FAIL  the ask card never appeared')
  process.exit(1)
}
await wait(600)
const t2 = await page.eval(() => {
  const cs = (el, ...p) => (el ? Object.fromEntries(p.map((k) => [k, getComputedStyle(el)[k]])) : null)
  const card = document.querySelector('.dock .ask')
  const opt = card?.querySelector('.ask-opt')
  return {
    cards: document.querySelectorAll('.dock .ask').length,
    padding: cs(card, 'padding')?.padding,
    head: cs(card?.querySelector('.ask-head'), 'padding')?.padding,
    body: cs(card?.querySelector('.ask-body'), 'padding')?.padding,
    radius: cs(card, 'borderRadius')?.borderRadius,
    optCols: cs(opt, 'gridTemplateColumns')?.gridTemplateColumns,
    btnDisabled: document.querySelector('.dock .ask-btn')?.disabled,
    scope: document.querySelector('.dock') ? getComputedStyle(document.querySelector('.dock')).containerType : null,
  }
})
check('ask cards render', t2.cards > 0, t2.cards)
check('ask card has no padding of its own', t2.padding === '0px', t2.padding)
check('ask head 14px 14px 0', t2.head === '14px 14px 0px', t2.head)
check('ask body 12px 6px 0', t2.body === '12px 6px 0px', t2.body)
check('ask card r12', t2.radius === '12px', t2.radius)
check('ask option 3 tracks', (t2.optCols || '').split(' ').length === 3, t2.optCols)
check('ask scope is a container', t2.scope === 'inline-size', t2.scope)
check('I3 continue is disabled until answered', t2.btnDisabled === true, t2.btnDisabled)

// I4：数字键直选 + 选后按钮可用
const t3 = await page.eval(() => {
  const card = document.querySelector('.dock .ask')
  if (!card) return 0
  const opts = [...card.querySelectorAll('.ask-opt')]
  card.dispatchEvent(new KeyboardEvent('keydown', { key: '2', bubbles: true, cancelable: true }))
  return opts.length
})
await wait(400)
const t4 = await page.eval(() => {
  const card = document.querySelector('.dock .ask')
  const opts = [...card.querySelectorAll('.ask-opt')]
  const btn = document.querySelector('.dock .ask-btn')
  return {
    checked: opts.map((o) => o.getAttribute('aria-checked')),
    enabled: btn ? !btn.disabled : null,
    bg: opts[1] ? getComputedStyle(opts[1]).backgroundColor : null,
  }
})
check('I4 digit key picks the nth option', t3 > 1 && t4.checked[1] === 'true', `${t3} opts / ${JSON.stringify(t4.checked)}`)
check('picked option gets the accent ground', t4.bg !== 'rgba(0, 0, 0, 0)', t4.bg)

// ---- 视觉产出行 -------------------------------------------------------------
// 新开一轮干净的会话：提问面板还挂着（没作答 = 工具阻塞），会挡住后面的断言
await page.goto(URL, { settleMs: 3000 })
await page.eval(() => window.__store.getState().setNarrativeSkin('runtime'))
await wait(700)
await say('html 预览')
await waitFor('.cy-frameline .cy-act', 25000)
// 等这一轮流完（流式中代码块会被重建，这时点开的折叠状态会被重置）
await waitFor('.answer-acts', 25000)
await wait(1200)
const t5 = await page.eval(() => {
  const cs = (el, ...p) => (el ? Object.fromEntries(p.map((k) => [k, getComputedStyle(el)[k]])) : null)
  const act = document.querySelector('.cy-frameline .cy-act')
  const col = document.querySelector('.cy-frameline .rt-collapse')
  return {
    present: !!act,
    rows: cs(col, 'gridTemplateRows')?.gridTemplateRows,
    expanded: act?.getAttribute('aria-expanded'),
  }
})
check('visual output is one frameless row', t5.present, t5.present)
check('I9 the artifact row starts folded', t5.rows === '0px', t5.rows)
// 流式期间 markdown 块会被重建，点开的折叠状态跟着丢 —— 先等光标消失
// （这一轮真的写完了），再点；仍被重建就再点一次，最多八次。
const openFrameline = async () => {
  for (let i = 0; i < 8; i++) {
    const caret = await page.eval(() => !!document.querySelector('.rt-caret'))
    if (caret) { await wait(700); continue }
    await page.eval(() => document.querySelector('.cy-frameline .cy-act')?.click())
    await wait(700)
    const exp = await page.eval(() => document.querySelector('.cy-frameline .cy-act')?.getAttribute('aria-expanded'))
    if (exp === 'true') return true
  }
  return false
}
if (t5.present) {
  const opened = await openFrameline()
  check('I9 the artifact row opens', opened, opened)
  const t6 = await page.eval(() => {
    const cs = (el, ...p) => (el ? Object.fromEntries(p.map((k) => [k, getComputedStyle(el)[k]])) : null)
    const fl = document.querySelector('.cy-frameline')
    return {
      expanded: fl?.querySelector('.cy-act')?.getAttribute('aria-expanded'),
      rows: cs(fl?.querySelector('.rt-collapse'), 'gridTemplateRows')?.gridTemplateRows,
      frame: cs(fl?.querySelector('.pv-frame'), 'height')?.height,
      pv: cs(fl?.querySelector('.pv'), 'padding')?.padding,
    }
  })
  check('artifact frame is 136px (in a turn)', t6.frame === '136px', t6.frame)
  check('artifact panel padding 12/14/11', t6.pv === '12px 14px 11px', t6.pv)
}


// ---- P3 · composer：`/` 技能、`@` 文件目录、多图扇形 + 灯箱 -------------------
await page.goto(URL, { settleMs: 3000 })
await page.eval(() => { document.documentElement.dataset.skin = 'runtime' })
await wait(700)

const setText = (v) => page.eval((x) => {
  const ta = document.querySelector('textarea')
  ta.focus()
  const set = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set
  set.call(ta, x)
  ta.dispatchEvent(new Event('input', { bubbles: true }))
}, v)

await setText('/')
await wait(500)
const p1 = await page.eval(() => {
  const cs = (el, ...p) => (el ? Object.fromEntries(p.map((k) => [k, getComputedStyle(el)[k]])) : null)
  const pop = document.querySelector('.pop[data-pop="cmd"]')
  const row = pop?.querySelector('.pop-row')
  return {
    present: !!pop,
    search: !!pop?.querySelector('.pop-search input'),
    labels: [...(pop?.querySelectorAll('.pop-label') ?? [])].map((l) => l.textContent),
    rows: pop?.querySelectorAll('.pop-row').length ?? 0,
    kbd: row?.querySelector('kbd')?.textContent,
    hasM: !!row?.querySelector('.m')?.textContent,
    rowH: cs(row, 'height')?.height,
    foot: pop?.querySelector('.pop-foot')?.textContent,
    legacyMenu: !!document.querySelector('.cmd-menu'),
  }
})
check('/ opens the design popover', p1.present && !p1.legacyMenu, `${p1.present} legacy=${p1.legacyMenu}`)
check('/ rows are 28px with a kbd column', p1.rowH === '28px' && p1.kbd === '1', `${p1.rowH} kbd=${p1.kbd}`)
check('/ groups say how many are in them', p1.labels.length >= 1 && /\d/.test(p1.labels[0] || ''), p1.labels.join('|'))
check('/ row carries its one-line reason', p1.hasM, p1.hasM)

// ↑↓ 移动 + Enter 插入
await page.eval(() => document.querySelector('textarea').dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true, cancelable: true })))
await wait(300)
const p2 = await page.eval(() => ({ active: [...document.querySelectorAll('.pop[data-pop="cmd"] .pop-row')].findIndex((r) => r.classList.contains('is-active')) }))
check('I4 ↓ moves the active row', p2.active === 1, p2.active)
await page.eval(() => document.querySelector('textarea').dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true })))
await wait(400)
const p3 = await page.eval(() => ({ text: document.querySelector('textarea')?.value, open: !!document.querySelector('.pop[data-pop="cmd"]') }))
check('I5 Enter inserts the active command', p3.text.startsWith('/') && !p3.open, JSON.stringify(p3))

// @ 文件目录
await setText('@')
await wait(900)
const p4 = await page.eval(() => {
  const pop = document.querySelector('.pop[data-pop="src"]')
  return {
    present: !!pop,
    dirs: pop?.querySelectorAll('.pop-row.dir').length ?? 0,
    hasFoot: !!pop?.querySelector('.pop-foot'),
    hasSearch: !!pop?.querySelector('.pop-search input'),
  }
})
check('@ opens the design file popover', p4.present && p4.hasSearch, JSON.stringify(p4))
check('@ lists directories as .pop-row.dir', p4.dirs > 0, p4.dirs)

// 多图扇形 + 灯箱
await setText('')
await page.eval(async () => {
  const c = document.createElement('canvas'); c.width = c.height = 8
  c.getContext('2d').fillRect(0, 0, 8, 8)
  const blob = await new Promise((r) => c.toBlob(r, 'image/png'))
  const ta = document.querySelector('textarea')
  for (let i = 0; i < 3; i++) {
    const dt = new DataTransfer()
    dt.items.add(new File([blob], `shot${i}.png`, { type: 'image/png' }))
    ta.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }))
  }
})
await wait(1500)
const p5 = await page.eval(() => {
  const cs = (el, ...p) => (el ? Object.fromEntries(p.map((k) => [k, getComputedStyle(el)[k]])) : null)
  const wrap = document.querySelector('.cy-imgs')
  const im = wrap?.querySelector('.cy-im')
  return {
    present: !!wrap,
    n: wrap?.getAttribute('data-n'),
    count: wrap?.querySelectorAll('.cy-im').length ?? 0,
    w: cs(im, 'width')?.width,
    h: cs(im, 'height')?.height,
    mid: im?.style.getPropertyValue('--mid'),
    rotates: [...(wrap?.querySelectorAll('.cy-im') ?? [])].map((e) => getComputedStyle(e).transform),
  }
})
check('images are a fan of three', p5.present && p5.n === '3' && p5.count === 3, JSON.stringify({ n: p5.n, c: p5.count }))
check('fan cards are 64x45 and overlapped', p5.w === '64px' && p5.h === '45px', `${p5.w}x${p5.h}`)
check('each card is rotated around the middle', new Set(p5.rotates).size === 3, p5.rotates.join(' | '))

await page.eval(() => document.querySelector('.cy-im')?.click())
await wait(400)
const p6 = await page.eval(() => {
  const box = document.querySelector('.cy-lb')
  return { present: !!box, caption: box?.querySelector('figcaption')?.textContent, x: !!box?.querySelector('.cy-lb-x') }
})
check('I10 clicking a card opens the lightbox', p6.present && p6.x, JSON.stringify(p6))
await page.eval(() => document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true })))
await wait(300)
check('I11 esc closes the lightbox', !(await page.eval(() => !!document.querySelector('.cy-lb'))), 'still open')



// 运行时（AgentStart → AgentEnd）这一整段都在「Working」里：标题 + 四行窗口，
// 每行带度量（与工具行同一口径）。这是设计 图1 的形态。
await page.goto(URL, { settleMs: 3000 })
await page.eval(() => {
  const st = window.__store.getState()
  st.setNarrativeSkin('runtime')
  st.setNarrativeDensity('concise')
})
await wait(700)
await say('运行时样例')
await waitFor('.cy-live', 30000)
await wait(2500)
const lp = await page.eval(() => {
  const l = document.querySelector('.cy-live')
  if (!l) return { present: false }
  const rows = [...l.querySelectorAll('.lv-row')]
  return {
    present: true,
    head: l.querySelector('.cy-live-head')?.textContent ?? '',
    n: l.querySelector('.lv-n')?.textContent ?? '',
    act: l.querySelector('.lv-act')?.textContent ?? '',
    rows: rows.length,
    capped: rows.length <= 4,
    measured: rows.filter((r) => (r.querySelector('.m')?.textContent ?? '') !== '').length,
    outsideWork: !!document.querySelector('.rt-turn-ans .tool-group'),
  }
})
check('running: the whole run lives in one Working block', lp.present && /Working/.test(lp.head), JSON.stringify({ present: lp.present, head: lp.head }))
check('running: the block counts calls', /\d+ calls?/.test(lp.n), lp.n)
check('running: the window keeps four rows', lp.capped && lp.rows > 0, `${lp.rows} rows`)
check('running: rows carry the same measure as a tool row', lp.measured > 0, String(lp.measured))
check('running: no work painted outside the block', !lp.outsideWork, lp.outsideWork)

// ---- 密度：简洁档收起 = 只有结算行 + 结论；详细档 = 全过程铺开 -----------------
const freshTurn = async (density) => {
  await page.goto(URL, { settleMs: 3000 })
  await page.eval((d) => {
    const st = window.__store.getState()
    st.setNarrativeSkin('runtime')
    st.setNarrativeDensity(d)
  }, density)
  await wait(700)
  await say('运行时样例')
  // 这一轮真的跑完才算「AgentEnd 之后」：live 面板与流式光标都消失、且这一轮
  // 不再标着 data-open（运行中的那一轮默认是敞开的）。
  await waitUntil("!document.querySelector('.cy-live') && !document.querySelector('.rt-caret') && !document.querySelector('.rt-turn[data-open]')", 90000)
  if (density === 'concise') {
    await waitUntil("!!document.querySelector('.rt-turn .rt-act[aria-expanded]')", 20000)
  }
  await wait(800)
}

await freshTurn('concise')
const c1 = await page.eval(() => {
  const t = [...document.querySelectorAll('.rt-turn')].pop()
  return {
    open: t?.getAttribute('data-open'),
    inTheTurn: !!t?.querySelector('.rt-turn-in'),
    fold: !!t?.querySelector('.rt-act[aria-expanded]'),
    foldText: t?.querySelector('.rt-act[aria-expanded]')?.textContent ?? '',
    fileLine: !!t?.querySelector('.cy-file'),
    kinds: [...(t?.children ?? [])].map((k) => k.className.toString().slice(0, 18)),
  }
})
check('concise: the working phase is behind one fold row', c1.fold && !c1.inTheTurn, JSON.stringify({ fold: c1.fold, inner: c1.inTheTurn }))
check('concise: the fold row settles the numbers', /Worked for/.test(c1.foldText) && /calls/.test(c1.foldText), c1.foldText)
check('concise: no second copy of the file line', !c1.fileLine, c1.fileLine)

// 展开：工作过程进来，并且有高度上限（超出滚动）
await page.eval(() => document.querySelector('.rt-turn .rt-act[aria-expanded]')?.click())
await wait(700)
const c2 = await page.eval(() => {
  const t = [...document.querySelectorAll('.rt-turn')].pop()
  // 高度上限在 .rt-turn-body 上（.rt-turn-in 是它的内容盒）
  const box = t?.querySelector('.rt-turn-body')
  if (!box) return { present: false }
  const cs = getComputedStyle(box)
  return { present: true, maxH: cs.maxHeight, overflow: cs.overflowY, capped: box.scrollHeight > box.clientHeight }
})
check('concise: expanding brings the work back', c2.present, c2.present)
check('concise: the expanded work has a height cap', c2.present && c2.maxH !== 'none' && c2.overflow === 'auto', JSON.stringify(c2))

await freshTurn('detailed')
const d1 = await page.eval(() => {
  const t = [...document.querySelectorAll('.rt-turn')].pop()
  return {
    density: t?.getAttribute('data-density'),
    fold: !!t?.querySelector('.rt-act[aria-expanded]'),
    groups: t?.querySelectorAll('.rt-turn-ans .rt-grp').length ?? 0,
    gaps: [...(t?.querySelectorAll('.rt-grp') ?? [])].map((g) => getComputedStyle(g).rowGap),
    msgMargin: t?.querySelector('.rt-turn-ans .msg') ? getComputedStyle(t.querySelector('.rt-turn-ans .msg')).marginBottom : null,
  }
})
check('detailed: no fold row, everything inline', !d1.fold && d1.groups >= 1, JSON.stringify({ fold: d1.fold, groups: d1.groups }))
check('detailed: rows sit 6px apart (no legacy 22px margins)',
  d1.gaps.every((g) => g === '6px') && d1.msgMargin === '0px', `${d1.gaps.join(',')} msg=${d1.msgMargin}`)

// 思考行：展开不转标记，只换颜色
await page.eval(() => document.querySelector('.thinking-toggle')?.click())
await wait(700)
const th = await page.eval(() => {
  const btn = document.querySelector('.thinking-toggle')
  const mk = btn?.querySelector('.rt-mk')
  const face = btn?.querySelector('.rt-face')
  return {
    expanded: btn?.getAttribute('aria-expanded'),
    state: mk?.getAttribute('data-mk-state'),
    mkColor: mk ? getComputedStyle(mk).color : null,
    faceTransform: face ? getComputedStyle(face).transform : null,
    foldTransform: btn?.querySelector('.rt-fold') ? getComputedStyle(btn.querySelector('.rt-fold')).transform : null,
  }
})
check('thinking: expanding changes the mark colour', th.expanded === 'true' && th.state === 'engaged' && th.mkColor !== 'rgb(69, 214, 165)', `${th.state} ${th.mkColor}`)
check('thinking: the mark does not rotate', th.faceTransform === 'none', th.faceTransform)

// ---- 减少动效 ---------------------------------------------------------------
await page.send('Emulation.setEmulatedMedia', { features: [{ name: 'prefers-reduced-motion', value: 'reduce' }] })
await wait(400)
const t7 = await page.eval(() => {
  const cs = (s, p) => { const el = document.querySelector(s); return el ? getComputedStyle(el)[p] : null }
  return {
    caret: cs('.rt-caret', 'animationName'),
    laneLive: cs('.la-live', 'animationName'),
    collapse: cs('.rt-collapse', 'transitionDuration'),
    cyAct: cs('.cy-act', 'transitionDuration'),
  }
})
check('reduced motion: no caret blink', t7.caret === 'none' || t7.caret === null, t7.caret)
check('reduced motion: no lane pulse', t7.laneLive === 'none' || t7.laneLive === null, t7.laneLive)
check('reduced motion: no collapse transition', ['0s', null].includes(t7.collapse), t7.collapse)

check('no runtime exceptions', errs.length === 0, errs.slice(0, 2).join(' | '))

console.log(results.map((r) => `${r.ok ? 'PASS' : 'FAIL'}  ${r.name}${r.ok ? '' : ' :: ' + JSON.stringify(r.detail)}`).join('\n'))
const failed = results.filter((r) => !r.ok).length
console.log(`\n${results.length - failed}/${results.length} pass`)
await b.close()
process.exit(failed ? 1 : 0)
