#!/usr/bin/env node
// 验证脚本（2026-08-30 修订为断言式）：覆盖问题 1/3/4 修复后的行为。
//   问题3：/ 命令菜单 Tab → completeCommand 接线（补全第一个命令）；@ 面板 Tab → 选中第一项（单触发）
//   问题4：@ token 中间回删 → 面板重新唤醒（正则化识别，删除与输入对称）；
//          /summary 删除唤醒（现状已通——原脚本 4c 回删次数 off-by-N 误判，已修正为 6 次）
//   问题1：promote 失败（mock 钩子「promote失败」）→ 乐观置位回滚 + 行内错误提示（不再是静默）
// 用法：cd desktop/app && npm run build && node e2e/verify_composer_issues.mjs
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = 4181
const URL_BASE = `http://localhost:${PORT}/`

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

async function waitPort(port, timeoutMs = 20000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const ok = await new Promise((resolve) => {
      const s = net.connect({ port, host: 'localhost' })
      const timer = setTimeout(() => { s.destroy(); resolve(false) }, 500)
      s.on('connect', () => { clearTimeout(timer); s.destroy(); resolve(true) })
      s.on('error', () => { clearTimeout(timer); resolve(false) })
    })
    if (ok) return
    if (Date.now() > deadline) throw new Error('vite preview 未就绪')
    await new Promise((r) => setTimeout(r, 250))
  }
}

const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), 'preview', '--port', String(PORT), '--strictPort'], { cwd: APP, stdio: 'pipe' })
const stopServer = () => { try { server.kill('SIGTERM') } catch {} }
process.on('exit', stopServer)

async function main() {
  await waitPort(PORT)
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 } })
  page.on('pageerror', (e) => console.error('[pageerror]', e.message))
  await page.addInitScript(
    (projects) => localStorage.setItem('go-code.projects', JSON.stringify(projects)),
    { paths: ['/mock/proj-auth'], active: '/mock/proj-auth' },
  )
  await page.goto(URL_BASE, { waitUntil: 'domcontentloaded' })
  await page.waitForSelector('textarea[placeholder*="输入消息"]', { timeout: 20000 })
  await page.getByRole('button', { name: '新建会话' }).first().click()
  await page.waitForTimeout(400)

  const composer = page.locator('textarea[placeholder*="输入消息"]')

  // ---- 问题3a：/sum 按 Tab → 补全为 /summary，菜单关闭 ----
  await composer.fill('/sum')
  await page.waitForTimeout(300)
  assert(await page.locator('.cmd-menu').count() === 1, '输入 /sum 后命令菜单出现')
  await composer.press('Tab')
  await page.waitForTimeout(300)
  assert((await composer.inputValue()) === '/summary', 'Tab 补全为 /summary（completeCommand 接线）')
  assert(await page.locator('.cmd-menu').count() === 0, '补全后菜单关闭')
  await composer.fill('')

  // ---- 问题3b：@ 面板按 Tab → 选中第一项（单触发，不搞乱文本）----
  await composer.click()
  await composer.pressSequentially('@', { delay: 30 })
  await page.waitForTimeout(500)
  assert(await page.locator('.ref-menu').count() === 1, '@ 打开文件面板')
  await composer.press('Tab')
  await page.waitForTimeout(300)
  const afterAtTab = await composer.inputValue()
  assert(/^@.+ $/.test(afterAtTab) && afterAtTab !== '@', `Tab 选中第一项并补分隔空格（值="${afterAtTab}"）`)
  assert(await page.locator('.ref-menu').count() === 0, 'Tab 选中后面板关闭（去重后单触发）')
  await composer.fill('')

  // ---- 问题4a：@ 选中文件后回删 → 面板重新唤醒 ----
  await composer.click()
  await composer.pressSequentially('@', { delay: 30 })
  await page.waitForTimeout(500)
  const fileRow = page.locator('.ref-menu .ref-row.file').first() // 只点文件行（旧脚本误点「..」导航行）
  if (await fileRow.count() > 0) {
    await fileRow.click()
    await page.waitForTimeout(300)
    assert(await page.locator('.ref-menu').count() === 0, '选中文件后面板关闭')
    const picked = await composer.inputValue()
    assert(picked.endsWith(' '), `文件引用已插入（值="${picked}"）`)
    await composer.press('Backspace') // 删分隔空格
    await composer.press('Backspace') // 删 token 尾字符
    await page.waitForTimeout(300)
    assert(await page.locator('.ref-menu').count() === 1, '回删 token 中间 → 面板重新唤醒（删除对称）')
  } else {
    console.log('  ⚠️ mock 树无文件条目，4a 跳过')
  }
  await composer.fill('')

  // ---- 问题4b：@token + 空格 关闭后回删 → 唤醒 ----
  await composer.click()
  await composer.pressSequentially('@./docs', { delay: 25 })
  await page.waitForTimeout(400)
  await composer.press(' ')
  await page.waitForTimeout(200)
  assert(await page.locator('.ref-menu').count() === 0, '@token 后补空格 → 面板关闭')
  await composer.press('Backspace') // 删空格
  await composer.press('Backspace') // 删 's' → @./doc
  await page.waitForTimeout(300)
  assert(await page.locator('.ref-menu').count() === 1, '回删到 @./doc → 面板重新唤醒')
  await composer.fill('')

  // ---- 问题4c：/summary 删除唤醒（修正回删次数：13 字符需 6 次才到 /summar）----
  await composer.fill('/summary 帮我看看')
  await page.waitForTimeout(300)
  for (let i = 0; i < 6; i++) await composer.press('Backspace')
  await page.waitForTimeout(300)
  assert((await composer.inputValue()) === '/summar', '回删 6 次到达 /summar（修正 off-by-N）')
  assert(await page.locator('.cmd-menu').count() === 1, '删除到命令严格前缀 → 菜单唤醒')
  await composer.fill('')

  // ---- 问题1：promote 失败 → 乐观回滚 + 行内错误提示（mock 钩子：文本含「promote失败」）----
  await composer.fill('长任务 promote失败')
  await composer.press('Enter')
  await page.waitForSelector('.tool-row .tool-promote', { hasText: '后台' }, { timeout: 15000 })
  ok('长工具运行中（「后台」按钮可见）')
  await page.locator('.tool-row .tool-promote', { hasText: '后台' }).first().click()
  await page.waitForTimeout(500) // mock 同步响应 → 乐观置位 + 失败回滚
  const notice = page.locator('.msg.system.error', { hasText: '移到后台失败' })
  assert(await notice.count() >= 1, '行内错误提示出现（替代静默/全局 bridgeError）')
  assert(await page.locator('.tool-row .tool-promote', { hasText: '停止' }).count() === 0, '乐观置位已回滚（按钮恢复「后台」）')

  await browser.close()
  stopServer()
  console.log(`\n[verify] 问题 1/3/4 修复验证全部通过（${pass} 项断言）`)
}

main().catch((e) => { console.error('FAIL', e); process.exit(1) })
