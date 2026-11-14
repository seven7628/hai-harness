// 宣传片素材截图：README 预览场景（1440×900，无真实本机路径）
// 走浏览器 mock（无 Electron / 无 API key）：左侧跑 readmeScript，右侧打开 README.zh-CN.md 预览。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import net from 'net'
import fs from 'fs'

const APP = path.resolve(import.meta.dirname, '..')
const OUT = path.resolve(APP, '../../brand/remotion/public/ratio/readme.png')
const PORT = 4199
const PROBE = path.join(APP, 'e2e', '_shot_run')

async function waitPort(port, timeoutMs = 30000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const up = await new Promise((res) => {
      const s = net.connect({ port, host: 'localhost' })
      const t = setTimeout(() => { s.destroy(); res(false) }, 500)
      s.on('connect', () => { clearTimeout(t); s.destroy(); res(true) })
      s.on('error', () => { clearTimeout(t); res(false) })
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
const page = await browser.newPage({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2 })
const errs = []
page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
page.on('response', (r) => { if (r.status() >= 400 && !/favicon/.test(r.url())) errs.push(`HTTP ${r.status()} ${r.url()}`) })

await waitPort(PORT)
await page.goto(`http://localhost:${PORT}/e2e/_shot_run/index.html`, { waitUntil: 'load' })
// 等 mock 自动进假工作区
await page.waitForFunction(() => (window).__store?.getState?.().workspace, null, { timeout: 30000 })
await page.waitForTimeout(1500)

// 发一条触发 readmeScript 的消息
await page.fill('.composer textarea, textarea', '展示一下项目 README 的渲染效果')
await page.keyboard.press('Enter')
await page.waitForTimeout(3200)

// 打开右侧「最终文件」：走真实动作链 openFile(path)，不伪造 store 终态
await page.evaluate(async () => {
  const st = window.__store
  st.getState().setRightExpanded?.(true)
  st.setState({ fileFocus: undefined, filePreviews: {} })
  await new Promise((r) => setTimeout(r, 300))
  st.getState().openFile('/mock/workspace/README.zh-CN.md')
  await new Promise((r) => setTimeout(r, 1200))
})

// 右栏拉宽到 ~60%：这一场景要「明确展示 README」，右栏默认只占 25%（App.tsx defaultSize=360），
// 画面里文档只剩一条窄缝。走真实拖拽分隔条（顺带触发「右栏 >50% 自动收左栏」的产品规则），
// 与用户手动调整后的形态一致。
const handles = page.locator('.resize-handle')
const n = await handles.count()
const box = await handles.nth(n - 1).boundingBox()
if (box) {
  const y = box.y + box.height / 2
  await page.mouse.move(box.x + box.width / 2, y)
  await page.mouse.down()
  await page.mouse.move(1440 * 0.4, y, { steps: 24 })
  await page.mouse.up()
  await page.waitForTimeout(900)
}

// 预览滚到文档顶部：这一帧要同时看到 logo、标题、徽标与第一张截图
await page.evaluate(async () => {
  const doc = document.querySelector('.md-preview-doc')
  if (doc) doc.scrollTop = 0
  await new Promise((r) => setTimeout(r, 600))
})

// 等图真正解码完（naturalWidth>0）再统计/截图：不等的话会拍到「图还没加载」的一帧
await page.waitForFunction(() => {
  const imgs = [...document.querySelectorAll('.md-preview-doc img')]
  return imgs.length > 0 && imgs.every((i) => i.complete)
}, null, { timeout: 30000 }).catch(() => console.log('⚠️ 有图片未加载完，继续截图'))
await page.waitForTimeout(800)

const stats = await page.evaluate(() => {
  const doc = document.querySelector('.md-preview-doc')
  const h = [...(doc?.querySelectorAll('h1,h2,h3') ?? [])].find((e) => /参考客户端长什么样/.test(e.textContent || ''))
  const sib = h?.nextElementSibling
  const sib2 = sib?.nextElementSibling
  return {
    headingFound: !!h,
    sib: sib?.outerHTML.slice(0, 500),
    sib2: sib2?.outerHTML.slice(0, 300),
    chips: [...document.querySelectorAll('.md-preview-doc .md-img-pending, .md-preview-doc .md-img-missing')].map((e) => e.className),
  }
})
console.log('预览图片：', JSON.stringify(stats, null, 2))

fs.mkdirSync(path.dirname(OUT), { recursive: true })
await page.screenshot({ path: OUT })
console.log('写入', OUT, fs.statSync(OUT).size, 'bytes')
if (errs.length) console.log('页面错误：\n' + errs.join('\n'))
await browser.close()
server.kill('SIGTERM')
fs.rmSync(PROBE, { recursive: true, force: true })
