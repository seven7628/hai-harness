// 审计：运行时皮肤下，叙述区里还有哪些「旧皮肤」的块级类名在活着。
// 判据：设计 v2 的类名前缀是 rt- / cy- / ar- / sig- / la- / ask / pop / pv / work；
// 其余（index.css 的老类）出现在 runtime 皮肤里就是「同一份事实的第二套渲染」。
// 用法：node lab/audit-legacy-blocks.mjs
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const b = await Browser.launch({ width: 1440, height: 1200 })
const page = await b.newPage({ width: 1440, height: 1200 })
await page.goto(URL, { settleMs: 4000 })
await page.eval(() => {
  window.__store.getState().setNarrativeDensity('concise')
})
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
await page.eval(() => {
  const ta = document.querySelector('textarea')
  const s = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set
  s.call(ta, '运行时样例')
  ta.dispatchEvent(new Event('input', { bubbles: true }))
  ta.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true }))
})
for (let i = 0; i < 80; i++) {
  await wait(1000)
  if (await page.eval(() => !!document.querySelector('.rt-ledger'))) break
}
await wait(1500)

const out = await page.eval(() => {
  const RUNTIME = /^(rt-|cy-|ar-|sig-|la-|ask|pop|pv|work|p-drop|p-|att\b|md-text|narrative)/
  const rows = []
  const walk = (el, depth) => {
    if (depth > 4) return
    for (const c of el.children) {
      const cls = (c.className || '').toString()
      if (!RUNTIME.test(cls) && cls && depth <= 3) rows.push({ d: depth, cls, tag: c.tagName.toLowerCase(), txt: (c.textContent || '').trim().slice(0, 28) })
      walk(c, depth + 1)
    }
  }
  const nar = document.querySelector('.narrative')
  if (nar) walk(nar, 0)
  const heads = [...document.querySelectorAll('.rt-head, .msg.assistant .who')].map((e) => ({
    cls: e.className.toString(),
    txt: (e.textContent || '').replace(/\s+/g, ' ').trim().slice(0, 40),
    fs: getComputedStyle(e).fontSize,
    h: Math.round(e.getBoundingClientRect().height),
  }))
  // 间距审计：叙述区里的块各自带着多少外边距 —— 旧类名若把 index.css 的 margin
  // 带进 runtime 皮肤，就会出现「有的块 6、有的块 18」这种一眼看得出不齐
  const spacing = [...(document.querySelector('.narrative')?.querySelectorAll(':scope > *, .rt-turn > *, .rt-turn-ans > *, .rt-turn-foot') ?? [])]
    .slice(0, 24)
    .map((e) => {
      const cs = getComputedStyle(e)
      return {
        cls: (e.className || '').toString().slice(0, 28),
        m: [cs.marginTop, cs.marginBottom].join('/'),
        gap: cs.rowGap,
        p: [cs.paddingTop, cs.paddingBottom].join('/'),
      }
    })
  return { count: rows.length, rows: rows.slice(0, 8), heads, spacing }
})
console.log(JSON.stringify(out, null, 2))
await b.close()
