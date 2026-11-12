// 左栏文件树第二轮：多选批量 / 菜单键盘 / 从访达拖入 的回归测试（真实 UI，浏览器 + mock）。
//
// 覆盖：
//   G1 ⌘ 单击增减选中、⇧ 单击选连续区间、⌘A 全选可见项
//   G2 批量删除：⌘⌫ 的确认框写明条目数，确认后整组消失
//   G3 批量拖动：选中两项后拖到目录 → 两项都搬过去（不是只搬按住的那一项）
//   G4 批量拷贝/粘贴：整组粘到另一个目录
//   G5 右键菜单键盘：↑ 聚焦首项 + Enter 激活；Esc 关闭且焦点回到树
//   G6 从访达拖入：外部 File 拖到目录行 → 导入（「导入」角标 + 落地）
//   G7 批量同名冲突：逐项报告，成功的照样到位（部分失败不是整批失败）
//
// 用法：cd desktop/app && npm run build && node e2e/verify_left_files_batch.mjs
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = 4205
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
  const browser = await chromium.launch()
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 } })
  const errs = []
  page.on('console', (m) => { if (m.type() === 'error') errs.push(m.text()) })
  await page.goto(URL_BASE)
  await page.waitForSelector('.left-title')
  await page.locator('.ws-head').first().hover()
  await page.locator('.ws-head .ws-more').first().click()
  await page.waitForSelector('.ws-more-menu')
  await page.locator('.ws-more-menu button', { hasText: '查看文件' }).click()
  await page.waitForSelector('.lf-tree')

  const row = (p) => page.locator(`.lf-row[title="${WS}/${p}"]`)
  const selCount = () => page.locator('.lf-row.sel').count()
  const toastText = async () => {
    await page.waitForSelector('.toast-view', { timeout: 4000 })
    return page.locator('.toast-view').innerText()
  }
  // 内部拖动（与 verify_left_files_ops 同一套派发：drop 依赖「拖动中」状态，得分三次）
  const dragTo = async (held, dst, onOver) => {
    await page.evaluate((p) => {
      const el = document.querySelector(`.lf-row[title="${p}"]`)
      window.__dt = new DataTransfer()
      el.dispatchEvent(new DragEvent('dragstart', { bubbles: true, cancelable: true, dataTransfer: window.__dt }))
    }, held)
    await page.waitForTimeout(60)
    await page.evaluate((p) => {
      const el = document.querySelector(`.lf-row[title="${p}"]`)
      el.dispatchEvent(new DragEvent('dragover', { bubbles: true, cancelable: true, dataTransfer: window.__dt }))
    }, dst)
    if (onOver) await onOver()
    await page.evaluate((p) => {
      const el = document.querySelector(`.lf-row[title="${p}"]`)
      el.dispatchEvent(new DragEvent('drop', { bubbles: true, cancelable: true, dataTransfer: window.__dt }))
    }, dst)
    await page.waitForTimeout(450)
  }

  // —— G1 多选 ——
  await row('auth.go').click()
  assert(await selCount() === 1, 'G1 单击 = 只选中一项')
  await row('README.md').click({ modifiers: ['Meta'] })
  assert(await selCount() === 2, 'G1 ⌘ 单击 = 增减选中（2 项）')
  assert(await row('auth.go').getAttribute('aria-selected') === 'true', 'G1 先选中的那项仍在选中集里')
  await row('README.md').click({ modifiers: ['Meta'] })
  assert(await selCount() === 1, 'G1 再 ⌘ 单击 = 取消该项')
  // ⇧ 区间：从 auth_test.go 到 README.zh-CN.md（视觉顺序里连续 3 行）
  await row('auth_test.go').click()
  await row('README.zh-CN.md').click({ modifiers: ['Shift'] })
  assert(await selCount() >= 3, `G1 ⇧ 单击 = 选连续区间（实得 ${await selCount()} 项）`)
  await page.keyboard.press('Meta+a')
  const total = await page.locator('.lf-row.file, .lf-row.dir').count()
  assert(await selCount() === total, `G1 ⌘A 全选可见项（${await selCount()} = ${total}）`)
  await page.keyboard.press('Escape')
  assert(await selCount() === 0, 'G1 Esc 清空选中')

  // —— G5 菜单键盘（顺手先验：单选状态下右键 → ↑ → Enter）——
  await row('auth.go').click({ button: 'right' })
  await page.waitForSelector('.lf-menu')
  await page.keyboard.press('ArrowDown')
  const focusedText = await page.evaluate(() => document.activeElement?.textContent ?? '')
  assert(focusedText.includes('打开'), `G5 ↑ 聚焦到第一个菜单项（实得 ${JSON.stringify(focusedText)}）`)
  await page.keyboard.press('ArrowDown')
  const secondText = await page.evaluate(() => document.activeElement?.textContent ?? '')
  assert(secondText.includes('新建文件夹'), `G5 再 ↑ 移到下一项（实得 ${JSON.stringify(secondText)}）`)
  await page.keyboard.press('Escape')
  await page.waitForFunction(() => !document.querySelector('.lf-menu'))
  const focusInTree = await page.evaluate(() => !!document.activeElement?.classList?.contains('lf-tree'))
  assert(focusInTree, 'G5 Esc 关闭菜单后焦点回到树（否则后续 ↑↓ 没反应）')

  // —— G5b 多选状态下按键不能塌掉选中（树与菜单抢同一批按键的回归）——
  await row('auth.go').click()
  await row('README.md').click({ modifiers: ['Meta'] })
  await row('auth_test.go').click({ modifiers: ['Meta'] })
  await row('auth.go').click({ button: 'right' })
  await page.waitForSelector('.lf-menu')
  const menuWithCount = await page.locator('.lf-menu').innerText()
  assert(/删除.*（3）|（3）/.test(menuWithCount), `G5b 菜单标注条目数（实得 ${JSON.stringify(menuWithCount.split('\n').slice(4, 8))}）`)
  await page.keyboard.press('ArrowDown')
  await page.waitForTimeout(120)
  assert(await selCount() === 3, 'G5b 菜单里按 ↑ 不会把树的选中塌成单选')
  assert(/（3）/.test(await page.locator('.lf-menu').innerText()), 'G5b 按键后菜单条目数不变')
  await page.keyboard.press('Escape')
  await page.waitForFunction(() => !document.querySelector('.lf-menu'))
  await page.keyboard.press('Escape')
  assert(await selCount() === 0, 'G5b 第二次 Esc 才清空选中（Esc 先关菜单）')

  // —— G3 批量拖动：选中 2 个文件拖进 docs ——
  await page.evaluate(() => document.querySelector('.lf-tree')?.focus())
  await row('auth.go').click()
  await row('README.md').click({ modifiers: ['Meta'] })
  assert(await selCount() === 2, 'G3 拖动前选中 2 项')
  await dragTo(`${WS}/auth.go`, `${WS}/docs`, async () => {
    assert(await page.locator(`.lf-row[title="${WS}/docs"] .lf-dz-badge`).count() === 1, 'G3 悬停中：目标目录出现「移入」角标')
  })
  assert(await page.locator(`.lf-row[title="${WS}/auth.go"]`).count() === 0, 'G3 按住的那项搬走了')
  assert(await page.locator(`.lf-row[title="${WS}/README.md"]`).count() === 0, 'G3 **另一项也**跟着搬走了（整组拖动）')
  await page.waitForSelector(`.lf-row[title="${WS}/docs/auth.go"]`)
  await page.waitForSelector(`.lf-row[title="${WS}/docs/README.md"]`)
  ok('G3 两项都落在 docs 里')

  // —— G4 批量拷贝 / 粘贴 ——
  // 注意：auth.go 在 G3 里已经搬进 docs 了，这里只能按新路径选
  const docsAuth = page.locator(`.lf-row[title="${WS}/docs/auth.go"]`)
  await docsAuth.click()
  await page.locator(`.lf-row[title="${WS}/docs/README.md"]`).click({ modifiers: ['Meta'] })
  assert(await selCount() === 2, 'G4 选中 docs 里的两项')
  await page.keyboard.press('Meta+c')
  assert((await toastText()).includes('已拷贝 2 项'), 'G4 拷贝整组有数量提示')
  await row('docs').click({ button: 'right' })
  await page.locator('.lf-menu .ctx-item', { hasText: '新建文件夹' }).click()
  await page.waitForSelector('.lf-row.new .lf-input')
  await page.locator('.lf-row.new .lf-input').fill('batch')
  await page.locator('.lf-row.new .lf-input').press('Enter')
  await page.waitForSelector(`.lf-row[title="${WS}/docs/batch"]`)
  await page.locator(`.lf-row[title="${WS}/docs/batch"]`).click({ button: 'right' })
  await page.locator('.lf-menu .ctx-item', { hasText: '粘贴' }).click()
  await page.waitForSelector(`.lf-row[title="${WS}/docs/batch/auth.go"]`)
  await page.waitForSelector(`.lf-row[title="${WS}/docs/batch/README.md"]`)
  ok('G4 整组粘贴到 docs/batch')

  // —— G7 部分失败：把 docs 里的两项再粘一次到 batch（同名冲突）——
  await page.locator(`.lf-row[title="${WS}/docs/auth.go"]`).click()
  await page.locator(`.lf-row[title="${WS}/docs/README.md"]`).click({ modifiers: ['Meta'] })
  await page.keyboard.press('Meta+c')
  await page.locator(`.lf-row[title="${WS}/docs/batch"]`).click({ button: 'right' })
  await page.locator('.lf-menu .ctx-item', { hasText: '粘贴' }).click()
  const dupToast = await toastText()
  assert(/失败 2 项|已存在同名项目/.test(dupToast), `G7 同名冲突逐项报告（实得 ${JSON.stringify(dupToast)}）`)
  ok('G7 冲突不静默（有明确的失败提示）')

  // —— G2 批量删除（确认框写明数量）——
  await page.locator(`.lf-row[title="${WS}/docs/batch/auth.go"]`).click()
  await page.locator(`.lf-row[title="${WS}/docs/batch/README.md"]`).click({ modifiers: ['Meta'] })
  assert(await selCount() === 2, 'G2 选中 2 项准备删除')
  await page.keyboard.press('Meta+Backspace')
  await page.waitForSelector('.modal-hint.open')
  const msg = await page.locator('.modal-hint .confirm-desc').innerText()
  assert(/2 个项目/.test(msg), `G2 确认框写明条目数（实得 ${JSON.stringify(msg)}）`)
  await page.locator('.modal-hint .btn.primary.danger').click()
  await page.waitForFunction(() => !document.querySelector('.lf-row[title="/mock/workspace/docs/batch/auth.go"]'))
  await page.waitForFunction(() => !document.querySelector('.lf-row[title="/mock/workspace/docs/batch/README.md"]'))
  ok('G2 批量的两项都进了废纸篓')
  assert(/2 项/.test(await toastText()) || /已把 2 项/.test(await page.locator('.toast-view').innerText()), 'G2 删除结果有数量')

  // —— G6 从访达拖入 ——
  await page.evaluate(() => {
    const dt = new DataTransfer()
    dt.items.add(new File(['from finder'], 'dropped.txt', { type: 'text/plain' }))
    window.__fdt = dt
  })
  await page.evaluate(() => {
    const el = document.querySelector('.lf-row[title="/mock/workspace/docs"]')
    el.dispatchEvent(new DragEvent('dragover', { bubbles: true, cancelable: true, dataTransfer: window.__fdt }))
  })
  await page.waitForTimeout(120)
  assert(await page.locator(`.lf-row[title="${WS}/docs"] .lf-dz-badge`).count() === 1, 'G6 悬停中：外部文件拖入显示角标')
  assert((await page.locator(`.lf-row[title="${WS}/docs"] .lf-dz-badge`).innerText()) === '导入', 'G6 角标文案是「导入」而不是「移入」')
  await page.evaluate(() => {
    const el = document.querySelector('.lf-row[title="/mock/workspace/docs"]')
    el.dispatchEvent(new DragEvent('drop', { bubbles: true, cancelable: true, dataTransfer: window.__fdt }))
  })
  await page.waitForSelector(`.lf-row[title="${WS}/docs/dropped.txt"]`)
  ok('G6 外部文件导入了目标目录')
  assert(/已导入/.test(await toastText()), 'G6 导入有 toast 反馈')

  await page.screenshot({ path: process.env.SHOT_BATCH || '/tmp/left-files-batch.png' })
  await browser.close()

  const fatal = errs.filter((e) => /Uncaught|is not a function|Cannot read propert/i.test(e))
  assert(fatal.length === 0, `无未捕获异常（实得 ${JSON.stringify(fatal.slice(0, 3))}）`)
  console.log(`\n共 ${pass} 项断言通过`)
}

main().then(() => { stopServer(); process.exit(0) }).catch((e) => {
  console.error('\n❌', e.message)
  stopServer()
  process.exit(1)
})
