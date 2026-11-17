// 左栏「插件」分组 e2e（真实 UI，浏览器 + mock transport）。
//
// 覆盖（docs/creator-mode-v0.1/09-agent-bridge.md §9 + §4.6 第 6 条）：
//   P0 分组存在：每个虚拟 Project 一行，图标/「插件」徽标/会话计数都在；真实项目区不受影响
//   P1 两个子组：已安装 / 已卸载 各有标题；已卸载行没有「新建会话」
//   P2 点已安装插件行 → 打开它第一个会话（该行变 active，会话行高亮）
//   P3 点已卸载插件的会话 → 能打开（只读不等于不可读）
//   P4 点「没有会话的已安装插件」行 → 直接新建一个会话（并水合进项目）
//   P5 插件行没有 pin / 查看文件 / 删除工作区（只有新建会话）
//   P6 回执为空 → 整组隐藏，真实项目照旧
//   P7 全程零 pageerror
//
// 为什么这条要单独一个探针：LeftPanel 的插件分组是**纯 JSX**，store 级测试看不到；
// 而「分组渲染错 / 徽标缺失 / 多渲染了 pin 按钮」全是渲染层问题，单元测试抓不到。
//
// 用法：cd desktop/app && npm run e2e:plugin-projects（内部先 vite build）
// 退出码：全通过 0；任一断言失败 1。
import { chromium } from 'playwright'
import { freePort, waitHttp } from './_serve.mjs'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = Number(process.env.PORT) || await freePort()
const URL_BASE = `http://localhost:${PORT}/`

const REAL = '/mock/workspace'
const PLUG_A = '/u/.go-code/plugin-sessions/alpha/home' // 已安装，1 个会话
const PLUG_B = '/u/.go-code/plugin-sessions/beta/home' // 已卸载，1 个会话
const PLUG_C = '/u/.go-code/plugin-sessions/gamma/home' // 已安装，还没有会话

const PLUGIN_PROJECTS = [
  { path: PLUG_A, plugin_id: 'alpha', name: 'PDF 阅读器', icon: '📕', installed: true, sessions: [{ id: 'ps-a1', title: '翻译草稿' }] },
  { path: PLUG_B, plugin_id: 'beta', name: '成本看板', installed: false, sessions: [{ id: 'ps-b1', title: '上个月的账' }] },
  // gamma 的 icon 是**目录内路径**（新 manifest 的形态）：必须走图片分支，不能当文本画
  { path: PLUG_C, plugin_id: 'gamma', name: '课表', icon: 'icon.svg', installed: true },
]

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

const server = spawn('node', [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), 'preview', '--port', String(PORT), '--strictPort'], { cwd: APP, stdio: 'pipe' })
const stopServer = () => { try { server.kill('SIGTERM') } catch { /* ignore */ } }
process.on('exit', stopServer)

async function main() {
  await waitHttp(URL_BASE, { child: server, timeout: 60000 })
  const browser = await chromium.launch()
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 } })
  const errs = []
  page.on('pageerror', (e) => errs.push(String(e?.message ?? e)))
  page.on('console', (m) => { if (m.type() === 'error') errs.push(m.text()); if (process.env.DBG) console.log('[console]', m.text()) })

  // 项目树 + 插件分组都从 localStorage 喂（mock transport 的联调开关，见 src/transport/mock.ts）。
  // addInitScript 每次导航都会跑：用 phase 标记让 P6 的 reload 能控制「插件清单为空」这一态
  //（否则第二次加载又把插件喂回去，P6 永远测不到空分组）。
  await page.addInitScript((cfg) => {
    if (!localStorage.getItem('go-code.projects')) {
      localStorage.setItem('go-code.projects', JSON.stringify({
        paths: [cfg.real], active: cfg.real, lastSession: { path: cfg.real, id: 'r1' },
      }))
    }
    if ((localStorage.getItem('e2e.pluginPhase') ?? '1') === '1') {
      localStorage.setItem('mock.pluginProjects', JSON.stringify(cfg.plugins))
    }
  }, { real: REAL, plugins: PLUGIN_PROJECTS })

  await page.goto(URL_BASE)
  await page.waitForSelector('.proj-list')
  await page.waitForSelector('.plugin-projects', { timeout: 10000 })

  // —— P0 分组渲染 ——
  const plugHeads = page.locator('.ws-head.plugin')
  assert(await plugHeads.count() === 3, `三个虚拟 Project 各一行（实得 ${await plugHeads.count()}）`)
  const names = await page.locator('.ws-head.plugin .wname').evaluateAll((els) => els.map((e) => e.textContent))
  // 顺序 = 已安装子组（alpha, gamma）在前，已卸载（beta）在后 —— 这不是回执顺序，是分组结果
  assert(JSON.stringify(names) === JSON.stringify(['PDF 阅读器', '课表', '成本看板']), `行名取回执的 name，已安装组在前（实得 ${JSON.stringify(names)}）`)
  const badges = await page.locator('.ws-head.plugin .plugin-badge').evaluateAll((els) => els.map((e) => e.textContent))
  assert(badges.length === 3 && badges.every((b) => b === '插件'), `每行都有「插件」徽标（实得 ${JSON.stringify(badges)}）`)
  const icons = await page.locator('.ws-head.plugin .plugin-icon').evaluateAll((els) => els.map((e) => e.textContent))
  assert(icons.includes('📕'), '回执里的 emoji icon 原样渲染（📕）')
  // 用户 2026-11 报障：manifest.icon = "icon.svg" 被当文本画成「icon.svg」四个字。
  assert(!icons.includes('icon.svg'), '目录内路径图标不进入文本分支（不是「icon.svg」四个字）')
  // 会话列表要等 list 回执（plugin_projects_list → 每个虚拟 path 一条 list）：轮询到稳定
  await page.waitForFunction(
    () => [...document.querySelectorAll('.ws-head.plugin .ws-count')].map((e) => e.textContent).join(',') === '1,0,1',
    null,
    { timeout: 8000 },
  )
  const counts = await page.locator('.ws-head.plugin .ws-count').evaluateAll((els) => els.map((e) => e.textContent))
  assert(JSON.stringify(counts) === JSON.stringify(['1', '0', '1']), `会话计数（实得 ${JSON.stringify(counts)}）`)
  assert(await page.locator('.ws-head:not(.plugin)').count() === 1, '真实项目区只有一个工作区行（没被插件挤掉/复制）')
  assert(await page.locator('.left-sub-title').first().innerText() === '插件', '分组标题「插件」')
  await page.screenshot({ path: process.env.SHOT_PLUGINS || '/tmp/left-plugin-projects.png' })
  ok('P0 插件分组渲染：名称/图标/徽标/计数')

  // —— P1 两个子组 + 已卸载只读 ——
  const subtitles = await page.locator('.left-sub-title.sub').evaluateAll((els) => els.map((e) => e.textContent))
  assert(JSON.stringify(subtitles) === JSON.stringify(['已安装', '未安装']), `两个子组标题（实得 ${JSON.stringify(subtitles)}）`)
  const betaRow = page.locator('.ws-head.plugin', { hasText: '成本看板' })
  const alphaRow = page.locator('.ws-head.plugin', { hasText: 'PDF 阅读器' })
  assert(await betaRow.locator('.ws-new').count() === 0, '已卸载插件行没有「新建会话」')
  assert(await alphaRow.locator('.ws-new').count() === 1, '已安装插件行有「新建会话」')
  ok('P1 子组标题 + 已卸载不提供新建')

  // —— P5 只留「新建会话」：没有 pin / 查看文件 / 删除工作区 ——
  assert(await plugHeads.locator('.ws-pin').count() === 0, '插件行没有 pin')
  assert(await plugHeads.locator('.ws-files').count() === 0, '插件行没有「查看文件」')
  assert(await plugHeads.locator('.ws-del').count() === 0, '插件行没有「删除工作区」')
  assert(await plugHeads.locator('.ws-more').count() === 0, '窄形态下也不给「⋯」菜单（否则 pin/files 会从菜单漏出来）')
  ok('P5 插件行只有「新建会话」（无 pin/files/删除工作区）')

  // —— P2 点已安装插件行 → 打开第一个会话 ——
  await alphaRow.click()
  await page.waitForFunction(
    (t) => document.querySelector('.ws-head.plugin.active .wname')?.textContent === t,
    'PDF 阅读器',
    { timeout: 8000 },
  )
  assert(await alphaRow.locator('.ws-count').innerText() === '1', '插件行仍是 1 个会话（点行不新建）')
  const activeTitle = await page.locator('.plugin-group .session-item.active .session-title').first().innerText()
  assert(activeTitle === '翻译草稿', `点行打开第一个会话（活跃会话行 = "${activeTitle}"）`)
  ok('P2 已安装插件行 → 打开第一个会话')

  // —— P3 已卸载插件的会话可打开 ——
  await betaRow.click()
  await page.waitForFunction(
    (t) => document.querySelector('.ws-head.plugin.active .wname')?.textContent === t,
    '成本看板',
    { timeout: 8000 },
  )
  const betaActive = await page.locator('.plugin-group .session-item.active .session-title').first().innerText()
  assert(betaActive === '上个月的账', `已卸载插件的会话可打开（"${betaActive}"）`)
  ok('P3 已卸载插件只读但会话可打开')

  // —— P4 无会话的已安装插件：点行 = 新建会话 ——
  const gammaRow = page.locator('.ws-head.plugin', { hasText: '课表' })
  await gammaRow.click()
  await page.waitForFunction(
    () => document.querySelector('.ws-head.plugin.active .wname')?.textContent === '课表',
    null,
    { timeout: 8000 },
  )
  await page.waitForFunction(
    () => {
      const rows = [...document.querySelectorAll('.ws-head.plugin')]
      const gamma = rows.find((r) => r.querySelector('.wname')?.textContent === '课表')
      return gamma?.querySelector('.ws-count')?.textContent === '1'
    },
    null,
    { timeout: 8000 },
  )
  const gammaSessions = await page.locator('.plugin-group', { hasText: '课表' }).locator('.session-item').count()
  assert(gammaSessions >= 1, `无会话插件点行 → 新建并水合一个会话（实得 ${gammaSessions} 行）`)
  // 新建的会话必须是干净的空会话：没有把上一个项目的输入/技能带进来
  assert(await page.locator('.plugin-group .session-item.active').count() === 1, '新会话是该插件项目的活跃会话')
  ok('P4 无会话插件行 → 新建会话')

  // —— P6 回执为空 → 整组隐藏 ——
  await page.evaluate(() => {
    localStorage.setItem('e2e.pluginPhase', '2') // 让 initScript 不再回填插件清单
    localStorage.setItem('mock.pluginProjects', JSON.stringify([]))
  })
  await page.reload()
  await page.waitForSelector('.proj-list')
  // 等一次 plugin_projects_list 往返（启动即发）后再看
  await page.waitForFunction(() => !document.querySelector('.plugin-projects'), null, { timeout: 8000 })
  assert(await page.locator('.ws-head.plugin').count() === 0, '空回执 → 插件分组整组消失')
  assert(await page.locator('.ws-head:not(.plugin)').count() === 1, '真实项目照旧（没被插件分组的消失带走）')
  ok('P6 空回执 → 整组隐藏')

  assert(errs.length === 0, `全程零 pageerror（实得 ${JSON.stringify(errs.slice(0, 3))}）`)
  await page.screenshot({ path: process.env.SHOT_PLUGINS_EMPTY || '/tmp/left-plugin-projects-empty.png' })
  await browser.close()
  console.log(`\n全部通过（${pass} 项断言组）`)
}

main()
  .then(() => { stopServer(); process.exit(0) })
  .catch((e) => { console.error(e); stopServer(); process.exit(1) })
