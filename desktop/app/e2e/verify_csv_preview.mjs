#!/usr/bin/env node
// CSV/TSV 表格预览回归（2026-09）。
//
// 起因：产出卡片与右栏「最终文件」把 .csv/.tsv 当**纯文本**渲染（shiki 源码视图）——
// 列对齐全靠逗号，中文错位，表格结构完全看不出来。改为：CSV → GFM 表格 markdown →
// 既有 Markdown 管线（react-markdown + remark-gfm，零新依赖）。
//
// 单测（scripts/test-csv.mjs）只能证明「解析与转义正确」；**接得对不对**只有真实渲染能证明：
//   - 表格是不是真的交给了 Markdown 管线（而不是走了源码分支）
//   - 含 | 的单元格在真 DOM 里有没有把表格撑坏
//   - 大文件闸门的提示有没有落到界面上、行数有没有真的收敛
//   - 两条渲染路径（独立大视图 / 活动列表内嵌）是否都能切模式
// 这正是 2026-09 Markdown 双模式的教训：当时只断言了「按钮文案」，漏掉了「用户点开
// 根本没有切换按钮」。
//
// 断言：
//   ① CSV 默认进表格视图（.csv-preview + 真 <table>），不是源码视图
//   ② 真实脏数据逐格对上：字段内逗号 / 多行字段(↵) / 转义引号"" / 中文不错位
//   ③ markdown 注入被挡住：含 | 的单元格不撑坏表格（列数不变、值原样）、
//      *粗* `码` 不产生 strong/em/code 元素
//   ④ 行列数不一致：每行单元格数一致（补齐），且界面给出提示
//   ⑤ 大文件闸门：只渲染 500 行 + 「还有 200 行未显示」，界面不卡（有用时可感知的时限兜底）
//   ⑥ 切「纯文本」→ shiki 源码（带行号）；切回「表格」→ 表格恢复（双向可逆）
//   ⑦ 非 CSV 文件不受影响：.go 无切换按钮；.md 仍是 Markdown 那套（不是 CSV 那套）
//
// 用法：cd desktop/app && node e2e/verify_csv_preview.mjs
// 退出码：全通过 0；断言失败 1；**前置不满足 2**（探针页起不来/端口被占 —— 不是代码坏了）。
// 入链史：R08b 之前它是「裸奔」的（不在 npm test 里），而 R08a 的真回归正是它抓到的
//（滚动位置恢复 140→0）；入链前先把就绪判据换成 _serve.mjs 的 waitProbePage（端口归属自检）。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import { freePort, isPrecondition, waitProbePage } from './_serve.mjs'
import fs from 'fs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
// 端口由内核分配（见 e2e/_serve.mjs 的注释）：固定端口在链里会撞上一个还没退干净的
// 服务器（F46 的 5199 事故），而 waitPort 只会证明「有人在听」，证明不了「那是我们的页面」。
const PORT = await freePort()

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }


// 探针页：挂**真实 RightPanel** + 真实 index.css + mock transport（含 sales.csv / big.csv 样例）。
// 用真实组件才能锁住「store → openFile → file_preview 响应 → 渲染分支」的完整接线；
// 复刻一份 JSX 只能证明样式，证明不了「CSV 真的走了表格分支」。
const PROBE_DIR = path.join(__dirname, '_csvprobe_run')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8"><title>csv preview probe</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import RightPanel from '../../src/components/RightPanel'
// ⚠️ 副作用注册，必须与 src/main.tsx 一致（04-review F45/F46）：少了这一行 registry 是空的，
// RightPanel 的 getSurface(kind)?.render(...) 恒走 ?? 兜底 —— 本探针验的一直是兜底组件
//（file 的兜底就是同一个 FilePreviewTab，像素一致、断言全绿）。下面 page 加载后的第一条
// 自检断言会当场戳穿这件事，补上这行才轮到真正的 provider 渲染。
import '../../src/surfaces/builtin'
import { getSurface } from '../../src/surfaces/registry'
import { ArtifactCard } from '../../src/components/ArtifactCard'
import { useAppStore } from '../../src/store/useAppStore'
import '../../src/index.css'

useAppStore.setState({ workspace: '/mock/proj-auth' })
;(window as unknown as { __store: unknown }).__store = useAppStore
;(window as unknown as { __surfaces: unknown }).__surfaces = {
  file: !!getSurface('file'), git: !!getSurface('git'), web: !!getSurface('web'), activity: !!getSurface('activity'),
}
void useAppStore.getState().start('/mock/proj-auth')

// 产出卡片与右栏同屏：验证**真实用户路径**（点卡片的「表格预览」→ 右栏渲染出表格），
// 而不是直接调 store.openFile（那只证明接线在，证明不了按钮点得通）。
// 路径写 'out/sales.csv'（带目录）：模拟真实产出（写在工作区子目录），
// mock 的 file_preview 按 basename 命中样例内容。
;(window as unknown as { __CASES: unknown }).__CASES = {
  csv: { items: [
    { name: 'sales.csv', path: 'out/sales.csv', kind: 'created', added: 6, size: 260, lines: 6, sha256: 'c1' },
    { name: 'big.csv', path: 'out/big.csv', kind: 'created', added: 701, size: 9000, lines: 701, sha256: 'c2' },
    { name: 'guide.md', path: 'guide.md', kind: 'modified', added: 3, size: 2100, lines: 88, sha256: 'c3' },
  ] },
}

createRoot(document.getElementById('root')!).render(
  <div style={{ height: '100vh', display: 'flex', minWidth: 0 }}>
    <div className="narrative" style={{ flex: '0 1 420px', overflowY: 'auto', padding: 12, minWidth: 0 }}>
      <div data-case="csv" className="msg artifacts">
        <ArtifactCard items={(window as unknown as { __CASES: { csv: { items: never[] } } }).__CASES.csv.items} />
      </div>
    </div>
    <div style={{ flex: '1 1 auto', borderLeft: '1px solid var(--border)', minWidth: 0 }}>
      <div style={{ height: '100vh' }}><RightPanel /></div>
    </div>
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

const PROBE_MARKER = '<title>csv preview probe</title>'
const PROBE_URL = `http://localhost:${PORT}/e2e/_csvprobe_run/index.html`
const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)

// 打开某文件的「最终文件」视图：走真实动作链（openFile → 切 tab/展开 + file_preview 命令），
// 而不是直接 setState 伪造终态。seedInlineActivity 额外注入一条 read_file 记录，
// 让文件「在活动列表里」→ 走**内嵌**渲染路径（②）；不注入则走**独立大视图**（①）。
const openFinalFile = (page, filePath, inline = false) => page.evaluate(async ({ p, inl }) => {
  const st = window.__store
  st.setState({
    rightExpanded: true, fileFocus: undefined, filePreviews: {},
    gitFiles: [],
    blocks: inl
      ? [{ kind: 'tool', id: 1, toolId: 't1', name: 'read_file', args: JSON.stringify({ path: p }), result: 'ok', status: 'done', startedAt: Date.now() }]
      : [],
  })
  await new Promise((r) => setTimeout(r, 250))
  if (inl) st.setState({ expanded: undefined })
  st.getState().openFile(p, undefined, 'source')
  await new Promise((r) => setTimeout(r, 500))
}, { p: filePath, inl: inline })

// DOM 快照：表格结构（逐格文本）+ 说明条 + 模式开关状态 + 当前渲染形态
const snap = (page) => page.evaluate(() => {
  const table = document.querySelector('.csv-preview .md table')
  const rows = table ? [...table.querySelectorAll('tr')] : []
  const grid = rows.map((r) => [...r.querySelectorAll('th,td')].map((c) => c.textContent))
  const sw = [...document.querySelectorAll('.md-view-switch')]
    .find((el) => el.getAttribute('aria-label')?.includes('CSV'))
  return {
    tableShown: !!table,
    csvShown: !!document.querySelector('.csv-preview'),
    grid,
    // 说明条文本（含截断提示）。.csv-notes 内两个 span：说明串 / 截断警告
    notes: document.querySelector('.csv-notes')?.textContent ?? '',
    notesWarn: document.querySelector('.csv-notes .warn')?.textContent ?? '',
    switchShown: !!sw,
    switchLabels: sw ? [...sw.querySelectorAll('button')].map((b) => b.textContent) : [],
    switchOn: sw ? [...sw.querySelectorAll('button')].find((b) => b.classList.contains('on'))?.textContent : null,
    srcShown: !!document.querySelector('.file-source .fp-code'),
    fpLnCount: document.querySelectorAll('.file-source .fp-code .fp-ln').length,
    // 注入防御：表格内**不得**出现被 markdown 解析出来的元素
    mdElems: table ? table.querySelectorAll('strong,em,del,code,a,ul,h1,h2').length : -1,
    loading: !!document.querySelector('.file-activity-empty'),
    // 表格是否被容器接管横滚（列多时不撑破）
    wrapScrollable: (() => {
      const w = document.querySelector('.csv-preview .md-table-wrap')
      return w ? w.scrollWidth > w.clientWidth : null
    })(),
  }
})

async function main() {
  await waitProbePage(PROBE_URL, { marker: PROBE_MARKER, probe: 'e2e:csv', child: server, timeoutMs: 120000 })
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  page.on('console', (m) => { if (m.type() === 'error' && !/favicon/.test(m.location()?.url || '')) errs.push(m.text().slice(0, 200)) })

  await page.goto(PROBE_URL, { waitUntil: 'domcontentloaded' })
  // 右侧栏默认折叠（只渲染 rail）：必须先展开，否则挂载的是折叠态而非工作面板。
  await page.waitForSelector('aside.right.collapsed', { timeout: 30000 })

  // —— 仪器自检（04-review F45/F46）——
  // RightPanel 的内容面板一律是 getSurface(kind)?.render(...) ?? 本地组件。本探针页只
  // import RightPanel 时注册表是空的 → 验的一直是 **兜底组件**，而兜底渲染的往往是同一个
  // 组件（file → FilePreviewTab），像素一致、断言全绿 —— 这是最坏的一种失败：看不出坏。
  // 所以先钉住「provider 真的在注册表里」：缺 import 时这里当场红，而不是让下面的面板断言
  // 继续在兜底分支上假绿。
  const surfaces = await page.evaluate(() => window.__surfaces ?? null)
  assert(!!surfaces?.file, `仪器：builtin provider 已注册（缺 builtin import 时验的是 ?? 兜底；实得 ${JSON.stringify(surfaces)}）`)

  console.log('\n[1] CSV 默认进表格视图（不是源码视图）')
  await openFinalFile(page, 'sales.csv')
  await page.waitForSelector('.csv-preview', { timeout: 20000 })
  await page.waitForTimeout(600)
  let s = await snap(page)
  assert(s.csvShown && s.tableShown, '默认渲染表格视图（.csv-preview + 真 <table>）')
  assert(!s.srcShown, '默认**不**走 shiki 源码视图（本次改动的核心：不再当纯文本看）')
  assert(!s.loading, 'file_preview 内容已回填（未停在「正在读取文件…」）')
  assert(s.switchShown, '表格视图下应有模式切换（表格 / 纯文本）')
  assert(JSON.stringify(s.switchLabels) === JSON.stringify(['表格', '纯文本']), `按钮文案 = ${JSON.stringify(s.switchLabels)}`)
  assert(s.switchOn === '表格', `默认停在表格（实际 ${s.switchOn}）`)

  console.log('\n[2] 真实脏数据逐格对上（字段内逗号 / 多行字段 / 转义引号 / 中文）')
  // 源文件（src/transport/mock.ts 的 sales.csv）：
  //   产品,数量,单价,备注
  //   苹果,3,5.5,"便宜, 常买"                     ← 字段内逗号
  //   梨,2,8,多行备注："需要冷藏\n第二行说明"      ← 字段内换行（引号包裹）
  //   他说,1,0,"好的""没问题"""                    ← 转义引号
  //   记号笔,4,12,*促销* `SKU-9527`                ← markdown 语法
  //   凑数行,1,1                                   ← 少一列（脏数据）
  assert(s.grid.length === 6, `表头 + 5 行数据（实际 ${s.grid.length} 行）`)
  assert(JSON.stringify(s.grid[0]) === JSON.stringify(['产品', '数量', '单价', '备注']), `表头逐格正确（首行当表头）：${JSON.stringify(s.grid[0])}`)
  assert(s.grid[1][3] === '便宜, 常买', `字段内的逗号保留在一个单元格里：${JSON.stringify(s.grid[1][3])}`)
  assert(s.grid[2][3].includes('↵'), `多行字段折叠为 ↵（GFM 单元格放不下真换行）：${JSON.stringify(s.grid[2][3])}`)
  assert(s.grid[2][3].includes('需要冷藏') && s.grid[2][3].includes('第二行说明'), '多行字段的两行内容都在（没丢后半段）')
  assert(s.grid[3][3] === '好的"没问题"', `转义引号 "" → 一个字面引号：${JSON.stringify(s.grid[3][3])}`)
  assert(s.grid[1][0] === '苹果' && s.grid[2][0] === '梨' && s.grid[4][0] === '记号笔', '中文列不错位（表头与数据列对齐）')
  assert(s.notes.includes('首行按表头显示'), `界面写明「首行按表头显示」（CSV 无法自动判断表头）：${JSON.stringify(s.notes)}`)

  console.log('\n[3] markdown 注入被挡住（含 | 的单元格不撑坏表格）')
  // 上一节已隐含：*促销* 没有变成 <strong>。这里正面断言元素数与列数。
  assert(s.grid[4][3] === '*促销* `SKU-9527`', `markdown 语法原样显示为文本：${JSON.stringify(s.grid[4][3])}`)
  assert(s.mdElems === 0, `表格内无 strong/em/code/a 等被解析出的元素（实际 ${s.mdElems} 个）`)
  // 管道符专项：CSV 里没有 | 的样例，直接在 store 里换一份含 | 的内容重渲染（走同一真实链路）
  await page.evaluate(async () => {
    window.__store.setState({ filePreviews: { 'sales.csv': { path: 'sales.csv', content: '名前,値\nA|B,1\nC,2\n', lines: 3 } } })
    await new Promise((r) => setTimeout(r, 400))
  })
  const pipe = await snap(page)
  assert(
    JSON.stringify(pipe.grid) === JSON.stringify([['名前', '値'], ['A|B', '1'], ['C', '2']]),
    `含 | 的单元格不撑坏表格（列数保持 2、值原样 A|B）：${JSON.stringify(pipe.grid)}`,
  )

  console.log('\n[4] 行列数不一致（脏数据）：对齐 + 提示')
  await openFinalFile(page, 'sales.csv')
  await page.waitForSelector('.csv-preview', { timeout: 20000 })
  await page.waitForTimeout(400)
  s = await snap(page)
  const widths = [...new Set(s.grid.map((r) => r.length))]
  assert(widths.length === 1 && widths[0] === 4, `每行单元格数一致（${JSON.stringify(widths)}）—— 脏数据未把表格撑坏`)
  assert(s.grid[5].length === 4 && s.grid[5][3] === '', `少列的行补空单元格：${JSON.stringify(s.grid[5])}`)
  assert(s.notes.includes('字段数不一致'), `界面提示各行字段数不一致：${JSON.stringify(s.notes)}`)

  console.log('\n[5] 大文件闸门（只渲染前 N 行 + 如实提示，界面不卡死）')
  const t0 = Date.now()
  await openFinalFile(page, 'big.csv')
  await page.waitForSelector('.csv-preview', { timeout: 20000 })
  await page.waitForTimeout(700)
  const elapsed = Date.now() - t0
  const big = await snap(page)
  assert(big.grid.length === 501, `只渲染 500 行数据 + 1 行表头（实际 ${big.grid.length} 行）`)
  assert(big.grid[500][0] === '500', `第 500 行内容正确（末行 id=500）：${JSON.stringify(big.grid[500])}`)
  assert(/还有 200 行未显示/.test(big.notesWarn), `提示未显示的行数（700 行数据 - 500）：${JSON.stringify(big.notesWarn)}`)
  assert(elapsed < 15000, `含渲染在内耗时 ${elapsed}ms（未卡死）`)

  console.log('\n[6] 切「纯文本」→ 高亮源码；切回「表格」→ 表格恢复（双向可逆）')
  await page.click('.md-view-switch button:nth-child(2)')
  await page.waitForTimeout(500)
  let plain = await snap(page)
  assert(plain.switchOn === '纯文本', `切到纯文本（实际 ${plain.switchOn}）`)
  assert(plain.srcShown && !plain.tableShown, '源码容器在、表格不在（既有 shiki 视图仍可达）')
  assert(plain.fpLnCount > 0, `纯文本模式带行号 gutter（fp-ln=${plain.fpLnCount}）`)
  await page.click('.md-view-switch button:nth-child(1)')
  await page.waitForTimeout(500)
  let back = await snap(page)
  assert(back.tableShown && !back.srcShown && back.switchOn === '表格', '切回表格（可逆）')
  assert(back.grid.length === 501, '切回后表格内容完整（行数不变）')

  console.log('\n[7] 两条渲染路径都要能切（独立大视图 / 活动列表内嵌）')
  // 注意：这一条对应 2026-09 Markdown 双模式的真实漏改（只在独立大视图加了切换按钮）。
  await openFinalFile(page, 'sales.csv', true)
  await page.waitForSelector('.csv-preview', { timeout: 20000 })
  await page.waitForTimeout(500)
  const inline = await snap(page)
  assert(inline.csvShown && inline.tableShown, '活动列表内嵌路径也渲染表格')
  assert(inline.switchShown, '活动列表内嵌路径也有切换按钮（漏改回归守卫）')

  console.log('\n[8] 非 CSV 文件不受影响')
  await openFinalFile(page, 'auth.go')
  await page.waitForSelector('.file-source .fp-code', { timeout: 20000 })
  await page.waitForTimeout(400)
  const go = await snap(page)
  assert(!go.csvShown && !go.tableShown, '.go 不出现 CSV 表格视图（仍是源码视图）')
  assert(!go.switchShown, '.go 不出现 CSV 模式切换')
  assert(go.srcShown, '.go 仍走 shiki 源码（既有行为不变）')
  // Markdown 仍走它自己那套（预览/纯文本），不被 CSV 分支截走
  await openFinalFile(page, 'guide.md')
  await page.waitForSelector('.file-source .md-preview-doc', { timeout: 20000 })
  await page.waitForTimeout(400)
  const md = await page.evaluate(() => ({
    csvShown: !!document.querySelector('.csv-preview'),
    mdSwitchLabels: [...document.querySelectorAll('.md-view-switch button')].map((b) => b.textContent),
    tables: document.querySelectorAll('.md-preview-doc .md table').length,
  }))
  assert(!md.csvShown, '.md 不走 CSV 表格分支')
  assert(JSON.stringify(md.mdSwitchLabels) === JSON.stringify(['预览', '纯文本']), `.md 仍是 Markdown 那套切换：${JSON.stringify(md.mdSwitchLabels)}`)
  assert(md.tables === 1, 'Markdown 文件里的表格照常渲染（未受影响）')

  assert(errs.length === 0, `无页面错误（${errs.length ? errs.join(' | ') : 'clean'}）`)

  console.log('\n[9] 真实点击路径：产出卡片「表格预览」按钮 → 右栏渲染出表格')
  // 这是本次改动对用户最直接的一环：卡片的主按钮。此前 CSV 点进去是纯文本源码。
  // 必须用**真实点击**（而非调 store）：按钮文案分派 + openFile 参数 + 右栏渲染
  // 三段接起来才成立，其中任一段错都只会表现为「点了没反应」。
  {
    // sales.csv 是首条产出 → 渲染为 hero（不是紧凑列表行）；这里就按 hero 点。
    const btn = page.locator('[data-case="csv"] .art-hero').locator('button', { hasText: '表格预览' }).first()
    const label = (await btn.textContent())?.trim()
    assert(label === '表格预览', `CSV 主按钮文案 = 表格预览（实际 ${label}）`)
    await btn.click()
    await page.waitForSelector('.csv-preview .md table', { timeout: 20000 })
    await page.waitForTimeout(500)
    const after = await snap(page)
    assert(after.tableShown, '真实点击后右栏渲染出表格（不是源码）')
    assert(!after.loading, '不停在「正在读取文件…」（路径归一化正确）')
    assert(
      JSON.stringify(after.grid[1]) === JSON.stringify(['苹果', '3', '5.5', '便宜, 常买']),
      `点击后表格内容正确：${JSON.stringify(after.grid[1])}`,
    )
    // 主按钮不该被改回纯文本：这是本次需求的核心
    const focusPath = await page.evaluate(() => window.__store.getState().fileFocus?.path ?? null)
    assert(focusPath === 'out/sales.csv', `卡片传相对路径（focusPath=${focusPath}）`)
  }

  await browser.close()
  console.log(`\n✅ CSV 表格预览：${pass} 项全部通过`)
  // 显式收尾：不 kill 掉 vite 子进程，它会让 Node 事件循环永不空 → 脚本**全绿却永不退出**
  //（终端卡住 + 留下僵尸 vite 占端口，之后用默认端口重跑会因 --strictPort 直接失败）。
  // 失败路径本来就 exit(1)，所以只有全绿时才卡。
  cleanup()
}

main().catch((e) => {
  cleanup()
  if (isPrecondition(e)) {
    // 前置不满足 ≠ 断言失败（00 §4.6 / 04 F46）：退出码 2
    console.error(`\n⛔ 前置不满足（退出码 2，不是测试失败）：${e.message}`)
    process.exit(2)
  }
  console.error('\n❌ ' + (e?.message ?? e))
  process.exit(1)
})
