// 最小验证：真实 Electron 中设置 → 权限 → 添加 App → 持久化。
// 用法：node e2e/perm_check.mjs
import { _electron } from 'playwright'
import { execSync, spawnSync } from 'child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const APP = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const REPO = path.resolve(APP, '../..')
const ELECTRON = path.join(APP, 'node_modules', '.bin', 'electron')
const HOME = fs.mkdtempSync(path.join(os.tmpdir(), 'perm-e2e-home-'))
const WS = fs.mkdtempSync(path.join(os.tmpdir(), 'perm-e2e-ws-'))
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

// 构建（如 renderer/electron 有新改动）
const index = path.join(APP, 'dist', 'index.html')
const mainJs = path.join(APP, 'dist-electron', 'main.js')
if (!fs.existsSync(index) || !fs.existsSync(mainJs)) {
  const b = spawnSync('npm', ['run', 'build'], { cwd: APP, encoding: 'utf8' })
  if (b.status !== 0) throw new Error(`npm run build 失败:\n${b.stdout}\n${b.stderr}`)
}
spawnSync('go', ['build', '-o', 'desktop/app/dist/hai-bridge', './desktop/bridge'], { cwd: REPO, encoding: 'utf8' })

async function waitFor(fn, { timeout = 30000, label = '条件' } = {}) {
  const end = Date.now() + timeout
  while (Date.now() < end) {
    try { const v = await fn(); if (v) return v } catch { /* 继续 */ }
    await sleep(250)
  }
  throw new Error(`等待超时: ${label}`)
}

const app = await _electron.launch({ executablePath: ELECTRON, args: ['.'], cwd: APP, env: { ...process.env, HOME } })
try {
  const win = await app.firstWindow()
  win.on('console', (m) => { if (m.type() === 'error') console.log('[renderer-err]', m.text()) })
  await win.waitForLoadState('domcontentloaded')
  // 注入工作区 → reload → 跳过原生选目录
  await win.evaluate(([ws]) => { localStorage.setItem('go-code.projects', JSON.stringify({ paths: [ws], active: ws })) }, [WS])
  await win.reload()
  await waitFor(() => win.locator('button', { hasText: '设置' }).first().isVisible(), { label: '主界面就绪' })

  // 打开设置 → 权限 tab
  await win.locator('button', { hasText: '设置' }).first().click()
  await waitFor(() => win.getByRole('button', { name: '权限', exact: true }).isVisible(), { label: '权限 tab 可见' })
  await win.getByRole('button', { name: '权限', exact: true }).click()

  // 添加 Calculator
  await waitFor(() => win.locator('.perm-add input').isVisible(), { label: '添加输入框可见' })
  await win.locator('.perm-add input').fill('Calculator')
  await win.locator('.perm-add .btn.primary').click()
  await waitFor(() => win.locator('.perm-name', { hasText: 'Calculator' }).isVisible(), { label: '添加后列表出现' })
  console.log('PASS: 添加 Calculator 成功，列表出现')

  // 「从系统选择」按钮存在 + preload 接线（真实 osascript 对话框需真人点选，此处只验接线）
  const hasPickBtn = await win.locator('button', { hasText: '从系统选择' }).isVisible()
  // L0（D6）之后页面里读不到 preload api（生产构建没有 __haiTestBridge；claim 已被宿主
  // 一次性领走，且 contextBridge 定义的属性 non-writable/non-configurable）。这条断言本来
  // 要证的就是「preload → 主进程」这段接线在，所以在**主进程侧**查同一个 IPC 通道：
  // preload.pickApp → ipcRenderer.invoke('app:pick')（electron/preload.ts:142）。
  const hasPickIpc = await app.evaluate(({ ipcMain }) => {
    const map = ipcMain._invokeHandlers
    return !!(map && typeof map.has === 'function' && map.has('app:pick'))
  })
  if (!hasPickBtn || !hasPickIpc) throw new Error(`从系统选择接线缺失: btn=${hasPickBtn} ipc=${hasPickIpc}`)
  console.log("PASS: 「从系统选择」按钮 + preload.pickApp → 主进程 app:pick 接线就绪")

  // 持久化：reload 后重进设置 → 权限 → 仍存在
  await win.reload()
  await waitFor(() => win.locator('button', { hasText: '设置' }).first().isVisible(), { label: 'reload 后主界面' })
  await win.locator('button', { hasText: '设置' }).first().click()
  await win.getByRole('button', { name: '权限', exact: true }).click()
  await waitFor(() => win.locator('.perm-name', { hasText: 'Calculator' }).isVisible(), { label: 'reload 后列表持久' })
  console.log('PASS: reload 后 Calculator 仍在（settings.json 持久化）')

  // 落盘检查
  const settingsFile = path.join(HOME, '.go-code', 'settings.json')
  const raw = fs.readFileSync(settingsFile, 'utf8')
  if (!raw.includes('allowed_apps') || !raw.includes('Calculator')) throw new Error(`settings.json 缺 permissions:\n${raw}`)
  console.log('PASS: settings.json 含 permissions.allowed_apps=["Calculator"]')
} finally {
  await app.close()
}
