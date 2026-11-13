#!/usr/bin/env node
// Markdown 渲染回归（2026-09-11）：表格横滚 + 本轮排查出的 9 类渲染缺陷。
//
// 背景：用户反馈「宽表格滑不动也看不全」修好后，对 markdown.tsx / .md 样式做了一轮
// 系统排查，确认并修复以下缺陷（每条都先复现、再修、再由本脚本锁住）：
//   ① 宽表格溢出被叙述区裁掉且无处可滚（首轮修复，见 verify_wide_table.mjs 深度覆盖）
//   ② 单行无语言围栏 ``` \n ls -la \n ``` 被误判成行内代码（丢复制按钮/高亮/横滚）
//   ③ 围栏代码块被 react-markdown 的 <pre> 再包一层 → 选中/全选复制带上「语言 + 复制」头部
//   ④ .md code 边框/背景套到 shiki 内层 <code> 上（双重底色 + 多余内边距）
//   ⑤ a 覆盖透传 node prop → DOM 出现 node="[object Object]"；且锚点链接也被 target=_blank
//   ⑥ 列表标记被 Tailwind preflight 的 `ol,ul,menu{list-style:none}` 清掉（无圆点/无编号）
//   ⑦ 正文/列表/引用里的超长 token 溢出行盒 → 被叙述区裁掉
//   ⑧ 行内图片被 preflight `img{display:block}` 顶成独占一行（拆断段落）
//   ⑨ 脚注区 <h2 class="sr-only"> 未隐藏（「Footnotes」直接显示在正文里）
//   ⑩ 源码 tab 在代码变化后显示旧高亮（stale）
//   ⑪ 散文里的 $ 被误判为数学，且吞掉正文（「成本 $5 到 $10」→ 成本 [5 到]10）
//
// 用法：cd desktop/app && npm run build && node e2e/verify_markdown_render.mjs
// 退出码：全通过 0；任一断言失败非 0。
//
// 实现方式：用真实 Markdown 组件 + 真实 index.css 渲染一组对抗样本
// （临时探针页 e2e/_mdprobe，见下），再逐条断言 DOM/几何/计算样式。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = Number(process.env.PORT || 4185)

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

async function waitPort(port, timeoutMs = 25000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const up = await new Promise((resolve) => {
      const s = net.connect({ port, host: 'localhost' })
      const timer = setTimeout(() => { s.destroy(); resolve(false) }, 500)
      s.on('connect', () => { clearTimeout(timer); s.destroy(); resolve(true) })
      s.on('error', () => { clearTimeout(timer); resolve(false) })
    })
    if (up) return
    if (Date.now() > deadline) throw new Error('vite 未就绪')
    await new Promise((r) => setTimeout(r, 250))
  }
}

// 临时探针页：只挂真实 Markdown 组件 + 真实 index.css（跑完删除）
import fs from 'fs'
const PROBE_DIR = path.join(__dirname, '_mdprobe_run')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8"><title>md probe</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import { Markdown } from '../../src/components/markdown'
import '../../src/index.css'
const CASES = (window as unknown as { __CASES: Record<string, string> }).__CASES
createRoot(document.getElementById('root')!).render(
  <div className="narrative" style={{ width: 620, margin: '0 auto' }}>
    {Object.entries(CASES).map(([k, v]) => (
      <div key={k} data-case={k} className="md-text"><Markdown text={v} /></div>
    ))}
  </div>,
)
`)
fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [APP] } } })
`)

const PROBE_URL = `http://localhost:${PORT}/e2e/_mdprobe_run/index.html`
const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)

async function main() {
  await waitPort(PORT)
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 900, height: 1000 } })
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  page.on('console', (m) => {
    const t = m.text()
    // 忽略样本里的外部图片/网络噪音
    if (m.type() === 'error' && !/ERR_NAME_NOT_RESOLVED|Failed to load resource/.test(t)) errs.push(t.slice(0, 200))
  })

  const px = Buffer.from('<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16"><rect width="16" height="16" fill="#0f0"/></svg>').toString('base64')
  const CASES = {
    table: '| # | 接口 | 操作人来自 JWT | 写入业务行 | 企业表 <table> | 备注 |\n| --- | --- | --- | --- | --- | --- |\n| 3 | `<service>/agents/offline` | ⚠️ 仅 userID | modify_by/modify_name、release_user、editor_id | ❌ 断链 | 漏 TenantID |',
    narrowTable: '| a | b |\n| --- | --- |\n| 1 | 2 |',
    oneLineFence: '```\nls -la\n```',
    goFence: '```go\npackage main\n\nfunc main() {}\n```',
    link: '[外部](https://example.com/x)',
    anchor: '[锚点](#sec)',
    footnote: '文字[^1]\n\n[^1]: 脚注内容',
    ul: '- 甲\n- 乙',
    ulNested: '- 父\n  - 子',
    ol: '1. 第一\n2. 第二',
    task: '- [ ] 未完成\n- [x] 完成',
    longToken: '超长 token：' + 'A'.repeat(150) + ' 尾部',
    longInList: '- 超长 token：' + 'B'.repeat(150) + ' 尾部',
    longInQuote: '> 超长 token：' + 'C'.repeat(150) + ' 尾部',
    inlineImg: `前文 ![图标](data:image/svg+xml;base64,${px}) 后文`,
    dollarPrice: '成本 $5 到 $10 之间。',
    dollarShell: '执行 echo $PATH 与 $HOME 看看。',
    realMath: '公式 $E=mc^2$ 成立。',
    blockMath: '$$\nE = mc^2\n$$',
  }
  await page.addInitScript((c) => { window.__CASES = c }, CASES)
  await page.goto(PROBE_URL, { waitUntil: 'domcontentloaded' })
  await page.waitForSelector('[data-case="ul"] .md li', { timeout: 25000 })
  await page.waitForTimeout(2500) // 等 shiki 高亮

  const r = await page.evaluate(() => {
    const C = (n) => document.querySelector(`[data-case="${n}"]`)
    // 「内容是否溢出宿主宽度」：取元素盒子 + 文本 Range 二者的最右边界
    const over = (host) => {
      const hb = host.getBoundingClientRect()
      let mx = -Infinity
      host.querySelectorAll('*').forEach((c) => { const b = c.getBoundingClientRect(); if (b.width) mx = Math.max(mx, b.right) })
      const rg = document.createRange(); rg.selectNodeContents(host)
      ;[...rg.getClientRects()].forEach((x) => { mx = Math.max(mx, x.right) })
      return { overflowPx: Math.round(mx - hb.right), overflows: mx > hb.right + 1 }
    }
    const md = (n) => C(n).querySelector('.md')
    const out = {}
    // ① 表格横滚
    {
      const w = md('table').querySelector('.md-table-wrap')
      const t = w?.querySelector('table')
      const wrap = w ?? t?.parentElement
      out.table = {
        hasWrap: !!w,
        overflowX: wrap ? getComputedStyle(wrap).overflowX : null,
        scrollable: wrap ? wrap.scrollWidth > wrap.clientWidth + 1 : false,
        tableWiderThanWrap: t ? Math.round(t.getBoundingClientRect().width) > wrap.clientWidth : false,
        tableMargin: t ? getComputedStyle(t).marginTop : null,
      }
    }
    // ② 窄表不被牵连
    {
      const w = md('narrowTable').querySelector('.md-table-wrap')
      const t = w?.querySelector('table')
      out.narrowTable = { scrollable: w ? w.scrollWidth > w.clientWidth + 1 : null, tableW: t ? Math.round(t.getBoundingClientRect().width) : null, wrapW: w ? w.clientWidth : null }
    }
    // ③ 单行无语言围栏 → 代码块
    {
      const el = md('oneLineFence')
      out.oneLineFence = { codeblock: !!el.querySelector('.codeblock'), copyBtn: !!el.querySelector('.cb-copy'), stillPlainPre: !el.querySelector('.codeblock') && !!el.querySelector('pre') }
    }
    // ④ 代码块不被 <pre> 再包 + shiki 内层 code 无行内代码样式 + 选择不吞头部
    {
      const el = md('goFence')
      const cb = el.querySelector('.codeblock')
      const inner = cb?.querySelector('code')
      const rg = document.createRange(); rg.selectNodeContents(cb)
      const s = window.getSelection(); s.removeAllRanges(); s.addRange(rg)
      out.goFence = {
        nestedInPre: !!cb?.closest('pre'),
        innerCodeInlineStyled: inner ? (getComputedStyle(inner).backgroundColor !== 'rgba(0, 0, 0, 0)' || getComputedStyle(inner).borderTopWidth !== '0px') : null,
        selectedText: s.toString(),
      }
    }
    // ⑤ 链接
    {
      const a = md('link').querySelector('a')
      const b = md('anchor').querySelector('a')
      out.link = { externalTarget: a?.getAttribute('target'), rel: a?.getAttribute('rel'), nodeLeaked: !!a?.hasAttribute('node') }
      out.anchor = { target: b?.getAttribute('target'), href: b?.getAttribute('href') }
    }
    // ⑥ 脚注
    {
      const el = md('footnote')
      const sr = el.querySelector('.sr-only')
      out.footnote = {
        srHidden: sr ? (sr.getBoundingClientRect().width <= 2 && sr.getBoundingClientRect().height <= 2) : null,
        srText: sr?.textContent ?? null,
        refTarget: el.querySelector('a[data-footnote-ref]')?.getAttribute('target') ?? null,
        sectionStyled: (() => { const f = el.querySelector('.footnotes'); return f ? getComputedStyle(f).borderTopWidth !== '0px' : null })(),
      }
    }
    // ⑦ 列表标记
    out.ul = { listStyle: getComputedStyle(md('ul').querySelector('ul')).listStyleType, nestedListStyle: getComputedStyle(md('ulNested').querySelector('ul ul')).listStyleType }
    out.ol = { listStyle: getComputedStyle(md('ol').querySelector('ol')).listStyleType }
    out.task = { liListStyle: getComputedStyle(md('task').querySelector('li')).listStyleType, cbMarginRight: getComputedStyle(md('task').querySelector('input')).marginRight }
    // ⑧ 长 token
    out.longToken = over(md('longToken'))
    out.longInList = over(md('longInList'))
    out.longInQuote = over(md('longInQuote'))
    // ⑨ 行内图片
    { const img = md('inlineImg').querySelector('img'); const p = md('inlineImg').querySelector('p')
      out.inlineImg = { display: getComputedStyle(img).display, pHeight: Math.round(p.getBoundingClientRect().height) } }
    // ⑩ $ 假阳性
    out.dollarPrice = { katex: !!md('dollarPrice').querySelector('.katex'), text: md('dollarPrice').textContent.trim() }
    out.dollarShell = { katex: !!md('dollarShell').querySelector('.katex'), text: md('dollarShell').textContent.trim() }
    out.realMath = { katex: !!md('realMath').querySelector('.katex') }
    out.blockMath = { katex: !!md('blockMath').querySelector('.katex-display') }
    return out
  })

  // ---- ① 宽表格横滚 ----
  assert(r.table.hasWrap, '表格包在 .md-table-wrap 里')
  assert(r.table.overflowX === 'auto', `表格容器 overflow-x: auto（实际 ${r.table.overflowX}）`)
  assert(r.table.scrollable, '宽表在容器内可横滚（scrollWidth > clientWidth）')
  assert(r.table.tableWiderThanWrap, '表格保留自身宽度（不被压扁）')
  assert(r.table.tableMargin === '0px', '表格 margin 归零（间距移到容器）')
  // ---- ② 窄表不被牵连 ----
  assert(r.narrowTable.scrollable === false, '窄表不出现多余横条')
  assert(Math.abs(r.narrowTable.tableW - r.narrowTable.wrapW) <= 1, '窄表仍占满列宽')
  // ---- ③ 单行无语言围栏 ----
  assert(r.oneLineFence.codeblock, '单行无语言围栏渲染为代码块（此前误判为行内代码）')
  assert(r.oneLineFence.copyBtn, '单行围栏有复制按钮')
  // ---- ④ 代码块 DOM 与样式 ----
  assert(!r.goFence.nestedInPre, '代码块不再被补丁 <pre> 再包一层（pre 不能嵌 div）')
  assert(r.goFence.innerCodeInlineStyled === false, 'shiki 内层 <code> 不带行内代码样式（无双重底色/内边距）')
  assert(r.goFence.selectedText.includes('package main'), '选中代码块取到代码文本')
  assert(!/复制|^\s*go\s*$/m.test(r.goFence.selectedText), '选中/全选复制不含「语言标签 + 复制」头部 UI')
  // ---- ⑤ 链接 ----
  assert(r.link.externalTarget === '_blank' && r.link.rel === 'noreferrer', '外链新窗口打开且带 rel=noreferrer')
  assert(!r.link.nodeLeaked, 'node prop 未泄漏到 DOM（无 node="[object Object]"）')
  // 2026-09 起：标题 id 带 user-content- 前缀（与 hast-util-sanitize 的 clobber 前缀、GFM 脚注
  // 同一命名空间，见 markdown.tsx rehypeHeadingIds），所以站内锚点的 href 也同参改写 ——
  // 不改写就点不到（id 是 user-content-sec，href 是 #sec）。「保持同页跳转」这条本意不变。
  assert(r.anchor.target === null && r.anchor.href === '#user-content-sec', '站内锚点保持同页跳转（不被 target=_blank），且 href 与标题 id 同参')
  // ---- ⑥ 脚注 ----
  assert(r.footnote.srHidden === true, '脚注区 sr-only 标题已视觉隐藏（此前「Footnotes」直接可见）')
  assert(r.footnote.refTarget === null, '脚注回链不被 target=_blank')
  assert(r.footnote.sectionStyled === true, '脚注区有分隔线样式')
  // ---- ⑦ 列表标记 ----
  assert(r.ul.listStyle === 'disc', `无序列表恢复圆点（实际 ${r.ul.listStyle}）`)
  assert(r.ul.nestedListStyle === 'circle', '嵌套无序列表用 circle')
  assert(r.ol.listStyle === 'decimal', `有序列表恢复编号（实际 ${r.ol.listStyle}）`)
  assert(r.task.liListStyle === 'none', '任务列表项不显示圆点')
  assert(r.task.cbMarginRight === '6px', '任务列表勾选框有右间距')
  // ---- ⑧ 长 token ----
  assert(!r.longToken.overflows, `正文长 token 不溢出（溢出 ${r.longToken.overflowPx}px）`)
  assert(!r.longInList.overflows, `列表长 token 不溢出（溢出 ${r.longInList.overflowPx}px）`)
  assert(!r.longInQuote.overflows, `引用长 token 不溢出（溢出 ${r.longInQuote.overflowPx}px）`)
  // ---- ⑨ 行内图片 ----
  assert(r.inlineImg.display === 'inline-block', `行内图片恢复行内（实际 ${r.inlineImg.display}）`)
  assert(r.inlineImg.pHeight <= 40, `行内图片不拆断段落（段落高 ${r.inlineImg.pHeight}px）`)
  // ---- ⑩ $ 假阳性 ----
  assert(!r.dollarPrice.katex && r.dollarPrice.text === '成本 $5 到 $10 之间。', `货币 $ 不误判为数学且正文完整（实际「${r.dollarPrice.text}」）`)
  assert(!r.dollarShell.katex && r.dollarShell.text === '执行 echo $PATH 与 $HOME 看看。', `shell 变量 $ 不误判为数学且正文完整（实际「${r.dollarShell.text}」）`)
  assert(r.realMath.katex, '真行内公式 $E=mc^2$ 仍渲染')
  assert(r.blockMath.katex, '块级公式 $$…$$ 仍渲染')

  assert(errs.length === 0, `无页面/控制台报错${errs.length ? '：' + errs.join(' | ') : ''}`)

  await browser.close()
  cleanup()
  console.log(`\n[verify] Markdown 渲染回归全部通过（${pass} 项断言）`)
}

main().catch((e) => { console.error('FAIL', e); cleanup(); process.exit(1) })
