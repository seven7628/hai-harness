// CSS 对账：把皮肤层复刻的规则与设计源逐条比声明（值必须逐字相同）。
// 用法：node lab/css-diff.mjs
import { readFileSync } from 'node:fs'

const DESIGN = '/Users/admin/workspace/go-code/.go-code/worktrees/runtime-frontend-ui/design/agent-runtime/v2/ui.components.css'
const PREFIX = "html[data-skin='runtime'] "

const strip = (css) => css.replace(/\/\*[\s\S]*?\*\//g, '')

// 拆规则，丢掉 @media/@keyframes（外层），返回 [{sel, decls}]
function parse(css) {
  const out = []
  css = strip(css)
  let i = 0
  while (i < css.length) {
    const open = css.indexOf('{', i)
    if (open === -1) break
    const head = css.slice(i, open)
    let k = open, depth = 0
    while (k < css.length) {
      if (css[k] === '{') depth++
      else if (css[k] === '}') { if (--depth === 0) break }
      k++
    }
    const body = css.slice(open + 1, k)
    if (!/@media|@keyframes/.test(head)) out.push({ sel: head.trim(), body })
    i = k + 1
  }
  return out
}

const decls = (body) => {
  const m = {}
  for (const part of body.split(';')) {
    const c = part.indexOf(':')
    if (c === -1) continue
    m[part.slice(0, c).trim()] = part.slice(c + 1).replace(/\s+/g, ' ').trim()
  }
  return m
}

// 每份复刻文件要比对的 selectors；own = 我们自己加的规则（不参与对账）
const FILES = [
  {
    port: 'src/components/runtime/pop/pop.css',
    selectors: ['.pop', '.pop-label', '.pop-label b', '.pop-row', '.pop-row:hover', '.pop-row.is-active',
      '.pop-row.is-on', '.pop-row.is-on::before', '.pop-row .k', '.pop-row .m', '.pop-row svg', '.pop-row kbd',
      '.pop-foot', '.pop-search', '.pop-search svg', '.pop-search input', '.pop-search input::placeholder',
      ".pop[data-pop='cmd']", ".pop[data-pop='src']", '.pop .pop-search', '.pop-row.sk', '.pop-row.sk .m',
      '.pop-row.sk .k', '.pop-row.dir .k', '.pop-row.dir svg', '.pop-row.dir .m',
      ".chip.src[data-kind='dir'] svg", ".chip.src[data-kind='dir']"],
  },
  {
    port: 'src/components/runtime/fan/fan.css',
    selectors: ['.cy-imgs', '.cy-im', '.cy-im:first-child', ".cy-imgs[data-n='1'] .cy-im", '.cy-im img',
      '.cy-im:hover', '.cy-im:focus-visible', '.cy-imgs:hover .cy-im:not(:hover)', '.cy-lb', '.cy-lb[hidden]',
      '.cy-lb figure', '.cy-lb img', '.cy-lb figcaption', '.cy-lb-x', '.cy-lb-x:hover'],
  },
]

const design = parse(readFileSync(DESIGN, 'utf8'))
const bySel = new Map()
for (const r of design) {
  // 设计里同一条规则常把 :hover / .is-active 写在同一组选择器里 —— 拆开索引，
  // 否则复刻侧按单个选择器查永远查不到
  for (const sel of r.sel.split(',').map((x) => x.trim())) {
    if (!sel) continue
    bySel.set(sel, { ...(bySel.get(sel) || {}), ...decls(r.body) })
  }
}

let bad = 0, checked = 0
for (const f of FILES) {
  const port = new Map()
  for (const r of parse(readFileSync(f.port, 'utf8'))) {
    const sel = r.sel.split(',').map((s) => s.trim().replace(PREFIX, ''))
    for (const s of sel) {
      if (!s) continue
      port.set(s, { ...(port.get(s) || {}), ...decls(r.body) })
    }
  }
  for (const sel of f.selectors) {
    // :hover 在源里常与别的选择器同组，逐组展开过就不必再查
    const want = bySel.get(sel)
    const got = port.get(sel)
    if (!want) { console.log(`? ${f.port}: 设计里找不到 ${sel}`); bad++; continue }
    if (!got) { console.log(`✗ ${f.port}: 没有 ${sel}`); bad++; continue }
    checked++
    for (const [k, v] of Object.entries(got)) {
      // 位置块与 reduced-motion 是我们自己的决定；背景/阴影的二次覆写取最后一条
      // 位置/动效开关/焦点环是我们自己的决定（宿主锚点、减少动效、键盘可达性）
      if (['position', 'bottom', 'left', 'right', 'z-index', 'animation', 'transition', 'outline'].includes(k)) continue
      const d = want[k]
      if (d == null) { console.log(`+ ${f.port}: ${sel} 多了 ${k}: ${v}`); continue }
      if (d.replace(/\s/g, '') !== v.replace(/\s/g, '')) { console.log(`✗ ${sel} ${k}\n    设计 ${d}\n    复刻 ${v}`); bad++ }
    }
  }
}
console.log(`\n${checked} 条选择器对账，差异 ${bad}`)
process.exit(bad ? 1 : 0)
