// 真实转录树渲染成本探针（2026-11-09）。
//
// 把**真实会话**的 live view blocks（导出的 JSON）灌进真实浏览器里的真 store，
// 让 Narrative 真的挂载这棵树，量：
//   ① 挂载耗时 & DOM 节点数
//   ② 之后每来一条事件的重渲耗时（长会话的真实节奏）
//   ③ 关掉 content-visibility 后是否变快（验证「离屏跳过」是否在生效）
//
// 用法：node lab/probe-real-transcript-render.mjs <blocks.json> [url]
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'
import { readFileSync } from 'node:fs'
import { createServer } from 'node:http'

const FILE = process.argv[2] ?? '/tmp/real-blocks-wiki.json'
const URL = process.argv[3] ?? 'http://localhost:5173/'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))

const raw = readFileSync(FILE, 'utf8')
console.log('数据：%s (%.1f MB)', FILE, raw.length / 1048576)

// 大 JSON 走 HTTP 拉（CDP eval 传 85MB 会超时）
const srv = createServer((_req, res) => {
  const buf = Buffer.from(raw, 'utf8')
  res.writeHead(200, {
    'content-type': 'application/json',
    'content-length': buf.length, // 字节数，不是字符数（中文 JSON 会差很多）
    'access-control-allow-origin': '*',
  })
  res.end(buf)
})
await new Promise((r) => srv.listen(0, '127.0.0.1', r))
const DATA_URL = `http://127.0.0.1:${srv.address().port}/blocks.json`

const b = await Browser.launch({ width: 1440, height: 900 })
const page = await b.newPage({ width: 1440, height: 900 })
await page.goto(URL, { settleMs: 5000 })
for (let i = 0; i < 200; i++) { if (await page.eval(() => !!window.__store && !!window.__dispatch)) break; await wait(150) }
console.log('页面已就绪')

// 把真实 blocks 灌进 store（走 store 的 setState，和真实运行时同一条路）
const t0 = Date.now()
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
    activeSessionId: sid,
    blocks: data.blocks,
    runNodes: data.runNodes ?? [],
    turns: data.turns ?? [],
  })
}, { url: DATA_URL })
console.log('灌入 store：%d ms', Date.now() - t0)

// 等 Narrative 挂载 + reveal 铺开
const mountStart = Date.now()
await wait(500)
const m1 = await page.eval(() => ({
  dom: document.querySelectorAll('*').length,
  turns: document.querySelectorAll('.rt-turn').length,
  users: document.querySelectorAll('.msg.user').length,
  bodyText: document.querySelector('.narrative')?.innerText?.length ?? 0,
}))
console.log('挂载后：DOM 节点 %d，.rt-turn %d，.msg.user %d（等 %d ms）', m1.dom, m1.turns, m1.users, Date.now() - mountStart)

// 等铺开动画结束（reveal 每帧 25 块）
for (let i = 0; i < 60; i++) {
  await wait(1000)
  const t = await page.eval(() => document.querySelectorAll('.rt-turn').length)
  if (i === 2 || i === 9) console.log('  t+%ds：.rt-turn %d 个', i + 1, t)
  if (t >= m1.turns && t > 0 && i > 3) break
}
const m2 = await page.eval(() => ({
  dom: document.querySelectorAll('*').length,
  turns: document.querySelectorAll('.rt-turn').length,
  users: document.querySelectorAll('.msg.user').length,
  bodyText: document.querySelector('.narrative')?.innerText?.length ?? 0,
}))
console.log('铺开后：DOM 节点 %d，.rt-turn %d，.msg.user %d，正文字符 %d', m2.dom, m2.turns, m2.users, m2.bodyText)
if (page.consoleErrors?.length) {
  console.log('')
  console.log('!! 页面异常 %d 条：', page.consoleErrors.length)
  for (const e of page.consoleErrors.slice(0, 5)) console.log('   ', e.split('\n')[0])
}

// 量每条事件的重渲（真实节奏：每事件都替换整个 blocks 数组 → 整树重渲）
const per = await page.eval(async (sid) => {
  const d = window.__dispatch
  const times = []
  for (let i = 0; i < 10; i++) {
    const s = performance.now()
    d({
      event_type: 'tool_run_start', session_id: sid, run_id: 'rq', id: 'pz' + i, name: 'bash',
      arguments: '{"command":"ls"}', timestamp: new Date().toISOString(),
    })
    await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)))
    times.push(performance.now() - s)
  }
  return times
}, 's-real')
console.log('')
console.log('每条事件 → 全树重渲+排版：%s', per.map((x) => x.toFixed(0)).join('ms, ') + 'ms')
const avg = per.reduce((a, x) => a + x, 0) / per.length
console.log('平均 %.0f ms/事件 → 28 事件/分钟 需要 %s 才能追平', avg, (avg * 28 / 60000).toFixed(1) + ' 分钟')

// 对照：把 content-visibility 关掉（模拟「跑着的那轮」被强制 visible 的效果）
await page.eval(() => {
  const st = document.createElement('style')
  st.id = 'cv-off'
  st.textContent = '.msg { content-visibility: visible !important; }'
  document.head.appendChild(st)
})
await wait(500)
const per2 = await page.eval(async (sid) => {
  const d = window.__dispatch
  const times = []
  for (let i = 0; i < 10; i++) {
    const s = performance.now()
    d({
      event_type: 'tool_run_start', session_id: sid, run_id: 'rq2', id: 'pz2' + i, name: 'bash',
      arguments: '{"command":"ls"}', timestamp: new Date().toISOString(),
    })
    await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)))
    times.push(performance.now() - s)
  }
  return times
}, 's-real')
const avg2 = per2.reduce((a, x) => a + x, 0) / per2.length
console.log('对照（content-visibility: visible）：平均 %.0f ms/事件（放大 %.1f×）', avg2, avg2 / Math.max(avg, 0.001))

await b.close()
srv.close()
