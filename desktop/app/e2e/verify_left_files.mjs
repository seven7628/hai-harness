// 左栏文件浏览 + MCP 项目维度 回归测试（真实 UI，浏览器 + mock transport）。
//
// 覆盖（2026-09-20 用户口径）：
//   L1 左栏「查看文件」入口存在；工作区行图标 = 文件夹（不再是 Globe）
//   L2 进入文件模式：标题条 = 「返回任务」+ 搜索框 + 工作区名；根目录条目渲染
//   L3 目录懒展开：点 src 才拉取子项（展开前 main.go 不存在）
//   L4 点文件 → 右侧切「文件」tab + 展开右栏 + 内容展示 + 左栏该行高亮
//   L5 搜索过滤：输入 auth 只留 auth*（目录无命中即隐藏）
//   L6 「返回任务」回到会话/工作区列表
//   M1 MCP 设置页：三层配置来源（用户/项目）+ 项目只读服务器（source=project）+ 用户配置分区
//
// 用法：cd desktop/app && npm run build && node e2e/verify_left_files.mjs
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const PORT = 4191
const URL_BASE = `http://localhost:${PORT}/`

// 行内操作按钮相对行中心的垂直偏差（px）：0 = 完全居中（此前 top:4px 实测偏高 1.7px）
async function centerDelta(page, btnSel) {
  return page.evaluate((sel) => {
    const row = document.querySelector('.ws-head')
    const btn = row?.querySelector(sel)
    if (!row || !btn) return null
    const r = row.getBoundingClientRect()
    const b = btn.getBoundingClientRect()
    return +((b.top + b.height / 2) - (r.top + r.height / 2)).toFixed(2)
  }, btnSel)
}

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
      s.on('error', () => { clearTimeout(timer); s.destroy(); resolve(false) })
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

  // ① 任务视图（默认宽度 236 = 窄形态）：行内操作收成「⋯」，工作区名因此有空间显示
  const rows = page.locator('.ws-head')
  const rowCount = await rows.count()
  assert(rowCount >= 1, `工作区行存在（实得 ${rowCount}）`)
  assert(await page.locator('.ws-head .ws-more').count() === rowCount, `每个工作区行都有「⋯」溢出按钮（${rowCount} 行）`)
  assert(await page.locator('.ws-head .ws-files, .ws-head .ws-pin, .ws-head .ws-new, .ws-head .ws-del').count() === 0, '窄形态下行内不再铺开 4 个按钮')
  const nameW = await page.locator('.ws-head .wname').first().evaluate((el) => Math.round(el.getBoundingClientRect().width))
  assert(nameW >= 100, `窄形态下工作区名有显示空间（实得 ${nameW}px）`)
  assert(await page.locator('.ws-files-btn').count() === 0, '顶部没有藏起来的全局「查看文件」入口')
  const wsIcon = await page.locator('.ws-ic svg path').first().getAttribute('d')
  assert(/^M20 20a2 2 0 0 0 2-2V8/.test(wsIcon ?? ''), `工作区行图标 = 文件夹（实得 path "${(wsIcon ?? '').slice(0, 24)}…"）`)
  assert(Math.abs((await centerDelta(page, '.ws-more')) ?? 9) <= 0.5, `「⋯」垂直居中（偏差 ${await centerDelta(page, '.ws-more')}px）`)
  await page.screenshot({ path: process.env.SHOT_TASKS || '/tmp/left-files-tasks.png' })

  // ② 从「⋯」菜单进入文件模式（窄形态下入口在菜单里；行内按钮 hover 才浮出，故先 hover 再点）
  await page.locator('.ws-head').first().hover()
  await page.locator('.ws-head .ws-more').first().click()
  await page.waitForSelector('.ws-more-menu')
  const moreItems = await page.locator('.ws-more-menu').innerText()
  assert(moreItems.includes('查看文件') && moreItems.includes('置顶') && moreItems.includes('新建会话') && moreItems.includes('删除工作区'), `⋯ 菜单收齐 4 个行内操作（实得 ${JSON.stringify(moreItems)}）`)
  await page.locator('.ws-more-menu button', { hasText: '查看文件' }).click()
  await page.waitForSelector('.lf-tree')
  assert(await page.locator('.lf-back').innerText().then((s) => s.includes('返回任务')), '文件模式标题条 = 「返回任务」')
  assert(await page.locator('.lf-search input').count() === 1, '文件模式有搜索框')
  assert((await page.locator('.lf-ws-name').innerText()).length > 0, '文件模式显示工作区名')
  const rootNames = await page.locator('.lf-row').evaluateAll((els) => els.map((e) => e.querySelector('.lf-nm')?.textContent))
  assert(rootNames.includes('src') && rootNames.includes('docs'), `根目录渲染（实得 ${JSON.stringify(rootNames)}）`)

  // ③ 懒展开：展开前无 main.go
  assert(!rootNames.includes('main.go'), '未展开 src 时不预加载其子项（懒展开）')
  await page.locator('.lf-row.dir', { hasText: 'src' }).first().click()
  await page.waitForSelector('.lf-row:has-text("main.go")')
  ok('点目录才拉取子项（展开后 main.go 出现）')

  // ④ 点文件 → 右侧展示
  await page.locator('.lf-row.file', { hasText: 'main.go' }).first().click()
  await page.waitForSelector('.lf-row.file.active')
  const activeName = await page.locator('.lf-row.file.active .lf-nm').innerText()
  assert(activeName === 'main.go', `左栏点击的文件行高亮（实得 "${activeName}"）`)
  await page.waitForSelector('.right.expanded, .right-panel.expanded, aside.right.expanded')
  // 文件内容异步到达（file_preview 响应 + 代码高亮）→ 等内容落地再断言，避免抢跑
  await page.waitForFunction(() => document.querySelector('.right')?.innerText?.includes('mock 示例'), null, { timeout: 8000 })
  const content = await page.locator('.right').innerText()
  assert(content.includes('main.go'), '右侧文件栏显示该文件路径')
  assert(content.includes('mock 示例'), '右侧展示文件内容（mock 内容可见）')
  await page.screenshot({ path: process.env.SHOT_FILES || '/tmp/left-files-open.png' })

  // ⑤ 搜索过滤
  await page.locator('.lf-search input').fill('auth')
  await page.waitForTimeout(200)
  const filtered = await page.locator('.lf-row').evaluateAll((els) => els.map((e) => e.querySelector('.lf-nm')?.textContent))
  assert(filtered.every((n) => n.includes('auth')), `搜索只留命中项（实得 ${JSON.stringify(filtered)}）`)
  assert(filtered.includes('auth.go') && filtered.includes('auth_test.go'), '搜索命中子目录里的文件（保留通路）')
  await page.locator('.lf-search input').fill('')
  await page.waitForTimeout(150)

  // ⑤b 切换工作区根目录：先加第二个工作区，再从任务视图用它的行按钮进入（每行按钮 = 以该工作区为根）
  await page.locator('.lf-ws').click()
  await page.waitForSelector('.lf-ws-menu')
  assert(await page.locator('.lf-ws-menu').innerText().then((s2) => s2.includes('切换工作区')), '切换菜单打开（标题「切换工作区」）')
  const before = await page.locator('.lf-ws-name').innerText()
  // 添加第二个工作区（mock pickWorkspace 轮换 → /mock/proj-auth），添加即切为活跃
  await page.locator('.lf-ws-menu button', { hasText: '选择文件夹' }).click()
  await page.waitForFunction((prev) => document.querySelector('.lf-ws-name')?.textContent !== prev, before, { timeout: 5000 })
  const afterAdd = await page.locator('.lf-ws-name').innerText()
  assert(afterAdd === 'proj-auth', `添加工作区后切为活跃并换根（"${before}" → "${afterAdd}"）`)
  const projRoot = await page.locator('.lf-row').evaluateAll((els) => els.map((e) => e.querySelector('.lf-nm')?.textContent))
  assert(projRoot.includes('auth.go') && projRoot.includes('README.md'), `树换成新工作区根（实得 ${JSON.stringify(projRoot)}）`)

  // 再切回原工作区：菜单里应列出两个工作区，当前项有「当前」标记
  await page.locator('.lf-ws').click()
  await page.waitForSelector('.lf-ws-menu')
  const menuText = await page.locator('.lf-ws-menu').innerText()
  assert(menuText.includes('proj-auth') && menuText.includes('workspace'), `菜单列出全部工作区（实得 ${JSON.stringify(menuText)}）`)
  assert(menuText.includes('当前'), '当前工作区有「当前」标记')
  await page.locator('.lf-ws-menu button', { hasText: 'workspace' }).first().click()
  await page.waitForFunction(() => document.querySelector('.lf-ws-name')?.textContent === 'workspace', null, { timeout: 5000 })
  const backRoot = await page.locator('.lf-row').evaluateAll((els) => els.map((e) => e.querySelector('.lf-nm')?.textContent))
  assert(backRoot.includes('src') && backRoot.includes('docs'), `切回原工作区后树随之重载（实得 ${JSON.stringify(backRoot)}）`)
  await page.screenshot({ path: process.env.SHOT_SWITCH || '/tmp/left-files-switch.png' })

  // ⑤c 返回任务后用**另一个工作区行**的按钮进入：一次点击 = 切活跃 + 以该工作区为根
  await page.locator('.lf-back').click()
  await page.waitForSelector('.ws-head')
  const targetRow = page.locator('.ws-head', { hasText: 'proj-auth' }).first()
  await targetRow.hover()
  await targetRow.locator('.ws-more').click()
  await page.locator('.ws-more-menu button', { hasText: '查看文件' }).click()
  await page.waitForSelector('.lf-tree')
  assert(await page.locator('.lf-ws-name').innerText() === 'proj-auth', '点某工作区行的按钮 → 文件视角以该工作区为根')
  const oneClickRoot = await page.locator('.lf-row').evaluateAll((els) => els.map((e) => e.querySelector('.lf-nm')?.textContent))
  assert(oneClickRoot.includes('auth.go'), `一次点击即看到该工作区文件（实得 ${JSON.stringify(oneClickRoot)}）`)

  // ⑥ 返回任务
  await page.locator('.lf-back').click()
  await page.waitForSelector('.left-title')
  assert(await page.locator('.proj-list').count() === 1, '「返回任务」回到工作区/会话列表')

  // ⑦ 拖宽面板（>240）→ 行内 4 个按钮铺开且垂直居中；拖回 → 收成「⋯」
  // 用 hover 驱动：.resize-handle 只有 4px 宽，坐标手算容易落空；hover 走元素自身命中区。
  const dragPanel = async (dx) => {
    const handle = page.locator('.resize-handle').first()
    const box = await handle.boundingBox()
    await handle.hover()
    const y = box.y + box.height / 2
    await page.mouse.down()
    await page.mouse.move(box.x + dx, y, { steps: 8 })
    await page.mouse.move(box.x + dx, y, { steps: 2 })
    await page.mouse.up()
    await page.waitForTimeout(400)
  }
  await dragPanel(90)
  const panelW = await page.locator('.left').evaluate((el) => Math.round(el.getBoundingClientRect().width))
  assert(panelW > 240, `拖宽后超过阈值（实得 ${panelW}px）`)
  const wideRows = await page.locator('.ws-head').count()
  assert(await page.locator('.ws-head .ws-files').count() === wideRows, `宽形态：每个工作区行都铺开「查看文件」按钮（${wideRows} 行）`)
  assert(await page.locator('.ws-head .ws-more').count() === 0, '宽形态：不再有「⋯」')
  assert(Math.abs((await centerDelta(page, '.ws-files')) ?? 9) <= 0.5, `宽形态行内按钮垂直居中（偏差 ${await centerDelta(page, '.ws-files')}px）`)
  await page.locator('.ws-head').first().hover()
  await page.screenshot({ path: process.env.SHOT_WIDE || '/tmp/left-files-wide.png' })
  await dragPanel(-90)
  assert(await page.locator('.ws-head .ws-more').count() === wideRows, '拖回窄宽度：重新收成「⋯」')

  // ⑧ MCP 设置页：项目维度查看
  await page.locator('.nav-item', { hasText: '设置' }).click()
  await page.waitForSelector('.settings-nav-item')
  await page.locator('.settings-nav-item', { hasText: 'MCP' }).click()
  await page.waitForSelector('.mcp-layers')
  const layerTexts = await page.locator('.mcp-layer').allInnerTexts()
  assert(layerTexts.length === 3, `配置来源列出三层（实得 ${layerTexts.length}）`)
  assert(layerTexts.some((s) => s.includes('用户')), '用户层标注「用户」')
  assert(layerTexts.filter((s) => s.includes('项目')).length >= 1, '项目层标注「项目」')
  const projRows = await page.locator('.mcp-proj .mcp-server').allInnerTexts()
  assert(projRows.some((s) => s.includes('repo-tools')), `项目层服务器只读展示（实得 ${JSON.stringify(projRows)}）`)
  assert(await page.locator('.mcp-user-h').count() === 1, '用户配置分区标题存在（与项目层区分）')
  await page.screenshot({ path: process.env.SHOT_MCP || '/tmp/left-files-mcp.png' })

  const realErrs = errs.filter((e) => !/favicon/.test(e))
  assert(realErrs.length === 0, `控制台 0 报错（实得 ${realErrs.length}：${realErrs.slice(0, 2).join(' | ')}）`)

  await browser.close()
  console.log(`\n全部通过（${pass} 项断言）`)
}

main().then(() => { stopServer(); process.exit(0) }).catch((e) => { console.error(`\n❌ ${e.message}`); stopServer(); process.exit(1) })
