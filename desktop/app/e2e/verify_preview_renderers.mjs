#!/usr/bin/env node
// 预览渲染器注册表的链内探针（R08a text/image/csv → R08b 补齐 pdf/docx/pptx/xlsx + markdown 复核）
//
// 为什么必须有这一层（04-review F45/F48 的教训）：注册表单测用的是假渲染器，证明不了
// 「真实面板走的是注册表那条路」。这里做三件事：
//   ① 正面：真实 store + 真实 RightPanel + 真实 surface，开 file{mode:'preview'} 与
//      file{mode:'source'} 两种标签，断言 `data-renderer` 命中正确 id **且内容真的渲染出来**
//      （只有容器没有内容 = 假绿：`.csv-preview table` / `.fp-code .fp-ln` / `<img>` 的
//      naturalWidth 都要断言）；
//   ② 专用格式的**失败口径**：pdf/docx/pptx/xlsx 在本环境（浏览器 mock，没有受管 Python、
//      没有 WebContentsView）必须落到 `data-renderer="unavailable"`，且 `data-renderer-id`
//      指名「是谁接不了」、`data-reason` 写清缺哪条通道 —— 不许白屏、不许一句笼统「加载失败」；
//   ③ 变异（F45 的判据）：weight 调小 → 赢家变了；依赖假装可用（= 删掉挂载前那道 unavailable
//      分支）→ 渲染器真挂上了、真拿不到通道，仍必须落在 unavailable；清空注册表 → 连旧路径
//      都必须失效。任何一条没红，前面所有绿色都是伪证。
//
// 本链**只覆盖能在这台浏览器里真渲染的格式**：pdf/office 的真实内容需要原生视图与受管运行时，
// 由 Electron 层验收（e2e/verify_preview_renderers_native.mjs，8 种格式 + 8 张截图）。
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
import * as React from 'react'
import { createRoot } from 'react-dom/client'
import RightPanel from '../../src/components/RightPanel'
import WorkspaceFiles from '../../src/components/WorkspaceFiles'
// ⚠️ 副作用注册，必须与 src/main.tsx 一致（04-review F45/F46）：少了这一行 getSurface() 全
// undefined，面板静默走 ?? 兜底 —— 文件面板的兜底恰好就是同一个 FilePreviewTab，像素一致、
// 断言全绿。下面 [0] 的自检断言会当场戳穿这件事。
import '../../src/surfaces/builtin'
// 渲染器注册表、loader、依赖探测暴露给探针：下面 [6][7] 的变异要能真的改注册表/依赖状态。
// 只在**探针页**里暴露，产品代码不挂任何 window 全局。
import * as previewRegistry from '../../src/surfaces/preview/registry'
import * as previewLoader from '../../src/surfaces/preview/loader'
import * as previewDeps from '../../src/surfaces/preview/deps'
import { getSurface } from '../../src/surfaces/registry'
import { useAppStore } from '../../src/store/useAppStore'
import '../../src/index.css'

useAppStore.setState({ workspace: '/mock/workspace' })
;(window as unknown as { __store: unknown }).__store = useAppStore
// 变异 A 的对照组渲染器要造一个真组件（page.evaluate 里 import('react') 解析不到 —— 没有打包器）
;(window as unknown as { __react: unknown }).__react = React
;(window as unknown as { __preview: unknown }).__preview = {
  registry: previewRegistry, loader: previewLoader, deps: previewDeps,
}
;(window as unknown as { __surfaces: unknown }).__surfaces = {
  file: !!getSurface('file'),
  // 仪器自检用：八个渲染器是否真的注册（缺 renderers 副作用 import 时这里是 []）
  renderers: previewRegistry.listRenderers().map((r) => r.id),
}
void useAppStore.getState().start('/mock/workspace')

createRoot(document.getElementById('root')!).render(
  <div style={{ height: '100vh', display: 'flex', minWidth: 0 }}>
    {/* 左栏挂**真实** WorkspaceFiles：下面 [10] 的「预览」入口是文件树右键菜单，
        复刻一份菜单证明不了接线（F45 的老教训）。 */}
    <div id="lp" style={{ width: 320, flex: '0 0 auto', minWidth: 0, borderRight: '1px solid var(--border)' }}>
      <WorkspaceFiles onBack={() => {}} />
    </div>
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

// waitFor 等一个选择器出现，**返回等待耗时**（ms），超时的报错里带上「当时面板长什么样」。
//
// 为什么要专门包一层（2026-11 实测）：`.fp-code .fp-ln` 是 shiki 高亮完成后的行号 gutter。
// 本探针跑在 **vite dev server** 上，shiki 的语法文件是**按需转换**的 —— 冷启动（尤其机器
// 负载高、同时挂着几套 vite/chrome 时）首屏确实要几十秒；而 20s 的硬超时一到，报错只有
// 一句 waitForSelector timeout，看不出「是在高亮中」还是「渲染器根本没接上」。
// 现在：超时预算 45s，报错里带 data-state / 行数 / 面板文本长度，成功时也打印等了多久
// ——「慢」与「等不到」从此可区分，且慢的那次不再算红。
const waitFor = async (page, selector, label, timeout = 45000) => {
  const t0 = Date.now()
  try {
    await page.waitForSelector(selector, { timeout })
  } catch (e) {
    const st = await page.evaluate(() => {
      const host = document.querySelector('[data-renderer="text"]')
      const r = host?.getBoundingClientRect()
      return {
        renderer: host?.getAttribute('data-renderer') ?? '<none>',
        state: host?.getAttribute('data-state') ?? '',
        lines: document.querySelectorAll('.fp-code .fp-ln').length,
        bodyLen: (document.querySelector('.right-body')?.textContent ?? '').trim().length,
        w: Math.round(r?.width ?? 0), h: Math.round(r?.height ?? 0),
      }
    })
    throw new Error(`${label}：${timeout}ms 内没出现 ${selector}；当时面板 = ${JSON.stringify(st)}`)
  }
  const ms = Date.now() - t0
  ok(`${label}（等了 ${ms}ms）`)
  return ms
}

// 当前面板的落点快照（unavailable 的 id/reason、任何 data-renderer 的值、面板是否空白）
const landingOf = (page) => page.evaluate(() => {
  const el = document.querySelector('[data-renderer="unavailable"]')
  return {
    ids: [...document.querySelectorAll('[data-renderer]')].map((x) => x.getAttribute('data-renderer')),
    byId: el?.getAttribute('data-renderer-id') ?? '',
    reason: el?.getAttribute('data-reason') ?? '',
    text: (el?.textContent ?? '').trim(),
    body: (document.querySelector('.right-body')?.textContent ?? '').trim().length,
  }
})

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

  // —— [0] 仪器自检 ——
  console.log('\n[0] 仪器自检：探针页自己走的是注册表那条路')
  const boot = await page.evaluate(() => window.__surfaces)
  assert(boot?.file === true, `builtin file provider 已注册（否则验的是 ?? 兜底；实得 ${JSON.stringify(boot)}）`)
  // R08c 起是八个：media 是新增的那个（音视频走 hai-media://，浏览器里没有这条通道）。
  assert(JSON.stringify(boot?.renderers) === JSON.stringify(['csv', 'image', 'text', 'pdf', 'docx', 'pptx', 'xlsx', 'media']),
    `八个渲染器在册（实得 ${JSON.stringify(boot?.renderers)}）`)
  const deps0 = await page.evaluate(() => ({
    conversion: window.__preview.registry.dependencyStatus('conversion'),
    embed: window.__preview.registry.dependencyStatus('embed'),
    media: window.__preview.registry.dependencyStatus('media'),
  }))
  assert(deps0.conversion?.ok === false && deps0.embed?.ok === false && deps0.media?.ok === false,
    `本环境（浏览器 mock）三条依赖都不可用且已探测（conversion=${JSON.stringify(deps0.conversion)} media=${JSON.stringify(deps0.media)}）`)
  ok('依赖探测真的跑过（不是注册表里的默认值）')

  // —— [1] text ——
  console.log('\n[1] text 渲染器：.go → data-renderer="text" + 行号 + 内容')
  await openTab(page, 'auth.go', 'preview')
  await waitRenderer(page, 'text')
  await waitFor(page, '[data-renderer="text"] .fp-code .fp-ln', 'text 行号 gutter 出现')
  let s = await page.evaluate(() => {
    const host = document.querySelector('[data-renderer="text"]')
    const r = host?.getBoundingClientRect()
    return {
      lines: document.querySelectorAll('[data-renderer="text"] .fp-code .fp-ln').length,
      text: host?.querySelector('.fp-code')?.textContent ?? '',
      w: Math.round(r?.width ?? 0), h: Math.round(r?.height ?? 0),
      state: host?.getAttribute('data-state'),
    }
  })
  assert(s.lines > 5, `行号元素 ${s.lines} 个（>5）`)
  assert(s.text.includes('package auth'), '内容是真的（首行 package auth）')
  assert(s.w > 200 && s.h > 200, `容器有面积（${s.w}×${s.h}）`)
  assert(s.state === 'ready', `data-state=ready（实得 ${s.state}）`)
  const shotText = await shot(page, '01-text-preview.png')

  // —— [2] image ——
  console.log('\n[2] image 渲染器：.png → <img> 真解码（naturalWidth）')
  await openTab(page, 'brand/screenshot-main.png', 'preview')
  await waitRenderer(page, 'image')
  await page.waitForSelector('[data-renderer="image"] img', { timeout: 20000 })
  await page.waitForFunction(() => {
    const img = document.querySelector('[data-renderer="image"] img')
    return !!img && img.complete && img.naturalWidth > 0
  }, { timeout: 20000 })
  s = await page.evaluate(() => {
    const img = document.querySelector('[data-renderer="image"] img')
    return { nw: img.naturalWidth, nh: img.naturalHeight, src: (img.getAttribute('src') ?? '').slice(0, 12) }
  })
  assert(s.nw === 120 && s.nh === 40, `图片真的解码出尺寸 ${s.nw}×${s.nh}（mock 固定 120×40）`)
  assert(s.src.startsWith('data:'), `走的是本地资产通道（data URL，实得 ${s.src}…）`)
  const shotImage = await shot(page, '02-image-preview.png')

  // —— [3] csv ——
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

  // —— [4] markdown 复核（R08a 的临时态，本轮**不改**）——
  console.log('\n[4] markdown 临时态复核：.md 仍走 text，且内容可读（R08b 不改它）')
  await openTab(page, 'docs/guide.md', 'preview')
  await waitRenderer(page, 'text')
  await waitFor(page, '[data-renderer="text"] .fp-code .fp-ln', 'markdown 行号 gutter 出现')
  const md = await page.evaluate(() => {
    const host = document.querySelector('[data-renderer="text"]')
    const code = host?.querySelector('.fp-code')
    return {
      lines: document.querySelectorAll('[data-renderer="text"] .fp-code .fp-ln').length,
      chars: (code?.textContent ?? '').trim().length,
      md: document.querySelectorAll('.md-preview-doc').length,
    }
  })
  assert(md.lines >= 2, `.md 渲染出行号 ${md.lines} 条（≥2）`)
  assert(md.chars > 10, `.md 内容可读（${md.chars} 字符）`)
  assert(md.md === 0, 'markdown 渲染器仍未接入（R08a 临时态：走 text 源码视图）')
  const shotMd = await shot(page, '04-markdown-text.png')

  // —— [5] 四种专用格式的失败口径（本环境缺通道 → 写明缺什么）——
  console.log('\n[5] pdf/docx/pptx/xlsx：缺通道时 data-renderer="unavailable" + 指名是谁 + 写清缺什么')
  const heavy = [
    ['report.pdf', 'pdf', /内嵌浏览器视图|WebContentsView/],
    ['docs/spec.docx', 'docx', /转换通道|watchDocx/],
    ['docs/deck.pptx', 'pptx', /转换通道|watchDocx/],
    ['out/book.xlsx', 'xlsx', /转换通道|watchDocx/],
  ]
  const reasons = {}
  for (const [p, id, re] of heavy) {
    await openTab(page, p, 'preview')
    await waitRenderer(page, 'unavailable')
    await page.evaluate(() => new Promise((r) => setTimeout(r, 160)))
    const now = await landingOf(page)
    assert(now.byId === id, `${p}: 落点指名渲染器 ${id}（实得 ${JSON.stringify(now.byId)}）`)
    assert(re.test(now.reason), `${p}: 原因点到具体缺什么 —— ${now.reason}`)
    assert(now.text.includes('预览不可用'), `${p}: 占位有标题（不是白屏）`)
    assert(now.ids.every((x) => x === 'unavailable'), `${p}: 没有别的渲染器同时挂上（实得 ${JSON.stringify(now.ids)}）`)
    reasons[id] = now.reason
  }
  assert(reasons.pdf !== reasons.docx, 'pdf 与 office 的缺失理由不能是同一句笼统文案')
  assert(reasons.docx === reasons.pptx && reasons.pptx === reasons.xlsx, 'office 三种格式共用同一条转换通道理由（同一根因别写成三句）')
  const shotHeavy = await shot(page, '05-heavy-unavailable.png')

  // —— [6] 变异 A：weight 调小 → 被同一个文件的另一个渲染器抢走 ——
  console.log('\n[6] 变异 A（weight）：pdf 100 → 5，同扩展名的另一个渲染器必须接管')
  // 先证明 pdf 与 text 的先后确实是 weight 说了算（不带 binary 时两者都接 .pdf）
  const beforePick = await page.evaluate(() => {
    const R = window.__preview.registry
    // 不带 binary 的 FileRef = 文件面板宿主里的形态（FilePreview 传的是 fileRefOf(path, len)）
    return { id: R.pickRenderer(R.fileRefOf('report.pdf', 0))?.id ?? '' }
  })
  assert(beforePick.id === 'pdf', `改之前：不带 binary 的 .pdf 由 pdf 接（实得 ${beforePick.id}）`)
  await page.evaluate(() => {
    const R = window.__preview.registry
    const orig = R.getRenderer('pdf')
    window.__preview.pdfOrig = orig
    R.unregisterRenderer('pdf')
    R.registerRenderer({ ...orig, weight: 5 })
  })
  const afterPick = await page.evaluate(() => {
    const R = window.__preview.registry
    return {
      id: R.pickRenderer(R.fileRefOf('report.pdf', 0))?.id ?? '',
      // 说实话：pdf 的 match 只看扩展名（不看 binary），所以二进制形态仍然只有它接得住。
      binaryId: R.pickRenderer(R.fileRefOf('report.pdf', 0, true))?.id ?? '',
    }
  })
  assert(afterPick.id === 'text', `改之后：不带 binary 的 .pdf 被 text 抢走（实得 ${afterPick.id}）`)
  assert(afterPick.binaryId === 'pdf', `二进制形态仍由 pdf 接（match 只看扩展名；实得 ${afterPick.binaryId}）`)
  await page.evaluate(() => {
    const R = window.__preview.registry
    R.unregisterRenderer('pdf')
    R.registerRenderer(window.__preview.pdfOrig)
  })

  // 面板层的同一件事：注册一个**同扩展名、低 weight** 的对照组渲染器（模拟将来多渲染器竞争的常态），
  // 再把 pdf 的 weight 压到它下面 —— 抢走必须是**可见的**（data-renderer 换成对照组、内容真的在）。
  await page.evaluate(async () => {
    const R = window.__preview.registry
    R.registerRenderer({
      id: 'pdf-rival',
      weight: 50,
      match: (f) => (f.ext === 'pdf' ? 1 : 0),
      load: async () => {
        const R = window.__react
        return () => R.createElement('div', { 'data-testid': 'rival' }, 'RIVAL-RENDERER')
      },
    })
    const R2 = window.__preview.registry
    const orig = R2.getRenderer('pdf')
    R2.unregisterRenderer('pdf')
    R2.registerRenderer({ ...orig, weight: 5 })
  })
  await openTab(page, 'report.pdf', 'preview')
  await waitRenderer(page, 'pdf-rival')
  await page.waitForSelector('[data-renderer="pdf-rival"] [data-testid="rival"]', { timeout: 20000 })
  const rival = await page.evaluate(() => {
    const host = document.querySelector('[data-renderer="pdf-rival"]')
    const r = host?.getBoundingClientRect()
    return {
      text: host?.textContent ?? '',
      w: Math.round(r?.width ?? 0),
      unavailable: document.querySelectorAll('[data-renderer="unavailable"]').length,
    }
  })
  assert(rival.text.includes('RIVAL-RENDERER'), `weight 变小后由对照组渲染器接管且内容真的在（${rival.text}）`)
  assert(rival.w > 200, `对照组容器有面积（宽 ${rival.w}）`)
  assert(rival.unavailable === 0, '此时 pdf 渲染器没被选中，也就没有它的 unavailable 占位')
  const shotWeight = await shot(page, '06-mutation-weight.png')
  // 还原：pdf 回到 100，撤掉对照组
  await page.evaluate(() => {
    const R = window.__preview.registry
    R.unregisterRenderer('pdf')
    R.registerRenderer(window.__preview.pdfOrig)
    R.unregisterRenderer('pdf-rival')
  })
  const restored = await page.evaluate(() => {
    const R = window.__preview.registry
    return {
      pick: R.pickRenderer(R.fileRefOf('report.pdf', 0))?.id ?? '',
      n: R.listRenderers().length,
    }
  })
  assert(restored.pick === 'pdf' && restored.n === 8, `还原成功（pdf=${restored.pick} 在册=${restored.n}）`)

  // —— [7] 变异 B：把「挂载前拦截」删掉 → 渲染器真挂上，但仍必须落在 unavailable ——
  console.log('\n[7] 变异 B（unavailable 分支）：依赖假装可用 → 渲染器真挂上了、真拿不到通道')
  await page.evaluate(() => {
    const R = window.__preview.registry
    R.setDependencyStatus('conversion', { ok: true })
    R.setDependencyStatus('embed', { ok: true })
  })
  const forged = await page.evaluate(() => ({
    conversion: window.__preview.registry.dependencyStatus('conversion'),
    embed: window.__preview.registry.dependencyStatus('embed'),
  }))
  assert(forged.conversion?.ok === true && forged.embed?.ok === true, '依赖被伪造成 ok（等价于删掉挂载前那道拦截）')
  const forgedCases = [
    ['docs/spec.docx', 'docx', /不支持 docx 转换|需桌面端受管运行时/],
    ['report.pdf', 'pdf', /embedOpenTarget|内嵌浏览器视图/],
  ]
  for (const [p, id, re] of forgedCases) {
    await openTab(page, p, 'preview')
    await waitRenderer(page, 'unavailable')
    await page.evaluate(() => new Promise((r) => setTimeout(r, 300)))
    const now = await landingOf(page)
    assert(now.byId === id, `${p}: 渲染器真的挂上了又失败了，落点仍指名 ${id}（实得 ${JSON.stringify(now.byId)}）`)
    assert(re.test(now.reason), `${p}: 原因是渲染器自己上报的真实失败 —— ${now.reason}`)
    assert(now.ids.every((x) => x === 'unavailable'), `${p}: 同屏没有残留的 data-renderer="docx" 容器（实得 ${JSON.stringify(now.ids)}）`)
    assert(now.body > 0, `${p}: 面板不是白屏`)
  }
  const shotUnavailable = await shot(page, '07-mutation-forced-deps.png')
  // 还原依赖状态
  await page.evaluate(() => window.__preview.deps.detectPreviewDeps())
  const restoredDeps = await page.evaluate(() => window.__preview.registry.dependencyStatus('conversion'))
  assert(restoredDeps?.ok === false, `依赖状态还原（实得 ${JSON.stringify(restoredDeps)}）`)

  // —— [8] 旧路径（FilePreview）也被注册表接管 ——
  console.log('\n[8] 旧路径保留但已走注册表：mode=\'source\' 的 FilePreview 分支')
  await openTab(page, 'auth.go', 'source')
  await waitFor(page, '.file-source [data-renderer="text"] .fp-code .fp-ln', '文件面板源码模式 gutter 出现')
  s = await page.evaluate(() => ({
    text: document.querySelectorAll('.file-source [data-renderer="text"]').length,
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

  // —— [9] 产品入口（R08b Commit B）：文件树右键「预览」+ 两个标签标题可区分 ——
  console.log('\n[9] 产品入口：文件树右键「预览」真的能进 preview 标签；同名 source 标签标题可区分')
  // 先把标签收干净：标签条在 10+ 个标签时会窄到只剩两个字（shortName 截断），
  // 截图就分辨不出「预览 vs 源码」—— 而这一节的判据正是「用户能不能分清」。
  await page.evaluate(() => {
    const st = window.__store.getState()
    const tabs = Object.values(st.tabsByScope).flatMap((x) => x?.tabs ?? [])
    for (const t of tabs) if (t.kind !== 'activity') st.closeTab(t.id)
  })
  await page.evaluate(() => new Promise((r) => setTimeout(r, 300)))

  // 9a 菜单顺序：前两项仍是「打开 / 新建文件夹」（G5 的 ↑↓ 口径），「预览」在后面
  const rowSel = (name) => `#lp .lf-row:has-text("${name}")`
  await page.locator(rowSel('auth.go')).first().click({ button: 'right' })
  await page.waitForSelector('.lf-menu', { timeout: 20000 })
  const menu = await page.evaluate(() => {
    const items = [...document.querySelectorAll('.lf-menu .ctx-item')].map((b) => (b.textContent ?? '').trim())
    return { items, previewIdx: items.findIndex((x) => x === '预览') }
  })
  assert(menu.items[0] === '打开', `菜单第一项仍是「打开」（实得 ${JSON.stringify(menu.items.slice(0, 3))}）`)
  assert(/新建文件夹/.test(menu.items[1] ?? ''), `第二项仍是「新建文件夹」（实得 ${JSON.stringify(menu.items.slice(0, 3))}）`)
  assert(menu.previewIdx > 1, `「预览」在后面、没插队（下标 ${menu.previewIdx}）`)

  // 9b 点「预览」→ 建 preview 标签 + 面板真的按注册表渲染
  await page.locator('.lf-menu [data-preview="1"]').click()
  await page.waitForSelector('.right-body [data-preview-mode="preview"] [data-renderer="text"]', { timeout: 20000 })
  const prevTab = await page.evaluate(() => {
    const st = window.__store.getState()
    const set = st.tabsByScope[Object.keys(st.tabsByScope).find((k) => st.tabsByScope[k]?.tabs?.some((x) => x.kind === 'file' && x.mode === 'preview')) ?? '']
    const active = set?.tabs?.find((x) => x.id === set.activeId)
    return { mode: active?.mode ?? '', path: active?.path ?? '' }
  })
  assert(prevTab.mode === 'preview' && /auth\.go$/.test(prevTab.path), `激活的是 preview 标签（${JSON.stringify(prevTab)}）`)

  // 9c 同一个文件再「打开」→ 多出一个 source 标签；两个标题必须能区分
  await page.locator(rowSel('auth.go')).first().click({ button: 'right' })
  await page.waitForSelector('.lf-menu', { timeout: 20000 })
  await page.locator('.lf-menu .ctx-item').first().click()
  await page.waitForSelector('.right-body [data-renderer="text"]', { timeout: 20000 })
  const titles = await page.evaluate(() => [...document.querySelectorAll('.rtab .rtab-t')].map((x) => (x.textContent ?? '').trim()))
  const fileTitles = titles.filter((x) => x.includes('auth.go') || x.includes('auth'))
  const withTag = fileTitles.filter((x) => x.includes('预览'))
  assert(withTag.length === 1, `有一个标签标题带「预览」后缀（实得 ${JSON.stringify(fileTitles)}）`)
  assert(fileTitles.length >= 2, `同一个文件的两个标签都在标签条上（实得 ${JSON.stringify(fileTitles)}）`)
  assert(new Set(fileTitles).size === fileTitles.length, `两个标题互不相同（实得 ${JSON.stringify(fileTitles)}）`)
  const modes = await page.evaluate(() => {
    const st = window.__store.getState()
    const all = Object.values(st.tabsByScope).flatMap((x) => x?.tabs ?? []).filter((x) => x.kind === 'file' && /auth\.go$/.test(x.path ?? ''))
    return all.map((x) => x.mode ?? 'source')
  })
  assert(modes.includes('preview') && modes.includes('source'), `标签身份按 (path, mode) 区分（实得 ${JSON.stringify(modes)}）`)
  // 标签窄到只剩两个字时标题会被截 —— 图标必须能替代（预览 = 眼睛，source = 文件）
  const icons = await page.evaluate(() => [...document.querySelectorAll('.rtab[data-kind="file"]')]
    .filter((x) => (x.getAttribute('data-mode') ?? '') === 'preview')
    .map((x) => (x.querySelector('.rtab-ico svg path, .rtab-ico svg circle')?.getAttribute('d') ?? '').slice(0, 24)))
  assert(icons.length > 0 && icons.every((d) => d.startsWith('M2 12s3.5-7')), `预览标签用眼睛图标（可截断后仍可辨；实得 ${JSON.stringify(icons.slice(0, 2))}）`)

  const shotEntry = await shot(page, '09-product-entry.png')

  // 9d T1 口径未改：可视类文件的「打开」仍落**内嵌浏览器（web 标签）**，不建 file 标签。
  // 本环境没有原生视图通道（web 标签会如实显示「实时视图不可用」），但**标签类型**这一步
  // 是纯前端决策 —— 它就是 T1 的口径，能在链里钉住；真渲染仍由 e2e:pdf 守（不在本轮改）。
  const before = await page.evaluate(() => {
    const st = window.__store.getState()
    return Object.values(st.tabsByScope).flatMap((x) => x?.tabs ?? []).filter((x) => x.kind === 'file').length
  })
  const activeOf = () => page.evaluate(() => {
    const st = window.__store.getState()
    const sets = Object.values(st.tabsByScope)
    const tabs = sets.flatMap((x) => x?.tabs ?? [])
    const active = sets.flatMap((x) => (x?.tabs ?? []).filter((t) => t.id === x.activeId))[0]
    return { fileTabs: tabs.filter((t) => t.kind === 'file').length, activeKind: active?.kind ?? '' }
  })
  await page.evaluate(() => window.__store.getState().openFile('report.pdf'))
  await page.evaluate(() => new Promise((r) => setTimeout(r, 300)))
  const pdfOpen = await activeOf()
  assert(pdfOpen.fileTabs === before, `openFile(pdf) 不新建 file 标签（${before} → ${pdfOpen.fileTabs}）`)
  assert(pdfOpen.activeKind === 'web', `pdf 的「打开」落 web 标签（实得 ${JSON.stringify(pdfOpen)}；内嵌视图渲染由 e2e:pdf 守）`)
  await page.evaluate(() => window.__store.getState().openFile('brand/screenshot-main.png'))
  await page.evaluate(() => new Promise((r) => setTimeout(r, 300)))
  const pngOpen = await activeOf()
  assert(pngOpen.fileTabs === before && pngOpen.activeKind === 'web', `图片同口径（不建 file 标签、落 web；实得 ${JSON.stringify(pngOpen)}）`)

  // —— [10] 变异 C（F45 标准动作）：清空注册表 → 两条路都必须变成 unavailable ——
  console.log('\n[10] 变异 C：清空注册表，同样的面板必须变成 unavailable')
  await page.evaluate(() => {
    window.__preview.registry.resetRenderersForTest()
    window.__preview.loader.resetRendererCacheForTest()
  })
  const emptied = await page.evaluate(() => window.__preview.registry.listRenderers().length)
  assert(emptied === 0, `注册表已清空（listRenderers().length = ${emptied}）`)

  await openTab(page, 'auth_test.go', 'preview')
  await waitRenderer(page, 'unavailable')
  await page.evaluate(() => new Promise((r) => setTimeout(r, 160)))
  s = await landingOf(page)
  assert(!s.ids.includes('text') && !s.ids.includes('csv') && !s.ids.includes('image'),
    `清空后没有任何渲染器挂载（实得 ${JSON.stringify(s.ids)}）`)
  assert(s.reason.includes('.go'), `占位写明了缺哪种格式（${s.reason}）`)
  assert(s.body > 0, '面板不是空白')

  await openTab(page, 'auth_test.go', 'source')
  await page.waitForSelector('.file-source [data-renderer="unavailable"]', { timeout: 20000 })
  s = await page.evaluate(() => ({
    inSource: !!document.querySelector('.file-source [data-renderer="unavailable"]'),
    text: document.querySelectorAll('[data-renderer="text"]').length,
    fpCode: document.querySelectorAll('.file-source .fp-code').length,
  }))
  assert(s.inSource && s.text === 0 && s.fpCode === 0, 'FilePreview 的纯文本分支也失效了（硬编码分支已被搬空）')
  const shotMutant = await shot(page, '08-mutation-registry-empty.png')

  // —— [11] 持久化往返：preview 标签能存能恢复（结构版本不变，不需要迁移）——
  console.log('\n[11] 持久化往返：刷新页面后 preview 标签按同一份结构恢复')
  // 先造一个 preview 标签（注册表在 [10] 被清空了也没关系：这里验的是**标签**能不能恢复）
  await openTab(page, 'auth.go', 'preview')
  await page.waitForTimeout(900) // 持久化有去抖（schedulePersistTabs），等它落盘
  const persisted = await page.evaluate(() => {
    const raw = localStorage.getItem('go-code.sessionTabs') ?? ''
    return { hasPreview: /"mode":"preview"/.test(raw), v: (JSON.parse(raw || '{}') || {}).v ?? null }
  })
  assert(persisted.hasPreview, 'localStorage 里有 mode:"preview"（toPersistedTab 带上 mode）')
  assert(persisted.v === 1, `持久化结构版本仍是 1（不需要 +1 的迁移；实得 ${persisted.v}）`)
  await page.reload({ waitUntil: 'domcontentloaded' })
  await page.waitForFunction(() => !!window.__store, { timeout: 30000 })
  await page.evaluate(() => window.__store.getState().setRightExpanded(true))
  await page.waitForSelector('.rtab[data-mode="preview"]', { timeout: 20000 })
  const restoredTabs = await page.evaluate(() => [...document.querySelectorAll('.rtab[data-mode="preview"]')]
    .map((x) => (x.querySelector('.rtab-t')?.textContent ?? '').trim()))
  assert(restoredTabs.length >= 1 && restoredTabs.some((x) => x.includes('预览')),
    `刷新后 preview 标签真的恢复出来了（${JSON.stringify(restoredTabs)}）`)

  assert(errs.length === 0, `无页面错误（${errs.length ? errs.join(' | ') : 'clean'}）`)
  await browser.close()

  console.log('\n截图：')
  for (const p of [shotText, shotImage, shotCsv, shotMd, shotHeavy, shotWeight, shotUnavailable, shotMutant, shotEntry]) console.log('  ' + p)
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
