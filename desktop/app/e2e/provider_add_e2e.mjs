#!/usr/bin/env node
// Provider 添加 E2E：验证「新增自定义 Provider 后，设置弹窗的 Provider 卡片列表立即刷新」。
// 复现路径：设置 → Provider tab → 添加自定义 provider（my-test-prov）→ 断言卡片立即出现。
//
// 用法：cd desktop/app && node e2e/provider_add_e2e.mjs
// 退出码：全通过 0；任一断言失败非 0。

import { _electron } from 'playwright'
import { execSync } from 'child_process'
import fs from 'fs'
import os from 'os'
import path from 'path'
import { fileURLToPath } from 'url'
import { createCleaner, assertNoForeignKill } from './safe_cleanup.mjs'

const require = createRequire(import.meta.url)
const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.join(__dirname, '..')
const ELECTRON = require('electron')
const HOME = path.join(os.tmpdir(), 'go-code-e2e-provider-home')
const WS = path.join(os.tmpdir(), 'go-code-e2e-provider-ws')
const GOMODCACHE = path.join(__dirname, '..', '..', '..', '.gocache')

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
let failed = 0
function log(msg) { console.log(`[provider-e2e] ${msg}`) }
function assert(cond, label) {
  if (cond) { log(`  ✓ ${label}`) } else { failed++; log(`  ✗ ${label}`) }
}
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

// 只回收本 E2E 自己起的进程（按隔离 HOME/WS 标记匹配）。
// 历史事故：这里曾按进程名 `pkill -f`（当时的名字是 go-code-desktop，今为 hai-desktop）
// 或 `-f Electron.*desktop/app` ——
// 会杀掉**用户正在使用的桌面应用**，强杀导致 localStorage 未落盘，
// 用户的工作区/登录态随之“消失”。现改为标记匹配 + 销毁前自检。
const cleaner = createCleaner({ markers: [HOME, WS] })
function cleanupOrphans() {
  const victims = cleaner.previewVictims()
  assertNoForeignKill(victims, { realHome: os.homedir() })
  cleaner.killByMarkers()
}

function ensureBuild() {
  const bin = path.join(APP, 'dist-electron', 'main.js')
  if (!fs.existsSync(bin)) {
    log('构建 electron main…')
    execSync('npm run build:electron', { cwd: APP, stdio: 'inherit' })
  }
  const bridge = path.join(APP, 'dist', 'hai-bridge')
  if (!fs.existsSync(bridge)) {
    log('构建 bridge…')
    execSync('npm run build:bridge', { cwd: APP, stdio: 'inherit' })
  }
}

function main() {
  cleanupOrphans()
  ensureBuild()
  fs.mkdirSync(WS, { recursive: true })
  fs.mkdirSync(HOME, { recursive: true })

  let app, win
  return (async () => {
    log(`启动 Electron（HOME=${HOME}）…`)
    app = await _electron.launch({
      executablePath: ELECTRON,
      args: ['.'],
      cwd: APP,
      env: { ...process.env, HOME, GOMODCACHE },
    })
    win = await app.firstWindow()
    win.on('console', (msg) => {
      if (msg.type() === 'error') console.log('[renderer-err]', msg.text())
    })

    await win.waitForLoadState('domcontentloaded')
    await win.evaluate(([ws]) => {
      localStorage.setItem('go-code.projects', JSON.stringify({ paths: [ws], active: ws }))
    }, [WS])
    await win.reload()
    log('1. 注入工作区并 reload，等待主界面…')

    await waitFor(() => win.locator('.nav-item:has-text("设置")').isVisible(), {
      timeout: 30000, label: '主界面（左栏设置入口）',
    })
    await win.locator('.nav-item:has-text("设置")').click()
    await waitFor(() => win.locator('[role="dialog"][aria-label="设置"]').isVisible(), {
      timeout: 10000, label: '设置弹窗',
    })
    log('2. 设置弹窗已打开')

    // 切到 Provider tab
    await win.locator('.settings-nav-item:has-text("Provider")').click()
    await waitFor(() => win.locator('.prov-addbar').isVisible(), {
      timeout: 10000, label: 'Provider 添加栏',
    })
    log('3. Provider tab 已打开')

    // 记录添加前的卡片数
    const before = await win.locator('.prov-card').count()
    log(`   添加前卡片数 = ${before}`)

    // 点击「添加」展开表单
    await win.locator('.prov-addbar button:has-text("添加")').first().click()
    await waitFor(() => win.locator('.prov-card input[aria-label="provider name"]').isVisible(), {
      timeout: 10000, label: '添加表单',
    })

    // 填自定义 provider
    const name = 'my-e2e-prov'
    await win.locator('.prov-card input[aria-label="provider name"]').fill(name)
    await win.locator('.prov-card input[aria-label="provider base url"]').fill('https://api.example.com/v1')
    log(`4. 填写自定义 provider: ${name}`)

    // 提交添加
    await win.locator('.prov-card button:has-text("添加")').last().click()
    log('5. 已点击添加确认，等待卡片出现…')

    // 关键断言：新卡片立即出现（不重开弹窗）
    await waitFor(() => win.locator(`.prov-card:has-text("${name}")`).isVisible(), {
      timeout: 15000, label: `新 Provider 卡片 ${name} 出现`,
    })
    const after = await win.locator('.prov-card').count()
    log(`   添加后卡片数 = ${after}`)
    assert(after === before + 1, `卡片数 +1（${before} → ${after}）`)
    assert(await win.locator(`.prov-card:has-text("${name}")`).isVisible(), `新卡片 ${name} 可见`)

    // 新卡片应展开（submitAdd 设置 expanded）
    await sleep(500)
    const expanded = await win.locator(`.prov-card:has-text("${name}") .prov-body`).isVisible()
    assert(expanded, `新卡片 ${name} 展开（含模型编辑区）`)

    log('=== Provider 添加 E2E 完成 ===')
    await app.close()
    process.exit(failed ? 1 : 0)
  })().catch((e) => {
    console.error('[provider-e2e] 失败:', e)
    try { app?.close() } catch { /* ignore */ }
    process.exit(1)
  })
}

main()
