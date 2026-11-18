// 空闲时为什么每帧都有强制布局？三个对照：
//   ① 现状（常驻 rAF tick 每帧读 scrollHeight + 写 scrollTop）
//   ② 关掉 content-visibility（.msg 的 content-visibility: auto 会自己重估可见性）
//   ③ 掐掉 tick 的 scrollTop 写回（写 scrollTop → 滚动锚定 → 布局重算）
//
// 量的是**空闲 2 秒**里的 LayoutCount / RecalcStyleCount / TaskDuration。
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'
import { readFileSync } from 'node:fs'
import { createServer } from 'node:http'

const FILE = process.argv[2] ?? '/tmp/real-blocks-46m.json'
const URL = process.argv[3] ?? 'http://localhost:5288/'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const raw = readFileSync(FILE, 'utf8')
const srv = createServer((_req, res) => {
  const buf = Buffer.from(raw, 'utf8')
  res.writeHead(200, { 'content-type': 'application/json', 'content-length': buf.length, 'access-control-allow-origin': '*' })
  res.end(buf)
})
await new Promise((r) => srv.listen(0, '127.0.0.1', r))
const DATA_URL = `http://127.0.0.1:${srv.address().port}/blocks.json`

const b = await Browser.launch({ width: 1440, height: 900 })
const page = await b.newPage({ width: 1440, height: 900 })
await page.goto(URL, { settleMs: 5000 })
for (let i = 0; i < 200; i++) { if (await page.eval(() => !!window.__store && !!window.__dispatch)) break; await wait(150) }
try { await page.send('Performance.enable', { timeDomain: 'timeTicks' }) } catch { /* 老 client */ }

await page.eval(async (arg) => {
  const data = await (await fetch(arg.url)).json()
  const sid = 's-real'
  const st = window.__store.getState()
  const view = {
    ...(st.views[sid] ?? {}),
    blocks: data.blocks, runNodes: data.runNodes ?? [], turns: data.turns ?? [],
    traceEvents: [], approvals: [], questions: [], todos: [], diffs: [], agentDetached: [],
    pendingQueue: [], toolStartTs: {}, reqBlock: {}, thinkStartTs: {}, turnThinkMs: {},
    usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0 },
    git: { is_repo: false, ahead: 0, behind: 0, staged: 0, modified: 0, untracked: 0, conflicted: 0, clean: true },
    gitHasMore: false, gitHistLoading: false, gitLoading: false,
    runEnded: false, aborted: false, compressing: false, streaming: false,
  }
  window.__store.setState({
    views: { ...st.views, [sid]: view },
    activeSessionId: sid, blocks: data.blocks, runNodes: data.runNodes ?? [], turns: data.turns ?? [],
  })
}, { url: DATA_URL })
await wait(9000)   // 等铺开放完

const metrics = async () => {
  const m = await page.send('Performance.getMetrics')
  const map = {}
  for (const { name, value } of m.metrics) map[name] = value
  return map
}
const D = (a, z) => {
  const o = {}
  for (const k of ['TaskDuration', 'ScriptDuration', 'LayoutDuration', 'RecalcStyleDuration', 'LayoutCount', 'RecalcStyleCount'])
    o[k] = +((z[k] ?? 0) - (a[k] ?? 0)).toFixed(4)
  return o
}
const idle = async (label) => {
  const m0 = await metrics()
  await wait(2000)
  const m1 = await metrics()
  const d = D(m0, m1)
  console.log(`  ${label.padEnd(34)} layout ${String(d.LayoutCount).padStart(4)} 次 ｜ style ${String(d.RecalcStyleCount).padStart(4)} 次 ｜ layout ${d.LayoutDuration}s ｜ style ${d.RecalcStyleDuration}s ｜ task ${d.TaskDuration}s`)
  return d
}

console.log('空闲 2 秒的主线程开销（无任何事件）：\n')
await idle('① 现状')

// ② 关掉 content-visibility
await page.eval(() => {
  const st = document.createElement('style')
  st.id = 'cv-off'
  st.textContent = '.msg { content-visibility: visible !important; }'
  document.head.appendChild(st)
})
await wait(600)
await idle('② 关 content-visibility')

// ③ 再关掉 content-visibility 的同时，掐掉 tick 的 scrollTop 写回
await page.eval(() => {
  // 盖住 Element.prototype.scrollTop 的 setter：只放行「不变」的写
  const proto = Element.prototype
  const desc = Object.getOwnPropertyDescriptor(proto, 'scrollTop')
  if (!desc || !desc.set) return
  let blocked = 0
  Object.defineProperty(proto, 'scrollTop', {
    configurable: true,
    get() { return desc.get.call(this) },
    set(v) {
      // 已经在目标位置就别再写（tick 每帧都在写同一个值）
      if (Math.abs(desc.get.call(this) - v) < 0.5) { blocked++; return }
      desc.set.call(this, v)
    },
  })
  window.__blocked = () => blocked
})
await wait(600)
await idle('③ 再掐掉 tick 的重复 scrollTop 写')
const blocked = await page.eval(() => window.__blocked?.() ?? -1)
console.log(`\n  （被拦下的重复 scrollTop 写：${blocked} 次 / 2 秒）`)

await b.close?.()
srv.close()
