// 「rich 8.5ms/chunk」里，**解析**占多少、**React 重建**占多少？
//
// 办法：同一段正文连做两种渲染，各测 10 次，取中位数差：
//   A) 追加 1 个字符 —— Markdown 的 useMemo 键（text）变了 → 重新解析 + 重建整棵树
//   B) 不改 text，只改一个「与本轮无关但会触发整条转录重渲染」的全局位（waiting）
//      —— useMemo 命中 → 只重建、不解析
// A − B 就是**解析**这一份账；B 本身是「用现成的 AST 重建 React 树」的账。
//
// 用法：npx vite --port 5288 & node lab/probe-md-split.mjs
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const SID = 'mds-1'
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
try { await page.send('Performance.enable', { timeDomain: 'timeTicks' }) } catch { /* 老 client */ }
const metrics = async () => {
  const m = await page.send('Performance.getMetrics')
  const map = {}
  for (const { name, value } of m.metrics) map[name] = value
  return map
}
const dScript = (a, z) => ((z.ScriptDuration ?? 0) - (a.ScriptDuration ?? 0)) * 1000

await page.eval((sid) => {
  const st = window.__store.getState()
  st.setNarrativeSkin('runtime')
  st.setNarrativeDensity('detailed')
  window.__store.setState({ activeSessionId: sid, blocks: [], turns: [], runNodes: [], runEnded: true })
}, SID)
await wait(400)
await page.eval((sid) => window.__dispatch({ event_type: 'agent_start', session_id: sid, run_id: 's-run', index: 0, timestamp: new Date().toISOString() }), SID)

// 铺到 ~8k 字（和 bench-markdown 的末段对齐）
for (let k = 0; k < 10; k++) {
  const pieces = []
  for (let i = 0; i < 20; i++) pieces.push(RICH[(k * 20 + i) % RICH.length](k * 20 + i))
  await page.eval(async (arg) => {
    for (const p of arg.pieces) {
      window.__dispatch({ event_type: 'content_chunk', session_id: arg.sid, request_id: 's-r', run_id: 's-run', content: p })
      await new Promise((r) => setTimeout(r, 14))
    }
  }, { sid: SID, pieces })
  await wait(120)
}
const chars = await page.eval(() => { const st = window.__store.getState(); return (st.blocks[st.blocks.length - 1]?.text ?? '').length })
console.log(`正文 ${chars} 字\n`)

const A = []
const B = []
for (let i = 0; i < 10; i++) {
  // A：追加 1 个空格 → 解析 + 重建
  const m0 = await metrics()
  await page.eval((sid) => window.__dispatch({ event_type: 'content_chunk', session_id: sid, request_id: 's-r', run_id: 's-run', content: ' ' }), SID)
  await wait(90)
  const m1 = await metrics()
  A.push(dScript(m0, m1))
  // B：只翻 waiting（approvals 有无）→ useMemo 命中，只重建
  const m2 = await metrics()
  await page.eval(() => {
    const st = window.__store.getState()
    window.__store.setState({ approvals: [{ id: 1, kind: 'x', title: 'probe' }] })
    void st
  })
  await wait(90)
  await page.eval(() => window.__store.setState({ approvals: [] }))
  await wait(90)
  const m3 = await metrics()
  B.push(dScript(m2, m3))
}
const med = (xs) => { const a = xs.slice().sort((x, y) => x - y); return +a[Math.floor((a.length - 1) / 2)].toFixed(2) }
console.log(`A 追加 1 字符（解析 + 重建）：${A.map((x) => x.toFixed(1)).join(', ')}  → 中位数 ${med(A)}ms`)
console.log(`B 仅重渲染（AST 走缓存）    ：${B.map((x) => x.toFixed(1)).join(', ')}  → 中位数 ${med(B)}ms`)
console.log(`\n解析 ≈ ${(med(A) - med(B)).toFixed(2)}ms/次，重建 ≈ ${med(B).toFixed(2)}ms/次（各占 ${((1 - med(B) / med(A)) * 100).toFixed(0)}% / ${((med(B) / med(A)) * 100).toFixed(0)}%）`)
await b.close?.()
