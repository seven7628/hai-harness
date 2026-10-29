// 量产出卡与 calls 行的实际几何（设计字面值来自 parts/artifacts.css + ui.components.css）。
// 用法：node lab/measure-artifacts.mjs（vite dev 在 5288）
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const b = await Browser.launch({ width: 1440, height: 1200 })
const page = await b.newPage({ width: 1440, height: 1200 })
await page.goto(URL, { settleMs: 4000 })
await page.eval(() => {
  const st = window.__store
  st.getState().setNarrativeSkin('runtime')
  st.getState().setNarrativeDensity('concise')
})
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const set = (t) => page.eval((x) => {
  const ta = document.querySelector('textarea')
  const s = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set
  s.call(ta, x)
  ta.dispatchEvent(new Event('input', { bubbles: true }))
  ta.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true }))
}, t)

await set('运行时样例')
for (let i = 0; i < 90; i++) {
  await wait(1000)
  if (await page.eval(() => !!document.querySelector('.ar'))) break
}
await wait(500)

const out = await page.eval(() => {
  const px = (v) => Math.round(v * 100) / 100
  const rect = (el) => { const r = el.getBoundingClientRect(); return { x: px(r.x), w: px(r.width), h: px(r.height) } }
  const ar = document.querySelector('.ar')
  const main = ar?.querySelector('.ar-main')
  const ico = main?.querySelector('.ar-ico')
  const body = main?.querySelector('.ar-m-b')
  const acts = main?.querySelector('.ar-m-a')
  const cs = (el, ...ps) => (el ? Object.fromEntries(ps.map((p) => [p, getComputedStyle(el)[p]])) : null)
  // calls 行：找出真的写着「calls」的那一行
  const callEl = [...document.querySelectorAll('*')].find((e) => e.children.length === 0 && /\bcalls?\b/.test(e.textContent || ''))
  const callRow = callEl?.closest('.rt-grp > *, .rt-batch, .rt-calls, [class*="batch"], [class*="call"]') ?? callEl?.parentElement
  return {
    arW: ar ? px(ar.getBoundingClientRect().width) : null,
    arPad: cs(ar, 'paddingLeft'),
    mainGrid: cs(main, 'gridTemplateColumns'),
    mainAreas: cs(main, 'gridTemplateAreas'),
    ico: ico ? { ...rect(ico), ...cs(ico, 'fontSize') } : null,
    bodyRect: body ? rect(body) : null,
    actsRect: acts ? rect(acts) : null,
    actsAlign: cs(acts, 'alignSelf'),
    rowSameLine: body && acts ? Math.abs(rect(body).x - rect(acts).x) > 100 : null,
    headText: ar?.querySelector('.ar-h-t')?.textContent?.trim() ?? null,
    footText: ar?.querySelector('.ar-cap, .ar-more')?.textContent?.trim() ?? null,
    btnText: acts ? [...acts.querySelectorAll('button')].map((x) => x.textContent.trim()) : null,
    callText: callEl?.textContent?.trim() ?? null,
    callCls: callRow?.className ?? null,
    callStyles: callRow ? cs(callRow, 'fontSize', 'color', 'fontFamily', 'gap', 'paddingLeft') : null,
    callRect: callRow ? rect(callRow) : null,
  }
})
console.log(JSON.stringify(out, null, 2))
await b.close()
