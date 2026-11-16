// 探针：被摘成后台任务、且**带部分输出**（checkpoint 恢复出来的 running 块）的工具行，
// 行头到底占几行。用户 2026-11 截图：`停止 / 后台运行中` 挤在右端，时长 `· 2m43s…` 掉到第二行。
// 用法：LAB_URL=http://localhost:5288/ node lab/probe-tool-row.mjs
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const b = await Browser.launch({ width: 1280, height: 1000 })
const page = await b.newPage({ width: 1280, height: 1000 })
await page.goto(URL, { settleMs: 4000 })
const waitUntil = async (expr, timeout = 20000) => {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) {
    if (await page.eval(new Function('return (' + expr + ')'))) return true
    await wait(250)
  }
  return false
}
if (!(await waitUntil('!!window.__store && !!window.__hydrate'))) throw new Error('store/hydrate never appeared')
await page.eval(() => window.__store.getState().setNarrativeDensity('concise'))
await wait(400)
await page.eval(() => { document.documentElement.dataset.skin = 'runtime' })
await wait(200)
for (let i = 0; i < 3 && !(await page.eval(() => !!window.__store.getState().activeSessionId)); i++) {
  await page.eval(() => window.__store.getState().newSession())
  await wait(900)
}
const sid = await page.eval(() => window.__store.getState().activeSessionId)

const probe = async (name, extras) => {
  await page.eval(new Function(`
    const store = window.__store
    const st = store.getState()
    const sid = ${JSON.stringify(sid)}
    const blocks = [
      { kind: 'user', id: 1, text: '跑一个长命令' },
      { kind: 'tool', id: 2, toolId: 'xr', name: 'bash', status: 'running', runId: 'r1',
        args: ${JSON.stringify(JSON.stringify({ command: 'ls ~/.go-code/events | head -5; echo ---; grep -rl "tooltask" .' }))},
        startedAt: Date.now() - 163000, ${extras} },
    ]
    const v = st.views[sid] ?? {}
    store.setState({
      blocks,
      views: { ...st.views, [sid]: { ...v, blocks, runEnded: false, runNodes: [{ id: 'r1', kind: 'run', status: 'running', label: 'run', depth: 0, startedAt: Date.now() - 163000 }], streaming: true, turns: [], usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0 } } },
    })
  `))
  await wait(700)
  const geo = await page.eval(() => {
    const hd = document.querySelector('.tool-row-hd')
    if (!hd) return null
    const cs = getComputedStyle(hd)
    const kids = [...hd.children].map((el) => {
      const r = el.getBoundingClientRect()
      const s = getComputedStyle(el)
      return {
        cls: String(el.className || '').split(' ').slice(0, 2).join('.'),
        text: (el.textContent || '').trim().slice(0, 16),
        w: Math.round(r.width), top: Math.round(r.top),
        display: s.display, col: s.gridColumnStart, row: s.gridRowStart,
      }
    })
    const vis = kids.filter((k) => k.display !== 'none')
    const tops = [...new Set(vis.map((k) => Math.round(k.top / 8)))]
    return {
      hdH: Math.round(hd.getBoundingClientRect().height),
      cols: cs.gridTemplateColumns,
      lines: tops.length,
      visible: vis.map((k) => `${k.cls}[w${k.w},top${k.top},c${k.col},r${k.row}]=${k.text}`),
      hidden: kids.filter((k) => k.display === 'none').map((k) => k.cls),
    }
  })
  console.log(`\n### ${name}\n` + JSON.stringify(geo, null, 2))
  return geo
}

// (1) 用户截图的状态：摘离 + 有部分输出（metric 有内容）+ 长命令
const a = await probe('promoted + result(metric) + long cmd', `promoted: true, taskId: 'tooltask-mtmkyu3s', taskStatus: 'running', result: 'Users_admin_a\\nUsers_admin_b\\nUsers_admin_c\\n'`)
// (2) 对照：同样行，但没有 result（纯 live 态）
const c = await probe('promoted, no result', `promoted: true, taskId: 'tooltask-mtmkyu3s', taskStatus: 'running'`)

await page.eval(() => document.querySelector('.tool-row').scrollIntoView({ block: 'center' }))
await wait(400)
const box = await page.eval(() => {
  const el = document.querySelector('.tool-row')
  const r = el.getBoundingClientRect()
  return { x: Math.max(0, Math.round(r.x) - 8), y: Math.max(0, Math.round(r.y) - 26), width: Math.round(r.width) + 16, height: Math.round(r.height) + 34 }
})
await page.screenshot({ path: '/tmp/probe-tool-row.png', clip: box })
console.log('clip', JSON.stringify(box))
console.log('\nshot /tmp/probe-tool-row.png')
await b.close()
