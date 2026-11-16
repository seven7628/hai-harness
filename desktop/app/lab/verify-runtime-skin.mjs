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
// 皮肤是静态的（index.html 的 <html data-skin="runtime">），没有运行时开关可走 ——
// 这里只调密度（简洁/详细），那是仍在设置里的开关。
await page.eval(() => {
  const store = window.__store
  if (!store) throw new Error('lab hook missing: window.__store（main.tsx 的 dev 钩子）')
  const st = store.getState()
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

// 光晕要**看得出来**（用户 2026-10：外围光晕不够明显、闪烁看不太出来）。
// 判据不是「有动画」（上面已经验过），而是动画过程中真能达到的扩散/亮度：
// 采样若干帧，取 box-shadow 里最大的扩散半径与最高不透明度。
const glow = await page.eval(async () => {
  const el = document.querySelector('.composer[data-live]')
  if (!el) return { present: false }
  let maxSpread = 0
  let maxAlpha = 0
  const nums = (s) => (s.match(/-?\d*\.?\d+px/g) || []).map((x) => Math.abs(parseFloat(x)))
  // 颜色的序列化形式随令牌而定：rgba(...) / color(srgb … / a) / oklab(… / a)。
  // 统一按「最后一个 / 之后是 alpha」取，rgba 的第 4 个参数另算。
  const alphas = (str) => {
    const out = []
    for (const m of str.matchAll(/(?:rgba?|oklab|oklch|lab|lch|color)\(([^)]*)\)/g)) {
      const body = m[1]
      const slash = body.split('/')
      if (slash.length > 1) {
        const a = parseFloat(slash[1])
        if (!Number.isNaN(a)) out.push(a)
      } else {
        const parts = body.split(',').map((x) => x.trim())
        if (parts.length > 3) {
          const a = parseFloat(parts[3])
          if (!Number.isNaN(a)) out.push(a)
        } else out.push(1)
      }
    }
    return out
  }
  // 有效亮度 = 阴影颜色的 alpha × 该层自己的 opacity（::before 的 0→1 是另一段动画）。
  // 采样 34 帧 × 70ms ≈ 一个完整周期，避开「刚好采在 0 上」。
  for (let i = 0; i < 34; i++) {
    const layers = [
      { shadow: getComputedStyle(el).boxShadow, opacity: 1 },
      { shadow: getComputedStyle(el, '::before').boxShadow, opacity: parseFloat(getComputedStyle(el, '::before').opacity) || 0 },
      { shadow: getComputedStyle(el, '::after').boxShadow, opacity: parseFloat(getComputedStyle(el, '::after').opacity) || 0 },
    ]
    for (const layer of layers) {
      for (const v of nums(layer.shadow)) maxSpread = Math.max(maxSpread, v)
      for (const a of alphas(layer.shadow)) maxAlpha = Math.max(maxAlpha, a * layer.opacity)
    }
    await new Promise((r) => setTimeout(r, 70))
  }
  return { present: true, maxSpread, maxAlpha, sample: getComputedStyle(el).boxShadow.slice(0, 160), ring: getComputedStyle(el, '::before').boxShadow.slice(0, 120) }
})
check('live: the glow actually reaches out (max spread ≥ 8px)', glow.present === true && glow.maxSpread >= 8, JSON.stringify(glow))
check('live: and it is visible, not a whisper (effective alpha ≥ 0.45)', glow.present === true && glow.maxAlpha >= 0.45, JSON.stringify(glow))

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
// 默认态由规格定（结算后自动收起）；这里只验「点一下会翻转、再点回来」
check('batch: clicking toggles the rows', batch1.closed?.aria !== batch1.aria, JSON.stringify({ before: batch1.aria, after: batch1.closed?.aria }))
check('batch: clicking again flips it back', batch1.reopened === batch1.aria, `${batch1.aria} → ${batch1.reopened}`)

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
// 规格（2026-10）：工具组**全部结束后自动收缩**。所以这里的默认态是收起的
// ——「什么都没藏在一次点击后面」不再成立，取而代之的是「收起的行仍然说清做了什么」。
check('batch: a settled group folds itself away', bt.aria === 'false' && bt.rowsOpen > 0, JSON.stringify({ aria: bt.aria, rows: bt.rowsOpen }))
check('batch: clicking brings the rows back', bt.collapsed?.aria === 'true' && bt.collapsed?.visible === true, JSON.stringify(bt.collapsed))
check('batch: clicking again folds them away', bt.reopened === 'false', bt.reopened)

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

// ---- codemode：Search / Execute 分型 + 批量进度 --------------------------------
// 用户 2026-10 规格：Search 与 Execute 必须一眼可辨（标签+信息重点，不只靠颜色）；
// 批量执行要有真实进度，**没有分母就不许画百分比**（协议只在 tool_run_end 给 nested_calls）。
await page.goto(URL, { settleMs: 3000 })
await waitStore()
await page.eval(() => {
  const st = window.__store.getState()
  st.setNarrativeDensity('concise')
})
await wait(500)
await say('codemode 脚本编排')
await waitFor('.cy-live', 30000)
await waitUntil("!document.querySelector('.cy-live') && !!document.querySelector('.rt-turn-foot')", 120000)
// 工作盒子结算后是收起的：展开才看得到那三行
await page.eval(() => [...document.querySelectorAll('.rt-turn')].filter((x) => x.querySelector('.rt-act[aria-expanded]')).pop()?.querySelector('.rt-act[aria-expanded]')?.click())
await wait(900)
const cm = await page.eval(() => {
  const rows = [...document.querySelectorAll('.tool-row')].filter((r) => r.querySelector('.cm-mode'))
  return rows.map((r) => ({
    mode: r.querySelector('.cm-mode')?.textContent?.replace(/\s+/g, ' ').trim() ?? null,
    mark: r.querySelector('.cm-mode .cm-mode-mark')?.textContent?.trim() ?? null,
    bp: r.querySelector('.cm-bp-line')?.textContent?.replace(/\s+/g, ' ').trim() ?? null,
    aria: r.querySelector('.cm-bp-bar')?.getAttribute('aria-valuenow') ?? null,
    width: r.querySelector('.cm-bp-fill')?.getAttribute('style') ?? null,
    failed: !!r.querySelector('.cm-bp-fail'),
    nested: r.querySelector('.tn-hd')?.textContent?.replace(/\s+/g, ' ').trim() ?? null,
    state: r.getAttribute('data-s') ?? null,
    text: (r.textContent || '').replace(/\s+/g, ' ').slice(0, 160),
  }))
})
check('codemode: the demo renders three calls', cm.length === 3, JSON.stringify(cm.map((r) => r.mode)))
check('codemode: search and execute are told apart by a word and a mark, not just colour',
  /搜索|检索|Search/i.test(cm[0]?.mode ?? '') && /执行|Execute/i.test(cm[1]?.mode ?? '') && cm[0]?.mark !== cm[1]?.mark,
  JSON.stringify(cm.map((r) => `${r.mark}${r.mode}`)))
check('codemode: the pure-search call carries no batch progress (nothing was executed)',
  cm[0]?.bp == null, cm[0]?.bp)
check('codemode: the execute call reports real counts from nested_calls',
  /Execute/i.test(cm[1]?.bp ?? '') && /2\/2 completed/.test(cm[1]?.bp ?? ''), cm[1]?.bp)
check('codemode: that progress bar is a real percentage, not a guess',
  cm[1]?.aria === '100' && /100%/.test(cm[1]?.width ?? ''), `${cm[1]?.aria} ${cm[1]?.width}`)
check('codemode: the failed call shows its failure while collapsed',
  cm[2]?.state === 'fail' && /boom|script error|失败/i.test(cm[2]?.text ?? ''), JSON.stringify({ state: cm[2]?.state, text: (cm[2]?.text ?? '').slice(0, 60) }))
check('codemode: a finished call with no sub-calls shows no fake progress bar',
  cm[0]?.bp == null && cm[2]?.bp == null, JSON.stringify([cm[0]?.bp, cm[2]?.bp]))

// ---- 行头六列：徽标不许抢列（列宽挤压 / 折成两行） ------------------------------
// 用户 2026-10 报障的根源：行头是设计给的六列网格，档位徽标若作为第 7 个网格子项，
// 网格会把它塞进 1fr 那格（药丸被拉宽到 260px），并把对象/度量/时长挤到第二行。
const cmGeom = await page.eval(() => {
  const rows = [...document.querySelectorAll('.tool-row')].filter((r) => r.querySelector('.cm-mode'))
  return rows.map((r) => {
    const hd = r.querySelector('.tool-row-hd')
    const hr = hd.getBoundingClientRect()
    const kids = [...hd.children].filter((c) => getComputedStyle(c).display !== 'none')
    const onRow = kids.filter((k) => {
      const b = k.getBoundingClientRect()
      return b.top < hr.bottom - 4 && b.bottom > hr.top + 4
    })
    return {
      hdH: Math.round(hr.height),
      kids: kids.length,
      onRow: onRow.length,
      pill: Math.round(r.querySelector('.cm-mode').getBoundingClientRect().width),
      argsW: Math.round(r.querySelector('.args')?.getBoundingClientRect().width ?? 0),
      durW: Math.round(r.querySelector('.dur')?.getBoundingClientRect().width ?? 0),
    }
  })
})
check('codemode: the tier pill is a flex child, not a grid track it can stretch',
  cmGeom.length > 0 && cmGeom.every((g) => g.pill < 90), JSON.stringify(cmGeom.map((g) => g.pill)))
check('codemode: the header stays ONE line (nothing wraps to a second grid row)',
  cmGeom.every((g) => g.hdH <= 40 && g.onRow === g.kids), JSON.stringify(cmGeom))
check('codemode: the object text keeps the flexible column, so the numbers keep their own',
  cmGeom.every((g) => g.argsW > 100 && g.durW > 20), JSON.stringify(cmGeom.map((g) => [g.argsW, g.durW])))

// ---- 失败行的呈现：数字不穿判决色；没有内容的原因行干脆不画 ---------------------
// 用户 2026-10 截图：一个 29 ms 的失败 bash 让「0.0 KB」「29 ms」都变红，还挂着一行
// `✗ .`，看着像上一次的失败状态没消。
const injected = await page.eval(() => {
  const st = window.__store.getState()
  const sid = st.activeSessionId
  const v = st.views[sid]
  window.__labBlocks = v.blocks
  const extra = [{
    kind: 'tool', id: 'lab-fail-1', toolId: 'lab-fail-1', name: 'bash', status: 'done',
    isError: true, durMs: 29, startedAt: Date.now(),
    args: JSON.stringify({ command: 'cd /Users/admin/Desktop/agent-server-g && ls' }),
    // 真实形态：分隔线 + 引擎退出码标记（首行**没有**信息量）
    result: '---\nls: cannot access: No such file or directory\n[exit: exit status 1]',
  }]
  const blocks = [...v.blocks, ...extra]
  window.__store.setState({ views: { ...st.views, [sid]: { ...v, blocks } }, blocks })
  return blocks.length
})
await wait(700)
// 简洁档交卷后的工作盒是**收起**的：折叠内容不挂 DOM（不是 display:none），所以注入
// 这一行之后先把它所在的那一轮展开，否则 .tool-row 根本不存在。展开所有收起的折叠行，
// 不假设轮数。
await page.eval(() => {
  document.querySelectorAll('.rt-act[aria-expanded="false"]').forEach((el) => el.click())
})
await wait(600)
const failRow = await page.eval(() => {
  const row = [...document.querySelectorAll('.tool-row')].find((r) => r.textContent.includes('agent-server-g') && r.getAttribute('data-s') === 'fail')
  if (!row) return null
  // 计算值可能是 oklab(...)（color-mix 的产物）—— 交给 canvas 归一成 rgb 再判红，
  // 否则「是不是红色」这件事根本无从比较（正则读不出 oklab 的分量）。
  // 直接画一个像素再读回来：fillStyle 的字符串往返对不同颜色空间不可靠（oklab 会原样回显），
  // 而 getImageData 拿到的一定是 sRGB 分量。
  const cv = document.createElement('canvas')
  cv.width = 1
  cv.height = 1
  const ctx = cv.getContext('2d')
  const rgb = (c) => {
    if (!c) return null
    ctx.clearRect(0, 0, 1, 1)
    ctx.fillStyle = 'rgb(0, 0, 0)'
    ctx.fillStyle = c
    ctx.fillRect(0, 0, 1, 1)
    const d = ctx.getImageData(0, 0, 1, 1).data
    return `rgb(${d[0]}, ${d[1]}, ${d[2]})`
  }
  const cs = (sel) => {
    const el = row.querySelector(sel)
    return el ? rgb(getComputedStyle(el).color) : null
  }
  return {
    metric: cs('.m'),
    dur: cs('.dur'),
    verb: cs('.t'),
    err: row.querySelector('.tool-err')?.textContent?.trim() ?? null,
    progress: !!row.querySelector('.cm-bp-bar'),
    text: (row.textContent || '').replace(/\s+/g, ' ').slice(0, 80),
  }
})
// 红 = 明显偏红的那一族（截图里是 #e4746a / #c0392b 这一档）
const reddish = (c) => {
  const hex = /^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i.exec(c ?? '')
  const rgbm = /rgba?\((\d+), (\d+), (\d+)/.exec(c ?? '')
  const [r, g, b] = hex ? [parseInt(hex[1], 16), parseInt(hex[2], 16), parseInt(hex[3], 16)]
    : rgbm ? [+rgbm[1], +rgbm[2], +rgbm[3]] : [0, 0, 0]
  return r > 120 && r - g > 40 && r - b > 40
}
check('failure: the numbers stay neutral (a 29 ms call is not "everything is red")',
  failRow && !reddish(failRow.metric) && !reddish(failRow.dur),
  JSON.stringify({ metric: failRow?.metric, dur: failRow?.dur }))
check('failure: the verdict still lands on the verb',
  failRow && reddish(failRow.verb), JSON.stringify({ verb: failRow?.verb }))
check('failure: the reason line says something, or is not drawn at all',
  failRow && (failRow.err === null || /[\p{L}\p{N}]{2,}/u.test(failRow.err)), JSON.stringify({ err: failRow?.err, text: failRow?.text }))
check('failure: a plain bash row never paints a codemode progress bar',
  failRow && failRow.progress === false, JSON.stringify({ progress: failRow?.progress }))
await page.eval(() => {
  const st = window.__store.getState()
  const sid = st.activeSessionId
  window.__store.setState({ views: { ...st.views, [sid]: { ...st.views[sid], blocks: window.__labBlocks } }, blocks: window.__labBlocks })
})
await wait(500)

// ---- wheel 冒泡：内层滚动容器吃掉的滚动不算「翻这个对话」 ----------------------
// 用户 2026-10：自动折叠后可滚动的小容器变多，在工具输出里往回翻两行就会把整个对话
// 判成「用户离开底部」，按钮出现后又被塌缩的高度变化搅乱 —— 看起来就是按钮坏了。
const nestedWheel = await page.eval(async () => {
  const nar = document.querySelector('.narrative')
  if (!nar) return { skipped: true }
  // 这条用例的前提是「叙述区自己可滚」：贴底、再往回滚才谈得上离开底部。
  // 收紧后的折叠态在 1440×1200 下可能整段放得下（实测 scrollHeight == clientHeight），
  // 那样 onWheel 的 stopFollow 没有可滚的余量、按钮永远不会出现 —— 断言测的就不是
  // wheel 归属了。先垫一段占位内容把滚动条造出来，结束时移除。
  const spacer = document.createElement('div')
  spacer.setAttribute('data-lab-spacer', '1')
  spacer.style.cssText = 'height:1200px'
  nar.appendChild(spacer)
  await new Promise((r) => setTimeout(r, 120))
  nar.scrollTop = nar.scrollHeight // 先贴底（按钮不该在）
  const box = document.createElement('div')
  box.setAttribute('data-lab-inner', '1')
  box.style.cssText = 'height:80px;overflow-y:auto'
  box.innerHTML = '<div style="height:600px">inner</div>'
  nar.appendChild(box)
  const t0 = Date.now()
  await new Promise((r) => setTimeout(r, 260))
  box.scrollTop = 100 // 内层还能往回滚 → 这次 wheel 该被内层消费
  box.dispatchEvent(new WheelEvent('wheel', { deltaY: -120, bubbles: true }))
  await new Promise((r) => setTimeout(r, 400))
  const withInner = !!document.querySelector('.narrative-jump')
  box.scrollTop = 0 // 内层到顶了 → 再往回滚就是用户在翻对话
  box.dispatchEvent(new WheelEvent('wheel', { deltaY: -120, bubbles: true }))
  await new Promise((r) => setTimeout(r, 400))
  const atTop = !!document.querySelector('.narrative-jump')
  box.remove()
  // 复位：滚回底部并点掉按钮（如果出现了）
  document.querySelector('.narrative-jump')?.click()
  await new Promise((r) => setTimeout(r, 300))
  nar.scrollTop = nar.scrollHeight
  spacer.remove()
  return { withInner, atTop, ms: Date.now() - t0 }
})
check('scroll: a wheel inside a nested scroller does not summon "jump to latest"',
  nestedWheel.skipped || nestedWheel.withInner === false, JSON.stringify(nestedWheel))
check('scroll: once the nested scroller is at its end, the wheel belongs to the transcript',
  nestedWheel.skipped || nestedWheel.atTop === true, JSON.stringify(nestedWheel))

// ---- 数字的身份色：+N 绿 / −N 红 / 计时一支色 ---------------------------------
// 用户 2026-10：文件变更要红绿对应（与工具行一致），所有计时信息要有一个与主题搭配的颜色。
// 用页面里的同构夹具量计算值：颜色令牌与 --md-measure 那样由浏览器自己换算成 rgb。
const tone = await page.eval(() => {
  const host = document.createElement('div')
  host.innerHTML = '<span class="rt-add">+96</span><span class="rt-del">−3</span><span class="rt-time">17 ms</span>'
  const probe = document.createElement('i')
  probe.style.color = 'var(--tone-add)'
  document.body.appendChild(probe)
  const add = getComputedStyle(probe).color
  probe.style.color = 'var(--tone-del)'
  const del = getComputedStyle(probe).color
  probe.style.color = 'var(--tone-time)'
  const time = getComputedStyle(probe).color
  probe.remove()
  document.body.appendChild(host)
  const [a, d, t] = [...host.children].map((el) => getComputedStyle(el).color)
  host.remove()
  return { a, d, t, add, del, time }
})
check('numbers: additions are green and deletions red (same tokens as the tool rows)', tone.a === tone.add && tone.d === tone.del && tone.a !== tone.d, JSON.stringify(tone))
check('numbers: timings carry one identity colour from the theme', tone.t === tone.time && tone.t !== tone.a && tone.t !== tone.d, JSON.stringify({ t: tone.t, time: tone.time }))

// ---- 跟随/暂停：上滚即停、提示可点 -------------------------------------------
// 规格：用户向上滚动后立即暂停自动跟随，并给出明确的 Jump to latest 提示；
// 点击提示恢复跟随。判据是**滚动容器自己**的位置（不是页面滚动）。
const jump = await page.eval(async () => {
  const el = document.querySelector('.narrative')
  if (!el) return { skipped: true }
  const overflow = el.scrollHeight - el.clientHeight
  if (overflow < 200) return { skipped: true, overflow }
  el.scrollTop = 0
  el.dispatchEvent(new Event('scroll', { bubbles: true }))
  await new Promise((r) => setTimeout(r, 400))
  const btn = document.querySelector('.narrative-jump')
  return { skipped: false, overflow, present: !!btn, label: btn?.textContent?.trim() ?? null, title: btn?.getAttribute('title') ?? null }
})
if (!jump.skipped) {
  check('follow: scrolling up stops the follow and offers a jump back', jump.present === true, JSON.stringify(jump))
  check('follow: that offer says Jump to latest (not a bare arrow)', /最新|latest/i.test(jump.label || ''), `${jump.label} / ${jump.title}`)
}
const resumed = await page.eval(async () => {
  const el = document.querySelector('.narrative')
  document.querySelector('.narrative-jump')?.click()
  await new Promise((r) => setTimeout(r, 600))
  const max = el.scrollHeight - el.clientHeight
  return { top: Math.round(el.scrollTop), max: Math.round(max), atBottom: Math.abs(el.scrollTop - max) < 4 }
})
if (!jump.skipped) check('follow: clicking the offer goes back to the latest output', resumed.atBottom === true, JSON.stringify(resumed))

// ---- 提问卡 ---------------------------------------------------------------
// 这一段要看**简洁档**的 Working 头（LivePanel 只在简洁档渲染），前面的密度段
// 结束时可能停在详细档 —— 显式设回来，别让断言的成败取决于上一段的残留状态
await page.eval(() => { window.__store.getState().setNarrativeDensity('concise') })
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
// 等待用户 = 运行暂停：Working 头必须说出来，而且**时钟停住**（活跃时长不含等待）。
// 读两次间隔 1.4s，两次相等才算停 —— 规格里的「不能把等待时间伪装成活跃执行时间」。
const waitCss = (await page.eval(() => {
  const live = document.querySelector('.cy-live')
  if (!live) return null
  const cs = getComputedStyle(live.querySelector('.cy-live-head'))
  return {
    state: live.getAttribute('data-waiting'),
    lbl: live.querySelector('.lv-lbl')?.textContent?.trim() ?? null,
    act: live.querySelector('.lv-act')?.textContent?.trim() ?? null,
    sticky: cs.position,
    top: cs.top,
  }
})) ?? { missing: true }
check('waiting: the working head says it is waiting for input', waitCss.state === 'input' && waitCss.lbl === 'Waiting' && waitCss.act === 'waiting for input', JSON.stringify(waitCss))
check('waiting: the head is sticky while the run is long', waitCss.sticky === 'sticky' && waitCss.top === '0px', `${waitCss.sticky} ${waitCss.top}`)
const clock1 = await page.eval(() => document.querySelector('.cy-live .lv-clock')?.textContent ?? null)
await wait(1400)
const clock2 = await page.eval(() => document.querySelector('.cy-live .lv-clock')?.textContent ?? null)
check('waiting: the active clock is frozen while we wait (waiting time is not execution time)', clock1 != null && clock1 === clock2, `${clock1} → ${clock2}`)

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
  const t = [...document.querySelectorAll('.rt-turn')].filter((x) => x.querySelector('.rt-act[aria-expanded]')).pop()
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
// 2026-10 用户口径：所有计时归一化、最多两级单位（1h30m / 1m25s / 1s666ms / 880ms）——
// 进位才有 h/m，单位之间不留空格，不出 `1 m 11 s` / `1.3s` 这种旧写法。
check('concise: the fold duration uses the normalized ladder',
  /Worked for (?:\d+h\d*m|\d+h|\d+m\d+s|\d+m|\d+s\d+ms|\d+s|\d+ms) ·/.test(c1.foldText), c1.foldText)
check('concise: no second copy of the file line', !c1.fileLine, c1.fileLine)

// 展开：工作过程进来，并且有高度上限（超出滚动）
// 点**最后一轮**的折角：会话恢复后页面上可能有更早的轮，`querySelector` 命中的
// 会是第一轮，点开的是别人的盒子（断言又在看最后一轮 → 假红）
await page.eval(() => [...document.querySelectorAll('.rt-turn')].filter((x) => x.querySelector('.rt-act[aria-expanded]')).pop()?.querySelector('.rt-act[aria-expanded]')?.click())
await wait(700)
const c2 = await page.eval(() => {
  const t = [...document.querySelectorAll('.rt-turn')].filter((x) => x.querySelector('.rt-workbox, .rt-act[aria-expanded]')).pop()
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
  const t = [...document.querySelectorAll('.rt-turn')].filter((x) => x.querySelector('.rt-workbox')).pop()
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
  // 轮尾顺序（用户 2026-10）：账在产出卡上方，产出卡是最后一块；有产出卡时不再
  // 单独画文件行（卡里已经逐个列了文件）。取**同一条轮**里的两个元素比对，跨轮比
  // 文档顺序没有意义。间距也取几何值：账底 → 卡顶应当是一个 10px 的节奏。
  const turn = [...document.querySelectorAll('.rt-turn')]
    .filter((t) => t.querySelector('.rt-ledger') && t.querySelector('.msg.artifacts'))
    .pop()
  const tLed = turn?.querySelector('.rt-ledger')
  const tArt = turn?.querySelector('.msg.artifacts')
  const order = {
    sameTurn: !!tLed && !!tArt,
    ledgerBeforeArtifacts: !!(tLed && tArt && (tLed.compareDocumentPosition(tArt) & Node.DOCUMENT_POSITION_FOLLOWING)),
    fileLine: !!turn?.querySelector('.cy-file'),
    gap: tLed && tArt ? Math.round(tArt.getBoundingClientRect().top - tLed.getBoundingClientRect().bottom) : null,
  }
  return { ledger: !!led, txt, meta, arts, order }
})
check('turn: the cost line exists and is the ledger', foot.ledger === true && /calls/.test(foot.txt || ''), foot.txt)
check('turn: the legacy TTFT line is gone (one fact, one line)', foot.meta === 0, foot.meta)
// 账上的四个数（turn）、悬停展开的三个时间（TTFT / LLM）、以及 tok/s 的速度值：
// 时间必须是归一化写法 —— 无小数秒、数字与单位之间无空格。
const fmtLedger = {
  txt: foot.txt,
  ladder: / · (?:\d+h\d*m|\d+h|\d+m\d+s|\d+m|\d+s\d+ms|\d+s|\d+ms) turn/.test(foot.txt || ''),
  noDecimalSec: !/\d\.\d+\s*s\b/.test(foot.txt || ''),
  noSpaceUnit: !/\d\s+(?:ms|s)\b/.test(foot.txt || ''),
}
check('turn: the ledger timings use the normalized ladder',
  fmtLedger.ladder && fmtLedger.noDecimalSec && fmtLedger.noSpaceUnit, JSON.stringify(fmtLedger))
check('turn: the ledger sits above the artifacts card', foot.order.sameTurn && foot.order.ledgerBeforeArtifacts, JSON.stringify(foot.order))
check('turn: artifacts replace the standalone file line', foot.order.sameTurn && !foot.order.fileLine, JSON.stringify(foot.order))
check('turn: ledger → artifacts keeps the 10px rhythm', foot.order.gap != null && foot.order.gap >= 6 && foot.order.gap <= 14, `${foot.order.gap}px`)
check('artifacts: the runtime card is the design one (.ar), not the old .art', foot.arts.modern > 0 && foot.arts.legacy === 0, JSON.stringify(foot.arts))
// 任务卡片里的产出也必须同一套（子会话＝主会话，只是 session 不同）：任何一处
// .ac-artifacts 里都不该再出现旧卡。没有任务卡时这条自然成立，有就必须是 .ar。
check('artifacts: the task card uses the same card', foot.arts.taskLegacy === 0, foot.arts.taskLegacy)

// ---- 上下文环：悬停看构成，点它问「要不要压缩」 ----------------------------
// 用户 2026-10 口径（覆盖设计里「点环 = 打开构成面板」的写法）：
//   ① 悬停 → 展开占用面板；移开后 **0.5s** 才收（慢半拍，不是一离开就断）；
//   ② 点击 / 回车 → 弹「压缩上下文？」确认。一个控件两种意图，点击不再兼作展开。
// 面板尾部的「压缩上下文…」仍是同一件事的第二入口（运行时段也够得着）。
const ctxMove = async (x, y) => page.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x, y, button: 'none' })
const ctxRing = await page.eval(() => {
  const r = document.querySelector('.row2 .ctx')?.getBoundingClientRect()
  return r ? { x: Math.round(r.x + r.width / 2), y: Math.round(r.y + r.height / 2) } : null
})
const ctxPanel = () => page.eval(() => !!document.querySelector('.ctx-panel'))
const ctxDialog = () => page.eval(() => !!document.querySelector('.modal-hint[role="dialog"]'))
await ctxMove(ctxRing.x, ctxRing.y)
await wait(240)
const ctxHover = await ctxPanel()
await ctxMove(12, 12)
await wait(220)
const ctxGrace = await ctxPanel() // 0.5s 宽限内：还开着
await wait(420)
const ctxGone = await ctxPanel() // 超过 0.5s：收掉
check('ctx ring: hovering opens the breakdown panel', ctxHover === true, ctxHover)
check('ctx ring: it stays open inside the 0.5s grace after the pointer leaves', ctxGrace === true, ctxGrace)
check('ctx ring: it closes 0.5s after the pointer leaves', ctxGone === false, ctxGone)

await page.eval(() => document.querySelector('.row2 .ctx').click())
await wait(400)
const ctxDialogOpen = await ctxDialog()
await page.eval(() => { document.querySelectorAll('.modal-hint .acts button')[0]?.click() })
await wait(250)
const ctxDialogClosed = await ctxDialog()
check('ctx ring: clicking asks whether to compact (not just opening the panel)',
  ctxDialogOpen === true && (await ctxPanel()) === false && ctxDialogClosed === false,
  JSON.stringify({ dialog: ctxDialogOpen, closedAfterCancel: ctxDialogClosed }))

// 面板里的第二入口（悬停打开 → 尾部按钮 → 同一个确认）
await ctxMove(ctxRing.x, ctxRing.y)
await wait(300)
const ctxCompactEntry = await page.eval(async () => {
  const btn = document.querySelector('.ctx-compact')
  const label = btn?.textContent?.trim() ?? null
  if (btn) btn.click()
  await new Promise((r) => setTimeout(r, 400))
  const dialog = !!document.querySelector('.modal-hint[role="dialog"]')
  document.querySelectorAll('.modal-hint .acts button')[0]?.click()
  return { label, dialog }
})
check('ctx: compaction keeps a second entry in the panel', ctxCompactEntry.label === '压缩上下文…' && ctxCompactEntry.dialog === true,
  JSON.stringify(ctxCompactEntry))
await ctxMove(12, 12)
await wait(600)

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

// ---- `/` 菜单：一行就是一行 ---------------------------------------------------
// 真实故障（2026-10）：技能的 description 是整段话，被原样塞进 28px 高的补全行 →
// 折成十几行、压到相邻行、溢出面板，整块菜单看上去是「字挤成一团」。
// 两道防线一起量：文案收敛（shortReason）+ 行裁切（overflow/nowrap/ellipsis）。
await page.eval(() => {
  const ta = document.querySelector('.composer textarea') || document.querySelector('textarea')
  ta.focus()
  const set = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set
  set.call(ta, '/')
  ta.dispatchEvent(new Event('input', { bubbles: true }))
})
await wait(700)
const slash = await page.eval(() => {
  const pop = document.querySelector('.pop[data-pop="cmd"]')
  if (!pop) return null
  const rows = [...pop.querySelectorAll('.pop-row')]
  const cs = getComputedStyle(pop)
  const m = (r) => r.querySelector('.m')
  return {
    width: Math.round(pop.getBoundingClientRect().width),
    maxH: cs.maxHeight,
    overflow: cs.overflowY,
    rowHeights: rows.map((r) => Math.round(r.getBoundingClientRect().height)),
    // 说明格必须是一行：字体 10.5 → 行高 16 左右，两行就会是 21+（故障现场实测 21/42…）
    descHeights: rows.filter((r) => m(r)).map((r) => Math.round(m(r).getBoundingClientRect().height)),
    // 每行内容的右边界不能越过行本身（溢出面板的前兆）
    bleed: rows.filter((r) => {
      const rb = r.getBoundingClientRect()
      return [...r.children].some((c) => {
        const cb = c.getBoundingClientRect()
        return cb.bottom > rb.bottom + 1 || cb.right > rb.right + 1
      })
    }).length,
    longest: Math.max(0, ...rows.filter((r) => m(r)).map((r) => (m(r).textContent ?? '').length)),
    // 被截断的那些行：title 里必须还是**全文**（截断只发生在视觉上，不许丢信息）
    truncated: rows
      .filter((r) => (r.querySelector('.m')?.textContent ?? '').endsWith('…'))
      .map((r) => ({ m: r.querySelector('.m').textContent, title: r.title || '' })),
    rowOverflow: rows.length ? getComputedStyle(rows[0]).overflow : null,
    ellipsis: (() => { const mm = rows.map(m).find((x) => x && (x.textContent ?? '').endsWith('…')); return mm ? getComputedStyle(mm)['textOverflow'] : null })(),
  }
})
check('slash menu: it is a completion popover, not a panel (386 / 430 cap)',
  slash?.width === 386 && slash?.maxH === '430px' && slash?.overflow === 'auto', JSON.stringify(slash && { w: slash.width, m: slash.maxH, o: slash.overflow }))
check('slash menu: every row is one 28px line', (slash?.rowHeights ?? []).every((h) => h === 28), JSON.stringify(slash?.rowHeights))
check('slash menu: the description cell never wraps to a second line',
  (slash?.descHeights ?? []).every((h) => h <= 17), JSON.stringify(slash?.descHeights))
check('slash menu: no row content bleeds outside its row', slash?.bleed === 0, slash?.bleed)
check('slash menu: a whole-paragraph description is shortened, not dumped',
  (slash?.longest ?? 0) <= 31, slash?.longest)
check('slash menu: the row clips and ellipsizes', slash?.rowOverflow === 'hidden' && slash?.ellipsis === 'ellipsis',
  JSON.stringify({ o: slash?.rowOverflow, e: slash?.ellipsis }))
check('slash menu: the full description survives in the hover title',
  (slash?.truncated ?? []).length > 0 &&
    (slash.truncated ?? []).every((r) => r.title.length > r.m.length) &&
    (slash.truncated ?? []).some((r) => r.title.includes('tasteskill v2')),
  JSON.stringify((slash?.truncated ?? []).map((r) => r.title.slice(0, 50))))
await page.eval(() => {
  const ta = document.querySelector('.composer textarea') || document.querySelector('textarea')
  const set = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set
  set.call(ta, '')
  ta.dispatchEvent(new Event('input', { bubbles: true }))
  ta.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
})
await wait(300)
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
// ---- 结算时机：折叠行与轮尾账只许在 AgentEnd 之后出现 ---------------------------
// 用户 2026-10 报障：这两样在 llm_end 就出现了（llm_end 与下一次 llm_start 之间
// `turn.running` 是 false，旧判据于是把最后一轮当成跑完了）。现在闸门是视图上的
// runEnded（agent_start 置 false / agent_end 置 true），见 lib/turnSettle.ts。
await page.goto(URL, { settleMs: 3000 })
await waitStore()
await page.eval(() => {
  const st = window.__store.getState()
  st.setNarrativeDensity('concise')
})
await wait(400)
await say('live output 尾巴')
await waitUntil("!!document.querySelector('.cy-live')", 20000)
const during = await page.eval(() => {
  const st = window.__store.getState()
  const v = st.views[st.activeSessionId]
  return {
    runEnded: v.runEnded,
    worked: [...document.querySelectorAll('.rt-act')].some((n) => /Worked/.test(n.textContent || '')),
    foot: document.querySelectorAll('.rt-turn-foot').length,
  }
})
check('settle: while the run is live, nothing is settled yet (runEnded=false)',
  during.runEnded === false, JSON.stringify(during))
check('settle: no "Worked for" line and no ledger before AgentEnd',
  during.worked === false && during.foot === 0, JSON.stringify(during))
await waitUntil("(window.__store.getState().views[window.__store.getState().activeSessionId].runEnded === true)", 60000)
await wait(900)
const after = await page.eval(() => {
  const st = window.__store.getState()
  const v = st.views[st.activeSessionId]
  return {
    runEnded: v.runEnded,
    worked: [...document.querySelectorAll('.rt-act')].some((n) => /Worked/.test(n.textContent || '')),
    foot: document.querySelectorAll('.rt-turn-foot').length,
    ledger: document.querySelector('.rt-turn-foot')?.textContent?.replace(/\s+/g, ' ').slice(0, 60) ?? null,
  }
})
check('settle: after AgentEnd the fold line and the ledger appear',
  after.runEnded === true && after.worked && after.foot > 0, JSON.stringify(after))

// ---- 骨架屏（切换会话的加载动画）-----------------------------------------------
// 直接驱动状态机：mock 的 new_session 是同步响应，靠真实加载时序测不到它 —— 而这里要
// 验的是「骨架屏长什么样、动效在不在」，不是「这次加载用了多久」。
const skel = async (phase) => {
  await page.eval((p) => { window.__store.setState({ skeleton: { phase: p, at: Date.now() } }) }, phase)
  await wait(120)
  return page.eval(() => {
    const el = document.querySelector('.session-skeleton')
    if (!el) return { present: false }
    const cs = getComputedStyle(el)
    const sheen = el.querySelector('.ss-sheen')
    const bar = el.querySelector('.ss-bar')
    return {
      present: true,
      rows: el.querySelectorAll('.ss-row').length,
      anim: cs.animationName,
      dur: cs.animationDuration,
      sheen: sheen ? getComputedStyle(sheen).animationName : null,
      barH: bar ? Math.round(bar.getBoundingClientRect().height) : 0,
      cls: el.className,
      leaving: el.classList.contains('is-leaving'),
    }
  })
}
const skPending = await skel('pending')
const skVisible = await skel('visible')
const skExit = await skel('exit')
check('skeleton: nothing is painted while the load is still under the threshold', skPending.present === false, JSON.stringify(skPending))
check('skeleton: it draws the conversation shape (rows with bars + scanning sheen)',
  skVisible.present && skVisible.rows > 3 && skVisible.sheen === 'ssSheen' && skVisible.barH === 9,
  JSON.stringify({ rows: skVisible.rows, sheen: skVisible.sheen, barH: skVisible.barH }))
check('skeleton: it fades in', skVisible.anim === 'ssFadeIn', `${skVisible.anim} ${skVisible.dur}`)
check('skeleton: the exit is a cross-fade, not a cut',
  skExit.present && skExit.leaving && skExit.anim === 'ssFadeOut' && skExit.dur === '0.32s',
  JSON.stringify({ anim: skExit.anim, dur: skExit.dur, cls: skExit.cls }))
// 立刻收回（后面的断言不该看到一个假加载态）
await page.eval(() => { window.__store.setState({ skeleton: { phase: 'idle', at: 0 } }) })
await wait(120)

// ---- 切回看过的会话：也要有一次入场动画 ----------------------------------------
// 用户 2026-10：「骨架屏/加载动画没有了」——他切的是**看过的**会话（缓存命中，
// 按设计不进骨架屏）。缓存路径必须补上「历史渐入」，否则是瞬间替换。
const enterAnim = await page.eval(() => {
  const st = window.__store.getState()
  const ids = Object.keys(st.views)
  if (ids.length < 2) return { skipped: true, ids }
  const target = ids.find((id) => id !== st.activeSessionId)
  st.setActiveSession(target)
  return { skipped: false, target }
})
await wait(120)
const enterState = await page.eval(() => {
  const n = document.querySelector('.narrative')
  const child = n?.querySelector('.rt-turn, .msg')
  return {
    cls: n?.className ?? '',
    childAnim: child ? getComputedStyle(child).animationName : null,
    childDur: child ? getComputedStyle(child).animationDuration : null,
    enterAnimAt: window.__store.getState().enterAnimAt,
  }
})
check('session switch: a cached switch still replays the history entrance',
  enterAnim.skipped || (/narrative-enter/.test(enterState.cls) && /narrativeEnter/.test(enterState.childAnim ?? '')),
  JSON.stringify({ enterAnim, ...enterState }))

// ---- 用户块：整列宽度 + 用户自己写的 Markdown + 换行 -----------------------------
// 用户 2026-10 报障两件事：① 用户块比整列窄一大截（设计原文 max-width: 62ch，
// 在这个字号下只有 534px，而列宽 752px）——自己说的话比模型的回答窄，读起来像引用；
// ② 换行没了、自己打的 Markdown 不生效（此前是手写的「只认行内 code」渲染器）。
// 直接往视图里追加一条用户块（形状与真实输入完全一致：{kind:'user', id, text}）。
// 为什么不用 say()：mock 的脚本 run 还在跑时新输入会排在 pendingQueue 里，要等这条
// run 结束才落块 —— 那一段是「队列语义」的测试，不是这段要验的**渲染**；而且它要等
// 十几秒，还会让后面所有断言都排在一个不确定的时刻上。
await page.eval(() => {
  // 活跃会话的转录就是顶层 blocks（Narrative 无 viewSid 时读它）
  const blocks = [...window.__store.getState().blocks, { kind: 'user', id: 999001, text: '第一行\n第二行\n\n- 条目一\n- 条目二\n\n**粗体** 和 `inline`' }]
  window.__store.setState({ blocks })
})
const you = await waitUntil(
  "[...document.querySelectorAll('.rt-you .md')].some((m) => m.textContent.includes('第一行'))",
  20000,
)
const user = await page.eval(() => {
  const block = [...document.querySelectorAll('.rt-you')].filter((el) => el.textContent.includes('第一行')).pop()
  const turn = block?.closest('.rt-turn')
  const body = block?.querySelector('.rt-body')
  const md = block?.querySelector('.md')
  const ps = md ? [...md.querySelectorAll('p')] : []
  const p0 = ps[0]
  const lh = p0 ? parseFloat(getComputedStyle(p0).lineHeight) : 0
  const cs = (el, k) => el ? getComputedStyle(el)[k] : null
  return {
    bodyW: body ? Math.round(body.getBoundingClientRect().width) : null,
    // 参照物 = 同一列的其它块（用户块自己在 grid 里就是整列宽；列宽含 12px 竖轨内距）
    youW: block ? Math.round(block.getBoundingClientRect().width) : null,
    colW: turn ? Math.round(turn.getBoundingClientRect().width) : null,
    maxW: cs(body, 'maxWidth'),
    ws: cs(p0, 'whiteSpace'),
    // 「第一行 / 第二行」必须占两行（单行高度 = lineHeight）
    firstLines: p0 && lh ? Math.round(p0.getBoundingClientRect().height / lh) : 0,
    firstText: p0?.textContent ?? '',
    items: md ? [...md.querySelectorAll('li')].map((el) => el.textContent) : [],
    strong: md ? [...md.querySelectorAll('strong')].map((el) => el.textContent) : [],
    codes: md ? [...md.querySelectorAll('code')].map((el) => el.textContent) : [],
    rail: cs(body, 'borderLeftWidth'),
    font: cs(p0, 'fontSize'),
  }
})
check('user block: the prompt spans the conversation column (no 62ch cap)',
  you && user.bodyW === user.youW && user.maxW === 'none' && user.bodyW > 700,
  JSON.stringify({ body: user.bodyW, column: user.youW, max: user.maxW }))
check('user block: line breaks the user typed survive', user.ws === 'pre-wrap' && user.firstLines === 2,
  JSON.stringify({ ws: user.ws, lines: user.firstLines, text: user.firstText.slice(0, 24) }))
check('user block: the user\'s own markdown is rendered (list / bold / inline code)',
  user.items.length === 2 && user.strong.includes('粗体') && user.codes.includes('inline'),
  JSON.stringify({ items: user.items, strong: user.strong, codes: user.codes }))
check('user block: it keeps the design rail and size', user.rail === '2px' && user.font === '13.5px',
  JSON.stringify({ rail: user.rail, font: user.font }))

// ---- 一行信号（Small signals）：九类事件 --------------------------------------------------
// 判据全部来自 design/agent-runtime/v2 的 .sig 规则（ui.components.css:970-990）：
// 图标 14 / gap 10 / padding 9·4 / 标题 12.5·500 / 副题 11 / 右侧事件名 9.5 mono 靠右。
// 数据走**真事件通道**（window.__mockEmit → store 门禁 → signal 块 → Signal 组件），
// 不直接改 store，也不 mock 组件；字段名照 Go events/event.go 的原名。
await say('信号样例')
await wait(600)
const sigInjected = await page.eval(() => {
  const emit = window.__mockEmit
  if (!emit) return 'no __mockEmit hook'
  const sid = window.__store.getState().activeSessionId
  const at = Date.now() / 1000
  // 两条 memory 事件聚成一行（store 里 900ms 内的同向写入是一条），file 是副题
  emit({ event_type: 'memory_activity', session_id: sid, action: 'saved', file: 'a.md', timestamp: at })
  emit({ event_type: 'memory_activity', session_id: sid, action: 'saved', file: 'b.md', timestamp: at })
  emit({ event_type: 'tool_hook_warning', session_id: sid, name: 'gofmt', message: 'returned 2', timestamp: at })
  // 故意不给 tool_calls：缺字段不该被编出来
  emit({ event_type: 'run_silent_end', session_id: sid, rounds: 4, saw_assistant_text: true, timestamp: at })
  emit({ event_type: 'goal_alignment_reminder', session_id: sid, round: 7, timestamp: at })
  emit({ event_type: 'task_promoted', session_id: sid, task_id: 'tooltask-abc', kind: 'tool', name: 'bash', timestamp: at })
  emit({ event_type: 'session_budget_exceeded', session_id: sid, cost: 0.0234, max_cost: 0.02, timestamp: at })
  emit({ event_type: 'session_persist_error', session_id: sid, error: 'disk full', timestamp: at })
  emit({ event_type: 'session_closed', session_id: sid, runs: 18, consumed: 17, abandoned_mid_tool: 1, timestamp: at })
  // 同一条再来一次：不该刷出第二行（去重）
  emit({ event_type: 'tool_hook_warning', session_id: sid, name: 'gofmt', message: 'returned 2', timestamp: at })
  return 'ok'
})
check('signals: real event channel accepts the events', sigInjected === 'ok', sigInjected)
// memory 那行是 900ms 防抖后落块的，所以等够再断
await wait(1600)
const gotSig = await waitUntil("document.querySelectorAll('.sig').length >= 8", 15000)
const sigRows = await page.eval(() => {
  const rows = [...document.querySelectorAll('.sig')]
  const one = (k) => rows.find((r) => r.dataset.kind === k)
  const cs = (el, p) => (el ? getComputedStyle(el)[p] : null)
  const row = one('hook')
  const icon = row?.querySelector('svg')
  const title = row?.querySelector('b')
  const detail = row?.querySelector('span:not(.sig-t)')
  const type = row?.querySelector('.sig-t')
  return {
    n: rows.length,
    kinds: rows.map((r) => r.dataset.kind),
    display: cs(row, 'display'),
    gap: cs(row, 'gap'),
    padding: cs(row, 'padding'),
    iconSize: [cs(icon, 'width'), cs(icon, 'height'), cs(icon, 'strokeWidth')],
    titleSize: [cs(title, 'fontSize'), cs(title, 'fontWeight')],
    detailSize: [cs(detail, 'fontSize'), cs(detail, 'color')],
    typeSize: [cs(type, 'fontSize'), type && cs(type, 'fontFamily').toLowerCase(), cs(type, 'marginLeft')],
    typeText: type?.textContent ?? null,
    typeRightGap: type && row ? Math.round(row.getBoundingClientRect().right - type.getBoundingClientRect().right) : null,
    colors: Object.fromEntries(rows.map((r) => { const s = r.querySelector('svg'); return [r.dataset.kind, s ? getComputedStyle(s).color : null] })),
    titles: Object.fromEntries(rows.map((r) => [r.dataset.kind, r.querySelector('b')?.textContent ?? ''])),
    details: Object.fromEntries(rows.map((r) => [r.dataset.kind, r.querySelector('span:not(.sig-t)')?.textContent ?? ''])),
    insideWork: rows.some((r) => !!r.closest('.rt-work')),
    height: row ? Math.round(row.getBoundingClientRect().height) : null,
  }
})
check('signals: the nine kinds render as one row each', gotSig && sigRows.n === 9, JSON.stringify({ n: sigRows.n, kinds: sigRows.kinds }))
check('signals: a repeated event does not add a row (dedupe)', sigRows.n === 9, sigRows.n)
check('signals: row is a 10px flex with 9/4 padding',
  sigRows.display === 'flex' && sigRows.gap === '10px' && sigRows.padding === '9px 4px', JSON.stringify({ d: sigRows.display, g: sigRows.gap, p: sigRows.padding }))
check('signals: icon 14px / stroke 1.8', sigRows.iconSize.join('|') === '14px|14px|1.8px', sigRows.iconSize.join('|'))
check('signals: title 12.5px / weight 500', sigRows.titleSize[0] === '12.5px' && sigRows.titleSize[1] === '500', sigRows.titleSize.join('|'))
check('signals: detail 11px', sigRows.detailSize[0] === '11px', sigRows.detailSize.join('|'))
// margin-left:auto 在 Chromium 里读到的是**用值**（px），所以这里只要求「有正的左外距」，
// 真正判「靠右」的是下一条：与行右边缘刚好差 4px（行内 padding）。
check('signals: event name on the right, 9.5px mono',
  sigRows.typeSize[0] === '9.5px' && /mono|menlo|ui-monospace|sfmono/.test(sigRows.typeSize[1] ?? '') &&
    parseFloat(String(sigRows.typeSize[2])) > 0,
  JSON.stringify(sigRows.typeSize))
check('signals: event name sits 4px off the row edge', sigRows.typeRightGap === 4, sigRows.typeRightGap)
check('signals: event name is the raw event_type, untranslated', sigRows.typeText === 'tool_hook_warning', sigRows.typeText)
// 九类图标各归其色（violet / amber / accent / red / green / ink-3）：至少 5 种不同颜色
const distinct = new Set(Object.values(sigRows.colors).filter(Boolean))
check('signals: icon colour differs by kind', distinct.size >= 5, JSON.stringify(sigRows.colors))
check('signals: zh copy is built from the fact, not prose in the store',
  sigRows.titles.memory === '已保存 2 条记忆' && sigRows.titles.hook === '自动化规则发出警告' &&
    sigRows.titles.silent === '这次运行没有产出文本' && sigRows.titles.budget === '会话预算已用尽',
  JSON.stringify(sigRows.titles))
check('signals: sub-line carries only the facts we were given',
  sigRows.details.memory === 'a.md · b.md' && sigRows.details.hook === 'gofmt · returned 2' &&
    sigRows.details.persist === 'disk full' && sigRows.details.budget === '$0.0234 / $0.0200',
  JSON.stringify(sigRows.details))
check('signals: a missing field is absent, not invented',
  (sigRows.details.silent ?? '') === '4 轮', sigRows.details.silent)
// session_closed 的运行账：三个数都是后端事实（Go 的 Session.recordRunOutcome）
check('signals: session_closed reports the run ledger',
  sigRows.details.closed === '18 次运行 · 17 次正常收尾 · 1 次停在工具中间', sigRows.details.closed)
check('signals: rows live outside the working box (a record, not work)', sigRows.insideWork === false, sigRows.insideWork)

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
