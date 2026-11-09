// 探针 v2：切会话的滚动定位，覆盖「普通/上滑后再切/长短差异/冷切换」四种路径。
// 用法：node lab/probe-switch-scroll2.mjs
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const b = await Browser.launch({ width: 1280, height: 720 })
const page = await b.newPage({ width: 1280, height: 720 })
await page.goto(URL, { settleMs: 4500 })
const waitUntil = async (expr, timeout = 30000, step = 150) => {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) {
    if (await page.eval(new Function('return (' + expr + ')'))) return true
    await wait(step)
  }
  return false
}
await waitUntil('!!window.__store')
await page.eval(() => window.__store.getState().setNarrativeDensity('detailed'))
await page.eval(() => { document.documentElement.dataset.skin = 'runtime' })
await wait(300)

const mk = async (askText) => {
  await page.eval(() => window.__store.getState().newSession())
  await wait(1200)
  const sid = await page.eval(() => window.__store.getState().activeSessionId)
  await page.eval((t) => window.__store.getState().ask(t), askText)
  await waitUntil(`(() => { const v = window.__store.getState().views[${JSON.stringify(sid)}]; return !!v && v.runEnded && v.blocks.length > 2 })()`, 25000)
  await wait(1500)
  const st = await page.eval((s) => {
    const el = document.querySelector('.narrative')
    return { sid: s, blocks: window.__store.getState().views[s].blocks.length, top: el.scrollTop, max: el.scrollHeight - el.clientHeight }
  }, sid)
  console.log('made', JSON.stringify(st))
  return sid
}

const A = await mk('长会话')
const B = await mk('短') // normalScript 走常规时长，块不多
const C = await mk('长会话')

// 上滑（wheel + scrollTop）把 A 的跟随关掉
const scrollUpIn = async (px) => {
  return page.eval((d) => {
    const el = document.querySelector('.narrative')
    el.dispatchEvent(new WheelEvent('wheel', { deltaY: -120, bubbles: true }))
    el.scrollTop = Math.max(0, el.scrollTop - d)
    return { top: el.scrollTop, max: el.scrollHeight - el.clientHeight }
  }, px)
}

const switchAndSample = async (to, label, ms = 1600) => {
  const rows = await page.eval((arg) => new Promise((res) => {
    const el0 = document.querySelector('.narrative')
    const rows = []
    const t0 = performance.now()
    window.__store.getState().setActiveSession(arg.sid, '/mock/workspace')
    const tick = () => {
      const el = document.querySelector('.narrative')
      const jump = document.querySelector('.narrative-jump')
      const d = window.__narrDbg || {}
      rows.push({
        t: Math.round(performance.now() - t0),
        same: el === el0,
        top: Math.round(el.scrollTop),
        max: Math.round(el.scrollHeight - el.clientHeight),
        away: !!jump,
        kind: jump ? (jump.querySelector('.narrative-jump-dot') ? 'fresh' : 'away') : null,
        follow: d.follow, stick: d.stick, hard: d.hard, exp: d.expected,
        kids: el.children.length,
      })
      if (performance.now() - t0 < arg.ms) requestAnimationFrame(tick)
      else res(rows)
    }
    requestAnimationFrame(tick)
  }), { sid: to, ms })
  const tops = rows.map((r) => r.top)
  const maxs = rows.map((r) => r.max)
  const tail = rows.slice(-8)
  const settled = tail.every((r) => Math.abs(r.top - r.max) <= 1)
  const offBottomFrames = rows.filter((r) => r.max > 200 && r.max - r.top > 120).length
  console.log(`\n[切到 ${label}] frames=${rows.length} 首帧 top/max=${rows[0].top}/${rows[0].max} 末帧=${tail.at(-1).top}/${tail.at(-1).max}`)
  console.log(`  节点替换=${rows[0].same === false} top ${Math.min(...tops)}..${Math.max(...tops)} max ${Math.min(...maxs)}..${Math.max(...maxs)}`)
  console.log(`  「离底 >120px」帧数=${offBottomFrames}  away 帧数=${rows.filter((r) => r.away).length} (away=${rows.filter((r) => r.kind === 'away').length} fresh=${rows.filter((r) => r.kind === 'fresh').length})  末态贴底=${settled}`)
  const bad = rows.filter((r) => r.away && r.max > 200 && r.max - r.top <= 4)
  if (bad.length) console.log(`  !! 贴底却显示按钮：${bad.length} 帧，例：`, bad.slice(0, 4).map((r) => `${r.t}:${r.kind} f=${r.follow ? 1 : 0} s=${r.stick ? 1 : 0} h=${r.hard ? 1 : 0} top=${r.top}/${r.max}`).join(' '))
  const interesting = rows.filter((r, i) => i === 0 || Math.abs(r.top - rows[i - 1].top) > 40 || r.away !== rows[i - 1].away)
  console.log('  跳变帧:', interesting.slice(0, 16).map((r) => `${r.t}:${r.top}/${r.max}${r.away ? '·away' : ''}${r.kids ? '' : '·0kid'}`).join(' '))
  return rows
}

await page.eval(() => { window.__narrLog = [] })
console.log('\n=== 1) 贴底状态下切到另一个长会话 ===')
await switchAndSample(C, 'C 长')
await wait(700)

console.log('\n=== 2) C 上滑 3000px 后再切到 A（A 也长）===')
console.log('  上滑后', JSON.stringify(await scrollUpIn(3000)))
await switchAndSample(A, 'A 长')
await wait(700)

const lg = await page.eval(() => (window.__narrLog || []).slice(-40))
console.log('--- log ---\n' + lg.join('\n'))
console.log('\n=== 3) 切到短会话 B ===')
await switchAndSample(B, 'B 短')
await wait(700)

console.log('\n=== 4) 从短会话 B 切回长会话 C ===')
await switchAndSample(C, 'C 长')
await b.close?.()
