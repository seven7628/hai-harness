#!/usr/bin/env node
// 端到端验证（浏览器 mock transport，2026-09）：
//   问题 1：顶部快捷栏（recentSessions）——满栏时新建会话入栏队尾 + 挤掉 index 0。
//   问题 2：设置→插件 ego lite 状态分层——已安装（未运行）不再显示「下载安装」按钮，
//           改为「启动 ego lite」；未安装才显示安装按钮。
//   问题 3：左栏折叠图标条——「新建会话」「添加工作区」在折叠态可达（此前收起后无处可点）；
//           tooltip 不被图标条 overflow 裁剪；添加工作区菜单能弹出。
//
// 用法：cd desktop/app && npm run build && node e2e/verify_recent_and_ego.mjs
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = 4183
const URL_BASE = `http://localhost:${PORT}/`

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

async function waitPort(port, timeoutMs = 25000) {
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
const stopServer = () => { try { server.kill('SIGTERM') } catch { /* 已退出 */ } }
process.on('exit', stopServer)

async function main() {
  await waitPort(PORT)
  const browser = await chromium.launch({ headless: true, channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 } })
  page.on('pageerror', (e) => console.error('[pageerror]', e.message))
  // 预置 10 条「最近会话」占满快捷栏（键名 go-code.projects 的 recent 字段）
  await page.addInitScript(
    (projects) => localStorage.setItem('go-code.projects', JSON.stringify(projects)),
    {
      paths: ['/mock/proj-auth'], active: '/mock/proj-auth',
      recent: Array.from({ length: 10 }, (_, i) => ({ path: '/mock/proj-auth', id: `old-${i}` })),
    },
  )
  await page.goto(URL_BASE, { waitUntil: 'domcontentloaded' })
  await page.waitForSelector('textarea[placeholder*="输入消息"]', { timeout: 25000 })

  // ============ 问题 1：快捷栏满栏新建会话 ============
  console.log('问题1 快捷栏：满栏（10 条）时新建会话 → 入栏队尾 + 挤掉 index 0')
  const tabs = page.locator('.recent-bar .recent-tab')
  await page.waitForTimeout(400)
  const before = await tabs.count()
  assert(before === 10, `初始应有 10 个快捷标签，got ${before}`)
  const firstBefore = (await tabs.first().innerText()).trim()
  assert(firstBefore.includes('old-0'), `index 0 应是 old-0，got "${firstBefore}"`)

  // 新建会话（左栏「新建会话」按钮）
  await page.getByRole('button', { name: '新建会话' }).first().click()
  await page.waitForTimeout(1200)

  const after = await tabs.count()
  assert(after === 10, `新建后仍应是 10 个标签（不超容量），got ${after}`)
  // 标签文本含序号/关闭符换行 → 归一化为单行便于断言与回显
  const titles = (await tabs.allInnerTexts()).map((s) => s.replace(/\s+/g, ' ').trim())
  const joined = titles.join(' | ')
  assert(!joined.includes('old-0'), `index 0（old-0）应被挤出，got ${joined}`)
  assert(joined.includes('old-1') && joined.includes('old-9'), `其余项应保留（old-1…old-9），got ${joined}`)
  // 新会话在队尾（第 10 个）且被标记为活跃
  const lastTab = tabs.nth(9)
  const lastCls = await lastTab.getAttribute('class')
  assert((lastCls ?? '').includes('active'), `队尾应是新激活的会话，got class="${lastCls}"`)
  const lastTitle = titles[9]
  assert(!lastTitle.includes('old-'), `队尾不应是历史项，got "${lastTitle}"`)
  ok(`新建会话入队尾且活跃：「${lastTitle}」，old-0 已挤出`)

  // ============ 问题 2：ego 插件页状态分层 ============
  console.log('问题2 插件页：ego lite 状态分层（已安装不再显示安装按钮）')
  // 复刻用户场景：引擎=ego（浏览器引擎行显示 ego 状态）
  await page.evaluate(() => {
    window.__mockBrowserEngine = 'ego'
    window.__mockEgoStatus = {
      installed: false, available: false, app_running: false, cli_found: false,
      error: 'ego-browser 命令未找到（未安装 ego lite 或未完成 onboarding）',
    }
  })
  await openPluginTab(page)
  await page.waitForTimeout(700)
  const installBtn = page.getByRole('button', { name: /下载安装 ego lite/ })
  assert(await installBtn.count() === 1, '未安装时应显示「下载安装 ego lite」按钮')

  // 切换为「已安装但未运行」→ 安装按钮消失、改为「启动 ego lite」
  await page.evaluate(() => {
    window.__mockEgoStatus = {
      installed: true, available: false, app_running: false, cli_found: true,
      app_path: '/Applications/ego lite.app', version: '', error: 'ego lite app 未运行',
    }
  })
  await reopenPluginTab(page)
  await page.waitForTimeout(700)
  assert(await page.getByRole('button', { name: /下载安装 ego lite/ }).count() === 0,
    '已安装（未运行）时不得再显示安装按钮')
  assert(await page.getByRole('button', { name: /启动 ego lite/ }).count() === 1,
    '已安装未运行时应显示「启动 ego lite」按钮')
  assert(await page.locator('text=ego lite 已安装').count() >= 1, '应显示「已安装」状态文案')
  ok('已安装未运行：安装按钮消失，改为「启动 ego lite」')

  // 「已安装 + 未完成引导（CLI 未注册）」→ 提示引导，仍不显示安装按钮
  await page.evaluate(() => {
    window.__mockEgoStatus = {
      installed: true, available: false, app_running: true, cli_found: false,
      app_path: '/Applications/ego lite.app', error: 'ego lite 已安装，但尚未完成引导',
    }
  })
  await reopenPluginTab(page)
  await page.waitForTimeout(700)
  assert(await page.getByRole('button', { name: /下载安装 ego lite/ }).count() === 0,
    '已安装（未完成引导）时同样不得显示安装按钮')
  assert(await page.locator('text=/尚未完成引导/').count() >= 1, '应提示「尚未完成引导」')
  ok('已安装未完成引导：无安装按钮，提示完成引导')

  // 「可用」→ 显示版本，无安装/启动按钮
  await page.evaluate(() => {
    window.__mockEgoStatus = {
      installed: true, available: true, app_running: true, cli_found: true,
      app_path: '/Applications/ego lite.app', version: 'ego-browser 0.5.0.28',
    }
  })
  await reopenPluginTab(page)
  await page.waitForTimeout(700)
  assert(await page.getByRole('button', { name: /下载安装 ego lite/ }).count() === 0, '可用时无安装按钮')
  assert(await page.getByRole('button', { name: /启动 ego lite/ }).count() === 0, '可用时无启动按钮')
  assert(await page.locator('text=/ego-browser 0.5.0.28/').count() >= 1, '应显示 ego-browser 版本')
  ok('可用：显示版本号，无安装/启动按钮')

  // ============ 问题 3：左栏「添加工作区」两个入口（展开态内联菜单 + 折叠态图标条） ============
  // 展开态内联菜单曾在 mousedown 阶段被误判为「点外部」而提前卸载 → 两个菜单项点击全部无效
  //（用户反馈：选择文件夹/新建文件夹都没反应）。先验展开态（问题现场），再验折叠态图标条。
  await verifyExpandedAddMenu(page)
  await verifyCollapsedRail(page)

  await browser.close()
  console.log(`\n全部通过（${pass} 项断言）`)
  stopServer()
  process.exit(0)
}

// verifyExpandedAddMenu 展开态「添加工作区」内联菜单：点击有效性回归。
// 历史 bug：菜单容器与触发按钮都没挂 ref → useEffect 的「点外部关闭」判定两个 ref 都是
// null（?.contains 短路成 undefined → falsy）→ 在菜单项 mousedown 时就把菜单卸载，
// 其后的 click 落空 → 「选择文件夹…」「新建文件夹…」都没反应（toggle 也退化成只开不关）。
async function verifyExpandedAddMenu(page) {
  console.log('问题3a 左栏展开态：添加工作区 → 两个入口都生效')
  await page.keyboard.press('Escape') // 关掉设置弹窗（折叠态验证前也在这之后进行）
  await page.waitForTimeout(300)

  const menuVisible = () => page.locator('.add-ws').locator('.add-ws-menu').isVisible().catch(() => false)
  const wsCount = () => page.locator('.ws-group').count()

  // 展开 → 收起：toggle 必须双向（ref 缺失时 mousedown 关、click 又开，表现为只开不关）
  await page.locator('.add-ws-btn').click()
  await page.waitForTimeout(300)
  assert(await menuVisible(), '展开态点「添加工作区」应弹出内联菜单')
  await page.locator('.add-ws-btn').click()
  await page.waitForTimeout(300)
  assert(!(await menuVisible()), '再点「添加工作区」应收起菜单（toggle 双向）')
  ok('展开态菜单开合正常')

  // 点外部关闭：ref 修复动了「点外部」判定，补一条守卫
  await page.locator('.add-ws-btn').click()
  await page.waitForTimeout(250)
  assert(await menuVisible(), '菜单应可再次展开')
  await page.mouse.click(760, 560) // 左栏之外
  await page.waitForTimeout(350)
  assert(!(await menuVisible()), '点菜单外部应收起菜单')
  ok('展开态菜单点外部关闭')

  // 「选择文件夹…」：mock transport 按次轮换工作区（第 1 次可能绕回当前工作区 = 无变化，
  // 故最多点两次）→ 应注册出新工作区
  const before = await wsCount()
  let after = before
  for (let i = 0; i < 2 && after === before; i++) {
    await page.locator('.add-ws-btn').click()
    await page.waitForTimeout(250)
    await page.locator('.add-ws-menu button').first().click()
    await page.waitForTimeout(700)
    after = await wsCount()
  }
  assert(after > before, `「选择文件夹…」应注册新工作区（${before} → ${after}）`)
  ok(`「选择文件夹…」生效（工作区 ${before} → ${after}）`)

  // 「新建文件夹…」：应打开应用内 modal（而非静默无反应）
  await page.locator('.add-ws-btn').click()
  await page.waitForTimeout(250)
  assert(await menuVisible(), '菜单应可再次展开')
  await page.locator('.add-ws-menu button').last().click()
  await page.waitForTimeout(600)
  assert(await page.locator('.nwf-card').count() === 1, '「新建文件夹…」应打开新建工作区弹窗')
  ok('「新建文件夹…」生效（打开弹窗）')
  await page.keyboard.press('Escape')
  await page.waitForTimeout(300)
}

// verifyCollapsedRail 折叠图标条验证：新按钮可达 + tooltip 不被裁剪 + 添加工作区菜单可弹出
async function verifyCollapsedRail(page) {
  console.log('问题3 左栏折叠图标条：新建会话/添加工作区可达 + tooltip 不被裁剪')
  await page.keyboard.press('Escape') // 关掉设置弹窗
  await page.waitForTimeout(300)
  // 折叠前记录展开态两个入口的图标路径（折叠后这些元素已卸载，无法再取）
  const expandedIcons = await page.evaluate(() => {
    const paths = (sel) => Array.from(document.querySelectorAll(`${sel} svg path`)).map((p) => p.getAttribute('d')).join('|')
    return { newSession: paths('.new-session-btn'), addWs: paths('.add-ws-btn') }
  })
  assert(expandedIcons.newSession && expandedIcons.addWs, '展开态应能取到两个入口的图标')
  await page.keyboard.press('Meta+b') // 折叠左栏
  await page.waitForTimeout(800)

  const rail = page.locator('.left-rail')
  assert(await rail.count() === 1, '折叠后应出现图标条')
  const newBtn = page.locator('.left-rail .rbtn[aria-label="新建会话"]')
  const addBtn = page.locator('.left-rail .rbtn[aria-label="添加工作区"]')
  assert(await newBtn.count() === 1, '折叠态应有「新建会话」图标')
  assert(await addBtn.count() === 1, '折叠态应有「添加工作区」图标')

  // 顺序：建会话 / 加工作区在工作区图标之上（入口动作在前，列表开关在后）
  const order = await page.locator('.left-rail .rbtn').evaluateAll((els) => els.map((e) => e.getAttribute('aria-label')))
  const iNew = order.indexOf('新建会话')
  const iAdd = order.indexOf('添加工作区')
  const iWs = order.indexOf('工作区')
  assert(iNew >= 0 && iAdd >= 0 && iWs >= 0, `三个图标都应存在，got ${order.join(',')}`)
  assert(iNew < iWs && iAdd < iWs, `建会话/加工作区应排在「工作区」之上，got ${order.join(',')}`)
  assert(iAdd === iNew + 1, `加工作区应紧跟建会话，got ${order.join(',')}`)
  ok(`图标条顺序：${order.join(' → ')}（动作在工作区之上）`)

  // 图标与展开态一一对应（折叠/展开用同一套语义图标，不再是裸加号）
  const collapsedIcons = await page.evaluate(() => {
    const paths = (sel) => Array.from(document.querySelectorAll(`${sel} svg path`)).map((p) => p.getAttribute('d')).join('|')
    return { newSession: paths('.rail .rbtn[aria-label="新建会话"]'), addWs: paths('.rail .rbtn[aria-label="添加工作区"]') }
  })
  assert(collapsedIcons.newSession === expandedIcons.newSession,
    '折叠态「新建会话」图标应与展开态入口一致')
  assert(collapsedIcons.addWs === expandedIcons.addWs,
    '折叠态「添加工作区」图标应与展开态入口一致')
  ok('折叠/展开两个态用同一套入口图标（气泡+ / 文件夹+）')

  // tooltip 不被图标条 overflow:hidden 裁剪：fixed 定位的坐标变量落在图标条之外
  await newBtn.hover()
  await page.waitForTimeout(500)
  const tip = await newBtn.evaluate((el) => {
    const cs = getComputedStyle(el)
    return {
      x: cs.getPropertyValue('--tip-x').trim(),
      opacity: getComputedStyle(el, '::after').opacity,
      position: getComputedStyle(el, '::after').position,
    }
  })
  assert(tip.position === 'fixed', `tooltip 应为 fixed 定位（避开裁剪），got ${tip.position}`)
  assert(parseInt(tip.x, 10) >= 44, `tooltip 横坐标应在图标条（44px）之外，got ${tip.x}`)
  assert(tip.opacity === '1', `hover 时 tooltip 应可见，got opacity=${tip.opacity}`)
  ok(`tooltip fixed 定位在 x=${tip.x}（图标条 44px 之外，未被裁剪）`)

  // 折叠态新建会话：建会话入快捷栏，且左栏保持折叠
  await newBtn.click()
  await page.waitForTimeout(1200)
  const st = await page.evaluate(() => ({
    leftCollapsed: document.querySelector('.left')?.classList.contains('collapsed') === true,
    tabs: document.querySelectorAll('.recent-bar .recent-tab').length,
  }))
  assert(st.leftCollapsed, '折叠态新建会话后左栏应保持折叠')
  assert(st.tabs >= 1, `折叠态新建的会话应进快捷栏，got ${st.tabs}`)
  ok(`折叠态新建会话 → 入快捷栏（${st.tabs} 条）且左栏保持折叠`)

  // 添加工作区菜单：能弹出（fixed 定位）且两个入口都在
  await addBtn.click()
  await page.waitForTimeout(500)
  const menu = page.locator('.rail-menu')
  assert(await menu.count() === 1, '折叠态点「添加工作区」应弹出菜单')
  const items = await menu.locator('button').allInnerTexts()
  assert(items.length === 2, `菜单应有两个入口，got ${items.length}`)
  assert(items.join('|').includes('选择文件夹'), `应有「选择文件夹…」，got ${items.join('|')}`)
  assert(items.join('|').includes('新建文件夹'), `应有「新建文件夹…」，got ${items.join('|')}`)
  const menuPos = await page.evaluate(() => getComputedStyle(document.querySelector('.rail-menu')).position)
  assert(menuPos === 'fixed', `菜单应为 fixed 定位（避开裁剪），got ${menuPos}`)
  ok('添加工作区菜单可弹出（fixed 定位，两入口齐全）')

  // 点外部关闭
  await page.mouse.click(700, 500)
  await page.waitForTimeout(400)
  assert(await page.locator('.rail-menu').count() === 0, '点菜单外部应关闭菜单')
  ok('点外部关闭菜单')

  // 菜单项可用：点「新建文件夹…」打开应用内 modal
  await addBtn.click()
  await page.waitForTimeout(400)
  await page.locator('.rail-menu button').last().click()
  await page.waitForTimeout(800)
  assert(await page.locator('[role="dialog"]').count() >= 1, '「新建文件夹…」应打开新建工作区弹窗')
  ok('菜单项「新建文件夹…」可用（打开弹窗）')
  await page.keyboard.press('Escape')
  await page.waitForTimeout(300)
}

// openPluginTab 打开设置 → 插件 tab（幂等：已打开则复用）
async function openPluginTab(page) {
  const modal = page.locator('.settings-modal, [role="dialog"]')
  if (!(await modal.count())) {
    await page.getByRole('button', { name: /设置/ }).first().click()
    await page.waitForTimeout(600)
  }
  await page.getByRole('button', { name: /^插件$/ }).first().click()
  await page.waitForTimeout(400)
}
// reopenPluginTab 关闭再打开（强制重新拉取 mock 状态，验证状态分层随之更新）
async function reopenPluginTab(page) {
  await page.keyboard.press('Escape')
  await page.waitForTimeout(300)
  await openPluginTab(page)
}

main().catch((e) => {
  console.error('❌', e.message)
  stopServer()
  process.exit(1)
})
