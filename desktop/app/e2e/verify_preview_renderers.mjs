#!/usr/bin/env node
// 预览渲染器注册表（R08a）的 e2e 探针：text / image / csv 三个渲染器 + unavailable 落点。
//
// 为什么必须有这一层（04-review F45/F48 的教训）：注册表单测用的是假渲染器，证明不了
// 「真实面板走的是注册表那条路」。这里做两件事：
//   ① 正面：真实 store + 真实 RightPanel + 真实 surface，开 file{mode:'preview'} 与
//      file{mode:'source'} 两种标签，断言 `data-renderer` 命中正确 id **且内容真的渲染出来**
//      （只有容器没有内容 = 假绿：`.csv-preview table` / `.fp-code .fp-ln` / `<img>` 的
//      naturalWidth 都要断言）；
//   ② 变异（F45 的判据）：把注册表**清空**，同样两条路必须变成 `data-renderer="unavailable"`
//      并写明原因。若清空后仍然渲染出 csv/text/image，说明面板上有一条绕过注册表的硬编码分支，
//      前面所有绿色都是伪证。
//
// 用法：node e2e/verify_preview_renderers.mjs
// 退出码：全通过 0；断言失败 1；前置不满足（探针页起不来/端口被占）2 —— 不许静默 skip。
import { spawn } from 'child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'
import { freePort, isPrecondition, waitProbePage } from './_serve.mjs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const SHOTS = path.join(__dirname, '_preview_renderers_shots')

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

// —— 探针页 ——
// 走 **mock transport**（不注入 window.desktop）：本探针要的是 file_preview 的样例内容与
// fs:read-image 的固定尺寸图（mock 侧 120×40），这两条都在 mock 里，且与真 bridge 同契约。
const PROBE_MARKER = '<title>preview renderers probe</title>'
const PROBE_DIR = path.join(__dirname, '_previewprobe_run')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8">'
  + PROBE_MARKER
  + '</head><body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import RightPanel from '../../src/components/RightPanel'
// ⚠️ 副作用注册，必须与 src/main.tsx 一致（04-review F45/F46）：少了这一行 getSurface() 全
// undefined，面板静默走 ?? 兜底 —— 文件面板的兜底恰好就是同一个 FilePreviewTab，像素一致、
// 断言全绿。下面 [0] 的自检断言会当场戳穿这件事。
import '../../src/surfaces/builtin'
// 渲染器注册表与 loader 暴露给探针：下面 [6] 的「清空变异」要能真的把注册表清空。
// 只在**探针页**里暴露，产品代码不挂任何 window 全局。
import * as previewRegistry from '../../src/surfaces/preview/registry'
import * as previewLoader from '../../src/surfaces/preview/loader'
import { getSurface } from '../../src/surfaces/registry'
import { useAppStore } from '../../src/store/useAppStore'
import '../../src/index.css'

useAppStore.setState({ workspace: '/mock/workspace' })
;(window as unknown as { __store: unknown }).__store = useAppStore
;(window as unknown as { __preview: unknown }).__preview = { registry: previewRegistry, loader: previewLoader }
;(window as unknown as { __surfaces: unknown }).__surfaces = {
  file: !!getSurface('file'),
  // 仪器自检用：三个渲染器是否真的注册（缺 renderers 副作用 import 时这里是 []）
  renderers: previewRegistry.listRenderers().map((r) => r.id),
}
void useAppStore.getState().start('/mock/workspace')

createRoot(document.getElementById('root')!).render(
  <div style={{ height: '100vh', display: 'flex', minWidth: 0 }}>
    <div style={{ flex: '1 1 auto', minWidth: 0 }} />
    <div id="rp" style={{ width: 860, height: '100vh', minWidth: 0, borderLeft: '1px solid var(--border)' }}>
      <RightPanel />
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

const port = await freePort()
const PROBE_URL = `http://localhost:${port}/e2e/_previewprobe_run/index.html`
const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(port), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)

// 打开一个标签：走真实 store 动作（openTab → 展开右栏 + 建标签 → 面板 ensureFilePreview
// → file_preview 命令 → 渲染）。不直接 setState 伪造终态。
const openTab = (page, filePath, mode) => page.evaluate(({ p, m }) => {
  const st = window.__store
  st.getState().openTab({ kind: 'file', path: p, mode: m })
}, { p: filePath, m: mode })

// 把一个面板截出来（右栏本体）：整页截图里左边是一片空白，人眼复核要的是面板。
async function shot(page, name) {
  fs.mkdirSync(SHOTS, { recursive: true })
  const box = await page.locator('#rp').boundingBox()
  const file = path.join(SHOTS, name)
  await page.screenshot({ path: file, ...(box ? { clip: box } : {}) })
  return file
}

const waitRenderer = (page, id, timeout = 20000) => page.waitForSelector(`[data-renderer="${id}"]`, { timeout })

async function main() {
  await waitProbePage(PROBE_URL, { marker: PROBE_MARKER, probe: 'e2e:preview-renderers', child: server })
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1280, height: 860 } })
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  page.on('console', (m) => { if (m.type() === 'error' && !/favicon/.test(m.location()?.url || '')) errs.push(m.text().slice(0, 200)) })

  await page.goto(PROBE_URL, { waitUntil: 'domcontentloaded' })
  await page.waitForSelector('aside.right.collapsed', { timeout: 30000 })
  await page.waitForFunction(() => !!window.__store && !!window.__preview, { timeout: 20000 })

  // —— [0] 仪器自检（F45）：先证明「注册表里有东西」且「面板真的来自 provider」——
  console.log('\n[0] 仪器自检：探针页自己走的是注册表那条路')
  const inst = await page.evaluate(() => window.__surfaces)
  assert(!!inst?.file, `builtin file provider 已注册（否则验的是 ?? 兜底；实得 ${JSON.stringify(inst)}）`)
  assert(
    JSON.stringify(inst.renderers) === JSON.stringify(['csv', 'image', 'text']),
    `渲染器注册表非空且就是本轮的三个（实得 ${JSON.stringify(inst.renderers)}）`,
  )

  // —— [1] text（mode='preview'，注册表选路）——
  console.log('\n[1] text 渲染器：.go 文件 → data-renderer="text" + 真 shiki 行号')
  await openTab(page, 'auth.go', 'preview')
  await waitRenderer(page, 'text')
  await page.waitForFunction(() => document.querySelectorAll('[data-renderer="text"] .fp-code .fp-ln').length > 0, { timeout: 20000 })
  let s = await page.evaluate(() => ({
    state: document.querySelector('[data-renderer="text"]')?.getAttribute('data-state'),
    inSource: !!document.querySelector('.file-source [data-renderer="text"]'),
    lines: document.querySelectorAll('[data-renderer="text"] .fp-code .fp-ln').length,
    code: (document.querySelector('[data-renderer="text"] .fp-code')?.textContent ?? '').slice(0, 40),
    previewMode: document.querySelector('.file-source')?.getAttribute('data-preview-mode'),
    unavailable: document.querySelectorAll('[data-renderer="unavailable"]').length,
  }))
  assert(s.state === 'ready', `容器进入 ready（实得 ${s.state}）`)
  assert(s.previewMode === 'preview', `渲染的是 mode='preview' 面板（实得 ${s.previewMode}）`)
  assert(s.inSource && s.lines > 5, `text 渲染器在 .file-source 里挂载且行号 gutter 真的生成了（${s.lines} 行）`)
  assert(s.code.includes('package auth'), `内容真的是 auth.go 的高亮源码（前 40 字：${JSON.stringify(s.code)}）`)
  assert(s.unavailable === 0, '正常路径不出现 unavailable')
  const shotText = await shot(page, '01-text-preview.png')

  // —— [2] image（<img> + 失败占位）——
  console.log('\n[2] image 渲染器：.png → data-renderer="image" + <img> 真的解码出来')
  await openTab(page, 'out/chart.png', 'preview')
  await waitRenderer(page, 'image')
  await page.waitForFunction(() => {
    const img = document.querySelector('[data-renderer="image"] img')
    return !!img && img.complete && img.naturalWidth > 0
  }, { timeout: 20000 })
  s = await page.evaluate(() => {
    const img = document.querySelector('[data-renderer="image"] img')
    return {
      w: img?.naturalWidth ?? 0,
      h: img?.naturalHeight ?? 0,
      objectFit: img ? getComputedStyle(img).objectFit : '',
      boxW: img?.clientWidth ?? 0,
      srcKind: (img?.getAttribute('src') ?? '').slice(0, 5),
      failed: document.querySelectorAll('.preview-image-empty[data-state="failed"]').length,
      path: document.querySelector('[data-renderer="image"] .preview-image-path')?.textContent ?? '',
    }
  })
  // mock 的 fs:read-image 固定回 120×40 的 SVG（见 src/transport/mock.ts 的断言锚点注释）
  assert(s.w === 120 && s.h === 40, `图片真的解码出来了（naturalWidth×Height = ${s.w}×${s.h}）`)
  assert(s.srcKind === 'data:', `走的是主进程 read-image 的 data URL 通道（src 前缀 ${s.srcKind}）`)
  // 不许被容器拉变形：契约是 object-fit:contain（元素盒子可以不是 3:1，内容按比例缩放留白）。
  // 只断言机制，不去猜像素：内容区无法用 DOM 直接量，量盒子比例反而会得出错误结论（实测过）。
  assert(s.objectFit === 'contain', `图片按比例缩放而不是拉变形（object-fit=${s.objectFit}）`)
  assert(s.boxW > 400, `图片铺开了整宽（img 宽 ${s.boxW}px）`)
  assert(s.failed === 0, '没有落到失败占位')
  assert(s.path.includes('chart.png'), `路径可读（${s.path}）`)
  const shotImage = await shot(page, '02-image-preview.png')

  // —— [3] csv（复用 CsvPreview）——
  console.log('\n[3] csv 渲染器：.csv → data-renderer="csv" + 真表格（不是源码）')
  await openTab(page, 'out/sales.csv', 'preview')
  await waitRenderer(page, 'csv')
  await page.waitForSelector('[data-renderer="csv"] .csv-preview .md table', { timeout: 20000 })
  s = await page.evaluate(() => {
    const rows = [...document.querySelectorAll('[data-renderer="csv"] .csv-preview .md table tr')]
    return {
      rows: rows.length,
      first: rows[1] ? [...rows[1].querySelectorAll('th,td')].map((c) => c.textContent) : [],
      notes: document.querySelector('[data-renderer="csv"] .csv-notes')?.textContent ?? '',
      fpCode: document.querySelectorAll('[data-renderer="csv"] .fp-code').length,
    }
  })
  assert(s.rows >= 6, `表格行数 = ${s.rows}（首行表头 + 5 行数据 + 可能的分隔）`)
  assert(JSON.stringify(s.first) === JSON.stringify(['苹果', '3', '5.5', '便宜, 常买']), `逐格对上 mock 的 sales.csv：${JSON.stringify(s.first)}`)
  assert(s.notes.length > 0, '说明条在（首行当表头这件事必须写明白）')
  assert(s.fpCode === 0, '不是源码视图（csv 渲染器没有抢占成 text）')
  const shotCsv = await shot(page, '03-csv-preview.png')

  // —— [4] unavailable：没有渲染器的格式（R08a 边界：pdf 交给 R08b）——
  console.log('\n[4] unavailable 落点：.pdf 本轮无渲染器 → 写明原因的占位（不白屏）')
  await openTab(page, 'report.pdf', 'preview')
  await waitRenderer(page, 'unavailable')
  await page.waitForTimeout(300)
  s = await page.evaluate(() => {
    const el = document.querySelector('[data-renderer="unavailable"]')
    return {
      reason: el?.getAttribute('data-reason') ?? '',
      text: (el?.textContent ?? '').trim(),
      byId: el?.getAttribute('data-renderer-id'),
      blank: (document.querySelector('.right-body')?.textContent ?? '').trim().length === 0,
    }
  })
  assert(/\.pdf/.test(s.reason), `原因写明了具体格式（${s.reason}）`)
  assert(!s.byId, `没有渲染器命中时 data-renderer-id 为空（实得 ${s.byId}）`)
  assert(!s.blank, '面板不是空白（有可读文案）')
  assert(s.text.includes('预览不可用'), `占位有标题（${s.text.slice(0, 30)}）`)
  const shotUnavailable = await shot(page, '04-unavailable-no-renderer.png')

  // —— [5] 旧路径（FilePreview）也被注册表接管 ——
  console.log('\n[5] 旧路径保留但已走注册表：mode=\'source\' 的 FilePreview 分支')
  await openTab(page, 'auth.go', 'source')
  await page.waitForSelector('.file-source [data-renderer="text"] .fp-code .fp-ln', { timeout: 20000 })
  s = await page.evaluate(() => ({
    text: document.querySelectorAll('.file-source [data-renderer="text"]').length,
    // 活动列表内嵌路径的容器（renderFinalFile 用的是同一个宿主）
    lines: document.querySelectorAll('.file-source [data-renderer="text"] .fp-ln').length,
  }))
  assert(s.text === 1 && s.lines > 5, `FilePreview 的纯文本分支经注册表挂载（${s.lines} 行）`)

  // CSV 的「表格 / 纯文本」开关：only 收窄的证据 —— 纯文本模式下不能被 weight 更高的 csv 抢走
  await page.evaluate(() => { window.__store.getState().openFile('out/sales.csv', undefined, 'source') })
  await page.waitForSelector('.file-source [data-renderer="csv"] .csv-preview .md table', { timeout: 20000 })
  ok('FilePreview 的 CSV 表格分支经注册表挂载（data-renderer="csv"）')
  await page.locator('.md-view-switch button', { hasText: '纯文本' }).first().click()
  await page.waitForSelector('.file-source [data-renderer="text"] .fp-code', { timeout: 20000 })
  s = await page.evaluate(() => ({
    csv: document.querySelectorAll('.file-source [data-renderer="csv"]').length,
    text: document.querySelectorAll('.file-source [data-renderer="text"]').length,
  }))
  assert(s.csv === 0 && s.text === 1, `切「纯文本」后由 text 渲染器接（csv=${s.csv} text=${s.text}）`)
  await page.locator('.md-view-switch button', { hasText: '表格' }).first().click()
  await page.waitForSelector('.file-source [data-renderer="csv"] .csv-preview .md table', { timeout: 20000 })
  ok('切回「表格」仍由 csv 渲染器接（双向可逆）')

  // —— [6] F45 变异：清空注册表 → 两条路都必须变成 unavailable ——
  console.log('\n[6] 变异检验（F45）：清空注册表，同样的面板必须变成 unavailable')
  await page.evaluate(() => {
    window.__preview.registry.resetRenderersForTest()
    window.__preview.loader.resetRendererCacheForTest()
  })
  const emptied = await page.evaluate(() => window.__preview.registry.listRenderers().length)
  assert(emptied === 0, `注册表已清空（listRenderers().length = ${emptied}）`)

  // 6a. 新开一个 preview 标签（auth_test.go 是纯文本）：面板必须给 unavailable
  await openTab(page, 'auth_test.go', 'preview')
  await waitRenderer(page, 'unavailable')
  await page.waitForTimeout(300)
  s = await page.evaluate(() => ({
    ids: [...document.querySelectorAll('[data-renderer]')].map((el) => el.getAttribute('data-renderer')),
    reason: document.querySelector('[data-renderer="unavailable"]')?.getAttribute('data-reason') ?? '',
    bodyText: (document.querySelector('.right-body')?.textContent ?? '').trim().length,
  }))
  assert(!s.ids.includes('text') && !s.ids.includes('csv') && !s.ids.includes('image'), `清空后没有任何渲染器挂载（实得 ${JSON.stringify(s.ids)}）`)
  assert(s.reason.includes('.go'), `占位写明了缺哪种格式（${s.reason}）`)
  assert(s.bodyText > 0, '面板不是空白')

  // 6b. 旧路径同样失效（auth_test.go 的 source 标签 = FilePreviewTab）
  await openTab(page, 'auth_test.go', 'source')
  await page.waitForSelector('.file-source [data-renderer="unavailable"]', { timeout: 20000 })
  s = await page.evaluate(() => ({
    inSource: !!document.querySelector('.file-source [data-renderer="unavailable"]'),
    text: document.querySelectorAll('[data-renderer="text"]').length,
    fpCode: document.querySelectorAll('.file-source .fp-code').length,
  }))
  assert(s.inSource && s.text === 0 && s.fpCode === 0, 'FilePreview 的纯文本分支也失效了（硬编码分支已被搬空）')
  const shotMutant = await shot(page, '05-mutation-unavailable.png')

  assert(errs.length === 0, `无页面错误（${errs.length ? errs.join(' | ') : 'clean'}）`)
  await browser.close()

  console.log('\n截图：')
  for (const p of [shotText, shotImage, shotCsv, shotUnavailable, shotMutant]) console.log('  ' + p)
  console.log(`\n✅ 预览渲染器：${pass} 项全部通过`)
}

let code = 0
try {
  await main()
} catch (e) {
  if (!isPrecondition(e)) {
    console.error('\n❌ ' + (e?.message ?? e))
    code = 1
  } else {
    // 前置不满足 ≠ 断言失败（00 §4.6）：退出码 2
    console.error(`\n⛔ 前置不满足（退出码 2，不是测试失败）：${e.message}`)
    code = 2
  }
} finally {
  cleanup()
}
process.exit(code)
