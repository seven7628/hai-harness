// 第 2 步的内存代价：markdown 解析缓存（hast 树）到底占多少堆。
//
// 做法：把 10 条 6k 字结构化正文铺进缓存 → 卸载 DOM → 强制 GC → 读堆；
// 再清空缓存 → 再 GC → 再读。差值就是「这些正文的 hast 树」的净占用。
//
// 用法：npx vite --port 5288 & node lab/probe-md-cache-mem.mjs
import { readFileSync } from 'node:fs'
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const CP_PATH = process.env.CP_PATH || '/tmp/real-cp-settled.json'
const base = JSON.parse(readFileSync(CP_PATH, 'utf8'))
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const MSGS = Number(process.env.MSGS || 10)
const CHARS = Number(process.env.CHARS || 6000)

const unit = (i) => [
  `第 ${i} 段正文，带 **粗体** 与 \`code\`。\n`,
  `- 项 ${i}A\n- 项 ${i}B\n`,
  `| a | b |\n|---|---|\n| ${i} | ${i * 2} |\n`,
  '```js\nconst x' + i + ' = ' + i + '\n```\n',
].join('\n')

const b = await Browser.launch({ width: 1280, height: 900 })
const page = await b.newPage({ width: 1280, height: 900 })
await page.goto(URL, { settleMs: 4500 })
for (let i = 0; i < 200; i++) { if (await page.eval(() => !!window.__store && !!window.__mdCache)) break; await wait(150) }
const gc = async () => { try { await page.send('HeapProfiler.collectGarbage') } catch { /* 老 client */ } await wait(400) }
const heap = () => page.eval(() => (performance.memory ? +(performance.memory.usedJSHeapSize / 1048576).toFixed(1) : null))

const blocks = []
let id = 1
for (let i = 0; i < MSGS; i++) {
  let text = ''
  while (text.length < CHARS) text += unit(i)
  blocks.push({ kind: 'user', id: id++, text: `问题 ${i}`, ts: Date.now() })
  blocks.push({ kind: 'assistant', id: id++, text: text.slice(0, CHARS), streaming: false, ts: Date.now(), model: 'm' })
}

await page.eval(() => window.__mdCache.clear())
await page.eval(() => window.__store.getState().setNarrativeDensity('detailed'))
await page.eval((arg) => window.__store.setState({ activeSessionId: 'mem-1', blocks: arg, turns: [], runNodes: [], runEnded: true }), blocks)
await wait(2500)
const stats = await page.eval(() => window.__mdCache.stats())
await page.eval(() => window.__store.setState({ activeSessionId: 'mem-empty', blocks: [], turns: [], runNodes: [], runEnded: true }))
await wait(800)
await gc()
const withCache = await heap()
await page.eval(() => window.__mdCache.clear())
await gc()
const withoutCache = await heap()
console.log(`正文 ${MSGS} 条 × ${CHARS} 字 = ${((MSGS * CHARS) / 1000).toFixed(0)}k 字`)
console.log(`缓存条目 ${stats.entries} / ${stats.chars} 字`)
console.log(`堆：带缓存 ${withCache}MB → 清空后 ${withoutCache}MB ⇒ **净占用 ${(withCache - withoutCache).toFixed(1)}MB**`)
console.log(`折合每 10k 字 ≈ ${(((withCache - withoutCache) / ((MSGS * CHARS) / 10000)) * 1024).toFixed(0)}KB`)
await b.close?.()
