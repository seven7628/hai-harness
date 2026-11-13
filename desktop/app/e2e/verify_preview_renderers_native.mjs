#!/usr/bin/env node
// 八种格式的**真内容**验收（R08b）：真 Electron + 真 bridge + 受管 Python + 真原生视图。
//
// 为什么必须另有一层（链内的 e2e:preview-renderers 只能验一半）：
//   · pdf 的可见内容只存在于原生 WebContentsView（Chromium 自带 PDF viewer），浏览器里没有
//     这条通道 —— 链内只能验「落点指名 pdf + 写明缺什么」；
//   · docx/pptx/xlsx 的可见内容来自受管 Python 的转换产物，浏览器 mock 恒失败（刻意如此）。
// 所以「8 种格式各开一次全部可见」这件事只有在这一层能真证明：每种都断言**内容**（行号/像素/
// 单元格/PDF 页面），不只断言容器存在。
//
// 与既有探针的分工：
//   · e2e:pdf（verify_pdf_preview.mjs）= T1「可视类默认开 web 标签」那条产品口径（不改）；
//   · e2e:container / e2e:scoping = 视图池 / 会话隔离；
//   · 本脚本 = `file{mode:'preview'}` 里的渲染器注册表：8 种格式 + 8 张截图。
//
// 用法：cd desktop/app && npm run e2e:preview-native
//   PDF_E2E_SRC=...            真实 PDF 夹具（默认取仓库 desktop/ 下那份；缺失 → 退出码 2）
//   GO_CODE_E2E_FORCE_BUILD=1  强制 npm run build（改了 electron/ 或 src/ 后必须）
// 退出码：全通过 0；断言失败 1；**前置不满足 2**（PDF 夹具/受管运行时缺失、端口被占）。
import { _electron } from 'playwright'
import { spawn, spawnSync, execFileSync } from 'child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import zlib from 'node:zlib'
import { fileURLToPath } from 'node:url'
import { freePort, isPrecondition, waitProbePage } from './_serve.mjs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const REPO = path.resolve(APP, '../..')
const ELECTRON = path.join(APP, 'node_modules', '.bin', 'electron')
const SHOTS = path.join(__dirname, '_preview_renderers_shots')
const REAL_HOME = os.homedir()
const PY = path.join(REAL_HOME, '.go-code/runtime/python/venv/bin/python')

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const fail = []
const check = (cond, m, detail = '') => {
  if (cond) ok(m)
  else { fail.push(m); console.log(`  ❌ ${m} ${detail}`) }
}
const precondition = (m) => {
  console.error(`\n⛔ 前置不满足（退出码 2，不是测试失败）：${m}`)
  process.exit(2)
}
async function waitFor(fn, { timeout = 60000, interval = 250, label = '条件' } = {}) {
  const end = Date.now() + timeout
  let last
  while (Date.now() < end) {
    try { const v = await fn(); if (v) return v; last = v } catch (e) { last = e?.message }
    await sleep(interval)
  }
  throw new Error(`等待超时: ${label}（最后：${JSON.stringify(last)}）`)
}

// —— 前置：PDF 夹具 + 受管 Python 运行时 ——
const PDF_SRC = process.env.PDF_E2E_SRC || path.join(REPO, 'desktop', '百工2026年上半年经营分析.pdf')
if (!fs.existsSync(PDF_SRC)) {
  precondition(`PDF 夹具不存在 —— ${PDF_SRC}；用 PDF_E2E_SRC=... 指一份（保留中文名，顺带钉住 file:// 编码）`)
}
if (!fs.existsSync(PY)) {
  precondition(`受管 Python 运行时未就绪（${PY}）—— docx/pptx/xlsx 的真实内容没有它就无从来`
    + '（先跑 go test ./runtime/ -run TestRealEnsureInstallsRuntime）')
}

// —— 构建：主进程跑 dist-electron/main.js，renderer 由 dev server 提供源码 ——
const indexHtml = path.join(APP, 'dist', 'index.html')
const mainJs = path.join(APP, 'dist-electron', 'main.js')
if (process.env.GO_CODE_E2E_FORCE_BUILD === '1' || !fs.existsSync(indexHtml) || !fs.existsSync(mainJs)) {
  const b = spawnSync('npm', ['run', 'build'], { cwd: APP, encoding: 'utf8' })
  if (b.status !== 0) precondition(`npm run build 失败（不是断言失败）：\n${b.stdout}\n${b.stderr}`)
}
const gb = spawnSync('go', ['build', '-o', 'desktop/app/dist/hai-bridge', './desktop/bridge'], { cwd: REPO, encoding: 'utf8' })
if (gb.status !== 0) precondition(`go build 失败（不是断言失败）：\n${gb.stderr}`)

// —— 夹具工作区 ——
const HOME = fs.mkdtempSync(path.join(os.tmpdir(), 'prev-native-home-'))
const WS = fs.mkdtempSync(path.join(os.tmpdir(), 'prev-native-ws-'))
fs.mkdirSync(SHOTS, { recursive: true })

// 1) text
const TXT = 'notes.txt'
fs.writeFileSync(path.join(WS, TXT), Array.from({ length: 14 }, (_, i) => `第 ${i + 1} 行：TEXT-MARKER-${i + 1}`).join('\n') + '\n')
// 2) image：用 zlib 手写一张 64×32 的 PNG（不引依赖，尺寸写进断言）
const CRC_TABLE = (() => {
  const t = new Int32Array(256)
  for (let n = 0; n < 256; n++) {
    let c = n
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1
    t[n] = c
  }
  return t
})()
function pngChunk(type, data) {
  const len = Buffer.alloc(4); len.writeUInt32BE(data.length, 0)
  const body = Buffer.concat([Buffer.from(type, 'ascii'), data])
  let c = ~0
  for (const b of body) c = CRC_TABLE[(c ^ b) & 0xff] ^ (c >>> 8)
  const crc = Buffer.alloc(4); crc.writeUInt32BE((~c) >>> 0, 0)
  return Buffer.concat([len, body, crc])
}
const IMG_W = 64
const IMG_H = 32
const PNG = (() => {
  const raw = Buffer.alloc((IMG_W * 3 + 1) * IMG_H)
  for (let y = 0; y < IMG_H; y++) {
    raw[y * (IMG_W * 3 + 1)] = 0
    for (let x = 0; x < IMG_W; x++) {
      const i = y * (IMG_W * 3 + 1) + 1 + x * 3
      raw[i] = 200; raw[i + 1] = x * 4; raw[i + 2] = y * 8
    }
  }
  const ihdr = Buffer.alloc(13)
  ihdr.writeUInt32BE(IMG_W, 0); ihdr.writeUInt32BE(IMG_H, 4)
  ihdr[8] = 8; ihdr[9] = 2; ihdr[10] = 0; ihdr[11] = 0; ihdr[12] = 0
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    pngChunk('IHDR', ihdr),
    pngChunk('IDAT', zlib.deflateSync(raw)),
    pngChunk('IEND', Buffer.alloc(0)),
  ])
})()
fs.writeFileSync(path.join(WS, 'chart.png'), PNG)
// 3) csv
fs.writeFileSync(path.join(WS, 'sales.csv'), '区域,营收,增长\n华东,12800,12%\n华南,9600,8%\n华北,7400,-3%\n')
// 4) pdf（中文名保留 → 顺带钉住 file:// 的 URI 编码）
const PDF_NAME = '经营分析.pdf'
fs.copyFileSync(PDF_SRC, path.join(WS, PDF_NAME))
// 5) markdown
fs.writeFileSync(path.join(WS, 'guide.md'), '# 指南\n\n- 第一条 MARKDOWN-BULLET-1\n- 第二条 MARKDOWN-BULLET-2\n')
// 6) docx / pptx / xlsx：用**受管解释器**造（与生产转换同一份库）
const GEN = `
from docx import Document
d = Document()
d.add_heading("季度经营回顾", level=1)
p = d.add_paragraph("营收同比增长 ")
p.add_run("12%").bold = True
t = d.add_table(rows=2, cols=2); t.style = "Table Grid"
t.cell(0, 0).text = "指标"; t.cell(0, 1).text = "数值"
t.cell(1, 0).text = "营收"; t.cell(1, 1).text = "DOCX-CELL-12800"
d.save("spec.docx")

from pptx import Presentation
from pptx.util import Inches
pr = Presentation()
s1 = pr.slides.add_slide(pr.slide_layouts[1])
s1.shapes.title.text = "路线图"
s1.placeholders[1].text = "PPTX-MARKER-Q1 上线"
s2 = pr.slides.add_slide(pr.slide_layouts[5])
s2.shapes.title.text = "第二页"
pr.save("deck.pptx")

from openpyxl import Workbook
wb = Workbook(); ws = wb.active
ws["A1"] = "区域"; ws["B1"] = "营收"
ws["A2"] = "华东"; ws["B2"] = "XLSX-CELL-12800"
wb.save("book.xlsx")
print("GEN_OK")
`
try {
  const out = execFileSync(PY, ['-c', GEN], { cwd: WS, encoding: 'utf8' })
  if (!out.includes('GEN_OK')) precondition(`样本生成异常：${out}`)
} catch (e) {
  precondition(`样本生成失败（受管解释器报错）：${e.message}`)
}

// HOME 隔离（localStorage/单实例），但受管运行时按 HOME 解析 → 用软链把**真**运行时借进来，
// 否则 bridge 会去空的 HOME 里重新 bootstrap（联网装 42MB，e2e 不能这么干）。
fs.mkdirSync(path.join(HOME, '.go-code'), { recursive: true })
try {
  fs.symlinkSync(path.join(REAL_HOME, '.go-code', 'runtime'), path.join(HOME, '.go-code', 'runtime'), 'dir')
} catch (e) {
  precondition(`无法把受管运行时软链进隔离 HOME：${e.message}`)
}

// —— vite dev server（renderer 走源码：e2e 才能 import 到与真应用**同一个** store 实例）——
const DEV_PORT = await freePort()
const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(DEV_PORT), '--strictPort'], { cwd: APP, stdio: 'pipe' })
const cleanup = () => { try { server.kill('SIGTERM') } catch { /* 已退出 */ } }
process.on('exit', cleanup)

let app
try {
  // vite 的就绪判据用**应用入口**（dev server 的 index.html 带应用自己的 title 吗？不带 ——
  // 用 HTTP 200 + 探针页同一套「是我的服务器」判据：入口 200 且能取到 /src/main.tsx）。
  await waitProbePage(`http://localhost:${DEV_PORT}/`, { marker: '<div id="root">', probe: 'e2e:preview-native', child: server, timeoutMs: 120000 })

  app = await _electron.launch({
    executablePath: ELECTRON,
    args: [APP],
    cwd: APP,
    env: { ...process.env, HOME, HAI_DATA_DIR: HOME, VITE_DEV_SERVER_URL: `http://localhost:${DEV_PORT}/` },
  })
  const win = await app.firstWindow()
  const errs = []
  win.on('console', (m) => {
    const t = m.text()
    if (m.type() === 'error' && !/favicon/.test(t)) errs.push(t.slice(0, 200))
  })
  await win.waitForLoadState('domcontentloaded')
  await win.evaluate(([ws]) => { localStorage.setItem('go-code.projects', JSON.stringify({ paths: [ws], active: ws })) }, [WS])
  await win.reload()
  await waitFor(() => win.locator('button', { hasText: '设置' }).first().isVisible(), { label: '主界面就绪', timeout: 90000 })

  // 真实 store 实例（vite 模块注册表按 URL 去重 → 与真应用同一个）
  // 会话是异步建的（new_session 响应回填）—— 视图归属靠它。
  // 空工作区按规格**不**自动建会话（store「不建空会话」）→ 用真应用的 newSession 建一个。
  const sid0 = await win.evaluate(async () => {
    const m = await import('/src/store/useAppStore.ts')
    return m.useAppStore.getState().activeSessionId ?? ''
  })
  if (!sid0) {
    await win.evaluate(async () => {
      const m = await import('/src/store/useAppStore.ts')
      await m.useAppStore.getState().newSession()
    })
  }
  await waitFor(() => win.evaluate(async () => {
    const m = await import('/src/store/useAppStore.ts')
    return !!m.useAppStore.getState().activeSessionId
  }), { label: 'activeSessionId', timeout: 60000 })
  const alive = await win.evaluate(async () => {
    const m = await import('/src/store/useAppStore.ts')
    const s = m.useAppStore.getState()
    return { bridge: s.bridgeStatus, sid: s.activeSessionId ?? null, ws: s.workspace ?? null }
  })
  check(alive.bridge === 'running' && !!alive.sid, '驱动前提：拿到的是活 store（bridge running + 真会话）', JSON.stringify(alive))
  check(alive.ws === WS, `工作区已切到隔离目录（${alive.ws}）`, JSON.stringify(alive.ws))

  const openPreview = (rel) => win.evaluate(async (p) => {
    const m = await import('/src/store/useAppStore.ts')
    m.useAppStore.getState().openTab({ kind: 'file', path: p, mode: 'preview' })
    m.useAppStore.getState().setRightExpanded(true)
  }, rel)
  const shotWin = async (name) => {
    const file = path.join(SHOTS, name)
    await win.screenshot({ path: file })
    return file
  }
  const shots = []

  // ============================ 1. text ============================
  console.log('\n[1] text：notes.txt')
  await openPreview(TXT)
  await win.waitForSelector('[data-renderer="text"] .fp-code .fp-ln', { timeout: 60000 })
  {
    const s = await win.evaluate(() => {
      const host = document.querySelector('[data-renderer="text"]')
      const r = host?.getBoundingClientRect()
      return {
        lines: document.querySelectorAll('[data-renderer="text"] .fp-ln').length,
        text: host?.querySelector('.fp-code')?.textContent ?? '',
        w: Math.round(r?.width ?? 0), h: Math.round(r?.height ?? 0),
      }
    })
    check(s.lines >= 14, `行号 ${s.lines} 条（≥14）`, JSON.stringify(s.lines))
    check(s.text.includes('TEXT-MARKER-14'), '内容是真的（末行 marker 在）')
    check(s.w > 200 && s.h > 150, `容器有面积（${s.w}×${s.h}）`)
    shots.push(await shotWin('n1-text.png'))
  }

  // ============================ 2. image ============================
  console.log('\n[2] image：chart.png')
  await openPreview('chart.png')
  await win.waitForSelector('[data-renderer="image"] img', { timeout: 60000 })
  await waitFor(() => win.evaluate(() => {
    const img = document.querySelector('[data-renderer="image"] img')
    return !!img && img.complete && img.naturalWidth > 0
  }), { label: '图片解码', timeout: 30000 })
  {
    const s = await win.evaluate(() => {
      const img = document.querySelector('[data-renderer="image"] img')
      return { nw: img.naturalWidth, nh: img.naturalHeight, src: (img.getAttribute('src') ?? '').slice(0, 11) }
    })
    check(s.nw === IMG_W && s.nh === IMG_H, `图片真解码出尺寸 ${s.nw}×${s.nh}（夹具 64×32）`)
    check(s.src.startsWith('data:'), `走本地资产通道（data URL）`)
    shots.push(await shotWin('n2-image.png'))
  }

  // ============================ 3. csv ============================
  console.log('\n[3] csv：sales.csv')
  await openPreview('sales.csv')
  await win.waitForSelector('[data-renderer="csv"] .csv-preview .md table', { timeout: 60000 })
  {
    const s = await win.evaluate(() => {
      const rows = [...document.querySelectorAll('[data-renderer="csv"] .csv-preview .md table tr')]
      return { rows: rows.length, cells: rows.map((r) => [...r.querySelectorAll('th,td')].map((c) => c.textContent)) }
    })
    check(s.rows >= 4, `表格 ${s.rows} 行（表头 + 3 行数据）`)
    check(JSON.stringify(s.cells[1]) === JSON.stringify(['华东', '12800', '12%']), `逐格对上夹具：${JSON.stringify(s.cells[1])}`)
    shots.push(await shotWin('n3-csv.png'))
  }

  // ============================ 4. pdf（原生视图）============================
  console.log('\n[4] pdf：经营分析.pdf（Chromium 内嵌视图，真渲染）')
  await openPreview(PDF_NAME)
  await win.waitForSelector('[data-renderer="pdf"]', { timeout: 60000 })
  await waitFor(() => win.evaluate(() => document.querySelector('[data-renderer="pdf"] [data-pdf-state="ready"]') != null), { label: 'pdf 视图就绪', timeout: 60000 })
  const pdfAbs = path.join(WS, PDF_NAME)
  const pdfUrl = 'file://' + pdfAbs.split('/').map(encodeURIComponent).join('/')
  {
    const s = await win.evaluate(() => {
      const host = document.querySelector('[data-renderer="pdf"]')
      const box = host?.querySelector('.pv-pdf')?.getBoundingClientRect()
      return { w: Math.round(box?.width ?? 0), h: Math.round(box?.height ?? 0), state: host?.querySelector('[data-pdf-state]')?.getAttribute('data-pdf-state') ?? '' }
    })
    check(s.w > 200 && s.h > 150, `pdf 宿主有面积（${s.w}×${s.h}）`)
    check(s.state === 'ready', `data-pdf-state=ready（实得 ${s.state}）`)
    // 视图池里真的有一条属于这个 pdf、且是激活/可见态（导航要时间 → 等它落定）
    const poolState = await waitFor(() => win.evaluate(async ([abs, url]) => {
      const st = await window.desktop.embedStatus() // 注意：preload 侧是 ipcRenderer.invoke → Promise
      const t = (st?.targets ?? []).find((x) => x.srcPath === abs)
      if (!t) return null
      if (t.url !== url || t.loading) return null
      return { id: t.id, url: t.url, sid: t.sid, visible: !!t.visible, activeId: st?.activeId ?? null }
    }, [pdfAbs, pdfUrl]), { label: 'pdf 视图加载完成', timeout: 60000 })
    check(!!poolState && poolState.url === pdfUrl, '视图池里有这条 pdf，URL 是逐段编码的 file://（中文名不炸）', JSON.stringify(poolState))
    check(!!poolState && poolState.activeId === poolState.id && poolState.visible === true, '它就是激活且可见的那一条', JSON.stringify(poolState))
    const viewDom = await win.evaluate(() => {
      const el = document.querySelector('[data-renderer="pdf"] [data-pdf-state]')
      return { view: el?.getAttribute('data-pdf-view') ?? '', created: el?.getAttribute('data-pdf-created') ?? '' }
    })
    check(viewDom.view === poolState?.id, `面板挂的就是这条视图（data-pdf-view=${viewDom.view}）`, JSON.stringify(viewDom))
    // CDP 读**激活视图**：它真的是 PDF 文档 + 插件铺满
    const probe = async (expr) => {
      const res = await win.evaluate((e) => window.desktop.embedCommand({ method: 'Runtime.evaluate', params: { expression: e, returnByValue: true } }), expr)
      return res?.result?.result?.value ?? res?.result?.value ?? res?.value ?? null
    }
    const contentType = await waitFor(async () => {
      const v = await probe('document.contentType')
      return v === 'application/pdf' ? v : null
    }, { label: 'contentType=application/pdf', timeout: 30000 })
    check(contentType === 'application/pdf', `CDP：contentType=${contentType}（Chromium 当 PDF 打开）`)
    const box = await waitFor(async () => {
      const v = await probe('(() => { const e = document.querySelector("embed[type=\'application/pdf\']"); return e ? Math.round(e.getBoundingClientRect().width) + "x" + Math.round(e.getBoundingClientRect().height) : "" })()')
      return /\d+x\d+/.test(String(v)) && !String(v).startsWith('0x') ? v : null
    }, { label: 'PDF 插件盒子', timeout: 30000 })
    check(/\d+x\d+/.test(String(box)), `PDF 插件铺满视图（${box}）`)
    // 截图取**原生视图**自己（窗口截图拍不到 WebContentsView —— 两条合成通道）。
    // 纸张占比要轮询：插件盒子在、页面还在排版（capturePage 会拍到尚未绘制的白底/灰底）。
    let shot = null
    await waitFor(async () => {
      shot = await app.evaluate(async ({ BrowserWindow }, arg) => {
        const w = BrowserWindow.getAllWindows()[0]
        const kids = (w.contentView?.children ?? []).filter((c) => c.webContents && typeof c.webContents.getURL === 'function')
        const v = kids.find((c) => c.webContents.getURL() === arg.url)
        if (!v) return null
        const img = await v.webContents.capturePage()
        const bmp = img.toBitmap()
        let paper = 0
        for (let i = 0; i < bmp.length; i += 4) if (bmp[i] > 200 && bmp[i + 1] > 200 && bmp[i + 2] > 200) paper++
        return { size: img.getSize(), paper: paper / (bmp.length / 4), png: img.toPNG().toString('base64') }
      }, { url: pdfUrl })
      return shot && shot.paper > 0.05
    }, { label: 'PDF 页面画出（纸张占比 > 5%）', timeout: 30000, interval: 400 })
    check(!!shot && shot.size.width > 0 && shot.size.height > 0, '原生视图 capturePage 拿到像素（没崩）', JSON.stringify(shot?.size))
    check((shot?.paper ?? 0) > 0.05, `PDF 页面真的画出来了（纸张占比 ${((shot?.paper ?? 0) * 100).toFixed(1)}% > 5%）`)
    if (shot) {
      const p = path.join(SHOTS, 'n4-pdf.png')
      fs.writeFileSync(p, Buffer.from(shot.png, 'base64'))
      shots.push(p)
    }
  }

  // ============================ 5~7. docx / pptx / xlsx ============================
  const officeCases = [
    { name: 'docx', file: 'spec.docx', want: ['季度经营回顾', '营收同比增长', 'DOCX-CELL-12800'], table: true, shot: 'n5-docx.png' },
    { name: 'pptx', file: 'deck.pptx', want: ['路线图', 'PPTX-MARKER-Q1 上线', '第二页'], table: false, shot: 'n6-pptx.png' },
    { name: 'xlsx', file: 'book.xlsx', want: ['区域', '营收', 'XLSX-CELL-12800'], table: true, shot: 'n7-xlsx.png' },
  ]
  for (const [i, c] of officeCases.entries()) {
    console.log(`\n[${5 + i}] ${c.name}：${c.file}（真 bridge 转换 → 沙箱 iframe）`)
    await openPreview(c.file)
    await win.waitForSelector(`[data-renderer="${c.name}"][data-state="ready"]`, { timeout: 90000 })
    await win.waitForSelector(`[data-renderer="${c.name}"] .pv-office[data-office-state="ready"]`, { timeout: 90000 })
    const s = await waitFor(() => win.evaluate((kind) => {
      const frame = document.querySelector(`[data-renderer="${kind}"] .pv-office-frame`)
      const doc = frame?.contentDocument
      const box = frame?.getBoundingClientRect()
      return doc && doc.body && (doc.body.textContent ?? '').length > 0
        ? {
          text: doc.body.textContent ?? '',
          html: doc.body.innerHTML.length,
          rows: doc.querySelectorAll('table tr').length,
          cells: [...doc.querySelectorAll('table td, table th')].map((x) => x.textContent.trim()).filter(Boolean),
          w: Math.round(box?.width ?? 0), h: Math.round(box?.height ?? 0),
          truncated: document.querySelector(`[data-renderer="${kind}"] .pv-office`)?.getAttribute('data-office-truncated'),
        }
        : null
    }, c.name), { label: `${c.name} 产物内容`, timeout: 60000 })
    for (const w of c.want) check(s.text.includes(w), `${c.name} 产物里有「${w}」`, JSON.stringify(s.text.slice(0, 120)))
    check(s.html > 80, `${c.name} iframe 里有真 DOM（${s.html} 字节 HTML）`)
    check(s.w > 200 && s.h > 100, `${c.name} iframe 铺开（${s.w}×${s.h}）`)
    if (c.table) check(s.rows >= 2 && s.cells.some((x) => /12800/.test(x)), `${c.name} 表格真的渲染出行/单元格（${s.rows} 行）`, JSON.stringify(s.cells.slice(0, 8)))
    check(s.truncated === '0', `${c.name} 没有被 1 MiB 截断（data-office-truncated=0）`)
    shots.push(await shotWin(c.shot))
  }

  // ============================ 8. markdown ============================
  console.log('\n[8] markdown：guide.md（R08a 临时态：走 text，仍必须可读）')
  await openPreview('guide.md')
  await win.waitForSelector('[data-renderer="text"] .fp-code .fp-ln', { timeout: 60000 })
  {
    const s = await win.evaluate(() => {
      const host = document.querySelector('[data-renderer="text"]')
      return {
        lines: document.querySelectorAll('[data-renderer="text"] .fp-ln').length,
        text: host?.querySelector('.fp-code')?.textContent ?? '',
        md: document.querySelectorAll('.md-preview-doc').length,
      }
    })
    check(s.lines >= 4, `行号 ${s.lines} 条（≥4）`)
    check(s.text.includes('MARKDOWN-BULLET-2'), '内容可读（bullet marker 在）')
    check(s.md === 0, 'markdown 渲染器仍未接入（R08a 临时态，本轮不改）')
    shots.push(await shotWin('n8-markdown.png'))
  }

  check(errs.length === 0, `渲染进程无 console error（${errs.length ? errs.join(' | ') : 'clean'}）`)
  check(shots.length === 8, `8 张截图都写出来了（实得 ${shots.length}）`)

  console.log('\n截图：')
  for (const p of shots) console.log('  ' + p)
  await app.close()
} catch (e) {
  if (isPrecondition(e)) {
    try { await app?.close() } catch { /* 忽略 */ }
    precondition(e.message)
  }
  console.error('\n❌ ' + (e?.message ?? e))
  fail.push(e?.message ?? String(e))
} finally {
  try { await app?.close() } catch { /* 已关闭 */ }
  cleanup()
}

if (fail.length) {
  console.error(`\n❌ ${fail.length} 项失败：\n - ` + fail.join('\n - '))
  process.exit(1)
}
console.log(`\n✅ 八种格式验收：${pass} 项全部通过（8 张截图在 ${SHOTS}）`)
process.exit(0)
