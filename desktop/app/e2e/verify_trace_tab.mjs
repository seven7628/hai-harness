#!/usr/bin/env node
// 链路 tab（trace 瀑布图）UI 回归（docs/TRACE_CHAIN_REDESIGN.md P3–P5）。
//
// 驱动 mock 传输的真实前端（无 bridge）：覆盖
//   ① 空态降级（无 trace → 提示，不崩）
//   ② 瀑布图渲染：五类 span（run/llm/tool/compress/wait）都出条 + 刻度尺
//   ③ 时间轴交互：缩放（⌘+滚轮 / ＋ / 适应）、折叠全部/展开全部
//   ④ trace 选择器：列表 → 切历史 trace（命令拉详情）→ Alt+←/→ 键盘切换
//   ⑤ Span 详情抽屉：按 kind 分支渲染 + 关闭
//
// 用法：cd desktop/app && node e2e/verify_trace_tab.mjs
// 退出码：全通过 0；任一断言失败非 0。
import { chromium } from 'playwright'
import { spawn } from 'child_process'
import path from 'path'
import { fileURLToPath } from 'url'
import net from 'net'
import { freePort } from './_serve.mjs'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
// 端口由内核分配（e2e/_serve.mjs 的注释 / 04-review F46）：固定端口在 npm test 链里会撞上一个
// 还没退干净的 preview 服务，那种「红」跟代码无关 —— 本轮把它挂进链，先把这个坑堵上。
const PORT = Number(process.env.PORT) || await freePort()
const URL_BASE = `http://localhost:${PORT}/`

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function waitPort(port, timeoutMs = 30000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const up = await new Promise((resolve) => {
      const s = net.connect({ port, host: 'localhost' })
      const timer = setTimeout(() => { s.destroy(); resolve(false) }, 500)
      s.on('connect', () => { clearTimeout(timer); s.destroy(); resolve(true) })
      s.on('error', () => { clearTimeout(timer); resolve(false) })
    })
    if (up) return
    if (Date.now() > deadline) throw new Error(`端口 ${port} 未就绪`)
    await sleep(300)
  }
}

const preview = spawn('npx', ['vite', 'preview', '--port', String(PORT), '--strictPort'], {
  cwd: APP, stdio: 'ignore', detached: false,
})
let browser
try {
  await waitPort(PORT)
  browser = await chromium.launch()
  const page = await browser.newPage({ viewport: { width: 1400, height: 900 } })
  page.on('pageerror', (e) => { throw new Error(`页面异常: ${e.message}`) })

  // 答一批 ask_user 提问（mock 流程的一环：答完才跑到 edit_file）。
  //
  // 【为什么重写】原来等的是 `[role=dialog][aria-modal=true]` —— 那是 ask_user 还是**弹窗**时的
  // 选择器。改版后 AskUserQuestionPanel 住在 CenterPanel 的 `.dock` 里（role="region"，每题
  // `.q-item`、选项 `.q-option`、提交是 dock 里的主按钮），旧选择器恒 null：提问永远没人答 →
  // mock 的 edit_file 永远不执行 → ⑧ 干等 18s 找不到行。它不在 npm test 链里，所以红了几轮
  // 没人知道（00 §4.6：链外的红迟早烂成「既有红」）。
  // 现在的答法对两种布局都成立：逐题点第一个未选选项 → 有「提交」就提交，没有就点「下一问」。
  const answerAskUser = async (timeoutMs = 0) => {
    const deadline = Date.now() + timeoutMs
    while (!(await page.$('.dock'))) {
      if (Date.now() >= deadline) return false
      await page.waitForTimeout(250)
    }
    for (let step = 0; step < 6; step++) {
      await page.evaluate(() => {
        for (const q of document.querySelectorAll('.dock .ask, .dock .q-item, .dock .ask-list')) {
          // 两个世代的选项类名都要认：设计版是 button.ask-opt（选中态在 aria-checked），
          // 旧版是 .q-option.sel。只认旧的 → 一次都没点中 → 提交按钮永远禁用 →
          // mock 永远跑不到 edit_file（⑧ 干等 18s）。
          const opt = q.querySelector('.ask-opt[aria-checked="false"], .q-option:not(.sel)')
          if (opt) opt.click()
        }
      })
      await page.waitForTimeout(200)
      const submitted = await page.evaluate(() => {
        const b = [...document.querySelectorAll('.dock button')].find((x) => /提交/.test(x.textContent || '') && !x.disabled)
        if (b) { b.click(); return true }
        const next = document.querySelector('.dock .ask-btn.go:not([disabled])')
        if (next) { next.click(); return false }
        return false
      })
      await page.waitForTimeout(300)
      if (submitted) return true
    }
    return false
  }
  await page.goto(URL_BASE, { waitUntil: 'load' })
  await page.waitForTimeout(2500)

  // —— 准备：新建会话 + 发一条消息（mock 会跑完整流程并生成 trace 事件） ——
  console.log('\n【准备】新建会话并产生执行链路')
  await page.evaluate(() => {
    const b = [...document.querySelectorAll('button')].find((x) => /新建会话/.test(x.textContent || ''))
    if (b) b.click()
  })
  await page.waitForTimeout(2500)
  await page.evaluate(() => {
    const ta = document.querySelector('textarea')
    if (!ta) return
    const setter = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set
    setter.call(ta, '给链路 tab 加时间轴瀑布图')
    ta.dispatchEvent(new Event('input', { bubbles: true }))
  })
  await page.waitForTimeout(400)
  await page.evaluate(() => {
    const b = [...document.querySelectorAll('button')].find((x) => /发送/.test(x.getAttribute('aria-label') || ''))
    if (b) b.click()
  })
  // ask_user 提问：答完提交（mock 流程的一环）
  const answeredOnce = await answerAskUser(20000)
  if (!answeredOnce) console.log('  ⚠️ 准备阶段没等到 .dock（提问面板）——后面的链路会缺 edit_file 行')
  await page.waitForTimeout(7000)

  // —— 展开右栏 + 链路 tab ——
  for (let i = 0; i < 5; i++) {
    const cls = await page.evaluate(() => document.querySelector('.right')?.className ?? '')
    if (!cls.includes('collapsed')) break
    await page.evaluate(() => document.querySelector('.right-rail .rbtn')?.click())
    await page.waitForTimeout(700)
  }
  await page.evaluate(() => {
    // 阶段 1 起：面板级标签条 = .rtabs 里的标签实例（.rtab 是 div，不是 button）。
    const b = [...document.querySelectorAll('.right .rtabs .rtab')].find((x) => /链路/.test(x.textContent || ''))
    if (b) b.click()
  })
  await page.waitForTimeout(1200)

  // —— ① 实时 trace 渲染 ——
  console.log('\n【①】实时投影渲染')
  const live = await page.evaluate(() => ({
    hasRail: !!document.querySelector('.tr-rail'),
    hasWf: !!document.querySelector('.wf'),
    rows: document.querySelectorAll('.wf-row').length,
    ticks: document.querySelectorAll('.tick').length,
    header: document.querySelector('.trace-hd')?.textContent ?? '',
  }))
  assert(live.hasRail, 'trace 选择器渲染')
  assert(live.hasWf, '瀑布图渲染')
  assert(live.rows > 0, `瀑布图有行（${live.rows}）`)
  assert(live.ticks > 0, `时间刻度尺渲染（${live.ticks} 个刻度）`)
  assert(/spans/.test(live.header), '汇总 header 含 spans 统计')

  // —— ② 切历史 trace（命令拉详情）→ 五类 span ——
  console.log('\n【②】历史 trace：命令拉取 + 五类 span')
  await page.evaluate(() => document.querySelector('.tr-pick')?.click())
  await page.waitForTimeout(1200)
  const popRows = await page.evaluate(() => document.querySelectorAll('.tr-pop .tr-row').length)
  assert(popRows >= 1, `trace 列表弹层有 ${popRows} 条`)
  await page.evaluate(() => {
    const rows = document.querySelectorAll('.tr-pop .tr-row')
    rows[rows.length - 1]?.click() // 选最后一条（最旧 = 历史，含完整示例 span）
  })
  await page.waitForTimeout(1800)
  const hist = await page.evaluate(() => ({
    kinds: [...document.querySelectorAll('.wf-lane .bar')].map((b) => b.className.replace('bar ', '')),
    labels: [...document.querySelectorAll('.wf-name .lbl')].map((x) => x.textContent),
  }))
  const kindSet = new Set(hist.kinds.map((k) => k.replace(/ (err|busy|warn)$/, '')))
  for (const k of ['k-run', 'k-llm', 'k-tool', 'k-compress', 'k-wait']) {
    assert(kindSet.has(k), `渲染 ${k} 类型的 span`)
  }
  assert(hist.kinds.some((k) => k.includes('err')), '错误态 span 有独立样式')
  assert(hist.kinds.some((k) => k.includes('busy')), '运行中 span 有独立样式')

  // —— ②b 并行窗口底纹 ——
  // 真实数据实测 22.8% 的 trace 含并发重叠；底纹必须精确覆盖重叠区间（不是整行涂色）。
  console.log('\n【②b】并行窗口底纹')
  const par = await page.evaluate(() => {
    const pars = [...document.querySelectorAll('.parw')]
    const lane = document.querySelector('.wf-lane')?.getBoundingClientRect()
    return {
      count: pars.length,
      spans: pars.map((p) => {
        const r = p.getBoundingClientRect()
        return { offset: lane ? Math.round(r.x - lane.x) : null, w: Math.round(r.width) }
      }),
    }
  })
  assert(par.count > 0, `并发分支渲染出并行窗口底纹（${par.count} 段）`)
  assert(par.spans.every((s) => s.w > 0 && s.offset >= 0), '底纹有正宽度且落在泳道内')
  assert(par.spans.every((s) => s.offset + s.w <= 400), '底纹不越出泳道右边界')

  // —— ③ 时间轴交互 ——
  console.log('\n【③】时间轴缩放 / 折叠')
  const z0 = await page.evaluate(() => document.querySelector('.wf-tools .zr')?.textContent)
  await page.evaluate(() => document.querySelector('.wf-foot button[data-z="in"]')?.click())
  await page.waitForTimeout(400)
  const z1 = await page.evaluate(() => document.querySelector('.wf-tools .zr')?.textContent)
  assert(z0 !== z1, `放大改变缩放比（${z0} → ${z1}）`)
  await page.evaluate(() => document.querySelector('.wf-foot button[data-z="fit"]')?.click())
  await page.waitForTimeout(400)
  const z2 = await page.evaluate(() => document.querySelector('.wf-tools .zr')?.textContent)
  assert(z2 !== z1, `适应宽度复位缩放（${z1} → ${z2}）`)

  const rowsAll = await page.evaluate(() => document.querySelectorAll('.wf-row').length)
  await page.evaluate(() => document.querySelectorAll('.wf-tools .tb')[1]?.click())
  await page.waitForTimeout(500)
  const rowsCollapsed = await page.evaluate(() => document.querySelectorAll('.wf-row').length)
  assert(rowsCollapsed < rowsAll, `折叠全部收起子行（${rowsAll} → ${rowsCollapsed}）`)
  await page.evaluate(() => document.querySelectorAll('.wf-tools .tb')[0]?.click())
  await page.waitForTimeout(500)
  const rowsBack = await page.evaluate(() => document.querySelectorAll('.wf-row').length)
  assert(rowsBack === rowsAll, `展开全部恢复（${rowsCollapsed} → ${rowsBack}）`)

  // —— ④ Alt + ←/→ 键盘切换 ——
  console.log('\n【④】Alt + ← / → 切换 trace')
  const beforeNav = await page.evaluate(() => document.querySelector('.tr-lab')?.textContent)
  await page.keyboard.down('Alt'); await page.keyboard.press('ArrowLeft'); await page.keyboard.up('Alt')
  await page.waitForTimeout(1400)
  const afterLeft = await page.evaluate(() => document.querySelector('.tr-lab')?.textContent)
  await page.keyboard.down('Alt'); await page.keyboard.press('ArrowRight'); await page.keyboard.up('Alt')
  await page.waitForTimeout(1400)
  const afterRight = await page.evaluate(() => document.querySelector('.tr-lab')?.textContent)
  assert(beforeNav !== afterLeft || afterLeft !== afterRight, 'Alt+←/→ 可切换 trace（标签变化）')

  // —— ⑤ Span 详情抽屉 ——
  console.log('\n【⑤】Span 详情抽屉（按 kind 分支）')
  const drawerChecks = []
  const n = await page.evaluate(() => document.querySelectorAll('.wf-row').length)
  for (let i = 0; i < Math.min(n, 10); i++) {
    await page.evaluate((k) => document.querySelectorAll('.wf-row')[k]?.click(), i)
    await page.waitForTimeout(350)
    const d = await page.evaluate(() => ({
      open: !!document.querySelector('.span-sheet.open'),
      kind: document.querySelector('.span-sheet .kindchip')?.textContent ?? '',
      hasKv: document.querySelectorAll('.span-sheet .kv-i').length > 0,
    }))
    if (d.open) drawerChecks.push(d)
    await page.evaluate(() => document.querySelector('.span-sheet .sheet-x')?.click())
    await page.waitForTimeout(250)
  }
  assert(drawerChecks.length > 0, `点击 span 升起抽屉（${drawerChecks.length} 次）`)
  assert(drawerChecks.every((d) => d.hasKv), '抽屉含时间/时长等元数据')
  const kindLabels = new Set(drawerChecks.map((d) => d.kind).filter(Boolean))
  assert(kindLabels.size >= 2, `抽屉按 kind 分支渲染（出现 ${[...kindLabels].join(' / ')}）`)

  // —— ⑤b 抽屉必须被右栏「关住」 ——
  // 回归：.right 缺 position:relative 时，absolute 抽屉会挂到 body 上横跨整个窗口、盖住中栏
  //（原型 docs/trace_waterfall_lab.html:908 记录过同一坑）。
  console.log('\n【⑤b】抽屉限定在右侧侧边栏内')
  await page.evaluate(() => {
    const rows = [...document.querySelectorAll('.wf-row')]
    ;(rows.find((r) => r.querySelector('.bar.k-llm')) ?? rows[0])?.click()
  })
  await page.waitForTimeout(600)
  const box = await page.evaluate(() => {
    const sheet = document.querySelector('.span-sheet')
    const scrim = document.querySelector('.sheet-scrim')
    const right = document.querySelector('.right')
    if (!sheet || !right) return null
    const s = sheet.getBoundingClientRect()
    const r = right.getBoundingClientRect()
    const sc = scrim?.getBoundingClientRect()
    return {
      sheet: { x: s.x, right: s.right, w: s.width },
      scrim: sc ? { x: sc.x, right: sc.right } : null,
      right: { x: r.x, right: r.right },
    }
  })
  assert(!!box, '抽屉与右栏均存在')
  assert(box.sheet.x >= box.right.x - 1, '抽屉左边界不越过右栏（不溢出到中栏）')
  assert(box.sheet.right <= box.right.right + 1, '抽屉右边界不超出右栏')
  assert(box.sheet.w < box.right.right - box.right.x, '抽屉窄于右栏（不是全窗口宽）')
  assert(!!box.scrim && box.scrim.x >= box.right.x - 1, '遮罩同样限定在右栏内')
  await page.evaluate(() => document.querySelector('.span-sheet .sheet-x')?.click())
  await page.waitForTimeout(300)

  // —— ⑥ 刷新后 trace 仍可回显 ——
  // 说明：**mock 传输不持久化会话**（reload 后回到空态、无历史可恢复），
  // 因此「刷新后回显」这条路径在 mock 下不可达 —— 它由
  // scripts/test-trace.mjs ⑥（store 级：traceEvents 清空 + traceFallback 兜底）覆盖。
  // 这里只断言刷新后不崩、且渲染链路 tab 不报错（真实 bridge 的刷新由 store 测试保证语义）。
  console.log('\n【⑥】刷新后不崩（回显语义见 store 测试 ⑥）')
  await page.reload({ waitUntil: 'load' })
  await page.waitForTimeout(3500)
  for (let i = 0; i < 5; i++) {
    const cls = await page.evaluate(() => document.querySelector('.right')?.className ?? '')
    if (!cls.includes('collapsed')) break
    await page.evaluate(() => document.querySelector('.right-rail .rbtn')?.click())
    await page.waitForTimeout(700)
  }
  await page.evaluate(() => {
    // 阶段 1 起：面板级标签条 = .rtabs 里的标签实例（.rtab 是 div，不是 button）。
    const b = [...document.querySelectorAll('.right .rtabs .rtab')].find((x) => /链路/.test(x.textContent || ''))
    if (b) b.click()
  })
  await page.waitForTimeout(1200)
  const afterReload = await page.evaluate(() => ({
    rightOk: !!document.querySelector('.right'),
    crashed: !!document.querySelector('#root:empty'),
  }))
  assert(afterReload.rightOk && !afterReload.crashed, '刷新后右栏正常挂载（未崩溃）')

  // —— ⑦ 运行中（无 agent_end）显示「运行中」而非「未完成」 ——
  // 新建会话并**不等待** agent_end，直接看链路：run span 应为 running。
  console.log('\n【⑦】运行中（agent_start 无 agent_end）显示运行中')
  await page.evaluate(() => {
    const b = [...document.querySelectorAll('button')].find((x) => /新建会话/.test(x.textContent || ''))
    if (b) b.click()
  })
  await page.waitForTimeout(2200)
  await page.evaluate(() => {
    const ta = document.querySelector('textarea')
    if (!ta) return
    const setter = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set
    setter.call(ta, '给链路 tab 加时间轴瀑布图')
    ta.dispatchEvent(new Event('input', { bubbles: true }))
  })
  await page.waitForTimeout(300)
  await page.evaluate(() => {
    const b = [...document.querySelectorAll('button')].find((x) => /发送/.test(x.getAttribute('aria-label') || ''))
    if (b) b.click()
  })
  // 展开右栏 + 链路 tab（刷新后右栏可能收起）
  await page.waitForTimeout(1200)
  for (let i = 0; i < 5; i++) {
    const cls = await page.evaluate(() => document.querySelector('.right')?.className ?? '')
    if (!cls.includes('collapsed')) break
    await page.evaluate(() => document.querySelector('.right-rail .rbtn')?.click())
    await page.waitForTimeout(600)
  }
  await page.evaluate(() => {
    // 阶段 1 起：面板级标签条 = .rtabs 里的标签实例（.rtab 是 div，不是 button）。
    const b = [...document.querySelectorAll('.right .rtabs .rtab')].find((x) => /链路/.test(x.textContent || ''))
    if (b) b.click()
  })
  // 等出现 wf-row 即查（不等流程跑完 → agent_end 尚未到）
  let mid = null
  for (let i = 0; i < 30; i++) {
    await page.waitForTimeout(400)
    mid = await page.evaluate(() => {
      const bars = [...document.querySelectorAll('.wf-lane .bar')]
      return {
        rows: document.querySelectorAll('.wf-row').length,
        busy: bars.some((b) => b.className.includes('busy')),
        header: document.querySelector('.trace-hd')?.textContent ?? '',
      }
    })
    if (mid.rows > 0) break
  }
  assert(!!mid && mid.rows > 0, `运行中已有链路行（${mid?.rows}）`)
  assert(!/未完成/.test(mid?.header ?? ''), `运行中不显示「未完成」（实得：${(mid?.header ?? '').slice(0, 60)}）`)

  // —— ⑧ 带 diff 的工具 span 开抽屉不崩（回归：diff.unified 是字符串，不是 {t,s}[]） ——
  // edit_file 的工具块带 diff（unified 为 go-difflib 文本 + truncated 标记）。
  // 曾按行对象渲染 → TypeError: diff.unified.map is not a function（抽屉打不开）。
  // 注意：edit_file 在上面的 ask_user 提问被回答之后才出现（mock 流程），故先答题。
  console.log('\n【⑧】带 diff 的工具 span 抽屉（unified 字符串渲染）')
  const pageErrors = []
  page.on('pageerror', (e) => pageErrors.push(e.message))
  // 答提问的 helper 在文件顶部（按当前 .dock 布局；旧的 [role=dialog] 选择器已失配）
  let answered = await answerAskUser(20000)
  let diffRow = -1
  for (let i = 0; i < 60; i++) {
    // 提问可能因时序稍晚才弹出：循环里持续兜底
    if (!answered) answered = await answerAskUser()
    diffRow = await page.evaluate(() =>
      [...document.querySelectorAll('.wf-row')].findIndex((r) => /edit_file/.test(r.querySelector('.lbl')?.textContent || '')))
    if (diffRow >= 0) break
    await page.waitForTimeout(300)
  }
  assert(diffRow >= 0, `实时链路含 edit_file 行（row ${diffRow}）`)
  await page.evaluate((k) => document.querySelectorAll('.wf-row')[k]?.click(), diffRow)
  await page.waitForTimeout(600)
  const diffSheet = await page.evaluate(() => ({
    open: !!document.querySelector('.span-sheet.open'),
    title: document.querySelector('.span-sheet .sec-title')?.textContent ?? '',
    uniLines: document.querySelectorAll('.span-sheet .diff-body .uni .ln').length,
    add: document.querySelectorAll('.span-sheet .diff-body .uni .ad').length,
    del: document.querySelectorAll('.span-sheet .diff-body .uni .dl').length,
  }))
  assert(diffSheet.open, '带 diff 的工具抽屉能打开（未崩溃）')
  assert(diffSheet.uniLines > 0, `diff 按行渲染（${diffSheet.uniLines} 行）`)
  assert(diffSheet.add > 0 || diffSheet.del > 0, `diff 有 +/- 着色（+${diffSheet.add} / -${diffSheet.del}）`)
  assert(pageErrors.length === 0, `抽屉渲染无页面异常（实得 ${pageErrors.length} 次：${pageErrors[0] ?? '—'}）`)
  await page.evaluate(() => document.querySelector('.span-sheet .sheet-x')?.click())
  await page.waitForTimeout(300)

  // —— ⑨ header 时间对比（墙钟 / 累计 / 并行省）+ 占比条 + 图例 ——
  // 回归：占比条此前复用 .mix（无 i 高度规则 + 父级 display:block）→ 条高 0 不可见；
  // header 也没有「墙钟 vs 累计」这组对比数字（并发时累计可超墙钟）。
  console.log('\n【⑨】header 时间对比 + 时间占比条 + 图例')
  const hd = await page.evaluate(() => {
    const box = document.querySelector('.trace-hd')
    const segs = [...document.querySelectorAll('.th-bar i')]
    const swatches = [...document.querySelectorAll('.th-legend i')]
    return {
      meta: [...box.querySelectorAll('.th-meta span')].map((s) => s.textContent ?? ''),
      barCount: segs.length,
      barH: segs.map((s) => Math.round(s.getBoundingClientRect().height)),
      barW: segs.map((s) => Math.round(s.getBoundingClientRect().width)),
      legend: [...document.querySelectorAll('.th-legend span')].map((s) => s.textContent ?? ''),
      swatch: swatches.map((s) => Math.round(s.getBoundingClientRect().width)),
    }
  })
  // 标签 = i18n rp.trace.wall；2026-11 把「墙钟」改成「实际耗时」（用户不懂墙钟）。
  assert(hd.meta.some((x) => /实际耗时/.test(x)), `header 显示实际耗时（实得 ${hd.meta.join(' | ')}）`)
  assert(hd.meta.some((x) => /累计/.test(x)), 'header 显示累计耗时（可与墙钟对比）')
  assert(hd.barCount > 0 && hd.barH.every((h) => h > 0), `时间占比条可见（${hd.barCount} 段，高 ${hd.barH.join('/')}px）`)
  assert(hd.barW.every((w) => w > 0), `占比条有正宽度（${hd.barW.join('/')}px）`)
  assert(hd.legend.length > 0, `占比条有图例（${hd.legend.join(' | ')}）`)
  assert(hd.swatch.every((w) => w > 0), '图例色块可见')

  // —— ⑩ 状态字形统一：成功一律 ✓（工具曾用 ◇） ——
  console.log('\n【⑩】span 状态字形统一 ✓')
  const glyphs = await page.evaluate(() => [...document.querySelectorAll('.wf-name .glyph')].map((g) => g.textContent))
  assert(glyphs.length > 0, `有状态字形（${glyphs.length} 个）`)
  assert(!glyphs.includes('◇'), `不再出现 ◇（实得 ${[...new Set(glyphs)].join('')}）`)
  assert(glyphs.every((g) => ['✓', '▶', '✗', '⏸'].includes(g)), '字形只取 ✓ / ▶ / ✗ / ⏸')

  // —— ⑪ trace 弹层：行数多时可滚动，不再只看到 3 条 ——
  // 回归：.tr-pop 绝对定位缺 top → 静态位置被 .tr-rail（flex, align-items:center）居中，
  // 行数 ≥4 时上半截越出 .right-body 顶边（块级 start 溢出不可见且不可滚）。
  console.log('\n【⑪】trace 弹层滚动（长列表可达末行）')
  await page.evaluate(() => { if (!document.querySelector('.tr-pop')) document.querySelector('.tr-pick')?.click() })
  await page.waitForTimeout(500)
  const popGeo = await page.evaluate(() => {
    const pop = document.querySelector('.tr-pop')
    if (!pop) return null
    const body = pop.querySelector('.tr-pop-body')
    if (!body) return null
    // 复制到 12 行（真实会话常见 10+ 条）；只动 DOM，不触发请求
    const proto = body.querySelector('.tr-row')
    while (body.querySelectorAll('.tr-row').length < 12) body.appendChild(proto.cloneNode(true))
    const pr = pop.getBoundingClientRect()
    const rr = document.querySelector('.tr-rail').getBoundingClientRect()
    const br = document.querySelector('.right-body').getBoundingClientRect()
    return {
      rows: body.querySelectorAll('.tr-row').length,
      belowRail: pr.top >= rr.bottom - 1,
      insideView: pr.top >= br.top - 1 && pr.bottom <= br.bottom + 1,
      canScroll: body.scrollHeight > body.clientHeight,
      popH: Math.round(pr.height),
    }
  })
  assert(!!popGeo, '弹层与列表体存在（.tr-pop-body）')
  assert(popGeo.belowRail, '弹层锚定在 trace 选择器下方（不再被垂直居中）')
  assert(popGeo.insideView, '弹层完整落在右栏可视区内（上半截未被裁掉）')
  assert(popGeo.canScroll, `12 条时列表体可滚动（${popGeo.rows} 行，弹层高 ${popGeo.popH}px）`)
  await page.mouse.move(1200, 300)
  await page.mouse.wheel(0, 600)
  await page.waitForTimeout(400)
  const scrolled = await page.evaluate(() => {
    const body = document.querySelector('.tr-pop-body')
    const rows = [...body.querySelectorAll('.tr-row')]
    return {
      top: Math.round(body.scrollTop),
      lastReachable: Math.round(rows[rows.length - 1].getBoundingClientRect().bottom) <= Math.round(body.getBoundingClientRect().bottom) + 1,
    }
  })
  assert(scrolled.top > 0, `滚轮可滚动列表体（scrollTop=${scrolled.top}）`)
  assert(scrolled.lastReachable, '滚动后末条 trace 可见（5 条以上都能选到）')

  console.log(`\n✅ 链路 tab e2e 全通过：${pass} 项断言`)
  await browser.close()
  preview.kill()
  process.exit(0)
} catch (err) {
  console.error('\n❌ 链路 tab e2e 失败:', err.message)
  if (browser) await browser.close().catch(() => {})
  preview.kill()
  process.exit(1)
}
