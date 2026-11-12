#!/usr/bin/env node
// MCP / 钩子 设置面板回归（全局 × 项目两级 + 二级详情，2026-09-21）。
//
// 起因：MCP 页原只服务「当前活跃工作区」，项目层只读；hooks 有能力但没有任何 UI。
// 用户要求：① MCP 区分全局/项目（已删除的项目不展示）；② 项目级 MCP 可在 UI 增删改
// （写 {ws}/.go-code/settings.json，.mcp.json 只读）；③ 补 hooks 管理（全局 + 项目级，
// 查看 + 启停 + 增删改）；④ 形态 = 分组卡片 + 点进去的二级详情面板（对齐参考截图）。
//
// 本脚本锁住的关键行为（每条都对应「实现错了就静默退化」的点）：
//   ① 一级 = 分组卡片：全局（用户配置）+ 每个仍存在的项目各一张；**已删除项目不出现在列表**（MCP 与钩子页都查）
//   ② MCP 项目详情：配置来源两行（.mcp.json / .go-code/settings.json）+ .mcp.json 的服务器只读（「随仓库」徽标、无编辑/移除）
//   ③ MCP 项目级可编辑：启停开关写盘后回拉（徽标变「已禁用」）、新增服务器落进列表
//   ④ 钩子页三个来源卡片（用户配置 / Claude 兼容只读 / 项目）+ 项目卡「未信任」徽标
//   ⑤ 用户配置详情：4 个全局开关；「停用全部钩子」打开后每条钩子显式标注「总闸已关」（不静默隐藏）
//   ⑥ 钩子条目：展开显示 处理程序/命令/超时/信任；单条启停 → 写盘回拉后显「已停用」
//   ⑦ 新增钩子：事件/匹配/命令/超时表单 → 保存后出现在对应事件分组
//   ⑧ 项目钩子信任：点「信任此项目的钩子」→ 未信任提示消失（写 hooks-state.json 的前端链路）
//
// 用法：cd desktop/app && node e2e/verify_mcp_hooks_panel.mjs
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { freePort, waitHttp } from './_serve.mjs'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'
import fs from 'fs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
// 端口由内核分配（见 e2e/_serve.mjs 的注释）：固定端口在链里会撞上一个还没退干净的服务器。
const PORT = Number(process.env.PORT) || await freePort()
const SHOTS = path.join(__dirname, '_mcphooks_shots')

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

async function waitPort(port, timeoutMs = 30000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const up = await new Promise((resolve) => {
      const s = net.connect({ port, host: 'localhost' })
      const timer = setTimeout(() => { s.destroy(); resolve(false) }, 500)
      s.on('connect', () => { clearTimeout(timer); s.destroy(); resolve(true) })
      s.on('error', () => { s.destroy(); resolve(false) })
    })
    if (up) return
    if (Date.now() > deadline) throw new Error('vite 未就绪')
    await new Promise((r) => setTimeout(r, 250))
  }
}

// 探针页：挂真实 SettingsModal + 真实 index.css；store 走真实启动路径（mock transport）。
// 项目列表预置一个「已删除」的项目路径（/mock/gone）：mock 对未知路径返回 exists=false，
// 面板必须把它过滤掉 —— 这就是「已删除项目不展示」的回归锚点。
const PROBE_DIR = path.join(__dirname, '_mcphooks_probe')
fs.rmSync(PROBE_DIR, { recursive: true, force: true })
fs.mkdirSync(PROBE_DIR, { recursive: true })
fs.writeFileSync(path.join(PROBE_DIR, 'index.html'),
  '<!doctype html><html lang="zh" data-theme="ink"><head><meta charset="utf-8"><title>mcp/hooks panel probe</title></head>'
  + '<body><div id="root"></div><script type="module" src="./main.tsx"></script></body></html>')
fs.writeFileSync(path.join(PROBE_DIR, 'main.tsx'), `
import { createRoot } from 'react-dom/client'
import SettingsModal from '../../src/components/SettingsModal'
import { useAppStore } from '../../src/store/useAppStore'
import '../../src/index.css'

const WS = '/mock/workspace'
const GONE = '/mock/gone' // 已删除/已移除的项目：不存在的目录
localStorage.setItem('go-code.projects', JSON.stringify({ paths: [WS, GONE], active: WS, pinned: [] }))
;(window as unknown as { __store: unknown }).__store = useAppStore

createRoot(document.getElementById('root')!).render(<SettingsModal />)

void (async () => {
  await useAppStore.getState().start(WS)
  useAppStore.getState().openSettings()
  ;(window as unknown as { __ready: boolean }).__ready = true
})()
`)
fs.writeFileSync(path.join(PROBE_DIR, 'vite.config.mts'), `
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
const APP = path.resolve(__dirname, '../..')
export default defineConfig({ root: APP, plugins: [react()], server: { fs: { allow: [APP] } } })
`)

const PROBE_URL = `http://localhost:${PORT}/e2e/_mcphooks_probe/index.html`
const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(PORT), '--strictPort', '--config', path.join(PROBE_DIR, 'vite.config.mts')], { cwd: APP, stdio: 'pipe' })
server.stderr.on('data', (d) => { const s = String(d); if (!/ready in|Local:/.test(s)) process.stderr.write(s) })
const cleanup = () => {
  try { server.kill('SIGTERM') } catch { /* 已退出 */ }
  try { fs.rmSync(PROBE_DIR, { recursive: true, force: true }) } catch { /* 忽略 */ }
}
process.on('exit', cleanup)

await waitPort(PORT)
// 端口可连只说明**有东西**在监听；再确认这张探针页真能开（子进程退出即失败）
await waitHttp(PROBE_URL, { child: server })
const browser = await chromium.launch()
let failed = false
let page
try {
  fs.mkdirSync(SHOTS, { recursive: true })
  page = await browser.newPage({ viewport: { width: 1180, height: 900 }, deviceScaleFactor: 2 })
  page.on('pageerror', (e) => { console.error('  ⚠ pageerror:', e.message) })
  await page.goto(PROBE_URL, { waitUntil: 'load' })
  await page.waitForSelector('.settings-card', { timeout: 20000 })
  await page.waitForFunction('window.__ready === true', null, { timeout: 20000 })

  const nav = (label) => page.locator('.settings-nav-item', { hasText: label })
  const shot = (name) => page.screenshot({ path: path.join(SHOTS, name) })

  // —— ① MCP 一级列表：全局 + 仍存在的项目（已删除项目不出现）——
  await nav('MCP').click()
  await page.waitForSelector('.src-card', { timeout: 5000 })
  const mcpCards = await page.locator('.src-card').allInnerTexts()
  assert(mcpCards.length === 2, `MCP 分组卡片 2 张（全局 + 1 个项目），实际 ${mcpCards.length}`)
  assert(mcpCards.some((x) => x.includes('用户配置')), 'MCP 有「用户配置」全局卡片')
  assert(mcpCards.some((x) => x.includes('/mock/workspace')), 'MCP 项目卡片指向已绑定工作区')
  assert(!mcpCards.some((x) => x.includes('/mock/gone')), '已删除项目不出现在 MCP 列表')
  assert((await page.locator('.src-sec').allInnerTexts()).join('|').includes('全局'), '有「全局 / 项目」两个分区标题')
  await shot('01-mcp-list.png')

  // —— ② MCP 项目详情：配置来源两行 + .mcp.json 只读 ——
  await page.locator('.src-card', { hasText: '/mock/workspace' }).click()
  await page.waitForSelector('.detail-head', { timeout: 5000 })
  const layerRows = await page.locator('.mcp-layer').allInnerTexts()
  assert(layerRows.length === 2, `项目详情列出两层配置文件，实际 ${layerRows.length}`)
  assert(layerRows.some((x) => x.includes('.mcp.json')) && layerRows.some((x) => x.includes('.go-code/settings.json')), '两层文件路径正确')
  const repoCard = page.locator('.mcp-server', { hasText: 'repo-tools' })
  assert((await repoCard.innerText()).includes('随仓库'), '.mcp.json 的服务器标「随仓库」')
  assert((await repoCard.locator('.switch').count()) === 0, '.mcp.json 的服务器没有启停开关（只读）')
  assert((await repoCard.innerText()).includes('只读'), '.mcp.json 的服务器标「只读」')
  await shot('02-mcp-project.png')

  // —— ③ MCP 项目级可编辑：启停写盘回拉 + 新增服务器 ——
  const scratchRow = page.locator('.mcp-server', { hasText: 'scratch-notes' })
  assert((await scratchRow.innerText()).includes('本项目'), '.go-code/settings.json 的服务器标「本项目」')
  await scratchRow.locator('.switch').click()
  await page.waitForFunction(() => {
    const el = [...document.querySelectorAll('.mcp-server')].find((x) => x.textContent?.includes('scratch-notes'))
    return !!el && el.textContent.includes('已禁用')
  }, null, { timeout: 8000 })
  ok('项目级启停写盘并回拉（徽标变「已禁用」）')

  await page.locator('button', { hasText: '添加 MCP Server' }).click()
  const form = page.locator('.mcp-form-add', { hasText: '服务器名' })
  await form.locator('input[aria-label="mcp name"]').fill('proj-new')
  await form.locator('input[aria-label="mcp command"]').fill('npx')
  await form.locator('input[aria-label="mcp args"]').fill('-y @acme/new-mcp')
  await form.locator('button', { hasText: '保存' }).click()
  await page.waitForSelector('.mcp-server:has-text("proj-new")', { timeout: 8000 })
  ok('项目级新增服务器落进列表（写 {ws}/.go-code/settings.json）')
  assert((await page.locator('.mcp-server:has-text("proj-new")').innerText()).includes('本项目'), '新增的服务器标「本项目」（可编辑那层）')
  await shot('03-mcp-project-edited.png')

  // —— ③b 用户配置详情：请求头（Headers）能力 ——
  // 起因：原缺口「mcp http 模式不支持 Header 设置，导致链接无法成功」——需要鉴权的
  // 远端 MCP 端点（Authorization: Bearer …）原本无处可填。本段锁住面板侧的完整链路：
  // 类型含 streamable_http（http 的别名）+ stdio 预设含 python/python3/uvx +
  // 请求头可读可改可存（回拉后新值在场）。
  await page.locator('.detail-back').click()
  await page.waitForSelector('.src-card', { timeout: 5000 })
  await page.locator('.src-card', { hasText: '用户配置' }).click()
  await page.waitForSelector('.mcp-server', { timeout: 5000 })
  const memCard = page.locator('.mcp-server', { hasText: 'memory' })
  await memCard.locator('button', { hasText: '编辑' }).click()
  const memForm = memCard.locator('.mcp-form')
  const typeBtns = await memForm.locator('.seg[aria-label="mcp type"] button').allInnerTexts()
  assert(
    ['stdio', 'http', 'streamable_http', 'sse'].every((x) => typeBtns.includes(x)),
    `类型含 streamable_http（http 的别名），实际 ${typeBtns.join('/')}`,
  )
  // 真实用例：带 Authorization 的远端服务器打开编辑 → 请求头原样回显
  assert(await memForm.locator('input[aria-label="header key 1"]').inputValue() === 'Authorization', '请求头名回显')
  assert(await memForm.locator('input[aria-label="header value 1"]').inputValue() === 'Bearer demo-token', '请求头值回显')
  await memForm.locator('input[aria-label="header value 1"]').fill('Bearer edited-token')
  await memForm.locator('button', { hasText: '添加请求头' }).click()
  await memForm.locator('input[aria-label="header key 2"]').fill('X-Tenant')
  await memForm.locator('input[aria-label="header value 2"]').fill('acme')
  await memForm.locator('button', { hasText: '保存' }).click()
  await page.waitForFunction(() => {
    const el = [...document.querySelectorAll('.mcp-server')].find((x) => x.textContent?.includes('memory'))
    return !!el && el.textContent.includes('2 个请求头')
  }, null, { timeout: 8000 })
  ok('请求头保存后回拉（卡片标「2 个请求头」）')
  // 再进编辑：新值必须在场（保存若被静默丢弃，这里就是空的）
  await memCard.locator('button', { hasText: '编辑' }).click()
  assert(await memForm.locator('input[aria-label="header value 1"]').inputValue() === 'Bearer edited-token', '保存后请求头值持久化（回拉是新值）')
  assert(await memForm.locator('input[aria-label="header value 2"]').inputValue() === 'acme', '新增的第二个请求头持久化')
  await shot('03b-mcp-headers.png') // 仍是 http 态：请求头两行在场
  // stdio 预设：python / python3 / uvx 都在（补「Python 启动方式」的入口）
  await memForm.locator('.seg[aria-label="mcp type"] button', { hasText: 'stdio' }).click()
  const presets = await memForm.locator('.seg[aria-label="mcp preset"] button').allInnerTexts()
  assert(
    ['npx', 'uvx', 'python', 'python3', 'custom'].every((x) => presets.includes(x)),
    `stdio 预设含 uvx/python/python3，实际 ${presets.join('/')}`,
  )
  await shot('03c-mcp-stdio-presets.png')
  await memForm.locator('button', { hasText: '取消' }).click()

  // —— ④ 钩子页一级列表：三个来源 + 项目未信任徽标（已删除项目不出现）——
  await nav('钩子').click()
  await page.waitForSelector('.src-card', { timeout: 5000 })
  const hookCards = await page.locator('.src-card').allInnerTexts()
  assert(hookCards.length === 3, `钩子分组卡片 3 张（用户 + Claude 兼容 + 1 个项目），实际 ${hookCards.length}`)
  assert(hookCards.some((x) => x.includes('用户配置')), '有「用户配置」卡片')
  assert(hookCards.some((x) => x.includes('Claude 兼容')), '有「Claude 兼容（只读）」卡片')
  assert(hookCards.some((x) => x.includes('未信任')), '项目卡片带「未信任」徽标')
  assert(!hookCards.some((x) => x.includes('/mock/gone')), '已删除项目不出现在钩子页列表')
  await shot('04-hooks-list.png')

  // —— ⑤ 用户配置详情：4 个全局开关 + 总闸标注 ——
  await page.locator('.src-card', { hasText: '用户配置' }).click()
  await page.waitForSelector('.detail-head', { timeout: 5000 })
  const flags = await page.locator('.flag-row').allInnerTexts()
  assert(flags.length === 4, `全局开关 4 行（总闸/允许项目/兼容层/强制信任），实际 ${flags.length}`)
  assert((await page.locator('.hook-item').first().innerText()).includes('works-task-start'), '用户配置里有已知钩子')
  await page.locator('.flag-row', { hasText: '停用全部钩子' }).locator('.switch').click()
  await page.waitForFunction(() => {
    const el = [...document.querySelectorAll('.hook-item')].find((x) => x.textContent?.includes('works-task-start'))
    return !!el && el.textContent.includes('总闸已关')
  }, null, { timeout: 8000 })
  ok('总闸打开后每条钩子显式标注「总闸已关」（不静默隐藏）')
  await page.locator('.flag-row', { hasText: '停用全部钩子' }).locator('.switch').click()
  await page.waitForFunction(() => {
    const el = [...document.querySelectorAll('.hook-item')].find((x) => x.textContent?.includes('works-task-start'))
    return !!el && !el.textContent.includes('总闸已关')
  }, null, { timeout: 8000 })
  ok('总闸关闭后标注消失（写盘 + 回拉）')

  // —— ⑥ 钩子条目展开：处理程序/命令/超时/信任 ——
  await page.locator('.hook-item-name', { hasText: 'works-task-start' }).click()
  const body = await page.locator('.hook-item-body').first().innerText()
  assert(body.includes('处理程序') && body.includes('命令') && body.includes('超时'), '展开显示 处理程序/命令/超时')
  assert(body.includes('works-task-start.sh'), '命令内容正确')
  assert(body.includes('600'), '超时 600 秒正确')
  assert(body.includes('信任'), '展开显示信任状态')
  await shot('05-hooks-user-detail.png')

  // —— ⑦ 新增钩子：事件/匹配/命令/超时 → 保存后进对应事件分组 ——
  await page.locator('button', { hasText: '添加钩子' }).click()
  const hform = page.locator('.mcp-form-add').first()
  await hform.locator('input[aria-label="hook matcher"]').fill('bash')
  await hform.locator('input[aria-label="hook command"]').fill('/tmp/new-hook.sh')
  await hform.locator('input[aria-label="hook name"]').fill('new-hook')
  await hform.locator('input[aria-label="hook timeout"]').fill('12')
  await hform.locator('button', { hasText: '保存' }).click()
  await page.waitForSelector('.hook-item:has-text("new-hook")', { timeout: 8000 })
  ok('新增钩子保存后出现在列表（写 settings.json 的 hooks 段）')
  const newGroup = page.locator('.hook-group', { hasText: 'PreToolUse' })
  assert((await newGroup.innerText()).includes('bash'), '新增钩子落在所选事件分组（matcher 保留）')
  await page.locator('.hook-item', { hasText: 'new-hook' }).locator('.switch').click()
  await page.waitForFunction(() => {
    const el = [...document.querySelectorAll('.hook-item')].find((x) => x.textContent?.includes('new-hook'))
    return !!el && el.textContent.includes('已停用')
  }, null, { timeout: 8000 })
  ok('新增钩子可单条停用（写盘回拉后显「已停用」）')

  // —— ⑧ 项目钩子信任：点「信任此项目的钩子」→ 未信任提示消失 ——
  await page.locator('.detail-back').click()
  await page.waitForSelector('.src-card', { timeout: 5000 })
  await page.locator('.src-card', { hasText: '/mock/workspace' }).click()
  await page.waitForSelector('.hook-note', { timeout: 5000 })
  assert((await page.locator('.hook-note').first().innerText()).includes('尚未信任'), '项目详情提示未信任')
  await page.locator('button', { hasText: '信任此项目的钩子' }).click()
  await page.waitForFunction(() => !document.body.innerText.includes('尚未信任'), null, { timeout: 8000 })
  ok('信任后未信任提示消失（hooks_trust 前端链路）')
  assert((await page.locator('.hook-item').first().innerText()).includes('已停用') === false, '信任后钩子不再是「未信任」灰态')
  await shot('06-hooks-project-trusted.png')

  console.log(`\n全部通过：${pass} 项断言`)
} catch (e) {
  failed = true
  console.error('\n❌ ' + (e instanceof Error ? e.message : String(e)))
  // 失败现场留档（面板态/滚动位置对排障很关键）
  try {
    await page.screenshot({ path: path.join(SHOTS, 'zz-failure.png'), fullPage: true })
    console.error('  现场截图: ' + path.join(SHOTS, 'zz-failure.png'))
  } catch { /* 忽略 */ }
} finally {
  await browser.close()
  cleanup()
}
process.exit(failed ? 1 : 0)
