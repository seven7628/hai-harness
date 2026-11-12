#!/usr/bin/env node
// 客户端级 E2E：统一异步任务（promote 长工具 → 精确停止 → interrupted 终态收敛）。
//
// 驱动**浏览器 mock transport**（无 window.desktop → src/transport/mock.ts 事件流），
// 覆盖三件事：
//   1. 长工具（bash sleep，tooltask-1）运行中 → 工具行「后台」按钮 promote；
//   2. promoted 后工具行/右侧任务栏出现「停止」按钮 → 按 taskId 精确中断
//      （interrupt_task 命令，只影响该任务，主会话 agent_end 正常 stop 无 abort）；
//   3. 完成通知默认折叠，展开可见完整结果/错误；任务卡终态 = 已中断（interrupted）。
//
// 用法：cd desktop/app && npm run build && node e2e/async_task_e2e.mjs
// 前置：npm 依赖已装（playwright + chromium）、dist 已构建。
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = 4180
const URL_BASE = `http://localhost:${PORT}/`

let pass = 0
function ok(label) {
  pass++
  console.log(`  ✅ ${label}`)
}
function fail(label, err) {
  console.error(`  ❌ ${label}`, err ?? '')
  process.exitCode = 1
}

async function waitPort(port, timeoutMs = 20000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const okConn = await new Promise((resolve) => {
      const s = net.connect({ port, host: 'localhost' })
      const timer = setTimeout(() => { s.destroy(); resolve(false) }, 500)
      s.on('connect', () => { clearTimeout(timer); s.destroy(); resolve(true) })
      s.on('error', () => { clearTimeout(timer); resolve(false) })
    })
    if (okConn) return
    if (Date.now() > deadline) throw new Error(`vite preview 未在 ${port} 端口就绪`)
    await new Promise((r) => setTimeout(r, 250))
  }
}

async function main() {
  // 1) 启动 vite preview（服务 dist；mock transport 不依赖后端 bridge）
  const server = spawn(
    'node',
    [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), 'preview', '--port', String(PORT), '--strictPort'],
    { cwd: APP, stdio: 'pipe' },
  )
  const stopServer = () => { try { server.kill('SIGTERM') } catch { /* 已退出 */ } }
  process.on('exit', stopServer)
  try {
    await waitPort(PORT)
  } catch (e) {
    console.error(server.stderr?.toString() ?? '')
    throw e
  }

  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 } })
  page.on('pageerror', (e) => console.error('[pageerror]', e.message))

  // 2) 预置持久项目树（mock 项目），跳过 BootScreen 直接进应用
  await page.addInitScript(
    (projects) => localStorage.setItem('go-code.projects', JSON.stringify(projects)),
    { paths: ['/mock/proj-auth'], active: '/mock/proj-auth' },
  )
  await page.goto(URL_BASE, { waitUntil: 'domcontentloaded' })

  // 3) 等应用就绪：输入框出现（工作区自动水合）
  await page.waitForSelector('textarea[placeholder*="输入消息"]', { timeout: 20000 })
  ok('应用启动（mock transport 水合完成）')

  // 4) 新建会话（左栏「新建会话」）
  await page.getByRole('button', { name: '新建会话' }).first().click()
  await page.waitForSelector('textarea[placeholder*="输入消息"]', { timeout: 10000 })
  await page.waitForTimeout(300) // 等会话激活（composer 解锁）
  ok('新建会话')

  // 5) 发送「长任务」触发 mock 长工具脚本（bash sleep 30 → tooltask-1）
  const composer = page.locator('textarea[placeholder*="输入消息"]')
  await composer.fill('长任务：sleep 30 移到后台')
  await composer.press('Enter')

  // 6) 等待工具行出现（tool_run_start 携带 task_id）→ 未 promote 时只显示「后台」
  await page.waitForSelector('.tool-row:has-text("bash")', { timeout: 15000 })
  const promoteBtn = page.locator('.tool-row .tool-promote', { hasText: '后台' }).first()
  await promoteBtn.waitFor({ state: 'visible', timeout: 5000 })
  ok('长工具运行中（工具行显示「后台」按钮）')

  // 7) promote → task_promoted → 工具行出现「停止」按钮
  await promoteBtn.click()
  await page.waitForSelector('.tool-row .tool-promote', { hasText: '停止' }) // 乐观 → 后台态
  await page.waitForTimeout(200) // task_promoted 事件落地
  const stopBtn = page.locator('.tool-row .tool-promote', { hasText: '停止' }).first()
  await stopBtn.waitFor({ state: 'visible', timeout: 5000 })
  ok('promote 后出现按 taskId 的「停止」按钮')

  // 8) 右侧任务栏：切到「任务」tab，工具任务卡片显示执行中 + 停止按钮
  await page.locator('aside.right button[data-tip*="异步任务"], aside.right .rbtn:has-text("任务")').first().click()
  await page.waitForSelector('.agent-card .ag-label:has-text("bash")', { timeout: 5000 })
  const panelStop = page.locator('.agent-card .ag-stop').first()
  await panelStop.waitFor({ state: 'visible', timeout: 5000 })
  ok('右侧任务栏展示工具任务卡片（含停止按钮）')

  // 9) 点击停止 → 乐观「中断中」→ mock 精确中断 → task_result_delivered(interrupted)
  await panelStop.click()
  await page.waitForTimeout(400) // 中断事件 + 交付（+60ms）
  await page.waitForSelector('.task-delivery-toggle:has-text("已中断")', { timeout: 5000 })
  ok('完成通知显示「已中断」（终态 interrupted）')

  // 10) 通知默认折叠；展开可见完整结果/错误
  const note = page.locator('.task-delivery-toggle', { hasText: '已中断' }).first()
  const noteText = await note.textContent()
  ok(`交付通知默认折叠（状态行：${noteText?.trim().slice(0, 40)}…）`)
  const body = page.locator('.task-delivery-toggle', { hasText: '已中断' }).locator('xpath=..').locator('pre')
  await note.click()
  await body.waitFor({ state: 'visible', timeout: 3000 })
  const bodyText = await body.textContent()
  if (!/context canceled/.test(bodyText ?? '')) throw new Error(`展开内容应为取消原因，got ${bodyText}`)
  ok('展开通知可见完整结果/错误（context canceled）')

  // 11) 工具行收敛为已中断（任务卡终态），主会话未被 abort（agent_end 正常 stop）
  await page.waitForSelector('.agent-card .ag-sec:has-text("已中断")', { timeout: 5000 })
  const agentEnds = await page.locator('.msg.assistant:has-text("（已中断）")').count()
  if (agentEnds > 0) throw new Error('主会话被误中断（不应出现「（已中断）」提示）')
  ok('任务卡终态 = 已中断；主会话未被误停')

  await browser.close()
  stopServer()
  console.log(`\n[e2e] 异步任务链路全部通过（${pass} 项断言）`)
}

main().catch((e) => {
  fail('E2E 失败', e)
  process.exit(1)
})
