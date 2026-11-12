#!/usr/bin/env node
// 桌面端客户端级 E2E（Browser Use 插件 UI 接线）。
//
// 驱动**真实 Electron 应用**（Electron main 起 Go bridge → preload contextBridge →
// React 渲染器），走「设置 → 插件 tab 安装/启用 → 右栏 web tab 页列表+截图」。
// 这是 plugin/browser 层（internal/integration/browser_e2e_test.go）与 bridge 进程级
// （scripts/bridge_browser_e2e.py）E2E 之上的**客户端**验证——覆盖 PluginTab / web tab /
// store 事件接线（plugin_status / browser_pages / browser_screenshot）在真实应用里的运行。
//
// 用法：cd desktop/app && npm run e2e:browser
// 前置：node/npm + 本机 Chrome + 网络（首次需 npm 安装 chrome-devtools-mcp）。
// 退出码：全通过 0；任一断言失败非 0（可作验证门禁）。
//
// 隔离：HOME → ~/.go-code-e2e-home（settings/keys/plugins/browser-profile 全隔离，
//   与 scripts/bridge_browser_e2e.py 共享——组件已装则复用跳过重装）；
//   GO_CODE_BROWSER_HEADLESS=1（bridge 插件 headless，E2E 不弹真实 Chrome 窗口）。
// 工作区：原生目录对话框 Playwright 无法驱动 → 注入 localStorage go-code.projects
//   再 reload，应用自动恢复工作区、跳过 BootScreen 选目录。

import { _electron } from 'playwright'
import { createRequire } from 'module'
import { execSync, spawnSync } from 'child_process'
import fs from 'fs'
import os from 'os'
import path from 'path'
import { fileURLToPath } from 'url'
import { createCleaner, assertNoForeignKill } from './safe_cleanup.mjs'

const require = createRequire(import.meta.url)
const __dirname = path.dirname(fileURLToPath(import.meta.url))

const APP = path.resolve(__dirname, '..') // desktop/app
const REPO = path.resolve(APP, '../..') // 仓库根
const HOME = process.env.GO_CODE_E2E_HOME || path.join(os.homedir(), '.go-code-e2e-home')
const WS = process.env.GO_CODE_CLIENT_E2E_WS || '/tmp/gocode-client-e2e-ws'

const ELECTRON = require('electron') // desktop/app/node_modules/electron 真实二进制路径
// 真实 Go 模块缓存：HOME 隔离下 Electron 兜底 go build 会找临时 HOME 的缓存（空 → 联网下载
// 超时）。把 GOMODCACHE 指回真实缓存，让 Electron 侧兜底构建也能命中。
const GOMODCACHE = execSync('go env GOMODCACHE').toString().trim()

let pass = 0

function log(...a) {
  console.log('[e2e]', ...a)
}
function ok(label) {
  pass++
  console.log(`  ✅ ${label}`)
}
function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms))
}

// —— 孤儿进程清理（只回收本 E2E 自己起的进程）——
// 历史事故：这里曾用 `pkill -9 -f chrome-devtools-mcp` / `-f browser-profile` 等**通用名**
// 泛匹配，而用户真实会话的命令行同样含这些字符串（真实受管 Chrome 是
// `--user-data-dir=~/.go-code/browser-profile`，真实 MCP 是 `.../chrome-devtools-mcp/...`）
// → 跑一次 E2E 就把用户正开着的 Chrome / 浏览器会话 SIGKILL，登录态中断。
// 现改为按“本次 E2E 的隔离路径标记”匹配：用户真实进程不含这些标记，天然免疫。
const cleaner = createCleaner({ markers: [HOME, WS] })
// 真实用户主目录（用于销毁前自检）；HOME 变量已被隔离值覆盖，故从 os.homedir() 另取。
const REAL_HOME = os.homedir()
function cleanupOrphans() {
  const victims = cleaner.previewVictims()
  assertNoForeignKill(victims, { realHome: REAL_HOME }) // 自检：命中用户真实环境则中止（宁可留孤儿也不误杀）
  cleaner.killByMarkers()
}

function maxMtime(dir) {
  let max = 0
  try {
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
      const p = path.join(dir, e.name)
      if (e.isDirectory()) max = Math.max(max, maxMtime(p))
      else max = Math.max(max, fs.statSync(p).mtimeMs)
    }
  } catch { /* 目录缺失 */ }
  return max
}

// —— 前置构建：renderer(vite) + electron main(tsc) + bridge 二进制。——
// 顺序关键：vite build 会清空 dist/（emptyOutDir 默认开），bridge 二进制必须最后放；
// 且须放对路径（desktop/app/dist/hai-bridge，Electron main 的 BRIDGE_BIN），
// 否则 Electron 的 ensureBridgeBin 会自己 go build——HOME 隔离下 Go 模块缓存
// 在临时 HOME 找不到依赖 → 联网下载超时，bridge 起不来。
function ensureBuild() {
  const index = path.join(APP, 'dist', 'index.html')
  const mainJs = path.join(APP, 'dist-electron', 'main.js')

  const srcMtime = Math.max(maxMtime(path.join(APP, 'src')), maxMtime(path.join(APP, 'electron')))
  const outMtime = Math.min(
    fs.existsSync(index) ? fs.statSync(index).mtimeMs : 0,
    fs.existsSync(mainJs) ? fs.statSync(mainJs).mtimeMs : 0,
  )
  if (!fs.existsSync(index) || !fs.existsSync(mainJs) || srcMtime > outMtime) {
    log('renderer/electron main 有更新或缺失，执行 npm run build …')
    const b = spawnSync('npm', ['run', 'build'], { cwd: APP, encoding: 'utf8' })
    if (b.status !== 0) throw new Error(`npm run build 失败:\n${b.stdout}\n${b.stderr}`)
  }
  const r = spawnSync('go', ['build', '-o', 'desktop/app/dist/hai-bridge', './desktop/bridge'], { cwd: REPO, encoding: 'utf8' })
  if (r.status !== 0) throw new Error(`go build bridge 失败:\n${r.stderr}`)
}

// —— 等待条件（Playwright locator 版本；fn 返回真值即停，超时抛错）——
async function waitFor(fn, { timeout = 30000, interval = 500, label = '条件' } = {}) {
  const end = Date.now() + timeout
  while (Date.now() < end) {
    try {
      const v = await fn()
      if (v) return v
    } catch { /* 继续轮询 */ }
    await sleep(interval)
  }
  throw new Error(`等待超时: ${label}`)
}

function main() {
  cleanupOrphans()
  ensureBuild()
  fs.mkdirSync(WS, { recursive: true })
  fs.mkdirSync(HOME, { recursive: true })

  let app
  let win
  return (async () => {
    log(`启动 Electron（HOME=${HOME}，GO_CODE_BROWSER_HEADLESS=1）…`)
    app = await _electron.launch({
      executablePath: ELECTRON,
      args: ['.'],
      cwd: APP,
      env: { ...process.env, HOME, GO_CODE_BROWSER_HEADLESS: '1', GOMODCACHE },
    })
    win = await app.firstWindow()
    win.on('console', (msg) => {
      if (msg.type() === 'error') console.log('[renderer-err]', msg.text())
    })

    // 1) 注入持久工作区（localStorage）再 reload → 应用自动恢复、跳过原生选目录对话框
    await win.waitForLoadState('domcontentloaded')
    await win.evaluate(([ws]) => {
      localStorage.setItem('go-code.projects', JSON.stringify({ paths: [ws], active: ws }))
    }, [WS])
    await win.reload()
    log('1. 注入工作区并 reload，等待主界面…')

    await waitFor(() => win.locator('.nav-item:has-text("设置")').isVisible(), {
      timeout: 30000, label: '主界面（左栏设置入口）',
    })
    ok('主界面挂载（工作区自动恢复）')

    // 2) 设置 → 插件 tab → Browser Use 卡片
    await win.locator('.nav-item:has-text("设置")').click()
    await waitFor(() => win.locator('[role="dialog"][aria-label="设置"]').isVisible(), {
      timeout: 10000, label: '设置弹窗',
    })
    await win.locator('.settings-nav-item:has-text("插件")').click()
    // 主界面挂载早于 bridge 就绪：首次 plugin_list 可能在 bridge 启动前发出（bridge 未运行
    // → 命令失败，插件列表空且不自动重试）。切走再切回重挂载 PluginTab → fetchPlugins 重发。
    const card = win.locator('.mcp-server', { hasText: 'Browser Use' })
    let cardCount = 0
    for (let i = 0; i < 12; i++) {
      cardCount = await card.count()
      if (cardCount > 0) break
      const sb = ((await win.locator('.statusbar').textContent()) ?? '').trim()
      log(`  插件卡片未出现（第 ${i + 1} 次，statusbar=${sb}），切 tab 重拉…`)
      await win.locator('.settings-nav-item:has-text("通用")').click()
      await sleep(200)
      await win.locator('.settings-nav-item:has-text("插件")').click()
      await sleep(500)
    }
    if (cardCount === 0) {
      throw new Error(`插件卡片未出现（statusbar=${(await win.locator('.statusbar').textContent())?.trim()}）`)
    }
    const badge = card.locator('.mcp-state')
    const status = (await badge.textContent())?.trim() ?? ''
    log(`2. 设置→插件 tab：browser 状态 = ${status}`)
    ok(`插件卡片出现（状态 ${status}）`)

    // 3) 未装则安装（npm install chrome-devtools-mcp@1.7.0，30-60s；异步安装中按钮消失）
    if (status === '未安装') {
      await card.locator('button:has-text("安装")').click()
      log('3. 点击安装，等待就绪（npm install 30-60s）…')
      await waitFor(async () => {
        const s = (await badge.textContent())?.trim() ?? ''
        if (s === '异常') throw new Error(`安装异常: ${(await card.textContent()) ?? ''}`)
        return s === '就绪' ? s : null
      }, { timeout: 180000, label: '插件安装完成（就绪）' })
      ok('组件安装完成 → 就绪')
    } else {
      log('3. 组件已装（跳过安装）')
      ok('组件已装（复用）')
    }

    // 4) 启用 → 私有 MCP 连接 up（chrome-devtools-mcp + Chrome headless）
    await card.locator('button:has-text("启用")').click()
    log('4. 点击启用，等待浏览器连接…')
    await waitFor(async () => {
      const s = (await badge.textContent())?.trim() ?? ''
      if (s === '异常') throw new Error(`启用异常: ${(await card.textContent()) ?? ''}`)
      return s === '已启用' ? s : null
    }, { timeout: 120000, label: '插件启用（已启用）' })
    ok('插件启用 → 已启用')

    // 5) 关设置 → 右栏 web tab → 状态徽标 + 页列表 + 截图
    await win.locator('.settings-close').click()
    await win.locator('button[aria-label="网页"]').click()
    log('5. 右栏 web tab：等待页列表与截图…')

    // web tab 默认沉浸模式（M6：tabsHidden 隐藏 fp-hd 状态徽标）——断言页列表渲染
    // 而非「浏览器在线」徽标（沉浸模式下 fp-hd 不可见，属设计行为）。
    const pages = win.locator('.browser-pages .browser-page')
    await waitFor(async () => (await pages.count()) > 0, { timeout: 20000, label: 'web tab 页列表渲染' })
    ok('web tab 页列表已渲染（沉浸模式）')

    const refresh = win.locator('button[aria-label="刷新浏览器面板"]')
    let pageCount = 0
    for (let i = 0; i < 4; i++) {
      try {
        await waitFor(async () => (await pages.count()) > 0, { timeout: 25000, label: `页列表出现（第 ${i + 1} 次尝试）` })
        pageCount = await pages.count()
        break
      } catch (e) {
        if (i === 3) throw e
        log(`  页列表未出现（${e.message}），点刷新重试…`)
        await refresh.click()
      }
    }
    ok(`页列表渲染（${pageCount} 页）`)
    // R4：标签只剩文件名 + title 里带完整 URL（不再把整条 URL 铺在标签上）→ 从 title 读回。
    const firstTitle = (await pages.first().getAttribute('title'))?.trim() ?? ''
    if (firstTitle) ok(`页 URL 渲染：${firstTitle}`)

    const shotImg = win.locator('.browser-shot img')
    const src = await waitFor(async () => {
      const s = await shotImg.getAttribute('src')
      return s && s.startsWith('data:image/') ? s : null
    }, { timeout: 25000, label: '截图 data URL' })
    const naturalWidth = await shotImg.evaluate((el) => el.naturalWidth)
    if (src.length < 1000) throw new Error(`截图 data URL 过短: ${src.length}`)
    if (!naturalWidth) throw new Error('截图 img 未加载（naturalWidth=0）')
    ok(`截图渲染（data:image ${src.length} 字符，${naturalWidth}px 宽）`)

    log(`🎉 客户端 E2E 全部通过（${pass} 项断言）`)
    return 0
  })().catch(async (e) => {
    log(`✗ 失败: ${e.message}`)
    if (win) {
      try {
        const shot = path.join('/tmp', `desktop-e2e-fail-${Date.now()}.png`)
        await win.screenshot({ path: shot })
        log(`  失败现场截图: ${shot}`)
        log(`  statusbar: ${(await win.locator('.statusbar').textContent())?.trim()}`)
        log(`  设置弹窗可见: ${await win.locator('[role="dialog"]').isVisible()}`)
        log(`  插件卡片数: ${await win.locator('.mcp-server').count()}`)
        const plugins = await win.locator('.mcp-server').allTextContents()
        log(`  插件卡片文本: ${JSON.stringify(plugins)}`)
      } catch { /* 窗口已关 */ }
    }
    return 1
  }).finally(async () => {
    if (app) {
      try { await app.close() } catch { /* 已退出 */ }
    }
    cleanupOrphans()
  })
}

main().then((code) => process.exit(code))
