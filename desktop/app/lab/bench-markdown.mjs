// 第 3 步的测量：**长回答流式时，markdown 重解析到底值多少钱**。
//
// 背景：每个 streaming chunk 都会让最后那一轮重渲染，正文 Markdown 组件按 text 这个
// 字符串重新解析一遍（useMemo 的键是 text，text 每 chunk 都变）。所以如果解析是
// O(累计长度)，整段流式就是 O(n²)。这条脚本量三件事：
//
//   ① 每 chunk 的 script 成本，按「累计字符数」分桶 —— 看它是平的还是随长度上涨
//      （平 = 解析很便宜，不值得优化；涨 = O(n²)，长回答会越来越卡）
//   ② 两种内容的对照：plain（纯中文叙述）vs rich（列表/表格/代码围栏/标题混排）
//      —— 差价就是「结构解析」的成本上限
//   ③ 旧皮肤（current）做对照 —— 同一个 Markdown 组件，判断这是不是新皮肤独有的账
//
// 用法：npx vite --port 5288 & node lab/bench-markdown.mjs [--chunks=200]
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const TOTAL = Number((process.argv.find((a) => a.startsWith('--chunks=')) || '').split('=')[1] || 200)
const BUCKET = 20
const wait = (ms) => new Promise((r) => setTimeout(r, ms))

const PLAIN = (i) => `这是第 ${i} 段用于测量的普通正文，没有特殊结构，长度大约三四十个字，用来代表日常回答。`
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
const SID = 'md-1'

const reset = async (skin, density) => {
  await page.eval((arg) => {
    const st = window.__store.getState()
    const views = { ...st.views }
    delete views[arg.sid]
    window.__store.setState({ views, activeSessionId: arg.sid, blocks: [], blocksTotal: 0, turns: [], runNodes: [], runEnded: true })
    st.setNarrativeDensity(arg.d)
  }, { sid: SID, skin, d: density })
  await wait(400)
}

// 一段流：TOTAL 个 chunk，每 BUCKET 个测一次 script 增量
const run = async (flavor) => {
  await page.eval((sid) => window.__dispatch({ event_type: 'agent_start', session_id: sid, run_id: 'md-run', index: 0, timestamp: new Date().toISOString() }), SID)
  await wait(250)
  const buckets = []
  let chars = 0
  let idx = 0
  const t0 = await metrics()
  let prev = t0
  for (let k = 0; k < TOTAL / BUCKET; k++) {
    // 函数过不了 CDP（只有结构化数据能序列化）—— 文本在 Node 侧生成，按桶传进去
    const pieces = []
    for (let i = 0; i < BUCKET; i++) {
      const n = idx + i
      pieces.push(flavor === 'rich' ? RICH[n % RICH.length](n) : PLAIN(n))
    }
    const txt = await page.eval(async (arg) => {
      const d = window.__dispatch
      for (const piece of arg.pieces) {
        d({ event_type: 'content_chunk', session_id: arg.sid, request_id: 'md-r', run_id: 'md-run', content: piece })
        await new Promise((r) => setTimeout(r, arg.gap))
      }
      return arg.pieces.join('').length
    }, { sid: SID, pieces, gap: 18 })
    idx += BUCKET
    chars += txt
    await wait(120)
    const m = await metrics()
    const sec = (m.ScriptDuration ?? 0) - (prev.ScriptDuration ?? 0)
    prev = m
    buckets.push({ from: k * BUCKET, to: (k + 1) * BUCKET, chars, msPerChunk: +((sec * 1000) / BUCKET).toFixed(2) })
  }
  const end = await metrics()
  return {
    buckets,
    total: +(((end.ScriptDuration ?? 0) - (t0.ScriptDuration ?? 0))).toFixed(3),
    chars,
    // 末段 / 首段的比值：≈1 = 线性（每 chunk 常数）；>1.3 = 随长度上涨（接近 O(n²)）
    ratio: +(buckets[buckets.length - 1].msPerChunk / Math.max(buckets[0].msPerChunk, 0.01)).toFixed(2),
  }
}

const rows = []
for (const skin of ['runtime']) { // 2026-11：current/v3 已删除
  for (const density of ['detailed']) {
    for (const flavor of ['plain', 'rich']) {
      await reset(skin, density)
      const r = await run(flavor)
      rows.push({ skin, density, flavor, ...r })
      console.log(`\n[${skin}/${density}/${flavor}] ${r.chars} 字，总 script ${r.total}s，末段/首段 = ${r.ratio}×`)
      console.log('  ' + r.buckets.map((x) => `${x.chars}字:${x.msPerChunk}ms`).join('  '))
    }
  }
}

const pick = (skin, flavor) => rows.find((r) => r.skin === skin && r.flavor === flavor)
console.log('\n=== 结论 ===')
for (const flavor of ['plain', 'rich']) {
  const rt = pick('runtime', flavor)
  const first = rt.buckets[0].msPerChunk
  const last = rt.buckets[rt.buckets.length - 1].msPerChunk
  console.log(`${flavor.padEnd(6)} runtime: 首段 ${first}ms/chunk → 末段 ${last}ms/chunk（${rt.ratio}×）`
    + `  整段 ${rt.total}s / ${rt.chars} 字`)
}
const richOverPlain = pick('runtime', 'rich').buckets.at(-1).msPerChunk - pick('runtime', 'plain').buckets.at(-1).msPerChunk
const plainGrowth = pick('runtime', 'plain').buckets.at(-1).msPerChunk - pick('runtime', 'plain').buckets[0].msPerChunk
console.log(`结构解析的差价（rich − plain，末段）≈ ${richOverPlain.toFixed(2)}ms/chunk；plain 自身随长度增长 ≈ ${plainGrowth.toFixed(2)}ms/chunk`)
console.log('判读：plain 的增长就是「同一段文字每次重解析」的价格；rich 额外那部分是表格/围栏的解析与渲染。')
await b.close?.()
