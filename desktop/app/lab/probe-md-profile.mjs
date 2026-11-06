// 把「rich markdown 每 chunk 8.5ms」拆开：是**解析**（remark/micromark）、
// 还是 **React 重渲染**（表格/代码块的树）？两者的修法完全不同，先看火焰图。
//
// 做法：先用 rich 内容流到 ~5k 字，然后用 CPU Profiler 采 10 个 chunk 的样本，
// 按 self time 聚合到函数名（采样法，只看主线程）。
//
// 用法：npx vite --port 5288 & node lab/probe-md-profile.mjs
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const SID = 'mdp-1'

const RICH = [
  (i) => `\n\n- 第 ${i} 项：**要点** 与 \`inline code\`\n- 第二项：普通文本\n`,
  (i) => `\n\n| 指标 | 值 |\n|---|---|\n| 第 ${i} 行 | 123 |\n`,
  (i) => `\n\n\`\`\`js\nconst v${i} = ${i}\nconsole.log(v${i})\n\`\`\`\n`,
  (i) => `\n\n### 小标题 ${i}\n\n再写一两句正文，长度与普通段落相当。\n`,
]

const b = await Browser.launch({ width: 1280, height: 900 })
const page = await b.newPage({ width: 1280, height: 900 })
await page.goto(URL, { settleMs: 4500 })
for (let i = 0; i < 200; i++) { if (await page.eval(() => !!window.__store && !!window.__hydrate)) break; await wait(150) }

await page.eval((sid) => {
  const st = window.__store.getState()
  st.setNarrativeSkin('runtime')
  st.setNarrativeDensity('detailed')
  window.__store.setState({ activeSessionId: sid, blocks: [], turns: [], runNodes: [], runEnded: true })
}, SID)
await wait(400)

// 先把正文铺到 ~5k 字
await page.eval((sid) => window.__dispatch({ event_type: 'agent_start', session_id: sid, run_id: 'p-run', index: 0, timestamp: new Date().toISOString() }), SID)
for (let k = 0; k < 5; k++) {
  const pieces = []
  for (let i = 0; i < 20; i++) pieces.push(RICH[(k * 20 + i) % RICH.length](k * 20 + i))
  await page.eval(async (arg) => {
    for (const p of arg.pieces) {
      window.__dispatch({ event_type: 'content_chunk', session_id: arg.sid, request_id: 'p-r', run_id: 'p-run', content: p })
      await new Promise((r) => setTimeout(r, 16))
    }
  }, { sid: SID, pieces })
  await wait(150)
}
const chars = await page.eval(() => {
  const st = window.__store.getState()
  const b = st.blocks[st.blocks.length - 1]
  return (b?.text ?? '').length
})
console.log(`正文已铺到 ${chars} 字，开始采样…`)

await page.send('Profiler.enable')
await page.send('Profiler.setSamplingInterval', { interval: 100 })
await page.send('Profiler.start')
const pieces2 = []
for (let i = 100; i < 110; i++) pieces2.push(RICH[i % RICH.length](i))
await page.eval(async (arg) => {
  for (const p of arg.pieces) {
    window.__dispatch({ event_type: 'content_chunk', session_id: arg.sid, request_id: 'p-r', run_id: 'p-run', content: p })
    await new Promise((r) => setTimeout(r, 30))
  }
}, { sid: SID, pieces: pieces2 })
await wait(200)
const { profile } = await page.send('Profiler.stop')

// 采样法：samples[i] 属于 timeDeltas[i] 这段时间；按 nodeId 聚合
const byId = new Map()
for (const n of profile.nodes) byId.set(n.id, n)
const self = new Map()
for (let i = 0; i < profile.samples.length; i++) {
  const id = profile.samples[i]
  const dt = profile.timeDeltas[i] || 0
  self.set(id, (self.get(id) || 0) + dt)
}
const rows = []
for (const [id, us] of self) {
  const n = byId.get(id)
  if (!n) continue
  const f = n.callFrame
  const name = `${f.functionName || '(anonymous)'}${f.url ? ' @ ' + f.url.split('/').pop().split('?')[0] : ''}`
  const cur = rows.find((r) => r.name === name)
  if (cur) cur.us += us
  else rows.push({ name, us })
}
rows.sort((a, b2) => b2.us - a.us)
const totalUs = rows.reduce((n, r) => n + r.us, 0)
console.log(`\n采样总时长 ${(totalUs / 1000).toFixed(1)}ms（10 个 chunk）\n`)
for (const r of rows.slice(0, 18)) {
  console.log(`${(r.us / 1000).toFixed(1).padStart(7)}ms  ${((r.us / totalUs) * 100).toFixed(1).padStart(5)}%  ${r.name.slice(0, 110)}`)
}
// 归类：解析 vs React vs 其它
const sum = (re) => rows.filter((r) => re.test(r.name)).reduce((n, r) => n + r.us, 0)
console.log('\n归类：')
console.log(`  解析类（micromark/remark/rehype/unified/mdast/hast）: ${(sum(/micromark|remark|rehype|unified|mdast|hast|mdurl|decode|parse/i) / 1000).toFixed(1)}ms`)
console.log(`  React 类（react-dom/commit/reconcile/beginWork/completeWork）: ${(sum(/react-dom|React|commit|reconcil|beginWork|completeWork/i) / 1000).toFixed(1)}ms`)
console.log(`  本仓渲染件（Markdown/Table/CodeBlock/ToolRow 等）: ${(sum(/Markdown|Table|Code|md-|ToolRow|RichStats|hljs|shiki|prism/i) / 1000).toFixed(1)}ms`)
await b.close?.()
