#!/usr/bin/env node
// 使用统计回归（设置 →「数据与统计 → 使用统计」+ 空会话近 90 天图表）。
//
// 背景（2026-09-19 用户决策）：右侧栏「指标」tab 下线，跨会话用量迁到设置页独立模块，
// 并在新建会话（无任何消息）中部展示近 90 天热力图。本脚本锁住的关键行为 ——
//   ① 空会话中部：近 90 天热力图 = 5 行 × 18 天（横向扁平、水平居中、空态整块垂直居中），
//      格子按时间从左到右排列，hover 把当日/累计内联到标题下一行
//   ② 设置导航：分组标题（模型 / Agent 能力 / 数据与统计）+「使用统计」入口可进入
//   ③ 指标卡 7 张，数值与 `metrics` 报告逐项对账（累计 / 峰值 / 最长聊天 / 连续天数 / 成本 /
//      无价调用占比 2026-09-23），无价卡 title 定位到具体模型（补价入口）
//   ④ 热力图 53 列 × 7 格 + ≥11 个月份刻度；切「累计」后末格 = 全量累计（CumTokens）
//   ⑤ 「近 7 日」→ x 轴 7 个刻度（同一份报告切窗口，不发新命令）
//   ⑥ 中英双语标题随语言切换（useT/useLang 生效）
//
// 数据源 = mock transport 的 metrics 报告（真 bridge 由 ~/.go-code/metrics.jsonl 聚合，
// 口径由 Go 单测 desktop/bridge/metrics_test.go 覆盖）。
//
// 用法：cd desktop/app && node e2e/verify_usage_stats.mjs
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { freePort, waitHttp } from './_serve.mjs'
import { spawn } from 'child_process'
import { build } from 'esbuild'
import path from 'path'
import net from 'net'
import fs from 'fs'
import { fileURLToPath } from 'url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
// 端口由内核分配（见 e2e/_serve.mjs 的注释）：固定端口在链里会撞上一个还没退干净的服务器。
const PORT = Number(process.env.PORT) || await freePort()
const SHOTS = path.join(__dirname, '_usagestats_shots')

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

// 1) 预期值直接用页面同款纯函数层算（esbuild 打包成 cjs 后 require）——
//    避免在测试里重写一份格式化规则（那只会把 bug 复制两份）。
const bundleDir = fs.mkdtempSync(path.join(APP, 'e2e', '_usagestats_bundle_'))
const bundleOut = path.join(bundleDir, 'usage-stats.cjs')
await build({
  entryPoints: [path.join(APP, 'src/lib/usageStats.ts')],
  bundle: true, format: 'cjs', platform: 'node', outfile: bundleOut, logLevel: 'silent',
})
const require_ = (await import('module')).createRequire(import.meta.url)
const U = require_(bundleOut)

// 2) 临时探针页：挂真实 SettingsModal + Narrative（真实 store 启动路径 + mock transport）
const PROBE_DIR = path.join(__dirname, '_usagestatsprobe_run')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8"><title>usage stats probe</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import SettingsModal from '../../src/components/SettingsModal'
import Narrative from '../../src/components/Narrative'
import { useAppStore } from '../../src/store/useAppStore'
import '../../src/index.css'

const WS = '/tmp/usage-stats-probe-ws'
;(window as unknown as { __store: unknown }).__store = useAppStore

createRoot(document.getElementById('root')!).render(
  <div style={{ height: '100vh', display: 'flex', flexDirection: 'column', background: 'var(--bg)' }}>
    {/* 与 CenterPanel 同构：.narrative-wrap（flex 列 + min-height:0）里装 .narrative，
        否则 .narrative 高度退化成内容高，空态垂直居中（auto 外边距）测不出来 */}
    <div style={{ flex: '1 1 auto', display: 'flex', flexDirection: 'column', minHeight: 0, overflow: 'hidden' }}>
      <Narrative />
    </div>
    <SettingsModal />
  </div>,
)

void (async () => {
  await useAppStore.getState().start(WS)
  // 空会话图表走 ensureMetrics（60s 缓存）：等待报告落地
  const t0 = Date.now()
  while (!(useAppStore.getState().metrics?.ByDay?.length > 0) && Date.now() - t0 < 8000) {
    await new Promise((r) => setTimeout(r, 100))
  }
  ;(window as unknown as { __ready: boolean }).__ready = true
})()
`)
fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [APP] } } })
`)

const PROBE_URL = `http://localhost:${PORT}/e2e/_usagestatsprobe_run/index.html`
const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
server.stderr.on('data', (d) => { const s = String(d); if (!/ready in|Local:/.test(s)) process.stderr.write(s) })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
  try { fs.rmSync(bundleDir, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)

await waitPort(PORT)
// 端口可连只说明**有东西**在监听；再确认这张探针页真能开（子进程退出即失败）
await waitHttp(PROBE_URL, { child: server })
const browser = await chromium.launch()
let failed = false
try {
  fs.mkdirSync(SHOTS, { recursive: true })
  const page = await browser.newPage({ viewport: { width: 1280, height: 860 }, deviceScaleFactor: 2 })
  page.on('pageerror', (e) => { console.error('  ⚠ pageerror:', e.message) })
  await page.goto(PROBE_URL, { waitUntil: 'load' })
  await page.waitForFunction('window.__ready === true', null, { timeout: 30000 })

  const report = await page.evaluate(() => window.__store.getState().metrics)
  assert(Boolean(report?.Stats), 'metrics 报告落地（含 Stats）')

  // —— ① 空会话中部：近 90 天 Token 活动热力图（5 行 × 18 天，横向扁平且居中）——
  console.log('\n① 空会话近 90 天热力图')
  await page.waitForSelector('.empty .us-mini', { timeout: 10000 })
  const miniHd = await page.locator('.empty .us-mini-hd').innerText()
  assert(miniHd.includes('近 90 天 Token 活动'), `图表标题 = ${miniHd.split('\n')[0]}`)
  const mini = await page.evaluate(() => {
    const rows = [...document.querySelectorAll('.empty .us-mini .us-heat-row')]
    const cells = [...document.querySelectorAll('.empty .us-mini .us-heat .us-heat-cell')]
    const box = document.querySelector('.empty .us-mini .us-heat').getBoundingClientRect()
    const cell0 = cells[0].getBoundingClientRect()
    const card = document.querySelector('.empty .us-mini').getBoundingClientRect()
    const empty = document.querySelector('.empty').getBoundingClientRect()
    const wrap = document.querySelector('.narrative').getBoundingClientRect()
    return {
      rows: rows.length,
      perRow: rows.map((r) => r.querySelectorAll('.us-heat-cell').length),
      total: cells.length,
      placeholder: cells.filter((c) => c.classList.contains('before') || c.classList.contains('future')).length,
      keys: cells.map((c) => c.dataset.key).filter(Boolean),
      box: { w: box.width, h: box.height },
      cell: { w: cell0.width, h: cell0.height },
      padLeft: box.left - card.left,
      padRight: card.right - box.right,
      above: empty.top - wrap.top,
      below: wrap.bottom - empty.bottom,
    }
  })
  assert(mini.rows === 5, `行数 = 5（实际 ${mini.rows}）`)
  assert(mini.perRow.every((n) => n === 18), `每行 18 天（实际 ${mini.perRow.join('/')}）`)
  assert(mini.total === 90, `网格 = 90 格（实际 ${mini.total}）`)
  assert(mini.placeholder === 0, `90 天正好铺满、无占位格（实际占位 ${mini.placeholder}）`)
  const flatRatio = mini.box.w / mini.box.h
  assert(flatRatio > 2.6, `横向扁平：网格宽高比 ${flatRatio.toFixed(2)}（> 2.6，不再是近正方形）`)
  assert(Math.abs(mini.cell.w - mini.cell.h) < 1.5, `格子仍是正方形（${mini.cell.w.toFixed(1)}×${mini.cell.h.toFixed(1)}）`)
  assert(Math.abs(mini.padLeft - mini.padRight) < 2, `网格水平居中（左 ${mini.padLeft.toFixed(1)} / 右 ${mini.padRight.toFixed(1)}）`)
  assert(Math.abs(mini.above - mini.below) < 12, `空态在可用高度里垂直居中（上 ${mini.above.toFixed(0)} / 下 ${mini.below.toFixed(0)}）`)
  // 时间从左到右、从上到下：日期严格递增，窗口 = 今天往前 90 天（末格 = 今天）
  const miniToday = U.dayKeyOf(new Date())
  assert(mini.keys.length === 90 && mini.keys[89] === miniToday, `末格 = 今天（${miniToday}）`)
  assert(mini.keys[0] === U.dayKeyOf(U.addDays(new Date(), -89)), `首格 = 89 天前（${mini.keys[0]}）`)
  assert(mini.keys.every((k, i) => i === 0 || k > mini.keys[i - 1]), '格子顺序 = 时间从左到右、从上到下')
  const miniTotal = await page.evaluate(() => {
    const r = window.__store.getState().metrics
    const keys = new Set(r.ByDay.slice(-90).map((d) => d.Key))
    return r.ByDay.filter((d) => keys.has(d.Key)).reduce((a, d) => a + d.Input + d.Output, 0)
  })
  const miniHdText = miniHd.replace(/\s+/g, '')
  assert(miniHdText.includes(U.fmtBigTokens(miniTotal, 'zh').replace(/\s+/g, '')), `近 90 天合计对账（${U.fmtBigTokens(miniTotal, 'zh')}）`)
  const miniRange = await page.locator('.empty .us-mini-range').innerText()
  assert(/-|–/.test(miniRange), `范围文案存在（${miniRange}）`)
  assert(await page.locator('.empty .us-mini .us-tick').count() === 0, '空态已无折线（无 x 轴刻度）')
  assert(await page.locator('.empty .us-mini .us-heat-col').count() === 0, '空态用「每行 18 天」排布（非列 = 周）')
  // 空态热力图 hover：信息内联在标题下一行（不用浮层 —— 卡片小，浮层会压住半张图）
  const lastActive = report.ByDay[report.ByDay.length - 1]
  await page.hover(`.empty .us-mini .us-heat .us-heat-cell[data-key="${lastActive.Key}"]`)
  await page.waitForTimeout(200)
  const miniCell = await page.locator('.empty .us-mini-range').innerText()
  assert(miniCell.includes('当日') && miniCell.includes('累计'), `空态 hover 内联当日/累计（${miniCell}）`)
  assert(await page.locator('.empty .us-mini .us-tip').count() === 0, '空态不用浮层（信息走标题行）')
  assert(
    miniCell.replace(/\s+/g, '').includes(U.fmtBigTokens(lastActive.Input + lastActive.Output, 'zh').replace(/\s+/g, '')),
    `空态 hover 当日值对账（${U.fmtBigTokens(lastActive.Input + lastActive.Output, 'zh')}）`,
  )
  assert(
    miniCell.replace(/\s+/g, '').includes(U.fmtBigTokens(lastActive.CumTokens, 'zh').replace(/\s+/g, '')),
    `空态 hover 累计值对账（${U.fmtBigTokens(lastActive.CumTokens, 'zh')}）`,
  )
  await page.screenshot({ path: path.join(SHOTS, '1-empty-mini.png') })

  // 窄窗（右栏/小窗口把对话列压窄）：格子跟着卡片等比缩，不溢出、不拉成非正方形
  await page.setViewportSize({ width: 560, height: 720 })
  await page.waitForTimeout(150)
  const narrow = await page.evaluate(() => {
    const card = document.querySelector('.empty .us-mini').getBoundingClientRect()
    const box = document.querySelector('.empty .us-mini .us-heat').getBoundingClientRect()
    const cell = document.querySelector('.empty .us-mini .us-heat .us-heat-cell').getBoundingClientRect()
    return { card: card.width, box: box.width, w: cell.width, h: cell.height }
  })
  assert(narrow.box <= narrow.card + 1, `窄窗网格不溢出卡片（${narrow.box.toFixed(1)} ≤ ${narrow.card.toFixed(1)}）`)
  assert(narrow.w > 4 && Math.abs(narrow.w - narrow.h) < 1.5, `窄窗格子仍等比（${narrow.w.toFixed(1)}×${narrow.h.toFixed(1)}）`)
  await page.setViewportSize({ width: 1280, height: 860 })
  await page.waitForTimeout(150)

  // —— ② 设置导航：分组 + 使用统计入口 ——
  console.log('\n② 设置导航')
  await page.evaluate(() => window.__store.setState({ inSettings: true }))
  await page.waitForSelector('.settings-card', { timeout: 5000 })
  const groups = await page.$$eval('.settings-nav .settings-nav-sec', (ns) => ns.map((n) => n.textContent.trim()))
  assert(groups.join('|') === '模型|Agent 能力|数据与统计', `分组标题 = ${groups.join(' / ')}`)
  const segsBefore = await page.locator('.usage-stats').count()
  assert(segsBefore === 0, '未点击「使用统计」前不渲染统计页')
  await page.click('.settings-nav button:has-text("使用统计")')
  await page.waitForSelector('.usage-stats', { timeout: 5000 })
  ok('点击「使用统计」→ 统计页渲染')

  // —— ③ 指标卡对账（累计/峰值/最长聊天/连续天数/成本）——
  console.log('\n③ 指标卡')
  const cards = await page.$$eval('.us-card', (ns) => ns.map((n) => ({ v: n.querySelector('.us-card-v')?.textContent.trim(), l: n.querySelector('.us-card-l')?.textContent.trim() })))
  assert(cards.length === 7, `指标卡 7 张（含无价调用占比，实际 ${cards.length}）`)
  const st = report.Stats
  // 无价调用占比（2026-09-23）：分母 = 有价 + 无价（旧行 unknown 单列，不稀释占比）
  const upN = st.UnpricedCalls ?? 0
  const pricedN = st.PricedCalls ?? 0
  const upPct = pricedN + upN > 0 ? (upN / (pricedN + upN)) * 100 : 0
  const expect = [
    [U.fmtBigTokens(st.TotalTokens, 'zh').replace(/\s+/g, ''), '累计 Token 数'],
    [U.fmtBigTokens(st.PeakDayTokens, 'zh').replace(/\s+/g, ''), '峰值 Token 数'],
    [U.fmtChatDuration(st.LongestChatMs, 'zh').replace(/\s+/g, ''), '最长聊天时长'],
    [`${st.CurrentStreak}天`, '当前连续天数'],
    [`${st.LongestStreak}天`, '最长连续天数'],
    ['$' + st.TotalCostUsd.toFixed(st.TotalCostUsd >= 100 ? 0 : 2), '累计成本'],
    [`${upPct.toFixed(upPct < 10 ? 1 : 0)}%`, '无价调用占比'],
  ]
  cards.forEach((c, i) => {
    assert(c.v.replace(/\s+/g, '') === expect[i][0], `卡片${i + 1} 值 = ${expect[i][0]}（实际 ${c.v}）`)
    assert(c.l === expect[i][1], `卡片${i + 1} 标签 = ${expect[i][1]}`)
  })
  // 无价卡 title 要能定位到具体模型（否则用户不知道该给哪个模型补价）
  const titles = await page.$$eval('.us-card', (ns) => ns.map((n) => n.getAttribute('title') ?? ''))
  const topUnpriced = report.ByModel
    .filter((m) => (m.UnpricedCalls ?? 0) > 0)
    .sort((a, b) => (b.UnpricedCalls ?? 0) - (a.UnpricedCalls ?? 0))[0]
  if (topUnpriced) {
    assert(titles[6].includes(topUnpriced.Key), `无价卡 title 含主要来源模型 ${topUnpriced.Key}`)
    assert(titles[6].includes(`(${topUnpriced.UnpricedCalls})`), `无价卡 title 含无价调用数 ${topUnpriced.UnpricedCalls}`)
  }
  const peakRatio = st.PeakDayTokens / Math.max(1, st.TotalTokens)
  assert(peakRatio > 0 && peakRatio <= 1, '峰值 ≤ 累计（口径自洽）')

  // 思考占比（2026-09-23 补齐维度）：Reasoning ⊆ Output —— 总 token 卡 hint 里给出
  // 「其中思考 X，占输出 Y%」（此前报表完全没有思考维度）
  const think = report.Total.Reasoning ?? 0
  const outTok = report.Total.Output ?? 0
  assert(think > 0 && outTok > 0, `报告应带 Reasoning 维度（think=${think} out=${outTok}）`)
  const totalHint = await page.$$eval('.us-card', (ns) => ns.map((n) => n.getAttribute('title') ?? ''))
  assert(totalHint[0].includes('其中思考'), `总 token 卡 hint 应含思考量: ${totalHint[0]}`)
  assert(totalHint[0].includes(((think / outTok) * 100).toFixed(1)), `思考占比应为 ${((think / outTok) * 100).toFixed(1)}%: ${totalHint[0]}`)
  assert(think <= outTok, 'Reasoning 必须 ⊆ Output')

  // —— ④ 热力图：53 列 × 7 格 + 月份刻度 + 累计视图 ——
  console.log('\n④ Token 活动热力图')
  // 空会话那边的近 90 天小热力图也在 DOM 里（设置弹窗之下）→ 页面断言一律限定 .settings-body
  const cols = await page.locator('.settings-body .us-heat-col').count()
  assert(cols === 53, `列数 = 53 周（实际 ${cols}）`)
  const cellsPerCol = await page.locator('.settings-body .us-heat-col').first().locator('.us-heat-cell').count()
  assert(cellsPerCol === 7, `每列 7 格（实际 ${cellsPerCol}）`)
  const months = await page.$$eval('.settings-body .us-heat-months span', (ns) => ns.map((n) => n.textContent.trim()).filter(Boolean))
  assert(months.length >= 11, `月份刻度 ≥ 11（实际 ${months.length}）`)
  // hover 浮层：今天那格 → 日期 + 当日 + 累计（与报告逐项对账）
  const todayKey = await page.evaluate(() => {
    const d = new Date()
    const p = (n) => String(n).padStart(2, '0')
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
  })
  const todayRow = report.ByDay.find((d) => d.Key === todayKey)
  const todayIdx = await page.evaluate((key) => {
    const cells = [...document.querySelectorAll('.settings-body .us-heat .us-heat-cell')]
    return cells.findIndex((n) => n.dataset.key === key)
  }, todayKey)
  assert(todayIdx >= 0, `今天（${todayKey}）的格子在页面上可定位`)
  await page.hover(`.settings-body .us-heat .us-heat-cell >> nth=${todayIdx}`)
  await page.waitForTimeout(200)
  const tip = await page.locator('.settings-body .us-heat-tip').innerText()
  const tipNorm = tip.replace(/\s+/g, '')
  assert(tipNorm.includes(U.fmtDayLong(todayKey, 'zh').replace(/\s+/g, '')), `浮层抬头 = 日期带星期（${tip.split('\n')[0]}）`)
  assert(tipNorm.includes(U.fmtBigTokens(todayRow.Input + todayRow.Output, 'zh').replace(/\s+/g, '')), `浮层当日 = ${U.fmtBigTokens(todayRow.Input + todayRow.Output, 'zh')}`)
  assert(tipNorm.includes(U.fmtBigTokens(todayRow.CumTokens, 'zh').replace(/\s+/g, '')), `浮层累计 = ${U.fmtBigTokens(todayRow.CumTokens, 'zh')}`)
  // 三视图切换：格子底色档位（累计视图下越近的日期越深）
  const dayLevels = await page.$$eval('.settings-body .us-heat .us-heat-cell', (ns) => ns.slice(-7).map((n) => n.className))
  await page.click('.us-block:not(.us-trend-block) .seg button:nth-child(3)') // 累计
  await page.waitForTimeout(300)
  const cumLevels = await page.$$eval('.settings-body .us-heat .us-heat-cell', (ns) => ns.slice(-7).map((n) => n.className))
  assert(dayLevels.join() !== cumLevels.join(), '切「累计」后格子分档变化（视图生效）')
  // 用「今天」那格判定最深档，而不是 DOM 末格 —— 末格是本周周六（今天之后的占位格，level 0）
  const todayCls = await page.evaluate(
    (key) => document.querySelector(`.settings-body .us-heat .us-heat-cell[data-key="${key}"]`)?.className ?? '',
    todayKey,
  )
  assert(todayCls.includes('lv4'), `累计视图今天格 = 最深档（全量累计最大，实际 ${todayCls}）`)
  // 浮层的「累计」行始终 = 报告 CumTokens（抽查 3 天逐格对账）
  for (const d of report.ByDay.slice(-3)) {
    await page.hover(`.settings-body .us-heat .us-heat-cell[data-key="${d.Key}"]`)
    await page.waitForTimeout(160)
    const tipText = (await page.locator('.settings-body .us-heat-tip').innerText()).replace(/\s+/g, '')
    assert(tipText.includes(U.fmtBigTokens(d.CumTokens, 'zh').replace(/\s+/g, '')), `累计视图浮层 ${d.Key} 累计 = ${U.fmtBigTokens(d.CumTokens, 'zh')}`)
    assert(tipText.includes(U.fmtBigTokens(d.Input + d.Output, 'zh').replace(/\s+/g, '')), `浮层 ${d.Key} 当日 = ${U.fmtBigTokens(d.Input + d.Output, 'zh')}`)
  }
  await page.screenshot({ path: path.join(SHOTS, '2-heat-cum.png') })
  await page.click('.us-block:not(.us-trend-block) .seg button:nth-child(1)') // 回每日
  await page.waitForTimeout(200)

  // —— ⑤ 时间范围：近 7 日切窗口（同一份报告）——
  console.log('\n⑤ 时间范围切换')
  const ticks30 = await page.locator('.us-trend-block .us-tick').count()
  assert(ticks30 === 8, `近 30 日刻度 = 8（实际 ${ticks30}）`)
  await page.click('.us-trend-block .seg button:nth-child(1)')
  await page.waitForTimeout(300)
  const ticks7 = await page.locator('.us-trend-block .us-tick').count()
  assert(ticks7 === 7, `近 7 日刻度 = 7（实际 ${ticks7}）`)
  const legend7 = await page.locator('.us-trend-block .us-legend-item').count()
  assert(legend7 >= 1, `近 7 日仍有图例（${legend7} 条）`)
  await page.screenshot({ path: path.join(SHOTS, '3-trend-7d.png') })
  await page.click('.us-trend-block .seg button:nth-child(2)') // 回近 30 日
  await page.waitForTimeout(200)

  // —— ⑥ 双语 ——
  console.log('\n⑥ 中英双语')
  await page.evaluate(() => window.__store.setState((s) => ({ settings: { ...s.settings, lang: 'en' } })))
  await page.waitForTimeout(320)
  const enTitle = await page.locator('.usage-stats .settings-sec-title').innerText()
  assert(enTitle === 'Usage', `英文标题 = ${enTitle}`)
  const enTicks = await page.locator('.us-trend-block .us-tick').count()
  assert(enTicks > 0, '英文下 x 轴刻度仍在')
  const enTickText = (await page.locator('.us-trend-block .us-tick').first().textContent()) ?? '' // SVG <text> 无 innerText
  assert(/^\d+\/\d+$/.test(enTickText), `英文刻度形如 9/19（实际 ${enTickText}）`)
  const enDays = await page.locator('.us-card').nth(3).locator('.us-card-v').innerText()
  assert(/^\d+d$/.test(enDays.trim()), `英文连续天数形如 6d（实际 ${enDays}）`)
  await page.screenshot({ path: path.join(SHOTS, '4-en.png') })
  const bigEn = U.fmtBigTokens(st.TotalTokens, 'en')
  assert(/^[\d.]+(B|M|K)$/.test(bigEn), `英文大数格式 = ${bigEn}`)

  console.log(`\n全部通过（${pass} 项断言），截图：${SHOTS}`)
} catch (e) {
  failed = true
  console.error(`\n❌ ${e.message}`)
  process.exitCode = 1
} finally {
  await browser.close()
  cleanup()
}
if (!failed) process.exit(0)
