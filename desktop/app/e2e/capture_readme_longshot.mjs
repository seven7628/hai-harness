// README.zh-CN.md 预览长图（宣传素材）：只拍右侧「最终文件」的 Markdown 预览，
// 高度 = 文档完整展开后的自然高度，不受 900px 视口截断。
//
// 与 capture_brand_readme_shot.mjs 的区别：那个拍 1440×900 视口（左聊天 + 右预览，
// 用于视频画面）；这个只要文档本身，所以把右栏拉满、临时解除预览容器的滚动限制
// （.md-preview-doc 的 overflow:auto + flex:1 会把长文档压在视口里滚不动），
// 再按 scrollHeight 撑大视口整页截。
//
// 走浏览器 mock（无 Electron / 无 API key）：内容来自 mock.ts 的 MOCK_README_ZH，
// 与真 bridge 从磁盘读同一份 README.zh-CN.md，图片经 Vite 资源 URL 改写后能正常显示。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import net from 'net'
import fs from 'fs'

const APP = path.resolve(import.meta.dirname, '..')
const OUT = path.resolve(APP, '../../brand/screenshot-readme-zh.png')
const PORT = 4199
const PROBE = path.join(APP, 'e2e', '_shot_run')

async function waitPort(port, timeoutMs = 30000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const up = await new Promise((res) => {
      const s = net.connect({ port, host: 'localhost' })
      const t = setTimeout(() => { s.destroy(); res(false) }, 500)
      s.on('connect', () => { clearTimeout(t); s.destroy(); res(true) })
      s.on('error', () => { clearTimeout(t); s.destroy(); res(false) })
    })
    if (up) return
    if (Date.now() > deadline) throw new Error('vite 未就绪')
    await new Promise((r) => setTimeout(r, 250))
  }
}

fs.mkdirSync(PROBE, { recursive: true })
fs.writeFileSync(path.join(PROBE, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8"><title>shot</title></head><body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import App from '../../src/App'
import { useAppStore } from '../../src/store/useAppStore'
import '../../src/index.css'
;(window as unknown as { __store: unknown }).__store = useAppStore
createRoot(document.getElementById('root')!).render(<App />)
`)
fs.writeFileSync(path.join(PROBE, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
const REPO = path.resolve(APP, '../..')
// fs.allow 必须放到仓库根：mock 把 README 里的 brand/*.png|svg 换成了 Vite 资源 URL，
// dev 下是 /@fs/<仓库根>/brand/… —— 只放 APP 会被 403 挡掉（图出不来）。
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [REPO] } } })
`)

const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
const cleanup = () => { try { server.kill('SIGTERM') } catch {} fs.rmSync(PROBE, { recursive: true, force: true }) }
process.on('exit', cleanup)

const browser = await chromium.launch({ headless: true, channel: 'chrome' })
// DPI 由文档高度反推，不是拍脑袋定的。
//
// **硬约束**：Chromium 单张纹理上限 16384px（超高页面超过就不再渲染，但截图仍按请求
// 尺寸补白 —— 于是「文件尺寸完全正确、DOM 断言全过、像素却停在半路」，极难发现）。
// 实测：文档 8401px 高，DSF=1（8401px）完整；DSF=2（16802px）尾部被截；DSF=3（25203px）更糟。
//
// 所以 DSF 取 min(想要的清晰度, floor(16384 / 文档高))，且 ≥1。整篇 8400px 的文档
// 最多只能到 1x —— 想更高清就得**分段截**（见 --split），不能靠加 DSF。
// 环境变量可强制：LONG_SHOT_DSF=2 …（超出上限时下方 assert 会拦下）
const DSF_WANT = Number(process.env.LONG_SHOT_DSF || 2)
const TEX_LIMIT = 16384
const page = await browser.newPage({ viewport: { width: 1440, height: 1200 }, deviceScaleFactor: 1 })
const errs = []
page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
page.on('response', (r) => { if (r.status() >= 400 && !/favicon/.test(r.url())) errs.push(`HTTP ${r.status()} ${r.url()}`) })

await waitPort(PORT)
await page.goto(`http://localhost:${PORT}/e2e/_shot_run/index.html`, { waitUntil: 'load' })
// 等 mock 自动进假工作区
await page.waitForFunction(() => (window).__store?.getState?.().workspace, null, { timeout: 30000 })

// 打开「最终文件」预览：走真实动作链 openFile(path)，不伪造 store 终态
await page.evaluate(async () => {
  const st = window.__store
  st.getState().setRightExpanded?.(true)
  st.setState({ rightTab: 'file', fileFocus: undefined, filePreviews: {} })
  st.getState().syncTabsToRightTab?.()
  await new Promise((r) => setTimeout(r, 300))
  st.getState().openFile('/mock/workspace/README.zh-CN.md')
  await new Promise((r) => setTimeout(r, 1500))
})
await page.waitForSelector('.md-preview-doc .md', { timeout: 30000 })

// 只留文档的样式：隐藏非文档面板 + 解除整条祖先链的滚动/裁切 + 文档铺满。
// 测量页与出图页都要用，故提成常量。
//
// 为什么不用鼠标拖分隔条（早期版本试过，不可靠）：面板宽度由 react-resizable-panels 的
// flex 基准决定（实测 flex: 25.14 1 0px），拖到 x=40 后又被 minSize/maxSize 约束弹回，
// 文档仍只有 367px 宽 —— 长图里表格和代码块全被压成窄条，读不了。
// 这里改成按 data-panel 定位 + display:none，绕开布局引擎。
const LONG_SHOT_CSS = `
    /* 非文档部分：左栏 / 中栏 / 分隔条 / 右栏头部一律不出镜 */
    .main-group > [data-panel]:not(:has(.md-preview-doc)),
    .main-group > .resize-handle,
    .file-source-meta { display: none !important; }
    .statusbar { display: none !important; }
    /* 外层壳解除「视口高 + 裁切」。**必须逐个祖先都解**：.md-preview-doc 往上还有
       .file-inspector-body(overflow:auto) → .file-side-detail(hidden) → .right-body(hidden)
       → .right(hidden) → 两个无类名容器(hidden / auto)，其中最外一个高度锁在视口高（1200px）
       —— 只解 .md-preview-doc 一层时，内容在 1200px 处被裁掉，长图尾部整段丢失
       （实测底部只剩「执行链路」表格，联系方式/许可证两节压根没拍到）。 */
    .app-root, .main, .main-group,
    .right, .right-body, .file-side-detail, .file-inspector-body,
    .file-source, .file-source .md-preview-doc {
      height: auto !important; min-height: 0 !important;
      overflow: visible !important; flex: none !important;
    }
    /* 文档所在面板独占 100% 宽 */
    .main-group > [data-panel]:has(.md-preview-doc) { flex: 1 1 100% !important; width: 100% !important; }
    .file-source .md-preview-doc > .md { max-width: 100% !important; }
  `

// 启动屏（.boot）在浏览器 mock 里永不卸载（App.tsx 的 onEnter 要真点「进入工作区」），
// 解除高度限制后它会被算进文档高度 → 长图底部多出一整屏重复的 logo+标题。
// 用 JS 直接摘节点，比 CSS 隐藏可靠：它是 .app-root.leaving 下的 absolute 覆盖层，
// 且 reveal/done class 随动画变化。摘完顺手清掉 app-root 的 leaving 态。
await page.addStyleTag({ content: LONG_SHOT_CSS })
await page.evaluate(() => {
  document.querySelectorAll('.boot').forEach((e) => e.remove())
  document.querySelector('.app-root')?.classList.remove('leaving')
})
await page.waitForTimeout(1500)

// 宽度取 1200：.md 内部有 1100px 阅读宽度上限，1200 既让上限生效（版式与真实预览
// 一致，不会把行拉得太长），又留得下 padding。
const W = 1200

// 等图真正解码完（naturalWidth>0）：不等会拍到「图还没加载」的一帧，且图片撑开的高度
// 会在截图后才定型 → 尾部被裁。
await page.waitForFunction(() => {
  const imgs = [...document.querySelectorAll('.md-preview-doc img')]
  return imgs.length > 0 && imgs.every((i) => i.complete && i.naturalWidth > 0)
}, null, { timeout: 30000 }).catch(() => console.log('⚠️ 有图片未加载完，继续截图'))

// 视口**一次开到足够高**，再量高度、再截。
//
// 千万别写成「量一次 → 调视口 → 再量」的循环（早期版本那么写，结果像素停在文档中段）：
// 每次 setViewportSize 都会让整棵布局重排，量出来的值是**上一轮**布局的，
// 截图时布局又变了 —— DOM 断言（读实时文档）全过，像素却是旧的，实测底部少了
// 「联系方式 / 许可证」两节。单次开大视口让 height:100% 的祖先一次到位，量与绘同源。
const TALL = 9500 // 文档约 8400px，留足余量；超出的部分只是空白，不影响 clip
await page.setViewportSize({ width: W, height: TALL })
await page.waitForTimeout(2500)

const doc = await page.evaluate(() => {
  const d = document.querySelector('.md-preview-doc')
  if (!d) return null
  const r = d.getBoundingClientRect()
  return {
    x: Math.floor(r.x), y: Math.floor(r.y),
    w: Math.ceil(r.width), h: Math.ceil(r.height),
    imgs: document.querySelectorAll('.md-preview-doc img').length,
    broken: [...document.querySelectorAll('.md-preview-doc img')].filter((i) => !i.naturalWidth).length,
    lastHead: (() => {
      const hs = [...d.querySelectorAll('.md h2')]
      return (hs[hs.length - 1]?.textContent || '').trim()
    })(),
  }
})
if (!doc) throw new Error('未找到 .md-preview-doc')
console.log('文档尺寸：', JSON.stringify(doc))

// 尾部完整性自检：README.zh-CN.md 最后一节固定是「许可证」。历史上出过两次静默截断
// （BootScreen 混进高度 / 祖先 overflow 裁切），都表现为「图出来了但尾部没了」——
// 光看文件大小发现不了，所以在这里硬断言，截断了就让脚本失败而不是产出一张废图。
if (!/许可证/.test(doc.lastHead)) {
  throw new Error(`长图尾部被截断：最后一个 h2 是「${doc.lastHead}」，应为「许可证」`)
}
if (doc.broken > 0) throw new Error(`有 ${doc.broken} 张图没加载出来`)
if (doc.y + doc.h > TALL) throw new Error(`文档底部 ${doc.y + doc.h} 超出视口 ${TALL}，请调大 TALL`)

// —— 出图 ——
// 清晰度策略：整篇单张 vs 分段。
//   单张：像素高 = doc.h × DSF，必须 ≤ TEX_LIMIT，否则尾部静默丢失（见上面 TEX_LIMIT 注释）。
//   分段：每段独立，段高 × DSF 各自受限 → 能用满 DSF，清晰度更高。
const fitOneShot = Math.max(1, Math.floor(TEX_LIMIT / doc.h))
const SPLIT = process.argv.includes('--split')
const effDsf = SPLIT ? DSF_WANT : Math.min(DSF_WANT, fitOneShot)

if (!SPLIT && DSF_WANT > fitOneShot) {
  console.log(`⚠️  文档 ${doc.h}px 高，整篇单张最多 ${fitOneShot}x（再高就撞 ${TEX_LIMIT}px 纹理上限，尾部会被静默截断）；已自动降到 ${effDsf}x。`)
  console.log(`    想要更清晰：node e2e/capture_readme_longshot.mjs --split   （分段出图，每段 ${Math.floor(TEX_LIMIT / effDsf)}px）`)
}
if (effDsf * doc.h > TEX_LIMIT && !SPLIT) {
  throw new Error(`DSF ${effDsf} × 文档高 ${doc.h} = ${effDsf * doc.h}px，超过纹理上限 ${TEX_LIMIT}px`)
}

// 视口已开到 TALL（≥ 文档底），clip 的 rect 完全落在视口内 —— 早期版本视口只比文档高
// 120px，clip 请求的区域超出可捕获范围，Playwright 静默只给视口内那截。
// （DSF 用 newContext 指定：deviceScaleFactor 在 page 上建好后不可改。）
const shotCtx = await browser.newContext({ viewport: { width: W, height: TALL }, deviceScaleFactor: effDsf })
const shotPage = await shotCtx.newPage()
await shotPage.goto(`http://localhost:${PORT}/e2e/_shot_run/index.html`, { waitUntil: 'load' })
await shotPage.waitForFunction(() => (window).__store?.getState?.().workspace, null, { timeout: 30000 })
await shotPage.evaluate(async () => {
  const st = window.__store
  st.getState().setRightExpanded?.(true)
  st.setState({ rightTab: 'file', fileFocus: undefined, filePreviews: {} })
  st.getState().syncTabsToRightTab?.()
  await new Promise((r) => setTimeout(r, 300))
  st.getState().openFile('/mock/workspace/README.zh-CN.md')
  await new Promise((r) => setTimeout(r, 1500))
})
await shotPage.waitForSelector('.md-preview-doc .md', { timeout: 30000 })
await shotPage.waitForFunction(() => {
  const imgs = [...document.querySelectorAll('.md-preview-doc img')]
  return imgs.length > 0 && imgs.every((i) => i.complete && i.naturalWidth > 0)
}, null, { timeout: 30000 }).catch(() => console.log('⚠️ 有图片未加载完，继续截图'))

// 复用同一套「只留文档」的样式
await shotPage.addStyleTag({ content: LONG_SHOT_CSS })
await shotPage.evaluate(() => {
  document.querySelectorAll('.boot').forEach((e) => e.remove())
  document.querySelector('.app-root')?.classList.remove('leaving')
})
await shotPage.waitForTimeout(2500)

const geo = await shotPage.evaluate(() => {
  const d = document.querySelector('.md-preview-doc')
  const r = d.getBoundingClientRect()
  return { x: Math.floor(r.x), y: Math.floor(r.y), w: Math.ceil(r.width), h: Math.ceil(r.height) }
})

fs.mkdirSync(path.dirname(OUT), { recursive: true })
if (SPLIT) {
  const segH = Math.floor(TEX_LIMIT / effDsf) // 每段像素高不撞上限
  const n = Math.ceil(geo.h / segH)
  for (let i = 0; i < n; i++) {
    const y = geo.y + i * segH
    const h = Math.min(segH, geo.y + geo.h - y)
    if (h <= 0) break
    const p = OUT.replace(/\.png$/, `-part${i + 1}.png`)
    await shotPage.screenshot({ path: p, clip: { x: geo.x, y, width: geo.w, height: h } })
    console.log(`  段 ${i + 1}/${n} →`, p, `${geo.w}×${h} @${effDsf}x`)
  }
  console.log(`分段完成：${n} 段，每段 ≤ ${segH} CSS px。`)
} else {
  await shotPage.screenshot({ path: OUT, clip: { x: geo.x, y: geo.y, width: geo.w, height: geo.h } })
  console.log('写入', OUT, fs.statSync(OUT).size, 'bytes', `${geo.w}×${geo.h} @${effDsf}x`)
}
if (errs.length) console.log('页面错误：\n' + errs.join('\n'))
await shotCtx.close()
await browser.close()
server.kill('SIGTERM')
fs.rmSync(PROBE, { recursive: true, force: true })
