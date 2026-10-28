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
const waitFor = async (selector, timeout = 30000) => {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) {
    if (await page.eval((s) => !!document.querySelector(s), selector)) return true
    await wait(400)
  }
  return false
}

await page.goto(URL, { settleMs: 4000 })
await page.eval(() => { document.documentElement.dataset.skin = 'runtime' })
await wait(700)

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
await page.eval(() => { document.documentElement.dataset.skin = 'runtime' })
await wait(600)
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
