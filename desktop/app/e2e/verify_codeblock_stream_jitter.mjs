#!/usr/bin/env node
// 流式代码块抖动回归（2026-10-09，用户实测：「带代码块的回答流式输出时页面抖动」）
//
// 根因（两个叠加，缺一不可）：
//   ① react-markdown 每次 text 变化都重建整棵组件树，而 pre 覆盖返回 `<>{children}</>`
//      （Fragment 不构成复用边界）→ CodeBlock 实例随之重建。**实测**：一个 200 行代码块
//      在一次流式里被挂载 133 次、卸载 67 次。state 全丢 → 每次都从「未高亮原文」重新
//      等 shiki resolve → 形态 plain→shiki 反复横跳，每次都是一次可见跳动。
//   ② 高亮风暴：code 每 32ms 变一次（store 的 flushChunkBuf 合批节拍）→ 200 行块单次
//      codeToHtml 18-21ms，流式全程累计约占主线程 30%
//      （定量分析见 docs/NARRATIVE_STYLE_DESIGN_2026-09-18.md §6.2 P1）。
//
// 修法（markdown.tsx CodeBlock）：模块级高亮缓存（key = lang+主题+行号+code）+ 150ms
// 稳定期去抖 + 250ms 最小高亮间隔。
//
// 本脚本锁住的四条不变量（每条都对应一个「改回去就静默退化」的点）：
//   ① 流式全程形态切换次数 ≤1（修复前实测 21 次）
//   ② 唯一那次切换不产生位移：plain 与 shiki 两种形态**版式高度逐像素一致**
//   ③ 流式期间（未定型时）不发生形态切换
//   ④ 贴底 gap 恒为 0（高度变化没有把视图推离底部）
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'
import fs from 'fs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = Number(process.env.PORT || 4196)

let pass = 0
const ok = (m) => { pass++; console.log(`  \u2705 ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

async function waitPort(port, timeoutMs = 60000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const up = await new Promise((resolve) => {
      const s = net.connect({ port, host: 'localhost' })
      const timer = setTimeout(() => { s.destroy(); resolve(false) }, 500)
      s.on('connect', () => { clearTimeout(timer); s.destroy(); resolve(true) })
      s.on('error', () => { clearTimeout(timer); s.destroy(); resolve(false) })
    })
    if (up) return
    if (Date.now() > deadline) throw new Error('vite 未就绪')
    await new Promise((r) => setTimeout(r, 250))
  }
}

// 临时探针页：真实 Markdown 组件 + 真实 index.css，按 store 的 32ms 合批节拍喂 chunk。
// 滚动容器复刻 Narrative 的贴底校准循环（rAF + scrollHeight 增长才钉底）—— 否则量不到
// 「高亮完成把底部顶起来」这个真实症状。
const PROBE_DIR = path.join(__dirname, '_cbjitprobe')
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8"><title>cb stream jitter</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import { useEffect, useRef, useState } from 'react'
import { Markdown } from '../../src/components/markdown'
import '../../src/index.css'

const TOTAL = 120
const CODE_LINES = Array.from({ length: TOTAL }, (_, i) =>
  'func handler' + i + '(ctx context.Context) error { return nil } // 第 ' + i + ' 行')

// 与 Narrative.tsx:206-228 同口径的贴底校准循环
function ScrollHost({ text }: { text: string }) {
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const el = ref.current
    if (!el) return
    let raf = 0, lastMax = -1
    const tick = () => {
      raf = 0
      const max = el.scrollHeight - el.clientHeight
      const grew = max !== lastMax
      lastMax = max
      if (grew && max > 0 && el.scrollTop !== max) el.scrollTop = max
      raf = requestAnimationFrame(tick)
    }
    raf = requestAnimationFrame(tick)
    return () => { if (raf) cancelAnimationFrame(raf) }
  }, [])
  return (
    <div className="narrative" ref={ref} data-testid="host"
      style={{ height: 320, overflowY: 'auto', width: 700 }}>
      <div style={{ height: 500 }} />
      <div className="md-text"><Markdown text={text} /></div>
    </div>
  )
}

function App() {
  const [n, setN] = useState(1)
  useEffect(() => {
    let i = 1
    const t = setInterval(() => {
      i = Math.min(i + 2, TOTAL)
      setN(i)
      if (i >= TOTAL) clearInterval(t)
    }, 32) // store flushChunkBuf 的合批节拍
    return () => clearInterval(t)
  }, [])
  const text = '修复方案：\\n\\n\`\`\`go\\n' + CODE_LINES.slice(0, n).join('\\n') + '\\n\`\`\`\\n'
  return (
    <div>
      <div style={{ padding: 8, font: '12px monospace' }}>
        行数：<b data-testid="nl">{n}</b>
        <span style={{ marginLeft: 14 }}>cb高：<b data-testid="cbh">-</b></span>
        <span style={{ marginLeft: 14 }}>形态：<b data-testid="form">-</b></span>
        <span style={{ marginLeft: 14 }}>gap：<b data-testid="gap">-</b></span>
      </div>
      <ScrollHost text={text} />
    </div>
  )
}
createRoot(document.getElementById('root')!).render(<App />)

const W = window as unknown as Record<string, unknown>
W.__cb = () => {
  const cb = document.querySelector('.codeblock') as HTMLElement | null
  const host = document.querySelector('[data-testid="host"]') as HTMLElement | null
  return {
    lines: Number(document.querySelector('[data-testid="nl"]')?.textContent),
    cbH: cb ? +cb.getBoundingClientRect().height.toFixed(1) : null,
    // 形态：当前渲染的是 shiki 高亮产物，还是未高亮的 cb-plain 回退
    form: cb ? (cb.querySelector('.shiki') ? 'shiki' : (cb.querySelector('.cb-plain') ? 'plain' : '?')) : null,
    scrollTop: host ? Math.round(host.scrollTop) : null,
    clientH: host?.clientHeight ?? null,
    gap: host ? host.scrollHeight - host.scrollTop - host.clientHeight : null,
  }
}
// 版式高度对齐探针：同一份 code，plain 与 shiki 两种产物各挂一次量高度。
// 不走组件（组件有去抖/异步），只比 CSS 版式 —— 这是「唯一那次切换是否可见」的判据。
W.__parity = (code: string) => {
  const host = document.createElement('div')
  host.className = 'narrative'
  host.style.width = '700px'
  host.innerHTML = '<div class="md-text"></div>'
  document.body.appendChild(host)
  const esc = (s: string) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;')
  const probe = (html: string) => {
    const d = document.createElement('div')
    d.className = 'codeblock'
    d.innerHTML = html
    host.querySelector('.md-text')!.appendChild(d)
    const h = +d.getBoundingClientRect().height.toFixed(2)
    d.remove()
    return h
  }
  const plain = '<pre class="cb-body cb-plain"><code>' + esc(code) + '</code></pre>'
  const lines = code.split('\\n')
    .map((l) => '<span class="line"><span class="fp-ln" data-line="1"></span>' + esc(l) + '</span>')
    .join('\\n')
  const shiki = '<div class="cb-body"><pre class="shiki github-dark" style="background-color:#24292e">'
    + '<code>' + lines + '</code></pre></div>'
  const out = { plainH: probe(plain), shikiH: probe(shiki), lines: code.split('\\n').length }
  host.remove()
  return out
}
`)
fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [APP] } } })
`)

const PROBE_URL = `http://localhost:${PORT}/e2e/_cbjitprobe/index.html`
const server = spawn('node', [path.join(APP, 'node_modules/vite/bin/vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)

async function main() {
  await waitPort(PORT)
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 900, height: 700 } })
  const errs = []
  page.on('pageerror', (e) => errs.push('pageerror: ' + e.message))
  page.on('console', (m) => {
    const t = m.text()
    if (m.type() === 'error' && !/ERR_NAME_NOT_RESOLVED|Failed to load resource/.test(t)) errs.push(t.slice(0, 200))
  })

  // ---- ① plain 与 shiki 版式高度逐像素一致（抖动的必要条件：切换不产生位移） ----
  await page.goto(PROBE_URL, { waitUntil: 'domcontentloaded' })
  await page.waitForSelector('.codeblock', { timeout: 60000 })
  const parity = await page.evaluate(() => {
    const W = window
    const out = []
    for (const n of [1, 2, 3, 5, 8, 12, 20]) {
      const code = Array.from({ length: n }, (_, i) => `line${i} := ${i}`).join('\n')
      out.push(W.__parity(code))
    }
    return out
  })
  for (const p of parity) {
    assert(Math.abs(p.shikiH - p.plainH) <= 0.5,
      `${p.lines} 行：plain 与 shiki 高度一致（${p.plainH} vs ${p.shikiH}）—— 形态切换不产生位移`)
  }

  // ---- ②③④ 流式全程：形态切换次数、切换时机、贴底 ----
  await page.reload({ waitUntil: 'domcontentloaded' })
  await page.waitForSelector('.codeblock', { timeout: 60000 })
  const trace = []
  let lastForm = null
  let flips = 0
  let flipsBeforeSettled = 0
  for (let i = 0; ; i++) {
    const s = await page.evaluate(() => window.__cb())
    if (s.form !== lastForm) {
      if (lastForm !== null) {
        flips++
        // 切换发生时行数还没到 TOTAL = 流式未完成就跳变了（修复前的典型症状）
        if (s.lines < 120) flipsBeforeSettled++
      }
      lastForm = s.form
    }
    trace.push(s)
    if (s.lines >= 120 && i > 40) break
    if (i > 400) throw new Error('流式探针超时（行数未到 120）')
    await new Promise((r) => setTimeout(r, 60))
  }
  // 等去抖定时器跑完，确认最终形态落到 shiki
  await page.waitForTimeout(1200)
  const fin = await page.evaluate(() => window.__cb())

  assert(flips <= 1, `流式全程形态切换 ≤1 次（修复前实测 21 次，实际 ${flips}）`)
  assert(flipsBeforeSettled === 0, `流式未完成（行数 <120）时不发生形态切换（实际 ${flipsBeforeSettled} 次）`)
  assert(fin.form === 'shiki', `流式结束后最终渲染为 shiki 高亮产物（实际 ${fin.form}）—— 修复没有把高亮整个关掉`)
  const offBottom = trace.filter((s) => s.gap !== 0)
  assert(offBottom.length <= Math.max(3, trace.length * 0.1),
    `贴底全程跟随（gap≠0 的采样 ${offBottom.length}/${trace.length}）`)

  assert(errs.length === 0, `无页面/控制台报错${errs.length ? '：' + errs.join(' | ') : ''}`)

  await browser.close()
  cleanup()
  console.log(`\n[verify] 流式代码块抖动回归全部通过（${pass} 项断言）`)
}

main().catch((e) => { console.error('FAIL', e); cleanup(); process.exit(1) })
