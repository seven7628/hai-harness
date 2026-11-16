// DOM 指纹：证明「纯性能改动」没有动任何可见/可交互的东西。
//
// 一次采样按 皮肤 × 密度 输出四组指纹：
//   ① innerHTML 的哈希与长度 —— 标签、属性、顺序、文本全算进去（差一个 class 就会变）
//   ② 元素按 tag.class 的计数表（排序后哈希）—— 结构变化的第二道网
//   ③ 采样元素的计算样式（display/height/color/background/font-size/padding/margin/
//      content-visibility/overflow）—— 抓「结构没变但样子变了」
//   ④ 关键交互属性：data-*（skin/density/open/live/foot）、可点元素数量、文本长度
//
// 用法：FP_OUT=/tmp/fp-before.json node lab/probe-dom-fingerprint.mjs
//       FP_OUT=/tmp/fp-after.json  node lab/probe-dom-fingerprint.mjs
//       diff <(...) <(...)  —— 两次必须完全一致
import { readFileSync, writeFileSync } from 'node:fs'
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const CP_PATH = process.env.CP_PATH || '/tmp/real-cp-settled.json'
const OUT = process.env.FP_OUT || '/tmp/fp.json'
const cp = JSON.parse(readFileSync(CP_PATH, 'utf8'))
const wait = (ms) => new Promise((r) => setTimeout(r, ms))

const b = await Browser.launch({ width: 1280, height: 800 })
const page = await b.newPage({ width: 1280, height: 800 })
await page.goto(URL, { settleMs: 4500 })
for (let i = 0; i < 200; i++) { if (await page.eval(() => !!window.__store && !!window.__hydrate)) break; await wait(150) }

const SID = 'fp-1'
const snap = async (skin, density) => {
  await page.eval((arg) => {
    const st = window.__store.getState()
    const views = { ...st.views }
    delete views[arg.sid]
    window.__store.setState({ views, activeSessionId: undefined, blocks: [], runNodes: [], runEnded: true })
    st.setNarrativeDensity(arg.d)
  }, { sid: SID, skin, d: density })
  await wait(300)
  await page.eval(({ cp, sid }) => window.__hydrate(sid, cp, undefined, { live: true }), { cp, sid: SID })
  await page.eval((sid) => window.__store.setState((s) => {
    const v = s.views[sid]
    return { activeSessionId: sid, blocks: v.blocks, blocksTotal: v.blocksTotal, blocksCursor: v.blocksCursor, blocksHasMore: v.blocksHasMore, runNodes: v.runNodes, usage: v.usage, turns: v.turns, costUsd: v.costUsd, runEnded: v.runEnded, turnsTotal: v.turnsTotal, todos: v.todos, diffs: v.diffs, agentDetached: v.agentDetached, approvals: v.approvals, workspace: '/mock/workspace' }
  }), SID)
  await wait(900)
  return page.eval(() => {
    const root = document.querySelector('.narrative')
    const html = root.innerHTML
    // FNV-1a 32 位：够用且不依赖 crypto
    const hash = (s) => { let h = 0x811c9dc5; for (let i = 0; i < s.length; i++) { h ^= s.charCodeAt(i); h = Math.imul(h, 0x01000193) } return (h >>> 0).toString(16) }
    const all = Array.from(root.querySelectorAll('*'))
    const tally = {}
    for (const el of all) {
      const c = el.className && typeof el.className === 'string' ? '.' + el.className.trim().split(/\s+/).sort().join('.') : ''
      const k = el.tagName.toLowerCase() + c
      tally[k] = (tally[k] || 0) + 1
    }
    const tallyKeys = Object.keys(tally).sort()
    const tallyStr = tallyKeys.map((k) => k + '×' + tally[k]).join('|')
    // 采样样式：每隔 37 个取一个，读一组「看得出差异」的属性
    const styles = []
    for (let i = 0; i < all.length; i += 37) {
      const el = all[i]
      const s = getComputedStyle(el)
      const r = el.getBoundingClientRect()
      styles.push([i, Math.round(r.width), Math.round(r.height), s.display, s.color, s.backgroundColor, s.fontSize, s.fontWeight, s.paddingTop, s.paddingLeft, s.marginTop, s.borderTopWidth, s.contentVisibility, s.overflowY, s.opacity, s.visibility].join(','))
    }
    const clickables = root.querySelectorAll('button, [role="button"], a').length
    const turns = Array.from(root.querySelectorAll('.rt-turn')).map((t) => [t.dataset.density, t.dataset.open, t.dataset.live, t.dataset.foot].join('/')).join(' ')
    return {
      htmlLen: html.length, htmlHash: hash(html),
      tallyHash: hash(tallyStr), tallyCount: tallyKeys.length, nodeCount: all.length,
      styleRows: styles.length, stylesHash: hash(styles.join('\n')),
      clickables, turnsAttr: turns, textLen: root.textContent.length,
      scrollH: Math.round(root.scrollHeight),
    }
  })
}

const out = {}
for (const density of ['concise', 'detailed']) {
  for (const skin of ['current', 'runtime']) {
    out[`${skin}/${density}`] = await snap(skin, density)
  }
}
writeFileSync(OUT, JSON.stringify(out, null, 2))
for (const [k, v] of Object.entries(out)) {
  console.log(`${k.padEnd(18)} html ${String(v.htmlLen).padStart(6)}B #${v.htmlHash}  nodes ${String(v.nodeCount).padStart(6)}  tally #${v.tallyHash}  styles #${v.stylesHash} (${v.styleRows})  btn ${v.clickables}  scrollH ${v.scrollH}`)
}
console.log('\nturns attrs:')
for (const [k, v] of Object.entries(out)) console.log(`  ${k.padEnd(18)} ${v.turnsAttr}`)
await b.close?.()
