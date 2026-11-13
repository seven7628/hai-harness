#!/usr/bin/env node
// 宽表格横滚回归（用户实测反馈 2026-09-11：Markdown 宽表格既滑不动也看不全）。
//
// 根因：单元格含不可断的长 token（长路径 / 长标识符，mono 字体）时，表格 min-content
// 宽度超过对话列宽 → 表格撑破 .md → 被 .narrative{overflow-x:hidden} 整块裁掉，
// 右侧列（如「备注」）永久不可见且无法横滑。
// 修复：Markdown 组件用 .md-table-wrap 包住表格，横向溢出收进表格自己的滚动容器
// （与代码块 .cb-body / mermaid .mermaid-block 同策略）。
//
// 断言：
//   ① 宽表容器可横滑（scrollWidth > clientWidth），且溢出不外泄到叙事区
//   ② 滚到最右：最后一个表头 / 末列内容完整可见（修复前被裁）
//   ③ 窄表不被牵连：容器不可横滑、表格占满列宽（不出现多余横条）
//   ④ 表格 margin 归零、纵向间距在容器上（横条与表格间无多出的缝）
//
// 用法：cd desktop/app && npm run build && node e2e/verify_wide_table.mjs
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = Number(process.env.PORT || 4184)
const URL_BASE = `http://localhost:${PORT}/`

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
    if (Date.now() > deadline) throw new Error('vite preview 未就绪')
    await new Promise((r) => setTimeout(r, 250))
  }
}

const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), 'preview', '--port', String(PORT), '--strictPort'], { cwd: APP, stdio: 'pipe' })
const stopServer = () => { try { server.kill('SIGTERM') } catch { /* 已退出 */ } }
process.on('exit', stopServer)

async function main() {
  await waitPort(PORT)
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 900, height: 1000 } })
  page.on('pageerror', (e) => console.error('[pageerror]', e.message))
  await page.goto(URL_BASE, { waitUntil: 'domcontentloaded' })
  await page.waitForSelector('textarea[placeholder*="输入消息"]', { timeout: 25000 })
  await page.getByRole('button', { name: '新建会话' }).first().click()
  await page.waitForTimeout(400)

  // 列宽收窄到 900 视口（左右栏同开场景）：宽表必然超出对话列宽
  const composer = page.locator('textarea[placeholder*="输入消息"]')
  await composer.fill('宽表格')
  await composer.press('Enter')
  // 等 .md table（而非修复后的容器类名）：修复前也能走到断言，失败信息直接指向根因
  await page.waitForSelector('.md table', { timeout: 25000 })
  await page.waitForTimeout(1200)

  // ---- ① 宽表溢出被容器接管，不撑破叙事区 ----
  const geom = await page.evaluate(() => {
    const n = document.querySelector('.narrative')
    const tables = [...document.querySelectorAll('.md table')]
    const wraps = [...document.querySelectorAll('.md-table-wrap')]
    const w = wraps[0]
    const t = w ? w.querySelector('table') : tables[0]
    return {
      tableCount: tables.length,
      wrapCount: wraps.length,
      wrapTag: w ? w.tagName : t.parentElement.tagName,
      narrativeOverflowX: getComputedStyle(n).overflowX,
      narrativeOverflows: n.scrollWidth > n.clientWidth + 1,
      wrapOverflowX: w ? getComputedStyle(w).overflowX : '(无容器)',
      wrapClientW: w ? w.clientWidth : t.parentElement.clientWidth,
      wrapScrollW: w ? w.scrollWidth : t.parentElement.scrollWidth,
      tableW: Math.round(t.getBoundingClientRect().width),
      // 容器（可见边界）必须落在叙事区内；表格自身可宽于容器（溢出由容器滚动承接）
      wrapInsideNarrative: w ? (w.getBoundingClientRect().right <= n.getBoundingClientRect().right + 1 && w.getBoundingClientRect().left >= n.getBoundingClientRect().left - 1) : false,
      tableMargin: getComputedStyle(t).margin,
      nodeAttrLeaked: t.hasAttribute('node'),
      headerCells: [...t.querySelectorAll('thead th')].length,
    }
  })
  assert(geom.tableCount === 2, `宽/窄两个表格都渲染（实际 ${geom.tableCount} 个）`)
  assert(geom.wrapCount === 2, `每个表格都包在 .md-table-wrap 里（实际 ${geom.wrapCount} 个/父元素 ${geom.wrapTag}）`)
  assert(geom.wrapOverflowX === 'auto', `表格容器 overflow-x: auto（实际 ${geom.wrapOverflowX}）`)
  assert(geom.wrapInsideNarrative, '滚动容器落在叙事区内（溢出由容器承接，不再被叙事区裁掉）')
  assert(geom.wrapScrollW > geom.wrapClientW + 1, `宽表在容器内横向溢出（${geom.wrapScrollW} > ${geom.wrapClientW}）`)
  assert(geom.narrativeOverflowX === 'hidden' && !geom.narrativeOverflows, '叙事区不再被宽表撑出横向溢出（不再有内容被裁掉）')
  assert(geom.tableW > geom.wrapClientW, '表格保持自身宽度（不被压扁成省略号/换行）')
  assert(geom.tableMargin === '0px', '表格 margin 归零（纵向间距移到容器，横条与表格间无余缝）')
  assert(!geom.nodeAttrLeaked, 'react-markdown 的 node prop 未泄漏到 DOM（无 node="[object Object]"）')
  assert(geom.headerCells === 6, `宽表 6 列全部渲染（实际 ${geom.headerCells}）`)

  // ---- ② 滚到最右：最后一个表头与末列单元格完整可见（修复前被裁）----
  const right = await page.evaluate(() => {
    const n = document.querySelector('.narrative')
    const w = document.querySelectorAll('.md-table-wrap')[0]
    const t = w.querySelector('table')
    w.scrollLeft = w.scrollWidth // 滚到最右
    const maxScroll = w.scrollLeft
    const nr = n.getBoundingClientRect()
    const ths = [...t.querySelectorAll('thead th')]
    const last = ths[ths.length - 1].getBoundingClientRect()
    const firstRowCells = [...t.querySelectorAll('tbody tr:first-child td')]
    const lastCell = firstRowCells[firstRowCells.length - 1].getBoundingClientRect()
    return {
      maxScroll,
      scrollablePx: w.scrollWidth - w.clientWidth,
      lastHeaderText: ths[ths.length - 1].textContent,
      lastHeaderVisible: last.left >= nr.left - 1 && last.right <= nr.right + 1,
      lastCellText: firstRowCells[firstRowCells.length - 1].textContent,
      lastCellVisible: lastCell.left >= nr.left - 1 && lastCell.right <= nr.right + 1,
    }
  })
  assert(right.maxScroll > 0, `容器实际可横滑到最右（${right.maxScroll}px）`)
  assert(right.scrollablePx === right.maxScroll, '滚动范围与溢出量一致（无额外/padding 假溢出）')
  assert(right.lastHeaderText === '备注' && right.lastHeaderVisible, '最右后「备注」表头完整可见（此前被裁在列宽之外）')
  assert(right.lastCellVisible, `首行末列单元格完整可见（${right.lastCellText.slice(0, 12)}…）`)

  // ---- ③ 窄表不被牵连：不可横滑 + 占满列宽 ----
  const narrow = await page.evaluate(() => {
    const w = document.querySelectorAll('.md-table-wrap')[1]
    const t = w.querySelector('table')
    return {
      clientW: w.clientWidth,
      scrollW: w.scrollWidth,
      tableW: Math.round(t.getBoundingClientRect().width),
      hScrollable: w.scrollWidth > w.clientWidth + 1,
    }
  })
  assert(!narrow.hScrollable, '窄表容器不可横滑（不出现多余横条）')
  assert(Math.abs(narrow.tableW - narrow.clientW) <= 1, `窄表仍占满列宽（${narrow.tableW} ≈ ${narrow.clientW}）`)

  // ---- ④ 叙事区纵向滚动不受影响（表格容器不吞掉页面滚动）----
  const scrollOk = await page.evaluate(() => {
    const n = document.querySelector('.narrative')
    return { narrativeOverflowY: getComputedStyle(n).overflowY, wrapOverflowY: getComputedStyle(document.querySelector('.md-table-wrap')).overflowY }
  })
  assert(scrollOk.narrativeOverflowY === 'auto', '叙事区纵向滚动保持 auto')
  assert(scrollOk.wrapOverflowY === 'hidden', '表格容器不产生纵向滚动（只横滑）')

  await browser.close()
  stopServer()
  console.log(`\n[verify] 宽表格横滚回归全部通过（${pass} 项断言）`)
}

main().catch((e) => { console.error('FAIL', e); process.exit(1) })
