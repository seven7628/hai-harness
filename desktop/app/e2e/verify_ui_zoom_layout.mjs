#!/usr/bin/env node
// 真实 Electron 里验收「外观 → 字体大小」缩放后的布局完整性。
//
// 为什么必须在真机验：缩放有两条路径 ——
//   ① Electron 原生缩放（webContents.setZoomFactor）：改的是**布局视口**（0.9 → 视口 CSS px 变多），
//      vh/fixed/媒体查询一并跟随；
//   ② 浏览器 CSS 退化路径（#root zoom + 尺寸补偿，见 index.css）。
//   两者的失效方式不同（① 会把内嵌视图 bounds 算错、② 会让布局盒比视口大），浏览器探针只能验到 ② 的
//   一半。用户报的「字体调大/调小后页面样式异常」正是 ① 早期没有原生桥时退化成 html zoom 造成的。
//
// 渲染源用**用户正在跑的 vite dev server**（VITE_DEV_SERVER_URL=http://localhost:5173）：
// 保证验的是当前工作区代码，而不是过期的 dist/。没有 dev server 时回退到 dist/（会提示）。
//
// 用法：cd desktop/app && node e2e/verify_ui_zoom_layout.mjs
// 退出码：全通过 0；任一断言失败非 0。
import { _electron } from 'playwright'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const APP = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const ELECTRON = path.join(APP, 'node_modules', '.bin', 'electron')
const SHOTS = path.join(APP, 'e2e', '_zoom_shots')
const HOME = fs.mkdtempSync(path.join(os.tmpdir(), 'zoom-e2e-home-'))
const WS = fs.mkdtempSync(path.join(os.tmpdir(), 'zoom-e2e-ws-'))
const DEV_URL = process.env.VITE_DEV_SERVER_URL || 'http://localhost:5173'
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

// 主进程/preload 是编译产物（dist-electron）：源码改过就要重编一次，否则验的是旧桥
//（原生缩放的 IPC 就在 preload/main 里 —— 不重编会静默退化成 CSS 路径，这条最坑）。
const be = spawnSync('npm', ['run', 'build:electron'], { cwd: APP, encoding: 'utf8' })
if (be.status !== 0) throw new Error(`npm run build:electron 失败:\n${be.stdout}\n${be.stderr}`)

fs.mkdirSync(SHOTS, { recursive: true })

async function main() {
  let app = null
  try {
    const useDev = await fetch(DEV_URL).then((r) => r.ok).catch(() => false)
    console.log(`[zoom] 渲染源：${useDev ? DEV_URL + '（dev server，当前工作区代码）' : 'dist/（构建产物，可能过期）'}`)
    app = await _electron.launch({
      executablePath: ELECTRON,
      args: ['.'],
      cwd: APP,
      env: {
        ...process.env,
        HOME,
        HAI_DATA_DIR: HOME,
        ...(useDev ? { VITE_DEV_SERVER_URL: DEV_URL } : {}),
      },
    })
    const win = await app.firstWindow()
    win.on('console', (m) => { if (m.type() === 'error' && !/favicon|DevTools/.test(m.text())) console.log('[renderer-err]', m.text()) })
    await win.waitForLoadState('domcontentloaded')
    await win.evaluate(([ws]) => {
      localStorage.setItem('go-code.projects', JSON.stringify({ paths: [ws], active: ws }))
    }, [WS])
    await win.reload()
    await win.waitForSelector('.app-root', { timeout: 30000 })
    await sleep(1200)

    const content = await app.evaluate(({ BrowserWindow }) => {
      const w = BrowserWindow.getAllWindows()[0]
      const b = w.getContentBounds()
      return { w: b.width, h: b.height }
    })

    const setScale = async (scale) => {
      await win.evaluate((s) => {
        const raw = JSON.parse(localStorage.getItem('go-code.appearance') || '{}')
        localStorage.setItem('go-code.appearance', JSON.stringify({ ...raw, scale: s }))
      }, scale)
      await win.reload()
      await win.waitForSelector('.app-root', { timeout: 30000 })
      await sleep(900) // 等 boot 水合 + 应用外观
    }

    const measure = async () => {
      const zoomFactor = await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].webContents.getZoomFactor())
      const m = await win.evaluate(() => {
        const rect = (sel) => {
          const el = document.querySelector(sel)
          if (!el) return null
          const b = el.getBoundingClientRect()
          return { x: Math.round(b.x), y: Math.round(b.y), w: Math.round(b.width), h: Math.round(b.height), bottom: Math.round(b.bottom) }
        }
        window.scrollTo(0, 9999); window.scrollTo(9999, 0)
        return {
          inner: { w: innerWidth, h: innerHeight },
          scroll: { w: document.documentElement.scrollWidth, h: document.documentElement.scrollHeight, x: window.scrollX, y: window.scrollY },
          uiScale: getComputedStyle(document.documentElement).getPropertyValue('--ui-scale').trim(),
          htmlInlineZoom: document.documentElement.style.zoom || '',
          rootZoom: getComputedStyle(document.getElementById('root')).zoom,
          appRoot: rect('.app-root'),
          main: rect('.main'),
          composer: rect('.composer-wrap'),
          statusbar: rect('.statusbar'),
        }
      })
      return { zoomFactor, ...m }
    }

    for (const [label, scale] of [['小 0.9', 0.9], ['标准 1.0', 1], ['特大 1.25', 1.25]]) {
      await setScale(scale)
      const m = await measure()
      console.log(`\n[${label}] zoomFactor=${m.zoomFactor.toFixed(2)} 视口 ${m.inner.w}×${m.inner.h}（窗口内容 ${content.w}×${content.h}）`
        + ` --ui-scale=${m.uiScale} #root.zoom=${m.rootZoom} htmlInlineZoom="${m.htmlInlineZoom}"`
        + `\n         appRoot ${JSON.stringify(m.appRoot)}\n         composer ${JSON.stringify(m.composer)} statusbar ${JSON.stringify(m.statusbar)}`)
      await win.screenshot({ path: path.join(SHOTS, `zoom-${String(scale).replace('.', '_')}.png`) })

      // ① 走的是原生缩放（有桥时不允许退化到 CSS 路径）
      assert(Math.abs(m.zoomFactor - scale) < 0.001, `${label}：走原生缩放（webContents zoomFactor=${m.zoomFactor.toFixed(2)}）`)
      assert(m.uiScale === '1' && m.rootZoom === '1' && m.htmlInlineZoom === '',
        `${label}：CSS 退化路径已关闭（--ui-scale=${m.uiScale}, #root.zoom=${m.rootZoom}, htmlInlineZoom 空）`)
      // ② 应用根铺满布局视口（原生缩放下 = 窗口/缩放比）
      // 允许 1px：布局视口高常是小数（892/1.25 = 713.6），rect 取整后与 innerHeight 差 1
      assert(m.appRoot && Math.abs(m.appRoot.w - m.inner.w) <= 1 && Math.abs(m.appRoot.h - m.inner.h) <= 1,
        `${label}：.app-root 铺满布局视口（${m.appRoot?.w}×${m.appRoot?.h} vs ${m.inner.w}×${m.inner.h}）`)
      // ③ 视觉尺寸 = 窗口内容区（换算回 DIP 后必须严丝合缝，否则就是"内容比窗口大/小一圈"）
      const visW = Math.round(m.appRoot.w * m.zoomFactor)
      const visH = Math.round(m.appRoot.h * m.zoomFactor)
      assert(Math.abs(visW - content.w) <= 1 && Math.abs(visH - content.h) <= 1,
        `${label}：视觉尺寸 = 窗口内容区（${visW}×${visH} vs ${content.w}×${content.h}）`)
      // ④ 页面级不滚动（用户的症状就是"内容被裁/要滚"）
      assert(m.scroll.x === 0 && m.scroll.y === 0, `${label}：页面不滚动（scrollTo 位移 ${m.scroll.x},${m.scroll.y}）`)
      // ⑤ 底部两个关键条必须在可视区内（composer = 输入框、statusbar = 状态栏 —— 最常被裁）
      for (const [name, r] of [['输入框 .composer-wrap', m.composer], ['状态栏 .statusbar', m.statusbar]]) {
        if (!r) continue
        const bottom = Math.round(r.bottom * m.zoomFactor)
        assert(bottom <= content.h + 1, `${label}：${name} 完整可见（视觉底 ${bottom} ≤ ${content.h}）`)
      }
    }

    console.log(`\n全部通过（${pass} 项断言）\n截图：${SHOTS}`)
  } finally {
    if (app) await app.close().catch(() => {})
  }
}

main().catch((e) => {
  console.error('\n❌ ' + (e instanceof Error ? e.message : String(e)))
  process.exitCode = 1
})
