// 探针：滚动跟随的三条不变量（回归用）。
//   ① 上滑后内容增长 → 视图一动不动（不被拽回底部）
//   ② 切会话 → 新会话停在底部，且不出现「贴底却有按钮」
//   ③ 贴底时内容增长 → 跟到底
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const b = await Browser.launch({ width: 1280, height: 720 })
const page = await b.newPage({ width: 1280, height: 720 })
await page.goto('http://localhost:5288/', { settleMs: 4500 })
const waitUntil = async (expr, timeout = 30000, step = 150) => {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) { if (await page.eval(new Function('return (' + expr + ')'))) return true; await wait(step) }
  return false
}
await waitUntil('!!window.__store')
await page.eval(() => window.__store.getState().setNarrativeDensity('detailed'))
await page.eval(() => { document.documentElement.dataset.skin = 'runtime' })
await wait(300)
const mk = async (text) => {
  await page.eval(() => window.__store.getState().newSession())
  await wait(1000)
  const sid = await page.eval(() => window.__store.getState().activeSessionId)
  await page.eval((t) => window.__store.getState().ask(t), text)
  await waitUntil(`(() => { const v = window.__store.getState().views[${JSON.stringify(sid)}]; return !!v && v.runEnded && v.blocks.length > 2 })()`, 25000)
  await wait(1500)
  return sid
}
const A = await mk('长会话')
const B = await mk('长会话')
await page.eval((s) => window.__store.getState().setActiveSession(s, '/mock/workspace'), A)
await wait(1200)

// ① 上滑 + 追加内容
const r1 = await page.eval(async (sid) => {
  const el = document.querySelector('.narrative')
  el.dispatchEvent(new WheelEvent('wheel', { deltaY: -120, bubbles: true }))
  el.scrollTop = Math.max(0, el.scrollTop - 2500)
  const before = Math.round(el.scrollTop)
  await new Promise((r) => setTimeout(r, 250))
  const midTop = Math.round(el.scrollTop)
  // 追加一段正文（与真实流式同类）
  window.__dispatch({ event_type: 'llm_start', session_id: sid, request_id: 'extra', run_id: 'R0', model: 'm', timestamp: new Date().toISOString() })
  for (let i = 0; i < 6; i++) {
    window.__dispatch({ event_type: 'content_chunk', session_id: sid, request_id: 'extra', run_id: 'R0', content: `追加正文第 ${i + 1} 段。\\n\\n` })
    await new Promise((r) => setTimeout(r, 60))
  }
  window.__dispatch({ event_type: 'llm_end', session_id: sid, request_id: 'extra', run_id: 'R0', model: 'm', finish_reason: 'stop', usage: { Input: 10, Output: 10, CacheRead: 0 }, timestamp: new Date().toISOString() })
  await new Promise((r) => setTimeout(r, 400))
  const after = Math.round(el.scrollTop)
  const btn = document.querySelector('.narrative-jump')
  return { before, midTop, after, gap: Math.round(el.scrollHeight - el.clientHeight - el.scrollTop), btn: btn ? (btn.querySelector('.narrative-jump-dot') ? 'fresh' : 'away') : null }
}, A)
console.log('① 上滑+追加:', JSON.stringify(r1), r1.after === r1.midTop ? 'OK 没被拽回' : '!! 被拽回了')

// ② 切会话（从上滑状态切）
const r2 = await page.eval(async (sid) => {
  const el0 = document.querySelector('.narrative')
  const rows = []
  const t0 = performance.now()
  window.__store.getState().setActiveSession(sid, '/mock/workspace')
  await new Promise((res) => {
    const tick = () => {
      const el = document.querySelector('.narrative')
      const btn = document.querySelector('.narrative-jump')
      rows.push({ t: Math.round(performance.now() - t0), el: el === el0, top: Math.round(el.scrollTop), max: Math.round(el.scrollHeight - el.clientHeight), btn: btn ? (btn.querySelector('.narrative-jump-dot') ? 'fresh' : 'away') : null })
      if (performance.now() - t0 < 1200) requestAnimationFrame(tick)
      else res()
    }
    requestAnimationFrame(tick)
  })
  const bad = rows.filter((r) => r.max > 200 && r.max - r.top > 4)
  return { frames: rows.length, firstAtBottom: rows[0].top === rows[0].max, offBottom: bad.length, away: rows.filter((r) => r.btn === 'away').length, last: rows.at(-1) }
}, B)
console.log('② 切会话:', JSON.stringify(r2), r2.offBottom === 0 && r2.away === 0 ? 'OK 首帧即贴底、无挂住按钮' : '!! 定位不准')

// ③ 贴底 + 追加 → 跟到底
const r3 = await page.eval(async (sid) => {
  const el = document.querySelector('.narrative')
  const before = Math.round(el.scrollTop)
  window.__dispatch({ event_type: 'llm_start', session_id: sid, request_id: 'extra2', run_id: 'R0', model: 'm', timestamp: new Date().toISOString() })
  for (let i = 0; i < 5; i++) {
    window.__dispatch({ event_type: 'content_chunk', session_id: sid, request_id: 'extra2', run_id: 'R0', content: `贴底追加第 ${i + 1} 段。\\n\\n` })
    await new Promise((r) => setTimeout(r, 80))
  }
  window.__dispatch({ event_type: 'llm_end', session_id: sid, request_id: 'extra2', run_id: 'R0', model: 'm', finish_reason: 'stop', usage: { Input: 10, Output: 10, CacheRead: 0 }, timestamp: new Date().toISOString() })
  await new Promise((r) => setTimeout(r, 500))
  const gap = Math.round(el.scrollHeight - el.clientHeight - el.scrollTop)
  return { before, gap, grew: Math.round(el.scrollHeight) }
}, B)
console.log('③ 贴底跟随:', JSON.stringify(r3), Math.abs(r3.gap) <= 2 ? 'OK 贴底' : '!! 掉队')
await b.close?.()
