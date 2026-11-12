#!/usr/bin/env node
// 真实触发 agent 调用 browser_new_page：Playwright 起应用（**隔离 userData**）→ 对话框发指令
// → agent 真实执行工具 → 读取会话输出验证登录态。
//
// 隔离说明：早期版本共享用户真实 userData（`~/Library/Application Support/hai-desktop`）
// 并直接写 `localStorage['go-code.projects']` —— 既会与用户正在使用的应用抢单实例锁，
// 又把用户的真实工作区配置覆盖成 E2E 临时目录（表现为“工作区/登录态被移除”）。
// 现改为独立 `--user-data-dir` + 独立工作区，双向隔离。
import { _electron } from 'playwright'
import { createRequire } from 'module'
import { execSync } from 'child_process'
import os from 'os'
import path from 'path'
import { fileURLToPath } from 'url'

const require = createRequire(import.meta.url)
const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const ELECTRON = require('electron')
const GOMODCACHE = execSync('go env GOMODCACHE').toString().trim()
const WS = '/tmp/gocode-agent-trigger-ws'
// 独立 userData（不碰用户真实 hai-desktop profile）
const USERDATA = path.join(os.tmpdir(), 'gocode-agent-trigger-userdata')
const sleep = ms => new Promise(r => setTimeout(r, ms))
async function waitFor(fn, { timeout = 60000, label = 'wait' } = {}) {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) { try { if (await fn()) return } catch {} await sleep(800) }
  throw new Error('超时: ' + label)
}

async function main() {
  const fs = require('fs')
  if (!fs.existsSync(WS)) fs.mkdirSync(WS, { recursive: true })
  console.log('[trigger] 启动应用（共享 userData）…')
  const app = await _electron.launch({
    executablePath: ELECTRON,
    args: ['.', '--user-data-dir=' + USERDATA],
    cwd: APP,
    env: { ...process.env, GOMODCACHE },
  })
  const win = await app.firstWindow()
  win.on('console', m => { if (m.type() === 'error') console.log('[renderer-err]', m.text().slice(0, 200)) })
  await win.waitForLoadState('domcontentloaded')
  await win.evaluate(([ws]) => localStorage.setItem('go-code.projects', JSON.stringify({ paths: [ws], active: ws })), [WS])
  await win.reload()
  await waitFor(() => win.locator('textarea').first().isVisible(), { timeout: 60000, label: '对话输入框' })
  console.log('[trigger] 主界面就绪，找到输入框')

  // 输入指令（中文，让 agent 用 browser_new_page 打开 git 并确认登录态）
  const textarea = win.locator('textarea').first()
  await textarea.click()
  await textarea.fill('请使用 browser_new_page 工具打开 https://git.100credit.cn ，然后确认当前是否已登录（页面是否显示登录后的 Dashboard）。')
  console.log('[trigger] 已输入指令')
  await sleep(500)
  await textarea.press('Enter')
  console.log('[trigger] 已发送，等待 agent 执行…')

  // 等待会话输出出现工具调用痕迹（browser_new_page / 登录 / Dashboard）
  let found = ''
  const t0 = Date.now()
  while (Date.now() - t0 < 120000) {
    const body = await win.evaluate(() => document.body.innerText).catch(() => '')
    if (/browser_new_page|Dashboard|已登录|登录成功|git\.100credit/i.test(body)) {
      // 找关键行
      const lines = body.split('\n').filter(l => /browser_new_page|Dashboard|已登录|登录成功|Sign in/i.test(l))
      if (lines.length > 0) { found = lines.slice(-6).join(' | '); break }
    }
    await sleep(2000)
  }
  console.log('[trigger] 会话关键输出:', found || '(未捕获到关键字，输出如下)')
  if (!found) {
    const body = await win.evaluate(() => document.body.innerText).catch(() => '')
    console.log('[trigger] 页面文本尾部:', body.slice(-800).replace(/\n+/g, ' | '))
  }
  await win.screenshot({ path: '/tmp/agent-trigger-result.png' })
  console.log('[trigger] 截图已存 /tmp/agent-trigger-result.png')
  await app.close()
}
main().catch(e => { console.error('[trigger] 失败:', e.message); process.exit(1) })
