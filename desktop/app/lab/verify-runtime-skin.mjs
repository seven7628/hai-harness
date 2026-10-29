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

// 发一条消息，并**确认它真的发出去了**：直接写 .value 再发 ⏎ 是模拟键盘的常用手法，
// 但只要那一下没落进 React 的 onChange（焦点不在、元素刚被替换、队列条里那个 textarea
// 排在前面），⏎ 就是一次空提交 —— 后面所有断言都在等一个永远不会开始的新轮。
// 所以这里读回 value、发完再看轮数/队列有没有变，没变就重试（最多三次）。
const say = async (text) => {
  for (let attempt = 0; attempt < 3; attempt++) {
    const before = await page.eval(() => ({
      turns: document.querySelectorAll('.rt-turn').length,
      queue: (window.__store.getState().pendingQueue ?? []).length,
    }))
    await page.eval((t) => {
      // 明确取输入卡的 textarea：队列条里那条「编辑某一条」的 textarea 也符合 textarea
      const ta = document.querySelector('.composer textarea') || document.querySelector('textarea')
      ta.focus()
      const set = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set
      set.call(ta, t)
      ta.dispatchEvent(new Event('input', { bubbles: true }))
    }, text)
    await wait(160)
    const typed = await page.eval(() => (document.querySelector('.composer textarea') || document.querySelector('textarea'))?.value ?? '')
    if (!typed.trim()) continue
    await page.eval(() => {
      const ta = document.querySelector('.composer textarea') || document.querySelector('textarea')
      ta.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true }))
    })
    await wait(500)
    const after = await page.eval(() => ({
      turns: document.querySelectorAll('.rt-turn').length,
      queue: (window.__store.getState().pendingQueue ?? []).length,
    }))
    if (after.turns > before.turns || after.queue > before.queue) return true
  }
  return false
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

// 每次 page.goto 之后都要等 window.__store —— 应用还在 boot 时它是 undefined，
// 直接 getState() 会在慢机器/热更新重载的瞬间炸掉（不是产品的错，是测试的时序）
const waitStore = async (timeout = 20000) => {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) {
    if (await page.eval(() => !!window.__store)) return true
    await wait(200)
  }
  throw new Error('window.__store never appeared')
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
await waitStore()
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

// ---- 运行中：盒子的呼吸 + 安静的停止键 -------------------------------------
// 设计把「跑起来了」写成两件事：盒子自己有呼吸（.prompt-box[data-live] 的
// p-breathe + 两道错开 2.3s 的 p-ring），以及停止键退回面色底、只留红字形 +
// 内描边（原文：a full red square is a fire alarm）。两样都量计算值。
await waitUntil("!!document.querySelector('.composer[data-live]')", 20000)
const live = await page.eval(() => {
  const c = document.querySelector('.composer[data-live]')
  if (!c) return null
  const stop = document.querySelector('.row2 .send-btn.is-stop')
  const scs = stop ? getComputedStyle(stop) : null
  const r = stop?.getBoundingClientRect()
  return {
    breathe: getComputedStyle(c).animationName,
    ring: getComputedStyle(c, '::before').animationName,
    ringDelay: getComputedStyle(c, '::after').animationDelay,
    stop: !!stop,
    stopColor: scs?.color ?? null,
    stopShadow: scs?.boxShadow ?? null,
    stopBg: scs?.backgroundColor ?? null,
    size: r ? [Math.round(r.width), Math.round(r.height)] : null,
  }
})
check('live: the prompt box breathes while a run is live', live?.breathe === 'rt-p-breathe', live?.breathe)
check('live: two rings, staggered 2.3s', live?.ring === 'rt-p-ring' && live?.ringDelay === '2.3s', `${live?.ring} @${live?.ringDelay}`)
check('live: the stop key turns quiet red', live?.stop === true && live?.stopShadow?.includes('inset'), `${live?.stopBg} ${live?.stopShadow}`)
check('live: send/stop key is 28px', live?.size?.[0] === 28 && live?.size?.[1] === 28, JSON.stringify(live?.size))

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

// ---- 一组调用的折叠行 = 设计 .cy-act ----------------------------------------
// 「动词 · 对象 · 这个动词数的量 · 耗时」+ 折角，行本身是按钮（role 由 button 自带），
// 点它收起/展开**这一组逐条的行**。判据是行为：aria-expanded 翻转、行真的不见。
const batch1 = await page.eval(async () => {
  const btn = document.querySelector('.rt-batch')
  if (!btn) return { present: false }
  const rowsOf = () => btn.parentElement.querySelector('.rt-collapse .tool-row')
  const out = {
    present: true,
    tag: btn.tagName,
    text: btn.textContent.trim().slice(0, 70),
    chev: !!btn.querySelector('.rt-fold'),
    aria: btn.getAttribute('aria-expanded'),
    rows: btn.parentElement.querySelectorAll('.rt-collapse .tool-row').length,
    // 语法四段：动词 … 数字 … 单位 … 耗时的单位
    grammar: /\s.*·.*(hits|lines|files|KB|[+\u2212])/.test(btn.textContent) && /ms|s$/.test(btn.textContent.trim()),
  }
  btn.click()
  await new Promise((r) => setTimeout(r, 450))
  out.closed = { aria: btn.getAttribute('aria-expanded'), rowH: rowsOf() ? rowsOf().getBoundingClientRect().height : null }
  btn.click()
  await new Promise((r) => setTimeout(r, 450))
  out.reopened = btn.getAttribute('aria-expanded')
  return out
})
check('batch: the calls fold into one line', batch1.present === true && batch1.text.length > 8, JSON.stringify(batch1))
check('batch: the line is a button with a fold mark', batch1.tag === 'BUTTON' && batch1.chev === true, `${batch1.tag} chev=${batch1.chev}`)
check('batch: it carries verb, object, the number the verb counts, and the time', batch1.grammar === true, batch1.text)
check('batch: clicking folds the rows away', batch1.closed?.aria === 'false', JSON.stringify(batch1.closed))
check('batch: clicking again brings them back', batch1.reopened === 'true', batch1.reopened)

// ---- 一组的折叠行（设计 .cy-act）：动词 + 对象 + 它数的量 + 耗时，行本身能开合 ----
// 截图里那几条红线要的就是这个东西：一行读完「做了什么、多少、多久」，折角决定
// 下面那几条逐条行露不露。
const bt = await page.eval(async () => {
  const btn = document.querySelector('.rt-batch')
  if (!btn) return { present: false }
  const body = btn.nextElementSibling
  const rows = () => body?.querySelectorAll('.tool-row').length ?? 0
  const out = {
    present: true,
    tag: btn.tagName,
    aria: btn.getAttribute('aria-expanded'),
    chev: !!btn.querySelector('.rt-fold'),
    text: (btn.textContent || '').trim(),
    rowsOpen: rows(), // 函数过不了 CDP 的序列化，必须在这里就地数出来
  }
  btn.click()
  await new Promise((r) => setTimeout(r, 450))
  out.collapsed = { aria: btn.getAttribute('aria-expanded'), visible: (body?.getBoundingClientRect().height ?? 0) > 4 }
  btn.click()
  await new Promise((r) => setTimeout(r, 450))
  out.reopened = btn.getAttribute('aria-expanded')
  return out
})
check('batch: the group folds into one line you can click', bt.present === true && bt.tag === 'BUTTON' && bt.chev === true, JSON.stringify({ tag: bt.tag, chev: bt.chev }))
check('batch: the line carries verb · measure · duration', /·/.test(bt.text || '') && /\d+\s*(ms|s)\b/.test(bt.text || ''), bt.text)
// bt.rows 是个函数（要等 DOM 真的画完才数），这里必须调用它 —— 之前写成 bt.rows > 0
// 是在拿函数和 0 比，永远假红
check('batch: open by default, so nothing is hidden behind a click', bt.aria === 'true' && bt.rowsOpen > 0, JSON.stringify({ aria: bt.aria, rows: bt.rowsOpen }))
check('batch: clicking folds the rows away', bt.collapsed?.aria === 'false' && bt.collapsed?.visible === false, JSON.stringify(bt.collapsed))
check('batch: clicking again brings them back', bt.reopened === 'true', bt.reopened)

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

// 思考正文与回答正文同一把尺：设计里的 58ch 只是散用默认值，进转写后被改成
// --md-measure（ui.components.css:2241）。两边不一致的话思考右边会空出半屏
// —— 用户 2026-10 报障「思考内容没沾满、右侧很大空白」就是这个。
const tk = await page.eval(() => {
  const tkP = document.querySelector('.rt-turn .thinking-body .tk-p')
  const mdP = document.querySelector('.rt-turn .md p')
  // 令牌是 rem，计算值是 px —— 让浏览器自己换算，别拿字符串比
  const probe = document.createElement('i')
  probe.style.maxWidth = 'var(--md-measure)'
  document.body.appendChild(probe)
  const meas = getComputedStyle(probe).maxWidth
  probe.remove()
  return {
    tk: tkP ? getComputedStyle(tkP).maxWidth : null,
    md: mdP ? getComputedStyle(mdP).maxWidth : null,
    tok: meas,
    tkBox: tkP ? Math.round(tkP.getBoundingClientRect().width) : null,
    col: Math.round((document.querySelector('.rt-turn .md')?.getBoundingClientRect().width) || 0),
  }
})
check('think text is measured like the answer text (not 58ch)', tk.tk === tk.md && tk.tk !== 'none', JSON.stringify(tk))
check('that measure is the design token --md-measure', tk.tk === tk.tok && tk.tok !== '', `${tk.tk} vs ${tk.tok}`)

// 表格：照设计 .bl-table（ui.components.css:1509-1525）量计算值。
// 这一段是把一张同构表塞进真实的 .rt-turn .md 里 —— 量的是我们自己的 CSS，
// 插件给数字列打 .n 的部分由 test:table-numbers 覆盖（hast 单测）。
const tb = await page.eval(() => {
  const host = document.querySelector('.rt-turn .md')
  if (!host) return null
  const probe = document.createElement('i')
  probe.style.color = 'var(--ink-3)'
  document.body.appendChild(probe)
  const ink3 = getComputedStyle(probe).color
  probe.style.color = 'var(--ink-2)'
  const ink2 = getComputedStyle(probe).color
  probe.remove()
  const wrap = document.createElement('div')
  wrap.className = 'md-table-wrap'
  wrap.innerHTML =
    '<table><thead><tr><th>调用</th><th class="n">耗时</th></tr></thead>' +
    '<tbody><tr><td>3</td><td class="n">1.2s</td></tr><tr><td>12</td><td class="n">880ms</td></tr></tbody></table>'
  host.appendChild(wrap)
  const cs = (el, ...p) => Object.fromEntries(p.map((k) => [k, getComputedStyle(el)[k]]))
  const table = wrap.querySelector('table')
  const th = wrap.querySelector('th')
  const thN = wrap.querySelector('th.n')
  const td = wrap.querySelector('td')
  const lastTd = wrap.querySelector('tbody tr:last-child td')
  const out = {
    ink3, ink2,
    table: cs(table, 'borderCollapse', 'fontSize', 'borderTopWidth', 'borderTopStyle'),
    th: cs(th, 'padding', 'borderBottomWidth', 'borderBottomStyle', 'fontSize', 'fontWeight', 'lineHeight', 'letterSpacing', 'textTransform', 'color', 'textAlign', 'backgroundColor'),
    td: cs(td, 'padding', 'borderBottomWidth', 'borderBottomStyle', 'color', 'verticalAlign', 'fontSize'),
    lastTd: cs(lastTd, 'borderBottomWidth'),
    thN: cs(thN, 'textAlign'),
    tdN: cs(wrap.querySelector('td.n'), 'textAlign', 'fontVariantNumeric'),
  }
  wrap.remove()
  return out
})
check('table: an audit grid, not a card — no outer frame, 13px text',
  tb?.table.borderCollapse === 'collapse' && tb.table.fontSize === '13px' && tb.table.borderTopWidth === '0px',
  JSON.stringify(tb?.table))
check('table: the header recedes to a mono label (10.5px/500, 0.05em, uppercase, ink-3)',
  tb?.th.fontSize === '10.5px' && tb.th.fontWeight === '500' && tb.th.letterSpacing === '0.525px' && tb.th.textTransform === 'uppercase' && tb.th.color === tb.ink3 && tb.th.backgroundColor === 'rgba(0, 0, 0, 0)',
  JSON.stringify(tb?.th))
check('table: header cell is 0/14/7 padding with a hairline under it',
  tb?.th.padding === '0px 14px 7px 0px' && tb.th.borderBottomWidth === '1px' && tb.th.borderBottomStyle === 'solid' && tb.th.textAlign === 'left',
  JSON.stringify({ p: tb?.th.padding, w: tb?.th.borderBottomWidth, a: tb?.th.textAlign }))
check('table: body cells are 7/14 padding, ink-2, top-aligned',
  tb?.td.padding === '7px 14px 7px 0px' && tb.td.color === tb.ink2 && tb.td.verticalAlign === 'top' && tb.td.borderBottomWidth === '1px',
  JSON.stringify({ p: tb?.td.padding, c: tb?.td.color, v: tb?.td.verticalAlign }))
check('table: the last row has no hairline under it', tb?.lastTd.borderBottomWidth === '0px', tb?.lastTd.borderBottomWidth)
check('table: numeric columns are right-aligned with tabular figures',
  tb?.thN.textAlign === 'right' && tb?.tdN.textAlign === 'right' && tb.tdN.fontVariantNumeric === 'tabular-nums',
  JSON.stringify({ th: tb?.thN.textAlign, td: tb?.tdN.textAlign, fv: tb?.tdN.fontVariantNumeric }))

// ---- 提问卡 ---------------------------------------------------------------
await say('修复登录接口返回 401')
// 60s 而不是默认 30s：这一问要等上一轮跑完（mock 的脚本化回放 15s 左右）+ 队列推送，
// 机器忙的时候 30s 会踩空 —— 踩空是测试的错，不是产品的错
if (!(await waitFor('.dock .ask', 60000))) {
  // 诊断而不是一句「没出现」：发出去没有（queue）、有没有提问事件（qs）、
  // 输入框还在不在、叙述区到哪了 —— 这四项能区分「没发」与「发了没回」
  const diag = await page.eval(() => {
    const st = window.__store.getState()
    return {
      queue: (st.pendingQueue ?? []).length,
      questions: (st.questions ?? []).length,
      textarea: !!document.querySelector('textarea'),
      turns: document.querySelectorAll('.rt-turn').length,
      composer: !!document.querySelector('.composer'),
      lastText: (document.querySelector('.narrative')?.textContent || '').trim().slice(-120),
    }
  })
  console.log('FAIL  the ask card never appeared', JSON.stringify(diag))
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
// 带 .ask-step 的卡上内距是 12（parts/ask.css 的 .ask[data-step] .ask-head）
check('ask head 12px 14px 0 (stepped card)', t2.head === '12px 14px 0px' || t2.head === '14px 14px 0px', t2.head)
check('ask body 12px 6px 0', t2.body === '12px 6px 0px', t2.body)
check('ask card r12', t2.radius === '12px', t2.radius)
check('ask option 3 tracks', (t2.optCols || '').split(' ').length === 3, t2.optCols)
check('ask scope is a container', t2.scope === 'inline-size', t2.scope)
check('I3 continue is disabled until answered', t2.btnDisabled === true, t2.btnDisabled)

/* 页脚永远在卡里：卡自己封顶、body 滚，而不是外面的 .q-list 裁一刀
   （那一刀落在页脚上时，按钮只露 4px —— 用户报障「按钮看不见」的根因）。 */
const t2b = await page.eval(() => {
  const card = document.querySelector('.dock .ask')
  const foot = card?.querySelector('.ask-foot')
  const btn = card?.querySelector('.ask-btn')
  const body = card?.querySelector('.ask-body')
  if (!card || !foot || !btn) return { ok: false }
  const c = card.getBoundingClientRect(), f = foot.getBoundingClientRect(), b = btn.getBoundingClientRect()
  return {
    ok: true,
    footInside: Math.round(f.bottom) <= Math.round(c.bottom),
    btnFullyVisible: Math.round(b.bottom) <= Math.round(c.bottom) && Math.round(b.top) >= Math.round(c.top),
    btnH: Math.round(b.height),
    cardFlex: getComputedStyle(card).display,
    bodyScrolls: body ? getComputedStyle(body).overflowY : null,
    cardCap: getComputedStyle(card).maxHeight,
    violetSoft: getComputedStyle(card.querySelector('.ask-ic')).backgroundColor,
  }
})
check('ask footer sits inside the card (nothing clips the buttons)', t2b.footInside && t2b.btnFullyVisible, JSON.stringify(t2b))
check('ask card is a capped flex column and only its body scrolls', t2b.cardFlex === 'flex' && t2b.cardCap !== 'none' && t2b.bodyScrolls === 'auto', JSON.stringify(t2b))
check('ask icon chip has its violet ground (--violet-soft resolves)', t2b.violetSoft !== 'rgba(0, 0, 0, 0)', t2b.violetSoft)

// I4：数字键直选 + 选后按钮可用
const t3 = await page.eval(() => {
  const card = document.querySelector('.dock .ask')
  if (!card) return 0
  const opts = [...card.querySelectorAll('.ask-opt')]
  card.dispatchEvent(new KeyboardEvent('keydown', { key: '2', bubbles: true, cancelable: true }))
  return opts.length
})
await wait(400)
const t2c = await page.eval(() => {
  const a = document.querySelector('.dock .ask')?.getBoundingClientRect()
  const t = document.querySelector('.rt-turn')?.getBoundingClientRect()
  return { ask: a ? Math.round(a.width) : null, col: t ? Math.round(t.width) : null }
})
check('the question card is as wide as the transcript column', t2c.ask != null && t2c.ask === t2c.col, JSON.stringify(t2c))

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

// ---- 提问卡的交互动词（设计 parts/ask.js 的 click / pointerdown / pointerover /
// keydown 四路）--------------------------------------------------------------
// 判据都是**行为**：焦点落在卡上、数字键选中、←→ 与 ↑↓ 同义、指针抢走高亮、
// Esc 跳过（未答的题按空答案交，不是关掉面板）。
const ask1 = await page.eval(() => {
  const card = document.querySelector('.dock.ask-scope .ask')
  if (!card) return { present: false }
  card.focus()
  return {
    present: true,
    focusable: card.tabIndex === 0 && document.activeElement === card,
    opts: card.querySelectorAll('.ask-opt').length,
    foot: [...card.querySelectorAll('.ask-foot .ask-all, .ask-foot .ask-btn')].map((b) => b.textContent.trim()),
  }
})
check('ask: the card itself takes focus, so 1-9 need no Tab', ask1.present === true && ask1.focusable === true, JSON.stringify(ask1))
// 页脚按设计的两种形态：单题卡 = [全选?] 跳过 提交；分步批次 = 上一问 下一问/提交
// （设计 parts/ask.js:190-208 明确不做「分步里的跳过」——中途跳过会让后面几问失去上下文）
{
  const foot = ask1.foot || []
  const isSteps = foot.includes('上一问')
  const rule = isSteps ? !foot.includes('跳过') : foot.includes('跳过')
  check('ask: the footer matches its form (skip on a single question, back/next on a batch)', rule, JSON.stringify(foot))
}

const askDigit = await page.eval(async () => {
  const card = document.querySelector('.dock.ask-scope .ask')
  if (!card) return { missing: true }
  card.focus()
  card.dispatchEvent(new KeyboardEvent('keydown', { key: '2', bubbles: true, cancelable: true }))
  await new Promise((r) => setTimeout(r, 180))
  const opts = [...card.querySelectorAll('.ask-opt')]
  return { checked: opts.map((o) => o.getAttribute('aria-checked')), n: opts.length }
})
check('ask: a number key picks that option', askDigit.checked.includes('true'), JSON.stringify(askDigit))

const askAxis = await page.eval(async () => {
  const card = document.querySelector('.dock.ask-scope .ask')
  if (!card) return { missing: true }
  const cur = () => [...card.querySelectorAll('.ask-opt')].findIndex((o) => o.dataset.cursor === '1')
  card.focus()
  card.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true, cancelable: true }))
  await new Promise((r) => setTimeout(r, 120))
  const afterDown = cur()
  card.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true, cancelable: true }))
  await new Promise((r) => setTimeout(r, 120))
  const afterRight = cur()
  return { afterDown, afterRight }
})
check('ask: ArrowRight moves the cursor like ArrowDown', askAxis.afterDown >= 0 && askAxis.afterRight !== askAxis.afterDown, JSON.stringify(askAxis))

const askHover = await page.eval(async () => {
  const card = document.querySelector('.dock.ask-scope .ask')
  if (!card) return { missing: true }
  card.focus()
  card.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true, cancelable: true }))
  await new Promise((r) => setTimeout(r, 120))
  const has = () => [...card.querySelectorAll('.ask-opt')].some((o) => o.dataset.cursor === '1')
  const before = has()
  const opt = card.querySelectorAll('.ask-opt')[1] || card.querySelectorAll('.ask-opt')[0]
  // React 的 enter/leave 是 pointerover/out 合成的，所以要发 pointerover
  opt.dispatchEvent(new PointerEvent('pointerover', { bubbles: true, relatedTarget: document.body }))
  await new Promise((r) => setTimeout(r, 180))
  return { before, after: has() }
})
check('ask: the pointer takes the highlight from the keyboard', askHover.before === true && askHover.after === false, JSON.stringify(askHover))

await page.eval(() => {
  const card = document.querySelector('.dock.ask-scope .ask')
  if (!card) return
  card.focus()
  card.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }))
})
await wait(1500)
const askEsc = await page.eval(() => {
  const st = window.__store.getState()
  return {
    panel: !!document.querySelector('.dock.ask-scope'),
    questions: (st.questions ?? []).length,
  }
})
check('ask: Esc skips the batch instead of only closing the panel', askEsc.panel === false || askEsc.questions === 0, JSON.stringify(askEsc))


// ---- 视觉产出块（运行时）：不折叠、按内容完整渲染、悬停出三个动作 -----------
// 用户 2026-10：不要折叠行、不要那些描述，默认自适应完整渲染 html frame，
// 指针移上来浮出 [预览 / 源码 / 在访达中打开]。
await page.goto(URL, { settleMs: 3000 })
await waitStore()
await page.eval(() => window.__store.getState().setNarrativeSkin('runtime'))
await wait(700)
await say('html 预览')
await waitFor('.cy-frame .pv-frame', 30000)
// 结算哨兵：流式中块会被重建，量到的节点下一秒就没了
await waitFor('.rt-turn-foot', 30000)
await wait(1500)
// 诊断：装了监听才知道是「消息根本没来」还是「来了被 source 校验丢掉」
await page.eval(() => {
  window.__msgs = []
  window.addEventListener('message', (e) => {
    const d = e.data
    if (!d || typeof d !== 'object') return
    if (d.__htmlh === undefined && d.__htmlhover === undefined) return
    window.__msgs.push({ h: d.__htmlh, hv: d.__htmlhover, fromFrame: e.source === document.querySelector('.cy-frame .pv-frame')?.contentWindow })
  })
})
// 探针（iframe 里的 $__htmlh）可能晚到：等**任一块**拿到按内容量出来的内联高度，最多 10s。
// 不指定「哪一块」：一轮里可能有好几块视觉产出（有的在折叠盒里、有的很大），
// 断言的是「探针→内联高度」这条链路通不通。
let measured = null
for (let i = 0; i < 40; i++) {
  measured = await page.eval(() => {
    for (const f of [...document.querySelectorAll('.cy-frame .pv-frame')].reverse()) {
      const h = parseFloat(f.style.height || '')
      if (Number.isFinite(h) && h > 136) return Math.round(h)
    }
    return null
  })
  if (measured) break
  await wait(250)
}
// 量之前先把它滚到视口中央：后面派的是真实鼠标坐标，元素在屏外就永远 hover 不到。
// 滚动要等它落地再量 —— 应用里 scroll-behavior 可能是 smooth，scrollIntoView 后立刻
// 读 rect 拿到的是上一帧的位置，派出去的坐标就落在了别处（这就是这条断言时红时绿的来源）。
await page.eval(() => {
  const frames = [...document.querySelectorAll('.cy-frame .pv-frame')]
  frames.at(-1)?.scrollIntoView({ block: 'center', behavior: 'instant' })
})
await wait(500)
const hr = await page.eval(() => {
  // 取**最后一块**：同一轮里旧的视觉产出可能还在（折叠盒里），它永远滚不进视口
  const frame = [...document.querySelectorAll('.cy-frame .pv-frame')].at(-1)
  const bar = frame?.closest('.cy-frame')?.querySelector('.pv-bar')
  const r = frame?.getBoundingClientRect()
  return {
    hasFrame: !!frame,
    foldRow: !!document.querySelector('.cy-frameline .cy-act'),
    head: !!document.querySelector('.cy-frame .pv-head'),
    foot: !!document.querySelector('.cy-frame .pv-foot'),
    blockCount: document.querySelectorAll('.cy-frame').length,
    barOpacity: bar ? getComputedStyle(bar).opacity : null,
    barPos: bar ? getComputedStyle(bar).position : null,
    inline: frame?.getAttribute('style') ?? null,
    h: r ? Math.round(r.height) : null,
    cx: r ? Math.round(r.x + r.width / 2) : 0,
    cy: r ? Math.round(r.y + Math.min(r.height / 2, 40)) : 0,
  }
})
check('visual output: no fold row, no captions — just the frame', hr.hasFrame && hr.foldRow === false && hr.head === false && hr.foot === false,
  JSON.stringify({ frame: hr.hasFrame, fold: hr.foldRow, head: hr.head, foot: hr.foot }))
if (hr.inline == null) {
  const dbg = await page.eval(() => ({
    framesInline: [...document.querySelectorAll('.cy-frame .pv-frame')].map((f) => f.getAttribute('style')),
    msgs: (window.__msgs || []).slice(-6),
    count: (window.__msgs || []).length,
    frames: document.querySelectorAll('.cy-frame .pv-frame').length,
    srcdocHasProbe: (document.querySelector('.cy-frame .pv-frame')?.getAttribute('srcdoc') || '').includes('__htmlh'),
    srcdocLen: (document.querySelector('.cy-frame .pv-frame')?.getAttribute('srcdoc') || '').length,
  }))
  console.log('DIAG  html probe', JSON.stringify(dbg))
}
check('visual output: the frame fits its content (measured, not capped at 136)', measured != null && measured > 136, `inline height ${measured}px`)
check('visual output: the actions are hidden until the pointer arrives', hr.barOpacity === '0' && hr.barPos === 'absolute', `${hr.barOpacity} ${hr.barPos}`)

// 「悬停浮出」这一条不靠像素坐标判：真浏览器里派生鼠标移到某块上，只要流式贴底滚动
// 在两次调用之间动一下，坐标就落到别处（实测时红时绿）。这里改成两条都成立的判据：
//   ① 默认态 == 隐藏（上面已经量过 opacity 0 + absolute）；
//   ② 样式表里确实有一条「指针进入 / 探针报 hd-on → opacity 1」的规则。
// 产品两条路径都要有：CSS :hover 管父文档，hd-on 管指针落在 iframe 内（父文档收不到事件）。
// 直接验产品自己的那条路径：指针落进 iframe 内时父文档收不到事件，靠 iframe 探针上报
// __htmlhover → 组件给 .pv 挂 `.hd-on` → 工具条浮出。这里把那个类挂上去量计算值即可，
// 不依赖坐标（像素级 hover 会被流式贴底滚动打断，实测时红时绿）。
// 这一条只验样式表：「指针进来 → 工具条浮出」是两条规则（父文档 :hover / iframe 探针报
// hd-on）。真块上做会跟流式抢 DOM（两次 eval 之间整块被重建，标记元素直接消失），
// 所以在页面里搭一份同构夹具量计算值 —— 样式表是同一份，判据是确定的。
const bar = await page.eval(() => {
  const host = document.createElement('div')
  host.className = 'cy-frame'
  host.innerHTML = '<div class="pv"><div class="pv-bar"><button>预览</button><button>源码</button><button>在浏览器打开</button></div></div>'
  document.body.appendChild(host)
  const pv = host.querySelector('.pv')
  const el = host.querySelector('.pv-bar')
  // 关掉过渡再量：否则读到的是动画起始值（永远是 0）
  el.style.transition = 'none'
  const before = getComputedStyle(el).opacity
  pv.classList.add('hd-on')
  const on = getComputedStyle(el).opacity
  pv.classList.remove('hd-on')
  const after = getComputedStyle(el).opacity
  const buttons = [...el.querySelectorAll('button')].map((b) => b.textContent.trim())
  host.remove()
  return { before, on, after, buttons }
})
check('visual output: hover reveals the bar (hidden by default, hd-on raises it, back after)',
  bar?.before === '0' && bar?.on === '1' && bar?.after === '0', JSON.stringify(bar))
check('visual output: preview / source / open (no path here, so it says browser)',
  bar?.buttons[0] === '预览' && bar?.buttons[1] === '源码' && bar?.buttons[2] === '在浏览器打开', JSON.stringify(bar?.buttons))

const hs = await page.eval(async () => {
  const pick = (i) => document.querySelectorAll('.cy-frame .pv-bar button')[i]
  const until = async (fn, ms = 1500) => {
    const t0 = Date.now()
    while (Date.now() - t0 < ms) { if (fn()) return true; await new Promise((r) => setTimeout(r, 100)) }
    return false
  }
  let src = false
  for (let i = 0; i < 3 && !src; i++) {
    pick(1)?.click()
    src = await until(() => !!document.querySelector('.cy-frame .pv-code'))
  }
  let back = false
  for (let i = 0; i < 3 && !back; i++) {
    pick(0)?.click()
    back = await until(() => !!document.querySelector('.cy-frame .pv-frame'))
  }
  return { src, backToFrame: back }
})
check('visual output: 源码 shows the code, 预览 goes back', hs.src && hs.backToFrame, JSON.stringify(hs))

// ---- P3 · composer：`/` 技能、`@` 文件目录、多图扇形 + 灯箱 -------------------
await page.goto(URL, { settleMs: 3000 })
await waitStore()
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
// 原生预览视图恒在 DOM 之上（CSS 压不住），所以右栏有预览时，任何全屏覆盖层都会躲在它后面
// —— 用户 2026-10 报的「右栏 html 预览先开、聊天里的大图后开，大图被挡住」就是它。
// 这里证明判据本身成立：灯箱开着时，右栏矩形中点上最顶层的元素已经不在右栏里 → 该隐藏视图。
const cover = await page.eval(() => {
  const host = document.querySelector('.browser-shot')
  if (!host) return { skipped: true }
  const r = host.getBoundingClientRect()
  if (r.width < 2 || r.height < 2) return { skipped: true }
  const el = document.elementFromPoint(Math.round(r.left + r.width / 2), Math.round(r.top + r.height / 2))
  return { skipped: false, insideHost: !!el && host.contains(el), top: el ? el.className?.toString().split(' ')[0] : null }
})
if (!cover.skipped) {
  check('an open lightbox covers the right panel (native view must be hidden then)',
    cover.insideHost === false && /cy-lb|rt-lb/.test(cover.top || ''), JSON.stringify(cover))
}
await page.eval(() => document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true })))
await wait(300)
check('I11 esc closes the lightbox', !(await page.eval(() => !!document.querySelector('.cy-lb'))), 'still open')



// 运行时（AgentStart → AgentEnd）这一整段都在「Working」里：标题 + 四行窗口，
// 每行带度量（与工具行同一口径）。这是设计 图1 的形态。
await page.goto(URL, { settleMs: 3000 })
await waitStore()
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
  const body = l.querySelector('.rt-work-body')
  const t = document.querySelector('.rt-turn')
  return {
    present: true,
    head: l.querySelector('.cy-live-head')?.textContent ?? '',
    n: l.querySelector('.lv-n')?.textContent ?? '',
    act: l.querySelector('.lv-act')?.textContent ?? '',
    thinkingInside: body?.querySelectorAll('.thinking-toggle').length ?? 0,
    toolsInside: body?.querySelectorAll('.tool-row').length ?? 0,
    narrationInside: body?.querySelectorAll('.md').length ?? 0,
    cap: body ? getComputedStyle(body).maxHeight : null,
    capScrolls: body ? getComputedStyle(body).overflowY : null,
    outsideThinking: t?.querySelectorAll('.rt-turn-ans .thinking-toggle').length ?? 0,
    outsideTools: t?.querySelectorAll('.rt-turn-ans .tool-group').length ?? 0,
    outsideText: (t?.querySelector('.rt-turn-ans')?.textContent ?? '').trim(),
  }
})
check('running: the whole run lives in one Working block', lp.present && /Working/.test(lp.head), JSON.stringify({ present: lp.present, head: lp.head }))
check('running: the block counts calls', /\d+ calls?/.test(lp.n), lp.n)
check('running: thoughts and narration are inside it', lp.thinkingInside > 0 && lp.narrationInside > 0,
  JSON.stringify({ thinking: lp.thinkingInside, narration: lp.narrationInside }))
check('running: tool rows are inside it', lp.toolsInside > 0, lp.toolsInside)
check('running: the box is capped and scrolls',
  lp.cap && lp.cap !== 'none' && parseFloat(lp.cap) > 100 && lp.capScrolls === 'auto', `${lp.cap}/${lp.capScrolls}`)
check('running: nothing of the run is painted outside', lp.outsideThinking === 0 && lp.outsideTools === 0 && lp.outsideText === '',
  JSON.stringify({ th: lp.outsideThinking, tools: lp.outsideTools, text: lp.outsideText.slice(0, 30) }))

// ---- 密度：简洁档收起 = 只有结算行 + 结论；详细档 = 全过程铺开 -----------------
const freshTurn = async (density) => {
  await page.goto(URL, { settleMs: 3000 })
await waitStore()
  await page.eval((d) => {
    const st = window.__store.getState()
    st.setNarrativeSkin('runtime')
    st.setNarrativeDensity(d)
  }, density)
  await wait(700)
  await say('运行时样例')
  // 先等它真的开跑（否则「没有 live 面板」在开跑前也成立，会立刻通过），
  // 再等 AgentEnd：live 面板与流式光标消失、轮尾账出现（结算行的唯一来源）。
  await waitFor('.cy-live', 30000)
  await waitUntil("!document.querySelector('.cy-live') && !document.querySelector('.rt-caret')", 120000)
  await waitUntil("!!document.querySelector('.rt-turn-foot')", 30000)
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
  // 展开态是**同一个盒子**（.rt-workbox：头 = 折角行，内容 = .rt-work-body），
  // 高度上限在内容盒上；不再把工作行摊进流里
  const box = t?.querySelector('.rt-workbox .rt-work-body')
  if (!box) return { present: false, kids: [...(t?.children ?? [])].map((k) => k.className) }
  const cs = getComputedStyle(box)
  return { present: true, maxH: cs.maxHeight, overflow: cs.overflowY, capped: box.scrollHeight > box.clientHeight }
})
check('concise: expanding brings the work back — inside the box, not into the flow', c2.present, JSON.stringify(c2))
check('concise: the expanded work has a height cap', c2.present && c2.maxH !== 'none' && c2.overflow === 'auto', JSON.stringify(c2))

/* 盒子里的内容比盒子窄一层，盒子外只有最后一轮正文 —— 用户的三条要求，
   逐条钉住：① 运行中盒子外没有思考/工具；② 结算后工作行仍在盒子里（不摊进流）；
   ③ 内容缩进（宽度差）与框本身分得开。 */
const c3 = await page.eval(() => {
  const t = [...document.querySelectorAll('.rt-turn')].pop()
  const box = t?.querySelector('.rt-workbox')
  const body = box?.querySelector(':scope > .rt-work-body')
  const ans = t?.querySelector(':scope > .rt-turn-ans')
  if (!box || !body) return { present: false, boxKids: [...(t?.children ?? [])].map((k) => k.className) }
  const b = box.getBoundingClientRect()
  const row = body.querySelector('.rt-grp > *')
  const r = row ? row.getBoundingClientRect() : null
  return {
    present: true,
    boxW: Math.round(b.width),
    // 缩进量要从**行的左边缘**量：padding 是 body 自己的内边距（border-box 下
    // 它的 rect 仍等于盒宽），真正的「窄一层」发生在内容区
    indentPx: r ? Math.round(r.left - b.left) : null,
    rowW: r ? Math.round(r.width) : null,
    indent: getComputedStyle(body).paddingLeft,
    // 盒子里的是工作行；外面只该有最后一轮的正文（所以数的是**工作类**的行，
    // 不是所有行 —— 正文自己也是 .rt-grp 里的一个孩子）
    rowsInBox: body.querySelectorAll('.rt-grp > *').length,
    // 外面只该有「最后一轮的正文」这**一个**块（块内部可以自带嵌套行，
    // 例如末轮的 codemode 结果，那不是被摊出来的工作行）
    // 盒外可以有：末轮正文、产出卡、系统信号（这三类各自成 bare 段，属设计里的流内块）；
    // 不许有的是**工作行**（工具/思考/运行面板）—— 那才是「没收敛在 working 内部」。
    workRowsOutside: ans ? ans.querySelectorAll('.tool-row, .cy-live, .rt-work-body').length : 0,
    ansBlocks: ans ? ans.querySelectorAll(':scope > .rt-grp > *').length : 0,
    ansText: (ans?.textContent || '').slice(0, 40),
  }
})
check('concise: work rows stay inside the box after the run (not spilled into the flow)', c3.present && c3.rowsInBox > 0 && c3.workRowsOutside === 0, JSON.stringify(c3))
check('concise: inside the box is narrower than the box (the indent carries the nesting)', c3.present && c3.indent === '14px' && c3.indentPx === 14 && c3.rowW < c3.boxW, JSON.stringify({ boxW: c3.boxW, rowW: c3.rowW, indentPx: c3.indentPx }))

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

// ---- 轮尾只有一条账：calls · tokens · cache · s turn -------------------------
// 设计里一轮结束时只有一条账；「本轮 TTFT · LLM 总耗时 · Agent 总耗时 · tok/s」那行
// 是另两档皮肤的写法，两个并排等于把同一份事实报两遍 —— 运行时皮肤下它不该出现。
const foot = await page.eval(() => {
  const led = document.querySelector('.rt-ledger')
  const meta = document.querySelectorAll('.assistant-turn-meta').length
  const arts = {
    modern: document.querySelectorAll('.msg.artifacts .ar').length,
    legacy: document.querySelectorAll('.art').length,
    taskLegacy: document.querySelectorAll('.ac-artifacts .art, .ac-artifacts .art-rest').length,
  }
  const txt = led?.textContent?.replace(/\s+/g, ' ').trim() ?? null
  return { ledger: !!led, txt, meta, arts }
})
check('turn: the cost line exists and is the ledger', foot.ledger === true && /calls/.test(foot.txt || ''), foot.txt)
check('turn: the legacy TTFT line is gone (one fact, one line)', foot.meta === 0, foot.meta)
check('artifacts: the runtime card is the design one (.ar), not the old .art', foot.arts.modern > 0 && foot.arts.legacy === 0, JSON.stringify(foot.arts))
// 任务卡片里的产出也必须同一套（子会话＝主会话，只是 session 不同）：任何一处
// .ac-artifacts 里都不该再出现旧卡。没有任务卡时这条自然成立，有就必须是 .ar。
check('artifacts: the task card uses the same card', foot.arts.taskLegacy === 0, foot.arts.taskLegacy)

// ---- 上下文环：点它是「打开构成面板」，不是「压缩」 ------------------------
// 设计 index.html:1028 里 .ctx-ring 打开的是 .pop[data-pop="ctx"]（容量条 + 分区行 +
// 缓存命中率）；压缩不在这一下上。我们原本把压缩挂在环的点击上，等于点「详情」弹「确认」。
// 运行时皮肤改成设计那样，压缩入口移到面板尾部（否则这一档就没有入口了）。
const ctxClick = await page.eval(async () => {
  const DIALOG = '.modal-hint[role="dialog"]'
  document.querySelector(DIALOG)?.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
  const ring = document.querySelector('.row2 .ctx')
  if (!ring) return { present: false }
  ring.click()
  await new Promise((r) => setTimeout(r, 700))
  const panel = document.querySelector('.ctx-panel')
  const compactBtn = document.querySelector('.ctx-compact')
  const out = {
    present: true,
    panel: !!panel,
    dialogAfterRing: !!document.querySelector('.modal-hint[role="dialog"]'),
    compactLabel: compactBtn?.textContent?.trim() ?? null,
  }
  if (compactBtn) {
    compactBtn.click()
    await new Promise((r) => setTimeout(r, 500))
    out.dialogAfterPanelBtn = !!document.querySelector('.modal-hint[role="dialog"]')
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
    await new Promise((r) => setTimeout(r, 300))
  }
  // 收尾：把面板关掉（再点一次环）
  document.querySelector('.row2 .ctx')?.click()
  await new Promise((r) => setTimeout(r, 300))
  return out
})
check('ctx: clicking the ring opens the breakdown', ctxClick.panel === true && ctxClick.dialogAfterRing === false, JSON.stringify(ctxClick))
check('ctx: compaction still has an entry, in the panel', ctxClick.compactLabel === '压缩上下文…' && ctxClick.dialogAfterPanelBtn === true, JSON.stringify(ctxClick))

// ---- 拖拽：盒子变成一个可以往里放东西的地方 -------------------------------
// 设计 .p-drop（ui.components.css:2492）：虚线强调边、28px 图标块、12.5px 标题、
// 11px mono 说明，落在整张输入卡上（inset -1px）。这里真的发一个带文件的
// dragenter —— 走的是组件自己的 hasFileDrag/onDragEnter，不是给类名打补丁。
const drop = await page.eval(async () => {
  const box = document.querySelector('.composer')
  if (!box) return { open: false, why: 'no composer' }
  const dt = new DataTransfer()
  dt.items.add(new File([new Uint8Array([137, 80, 78, 71])], 'shot.png', { type: 'image/png' }))
  box.dispatchEvent(new DragEvent('dragenter', { bubbles: true, cancelable: true, dataTransfer: dt }))
  await new Promise((r) => setTimeout(r, 300))
  const d = document.querySelector('.composer .p-drop')
  if (!d) return { open: false, why: 'overlay missing' }
  const b = box.getBoundingClientRect()
  const r = d.getBoundingClientRect()
  const out = {
    open: true,
    border: getComputedStyle(d).borderStyle,
    inset: getComputedStyle(d).top + '/' + getComputedStyle(d).left,
    delta: [Math.round(r.width - b.width), Math.round(r.height - b.height)],
    covers: Math.abs(r.width - b.width) <= 2 && Math.abs(r.height - b.height) <= 2
      && Math.abs(r.x - b.x) <= 2 && Math.abs(r.y - b.y) <= 2,
    icon: Math.round(d.querySelector('.pd-ic')?.getBoundingClientRect().width ?? 0),
    title: getComputedStyle(d.querySelector('b')).fontSize,
    what: getComputedStyle(d.querySelector('.pd-what')).fontSize,
    text: (d.textContent || '').trim().slice(0, 40),
  }
  box.dispatchEvent(new DragEvent('dragleave', { bubbles: true, cancelable: true, dataTransfer: dt }))
  await new Promise((r2) => setTimeout(r2, 250))
  out.closed = !document.querySelector('.composer .p-drop')
  return out
})
check('drop: the card shows a drop target', drop.open === true && drop.border === 'dashed', JSON.stringify(drop))
check('drop: the overlay is the design inset (-1px) and sits on the card', drop.inset === '-1px/-1px' && drop.covers === true, `${drop.inset} ${JSON.stringify(drop.delta)}`)
check('drop: 28px icon block, 12.5px title, 11px mono detail', drop.icon === 28 && drop.title === '12.5px' && drop.what === '11px', `${drop.icon} ${drop.title} ${drop.what}`)
check('drop: leaving puts the card back', drop.closed === true, drop.closed)

// ---- 队列条：逐条编辑 / 保存 ------------------------------------------------
// 设计要求队列行可逐条处置（.pq-act）；我们的暂存队列同一条规矩：点哪一条只改
// 那一条，保存后其他条原样，Esc 放弃。整段放在**一次** page eval 里跑：队列会在
// 下一个 llm_end 被 flushPending 整队推走（mock 一直在推事件），跨多次 eval 的
// 等待会让刚种进去的两条在中途被消费掉 —— 那测的就不是渲染，是竞态。
const qq = await page.eval(async () => {
  const store = window.__store
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
  const pick = () => {
    const st = store.getState()
    return st.activeSessionId
      ?? Object.keys(st.views).find((k) => (st.views[k]?.blocks?.length ?? 0) > 0)
      ?? Object.keys(st.views)[0]
  }
  let sid = pick()
  if (!sid) {
    await store.getState().newSession?.()
    await sleep(1500)
    sid = pick()
  }
  if (!sid) throw new Error('no session to seed the queue into')

  const mk = (id, text) => ({ id, contents: [{ type: 'text', content: text }], pushed: false })
  const seed = () => [mk(900001, 'alpha 这一条'), mk(900002, 'beta 这一条')]
  // Composer 读的是**顶层** s.pendingQueue（setView 会镜像一份），所以两处都写：
  // 只写 views[sid] 的话 store 里有、屏幕上没有 —— 上一版就是这样假绿的。
  const setQ = (q) => store.setState((s) => ({
    activeSessionId: sid,
    pendingQueue: q,
    views: { ...s.views, [sid]: { ...s.views[sid], pendingQueue: q } },
  }))
  const rows = () => [...document.querySelectorAll('.pending-queue .pq-item')]
  const texts = () => [...document.querySelectorAll('.pending-queue .pq-text')].map((e) => e.textContent.trim())
  const storeTexts = () => (store.getState().pendingQueue ?? []).map((x) => x.contents.map((c) => c.content).join(''))
  const setVal = (el, v) => {
    Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set.call(el, v)
    el.dispatchEvent(new Event('input', { bubbles: true }))
  }
  const key = (el, k) => el.dispatchEvent(new KeyboardEvent('keydown', { key: k, bubbles: true, cancelable: true }))
  // 队列会在下一个 llm_end 被 flushPending 整队推走（mock 一直在推事件）。所以每打开
  // 一条都先确认队列还在：没了就重新种，再点。这不是掩盖问题——它测的是「点这条只改
  // 这条」，不是「mock 什么时候收走队列」。
  const openEdit = async (i) => {
    for (let k = 0; k < 4; k++) {
      if (rows().length < 2) { setQ(seed()); await sleep(150) }
      rows()[i]?.querySelector('.pq-text')?.click()
      await sleep(150)
      const t = document.querySelector('.pending-queue .pq-item:nth-child(' + (i + 1) + ') textarea')
      if (t) return t
    }
    return null
  }
  const out = {}

  setQ(seed())
  await sleep(150)
  out.seeded = { n: rows().length, texts: texts() }

  // 点第二条正文 = 只编辑第二条
  const ta = await openEdit(1)
  out.opened = {
    value: ta?.value ?? null,
    editing: document.querySelectorAll('.pending-queue .pq-item.editing').length,
    focused: document.activeElement === ta,
    save: document.querySelector('.pending-queue .pq-btn.primary')?.textContent?.trim() ?? null,
  }

  // 改文本 → Enter 保存
  if (ta) {
    setVal(ta, 'beta 改过了')
    await sleep(60)
    key(ta, 'Enter')
    await sleep(200)
  }
  out.saved = { texts: texts(), store: storeTexts(), editing: document.querySelectorAll('.pending-queue .pq-item.editing').length }

  // Esc 放弃：改到一半按 Esc，内容与 store 都不动
  const ta2 = await openEdit(0)
  if (ta2) {
    setVal(ta2, 'alpha 改了但不要')
    await sleep(60)
    key(ta2, 'Escape')
    await sleep(200)
  }
  out.escaped = { texts: texts(), store: storeTexts(), editing: document.querySelectorAll('.pending-queue .pq-item.editing').length }

  // 收尾：清空，别让后面的断言看见队列条
  setQ([])
  await sleep(120)
  out.cleared = rows().length
  return out
})
check('queue: two rows seeded', qq.seeded.n === 2 && qq.seeded.texts[0] === 'alpha 这一条' && qq.seeded.texts[1] === 'beta 这一条', JSON.stringify(qq.seeded))
check('queue: clicking a row edits that row only', qq.opened.value === 'beta 这一条' && qq.opened.editing === 1, JSON.stringify(qq.opened))
check('queue: the edit box takes focus', qq.opened.focused === true, qq.opened.focused)
check('queue: the primary button says save', qq.opened.save === '保存', qq.opened.save)
check('queue: Enter saves that row', qq.saved.texts[1] === 'beta 改过了' && qq.saved.editing === 0, JSON.stringify(qq.saved.texts))
check('queue: the other row is untouched', qq.saved.texts[0] === 'alpha 这一条', qq.saved.texts[0])
check('queue: the save is in the store, not just on screen', qq.saved.store[1] === 'beta 改过了' && qq.saved.store[0] === 'alpha 这一条', JSON.stringify(qq.saved.store))
check('queue: Esc drops the edit', qq.escaped.editing === 0 && qq.escaped.texts[0] === 'alpha 这一条' && qq.escaped.store[0] === 'alpha 这一条', JSON.stringify(qq.escaped))
check('queue: seeded rows are cleaned up', qq.cleared === 0, qq.cleared)

// ---- 能力入口（设计 .caps-btn）+ 附件 chip（设计 .att）---------------------
// 「能力」= 设计 index.html:916 的 .pop.caps-pop：本轮可用 <N 项> + 按类折叠。
// 判据：按钮上的数 = 弹层里的「N 项」（同源 capsCountOf），技能展开有行，
// 点技能行把 `/名字 ` 写进输入框并关掉弹层 —— 而且**不能**顺手把 `/` 菜单顶出来。
const caps = await page.eval(() => {
  const btn = document.querySelector('.row2 .caps-btn')
  if (!btn) return null
  const out = {
    n: Number(btn.querySelector('b')?.textContent),
    label: btn.querySelector('.lbl')?.textContent ?? null,
    h: Math.round(btn.getBoundingClientRect().height * 10) / 10,
    aria: btn.getAttribute('aria-expanded'),
  }
  btn.click()
  return out
})
await wait(600)
const capsPop = await page.eval(() => {
  const pop = document.querySelector('.caps-pop')
  if (!pop) return { open: false }
  return {
    open: true,
    head: (pop.querySelector('.caps-head')?.textContent ?? '').replace(/\s+/g, ' ').trim(),
    total: Number((pop.querySelector('.caps-head b')?.textContent ?? '').replace(/\D/g, '')),
    w: Math.round(pop.getBoundingClientRect().width),
    groups: [...pop.querySelectorAll('.acc-row')].map((r) => (r.textContent ?? '').replace(/\s+/g, ' ').trim()),
    skillRows: pop.querySelectorAll('.acc-body .pop-row.two').length,
    cmdMenu: !!document.querySelector('.pop[data-pop="cmd"]'),
  }
})
check('caps: shown in the tool row at 26px', caps?.h === 26, caps?.h)
check('caps: labelled 能力', caps?.label === '能力', caps?.label)
check('caps: the button number is the popover total (one source)', caps && caps.n === capsPop.total, `${caps?.n} vs ${capsPop.total}`)
check('caps: clicking it opens the capability list, not the slash menu', capsPop.open === true && capsPop.cmdMenu === false, JSON.stringify({ open: capsPop.open, cmd: capsPop.cmdMenu }))
check('caps: it says how many are available this turn', /本轮可用/.test(capsPop.head || '') && capsPop.total > 0, capsPop.head)
check('caps: groups are counted rows', (capsPop.groups || []).length >= 2, JSON.stringify(capsPop.groups))
check('caps: the skills group lists real skills', capsPop.skillRows > 0, capsPop.skillRows)

// 点技能行 = 把 `/名字 ` 写进输入框（设计：只有技能行会插入文本）
const capsPick = await page.eval(async () => {
  const row = document.querySelector('.caps-pop .acc-body .pop-row.two')
  const name = row?.querySelector('.k')?.textContent?.trim() ?? null
  row?.click()
  await new Promise((r) => setTimeout(r, 500))
  const ta = document.querySelector('.composer textarea') || document.querySelector('textarea')
  const out = { name, value: ta?.value ?? null, popGone: !document.querySelector('.caps-pop'), cmd: !!document.querySelector('.pop[data-pop="cmd"]') }
  // 收尾：清掉写进正文的内容
  const set = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set
  set.call(ta, '')
  ta.dispatchEvent(new Event('input', { bubbles: true }))
  return out
})
check('caps: a skill row inserts its command and closes', capsPick.name != null && capsPick.value === `/${capsPick.name} ` && capsPick.popGone === true, JSON.stringify(capsPick))
check('caps: inserting a skill does not open the slash menu', capsPick.cmd === false, capsPick.cmd)
// 收尾：Esc 关菜单、清掉写进正文的 `/`
await page.eval(() => {
  const ta = document.querySelector('textarea')
  ta.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }))
  Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set.call(ta, '')
  ta.dispatchEvent(new Event('input', { bubbles: true }))
})
await wait(200)

// 附件 chip 的度量：docRefs 是选文件的组件态（浏览器里拿不到 Electron 的 file.path），
// 这里量的是**同一组规则**落在真 DOM 上的计算值 —— 断言的仍是设计给的那几个字面值。
const chip = await page.eval(async () => {
  const host = document.querySelector('.composer') || document.body
  const strip = document.createElement('div')
  strip.className = 'attach-strip'
  const c = document.createElement('div')
  c.className = 'attach-chip doc-chip'
  c.innerHTML = '<svg class="att-i" viewBox="0 0 16 16"></svg><span class="doc-chip-name">merge_test.go</span>'
    + '<span class="doc-chip-size">4.1 KB</span><button class="attach-x">×</button>'
  strip.appendChild(c)
  host.appendChild(strip)
  // 入场动画 from{scale:.94}：不等它跑完，量到的是 22.56 而不是 24
  const anim = c.getAnimations?.()[0]
  if (anim) await anim.finished.catch(() => {})
  const cs = getComputedStyle(c)
  const x = c.querySelector('.attach-x')
  const xr = x.getBoundingClientRect()
  const out = {
    h: c.getBoundingClientRect().height,
    font: cs.fontSize,
    mono: cs.fontFamily.toLowerCase().includes('mono'),
    pad: cs.padding,
    radius: cs.borderTopLeftRadius,
    xPos: getComputedStyle(x).position,
    xSize: [Math.round(xr.width), Math.round(xr.height)],
    sizeFont: getComputedStyle(c.querySelector('.doc-chip-size')).fontSize,
    icon: getComputedStyle(c.querySelector('.att-i')).width,
    anim: cs.animationName,
  }
  strip.remove()
  return out
})
check('chip: 24px tall', chip.h === 24, chip.h)
check('chip: it animates in (design att-in)', chip.anim === 'rt-att-in', chip.anim)
check('chip: 11px mono text', chip.font === '11px' && chip.mono, `${chip.font} mono=${chip.mono}`)
check('chip: padding matches the design (0 4px 0 7px)', chip.pad === '0px 4px 0px 7px', chip.pad)
check('chip: file icon is 11px', chip.icon === '11px', chip.icon)
check('chip: the size reads as 10px', chip.sizeFont === '10px', chip.sizeFont)
check('chip: the × is an inline 16px square, not an overlay', chip.xPos === 'static' && chip.xSize[0] === 16 && chip.xSize[1] === 16, JSON.stringify(chip.xSize))

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
