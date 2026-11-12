#!/usr/bin/env node
// 上下文构成面板回归（Composer ctx 环 hover）。
//
// 起因：ctx 环此前只有 title 提示（「上下文占用（点击压缩）」），看不到占用构成——
// 用户要求 hover 时展开分区占比（Tools / Skills / MCP / 系统提示词 / 消息上下文 / 其他）。
// 2026-09-18 追加：底部缓存命中率**不能是会话累计**（那是状态栏「缓存」的口径，会被历史
// 轮次/缓存过期/子 agent 冷前缀拉低）——主行必须是「当前上下文」（最近一轮主 agent 调用
// cacheRead/input），会话累计降为次要行。
//
// 本脚本锁住的关键行为（每条都对应一个「实现错了就会静默退化」的点）：
//   ① hover ctx 环 → 面板出现（140ms 防误触延迟后），离开 → 面板消失
//   ② 七个分区行齐备 + 中文标签正确（系统提示词/技能/记忆/工具/MCP/消息上下文/其他）
//   ③ 行序按占用降序（大块先见）；占比之和 = 100.0%（取整余数补给最大分区）
//   ④ 分段条形图段数与占比一致（0 值分区不出段）
//   ⑤ 缓存命中率两行：主行 = 当前上下文（= 最近一轮 cacheRead/input），次行 = 会话累计
//      （= usage.cacheRead/usage.input）——两行数值都按 store 数据逐一对账，防「口径串台」
//   ⑥ 面板总量 = ctx 环总量（分区合计恒等于它；条形图不留空白）
//   ⑦ 点面板不误触「点击压缩」（面板是 ctx 的兄弟节点，不是子节点）
//   ⑧ 无会话时显示空态（不渲染假数据）
//   ⑨ 压缩后口径：锚点变估算（ctx 环读数带 ≈、面板说明标「压缩后估算」、
//      anchor_source=estimate），面板总量仍与 ctx 环一致（2026-09-20 追加）
//   ⑪ 缓存写入两行（2026-09-21 追加）：本轮写入 = 最近一轮主 agent 调用的 cache_write，
//      本会话写入 = usage.cacheWrite。只在端点上报写入量（Anthropic 协议）时渲染 ——
//      OpenAI/DeepSeek 系无该计数（恒 0）→ 保持两行，不出现「0 tok」噪声行。
//
// 2026-09-20 口径变更：静态分区（系统提示词/技能/工具/MCP/其他）按组装字节**固定**展示，
// 消息分区 = 锚点 − 静态合计（残差）。旧的「按真实用量等比缩放全部分区」已废弃（会把会话
// 内容摊派到静态块上，见 docs/CTX_ACCOUNTING_FIX_2026-09-20.md §1）——故本脚本断言的是
// 「静态分区不随锚点缩放」+「合计恒等于锚点」。
//
// 用法：cd desktop/app && node e2e/verify_ctx_panel.mjs
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'
import fs from 'fs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = Number(process.env.PORT || 4191)
const SHOTS = path.join(__dirname, '_ctxpanel_shots')

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

async function waitPort(port, timeoutMs = 30000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const up = await new Promise((resolve) => {
      const s = net.connect({ port, host: 'localhost' })
      const timer = setTimeout(() => { s.destroy(); resolve(false) }, 500)
      s.on('connect', () => { clearTimeout(timer); s.destroy(); resolve(true) })
      s.on('error', () => { s.destroy(); resolve(false) })
    })
    if (up) return
    if (Date.now() > deadline) throw new Error('vite 未就绪')
    await new Promise((r) => setTimeout(r, 250))
  }
}

// 临时探针页：挂真实 Composer + 真实 index.css（跑完删除）。
// store 走真实启动路径（start → mock transport，无 window.desktop），再发一轮 mock 对话
// 让 usage/turns/ctx 锚点都有真实数据（面板的两行缓存命中率按这些数据对账）。
// 探针目录按 pid 唯一：上一轮若还在收尾（cleanup 删目录），不会删到本轮刚写好的文件
const PROBE_DIR = path.join(__dirname, `_ctxprobe_run_${process.pid}`)
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8"><title>ctx panel probe</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import Composer from '../../src/components/Composer'
import { useAppStore } from '../../src/store/useAppStore'
import '../../src/index.css'

const WS = '/tmp/ctx-probe-ws'
;(window as unknown as { __store: unknown }).__store = useAppStore

createRoot(document.getElementById('root')!).render(
  <div style={{ display: 'flex', flexDirection: 'column', minHeight: '100vh', background: 'var(--bg)' }}>
    <div style={{ flex: '1 1 auto' }} />
    <Composer />
  </div>,
)

// 真实启动路径（mock transport）+ 建会话 + 跑一轮 mock 对话（mermaid 流程不弹提问 modal，
// 会发多条带 usage 的 llm_end → usage/turns/ctx 锚点齐备）。
void (async () => {
  await useAppStore.getState().start(WS)
  await useAppStore.getState().newSession()
  const t0 = Date.now()
  while (!useAppStore.getState().activeSessionId && Date.now() - t0 < 5000) {
    await new Promise((r) => setTimeout(r, 50))
  }
  useAppStore.getState().ask('画个 mermaid 架构图')
  const t1 = Date.now()
  while (useAppStore.getState().turns.length < 2 && Date.now() - t1 < 8000) {
    await new Promise((r) => setTimeout(r, 100))
  }
  ;(window as unknown as { __ready: boolean }).__ready = true
})()
`)
// 插件表与 vite.config.ts 一致（react + tailwind）：index.css 顶部是 @import 'tailwindcss'、
// 底部是三个皮肤层 @import（运行时色板/皮肤、V3 皮肤）。少了 tailwind 插件，Vite 的
// postcss-import 会把底部那三条当作非法位置的 @import 原样留下，浏览器直接忽略 ——
// 探针里切 data-skin='runtime' 就只剩默认皮肤色（2026-10 实测踩到）。
fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react(), tailwindcss()], server: { fs: { allow: [APP] } } })
`)

const PROBE_URL = `http://localhost:${PORT}/e2e/${path.basename(PROBE_DIR)}/index.html`
const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
server.stderr.on('data', (d) => { const s = String(d); if (!/ready in|Local:/.test(s)) process.stderr.write(s) })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)

const browser = await chromium.launch()
let failed = false
try {
  fs.mkdirSync(SHOTS, { recursive: true })
  const page = await browser.newPage({ viewport: { width: 1180, height: 760 }, deviceScaleFactor: 2 })
  page.on('pageerror', (e) => { console.error('  ⚠ pageerror:', e.message) })
  await page.goto(PROBE_URL, { waitUntil: 'load' })
  await page.waitForSelector('.composer .ctx', { timeout: 20000 })
  await page.waitForFunction('window.__ready === true', null, { timeout: 20000 })
  await page.waitForTimeout(400)

  // —— ① 初始：无面板 ——
  assert(await page.locator('.ctx-panel').count() === 0, '未 hover 时无面板')

  // —— ② hover → 面板出现（含防误触延迟）——
  await page.hover('.composer .ctx')
  await page.waitForSelector('.ctx-panel', { timeout: 3000 })
  ok('hover ctx 环 → 面板出现')

  const title = await page.locator('.ctx-panel-title').innerText()
  assert(title === '上下文容量', `面板标题 = ${title}`)

  // 头部总量与 ctx 环一致
  const hd = await page.locator('.ctx-panel-total').innerText()
  const ring = await page.locator('.composer .ctx').innerText()
  const hdNorm = hd.replace(/\s+/g, '').replace(/[()]/g, '')
  const ringNorm = ring.replace(/\s+/g, '')
  assert(hdNorm === ringNorm, `面板头部 = ctx 环文本（${hdNorm} vs ${ringNorm}）`)

  // —— ③ 七个分区行齐备 + 标签 + 降序 + 占比合计 100% ——
  const rows = await page.locator('.ctx-row').evaluateAll((els) => els.map((el) => ({
    name: el.querySelector('.ctx-name')?.textContent ?? '',
    tok: el.querySelector('.ctx-tok')?.textContent ?? '',
    pct: el.querySelector('.ctx-pct')?.textContent ?? '',
  })))
  assert(rows.length === 7, `分区行数 = ${rows.length}, want 7`)
  for (const n of ['消息上下文', '工具', 'MCP', '系统提示词', '技能', '记忆', '其他']) {
    assert(rows.some((r) => r.name === n), `分区行含「${n}」`)
  }
  const pcts = rows.map((r) => Number(r.pct.replace('%', '')))
  assert(pcts.every((p, i) => i === 0 || pcts[i - 1] >= p), `行序按占用降序: ${pcts.join(', ')}`)
  const sum = pcts.reduce((a, b) => a + b, 0)
  assert(Math.abs(sum - 100) < 0.3, `占比合计 = ${sum.toFixed(1)}% ≈ 100%`)

  // —— ④ 条形图段数 = 非零分区数；段宽按占比 ——
  const segs = await page.locator('.ctx-seg').evaluateAll((els) => els.map((el) => el.getBoundingClientRect().width))
  assert(segs.length === rows.filter((r) => r.tok !== '—').length, `条形段数 = ${segs.length}（与有值分区一致）`)
  assert(segs[0] > segs[1], `段宽随占比递减: ${segs.map((w) => w.toFixed(0)).join(' > ')}`)

  // —— ④b 面板不溢出 + 七段配色互不相同（2026-10 用户实测两处）——
  //    ① 说明文字画出面板右边界：面板挂在 .row2-right 里，那一层 white-space: nowrap
  //      （给 chip 用的）会被继承，说明文字于是不折行。判据取几何：说明文字的右缘不越过
  //      面板的**内容**右缘（面板 border-box 右缘 − 右内边距 − 右边框）。
  //    ② 七个圆点同色：分区色曾借工具族 --cat-*，而运行时皮肤把 --cat-* 统一退到弱字色
  //      → 7 段全灰。判据取计算色：七个圆点恰好七种颜色，且各自与面板底色的对比度达标
  //      （图形元素 WCAG 3:1；运行时那套自己取色，要求 4.5:1）。
  //    同一份读数在「默认皮肤 ink/paper」「运行时皮肤 ink/paper」四个组合上各验一次。
  const readCtxPalette = () => page.evaluate(() => {
    const parse = (s) => (s.match(/[\d.]+/g) ?? []).map(Number)
    const lum = ([r, g, b]) => {
      const f = (v) => { v /= 255; return v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4 }
      return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b)
    }
    const ratio = (a, b) => {
      const la = lum(a), lb = lum(b)
      return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05)
    }
    const panel = document.querySelector('.ctx-panel')
    const note = document.querySelector('.ctx-panel-note')
    const cs = getComputedStyle(panel)
    const bg = parse(cs.backgroundColor)
    const box = panel.getBoundingClientRect()
    const noteBox = note.getBoundingClientRect()
    // 溢出定位：取面板内「右缘越界最多」的那个后代（失败信息直接指向元凶）
    const children = [...panel.querySelectorAll('*')].map((el) => {
      const r = el.getBoundingClientRect()
      return { cls: String(el.className || el.tagName), over: r.right - (box.right - parseFloat(cs.paddingRight)) }
    })
    const widest = children.sort((a, b) => b.over - a.over)[0]
    const dots = [...panel.querySelectorAll('.ctx-dot')].map((el) => {
      const color = parse(getComputedStyle(el).backgroundColor)
      return {
        name: el.parentElement?.querySelector('.ctx-name')?.textContent?.trim() ?? '',
        color,
        contrast: ratio(color, bg),
      }
    })
    return {
      skin: document.documentElement.dataset.skin ?? '',
      theme: document.documentElement.dataset.theme ?? '',
      bg: cs.backgroundColor,
      clientW: panel.clientWidth,
      scrollW: panel.scrollWidth,
      contentRight: box.right - parseFloat(cs.paddingRight) - parseFloat(cs.borderRightWidth),
      noteRight: noteBox.right,
      noteClientW: note.clientWidth,
      noteScrollW: note.scrollWidth,
      widest,
      dots,
      uniq: new Set(dots.map((d) => d.color.join(','))).size,
      count: dots.length,
    }
  })
  const assertPalette = (pal, label, minContrast) => {
    assert(pal.scrollW <= pal.clientW + 1,
      `${label}：面板内容不横向溢出（scrollWidth ${pal.scrollW} ≤ clientWidth ${pal.clientW}；最宽 ${pal.widest?.cls} 越界 ${pal.widest?.over?.toFixed(1)}px）`)
    assert(pal.noteScrollW <= pal.noteClientW + 1,
      `${label}：底部说明在面板内折行（${pal.noteScrollW} ≤ ${pal.noteClientW}）`)
    assert(pal.noteRight <= pal.contentRight + 0.5,
      `${label}：说明文字右缘不越过内容右缘（${pal.noteRight.toFixed(1)} ≤ ${pal.contentRight.toFixed(1)}）`)
    assert(pal.count === 7 && pal.uniq === 7,
      `${label}：七个分区圆点颜色互不相同（${pal.uniq}/${pal.count}）`)
    const min = Math.min(...pal.dots.map((d) => d.contrast))
    assert(min >= minContrast,
      `${label}：分区色与面板底对比度 ≥ ${minContrast}:1（最小 ${min.toFixed(2)}:1，${pal.dots.find((d) => d.contrast === min)?.name}）`)
  }

  const baseInk = await readCtxPalette()
  assertPalette(baseInk, '默认皮肤 ink', 3)

  // —— ⑤ 缓存命中率两行：与 store 数据逐一对账（主行=最近一轮，次行=会话累计）——
  const store = await page.evaluate(() => {
    const s = window.__store.getState()
    const last = s.turns[s.turns.length - 1]
    return {
      turns: s.turns.length,
      lastRate: last && last.input > 0 ? (last.cacheRead / last.input) * 100 : null,
      sessionRate: s.usage.input > 0 ? (s.usage.cacheRead / s.usage.input) * 100 : null,
      ctxTokens: s.ctxTokens,
      breakdownTotal: s.ctxBreakdown?.total,
    }
  })
  const ftRows = await page.locator('.ctx-ft-row').evaluateAll((els) => els.map((el) => ({
    label: el.querySelector('span')?.textContent ?? '',
    value: el.querySelector('b, .mono:last-child')?.textContent ?? '',
  })))
  // 行数 = 2（两条命中率）+ 2（两条写入，仅 Anthropic 协议端点上报时渲染）
  assert(ftRows.length === 2 || ftRows.length === 4, `面板底部行数 = ${ftRows.length}, want 2 或 4（命中率 ± 写入）`)
  assert(ftRows[0].label.includes('当前上下文'), `主行标签 = ${ftRows[0].label}`)
  assert(ftRows[1].label.includes('本会话'), `次行标签 = ${ftRows[1].label}`)
  if (ftRows.length === 4) {
    assert(ftRows[2].label.includes('缓存写入（本轮）'), `第三行 = 本轮写入（实际 ${ftRows[2].label}）`)
    assert(ftRows[3].label.includes('缓存写入（本会话）'), `第四行 = 本会话写入（实际 ${ftRows[3].label}）`)
    const lastWrite = await page.evaluate(() => {
      const s = window.__store.getState()
      for (let i = s.turns.length - 1; i >= 0; i--) return s.turns[i].cacheWrite
    })
    assert(ftRows[2].value === `${lastWrite >= 1000 ? `${Math.round(lastWrite / 1000)}k` : lastWrite} tok`,
      `本轮写入 = 最近一轮 cache_write（面板 ${ftRows[2].value} vs store ${lastWrite}）`)
  }
  const shownNow = Number(ftRows[0].value.replace('%', ''))
  const shownSession = Number(ftRows[1].value.replace('%', ''))
  assert(Math.abs(shownNow - store.lastRate) < 0.1, `主行 = 最近一轮命中率（面板 ${shownNow}% vs store ${store.lastRate?.toFixed(1)}%）`)
  assert(Math.abs(shownSession - store.sessionRate) < 0.1, `次行 = 会话累计命中率（面板 ${shownSession}% vs store ${store.sessionRate?.toFixed(2)}%）`)
  assert(/^\d+\.\d{2}%$/.test(ftRows[0].value) && /^\d+\.\d{2}%$/.test(ftRows[1].value),
    `两行均为两位小数（与状态栏同格式）：${ftRows[0].value} / ${ftRows[1].value}`)

  // —— ⑥ 面板总量 = ctx 环总量（分区合计 = 真实锚点，条形图不留空白）——
  assert(store.breakdownTotal === store.ctxTokens, `面板总量 = ctx 环（${store.breakdownTotal} vs ${store.ctxTokens}）`)

  // —— ⑥b 用户实测场景（2026-09-18）：会话累计远低于当前上下文（历史轮次/缓存过期拖累）——
  // 注入「累计 24.0% / 最近一轮 96.0%」→ 主行必须是 96.0%（当前上下文），不是 24.0%（总）。
  await page.mouse.move(60, 60)
  await page.waitForTimeout(120)
  await page.evaluate(() => {
    const s = window.__store.getState()
    window.__store.setState({
      usage: { input: 1000000, output: 5000, cacheRead: 240000, cacheWrite: 250000, reasoning: 0 },
      turns: [...s.turns, { model: 'probe', input: 1000, output: 10, cacheRead: 960, cacheWrite: 40, reasoning: 0, costUsd: 0 }],
    })
  })
  await page.hover('.composer .ctx')
  await page.waitForSelector('.ctx-panel', { timeout: 3000 })
  const two = await page.locator('.ctx-ft-row').evaluateAll((els) => els.map((el) => ({
    label: el.querySelector('span')?.textContent ?? '',
    value: el.querySelector('b, .mono:last-child')?.textContent ?? '',
  })))
  assert(two[0].value === '96.00%', `主行 = 当前上下文 96.00%（实际 ${two[0].value}）`)
  assert(two[1].value === '24.00%', `次行 = 会话累计 24.00%（实际 ${two[1].value}）`)
  assert(two[0].value !== two[1].value, '两行口径不同源（主行不是会话累计）')
  // 写入两行：命中率之外的第二个信号 —— 前缀被整段重写时命中率会掉、写入量会顶上去，
  // 两者必须各自对账到 store（本轮 = 最后一轮 turn.cacheWrite，累计 = usage.cacheWrite）。
  assert(two.length === 4, `注入写入量后底部行数 = ${two.length}, want 4（2 命中率 + 2 写入）`)
  assert(two[2].label.includes('缓存写入（本轮）') && two[2].value === '40 tok',
    `本轮写入 = 40 tok（实际 ${two[2].label} / ${two[2].value}）`)
  assert(two[3].label.includes('缓存写入（本会话）') && two[3].value === '250k tok',
    `本会话写入 = 250k tok（实际 ${two[3].label} / ${two[3].value}）`)

  await page.screenshot({ path: path.join(SHOTS, 'ctx-panel.png') })

  // —— 亮色主题（宣纸）：同一面板两套主题都必须可读（配色走主题变量）——
  await page.evaluate(() => { document.documentElement.dataset.theme = 'paper' })
  await page.mouse.move(60, 60)
  await page.waitForTimeout(120)
  await page.hover('.composer .ctx')
  await page.waitForSelector('.ctx-panel', { timeout: 3000 })
  await page.screenshot({ path: path.join(SHOTS, 'ctx-panel-paper.png') })
  assert(await page.locator('.ctx-row').count() === 7, '亮色主题下面板同样渲染七行')
  assertPalette(await readCtxPalette(), '默认皮肤 paper', 3)

  // —— ④c 运行时皮肤两档主题：那套基色把 --cat-* 统一退到弱字色（工具行不该有色），
  //    分区色因此必须在 runtime-palette.css 里另取一套。皮肤由 html 属性驱动，
  //    直接切属性即可（不必改设置）。面板底色也一并断言：它是运行时基色（#101015 /
  //    #f0efec），能证明色板层真的加载了，而不是悄悄退回默认皮肤。 ——
  const runtimeBg = { ink: 'rgb(16, 16, 21)', paper: 'rgb(240, 239, 236)' }
  // 属性值写完还要活过一次依赖预构建/HMR 的整页 reload（那会把 <html> 上的属性全部清掉），
  // 所以「写 → 等 → 复核」；面板若在中途被 reload 掉就重开一次。
  const applyRuntime = async (theme) => {
    for (let i = 0; i < 6; i++) {
      await page.evaluate((t) => {
        document.documentElement.dataset.skin = 'runtime'
        document.documentElement.dataset.theme = t
      }, theme)
      await page.waitForTimeout(150)
      if (await page.evaluate(() => document.documentElement.dataset.skin === 'runtime'
        && document.documentElement.dataset.theme !== '')) return
    }
  }
  for (const theme of ['ink', 'paper']) {
    for (let attempt = 0; attempt < 3; attempt++) {
      await applyRuntime(theme)
      await page.mouse.move(60, 60)
      await page.waitForTimeout(150)
      await page.hover('.composer .ctx')
      try {
        await page.waitForSelector('.ctx-panel', { timeout: 3000 })
      } catch { continue }
      await applyRuntime(theme) // hover 期间若发生 reload，属性会丢：再落一次
      break
    }
    const pal = await readCtxPalette()
    assert(pal.skin === 'runtime', `运行时皮肤 ${theme}：data-skin 保持 runtime（实际 ${pal.skin || '空'}）`)
    assert(pal.theme === theme, `运行时皮肤 ${theme}：data-theme = ${theme}（实际 ${pal.theme || '空'}）`)
    assert(pal.bg === runtimeBg[theme], `运行时皮肤 ${theme}：面板底色 = ${runtimeBg[theme]}（实际 ${pal.bg}）`)
    assertPalette(pal, `运行时皮肤 ${theme}`, 4.5)
    await page.screenshot({ path: path.join(SHOTS, `ctx-panel-runtime-${theme}.png`) })
  }
  await page.evaluate(() => {
    document.documentElement.dataset.skin = 'current'
    document.documentElement.dataset.theme = 'ink'
  })
  await page.mouse.move(60, 60)
  await page.waitForTimeout(120)
  await page.hover('.composer .ctx')
  await page.waitForSelector('.ctx-panel', { timeout: 3000 })

  // —— ⑦ 点面板不误触「点击压缩」 ——
  await page.locator('.ctx-panel-title').click()
  await page.waitForTimeout(150)
  assert(await page.locator('.compact-confirm').count() === 0, '点面板不触发压缩确认')

  // —— ⑩ 静态分区固定（2026-09-20 口径）：再跑一轮 → 锚点/会话内容变化，静态分区
  //（工具/系统提示词/技能/MCP/其他）数值必须逐字不变，只有消息上下文跟着动。 ——
  const readRows = async () => {
    await page.mouse.move(60, 60)
    await page.waitForTimeout(150)
    await page.hover('.composer .ctx')
    await page.waitForSelector('.ctx-panel', { timeout: 3000 })
    const rows = await page.locator('.ctx-row').evaluateAll((els) => els.map((el) => ({
      name: el.querySelector('.ctx-name')?.textContent ?? '',
      tok: el.querySelector('.ctx-tok')?.textContent ?? '',
    })))
    return Object.fromEntries(rows.map((r) => [r.name, r.tok]))
  }
  const beforeRows = await readRows()
  await page.evaluate(() => window.__store.getState().ask('再画一张时序图'))
  await page.waitForFunction(
    (n) => window.__store.getState().turns.length > n,
    await page.evaluate(() => window.__store.getState().turns.length),
    { timeout: 20000 })
  // 等这一轮彻底跑完（turns 数稳定）：否则后续轮的 llm_end 会在 ⑨（压缩后估算锚点）断言
  // 之前把 ≈ 覆盖成真实口径，把断言打乱。
  const waitQuiet = async (idleMs = 800) => {
    let last = -1
    for (;;) {
      const n = await page.evaluate(() => window.__store.getState().turns.length)
      if (n === last) return
      last = n
      await page.waitForTimeout(idleMs)
    }
  }
  await waitQuiet()
  // 分区数据是 hover 时异步拉的（fetchCtxBreakdown），而 ctx 环读数来自 store 即时值：
  // 直接读 store 有概率读到「上一轮的总量」（实测 400 vs 1600 的假失败）。这里等两者
  // 收敛再断言——超时也照样往下走，由断言报出实际值，不掩盖真正的口径不一致。
  const waitBreakdownSettled = async (timeout = 4000) => {
    const deadline = Date.now() + timeout
    for (;;) {
      const settled = await page.evaluate(() => {
        const s = window.__store.getState()
        if (s.ctxBreakdown != null && s.ctxBreakdown.total === s.ctxTokens) return true
        // hover 只在面板打开时拉一次，可能早于本轮 llm_end（锚点还是上一轮的）→ 主动重拉
        s.fetchCtxBreakdown()
        return false
      })
      if (settled) return
      if (Date.now() > deadline) return // 超时交给断言报实际值，不掩盖真不一致
      await page.waitForTimeout(250)
    }
  }
  await waitBreakdownSettled()
  const afterRows = await readRows()
  for (const label of ['工具', '系统提示词', '技能', 'MCP', '其他']) {
    assert(afterRows[label] === beforeRows[label],
      `静态分区「${label}」不随会话变化（${beforeRows[label]} → ${afterRows[label]}）`)
  }
  const invariant = await page.evaluate(() => {
    const s = window.__store.getState()
    const b = s.ctxBreakdown
    const parts = Object.fromEntries((b?.parts ?? []).map((p) => [p.key, p.tokens]))
    const stat = ['system_prompt', 'skills', 'tools', 'mcp', 'other'].reduce((a, k) => a + (parts[k] ?? 0), 0)
    return { total: b?.total, ctx: s.ctxTokens, msg: parts.messages, stat }
  })
  assert(invariant.total === invariant.ctx, `总量仍与 ctx 环一致（${invariant.total} vs ${invariant.ctx}）`)
  assert(invariant.msg === Math.max(0, invariant.total - invariant.stat),
    `消息分区 = 总量 − 静态合计（${invariant.msg} vs ${invariant.total - invariant.stat}）`)

  // —— ⑨ 压缩后锚点 = 估算口径（2026-09-20）：ctx 环读数加 ≈ 前缀，面板说明改用
  // 「总量为压缩后估算」文案 —— 压后 ctx_tokens 来自 SDK 字符估算，不是 provider 真实
  // usage，UI 必须显式区分（否则估算会被当成真实占用）。 ——
  await page.mouse.move(60, 60)
  await page.waitForTimeout(150)
  await page.locator('.composer .ctx').click()
  await page.waitForSelector('.compact-card', { timeout: 3000 })
  await page.locator('.compact-card .btn.primary').click()
  await page.waitForFunction(() => window.__store.getState().ctxTokensEstimated === true, null, { timeout: 5000 })
  await page.mouse.move(60, 60)
  await page.waitForTimeout(150)
  await page.hover('.composer .ctx')
  await page.waitForSelector('.ctx-panel', { timeout: 3000 })
  const ringEst = (await page.locator('.composer .ctx').innerText()).replace(/\s+/g, '')
  assert(ringEst.includes('≈'), `压缩后 ctx 环读数带 ≈（实际 ${ringEst}）`)
  const ringTitleEst = await page.locator('.composer .ctx').getAttribute('title')
  assert((ringTitleEst ?? '').includes('估算'), `压缩后 ctx 环 title 说明估算（实际 ${ringTitleEst}）`)
  const noteEst = await page.locator('.ctx-panel-note').innerText()
  assert(noteEst.includes('压缩后估算'), `面板说明 = 压缩后估算口径（实际 ${noteEst}）`)
  await waitBreakdownSettled()
  const est = await page.evaluate(() => {
    const s = window.__store.getState()
    return { ctx: s.ctxTokens, total: s.ctxBreakdown?.total, src: s.ctxBreakdown?.anchorSource, degraded: s.ctxBreakdown?.degraded, est: s.ctxBreakdown?.estimated }
  })
  assert(est.src === 'estimate' && est.est === true, `anchor_source = estimate / estimated = true（实际 ${est.src}/${est.est}）`)
  assert(est.degraded === false, '压缩后锚点仍高于静态前缀合计（不 degraded）')
  assert(est.total === est.ctx, `面板总量仍与 ctx 环一致（${est.total} vs ${est.ctx}）`)
  await page.screenshot({ path: path.join(SHOTS, 'ctx-panel-estimated.png') })

  // —— ① 离开 → 面板消失 ——
  await page.mouse.move(60, 60)
  // 关闭是 500ms 防误触延迟（Composer.closeCtxPanel），200ms 内面板本来还在
  await page.waitForFunction(() => !document.querySelector('.ctx-panel'), null, { timeout: 2000 })
  assert(await page.locator('.ctx-panel').count() === 0, '移开后面板消失')

  // —— ⑧ 无会话：空态（清掉会话后再 hover）——
  await page.evaluate(() => {
    window.__store.setState({ activeSessionId: undefined, ctxBreakdown: undefined })
  })
  await page.hover('.composer .ctx')
  await page.waitForSelector('.ctx-panel', { timeout: 3000 })
  const empty = await page.locator('.ctx-empty').innerText()
  assert(empty === '暂无会话', `无会话空态 = ${empty}`)
  assert(await page.locator('.ctx-row').count() === 0, '空态不渲染假数据行')

  console.log(`\n✅ 全部通过（${pass} 项）；截图：${path.relative(APP, path.join(SHOTS, 'ctx-panel.png'))}`)
} catch (e) {
  failed = true
  console.error(`\n❌ ${e.message}`)
} finally {
  await browser.close()
  cleanup()
}
process.exit(failed ? 1 : 0)
