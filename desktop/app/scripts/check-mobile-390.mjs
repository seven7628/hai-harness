#!/usr/bin/env node
// 390x844 手机页面实测（P4-03c N10）。
//
// 做的事：
//   1. 静态审计（mobile-web-audit.mjs）永远先跑；
//   2. 起一个本地 HTTP 服务托管 desktop/bridge/mobile_web，mock /api/cmd + Hub WS，
//      用 Playwright Chromium 在 390x844 真实渲染 列表 / Hub 首页 / 群 / DM；
//   3. 断言 documentElement.scrollWidth <= 390、顶栏按钮不被裁切、可见按钮 >=44px；
//   4. 顺带实测主题持久化、reduced-motion、键入 @、发送失败保草稿、已读游标。
//   5. P4-03d：发送失败后「重试」的 payload 必须沿用首次 client_msg_id（幂等键），
//      且结构化 mentions 真的发给后端（不是只画在输入框里）。
//
// 浏览器不可用时不会静默跳过：打印 MODE=static + 原因，只跑静态审计；
// 传 --require-browser 则直接失败（CI 可用来强制真实测）。
import http from 'node:http'
import { readFile, stat } from 'node:fs/promises'
import { extname, join, normalize, resolve } from 'node:path'
import { auditMobileWeb, MOBILE_WEB_DIR } from './mobile-web-audit.mjs'

const VIEWPORT = { width: 390, height: 844 }
const requireBrowser = process.argv.includes('--require-browser')
const TOKEN = 'a'.repeat(64)
const NOW = '2026-01-01T00:00:00Z'

const GROUP = {
  id: 'grp_1',
  name: '产品评审群',
  pinned: false,
  // 6 名成员：叠放规则（≥5 → 前 3 + N）在 390 下要有真实数据可测。
  members: [
    { agent_id: 'ag_alice', role: 'owner' },
    { agent_id: 'ag_bob', role: 'member' },
    { agent_id: 'ag_carol', role: 'member' },
    { agent_id: 'ag_dave', role: 'member' },
    { agent_id: 'ag_erin', role: 'member' },
    { agent_id: 'ag_frank', role: 'member' },
  ],
}
const AGENT = {
  id: 'ag_alice',
  name: 'Alice',
  role: '研究员',
  provider: 'anthropic',
  model: 'claude-opus',
}
const AGENTS = [
  AGENT,
  { id: 'ag_bob', name: 'Bob', role: '写作', provider: 'anthropic', model: 'claude-opus' },
  { id: 'ag_carol', name: 'Carol', role: '找反例、标风险', provider: 'anthropic', model: 'claude-opus' },
  { id: 'ag_dave', name: 'Dave', role: '分类、起草、等批准', provider: 'anthropic', model: 'claude-opus' },
  { id: 'ag_erin', name: 'Erin', role: '盯竞品、盯价格变动', provider: 'anthropic', model: 'claude-opus' },
  { id: 'ag_frank', name: 'Frank', role: '拆数据、算口径', provider: 'anthropic', model: 'claude-opus' },
]
const groupMessages = [
  {
    id: 'm1',
    seq: 1,
    group_id: 'grp_1',
    kind: 'text',
    sender: { kind: 'agent', agent_id: 'ag_alice' },
    text: '**结论**：先修登录。\n```js\nconst ok = true\n```',
    created_at: NOW,
  },
  {
    id: 'm2',
    seq: 2,
    group_id: 'grp_1',
    kind: 'text',
    sender: { kind: 'human' },
    text: '<img src=x onerror="window.__xss=1"> 这不是标签',
    created_at: NOW,
  },
]
// 足够长的时间线：390x844 下必须真的能向上滚，已读守卫才有意义。
for (let i = 3; i <= 30; i++) {
  groupMessages.push({
    id: 'm' + i,
    seq: i,
    group_id: 'grp_1',
    kind: 'text',
    sender: { kind: 'agent', agent_id: i % 2 ? 'ag_alice' : 'ag_bob' },
    text: `第 ${i} 条历史消息：` + '这是一段用于撑开滚动区域的历史正文。'.repeat(4),
    created_at: NOW,
  })
}

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.woff2': 'font/woff2',
}

const seenCmds = []
// 发送类命令的 payload：P4-03d 要证明「失败后重试沿用同一个 client_msg_id」，
// 以及结构化 mentions 真的发给了后端（不是只画在输入框里）。
const seenSends = []
// hub_group_history / hub_history 的 payload：搜索跳转要证明「真的拉了命中附近一页」。
const seenHistory = []
// hub_search 的 before 游标：要证明手机端把 next_before.conversations 原样回传（B1）。
const seenSearchBefore = []
let failSend = false
let sendOk = 0

function json(res, status, body) {
  res.writeHead(status, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' })
  res.end(JSON.stringify(body))
}

async function serveStatic(res, pathname) {
  let rel = pathname.replace(/^\/m\/?/, '')
  if (!rel) rel = 'index.html'
  const full = normalize(join(MOBILE_WEB_DIR, rel))
  if (!resolve(full).startsWith(resolve(MOBILE_WEB_DIR))) {
    res.writeHead(403)
    res.end('forbidden')
    return
  }
  try {
    const info = await stat(full)
    if (!info.isFile()) throw new Error('not file')
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

// 搜索命中：一条群命中故意放在 seq=40（初始页只加载 1/2），用来验证
// 「命中不在已加载历史里 → 拉 before_seq=seq+1 的一页 → 跳转高亮」。
const SEARCH_HITS = [
  {
    scope: 'group', group_id: 'grp_1', name: '产品评审群', seq: 40, message_id: 'msg_40',
    snippet: '…深历史里的关键词命中…', sender: 'Alice', sender_kind: 'agent', kind: 'text',
    created_at: NOW, owner_only: false,
  },
  {
    scope: 'group', group_id: 'grp_1', name: '产品评审群', seq: 2, message_id: 'msg_2',
    snippet: '…关键词只给群主看…', sender: '我', sender_kind: 'human', kind: 'text',
    created_at: NOW, owner_only: true,
  },
  {
    scope: 'dm', agent_id: 'ag_alice', name: 'Alice', seq: 1, message_id: 'msg_1',
    snippet: '…关键词的私聊…', sender: 'Alice', sender_kind: 'agent', kind: 'text',
    created_at: NOW, owner_only: false,
  },
]

function mockCmd(cmd, payload) {
  switch (cmd) {
    case 'workspace_list':
      return { workspaces: [{ path: '/Users/demo/proj', title: 'demo' }] }
    case 'list':
      return { sessions: [{ id: 'sess-1', title: '修复登录', updated_at: Date.parse(NOW) }] }
    case 'hub_agent_list':
      return {
        agents: AGENTS,
        dm: { ag_alice: { unread: 2, last_text: '昨天的结论', last_at: NOW } },
      }
    case 'hub_group_list':
      return {
        groups: [GROUP],
        progress: { grp_1: { unread: 3, last_text: '最近一条', last_at: NOW } },
      }
    case 'hub_need_list':
      return { needs: [] }
    case 'hub_group_get':
      return { group: GROUP, conversation: { status: 'idle', turns_used: 0, turn_budget: 8 } }
    case 'hub_group_history': {
      // 搜索跳转的 around 页：以 before_seq 结尾的一页（含 seq=40 的命中）。
      if (Number((payload || {}).before_seq) === 41) {
        return {
          messages: [
            {
              id: 'msg_40', seq: 40, group_id: 'grp_1', kind: 'text',
              sender: { kind: 'agent', agent_id: 'ag_alice' }, text: '深历史里的关键词命中', created_at: NOW,
            },
          ],
          has_more: true,
        }
      }
      return { messages: groupMessages, has_more: false }
    }
    case 'hub_search': {
      const before = (payload || {}).before || {}
      const convs = before.conversations || {}
      const base = {
        query: String((payload || {}).query || ''),
        scope: String((payload || {}).scope || 'all'),
        limit: 50,
        scanned: 2,
      }
      // 第二页：只有客户端回传了 conversations 才会走到这里（否则后端会退回第一页）。
      if (convs['g:grp_1'] === 40) {
        const older = {
          scope: 'group', group_id: 'grp_1', name: '产品评审群', seq: 30, message_id: 'msg_30',
          snippet: '…更旧的关键词命中…', sender: 'Alice', sender_kind: 'agent', kind: 'text',
          created_at: NOW, owner_only: false,
        }
        return { ...base, hits: [older], count: 1, has_more: false, next_before: null }
      }
      return {
        ...base,
        hits: SEARCH_HITS,
        count: SEARCH_HITS.length,
        has_more: true,
        next_before: { created_at: NOW, id: 'msg_1', seq: 1, conversations: { 'g:grp_1': 40 } },
      }
    }
    case 'hub_open':
      return {
        agent: AGENT,
        messages: [
          {
            id: 'dm1',
            seq: 1,
            kind: 'text',
            sender: { kind: 'agent', agent_id: 'ag_alice' },
            text: '**已收到**，我来看。',
            created_at: NOW,
          },
        ],
        has_more: false,
      }
    case 'hub_mark_read':
      return {}
    case 'hub_group_send':
      if (failSend) throw new Error('mock send failure')
      sendOk++
      return { message_id: 'srv-g-' + sendOk, seq: 100 + sendOk }
    case 'hub_send':
      sendOk++
      return { message_id: 'srv-d-' + sendOk, seq: 200 + sendOk }
    default:
      return {}
  }
}

function startServer() {
  const server = http.createServer(async (req, res) => {
    const url = new URL(req.url, 'http://localhost')
    if (url.pathname === '/api/ping') return json(res, 200, { paired: true })
    if (url.pathname === '/api/cmd') {
      if (req.method !== 'POST') return json(res, 405, { error: 'method' })
      let raw = ''
      for await (const chunk of req) raw += chunk
      let body = {}
      try {
        body = JSON.parse(raw || '{}')
      } catch {
        return json(res, 400, { error: 'bad json' })
      }
      const cmd = String(body.cmd || '')
      seenCmds.push(cmd)
      if (cmd === 'hub_group_send' || cmd === 'hub_send') {
        seenSends.push({ cmd, payload: body.payload || {} })
      }
      if (cmd === 'hub_group_history' || cmd === 'hub_history') {
        seenHistory.push(body.payload || {})
      }
      if (cmd === 'hub_search') {
        seenSearchBefore.push((body.payload || {}).before || {})
      }
      try {
        const data = mockCmd(cmd, body.payload || {})
        return json(res, 200, { ok: true, data })
      } catch (e) {
        return json(res, 500, { ok: false, error: e.message })
      }
    }
    if (url.pathname === '/' || url.pathname === '/m') {
      res.writeHead(302, { Location: '/m/' })
      return res.end()
    }
    if (url.pathname === '/m/' || url.pathname.startsWith('/m/')) {
      return serveStatic(res, url.pathname)
    }
    res.writeHead(404)
    res.end('not found')
  })
  return new Promise((resolve) => {
    server.listen(0, '127.0.0.1', () => resolve({ server, port: server.address().port }))
  })
}

function assert(cond, msg) {
  if (!cond) throw new Error(msg)
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function waitFor(fn, timeout = 4000, step = 25) {
  const end = Date.now() + timeout
  while (Date.now() < end) {
    const v = await fn()
    if (v) return v
    await sleep(step)
  }
  throw new Error('waitFor timeout')
}

async function measure(page, label) {
  await page.waitForTimeout(60)
  const m = await page.evaluate(() => {
    const de = document.documentElement
    const rect = (el) => {
      const r = el.getBoundingClientRect()
      return { id: el.id || el.className || el.tagName, left: r.left, right: r.right, width: r.width, height: r.height }
    }
    const headerButtons = [...document.querySelectorAll('#bar button')].filter((b) => !b.hidden).map(rect)
    const screen = [...document.querySelectorAll('.screen')].find((s) => !s.hidden)
    const buttons = screen ? [...screen.querySelectorAll('button')].filter((b) => !b.hidden && b.offsetParent !== null).map(rect) : []
    return {
      innerWidth: window.innerWidth,
      scrollWidth: de.scrollWidth,
      barScrollWidth: document.getElementById('bar').scrollWidth,
      headerButtons,
      buttons,
    }
  })
  assert(m.innerWidth === VIEWPORT.width, `${label}: innerWidth=${m.innerWidth}，不是 390`)
  assert(m.scrollWidth <= VIEWPORT.width, `${label}: documentElement.scrollWidth=${m.scrollWidth} > 390`)
  assert(m.barScrollWidth <= VIEWPORT.width, `${label}: 顶栏 scrollWidth=${m.barScrollWidth} > 390`)
  for (const b of m.headerButtons) {
    assert(b.left >= -0.5 && b.right <= VIEWPORT.width + 0.5, `${label}: 顶栏按钮 ${b.id} 被裁切 (${b.left}..${b.right})`)
    assert(b.width >= 43.5 && b.height >= 43.5, `${label}: 顶栏按钮 ${b.id} 只有 ${b.width}x${b.height}`)
  }
  for (const b of m.buttons) {
    assert(b.height >= 43.5, `${label}: 按钮 ${b.id} 高度 ${b.height} < 44`)
    assert(b.width >= 43.5, `${label}: 按钮 ${b.id} 宽度 ${b.width} < 44`)
  }
  console.log(`  ✅ ${label}: scrollWidth=${m.scrollWidth}, header=${m.headerButtons.length}, buttons=${m.buttons.length} all >=44px`)
}

async function openTheme(page) {
  await page.click('#theme')
  await page.waitForSelector('#theme-sheet:not([hidden])')
}

async function main() {
  console.log('[390] 静态审计 ...')
  const audit = auditMobileWeb()
  if (audit.errors.length) {
    for (const e of audit.errors) console.error(`  ❌ ${e}`)
    throw new Error(`静态审计失败（${audit.errors.length} 项）`)
  }
  console.log(`  ✅ 静态审计通过（${audit.checks} 条 CSS 规则）`)

  let chromium
  try {
    ;({ chromium } = await import('playwright'))
  } catch (e) {
    console.log(`[390] MODE=static browser-unavailable: ${String(e.message).split('\n')[0]}`)
    console.log('[390] 真实 390x844 未测量；仅静态审计通过。')
    if (requireBrowser) process.exitCode = 2
    return
  }

  const { server, port } = await startServer()
  let browser
  try {
    try {
      browser = await chromium.launch({ headless: true })
    } catch (e) {
      console.log(`[390] MODE=static browser-unavailable: ${String(e.message).split('\n')[0]}`)
      console.log('[390] 真实 390x844 未测量；仅静态审计通过。')
      if (requireBrowser) process.exitCode = 2
      return
    }
    console.log(`[390] MODE=browser chromium=${browser.version()} viewport=${VIEWPORT.width}x${VIEWPORT.height}`)
    const context = await browser.newContext({ viewport: VIEWPORT, deviceScaleFactor: 1 })
    const page = await context.newPage()
    page.setDefaultTimeout(5000)
    await page.addInitScript((tok) => {
      localStorage.setItem('hai.token', tok)
    }, TOKEN)
    await page.emulateMedia({ colorScheme: 'light', reducedMotion: 'no-preference' })

    let hubWS = null
    await page.routeWebSocket(/\/api\/events/, (ws) => {
      hubWS = ws
      ws.onMessage(() => {})
      ws.send(JSON.stringify({ type: 'hello', scope: 'hub' }))
    })

    await page.goto(`http://127.0.0.1:${port}/m/`, { waitUntil: 'domcontentloaded' })
    await page.waitForSelector('#screen-list:not([hidden])')
    await page.waitForSelector('#open-hub')
    console.log('[390] 列表屏就绪')
    await measure(page, '列表 390x844')

    // —— 主题：跟随系统 → 手动墨/宣纸 → 持久化 ——
    assert((await page.evaluate(() => document.documentElement.dataset.theme)) === 'paper', 'system + 浅色系统应为 paper')
    await openTheme(page)
    const themeHits = await page.evaluate(() =>
      [...document.querySelectorAll('#theme-options .theme-option')].map((b) => {
        const r = b.getBoundingClientRect()
        return { w: r.width, h: r.height }
      }),
    )
    assert(themeHits.length === 3 && themeHits.every((r) => r.h >= 44 && r.w >= 44), `主题选项应 >=44px: ${JSON.stringify(themeHits)}`)
    await page.click('[data-theme-mode="system"]')
    await page.emulateMedia({ colorScheme: 'dark' })
    await page.waitForFunction(() => document.documentElement.dataset.theme === 'ink')
    console.log('  ✅ 主题: system 跟随系统浅→深 (paper→ink)')
    await openTheme(page)
    await page.click('[data-theme-mode="ink"]')
    await page.emulateMedia({ colorScheme: 'light' })
    await page.waitForTimeout(80)
    assert((await page.evaluate(() => document.documentElement.dataset.theme)) === 'ink', '手动 ink 不应被浅色系统覆盖')
    console.log('  ✅ 主题: 手动 ink 不被系统浅色覆盖')
    await openTheme(page)
    await page.click('[data-theme-mode="paper"]')
    assert((await page.evaluate(() => localStorage.getItem('hai.theme'))) === 'paper', '主题持久化到 hai.theme')
    assert((await page.getAttribute('meta[name="theme-color"]', 'content')) === '#fdfcfb', 'paper 更新 meta theme-color')
    await page.reload({ waitUntil: 'domcontentloaded' })
    await page.waitForSelector('#screen-list:not([hidden])')
    assert((await page.evaluate(() => document.documentElement.dataset.theme)) === 'paper', '刷新后主题仍为 paper')
    console.log('  ✅ 主题: paper 持久化并在刷新后生效')

    // —— Hub 首页 ——
    await page.click('#open-hub')
    await page.waitForSelector('#screen-hub:not([hidden])')
    await page.waitForSelector('#hub-groups .hub-row')
    assert(!(await page.locator('#theme').isHidden()), 'Hub 首页仍可打开外观')
    await measure(page, 'Hub 首页 390x844')

    // —— Luna 头像（demo 同源）：无 emoji/首字母；群行叠放 前 4 +N；ink/paper 眼睛令牌切换 ——
    const avatarHome = await page.evaluate(() => {
      const rows = (sel) => [...document.querySelectorAll(sel)]
      const groupRows = rows('#hub-groups .hub-row')
      const dmRows = rows('#hub-dms .hub-row')
      const faces = [...document.querySelectorAll('#screen-hub .luna-avatar [data-luna-face]')]
      const root = document.documentElement
      const oldTheme = root.dataset.theme
      const avatar = document.querySelector('#hub-groups .luna-avatar')
      root.dataset.theme = 'ink'
      const inkEye = avatar ? getComputedStyle(avatar).getPropertyValue('--eye').trim() : ''
      root.dataset.theme = 'paper'
      const paperEye = avatar ? getComputedStyle(avatar).getPropertyValue('--eye').trim() : ''
      root.dataset.theme = oldTheme
      return {
        legacy: document.querySelectorAll('#screen-hub .hub-avatar, #screen-hub .sigil').length,
        faceCount: faces.length,
        faceText: [...document.querySelectorAll('#screen-hub .luna-avatar')].map((n) => n.textContent || '').filter(Boolean),
        groupRows: groupRows.map((row) => {
          const stack = row.querySelector('.avatar-stack')
          const parts = [...row.querySelectorAll('.avatar-stack .luna-avatar')]
          return {
            parts: parts.length,
            more: row.querySelector('.avatar-more')?.textContent || '',
            lead: parts[0]?.getAttribute('data-state') || '',
            peersIdle: parts.slice(1).every((n) => n.getAttribute('data-state') === 'idle'),
            hidden: stack?.getAttribute('aria-hidden') || '',
          }
        }),
        dmRows: dmRows.map((row) => {
          const avatarEl = row.querySelector('.luna-avatar')
          return avatarEl ? {
            size: avatarEl.getAttribute('data-size'),
            face: !!avatarEl.querySelector('[data-luna-face]'),
            hidden: avatarEl.getAttribute('aria-hidden') || '',
          } : null
        }),
        inkEye,
        paperEye,
      }
    })
    assert(avatarHome.legacy === 0, `Hub 首页不应再有 .hub-avatar / .sigil 旧头像（got=${avatarHome.legacy}）`)
    assert(avatarHome.faceCount > 0 && avatarHome.faceText.length === 0,
      `Luna 头像内不能有 emoji/首字母文本：${JSON.stringify(avatarHome.faceText)}`)
    assert(avatarHome.groupRows.length === 1 && avatarHome.groupRows[0].parts === 4 &&
      avatarHome.groupRows[0].more === '+2', `群行 ≥5 成员应显示前 4 + +2（demo 口径）：${JSON.stringify(avatarHome.groupRows)}`)
    assert(avatarHome.groupRows[0].lead !== '' && avatarHome.groupRows[0].peersIdle,
      '群叠放只有 lead 画状态层，peer 的 data-state=idle')
    assert(avatarHome.groupRows[0].hidden === 'true', '侧栏式群行里的叠放对读屏 aria-hidden（行名已承载身份）')
    assert(avatarHome.dmRows.every((r) => r && r.size === '32' && r.face && r.hidden === 'true'),
      `DM 行头像应为 32px Luna 且 aria-hidden：${JSON.stringify(avatarHome.dmRows)}`)
    assert(avatarHome.inkEye && avatarHome.paperEye && avatarHome.inkEye !== avatarHome.paperEye,
      `ink/paper 的眼睛令牌应不同（ink=${avatarHome.inkEye} paper=${avatarHome.paperEye}）`)
    console.log('  ✅ 头像: Luna 无文本，群行 4++2、lead 状态、DM 32、ink/paper 眼睛令牌切换')

    // —— 群时间线 ——
    await page.click('#hub-groups .hub-row')
    await page.waitForSelector('#screen-hub-group:not([hidden])')
    await page.waitForSelector('#hub-group-messages .hub-msg')
    await waitFor(() => hubWS)
    await measure(page, '群时间线 390x844')
    const groupHeadAvatar = await page.evaluate(() => {
      const stack = document.querySelector('#hub-group-head .avatar-stack')
      const parts = [...document.querySelectorAll('#hub-group-head .avatar-stack .luna-avatar')]
      return stack ? {
        role: stack.getAttribute('role'),
        label: stack.getAttribute('aria-label') || '',
        parts: parts.length,
        more: document.querySelector('#hub-group-head .avatar-more')?.textContent || '',
        lead: parts[0]?.getAttribute('data-state') || '',
        peersIdle: parts.slice(1).every((n) => n.getAttribute('data-state') === 'idle'),
        box: Math.round(stack.getBoundingClientRect().width),
      } : null
    })
    assert(groupHeadAvatar && groupHeadAvatar.role === 'img' &&
      groupHeadAvatar.label.includes('产品评审群') && groupHeadAvatar.label.includes('6 个智能体'),
      `群头独立叠放应有 role=img + 群名/成员数：${JSON.stringify(groupHeadAvatar)}`)
    assert(groupHeadAvatar.parts === 4 && groupHeadAvatar.more === '+2' && groupHeadAvatar.lead !== '' &&
      groupHeadAvatar.peersIdle, `群头叠放 4++2（demo 口径）且只有 lead 带状态：${JSON.stringify(groupHeadAvatar)}`)
    assert(groupHeadAvatar.box <= 150, `群头叠放在 390 下不应挤压标题（w=${groupHeadAvatar.box}）`)
    console.log('  ✅ 群头叠放: role=img + 群名/6 个智能体/+2，仅 lead 状态')
    const md = await page.evaluate(() => {
      const body = document.querySelector('#hub-group-messages .hub-msg.agent .body')
      return {
        bold: body ? body.querySelector('b') && body.querySelector('b').textContent : '',
        pre: body ? body.querySelector('pre') && body.querySelector('pre').textContent : '',
        img: document.querySelectorAll('#hub-group-messages img').length,
        xss: !!window.__xss,
      }
    })
    assert(md.bold === '结论', `群定稿正文应有粗体节点，got=${JSON.stringify(md.bold)}`)
    assert(md.pre.includes('const ok = true'), '群定稿正文应渲染围栏代码块')
    assert(md.img === 0 && !md.xss, '不可信 HTML 只作为文本，不产生 img/脚本')
    console.log('  ✅ 群 mdLite: bold/pre 渲染，<img onerror> 未执行')

    // —— reduced motion 实测 ——
    await page.emulateMedia({ reducedMotion: 'reduce' })
    hubWS.send(JSON.stringify({
      event_type: 'hub_message_delta', group_id: 'grp_1', message_id: 'draft-1',
      agent_id: 'ag_alice', index: 0, delta: '正在分析',
    }))
    await page.waitForSelector('.hub-msg.draft .body')
    const motion = await page.evaluate(() => {
      const body = document.querySelector('.hub-msg.draft .body')
      const dots = document.querySelector('.hub-typing .dots')
      return {
        draft: getComputedStyle(body, '::after').animationName,
        dots: dots ? getComputedStyle(dots, '::after').animationName : 'missing',
      }
    })
    await openTheme(page)
    const sheetAnim = await page.evaluate(() => getComputedStyle(document.getElementById('theme-sheet')).animationName)
    await page.click('#theme-close')
    assert(motion.draft === 'none', `reduced-motion: 草稿光标动画=${motion.draft}`)
    assert(motion.dots === 'none', `reduced-motion: 输入中省略号动画=${motion.dots}`)
    assert(sheetAnim === 'none', `reduced-motion: 抽屉动画=${sheetAnim}`)
    console.log('  ✅ reduced-motion: 草稿光标/输入中/抽屉动画均为 none')

    // —— N8 已读游标：读历史不推进，回底才推进 ——
    await sleep(250)
    const markBefore = seenCmds.filter((c) => c === 'hub_mark_read').length
    await page.evaluate(() => {
      const box = document.getElementById('hub-group-messages')
      box.scrollTop = 0
    })
    await sleep(120)
    hubWS.send(JSON.stringify({
      event_type: 'hub_message', group_id: 'grp_1', seq: 99, message_id: 'm99',
      message: {
        id: 'm99', seq: 99, group_id: 'grp_1', kind: 'text',
        sender: { kind: 'agent', agent_id: 'ag_bob' }, text: '历史阅读中到达的新消息', created_at: NOW,
      },
    }))
    await page.waitForFunction(() => document.getElementById('hub-group-messages').textContent.includes('历史阅读中到达的新消息'))
    await sleep(250)
    const markAtTop = seenCmds.filter((c) => c === 'hub_mark_read').length
    assert(markAtTop === markBefore, `读历史时不应推进已读（before=${markBefore} after=${markAtTop}）`)
    await page.evaluate(() => {
      const box = document.getElementById('hub-group-messages')
      box.scrollTop = box.scrollHeight
    })
    await waitFor(() => seenCmds.filter((c) => c === 'hub_mark_read').length > markBefore, 3000)
    console.log('  ✅ 已读游标: 顶部读历史不推进，滚回底部后推进')

    // —— N4 键入 @ ——
    await page.fill('#hub-group-input', '请 ')
    await page.focus('#hub-group-input')
    await page.keyboard.press('End')
    await page.keyboard.type('@')
    await page.waitForSelector('#mention-picker:not([hidden])')
    const mentionAvatars = await page.evaluate(() => [...document.querySelectorAll('#mention-list .luna-avatar')].map((n) => ({
      size: n.getAttribute('data-size'), face: !!n.querySelector('[data-luna-face]'),
      text: n.textContent || '', hidden: n.getAttribute('aria-hidden') || '',
    })))
    assert(mentionAvatars.length >= 1 && mentionAvatars.every((a) => a.size === '24' && a.face &&
      !a.text && a.hidden === 'true'),
    `@ 选择器成员行应用 24px Luna 头像（无 emoji/首字母）：${JSON.stringify(mentionAvatars.slice(0, 2))}`)
    assert((await page.evaluate(() => document.activeElement && document.activeElement.id)) === 'mention-filter', '敲 @ 后焦点进入筛选框')
    await page.keyboard.press('ArrowDown')
    const selected = await page.evaluate(() => {
      const el = document.querySelector('#mention-list .mention-item[aria-selected="true"]')
      return el ? el.textContent : ''
    })
    assert(selected.includes('Alice'), `ArrowDown 应选中 Alice，got=${JSON.stringify(selected)}`)
    await page.keyboard.press('ArrowUp')
    await page.keyboard.type('Alice')
    await page.waitForFunction(() => document.getElementById('mention-filter').value === 'Alice')
    assert(await page.locator('#mention-list .mention-item').count() >= 1, '筛选后仍有 Alice 候选')
    const mentionHits = await page.evaluate(() =>
      [...document.querySelectorAll('#mention-list .mention-item')].map((b) => b.getBoundingClientRect().height),
    )
    assert(mentionHits.every((h) => h >= 44), `提及行应 >=44px: ${JSON.stringify(mentionHits)}`)
    await page.keyboard.press('Enter')
    await page.waitForSelector('#mention-picker', { state: 'hidden' })
    assert((await page.inputValue('#hub-group-input')) === '请 @Alice ', '选中提及后保留 @ 前面的正文')
    console.log('  ✅ 键入 @: 自动开选择器、筛选、Enter 插入且保留正文')
    await page.keyboard.type('@')
    await page.waitForSelector('#mention-picker:not([hidden])')
    await page.keyboard.press('Escape')
    await page.waitForSelector('#mention-picker', { state: 'hidden' })
    console.log('  ✅ 键入 @: Escape 关闭')
    await page.click('#hub-group-at')
    await page.waitForSelector('#mention-picker:not([hidden])')
    await page.click('#mention-back')
    await page.waitForSelector('#mention-picker', { state: 'hidden' })
    console.log('  ✅ @ 按钮仍可用，返回键关闭选择器')

    // —— N5 发送失败保草稿 + 重试 ——
    failSend = true
    const draft = '请 @Alice 看下价格表'
    await page.fill('#hub-group-input', draft)
    await page.click('#hub-group-send')
    await page.waitForSelector('.hub-send-error')
    assert((await page.inputValue('#hub-group-input')) === draft, `失败后草稿应原样保留，got=${JSON.stringify(await page.inputValue('#hub-group-input'))}`)
    const retryBox = await page.locator('.hub-retry').boundingBox()
    assert(retryBox && retryBox.height >= 44, `重试按钮应 >=44px，got=${retryBox && retryBox.height}`)
    console.log('  ✅ 发送失败: 草稿保留 + 44px 重试入口')
    failSend = false
    await page.click('.hub-retry')
    await page.waitForFunction(() => document.getElementById('hub-group-input').value === '')
    await page.waitForSelector('.hub-send-error', { state: 'detached' })
    assert(seenCmds.filter((c) => c === 'hub_group_send').length === 2, '重试确实重新发送了一次')
    console.log('  ✅ 重试成功后才清空草稿')
    const sends = seenSends.filter((x) => x.cmd === 'hub_group_send')
    const cid1 = sends[0] && sends[0].payload.client_msg_id
    const cid2 = sends[1] && sends[1].payload.client_msg_id
    assert(Boolean(cid1), `首次发送必须带幂等键 client_msg_id，got=${JSON.stringify(cid1)}`)
    assert(cid1 === cid2, `失败后重试必须沿用首次 client_msg_id（got ${cid1} vs ${cid2}）`)
    console.log('  ✅ 重试沿用同一 client_msg_id（首次其实落盘也不会写两条）')
    assert(
      Array.isArray(sends[1].payload.mentions) && sends[1].payload.mentions.includes('ag_alice'),
      `发送 payload 必须带结构化 mentions，got=${JSON.stringify(sends[1].payload.mentions)}`,
    )
    console.log('  ✅ 发送 payload 带结构化 mentions（@Alice → ag_alice）')

    // —— DM ——
    await page.click('#back')
    await page.waitForSelector('#screen-hub:not([hidden])')
    const dmMarkBefore = seenCmds.filter((c) => c === 'hub_mark_read').length
    await page.click('#hub-dms .hub-row')
    await page.waitForSelector('#screen-hub-dm:not([hidden])')
    await page.waitForSelector('#hub-dm-messages .hub-msg')
    await measure(page, 'DM 390x844')
    const dmHeadAvatar = await page.evaluate(() => {
      const avatarEl = document.querySelector('#hub-dm-head .luna-avatar')
      return avatarEl ? {
        role: avatarEl.getAttribute('role'),
        label: avatarEl.getAttribute('aria-label') || '',
        size: avatarEl.getAttribute('data-size'),
        face: !!avatarEl.querySelector('[data-luna-face]'),
      } : null
    })
    assert(dmHeadAvatar && dmHeadAvatar.role === 'img' && dmHeadAvatar.label.startsWith('Alice，') &&
      dmHeadAvatar.size === '32' && dmHeadAvatar.face,
      `DM 头独立头像应有 role=img + 名字/状态：${JSON.stringify(dmHeadAvatar)}`)
    console.log('  ✅ DM 头: 32px Luna + role=img 名字/状态')
    const dmBold = await page.evaluate(() => {
      const b = document.querySelector('#hub-dm-messages .hub-msg .body b')
      return b ? b.textContent : ''
    })
    assert(dmBold === '已收到', 'DM 定稿正文也走 mdLite')
    await waitFor(() => seenCmds.filter((c) => c === 'hub_mark_read').length > dmMarkBefore, 3000)
    console.log('  ✅ DM: 打开在底部才推进已读')

    // —— P4-04b-1 全历史搜索：入口 → 结果行 → 点命中 → around 页 → 跳转高亮 ——
    await page.click('#hub-search-btn')
    await page.waitForSelector('#hub-search:not([hidden])')
    await page.fill('#hub-search-input', '关键词')
    await page.waitForFunction(() => document.querySelectorAll('#hub-search-results .hub-search-hit').length >= 3)
    const sheetMetrics = await page.evaluate(() => {
      const box = document.getElementById('hub-search')
      return {
        docScrollWidth: document.documentElement.scrollWidth,
        boxScrollWidth: box.scrollWidth,
        rows: [...box.querySelectorAll('.hub-search-hit')].map((r) => r.getBoundingClientRect().height),
        scopes: [...box.querySelectorAll('.hub-search-scopes button')].map((b) => b.getBoundingClientRect().height),
        ownerBadges: box.querySelectorAll('.hub-search-hit .owner-only').length,
      }
    })
    assert(sheetMetrics.docScrollWidth <= VIEWPORT.width, `搜索面板: documentElement.scrollWidth=${sheetMetrics.docScrollWidth} > 390`)
    assert(sheetMetrics.boxScrollWidth <= VIEWPORT.width, `搜索面板: scrollWidth=${sheetMetrics.boxScrollWidth} > 390`)
    assert(sheetMetrics.rows.every((h) => h >= 44), `搜索命中行应 >=44px: ${JSON.stringify(sheetMetrics.rows)}`)
    assert(sheetMetrics.scopes.every((h) => h >= 44), `搜索范围 chip 应 >=44px: ${JSON.stringify(sheetMetrics.scopes)}`)
    assert(sheetMetrics.ownerBadges >= 1, 'owner-only 命中必须有「仅群主可见」徽标')
    console.log(`  ✅ 搜索面板: ${sheetMetrics.rows.length} 行结果，全部 >=44px，无横向溢出`)

    // 深翻页：点「更早的结果」必须把 next_before.conversations 原样回传（B1），
    // 第二页追加而不是重画（不重复第一页的 3 条）。
    await page.click('#hub-search-more')
    await page.waitForFunction(() => document.querySelectorAll('#hub-search-results .hub-search-hit').length === 4)
    assert(
      seenSearchBefore.some((b) => b.conversations && b.conversations['g:grp_1'] === 40),
      `第二页请求必须回传 conversations 游标，got=${JSON.stringify(seenSearchBefore)}`,
    )
    const paged = await page.evaluate(() =>
      [...document.querySelectorAll('#hub-search-results .hub-search-hit')].map((r) => r.dataset.searchKey),
    )
    assert(new Set(paged).size === paged.length, `翻页后结果不能重复，got=${JSON.stringify(paged)}`)
    console.log('  ✅ 搜索: 更早的结果回传 conversations 游标、追加 1 条且无重复')

    // Escape：关闭面板（浏览器对 type=search 的原生行为会同时清空输入，
    // 我们的 input 处理把 query 一起清掉 —— 重新打开是干净的空搜索）。
    await page.keyboard.press('Escape')
    await page.waitForSelector('#hub-search', { state: 'hidden' })
    assert((await page.inputValue('#hub-search-input')) === '', 'Escape 后输入框应清空')
    console.log('  ✅ 搜索: Escape 关闭面板并清空输入')

    await page.click('#hub-search-btn')
    await page.waitForSelector('#hub-search:not([hidden])')
    await page.fill('#hub-search-input', '关键词')
    await page.waitForFunction(() => document.querySelectorAll('#hub-search-results .hub-search-hit').length >= 3)
    await page.click('#hub-search-results .hub-search-hit[data-search-key="group:grp_1:msg_40"]')
    await page.waitForSelector('#screen-hub-group:not([hidden])')
    await page.waitForSelector('#hub-group-messages [data-seq="40"]')
    assert(
      seenHistory.some((p) => Number(p.before_seq) === 41 && p.group_id === 'grp_1'),
      `点旧命中必须请求命中附近一页（before_seq=seq+1），got=${JSON.stringify(seenHistory)}`,
    )
    const highlighted = await page.evaluate(() => !!document.querySelector('#hub-group-messages [data-seq="40"].hub-msg-highlight'))
    assert(highlighted, '跳转后命中消息应高亮')
    console.log('  ✅ 搜索: 点旧命中 → 加载 seq 附近一页 → 跳转并高亮')

    console.log('[390] PASS: 390x844 真实浏览器测量完成（列表/Hub/群/DM + 主题 + 动效 + @ + 草稿 + 已读 + 搜索）')
  } finally {
    if (browser) await browser.close().catch(() => {})
    await new Promise((r) => server.close(r))
  }
}

main().catch((e) => {
  console.error('[390] FAIL:', e.message)
  process.exit(1)
})
