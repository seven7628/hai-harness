#!/usr/bin/env node
// 手机 Hub 屏幕的浏览器夹具（P8-01b）：390×844 / 430×932 两个视口各走一遍。
//
// 验的是「配对成功后手机真的能用到 Hub」，不是组件快照：
//   配对 → 列表（智能体/群组 tab、未读、搜索）→ DM（任务卡/发消息）→ 返回 →
//   群（GroupView）→ 详情抽屉（Context/任务/Needs）→ 返回列表。
// 每一步都量：无横向溢出、主触控目标 ≥44px、0 pageerror。
//
// 前置：`npm run mobile:build`（产物 dist-mobile/）。本脚本自己起静态服务 + mock bridge，
// 端口用 52xx（不占 5173/5180），跑完关服务；截图写到 /tmp/p8-hub-screens/。
//
// 为什么用 page.routeWebSocket 而不是真 WS 服务：手机 transport 只需要「hub 事件流是
// 通的」；本夹具的断言全部来自 HTTP /api/cmd 的应答（发送/列表/历史），不需要服务端推帧。
import { chromium } from 'playwright'
import http from 'node:http'
import { readFile } from 'node:fs/promises'
import { existsSync, statSync, mkdirSync } from 'node:fs'
import { extname, join, resolve, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { auditContrast, installFakeVisualViewport, installTheme } from './mobile-fixture-helpers.mjs'

const APP = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const DIST = join(APP, 'dist-mobile')
const SHOTS = '/tmp/p8-polish'
const TOKEN = 'tok-p8-hub-screens'
const PAIR_CODE = '123456'

const VIEWPORTS = [
  { name: '390', width: 390, height: 844 },
  { name: '430', width: 430, height: 932 },
  { name: '640', width: 640, height: 900 },
]
// P8-03a：手机固定 data-skin='runtime'，主题 ink/paper 与桌面同 key（hai.theme）。
const THEMES = ['ink', 'paper']

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

const NOW = new Date().toISOString()
const AGENTS = [
  {
    id: 'ag_alice', name: 'Alice', role: '研究', version: 2, provider: 'mock', model: 'mock-large',
    effort: 'medium', description: '负责资料研究与对比。', compress_threshold: 0.8,
    policy: { daily_budget_usd: 0 },
  },
  {
    id: 'ag_bob', name: 'Bob', role: '数据', version: 1, provider: 'mock', model: 'mock-small',
    effort: 'low', description: '负责数据清洗与图表。', compress_threshold: 0.8,
    policy: { daily_budget_usd: 0 },
  },
]
const GROUP = {
  id: 'grp_1', name: '产品评审群',
  members: [{ agent_id: 'ag_alice', role: 'owner' }, { agent_id: 'ag_bob', role: 'member' }],
  owner: { agent_id: 'ag_alice' },
  policy: { mode: 'ask', daily_budget_usd: 0 },
  created_at: NOW,
}
const DM_MESSAGES = [
  {
    id: 'dm_1', seq: 1, kind: 'text', sender: { kind: 'agent', agent_id: 'ag_alice' },
    text: '**已收到**，我先看一遍资料。', created_at: NOW,
  },
]
// 任务卡（TaskResultCard）只在**群**时间线里渲染：DM 的 HubBubble 走 AgentMessage 气泡，
// 不解析 task_result（见 HubSurface 的 HubBubble）。所以任务卡断言放在群会话上。
const GROUP_MESSAGES = [
  {
    id: 'g_1', seq: 1, group_id: 'grp_1', kind: 'text', sender: { kind: 'agent', agent_id: 'ag_bob' },
    text: '@Alice 这版数据我看过了，结论在 reports/pricing.md。', created_at: NOW,
  },
  {
    id: 'g_2', seq: 2, group_id: 'grp_1', kind: 'task_result', sender: { kind: 'agent', agent_id: 'ag_bob' },
    text: '整理了三份竞品资料', created_at: NOW,
    refs: {
      status: 'completed', summary: '整理了三份竞品资料', title: '资料整理',
      agent_id: 'ag_bob', origin_kind: 'group', origin_agent_id: 'ag_alice', origin_agent_name: 'Alice',
      task_id: 'task_1',
    },
  },
]
const TASKS = [
  {
    id: 'task_1', status: 'running', title: '整理竞品资料', instruction: '把三份竞品资料整理成对比表',
    origin: { kind: 'group', agent_id: 'ag_alice', agent_name: 'Alice', group_id: 'grp_1' },
    target: { kind: 'agent', agent_id: 'ag_bob' }, created_at: NOW, updated_at: NOW,
  },
]
const NEEDS = [
  {
    id: 'need_1', kind: 'approval', status: 'pending', agent_id: 'ag_alice',
    group_id: 'grp_1', origin_agent_id: 'ag_alice', origin_agent_name: 'Alice', origin_kind: 'group',
    target_kind: 'workspace', target_workspace: '/mock/workspace', target_session_id: 's_demo',
    run_name: 'Alice-sub', depth: 1, task_id: 'task_1', session_id: 's_demo', anchor: 'call_1',
    title: '写文件前确认：把对比表落到 reports/pricing.md',
    body: '工具 write_file 需要一次审批：reports/pricing.md（新建，约 1.2KB）',
    created_at: NOW,
  },
]

const seen = { cmds: [], sends: [] }

function mockCmd(cmd, payload) {
  switch (cmd) {
    case 'hub_agent_list':
      return {
        agents: AGENTS,
        dm: {
          ag_alice: { unread: 2, last_text: '整理了三份竞品资料', last_at: NOW },
          ag_bob: { unread: 0, last_text: '数据看板已更新', last_at: NOW },
        },
      }
    case 'hub_group_list':
      return { groups: [GROUP], progress: { grp_1: { unread: 3, last_text: '这版数据我看过了', last_at: NOW } } }
    case 'hub_open':
      return {
        target: { kind: 'dm', agent_id: 'ag_alice' },
        agent: AGENTS[0],
        session_id: 'hub.ag_alice.dm',
        messages: DM_MESSAGES,
        has_more: false,
        cursor: { human_read_seq: 2 },
        exists: true,
      }
    case 'hub_history':
      return { messages: DM_MESSAGES, has_more: false }
    case 'hub_group_get':
      return { group: GROUP, conversation: { status: 'idle', turns_used: 0, turn_budget: -1 } }
    case 'hub_group_history':
      return { messages: GROUP_MESSAGES, has_more: false }
    case 'hub_group_activity':
      return { activity: [] }
    case 'hub_task_list':
      return { tasks: TASKS }
    case 'hub_need_list':
      return { needs: NEEDS }
    case 'hub_search':
      return {
        query: String(payload?.query ?? ''), scope: 'all', limit: 50, scanned: 1,
        hits: [{
          scope: 'group', group_id: 'grp_1', name: '产品评审群', seq: 1, message_id: 'g_1',
          snippet: '…结论在 reports/pricing.md…', sender: 'Bob', sender_kind: 'agent',
          kind: 'text', created_at: NOW, owner_only: false,
        }],
        count: 1, has_more: false, next_before: null,
      }
    case 'hub_send':
      return { message_id: 'srv_dm_1', seq: 3 }
    case 'hub_group_send':
      return { message_id: 'srv_g_1', seq: 2 }
    case 'hub_mark_read':
      return {}
    default:
      return {}
  }
}

function json(res, code, body) {
  res.writeHead(code, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' })
  res.end(JSON.stringify(body))
}

// 根路径：mobile:embed 会把 mobile.html 改名 index.html（bridge 的 /m/ 就是这么兜底的），
// 只跑过 mobile:build 时还是 mobile.html —— 两种产物都认。
const INDEX = existsSync(join(DIST, 'index.html')) ? '/index.html' : '/mobile.html'

async function serveStatic(res, pathname) {
  const rel = pathname === '/' ? INDEX : pathname
  const full = join(DIST, rel)
  if (!full.startsWith(DIST)) {
    res.writeHead(403)
    res.end('forbidden')
    return
  }
  try {
    if (!statSync(full).isFile()) throw new Error('not file')
    const buf = await readFile(full)
    res.writeHead(200, {
      'Content-Type': MIME[extname(full)] || 'application/octet-stream',
      'Cache-Control': 'no-store',
    })
    res.end(buf)
  } catch {
    res.writeHead(404)
    res.end('not found')
  }
}

function startServer() {
  const server = http.createServer(async (req, res) => {
    const url = new URL(req.url, 'http://localhost')
    const auth = String(req.headers.authorization || '')
    if (url.pathname === '/api/ping') {
      return json(res, 200, { app: 'hai', version: 'p8', paired: auth === `Bearer ${TOKEN}` })
    }
    if (url.pathname === '/api/pair') {
      if (req.method !== 'POST') return json(res, 405, { error: 'method' })
      let raw = ''
      for await (const chunk of req) raw += chunk
      let body = {}
      try {
        body = JSON.parse(raw || '{}')
      } catch {
        return json(res, 400, { error: 'bad json' })
      }
      if (String(body.code || '') !== PAIR_CODE) return json(res, 403, { error: '配对码不正确' })
      return json(res, 200, { token: TOKEN })
    }
    if (url.pathname === '/api/cmd') {
      if (req.method !== 'POST') return json(res, 405, { error: 'method' })
      if (auth !== `Bearer ${TOKEN}`) return json(res, 401, { error: 'unauthorized' })
      let raw = ''
      for await (const chunk of req) raw += chunk
      let body = {}
      try {
        body = JSON.parse(raw || '{}')
      } catch {
        return json(res, 400, { error: 'bad json' })
      }
      const cmd = String(body.cmd || '')
      seen.cmds.push(cmd)
      if (cmd === 'hub_send' || cmd === 'hub_group_send') seen.sends.push({ cmd, payload: body.payload || {} })
      try {
        return json(res, 200, { ok: true, data: mockCmd(cmd, body.payload || {}) })
      } catch (e) {
        return json(res, 500, { ok: false, error: e.message })
      }
    }
    if (url.pathname === '/api/events') {
      // WS 走 page.routeWebSocket（见 runViewport），HTTP 打到这里说明客户端没走 WS。
      return json(res, 400, { error: 'use websocket' })
    }
    return serveStatic(res, url.pathname)
  })
  return new Promise((resolveP) => {
    const tryPort = (port, left) => {
      server.once('error', (err) => {
        if (err.code === 'EADDRINUSE' && left > 0) tryPort(port + 1, left - 1)
        else throw err
      })
      server.listen(port, '127.0.0.1', () => resolveP({ server, port: server.address().port }))
    }
    tryPort(Number(process.env.MOBILE_HUB_PORT || 5274), 5)
  })
}

function assert(cond, msg) {
  if (!cond) throw new Error(msg)
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function waitFor(fn, timeout = 6000, step = 30) {
  const end = Date.now() + timeout
  while (Date.now() < end) {
    const v = await fn()
    if (v) return v
    await sleep(step)
  }
  throw new Error(`waitFor 超时：${fn.toString().replace(/\s+/g, ' ').slice(0, 160)}`)
}

// 主触控目标（WCAG 2.5.8 的例外是「句子里的行内链接」，这里只量真正的按钮/行）：
// 手机壳的导航、列表行、tab、搜索框、composer、抽屉页签与关闭。
const TOUCH_SELECTORS = [
  '[data-testid="mobile-back"]',
  // P8-03a：外层「⋯」撤掉，抽屉入口 = 共享 chat-header 的「上下文」按钮；
  // 头里两个 icon-btn（上下文/更多）都必须是 44px。
  '.m-conv .chat-header .icon-btn',
  '[data-testid="mobile-lang"]',
  '[data-testid="mobile-theme"]',
  '[data-testid="mobile-pair-submit"]',
  '[data-testid="mobile-reconnect"]',
  '[data-testid="mobile-repair"]',
  '.hub-side-tabs button',
  '.hub-side-scopes button',
  '.hub-side-row',
  '.hub-side-search-box',
  '.hub-demo .composer .icon-btn',
  '.hub-demo .composer .send-btn',
  '.composer-mode-switch button',
  '.hub-conv-bar button',
  '.event-card--need .hub-btn',
  '.m-drawer .hub-inspector-close',
  '.m-drawer .hub-inspector-tabs button[role="tab"]',
]

async function measure(page, label, vp, settle = 100) {
  // settle：抽屉有 180ms 的滑入动画，动画期间的 getBoundingClientRect 会把元素算到
  // 视口外（测量必须等它停稳，否则「横向溢出」是假的）。
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
          small.push({ sel, cls: el.className || el.tagName, w: Math.round(r.width), h: Math.round(r.height) })
        }
      }
    }
    // 越界：可见元素右边缘超出视口（横向溢出的直接证据，比 scrollWidth 更早定位）。
    const overflow = []
    for (const el of document.querySelectorAll('.m-app *')) {
      if (!visible(el)) continue
      const r = el.getBoundingClientRect()
      if (r.right > window.innerWidth + 1 && r.width < window.innerWidth * 3) {
        overflow.push({ cls: String(el.className || el.tagName).slice(0, 60), right: Math.round(r.right) })
      }
    }
    return {
      innerWidth: window.innerWidth,
      scrollWidth: de.scrollWidth,
      bodyScrollWidth: document.body.scrollWidth,
      checked,
      small: small.slice(0, 8),
      overflow: overflow.slice(0, 6),
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

async function shot(page, vp, name, theme) {
  // 等动画停稳再截：抽屉滑入 180ms，动画中途截图会拍出「抽屉只露一半」的假象。
  await page.waitForTimeout(300)
  const file = join(SHOTS, `${vp.name}-${theme}-${name}.png`)
  await page.screenshot({ path: file })
  return file
}

async function runViewport(browser, baseURL, vp, theme) {
  console.log(`\n── ${vp.name}×${vp.height} · ${theme} ──`)
  const context = await browser.newContext({
    viewport: { width: vp.width, height: vp.height },
    deviceScaleFactor: 2,
    isMobile: true,
    hasTouch: true,
  })
  await installTheme(context, theme)
  await installFakeVisualViewport(context)
  const page = await context.newPage()
  const errors = []
  page.on('pageerror', (e) => errors.push(`pageerror ${e.message}`))
  page.on('console', (m) => {
    if (m.type() !== 'error') return
    const text = m.text()
    // 夹具从 127.0.0.1 加载，Google Fonts 被墙/被拦是环境噪声（字体不参与任何断言）。
    if (/fonts\.(googleapis|gstatic)\.com/.test(text)) return
    if (/Failed to load resource/.test(text) && /fonts/.test(text)) return
    errors.push(text)
  })
  // hub 事件流：mock 成「连上就保持」。手机 transport 只要求它 open（状态条用）。
  await page.routeWebSocket(/\/api\/events/, (ws) => {
    // 不 connectToServer：这是被 mock 的连接，页面侧保持 open。
    ws.onMessage(() => {})
  })

  const shots = []
  await page.goto(baseURL, { waitUntil: 'load' })

  // 主题 boot：data-skin 固定 runtime，data-theme 由 hai.theme 映射成 ink/paper；
  // theme-color 由 mobile/chrome.ts 的 MutationObserver 跟着写（状态栏/地址栏）。
  const boot = await page.evaluate(() => ({
    theme: document.documentElement.dataset.theme,
    skin: document.documentElement.dataset.skin,
    meta: document.querySelector('meta[name="theme-color"]')?.getAttribute('content') ?? '',
  }))
  assert(boot.skin === 'runtime', `data-skin 不是 runtime：${boot.skin}`)
  assert(boot.theme === theme, `data-theme=${boot.theme}，期望 ${theme}（hai.theme 没生效？）`)
  assert(boot.meta === (theme === 'paper' ? '#f4f3f1' : '#08080a'), `theme-color 没跟随主题：${boot.meta}`)
  console.log(`  ✅ 主题：data-skin=runtime, data-theme=${theme}, theme-color=${boot.meta}`)

  // 1) 配对屏 → 配对
  await waitFor(() => page.locator('[data-testid="mobile-pair-code"]').count())
  await measure(page, '配对屏', vp)
  shots.push(await shot(page, vp, '1-pair', theme))
  await page.fill('[data-testid="mobile-pair-code"]', PAIR_CODE)
  await page.fill('[data-testid="mobile-pair-name"]', 'P8 夹具')
  await page.click('[data-testid="mobile-pair-submit"]')

  // 2) 列表：智能体/群组 tab + 未读 + 预览
  await waitFor(() => page.locator('[data-testid="mobile-hub-list"]').count(), 10000)
  await waitFor(async () => (await page.locator('.hub-side-row').count()) > 0)
  const tabs = await page.locator('.hub-side-tabs button').allInnerTexts()
  assert(tabs.some((t) => t.includes('智能体')), `列表缺少「智能体」tab：${JSON.stringify(tabs)}`)
  assert(tabs.some((t) => t.includes('群组')), `列表缺少「群组」tab：${JSON.stringify(tabs)}`)
  assert((await page.locator('.hub-side-row').count()) >= 2, '列表里智能体行不足')
  assert((await page.locator('.hub-unread').count()) >= 1, '列表没有未读角标')
  const listText = await page.locator('.hub-side-list').innerText()
  assert(listText.includes('Alice'), `列表里没有 Alice：${listText.slice(0, 80)}`)
  await measure(page, '列表', vp)
  shots.push(await shot(page, vp, '2-list', theme))

  // 640（平板竖屏）：单列内容最宽 640 居中，390/430 等于铺满；三档都不横向溢出。
  const listW = await page.evaluate(() => {
    const r = document.querySelector('.hub-side').getBoundingClientRect()
    return { w: Math.round(r.width), left: Math.round(r.left), right: Math.round(r.right) }
  })
  assert(listW.w <= 640, `列表内容宽 ${listW.w} > 640（单列被拉宽）`)
  if (vp.width > 640) {
    assert(Math.abs(listW.left - (vp.width - listW.right)) <= 1, `列表没有居中：${JSON.stringify(listW)}`)
  }
  await auditContrast(page, [
    ['.hub-side-row .hub-name', '列表名字'],
    ['.hub-side-row .hub-preview', '列表预览'],
    ['.hub-side-tabs button.on', '列表 tab（选中）'],
    ['.hub-side-tabs button:not(.on)', '列表 tab（未选中）'],
  ], `${vp.name}/${theme} 列表`)

  // 3) DM：HubSurface（DM 时间线/composer 都是桌面同一份）+ 发消息（乐观气泡）
  await page.locator('.hub-side-row', { hasText: 'Alice' }).first().click()
  await waitFor(() => page.locator('[data-testid="mobile-conversation"]').count())
  await waitFor(() => page.locator('.hub-dm').count())
  await waitFor(async () => (await page.locator('.hub-dm .messages').innerText()).includes('我先看一遍资料'))
  assert((await page.locator('[data-testid="mobile-conv-title"]').innerText()).includes('Alice'), '会话标题不是当前智能体')
  await measure(page, 'DM 会话', vp)
  shots.push(await shot(page, vp, '3-dm', theme))

  // 单头（P8-03a）：共享 chat-header 是唯一的头；外层不再有「⋯」，标题只画一次。
  const header = await page.evaluate(() => {
    const visible = (el) => {
      const r = el.getBoundingClientRect()
      return r.width > 0 && r.height > 0 && getComputedStyle(el).display !== 'none'
    }
    const more = [...document.querySelectorAll('[aria-label="更多操作"]')].filter(visible)
    const outerTitle = document.querySelector('[data-testid="mobile-conv-title"]')
    const ot = outerTitle?.getBoundingClientRect()
    return {
      more: more.length,
      outerInspector: document.querySelectorAll('[data-testid="mobile-inspector-open"]').length,
      outerTitleW: ot ? Math.round(ot.width) : null,
      innerTitle: document.querySelector('.hub-dm .chat-title .name')?.innerText.trim() ?? '',
      chatHeaderTop: Math.round(document.querySelector('.hub-dm .chat-header').getBoundingClientRect().top),
      mainW: Math.round(document.querySelector('.hub-dm').getBoundingClientRect().width),
    }
  })
  assert(header.more === 1, `会话页有 ${header.more} 个「更多操作」入口（应只有 chat-header 一个）`)
  assert(header.outerInspector === 0, '外层「⋯」抽屉入口还在（与 chat-header 的更多菜单重复）')
  assert(header.outerTitleW !== null && header.outerTitleW <= 1, `外层标题没有视觉隐藏：w=${header.outerTitleW}`)
  assert(header.innerTitle === 'Alice', `内层标题不是 Alice：${header.innerTitle}`)
  assert(header.chatHeaderTop === 0, `chat-header 没有贴顶（返回键浮层对不齐）：top=${header.chatHeaderTop}`)
  assert(header.mainW <= 640, `会话面宽 ${header.mainW} > 640`)
  console.log('  ✅ 单头：chat-header 贴顶，外层无重复「⋯」，标题只画一次')

  // composer 高度（P8-03a）：100px → ~71px；输入行 28px；空 meta 行不再占位。
  const comp = await page.evaluate(() => {
    const r = (s) => {
      const e = document.querySelector(s)
      const b = e.getBoundingClientRect()
      return { h: Math.round(b.height), bottom: Math.round(b.bottom) }
    }
    return {
      wrap: r('.hub-dm .composer-wrap'),
      composer: r('.hub-dm .composer'),
      ta: r('.hub-dm .composer textarea'),
      meta: r('.hub-dm .composer-meta'),
    }
  })
  assert(comp.wrap.h <= 90, `DM composer 整块 ${comp.wrap.h}px 偏高（应 ≤90）`)
  assert(comp.composer.h >= 44, `DM composer ${comp.composer.h}px 太小`)
  assert(comp.ta.h >= 24 && comp.ta.h <= 96, `DM 输入行 ${comp.ta.h}px 不在 24–96 内`)
  assert(comp.meta.h === 0, `DM 空 meta 行仍占 ${comp.meta.h}px`)
  console.log(`  ✅ composer：整块 ${comp.wrap.h}px，输入行 ${comp.ta.h}px，空 meta 行 0px`)

  await auditContrast(page, [
    ['.hub-dm .agent-text', 'DM 正文'],
    ['.hub-dm .chat-title .name', 'DM 标题'],
    ['.hub-dm .composer textarea', 'composer 输入'],
  ], `${vp.name}/${theme} DM`)

  // 软键盘（visualViewport）：layout viewport 不变、可见区缩到 400 —— composer 仍在上沿之上。
  const beforeVv = await page.evaluate(() => Math.round(document.querySelector('.m-app').getBoundingClientRect().height))
  await page.evaluate(() => {
    window.visualViewport.height = 400
    window.visualViewport.dispatchEvent(new Event('resize'))
  })
  await page.waitForTimeout(150)
  const kb = await page.evaluate(() => {
    const app = document.querySelector('.m-app').getBoundingClientRect()
    const c = document.querySelector('.hub-dm .composer').getBoundingClientRect()
    return {
      vvH: window.visualViewport.height,
      innerH: window.innerHeight,
      appH: Math.round(app.height),
      compBottom: Math.round(c.bottom),
      varH: getComputedStyle(document.documentElement).getPropertyValue('--m-vv-height').trim(),
    }
  })
  assert(kb.innerH === vp.height, `键盘模拟前 innerHeight=${kb.innerH}，期望 ${vp.height}（layout viewport 不该变）`)
  assert(kb.vvH === 400, `假 visualViewport 高度没生效：${kb.vvH}`)
  assert(kb.varH === '400px', `--m-vv-height=${kb.varH}，期望 400px`)
  assert(kb.appH <= 401, `键盘弹出后 .m-app 高 ${kb.appH}，没有跟着可见区缩`)
  assert(kb.compBottom <= 401, `键盘弹出后 composer 底 ${kb.compBottom} 超出可见区 400`)
  shots.push(await shot(page, vp, '9-keyboard', theme))
  console.log(`  ✅ 软键盘：可见区 400（innerHeight 仍 ${kb.innerH}），app=${kb.appH}，composer 底=${kb.compBottom}`)
  await page.evaluate(() => {
    window.visualViewport.height = 0
    window.visualViewport.dispatchEvent(new Event('resize'))
  })
  await page.waitForTimeout(150)
  const restored = await page.evaluate(() => Math.round(document.querySelector('.m-app').getBoundingClientRect().height))
  assert(Math.abs(restored - beforeVv) <= 1, `键盘收起后 .m-app 没恢复：${restored} vs ${beforeVv}`)

  // safe-area：home 条由 .m-app 统一让位。夹具覆写 --m-safe-bottom=34px 量真值
  // （桌面 Chromium 的 env() 恒为 0，不覆写就永远测不到）。
  await page.addStyleTag({ content: ':root{--m-safe-bottom:34px !important}' })
  await page.waitForTimeout(120)
  const safe = await page.evaluate(() => {
    const c = document.querySelector('.hub-dm .composer').getBoundingClientRect()
    return {
      gap: Math.round(window.innerHeight - c.bottom),
      appPad: getComputedStyle(document.querySelector('.m-app')).paddingBottom,
    }
  })
  assert(safe.gap >= 33, `--m-safe-bottom=34px 时 composer 离底 ${safe.gap}px（应 ≥33，说明安全区没生效）`)
  console.log(`  ✅ safe-area：--m-safe-bottom=34px → composer 离底 ${safe.gap}px（app padding ${safe.appPad}）`)
  await page.evaluate(() => {
    for (const el of document.querySelectorAll('style')) {
      if (el.textContent?.includes('--m-safe-bottom:34px')) el.remove()
    }
  })
  await page.waitForTimeout(100)

  const draft = '手机端发一条'
  await page.fill('.hub-dm .composer textarea', draft)
  await page.click('.hub-dm .composer .send-btn')
  await waitFor(async () => (await page.locator('.hub-dm .messages').innerText()).includes(draft))
  const sendPayload = seen.sends.find((s) => s.cmd === 'hub_send')
  assert(sendPayload, 'hub_send 没有被调用')
  assert(JSON.stringify(sendPayload.payload).includes(draft), `hub_send 载荷里没有正文：${JSON.stringify(sendPayload.payload).slice(0, 120)}`)
  console.log('  ✅ 发消息：乐观气泡出现 + hub_send 带上正文')
  await measure(page, 'DM 会话（发送后）', vp)

  // 4) 详情抽屉：Context / 任务 / Needs
  await page.locator('.m-conv .chat-header .icon-btn[aria-label="上下文"]').first().click()
  await waitFor(() => page.locator('[data-testid="mobile-inspector-drawer"]').count())
  const drawerTabs = await page.locator('.m-drawer .hub-inspector-tabs button').allInnerTexts()
  for (const label of ['概览', '任务', 'Needs', '会话', 'Context']) {
    assert(drawerTabs.some((t) => t.includes(label)), `抽屉缺少「${label}」页签：${JSON.stringify(drawerTabs)}`)
  }
  // 页签可达性：每个 ≥44px，且 scrollIntoView 后完整落在页签条可视区内
  // （390 下 5 个页签 + 关闭按钮只剩 0px 余量，320 下必须靠横向滚动）。
  const tabCheck = await page.evaluate(() => {
    const nav = document.querySelector('.m-drawer .hub-inspector-tabs')
    const navRect = nav.getBoundingClientRect()
    const tabs = [...nav.querySelectorAll('button[role="tab"]')].map((b) => {
      b.scrollIntoView({ inline: 'nearest', block: 'nearest' })
      const r = b.getBoundingClientRect()
      return {
        t: b.innerText.trim().replace(/\s+/g, ' '),
        left: Math.round(r.left),
        right: Math.round(r.right),
        h: Math.round(r.height),
        inNav: r.left >= navRect.left - 1 && r.right <= navRect.right + 1,
      }
    })
    return {
      clientW: Math.round(nav.clientWidth),
      scrollW: Math.round(nav.scrollWidth),
      overflow: nav.dataset.overflow ?? 'none',
      tabs,
    }
  })
  assert(tabCheck.tabs.length === 5, `抽屉页签数量 ${tabCheck.tabs.length}，期望 5`)
  for (const t of tabCheck.tabs) {
    assert(t.h >= 43.5, `页签「${t.t}」高 ${t.h}px < 44`)
    assert(t.inNav, `页签「${t.t}」不在可视区（left=${t.left}, right=${t.right}）`)
  }
  console.log(`  ✅ 抽屉页签：5 个全 ≥44px 且在可视区（scroll ${tabCheck.scrollW}/${tabCheck.clientW}, overflow=${tabCheck.overflow}）`)
  // 「上下文」按钮打开的是 Context 页；先切回概览，量概览的排版/对比度。
  await page.locator('.m-drawer .hub-inspector-tabs button', { hasText: '概览' }).first().click()
  await waitFor(() => page.locator('.m-drawer .hub-ov-id').count())
  await measure(page, '详情抽屉（概览）', vp, 350)
  shots.push(await shot(page, vp, '4-drawer', theme))
  await auditContrast(page, [
    ['.m-drawer .hub-ov-id strong', '抽屉名字'],
    ['.m-drawer .hub-ov-id span', '抽屉副标题'],
  ], `${vp.name}/${theme} 抽屉`)

  await page.locator('.m-drawer .hub-inspector-tabs button', { hasText: '任务' }).first().click()
  await waitFor(async () => (await page.locator('.m-drawer .hub-panel-list').innerText()).includes('整理竞品资料'))
  console.log('  ✅ 抽屉 → 任务：任务行可见')
  await measure(page, '详情抽屉（任务）', vp, 350)
  shots.push(await shot(page, vp, '5-drawer-tasks', theme))

  await page.locator('.m-drawer .hub-inspector-tabs button', { hasText: 'Needs' }).first().click()
  await waitFor(async () => (await page.locator('.m-drawer').innerText()).includes('写文件前确认'))
  console.log('  ✅ 抽屉 → Needs：审批卡可见')
  await measure(page, '详情抽屉（Needs）', vp, 350)
  shots.push(await shot(page, vp, '6-drawer-needs', theme))

  // 会话页签：五档里最后一个没被上面覆盖的，点一下确认面板可切换。
  await page.locator('.m-drawer .hub-inspector-tabs button', { hasText: '会话' }).first().click()
  await page.waitForTimeout(200)
  const sessTabText = await page.locator('.m-drawer').innerText()
  assert(sessTabText.length > 20, `抽屉「会话」页签内容为空：${sessTabText.slice(0, 60)}`)
  console.log('  ✅ 抽屉 → 会话：面板可切换')

  // 5) Esc 先关抽屉（不退出会话）
  await page.keyboard.press('Escape')
  await waitFor(async () => (await page.locator('[data-testid="mobile-inspector-drawer"]').count()) === 0)
  assert((await page.locator('[data-testid="mobile-conversation"]').count()) === 1, 'Esc 关抽屉时把会话也关了')
  console.log('  ✅ Esc：先关抽屉，会话仍在')

  // 6) 返回列表（44px 按钮 → popstate）
  await page.click('[data-testid="mobile-back"]')
  await waitFor(() => page.locator('[data-testid="mobile-hub-list"]').count())
  console.log('  ✅ 返回按钮回到列表')

  // 7) 群：GroupView（同一个 HubSurface）
  await page.locator('.hub-side-tabs button', { hasText: '群组' }).first().click()
  await waitFor(() => page.locator('.hub-group-row').count())
  await page.locator('.hub-group-row', { hasText: '产品评审群' }).first().click()
  await waitFor(() => page.locator('.hub-group-view').count())
  await waitFor(async () => (await page.locator('.hub-group-view').innerText()).includes('产品评审群'))
  // 历史是异步回来的（hub_group_get → hub_group_history → hub_group_activity）：等正文出现，
  // 不要刚看到 .hub-group-view 就读 innerText（那会读到空时间线）。
  await waitFor(async () => (await page.locator('.hub-group-view .messages').innerText()).includes('这版数据我看过了'))
  const groupText = await page.locator('.hub-group-view .messages').innerText()
  assert(groupText.includes('这版数据我看过了'), `群里没有历史消息：${groupText.slice(0, 80)}`)
  await waitFor(() => page.locator('.hub-group-view .event-card--task').count())
  assert((await page.locator('.hub-group-view .event-card--task').innerText()).includes('资料整理'), '群时间线里没有任务卡')
  console.log('  ✅ 群会话：GroupView + 任务卡可见')
  await measure(page, '群会话', vp)
  shots.push(await shot(page, vp, '7-group', theme))

  // 注意量 .composer 本身：.composer-wrap 里还有内联 Needs/待办卡（审批卡停靠区），
  // 那是消息区内容，不是 composer 高度。
  const gcomp = await page.evaluate(() => {
    const r = (s) => Math.round(document.querySelector(s).getBoundingClientRect().height)
    return { composer: r('.hub-group-view .composer'), meta: r('.hub-group-view .composer-meta'), ta: r('.hub-group-view .composer textarea') }
  })
  assert(gcomp.composer <= 80, `群 composer ${gcomp.composer}px 偏高（应 ≤80）`)
  assert(gcomp.meta <= 52, `群 composer meta 行 ${gcomp.meta}px 偏高（应 ≤52，含 44px 切换）`)
  assert(gcomp.ta >= 24 && gcomp.ta <= 96, `群输入行 ${gcomp.ta}px 不在 24–96 内`)
  await auditContrast(page, [
    ['.hub-group-view .event-card--task .event-title', '任务卡标题'],
    ['.hub-group-view .agent-text', '群正文'],
    ['.hub-group-view .composer textarea', '群 composer'],
  ], `${vp.name}/${theme} 群`)

  // 8) 群 composer 发消息（hub_group_send）
  await page.fill('.hub-group-view .composer textarea', '群里也说一句')
  await page.click('.hub-group-view .composer .send-btn')
  await waitFor(async () => (await page.locator('.hub-group-view .messages').innerText()).includes('群里也说一句'))
  assert(seen.sends.some((s) => s.cmd === 'hub_group_send'), 'hub_group_send 没有被调用')
  console.log('  ✅ 群发消息：乐观气泡出现 + hub_group_send')

  // 9) 群会话里打开抽屉的 Context（只读、手机不渲染提示词分层）
  await page.locator('.hub-group-view .chat-header .icon-btn[aria-label="上下文"]').first().click()
  await waitFor(() => page.locator('[data-testid="mobile-inspector-drawer"]').count())
  await waitFor(async () => (await page.locator('.m-drawer').innerText()).includes('消息上下文'))
  assert(!(await page.locator('.m-drawer').innerText()).includes('提示词分层'), '手机不应渲染提示词分层（allowlist 未放行）')
  console.log('  ✅ 抽屉 Context：消息上下文可见、提示词分层不渲染（避免 403）')
  shots.push(await shot(page, vp, '8-drawer-context', theme))
  await page.locator('.m-drawer .hub-inspector-close').click()
  await waitFor(async () => (await page.locator('[data-testid="mobile-inspector-drawer"]').count()) === 0)

  // 10) 再返回，确认列表还在（导航往返两次不丢状态）
  await page.click('[data-testid="mobile-back"]')
  await waitFor(() => page.locator('[data-testid="mobile-hub-list"]').count())
  await measure(page, '回到列表', vp)

  // 11) 能力收窄：手机不渲染群编辑/智能体编辑/新建（allowlist 外的入口）
  assert((await page.locator('[data-testid="mobile-hub-list"] .hub-side-add').count()) === 0, '手机列表仍渲染「新建」按钮（会 403）')
  console.log('  ✅ 能力收窄：列表没有「新建群/智能体」入口')

  assert(errors.length === 0, `${vp.name}: 有 pageerror/console error：${JSON.stringify(errors.slice(0, 5))}`)
  console.log(`  ✅ 0 pageerror / 0 console error`)
  await context.close()
  return shots
}

async function main() {
  if (!existsSync(join(DIST, 'mobile.html')) && !existsSync(join(DIST, 'index.html'))) {
    throw new Error(`没有 ${DIST}/mobile.html 或 index.html —— 先跑 npm run mobile:build`)
  }
  mkdirSync(SHOTS, { recursive: true })
  const { server, port } = await startServer()
  const baseURL = `http://127.0.0.1:${port}/`
  console.log(`静态服务：${baseURL}（dist-mobile）`)
  const browser = await chromium.launch()
  const all = []
  try {
    for (const vp of VIEWPORTS) {
      for (const theme of THEMES) all.push(...await runViewport(browser, baseURL, vp, theme))
    }
  } finally {
    await browser.close()
    await new Promise((r) => server.close(r))
  }
  console.log(`\n截图（${all.length} 张）：`)
  for (const f of all) console.log(`  · ${f}`)
  console.log('\nMOBILE_HUB_SCREENS_PASS')
}

main().catch((e) => {
  console.error(`\n❌ ${e.message}`)
  process.exit(1)
})
