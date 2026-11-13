// 左栏文件树的「文件夹管理」回归测试（真实 UI，浏览器 + mock transport）。
//
// 覆盖（2026-10 用户口径：对齐访达的文件夹管理；同名**拒绝**；删除**一律确认**）：
//   F1 新建文件夹：标题条按钮 → 目录内联输入框 → Enter 落盘
//   F2 重命名：右键菜单 → 内联输入框（改名成功）
//   F3 同名冲突：改成已存在的名字 → toast 报错 + **名字不变**（不是先改后回滚）
//   F4 拖动移动：dragstart/dragover/drop 派发到目标目录行 → 源目录消失、目标目录出现
//   F5 拖动**自身父目录**：不该有任何变化（无效目标，无 IPC）
//   F6 拷贝/粘贴：右键「拷贝」→ 另一个目录右键「粘贴」→ 出现一份
//   F7 复制副本：⌘D 语义（菜单项）→ a copy 命名
//   F8 删除：右键「移到废纸篓」→ 确认框 → 确认后行消失
//   F9 ⌘⌫：选中行 → 弹确认框 → Esc 取消（不删）
//   F10 右栏联动：打开的文件被重命名后，右栏标签/内容跟着换路径（不是变「读取失败」）
//
// 用法：cd desktop/app && npm run build && node e2e/verify_left_files_ops.mjs
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { freePort, waitHttp } from './_serve.mjs'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
// 端口由内核分配（见 e2e/_serve.mjs 的注释）：固定端口在链里会撞上一个还没退干净的服务器。
const PORT = Number(process.env.PORT) || await freePort()
const URL_BASE = `http://localhost:${PORT}/`
const WS = '/mock/workspace'

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

async function waitPort(port, timeoutMs = 20000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const up = await new Promise((resolve) => {
      const s = net.connect({ port, host: 'localhost' })
      const timer = setTimeout(() => { s.destroy(); resolve(false) }, 500)
      s.on('connect', () => { clearTimeout(timer); s.destroy(); resolve(true) })
      s.on('error', () => { clearTimeout(timer); resolve(false) })
    })
    if (up) return
    if (Date.now() > deadline) throw new Error('vite preview 未就绪')
    await new Promise((r) => setTimeout(r, 250))
  }
}

const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), 'preview', '--port', String(PORT), '--strictPort'], { cwd: APP, stdio: 'pipe' })
const stopServer = () => { try { server.kill('SIGTERM') } catch { /* ignore */ } }
process.on('exit', stopServer)

async function main() {
  await waitPort(PORT)
  // 端口可连只说明**有东西**在监听；再确认这张探针页真能开（子进程退出即失败）
  await waitHttp(URL_BASE, { child: server })
  const browser = await chromium.launch()
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 } })
  const errs = []
  page.on('console', (m) => { if (m.type() === 'error') errs.push(m.text()) })
  await page.goto(URL_BASE)
  await page.waitForSelector('.left-title')

  // 进入文件模式（窄形态入口在「⋯」菜单里）
  await page.locator('.ws-head').first().hover()
  await page.locator('.ws-head .ws-more').first().click()
  await page.waitForSelector('.ws-more-menu')
  await page.locator('.ws-more-menu button', { hasText: '查看文件' }).click()
  await page.waitForSelector('.lf-tree')
  const names = () => page.locator('.lf-row .lf-nm').evaluateAll((els) => els.map((e) => e.textContent))
  const row = (name) => page.locator('.lf-row', { has: page.locator('.lf-nm', { hasText: new RegExp(`^${name}$`) }) })
  const menuClick = async (name, label) => {
    await row(name).first().click({ button: 'right' })
    await page.waitForSelector('.lf-menu')
    await page.locator('.lf-menu .ctx-item', { hasText: label }).first().click()
  }
  const toastText = async () => {
    await page.waitForSelector('.toast-view', { timeout: 4000 })
    return page.locator('.toast-view').innerText()
  }

  assert((await names()).includes('auth.go'), '进入文件模式，根目录有 auth.go')

  // —— F1 新建文件夹 ——
  await page.locator('.lf-newdir').click()
  await page.waitForSelector('.lf-row.new .lf-input')
  await page.locator('.lf-row.new .lf-input').fill('notes')
  await page.locator('.lf-row.new .lf-input').press('Enter')
  await page.waitForSelector('.lf-row.dir:has-text("notes")')
  assert((await names()).includes('notes'), 'F1 新建文件夹：notes 出现在根目录')

  // —— F2 重命名 ——
  await menuClick('notes', '重命名')
  await page.waitForSelector('.lf-row .lf-input')
  assert(await page.locator('.lf-row .lf-input').inputValue() === 'notes', 'F2 重命名输入框预填当前名字')
  await page.locator('.lf-row .lf-input').fill('notes2')
  await page.locator('.lf-row .lf-input').press('Enter')
  await page.waitForSelector('.lf-row.dir:has-text("notes2")')
  assert(!(await names()).includes('notes'), 'F2 重命名：旧名字消失')
  assert((await names()).includes('notes2'), 'F2 重命名：新名字出现')

  // —— F3 同名冲突（拒绝，不改名）——
  await menuClick('notes2', '重命名')
  await page.locator('.lf-row .lf-input').fill('src')
  await page.locator('.lf-row .lf-input').press('Enter')
  const conflictToast = await toastText()
  assert(/已存在同名项目/.test(conflictToast), `F3 同名冲突 toast 报错（实得 ${JSON.stringify(conflictToast)}）`)
  assert((await names()).includes('notes2'), 'F3 冲突后名字**不变**（notes2 仍在）')

  // —— F4 拖动移动：auth.go → notes2 ——
  // 拖动 → 投放。分三次 evaluate 是必需的：drop 依赖 React 里「正在拖动」的状态，
  // 同一次调用里连续派发的话 dragover 时 state 还没落地（真实用户拖动天然有这段时间）。
  const dragTo = async (srcPath, dstPath, { onOver } = {}) => {
    await page.evaluate((p) => {
      const el = document.querySelector(`.lf-row[title="${p}"]`)
      window.__lfDt = new DataTransfer()
      el.dispatchEvent(new DragEvent('dragstart', { bubbles: true, cancelable: true, dataTransfer: window.__lfDt }))
    }, srcPath)
    await page.waitForTimeout(60)
    await page.evaluate((p) => {
      const el = document.querySelector(`.lf-row[title="${p}"]`)
      el.dispatchEvent(new DragEvent('dragover', { bubbles: true, cancelable: true, dataTransfer: window.__lfDt }))
    }, dstPath)
    if (onOver) await onOver()
    await page.evaluate((p) => {
      const el = document.querySelector(`.lf-row[title="${p}"]`)
      el.dispatchEvent(new DragEvent('drop', { bubbles: true, cancelable: true, dataTransfer: window.__lfDt }))
    }, dstPath)
    await page.waitForTimeout(400)
  }
  await dragTo(`${WS}/auth.go`, `${WS}/notes2`, {
    onOver: async () => {
      // 悬停时的高亮必须落在**这个**目标上（环 + 「移入」角标），而不是别的行
      const badge = page.locator(`.lf-row[title="${WS}/notes2"] .lf-dz-badge`)
      assert(await badge.count() === 1, 'F4 悬停中：目标行出现「移入」角标')
      assert(await page.locator('.lf-row.dz').count() === 1, 'F4 悬停中：只有一行是投放目标')
    },
  })
  // 判据用**绝对路径行**而不是名字：目标目录会自动展开，里面也有一个叫 auth.go 的行
  assert(await page.locator(`.lf-row[title="${WS}/auth.go"]`).count() === 0, 'F4 拖动后源目录里 auth.go 消失')
  // 投放到折叠目录会自动展开（否则用户看不到结果）——所以这里**不点**，直接等子项出现；
  // 点一下反而会把它收起来（这正是第一版测试踩的坑）。
  await page.waitForSelector(`.lf-row[title="${WS}/notes2/auth.go"]`)
  ok('F4 目标目录自动展开并出现 auth.go（拖动 = 移动）')

  // —— F5 拖到自身父目录：无变化 ——
  await dragTo(`${WS}/notes2/auth.go`, `${WS}/notes2`)
  assert(await page.locator(`.lf-row[title="${WS}/notes2/auth.go"]`).count() === 1, 'F5 拖到自身父目录：位置不变')

  // —— F6 拷贝 / 粘贴 ——
  await menuClick('README.md', '拷贝')
  await menuClick('notes2', '粘贴')
  await page.waitForSelector(`.lf-row[title="${WS}/notes2/README.md"]`)
  ok('F6 拷贝粘贴：notes2 里出现 README.md')
  assert(await page.locator(`.lf-row[title="${WS}/README.md"]`).count() === 1, 'F6 拷贝不搬走源文件（README.md 仍在根目录）')

  // —— F7 复制副本 ——
  await menuClick('auth_test.go', '复制副本')
  await page.waitForSelector('.lf-row[title="/mock/workspace/auth_test copy.go"]')
  ok('F7 复制副本：auth_test copy.go 出现')

  // —— F8 删除（确认框）——
  await menuClick('auth_test copy.go', '移到废纸篓')
  await page.waitForSelector('.modal-hint.open')
  assert((await page.locator('.modal-hint .confirm-desc').innerText()).includes('auth_test copy.go'), 'F8 确认框写明要删的条目')
  await page.locator('.modal-hint .btn.primary.danger').click()
  await page.waitForFunction(() => !document.querySelector('.lf-row[title="/mock/workspace/auth_test copy.go"]'))
  ok('F8 确认后条目从树里消失')
  assert(/已移到废纸篓/.test(await toastText()), 'F8 删除后有 toast 反馈')

  // —— F9 ⌘⌫ 弹确认，Esc 取消 ——
  await row('README.md').first().click()
  await page.keyboard.press('Meta+Backspace')
  await page.waitForSelector('.modal-hint.open')
  await page.keyboard.press('Escape')
  await page.waitForFunction(() => !document.querySelector('.modal-hint.open'))
  assert(await page.locator(`.lf-row[title="${WS}/README.md"]`).count() === 1, 'F9 Esc 取消后文件仍在')

  // —— F10 右栏联动：打开的文件被重命名后，右栏跟着换路径 ——
  // 用根目录的 README.zh-CN.md：点开 → 右栏展示 → 重命名 → 右栏标题/内容跟着变成新名字
  await page.locator(`.lf-row[title="${WS}/README.zh-CN.md"]`).click()
  await page.waitForSelector('.lf-row.file.active')
  await page.waitForFunction(() => document.querySelector('.right')?.innerText?.includes('README.zh-CN.md'), null, { timeout: 8000 })
  await menuClick('README.zh-CN.md', '重命名')
  await page.locator('.lf-row .lf-input').fill('READMEx.md')
  await page.locator('.lf-row .lf-input').press('Enter')
  await page.waitForSelector(`.lf-row[title="${WS}/READMEx.md"]`)
  const rightText = await page.locator('.right').innerText()
  assert(rightText.includes('READMEx.md'), `F10 右栏跟着换到新路径（实得 ${JSON.stringify(rightText.slice(0, 120))}）`)
  assert(!rightText.includes('读取失败') && !rightText.includes('加载失败'), 'F10 右栏没有退化成失败态')

  await page.screenshot({ path: process.env.SHOT_OPS || '/tmp/left-files-ops.png' })
  await browser.close()

  // 控制台错误：忽略与本次无关的既有噪声（图片/embed 等），只拦「未捕获异常」类
  const fatal = errs.filter((e) => /Uncaught|is not a function|Cannot read propert/i.test(e))
  assert(fatal.length === 0, `无未捕获异常（实得 ${JSON.stringify(fatal.slice(0, 3))}）`)

  console.log(`\n共 ${pass} 项断言通过`)
}

main().then(() => { stopServer(); process.exit(0) }).catch((e) => {
  console.error('\n❌', e.message)
  stopServer()
  process.exit(1)
})
