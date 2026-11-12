#!/usr/bin/env node
// 右侧「最终文件」Markdown 双模式回归（2026-09）
//
// 起因：用户要求「右侧侧边栏最终文件展示的时候，对 Markdown 格式文档增加个模式，
// 支持预览模式，和纯文本的展示」。确认的口径：
//   - 默认预览（渲染后的文档）
//   - 模式选择不持久化（每次打开回到默认）
//   - 只作用于独立「最终文件」大视图（standalone），不含文件活动列表内嵌视图
//
// 本脚本锁住的关键行为（每条都对应一个「实现错了就会静默退化」的点）：
//   ① .md 文件：切换按钮出现，默认停在「预览」
//   ② 预览模式渲染的是**真文档**而非高亮源码：有真实 DOM 结构（标题/表格/列表），
//      且不出现 shiki 行号 gutter（.fp-ln）
//   ③ 切「纯文本」：出现 shiki 高亮源码 + 行号 gutter，且文档结构消失
//   ④ 切回「预览」：结构与 ② 一致（双向可逆，不是一次性开关）
//   ⑤ 非 Markdown 文件：切换按钮不出现（不能给 .go/.txt 也塞一个无意义的开关）
//   ⑥ 预览模式的 Markdown 走**真实渲染管线**：GFM 表格成立（<table> 真实元素），
//      代码围栏升级为带复制按钮的代码块（不是原样文本）
//   ⑦ 预览容器可滚动（长文档要能看到底部，不能 overflow 被裁）
//   ⑧ **两条渲染路径都要有切换按钮**：文件在活动列表 → 内嵌「最终文件」（detail-tabs
//      里的 tab）；文件不在活动列表 → 独立大视图。二者由同一 renderFinalFile 渲染。
//      （2026-09 实测踩过：只在独立大视图加了按钮，用户从活动列表点开完全没有 ——
//        本用例就是那条漏改的回归锚点。）
//
// 用法：cd desktop/app && node e2e/verify_md_file_modes.mjs
// （用真实 RightPanel 的 FilePreviewTab 语义烘一个临时探针页 + 真实 index.css，
//   跑完自动删除；不依赖 npm run build）
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'
import fs from 'fs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = Number(process.env.PORT || 4187)

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

const PROBE_DIR = path.join(__dirname, '_mdfileprobe_run')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8"><title>md file modes probe</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')

// 探针页：挂**真实 RightPanel**（不是复刻的 JSX）+ 真实 index.css + mock transport。
// 用真实组件才能锁住「store → openFile → file_preview 响应 → FilePreviewTab 渲染」
// 的完整链路；复刻版只能证明样式，证明不了接线。mock transport 提供 file_preview 内容
// （见 src/transport/mock.ts），因此这条链路在无 bridge 下也能真实跑通。
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import RightPanel from '../../src/components/RightPanel'
// ⚠️ 副作用注册，必须与 src/main.tsx 一致（04-review F45/F46）：少了这一行 registry 是空的，
// RightPanel 的 getSurface(kind)?.render(...) 恒走 ?? 兜底 —— 本探针验的一直是兜底组件
//（file 的兜底就是同一个 FilePreviewTab，像素一致、断言全绿）。下面 page 加载后的第一条
// 自检断言会当场戳穿这件事，补上这行才轮到真正的 provider 渲染。
import '../../src/surfaces/builtin'
import { getSurface } from '../../src/surfaces/registry'
import { useAppStore } from '../../src/store/useAppStore'
import '../../src/index.css'

useAppStore.setState({ workspace: '/mock/proj-auth' })
;(window as unknown as { __store: unknown }).__store = useAppStore
;(window as unknown as { __surfaces: unknown }).__surfaces = {
  file: !!getSurface('file'), git: !!getSurface('git'), web: !!getSurface('web'), activity: !!getSurface('activity'),
}
// 启动 store（建立 mock transport 连接）：不启动 → file_preview 响应无人接收，
// 「最终文件」永远停在「正在读取文件…」。
void useAppStore.getState().start('/mock/proj-auth')

createRoot(document.getElementById('root')!).render(
  <div style={{ height: '100vh' }}>
    <RightPanel />
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

const PROBE_URL = `http://localhost:${PORT}/e2e/_mdfileprobe_run/index.html`
const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)

// 打开某文件的「最终文件」视图：走真实动作链（openFile → setRightTab/Expanded +
// file_preview 命令），而不是直接 setState 伪造终态。
// seedActivity 往文件活动列表注入一条 read_file 记录（让文件「在列表里」）。
// 必须在 start() 之后注入 —— start 会用 mock 数据覆盖 blocks。
const seedActivity = (page, filePath) => page.evaluate(async (p) => {
  const st = window.__store
  const s0 = st.getState()
  const scope = s0.activeSessionId || s0.workspace
  // 阶段 2：内容与视图都归**标签**所有（filePreview/fileFocus 单例已拆），复位必须连标签一起 ——
  // 否则面板会停在「上一个标签的完整文件视图」上（standalone 由标签 payload 驱动，清 fileFocus
  // 已经关不掉它），下面等 .file-activity-row 就会超时。
  for (const t of [...(s0.tabsByScope[scope]?.tabs ?? [])]) if (t.kind === 'file') s0.closeTab(t.id)
  st.setState({
    rightExpanded: true, rightTab: 'file', fileFocus: undefined, filePreviews: {},
    blocks: [{ kind: 'tool', id: 1, toolId: 't1', name: 'read_file', args: JSON.stringify({ path: p }), result: 'ok', status: 'done', startedAt: Date.now() }],
    gitFiles: [],
  })
  // rightTab='file' 且此刻没有 file 标签 → **显式**开一个空标签（无 path = 文件活动列表态）。
  // 不能只靠 syncTabsToRightTab：PR-02 起它只在「这个 scope 还没有标签集」时建默认集，
  // 而这里恰恰是「标签集还在、只是被清空了」—— 于是 [9] 起的用例一直是红的（R07 复跑时
  // 发现：补 surfaces/builtin import **之前**就已经红，与本轮改动无关，属链外腐化）。
  st.getState().openTab({ kind: 'file' })
  await new Promise((r) => setTimeout(r, 300))
}, filePath)

const openFinalFile = (page, filePath) => page.evaluate(async (p) => {
  const st = window.__store
  // openFile(path, toolId=undefined, mode='source') → 打开「最终文件」全量视图
  st.getState().openFile(p, undefined, 'source')
  // 等 file_preview 响应回填（mock 同步 respond，但 React 渲染需一拍）
  await new Promise((r) => setTimeout(r, 400))
}, filePath)

const url = (p) => `http://localhost:${PORT}/e2e/_mdfileprobe_run/index.html?path=${encodeURIComponent(p)}`

// 视图快照：模式开关 / 文档结构 / 源码结构 / 行号 gutter 一并采样
// （选择器对齐真实 RightPanel：standalone 分支的 .file-side-detail > .file-inspector）
const snap = (page) => page.evaluate(() => {
  const side = document.querySelector('.file-side-detail:not(:empty)') || document.querySelector('.file-side-detail')
  const sw = side ? side.querySelector('.md-view-switch') : document.querySelector('.md-view-switch')
  const doc = document.querySelector('.md-preview-doc')
  const src = document.querySelector('.file-source .fp-code')
  const md = document.querySelector('.md-preview-doc .md')
  return {
    switchShown: !!sw,
    switchLabels: sw ? [...sw.querySelectorAll('button')].map((b) => b.textContent) : [],
    switchOn: sw ? [...sw.querySelectorAll('button')].find((b) => b.classList.contains('on'))?.textContent : null,
    docShown: !!doc,
    srcShown: !!src,
    fpLnCount: document.querySelectorAll('.file-source .fp-code .fp-ln').length,
    h1: md ? md.querySelectorAll('h1').length : 0,
    tables: md ? md.querySelectorAll('table').length : 0,
    tbodyRows: md ? md.querySelectorAll('tbody tr').length : 0,
    codeBlocks: md ? md.querySelectorAll('.codeblock').length : 0,
    listItems: md ? md.querySelectorAll('li').length : 0,
    quotes: md ? md.querySelectorAll('blockquote').length : 0,
    // 预览容器是否真的可滚动（长文档底部可达）
    scrollH: doc ? doc.scrollHeight : 0,
    clientH: doc ? doc.clientHeight : 0,
    // 加载中占位（内容未回填的标志）
    loading: !!document.querySelector('.file-activity-empty'),
  }
})

// 布局快照（整文件独占整宽）：量**真实矩形**而不是只看类名 —— 「不渲染左侧列表」的用户口径是
// 「文件占满整宽」，只看 DOM 是否存在挡不住「渲染了但把文件挤在半栏」这种形态。
const layoutSnap = (page) => page.evaluate(() => {
  const w = (el) => (el ? Math.round(el.getBoundingClientRect().width) : 0)
  const master = document.querySelector('.file-activity-master')
  const splitter = document.querySelector('.file-splitter')
  const detail = document.querySelector('.file-side-detail')
  return {
    panelW: w(document.querySelector('.file-activity-panel')),
    detailW: w(detail),
    masterShown: !!master,
    // 分隔条/详情在窄屏是 display:none（两栏态退回列表）→ 宽度 0 不等于「没渲染」
    splitterShown: !!splitter && getComputedStyle(splitter).display !== 'none',
    detailShown: !!detail && getComputedStyle(detail).display !== 'none',
    tabsOn: [...document.querySelectorAll('.file-detail-tabs button')].map((b) => (b.textContent || '') + (b.className.includes('on') ? '*' : '')).join('|'),
  }
})

// 点 detail-tabs 里的某个 tab（按文案匹配，避免依赖按钮顺序）
const clickDetailTab = (page, label) => page.evaluate((lb) => {
  const target = [...document.querySelectorAll('.file-detail-tabs button')].find((b) => (b.textContent || '').includes(lb))
  if (!target) return false
  target.click()
  return true
}, label)

async function main() {
  await waitPort(PORT)
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1280, height: 800 } })
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  page.on('console', (m) => {
    if (m.type() === 'error' && !/favicon/.test(m.location()?.url || '')) errs.push(m.text().slice(0, 200))
  })
  // 过滤探针页自身噪音：favicon 未提供（与功能无关）
  page.on('response', (r) => { if (r.status() >= 400 && !/favicon/.test(r.url())) errs.push(`HTTP ${r.status()} ${r.url()}`) })

  console.log('\n[1] Markdown 文件：开关存在，默认预览')
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
  await openFinalFile(page, 'guide.md')
  await page.waitForSelector('.file-activity-panel', { timeout: 30000 })
  await page.waitForSelector('.md-view-switch', { timeout: 20000 })
  await page.waitForTimeout(500)
  let s = await snap(page)
  assert(s.switchShown, 'Markdown 文件应出现模式切换按钮')
  assert(JSON.stringify(s.switchLabels) === JSON.stringify(['预览', '纯文本']), `按钮文案 = ${JSON.stringify(s.switchLabels)}`)
  assert(s.switchOn === '预览', `默认停在预览（实际 ${s.switchOn}）`)
  assert(s.docShown && !s.srcShown, '默认渲染预览（文档容器在、源码容器不在）')
  assert(!s.loading, 'file_preview 内容已回填（不再停在「正在读取文件…」）')

  console.log('\n[2] 预览 = 真文档结构（非高亮源码）')
  assert(s.h1 === 1, `标题渲染（h1=${s.h1}）`)
  assert(s.tables === 1 && s.tbodyRows === 2, `GFM 表格渲染（table=${s.tables} rows=${s.tbodyRows}，样例 2 行数据）`)
  assert(s.listItems >= 3, `列表渲染（li=${s.listItems}）`)
  assert(s.quotes >= 1, `引用块保留语义（blockquote=${s.quotes}）`)
  assert(s.codeBlocks >= 1, `围栏代码升级为代码块组件（codeblock=${s.codeBlocks}，含复制按钮）`)
  assert(s.fpLnCount === 0, `预览模式不应出现 shiki 行号 gutter（fp-ln=${s.fpLnCount}）`)

  console.log('\n[3] 预览容器可滚动（长文档底部可达）')
  assert(s.scrollH > s.clientH, `预览容器可滚动（scrollHeight=${s.scrollH} > clientHeight=${s.clientH}）`)

  console.log('\n[4] 切「纯文本」：高亮源码 + 行号，文档结构消失')
  await page.click('.md-view-switch button:nth-child(2)')
  await page.waitForTimeout(400)
  s = await snap(page)
  assert(s.switchOn === '纯文本', `切到纯文本（实际 ${s.switchOn}）`)
  assert(s.srcShown && !s.docShown, '源码容器在、文档容器不在')
  assert(s.fpLnCount > 0, `纯文本模式应出现 shiki 行号 gutter（fp-ln=${s.fpLnCount}）`)

  console.log('\n[5] 切回「预览」：结构恢复（双向可逆）')
  await page.click('.md-view-switch button:nth-child(1)')
  await page.waitForTimeout(400)
  s = await snap(page)
  assert(s.docShown && !s.srcShown && s.switchOn === '预览', '切回预览')
  assert(s.tables === 1 && s.codeBlocks >= 1 && s.listItems >= 3, '结构完整恢复（表格 + 代码块 + 列表）')

  console.log('\n[6] 非 Markdown 文件：不出现切换按钮')
  await openFinalFile(page, 'auth.go')
  await page.waitForSelector('.file-source .fp-code', { timeout: 20000 })
  await page.waitForTimeout(400)
  s = await snap(page)
  assert(!s.switchShown, '.go 文件不应出现 Markdown 模式切换')
  assert(s.srcShown, '非 Markdown 文件仍走源码视图（既有行为不变）')

  console.log('\n[7] 切文件后模式回到默认（不持久化）')
  await openFinalFile(page, 'guide.md')
  await page.waitForSelector('.md-view-switch', { timeout: 20000 })
  await page.waitForTimeout(400)
  // 先切到纯文本，再切到另一个 md 文件 → 应回到预览（用户决策「不记住」）
  await page.click('.md-view-switch button:nth-child(2)')
  await page.waitForTimeout(300)
  await openFinalFile(page, 'docs/guide.markdown')
  await page.waitForTimeout(500)
  s = await snap(page)
  assert(s.switchOn === '预览', `换文件后回到默认预览（实际 ${s.switchOn}）`)
  // 重开**同一文件**同样回默认（语义是「每次打开」而非「换文件才重置」）
  await page.click('.md-view-switch button:nth-child(2)')
  await page.waitForTimeout(300)
  assert((await snap(page)).switchOn === '纯文本', '前置：已切到纯文本')
  await openFinalFile(page, 'docs/guide.markdown')
  await page.waitForTimeout(500)
  s = await snap(page)
  assert(s.switchOn === '预览', `重开同一文件也回到默认预览（实际 ${s.switchOn}）`)

  console.log('\n[9] 活动列表内嵌「最终文件」同样可切换（漏改回归锚点）')
  // 场景：文件已被 read_file/write_file 记录过 → 出现在「文件活动列表」→ 用户点它
  // → 内嵌 detail 渲染（detailMode 默认 operation）→ 点「最终文件」tab。
  // 这条路径与独立大视图是**两个不同的渲染分支**，历史上只修了后者。
  await seedActivity(page, 'guide.md')
  await page.waitForSelector('.file-activity-row', { timeout: 20000 })
  await page.evaluate(async () => {
    window.__store.getState().openFile('guide.md', undefined, 'source')
    await new Promise((r) => setTimeout(r, 500))
  })
  await page.waitForTimeout(600)
  // 切到内嵌的「最终文件」tab（detail-tabs 里文案匹配，避免依赖按钮顺序）
  const tabClicked = await page.evaluate(() => {
    const btns = [...document.querySelectorAll('.file-detail-tabs button')]
    const target = btns.find((b) => (b.textContent || '').includes('最终文件'))
    if (!target) return false
    target.click()
    return true
  })
  assert(tabClicked, '内嵌视图应存在「最终文件」tab')
  await page.waitForTimeout(600)
  s = await snap(page)
  assert(s.switchShown, '内嵌「最终文件」也应出现 Markdown 模式切换（此前只在独立大视图有）')
  assert(JSON.stringify(s.switchLabels) === JSON.stringify(['预览', '纯文本']), `内嵌按钮文案 = ${JSON.stringify(s.switchLabels)}`)
  assert(s.docShown && !s.srcShown, '内嵌「最终文件」默认渲染预览')

  console.log('\n[10] 内嵌路径双向切换')
  await page.click('.md-view-switch button:nth-child(2)')
  await page.waitForTimeout(400)
  s = await snap(page)
  assert(s.srcShown && !s.docShown && s.fpLnCount > 0, '内嵌路径切「纯文本」→ shiki 源码 + 行号')
  await page.click('.md-view-switch button:nth-child(1)')
  await page.waitForTimeout(400)
  s = await snap(page)
  assert(s.docShown && !s.srcShown, '内嵌路径切回预览')

  console.log('\n[11] 整文件独占整宽：不渲染左侧活动列表与分隔条（用户口径 2026-09）')
  // 用户原话：「完整视图就是整个文件，就不需要左侧的变更记录了，而查看变更的时候才会展示
  // 变更列表，否则会导致文件预览不全」。两条渲染路径（独立大视图 / 列表内「最终文件」）都要整宽，
  // 而**变更**视图必须原样保留两栏 —— 故三态（整文件 → 变更 → 整文件）全部钉住。
  await page.evaluate(async () => {
    window.__store.setState({ blocks: [], gitFiles: [], fileFocus: undefined, filePreviews: {} })
    await new Promise((r) => setTimeout(r, 300))
  })
  await openFinalFile(page, 'docs/guide.markdown')
  await page.waitForTimeout(600)
  let L = await layoutSnap(page)
  assert(L.detailShown, '整文件态：详情列可见')
  assert(!L.masterShown, '整文件态：不渲染 .file-activity-master（左侧活动列表整块不渲染）')
  assert(!L.splitterShown, '整文件态：不渲染 .file-splitter')
  assert(L.detailW >= L.panelW - 2, `整文件态：文件占满面板整宽（detail=${L.detailW} panel=${L.panelW}）`)

  // 列表内路径（文件在活动列表 → 走 detailMode==='source' 分支）：同样整宽
  await seedActivity(page, 'guide.md')
  await page.waitForSelector('.file-activity-row', { timeout: 20000 })
  await page.evaluate(async () => {
    window.__store.getState().openFile('guide.md', undefined, 'source')
    await new Promise((r) => setTimeout(r, 500))
  })
  await page.waitForTimeout(600)
  L = await layoutSnap(page)
  assert(L.tabsOn.includes('最终文件*'), `列表内路径停在「最终文件」（实际 ${L.tabsOn}）`)
  assert(!L.masterShown && L.detailW >= L.panelW - 2, `列表内整文件同样占满整宽（detail=${L.detailW} panel=${L.panelW}）`)

  // 切「本次操作」→ 两栏回来（列表 + 分隔条 + 半栏详情）；再切回「最终文件」→ 又整宽
  assert(await clickDetailTab(page, '本次修改'), '整宽态仍能切回「本次修改」（否则用户出不去）')
  await page.waitForTimeout(500)
  L = await layoutSnap(page)
  assert(L.masterShown, '变更态：左侧活动列表回来')
  assert(L.splitterShown, '变更态：分隔条回来')
  assert(L.detailW < L.panelW - 100, `变更态：仍是两栏（detail=${L.detailW} panel=${L.panelW}）`)
  await clickDetailTab(page, '最终文件')
  await page.waitForTimeout(500)
  L = await layoutSnap(page)
  assert(!L.masterShown && L.detailW >= L.panelW - 2, `切回「最终文件」→ 又回到整宽（detail=${L.detailW} panel=${L.panelW}）`)

  // 窄屏（<720px）：两栏态在窄屏退回列表，但**整文件态没有列表可退** → 必须仍能看到文件本体
  //（.whole-file 的 display 规则刻意放在媒体查询之外，这条就是那个决定的回归锚点）
  await page.setViewportSize({ width: 640, height: 900 })
  await page.waitForTimeout(500)
  L = await layoutSnap(page)
  assert(L.detailShown && !L.masterShown, '窄屏整文件态：仍渲染文件本体（不是一块空白面板）')
  await page.setViewportSize({ width: 1280, height: 800 })
  await page.waitForTimeout(400)

  console.log('\n[12] 两个文件标签并存：各自内容正确、关一个不影响另一个（阶段 2）')
  // 本节的由来：阶段 1 的 file 标签**每 scope 只有一个**（幂等键 = kind），内容又来自全局单例
  // filePreview —— 打开第二个文件只是**覆盖**第一个，两个文件标签必然串台。阶段 2 把内容改成
  // 按归一化路径索引（store.filePreviews）、幂等键改成 kind+path+mode，本节就钉住这三件事：
  //   ① 两个文件 → 两个标签，各自显示自己的内容；
  //   ② 切走再切回 → 还在该标签自己的视图（模式 + 滚动位置，浏览器式标签的基本预期）；
  //   ③ 关掉其中一个 → 另一个的内容/视图模式/滚动位置**一个都不变**。
  await page.evaluate(async () => {
    const st = window.__store
    const s0 = st.getState()
    const scope = s0.activeSessionId || s0.workspace
    for (const t of [...(s0.tabsByScope[scope]?.tabs ?? [])]) if (t.kind === 'file') s0.closeTab(t.id)
    // rightTab 落到 activity 而不是 'file'：否则 RightPanel 的收敛 effect 会立刻补一个**空** file
    // 标签（无 path），本节的两个标签就变成三个（实测踩过）。
    st.setState({ rightExpanded: true, rightTab: 'activity', fileFocus: undefined, filePreviews: {}, blocks: [], gitFiles: [] })
    await new Promise((r) => setTimeout(r, 250))
  })
  await openFinalFile(page, 'guide.md')
  await openFinalFile(page, 'auth.go')
  await page.waitForTimeout(600)

  // 同时读 store（结构真相）与 DOM（用户可见的形态）：只读一边都可能漏掉「标签在、内容串了」
  const multiSnap = () => page.evaluate(() => {
    const st = window.__store.getState()
    const scope = st.activeSessionId || st.workspace
    const set = st.tabsByScope[scope] ?? { tabs: [], activeId: null }
    const fileTabs = set.tabs.filter((t) => t.kind === 'file')
    const code = document.querySelector('.file-source .fp-code')
    return {
      store: fileTabs.map((t) => `${t.path}:${t.mode}`),
      active: set.tabs.find((t) => t.id === set.activeId)?.path ?? '',
      bar: [...document.querySelectorAll('.rtab[data-kind="file"]')].map((r) =>
        (r.querySelector('.rtab-t')?.textContent ?? '').trim() + (r.getAttribute('aria-selected') === 'true' ? '*' : '')),
      // 当前渲染出的内容：源码容器文本（.go 走这条）/ 文档容器（.md 默认预览走这条）
      code: (code?.textContent ?? '').slice(0, 60),
      ln: code ? code.querySelectorAll('.fp-ln').length : 0,
      doc: !!document.querySelector('.md-preview-doc'),
      mdDocText: (document.querySelector('.md-preview-doc')?.textContent ?? '').slice(0, 40),
      mode: document.querySelector('.md-view-switch button.on')?.textContent ?? null,
      scroll: code?.scrollTop ?? -1,
    }
  })

  let M = await multiSnap()
  assert(M.store.length === 2, `两个 file 标签并存（store=${JSON.stringify(M.store)}）`)
  assert(M.store.join('|') === 'guide.md:source|auth.go:source', `各自 payload 正确（${JSON.stringify(M.store)}）`)
  assert(M.bar.join('|') === 'guide.md|auth.go*', `标签条上两个标签、最后打开的是激活项（${JSON.stringify(M.bar)}）`)
  assert(M.active === 'auth.go', `激活的是刚打开的 auth.go（${M.active}）`)
  assert(M.code.includes('package auth') && !M.doc, `激活标签显示的是 auth.go 的内容（不是上一个文件）`)

  // ① 切回第一个标签 → 显示 guide.md 自己的内容（不是 auth.go 的残留）
  await page.locator('.rtab[data-kind="file"] .rtab-t', { hasText: 'guide.md' }).click()
  await page.waitForTimeout(500)
  M = await multiSnap()
  assert(M.active === 'guide.md' && M.doc && !M.code.includes('package auth'),
    `切回 guide.md 标签 → 渲染它自己的 Markdown 预览（active=${M.active} doc=${M.doc}）`)

  // ② 在该标签里切「纯文本」并滚动，然后切到另一个标签再切回来 → 还在原来的视图
  await page.click('.md-view-switch button:nth-child(2)')
  await page.waitForTimeout(400)
  await page.evaluate(() => { const el = document.querySelector('.file-source .fp-code'); if (el) el.scrollTop = 140 })
  await page.waitForTimeout(200)
  M = await multiSnap()
  assert(M.mode === '纯文本' && M.scroll > 0 && M.ln > 0,
    `前置：guide.md 标签已切纯文本并滚动（mode=${M.mode} scroll=${M.scroll} ln=${M.ln}）`)
  const beforeSwitch = M
  await page.locator('.rtab[data-kind="file"] .rtab-t', { hasText: 'auth.go' }).click()
  await page.waitForTimeout(400)
  M = await multiSnap()
  assert(M.code.includes('package auth') && M.mode === null,
    `切到 auth.go 标签 → 显示它自己的内容、且不带 guide.md 的模式开关（${JSON.stringify({ code: M.code.slice(0, 20), mode: M.mode })}）`)
  await page.locator('.rtab[data-kind="file"] .rtab-t', { hasText: 'guide.md' }).click()
  await page.waitForTimeout(500)
  M = await multiSnap()
  assert(M.mode === beforeSwitch.mode && M.scroll === beforeSwitch.scroll && M.ln === beforeSwitch.ln,
    `切回来仍在原视图（模式 ${M.mode}、滚动 ${M.scroll}、行数 ${M.ln}）`)

  // ③ 关掉**另一个**标签 → 本标签的内容 / 视图模式 / 滚动位置一个都不变
  await page.locator('.rtab[data-kind="file"]', { hasText: 'auth.go' }).locator('.rtab-close').click()
  await page.waitForTimeout(500)
  M = await multiSnap()
  assert(M.bar.join('|') === 'guide.md*', `关掉 auth.go → 只剩 guide.md 且仍是激活项（${JSON.stringify(M.bar)}）`)
  assert(M.mode === beforeSwitch.mode, `内容/视图模式不受影响（mode=${M.mode}）`)
  assert(M.scroll === beforeSwitch.scroll, `滚动位置不受影响（scroll=${M.scroll}，关之前是 ${beforeSwitch.scroll}）`)
  assert(M.ln === beforeSwitch.ln && M.ln > 0, `内容不受影响（行号 gutter ${M.ln} 行）`)

  // ④ 重复打开同一 (path, mode) → 激活已有标签，不新增实例（幂等；行为等价于阶段 1 的「打开即更新」）
  await openFinalFile(page, 'guide.md')
  await page.waitForTimeout(500)
  M = await multiSnap()
  assert(M.store.length === 1 && M.bar.join('|') === 'guide.md*', `重复打开不新增实例（${JSON.stringify(M.bar)}）`)
  assert(M.mode === '预览', `重开同一文件回默认视图（用户决策「不记住」；实际 ${M.mode}）`)

  // ⑤ 内容缓存被清掉（= 切工作区/被上限淘汰的等价场景）→ 自动补拉，不停在「正在读取文件…」
  //   这条兜底是阶段 2 新增的（ensureFilePreview）：阶段 1 靠「打开即清空 + 立即请求」顺带保证。
  await page.evaluate(async () => {
    window.__store.setState({ filePreviews: {} })
    await new Promise((r) => setTimeout(r, 800))
  })
  M = await multiSnap()
  assert(M.mdDocText.length > 0 || M.ln > 0, `缓存被清掉后内容自动补拉（doc=${JSON.stringify(M.mdDocText)} ln=${M.ln}）`)

  // ⑥ 同一文件的 diff 标签与 source 标签**并存**，各自渲染各自的内容（D5：文件 Diff / 完整预览）
  // 先种一条带 diff 的 write_file 记录（文件活动列表里才有「本次修改」可看），
  // 再先开 diff 后开 source —— 两条必须落在**两个**标签上，且互不覆盖。
  await page.evaluate(async () => {
    window.__store.setState({
      blocks: [{
        kind: 'tool', id: 7, toolId: 'tw', name: 'write_file',
        args: JSON.stringify({ path: 'auth.go' }), result: 'written', status: 'done', startedAt: Date.now(),
        diff: { path: 'auth.go', added: 2, removed: 1, unified: '@@ -1,2 +1,3 @@\n package auth\n+// DIFF-MARKER\n-var old = 1' },
      }],
      gitFiles: [],
    })
    await new Promise((r) => setTimeout(r, 300))
  })
  await page.evaluate(async () => { window.__store.getState().openFile('auth.go', 'tw'); await new Promise((r) => setTimeout(r, 500)) })
  const diffSnap = () => page.evaluate(() => ({
    uni: (document.querySelector('.file-inspector-body .uni')?.textContent ?? '').slice(0, 80),
    code: (document.querySelector('.file-source .fp-code')?.textContent ?? '').slice(0, 40),
    tabsOn: [...document.querySelectorAll('.file-detail-tabs button')].map((b) => (b.textContent || '') + (b.className.includes('on') ? '*' : '')).join('|'),
  }))
  let D = await diffSnap()
  assert(D.uni.includes('DIFF-MARKER') && !D.code.includes('package auth'),
    `diff 标签渲染的是这次改动（${JSON.stringify(D.uni.slice(0, 30))}），不是完整文件`)
  assert(D.tabsOn.includes('本次修改*'), `且停在「本次修改」（实际 ${D.tabsOn}）`)
  await openFinalFile(page, 'auth.go')
  await page.waitForTimeout(500)
  M = await multiSnap()
  const authModes = M.store.filter((x) => x.startsWith('auth.go:')).map((x) => x.split(':')[1]).sort()
  assert(authModes.join(',') === 'diff,source', `同一文件的 diff 与 source 两个标签并存（${JSON.stringify(M.store)}）`)
  assert(M.code.includes('package auth') && M.ln > 0, `source 标签渲染的是完整文件（ln=${M.ln}）`)
  // 切回 diff 标签 → 仍是那次改动（没被 source 覆盖）
  await page.locator('.rtab[data-kind="file"]', { hasText: 'auth.go' }).first().click()
  await page.waitForTimeout(400)
  D = await diffSnap()
  assert(D.uni.includes('DIFF-MARKER') && !D.code.includes('package auth'), `切回 diff 标签仍是那次改动（${JSON.stringify(D.uni.slice(0, 24))}）`)

  console.log('\n[8] 无控制台/页面错误')
  assert(errs.length === 0, errs.length ? `存在错误: ${errs.slice(0, 5).join(' | ')}` : '无错误')

  await browser.close()
  console.log(`\n全部通过（${pass} 项断言）`)
  // 成功路径**必须显式收尾**：spawn 的 vite 子进程（stdio:'pipe'）会让 Node 事件循环永不空 →
  // 脚本「全绿但永不退出」，终端一直卡住，并留下一个僵尸 vite 占住端口（实测卡了 1h56m，
  // 之后用默认端口重跑会因 --strictPort 直接失败）。
  // 注意：失败路径本来就 process.exit(1)，所以只有全绿时才卡 —— 这是个很反直觉的坑。
  // 这里 kill 掉子进程后事件循环自然排空，Node 正常退出（不用 process.exit(0)，
  // 避免 stdout 是管道时最后的汇总行被截断）。
  cleanup()
}

main().catch((e) => {
  console.error('\n❌ ' + e.message)
  cleanup()
  process.exit(1)
})
