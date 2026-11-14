#!/usr/bin/env node
// 手机主会话页的浏览器夹具（P8-02）：390×844 / 430×932 两个视口各走一遍。
//
// 验的是「配对后手机上真的能用会话」而不是组件快照：
//   入口条（Hub / 会话）→ 会话列表（工作区 + 会话行）→ 打开会话 →
//   历史可见（Markdown）→ 发消息（ask_batch）→ 流式气泡 → 工具折叠 →
//   审批卡（停靠在 composer 上方）→ 批准/拒绝 → Ask 卡 → 作答 → 返回列表。
// 每一步都量：无横向溢出、主触控目标 ≥44px、composer 贴底、0 pageerror。
//
// 前置：`npm run mobile:build`（产物 dist-mobile/）。本脚本自己起静态服务 + mock bridge，
// 端口用 52xx（不占 5173/5273），跑完关服务；截图写到 /tmp/p8-session/。
//
// 为什么用 page.routeWebSocket 而不是真 WS 服务：会话页的流式/工具/审批/提问都是
// **服务端推帧**，HTTP /api/cmd 只负责命令。这里把 chat WS 抓在手里，按真实事件形状
// 逐帧推给页面（content_chunk / tool_run_* / tool_approval_requested / ask_user_question）。
import { chromium } from 'playwright'
import http from 'node:http'
import { readFile } from 'node:fs/promises'
import { existsSync, mkdirSync } from 'node:fs'
import { extname, join, resolve, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const APP = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const DIST = join(APP, 'dist-mobile')
const SHOTS = '/tmp/p8-session'
const PAIR_CODE = '123456'

const VIEWPORTS = [
  { name: '390', width: 390, height: 844 },
  { name: '430', width: 430, height: 932 },
]

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.woff2': 'font/woff2',
  '.woff': 'font/woff',
  '.png': 'image/png',
  '.svg': 'image/svg+xml',
  '.json': 'application/json',
}

const WS_PATH = '/mock/go-code'
const NOW = Date.now()
const SESSIONS = [
  { id: 's_demo', title: 'P8 会话', updated_at: NOW, pinned: false, running: false },
  { id: 's_old', title: '更早的会话', updated_at: NOW - 3 * 3600_000, pinned: true, running: false },
]
// 历史走 new_session 的 messages 降级路径（与 bridge 无事件日志时同形）：纯文本 + Markdown。
const HISTORY = [
  { role: 'user', text: '帮我看看这个仓库的结构' },
  { role: 'assistant', text: '**先看目录**：\n\n- `src/`\n- `docs/`\n\n我先读 README。' },
]

// 手机 bridge 的 allowlist（desktop/bridge/mobile_server.go）。夹具对**每条**命令都记账：
// 出现集合外的命令 = 界面上有个按钮点了会 403，属于能力收窄没做到位。
const ALLOWED = new Set([
  'list',
  'new_session',
  'ask',
  'ask_batch',
  'interrupt',
  'approve',
  'answer_question',
  'workspace_list',
  'workspace_add',
  'workspace_sync',
  // Hub 列表（P8-01b 的既有入口，同一个手机壳里并存）
  'hub_agent_list',
  'hub_group_list',
  'hub_open',
  'hub_history',
  'git_snapshot',
  'git_history',
])

const seen = { cmds: [], payloads: [] }

function mockCmd(cmd, payload) {
  seen.cmds.push(cmd)
  seen.payloads.push({ cmd, payload })
  switch (cmd) {
    case 'workspace_list':
      return { workspaces: [{ path: WS_PATH, title: '', pinned: false, last_opened: NOW }] }
    case 'list':
      return { sessions: SESSIONS }
    case 'new_session':
      return {
        session_id: String(payload?.id || payload?.session_id || 's_demo'),
        restored: 'messages',
        messages: HISTORY,
        sessions: SESSIONS,
      }
    case 'ask_batch':
      return { session_id: String(payload?.session_id || ''), queued: Array.isArray(payload?.messages) ? payload.messages.length : 0 }
    case 'approve':
      return { id: String(payload?.id || ''), approve: payload?.approve === true }
    case 'answer_question':
      return { batch_id: String(payload?.id || payload?.batch_id || '') }
    case 'interrupt':
      return { session_id: String(payload?.session_id || '') }
    // Hub 列表与手机壳共用（P8-01b 的既有入口）：空清单就够 —— 本夹具只验会话页。
    case 'hub_agent_list':
      return { agents: [], dm: {} }
    case 'hub_group_list':
      return { groups: [], progress: {} }
    case 'hub_open':
    case 'hub_history':
      return { messages: [], has_more: false }
    // 桌面 store 的 setActiveSession 会顺手 refreshGit()；这两条不在手机白名单里，
    // 真 bridge 回 403 —— 夹具照抄，用来证明「403 不会在手机界面上炸出错误」。
    case 'git_snapshot':
    case 'git_history':
      return { __status: 403, error: `command not allowed: ${cmd}` }
    default:
      return { __status: 403, error: `command not allowed: ${cmd}` }
  }
}

function json(res, status, body) {
  const buf = Buffer.from(JSON.stringify(body))
  res.writeHead(status, { 'Content-Type': 'application/json', 'Content-Length': buf.length })
  res.end(buf)
}

async function serveStatic(res, pathname) {
  const rel = pathname === '/' || pathname === '' ? '/mobile.html' : pathname
  const file = join(DIST, rel)
  if (!file.startsWith(DIST) || !existsSync(file)) {
    res.writeHead(404)
    res.end('not found')
    return
  }
  const buf = await readFile(file)
  res.writeHead(200, { 'Content-Type': MIME[extname(file)] || 'application/octet-stream', 'Content-Length': buf.length })
  res.end(buf)
}

async function startServer() {
  const server = http.createServer((req, res) => {
    void (async () => {
      const url = new URL(req.url || '/', 'http://127.0.0.1')
      if (url.pathname === '/api/ping') {
        return json(res, 200, { ok: true, paired: true })
      }
      if (url.pathname === '/api/pair') {
        return json(res, 200, { token: 'tok-p8-session' })
      }
      if (url.pathname === '/api/cmd') {
        const chunks = []
        for await (const c of req) chunks.push(c)
        let body = {}
        try {
          body = JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}')
        } catch {
          return json(res, 400, { error: 'bad json' })
        }
        const cmd = String(body.cmd || '')
        if (!ALLOWED.has(cmd)) {
          seen.cmds.push(cmd)
          return json(res, 403, { error: `command not allowed: ${cmd}` })
        }
        const out = mockCmd(cmd, body.payload || {})
        if (out && out.__status) return json(res, out.__status, { error: out.error })
        return json(res, 200, { ok: true, data: out })
      }
      if (url.pathname === '/api/events') {
        return json(res, 400, { error: 'use websocket' })
      }
      return serveStatic(res, url.pathname)
    })().catch((e) => {
      res.writeHead(500)
      res.end(String(e))
    })
  })
  return new Promise((resolveP) => {
    const tryPort = (port, left) => {
      server.once('error', (err) => {
        if (err.code === 'EADDRINUSE' && left > 0) tryPort(port + 1, left - 1)
        else throw err
      })
      server.listen(port, '127.0.0.1', () => resolveP({ server, port: server.address().port }))
    }
    tryPort(Number(process.env.MOBILE_SESSION_PORT || 5275), 5)
  })
}

function assert(cond, msg) {
  if (!cond) throw new Error(msg)
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function waitFor(fn, timeout = 8000, step = 30) {
  const end = Date.now() + timeout
  while (Date.now() < end) {
    const v = await fn()
    if (v) return v
    await sleep(step)
  }
  throw new Error(`waitFor 超时：${fn.toString().replace(/\s+/g, ' ').slice(0, 160)}`)
}

// 主触控目标：手机壳导航 + 会话列表行 + composer 的发送/停止。
// 正文里的行内链接、Markdown 表格里的按钮不算（WCAG 2.5.8 的 inline 例外）。
const TOUCH_SELECTORS = [
  '[data-testid="mobile-tab-hub"]',
  '[data-testid="mobile-tab-sessions"]',
  '[data-testid="mobile-session-row"]',
  '[data-testid="mobile-session-refresh"]',
  '[data-testid="mobile-session-workspace"]',
  '[data-testid="mobile-session-back"]',
  '[data-testid="mobile-lang"]',
  '[data-testid="mobile-theme"]',
  '[data-testid="mobile-pair-submit"]',
  '.m-session .composer .send-btn',
  '.m-session .appr-dock .btn',
  '.m-session .dock.ask-scope .ask-opt',
  '.m-session .dock.ask-scope .ask-btn',
]

async function measure(page, label, vp, settle = 120) {
  await page.waitForTimeout(settle)
  const m = await page.evaluate((selectors) => {
    const de = document.documentElement
    const visible = (el) => {
      const r = el.getBoundingClientRect()
      if (r.width <= 0 || r.height <= 0) return false
      const cs = getComputedStyle(el)
      return cs.visibility !== 'hidden' && cs.display !== 'none'
    }
    const small = []
    let checked = 0
    for (const sel of selectors) {
      for (const el of document.querySelectorAll(sel)) {
        if (!visible(el)) continue
        const r = el.getBoundingClientRect()
        checked++
        if (r.width < 43.5 || r.height < 43.5) {
          small.push({ sel, cls: String(el.className || el.tagName).slice(0, 50), w: Math.round(r.width), h: Math.round(r.height) })
        }
      }
    }
    const overflow = []
    for (const el of document.querySelectorAll('.m-app *')) {
      if (!visible(el)) continue
      const r = el.getBoundingClientRect()
      if (r.right > window.innerWidth + 1 && r.width < window.innerWidth * 3) {
        overflow.push({ cls: String(el.className || el.tagName).slice(0, 60), right: Math.round(r.right) })
      }
    }
    // composer 贴底：输入框底部要在视口底部附近（安全区以内），且消息区在它上面。
    const comp = document.querySelector('.m-session .composer')
    const nar = document.querySelector('.m-session .narrative')
    const compRect = comp ? comp.getBoundingClientRect() : null
    const narRect = nar ? nar.getBoundingClientRect() : null
    return {
      innerWidth: window.innerWidth,
      innerHeight: window.innerHeight,
      scrollWidth: de.scrollWidth,
      bodyScrollWidth: document.body.scrollWidth,
      checked,
      small: small.slice(0, 8),
      overflow: overflow.slice(0, 6),
      composerBottomGap: compRect ? Math.round(window.innerHeight - compRect.bottom) : null,
      composerAboveNarrative: compRect && narRect ? narRect.bottom <= compRect.top + 2 : null,
    }
  }, TOUCH_SELECTORS)
  assert(m.innerWidth === vp.width, `${label}: innerWidth=${m.innerWidth}，期望 ${vp.width}`)
  assert(m.scrollWidth <= vp.width, `${label}: documentElement.scrollWidth=${m.scrollWidth} > ${vp.width}`)
  assert(m.bodyScrollWidth <= vp.width, `${label}: body.scrollWidth=${m.bodyScrollWidth} > ${vp.width}`)
  assert(m.overflow.length === 0, `${label}: 有元素超出视口右边缘 ${JSON.stringify(m.overflow)}`)
  assert(m.small.length === 0, `${label}: 触控目标 <44px ${JSON.stringify(m.small)}`)
  console.log(`  ✅ ${label}: scrollWidth=${m.scrollWidth}, 触控目标=${m.checked} 个全 ≥44px`)
  return m
}

async function shot(page, vp, name) {
  await page.waitForTimeout(300)
  const file = join(SHOTS, `${vp.name}-${name}.png`)
  await page.screenshot({ path: file })
  return file
}

async function runViewport(browser, baseURL, vp) {
  console.log(`\n── ${vp.name}×${vp.height} ──`)
  const context = await browser.newContext({
    viewport: { width: vp.width, height: vp.height },
    deviceScaleFactor: 2,
    isMobile: true,
    hasTouch: true,
  })
  const page = await context.newPage()
  const errors = []
  page.on('pageerror', (e) => errors.push(`pageerror ${e.message}`))
  page.on('console', (m) => {
    if (m.type() !== 'error') return
    const text = m.text()
    if (/fonts\.(googleapis|gstatic)\.com/.test(text)) return
    if (/Failed to load resource/.test(text) && /fonts/.test(text)) return
    errors.push(text)
  })

  // chat WS 抓在手里：后面按真实事件形状推帧（流式 / 工具 / 审批 / 提问）。
  let chatWs = null
  await page.routeWebSocket(/\/api\/events/, (ws) => {
    const url = ws.url()
    ws.onMessage(() => {})
    if (url.includes('session_id=')) chatWs = ws
  })

  const shots = []
  await page.goto(baseURL, { waitUntil: 'load' })

  // 1) 配对
  await waitFor(() => page.locator('[data-testid="mobile-pair-code"]').count())
  await page.fill('[data-testid="mobile-pair-code"]', PAIR_CODE)
  await page.fill('[data-testid="mobile-pair-name"]', 'P8 会话夹具')
  await page.click('[data-testid="mobile-pair-submit"]')

  // 2) 入口条：Hub / 会话（并列，可切换）
  await waitFor(() => page.locator('[data-testid="mobile-tab-sessions"]').count(), 12000)
  assert((await page.locator('[data-testid="mobile-tab-hub"]').count()) === 1, '入口条缺少 Hub 入口')
  await measure(page, 'Hub 列表（入口条）', vp)
  shots.push(await shot(page, vp, '1-hub'))

  // 3) 会话列表：工作区 + 会话行（数据来自 workspace_list + list）
  await page.click('[data-testid="mobile-tab-sessions"]')
  await waitFor(() => page.locator('[data-testid="mobile-session-list"]').count())
  await waitFor(async () => (await page.locator('[data-testid="mobile-session-row"]').count()) > 0)
  const rows = await page.locator('[data-testid="mobile-session-row"]').allInnerTexts()
  assert(rows.some((t) => t.includes('P8 会话')), `会话列表里没有 P8 会话：${JSON.stringify(rows)}`)
  assert(seen.cmds.includes('workspace_list'), '没有调用 workspace_list（不该依赖桌面 localStorage 项目树）')
  assert(seen.cmds.includes('list'), '没有调用 list')
  await measure(page, '会话列表', vp)
  shots.push(await shot(page, vp, '2-sessions'))

  // 4) 打开会话：历史可见（Markdown 渲染）
  await page.locator('[data-testid="mobile-session-row"]', { hasText: 'P8 会话' }).first().click()
  await waitFor(() => page.locator('[data-testid="mobile-session"]').count())
  await waitFor(async () => (await page.locator('.m-session .narrative').innerText()).includes('先看目录'))
  const historyText = await page.locator('.m-session .narrative').innerText()
  assert(historyText.includes('帮我看看这个仓库的结构'), `历史里没有用户消息：${historyText.slice(0, 120)}`)
  assert((await page.locator('.m-session .narrative strong').count()) >= 1, 'Markdown 没有渲染成 DOM（粗体）')
  assert(seen.cmds.includes('new_session'), '打开会话没有走 new_session（没接桌面恢复路径）')
  // composer 在（桌面同一份 Composer），贴底
  assert((await page.locator('.m-session .composer textarea').count()) === 1, 'composer 不在')
  const m1 = await measure(page, '会话页（历史）', vp)
  assert(m1.composerAboveNarrative === true, `composer 没有贴在叙述区下方：${JSON.stringify(m1)}`)
  assert(m1.composerBottomGap !== null && m1.composerBottomGap <= 24, `composer 没贴底：底部留白 ${m1.composerBottomGap}px`)
  shots.push(await shot(page, vp, '3-session'))

  // 5) 发消息：ask_batch（桌面 Composer 的暂存队列整队推送）→ 服务端消费 → 用户气泡
  //    桌面语义：点发送先入本地暂存队列（composer 里的「待发送」），服务端 user_inputs_consumed
  //    回执到达才移进对话页 —— 手机复用同一份 Composer，行为必须一致。
  const draft = '手机端发一条'
  await page.fill('.m-session .composer textarea', draft)
  await page.click('.m-session .composer .send-btn')
  await waitFor(async () => (await page.locator('.m-session .composer').innerText()).includes(draft))
  const batch = seen.payloads.filter((p) => p.cmd === 'ask_batch').pop()
  assert(batch, 'ask_batch 没有被调用（手机发消息必须真的可用）')
  assert(JSON.stringify(batch.payload).includes(draft), `ask_batch 载荷里没有正文：${JSON.stringify(batch.payload).slice(0, 160)}`)
  assert((await page.locator('.m-session .composer .pq-item').count()) >= 1, '暂存队列里没有待发送消息')
  assert(chatWs, '没有建立 chat WS（connectChat 没被调用）')
  const push = (ev) => chatWs.send(JSON.stringify(ev))
  push({
    event_type: 'user_inputs_consumed',
    session_id: 's_demo',
    user_texts: [draft],
    user_contents: [[{ type: 'text', content: draft }]],
  })
  await waitFor(async () => (await page.locator('.m-session .narrative').innerText()).includes(draft))
  console.log('  ✅ 发消息：待发送入队 + ask_batch 带上正文 + 消费回执后气泡进对话页')

  // 6) 流式：agent_start → content_chunk → 停止按钮
  push({ event_type: 'agent_start', session_id: 's_demo', run_id: 'r1', content: 'run' })
  push({ event_type: 'content_chunk', session_id: 's_demo', run_id: 'r1', content: '正在读取 ' })
  push({ event_type: 'content_chunk', session_id: 's_demo', run_id: 'r1', content: 'README…' })
  await waitFor(async () => (await page.locator('.m-session .narrative').innerText()).includes('正在读取 README'))
  assert((await page.locator('.m-session .composer .send-btn.is-stop').count()) === 1, '流式中发送钮没变成「停止」')
  console.log('  ✅ 流式气泡出现 + 发送钮变停止')
  await measure(page, '会话页（流式）', vp)
  shots.push(await shot(page, vp, '4-stream'))

  // 7) 工具折叠：运行中工具行在 Working 盒子里（可直接展开）；交卷后整轮收成一行
  //    「Worked for …」（.rt-act），展开它又能看到同一批工具行 —— 两级折叠都要能点开。
  push({ event_type: 'tool_run_start', session_id: 's_demo', id: 'call_1', run_id: 'r1', name: 'read_file', arguments: '{"path":"README.md"}' })
  push({ event_type: 'tool_run_end', session_id: 's_demo', id: 'call_1', run_id: 'r1', ok: true, result: '# go-code\n\n项目说明' })
  await waitFor(() => page.locator('.m-session .tool-row').count())
  const row = page.locator('.m-session .tool-row').first()
  assert((await row.locator('.tool-row-hd').getAttribute('aria-expanded')) === 'false', '工具行默认应是收起的')
  // 点行的左侧（折叠箭头那一列）：行中间的参数摘要对文件工具是「打开预览」的链接，
  // 它自己 stopPropagation —— 点那里不会折叠行（桌面同一条规则）。
  await row.locator('.tool-row-hd').click({ position: { x: 10, y: 12 } })
  await waitFor(async () => (await row.locator('.tool-row-hd').getAttribute('aria-expanded')) === 'true')
  await waitFor(() => row.locator('.tool-row-body').count())
  console.log('  ✅ 工具行：运行中默认折叠 → 点开显示参数/结果')
  await measure(page, '会话页（工具折叠）', vp)
  shots.push(await shot(page, vp, '5-tool'))

  // 交卷：发送钮回到「发送」态，工作过程收成一行
  push({ event_type: 'agent_end', session_id: 's_demo', run_id: 'r1' })
  await waitFor(async () => (await page.locator('.m-session .composer .send-btn.is-stop').count()) === 0)
  await waitFor(() => page.locator('.m-session .rt-act').count())
  const act = page.locator('.m-session .rt-act').last()
  assert((await act.getAttribute('aria-expanded')) === 'false', '工作过程交卷后应收起')
  await act.click()
  await waitFor(async () => (await act.getAttribute('aria-expanded')) === 'true')
  await waitFor(() => page.locator('.m-session .tool-row').count())
  console.log('  ✅ 交卷后工作过程收成一行「Worked for …」→ 展开又能看到工具行')

  // 8) 审批卡：停靠在 composer 上方（非全屏遮罩），能批准/拒绝
  push({
    event_type: 'tool_approval_requested',
    session_id: 's_demo',
    id: 'ap_1',
    run_id: 'r1',
    name: 'write_file',
    arguments: '{"path":"reports/pricing.md"}',
    index: 0,
  })
  await waitFor(() => page.locator('.m-session .appr-dock').count())
  const dock = await page.evaluate(() => {
    const d = document.querySelector('.m-session .appr-dock')
    const c = document.querySelector('.m-session .composer')
    if (!d || !c) return null
    const dr = d.getBoundingClientRect()
    const cr = c.getBoundingClientRect()
    const cs = getComputedStyle(d)
    return {
      aboveComposer: dr.bottom <= cr.top + 2,
      fullScreen: dr.width >= window.innerWidth - 2 && dr.height >= window.innerHeight - 2,
      position: cs.position,
      bg: cs.backgroundColor,
    }
  })
  assert(dock, '审批卡不在 DOM 里')
  assert(dock.aboveComposer, `审批卡没有停靠在 composer 上方：${JSON.stringify(dock)}`)
  assert(!dock.fullScreen, `审批卡是全屏遮罩：${JSON.stringify(dock)}`)
  console.log('  ✅ 审批卡停靠在 composer 上方（非全屏）')
  await measure(page, '会话页（审批）', vp)
  shots.push(await shot(page, vp, '6-approval'))
  // 拒绝（第一个 .btn 是「拒绝」）
  await page.locator('.m-session .appr-dock .btn').first().click()
  await waitFor(async () => (await page.locator('.m-session .appr-dock').count()) === 0)
  const appr = seen.payloads.filter((p) => p.cmd === 'approve').pop()
  assert(appr, 'approve 没有被调用')
  assert(appr.payload.approve === false, `拒绝没有传 approve=false：${JSON.stringify(appr.payload)}`)
  console.log('  ✅ 审批：点拒绝 → approve(approve=false) 且卡片收起')

  // 9) Ask 卡：可见、能作答提交
  push({
    event_type: 'ask_user_question',
    session_id: 's_demo',
    id: 'q_1',
    run_id: 'r1',
    timeout_ms: 0,
    questions: [{ id: 'q1', question: '用哪个分支？', options: ['main', 'feature'], multi: false }],
  })
  await waitFor(() => page.locator('.m-session .dock.ask-scope').count())
  await waitFor(async () => (await page.locator('.m-session .dock.ask-scope').innerText()).includes('用哪个分支'))
  await measure(page, '会话页（提问）', vp)
  shots.push(await shot(page, vp, '7-ask'))
  await page.locator('.m-session .dock.ask-scope .ask-opt', { hasText: 'main' }).first().click()
  const submit = page.locator('.m-session .dock.ask-scope .ask-btn').first()
  await waitFor(async () => !(await submit.isDisabled()))
  await submit.click()
  await waitFor(async () => (await page.locator('.m-session .dock.ask-scope').count()) === 0)
  const ans = seen.payloads.filter((p) => p.cmd === 'answer_question').pop()
  assert(ans, 'answer_question 没有被调用')
  assert(JSON.stringify(ans.payload).includes('main'), `回答里没有选中项：${JSON.stringify(ans.payload)}`)
  console.log('  ✅ 提问：选 main → answer_question 带上答案且卡片收起')

  // 10) 返回列表：会话返回回**会话列表**（不是 Hub 列表）
  await page.click('[data-testid="mobile-session-back"]')
  await waitFor(() => page.locator('[data-testid="mobile-session-list"]').count())
  assert((await page.locator('[data-testid="mobile-hub-list"]').count()) === 0, '会话返回落到了 Hub 列表（返回栈串了）')
  await measure(page, '返回会话列表', vp)
  shots.push(await shot(page, vp, '8-back'))

  // 11) 能力收窄：桌面专属入口不渲染（点了会 403 的按钮一个都不该在）
  const dead = await page.evaluate(() => ({
    modelChip: document.querySelectorAll('.m-session .model-chip').length,
    modeChip: document.querySelectorAll('.m-session .mode-chip').length,
    effortChip: document.querySelectorAll('.m-session .effort-chip').length,
    ctx: document.querySelectorAll('.m-session .ctx').length,
    attach: document.querySelectorAll('.m-session .attach-btn').length,
  }))
  assert(dead.modelChip === 0 && dead.modeChip === 0 && dead.effortChip === 0 && dead.ctx === 0 && dead.attach === 0,
    `手机上仍渲染了桌面专属入口：${JSON.stringify(dead)}`)
  console.log('  ✅ 能力收窄：模型/模式/Effort/上下文/附件入口都不渲染')

  // 12) 命令集合：只出现 allowlist 内的命令（集合外的会被 mock 403，界面上就是死按钮）
  const outside = [...new Set(seen.cmds)].filter((c) => !ALLOWED.has(c))
  assert(outside.length === 0, `出现了 allowlist 外的命令（界面有会 403 的入口）：${JSON.stringify(outside)}`)
  for (const must of ['workspace_list', 'list', 'new_session', 'ask_batch', 'approve', 'answer_question']) {
    assert(seen.cmds.includes(must), `缺少关键命令 ${must}（seen=${JSON.stringify([...new Set(seen.cmds)])}）`)
  }
  console.log(`  ✅ 命令集合：${JSON.stringify([...new Set(seen.cmds)])} 全在 allowlist 内`)

  assert(errors.length === 0, `${vp.name}: 有 pageerror/console error：${JSON.stringify(errors.slice(0, 5))}`)
  console.log('  ✅ 0 pageerror / 0 console error')
  await context.close()
  return shots
}

async function main() {
  if (!existsSync(join(DIST, 'mobile.html'))) {
    throw new Error(`没有 ${DIST}/mobile.html —— 先跑 npm run mobile:build`)
  }
  mkdirSync(SHOTS, { recursive: true })
  const { server, port } = await startServer()
  const baseURL = `http://127.0.0.1:${port}/`
  console.log(`静态服务：${baseURL}（dist-mobile）`)
  const browser = await chromium.launch()
  const all = []
  try {
    for (const vp of VIEWPORTS) all.push(...await runViewport(browser, baseURL, vp))
  } finally {
    await browser.close()
    await new Promise((r) => server.close(r))
  }
  console.log(`\n截图（${all.length} 张）：`)
  for (const f of all) console.log(`  · ${f}`)
  console.log('\nMOBILE_SESSION_SCREENS_PASS')
}

main().catch((e) => {
  console.error(`\n❌ ${e.message}`)
  process.exit(1)
})
