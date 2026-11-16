// 切分渲染必须与**整段渲染**产出完全相同的 DOM —— 这是这次改动的唯一硬判据。
//
// 做法：同一条 assistant 正文，渲染两次：
//   A) streaming=false → 整段一次解析（改动前的唯一路径）
//   B) streaming=true  → 切分路径（已完成的块各自解析 + 尾部一块）
// 比较 .md 的 innerHTML（逐字节）+ 元素计数 + 文本长度。相等才算「样子没变」。
//
// 覆盖三种形态：① 停在段落边界 ② 停在列表中间 ③ 停在未闭合的代码围栏里
//
// 用法：npx vite --port 5288 & node lab/probe-md-stream-equal.mjs
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
let fail = 0
const ok = (m) => console.log('OK ' + m)
const bad = (m) => { fail++; console.log('!! ' + m) }

const unit = (i) => [
  `第 ${i} 段正文，带 **粗体** 与 \`inline code\`，以及一个 [链接](https://example.com/${i})。\n`,
  `- 列表项 ${i}A\n- 列表项 ${i}B\n`,
  `| 列一 | 列二 |\n|---|---|\n| ${i} | ${i * 2} |\n`,
  '```js\nconst a' + i + ' = ' + i + '\nconsole.log(a' + i + ')\n```\n',
  `### 小标题 ${i}\n\n再写一句正文，把字数顶上去，方便触发切分。\n`,
].join('\n')

const buildRich = (n) => Array.from({ length: n }, (_, i) => unit(i)).join('\n')

const b = await Browser.launch({ width: 1280, height: 900 })
const page = await b.newPage({ width: 1280, height: 900 })
await page.goto(URL, { settleMs: 4500 })
for (let i = 0; i < 200; i++) { if (await page.eval(() => !!window.__store && !!window.__hydrate)) break; await wait(150) }

const SID = 'eq-1'
await page.eval((sid) => {
  const st = window.__store.getState()
  st.setNarrativeDensity('detailed')
  window.__store.setState({ activeSessionId: sid, blocks: [], turns: [], runNodes: [], runEnded: true })
}, SID)
await wait(300)

// 一条 user + 一条 assistant（runEnded，非流式）
const setText = async (text, streaming) => {
  await page.eval((arg) => {
    const now = Date.now()
    window.__store.setState({
      blocks: [
        { kind: 'user', id: 1, text: '开始', ts: now },
        { kind: 'assistant', id: 2, text: arg.text, streaming: arg.streaming, ts: now, model: 'probe' },
      ],
      runEnded: true,
    })
  }, { text, streaming })
  await wait(700)
}
const fp = () => page.eval(() => {
  window.__lastMd = null
  // 用户块也渲染 markdown —— 取**最后一个** .md（assistant 的正文）
  const md = [...document.querySelectorAll('.narrative .md')].pop()
  if (!md) return null
  const html = md.innerHTML
  let h = 0x811c9dc5
  for (let i = 0; i < html.length; i++) { h ^= html.charCodeAt(i); h = Math.imul(h, 0x01000193) }
  window.__lastMd = html
  const norm = html.replace(/>\s+</g, '><')
  let hn = 0x811c9dc5
  for (let i = 0; i < norm.length; i++) { hn ^= norm.charCodeAt(i); hn = Math.imul(hn, 0x01000193) }
  const geo = Array.from(md.children).map((el) => Math.round(el.getBoundingClientRect().height)).join(',')
  return {
    len: html.length, hash: (h >>> 0).toString(16),
    nlen: norm.length, nhash: (hn >>> 0).toString(16),
    nodes: md.querySelectorAll('*').length, text: md.textContent.replace(/\s+/g, '').length,
    mdH: Math.round(md.getBoundingClientRect().height), geo,
  }
})

const stableFp = async () => {
  let prev = null
  for (let i = 0; i < 8; i++) {
    await wait(350)
    const cur = await fp()
    if (prev && cur && prev.nhash === cur.nhash && prev.nodes === cur.nodes && prev.geo === cur.geo) return cur
    prev = cur
  }
  return prev
}

const cases = {
  '① 停在段落边界': buildRich(12),
  '② 停在列表中间': buildRich(12) + '\n- 还没写完的列表项 A\n- 还没写完的列表项 B\n',
  '③ 停在未闭合围栏': buildRich(12) + '\n```js\nconst半截 = 1\n\nconsole.log(半截)\n',
}
for (const [name, text] of Object.entries(cases)) {
  await setText(text, false)
  await wait(600)
  const a = await stableFp()
  const HTML_A = await page.eval(() => window.__lastMd)
  await setText(text, true)
  await wait(600)
  const b2 = await stableFp()
  const HTML_B = await page.eval(() => window.__lastMd)
  if (!a || !b2) { bad(`${name}: 没渲染出 .md`); continue }
  const sameStruct = a.nhash === b2.nhash && a.nodes === b2.nodes && a.text === b2.text
  const sameGeo = a.mdH === b2.mdH && a.geo === b2.geo
  if (sameStruct && sameGeo) {
    ok(`${name}: 与整段一致（${a.nodes} 元素 / ${a.text} 字 / 高 ${a.mdH}px；仅块间空白文本节点不同：${a.len}B vs ${b2.len}B）`)
    continue
  } else {
    bad(`${name}: 不一致`)
    console.log(`   结构: norm ${a.nlen}/${b2.nlen} 哈希 ${a.nhash}/${b2.nhash} 元素 ${a.nodes}/${b2.nodes} 字 ${a.text}/${b2.text}`)
    console.log(`   几何: 高 ${a.mdH}/${b2.mdH} 逐块 ${a.geo === b2.geo ? '相同' : a.geo + ' vs ' + b2.geo}`)
    const files = [`/tmp/eq-a-${name.length}-${a.len}.html`, `/tmp/eq-b-${name.length}-${b2.len}.html`]
    const { writeFileSync } = await import('node:fs')
    writeFileSync(files[0], HTML_A)
    writeFileSync(files[1], HTML_B)
    const A = HTML_A, B2 = HTML_B
    let d = 0
    while (d < Math.min(A.length, B2.length) && A[d] === B2[d]) d++
    console.log(`   首个差异位置 ${d}`)
    console.log(`   整段: …${JSON.stringify(A.slice(Math.max(0, d - 60), d + 60))}`)
    console.log(`   切分: …${JSON.stringify(B2.slice(Math.max(0, d - 60), d + 60))}`)
  }
}
console.log(fail ? `\nFAIL ${fail}` : '\nPASS 切分路径与整段路径逐字节一致')
await b.close?.()
