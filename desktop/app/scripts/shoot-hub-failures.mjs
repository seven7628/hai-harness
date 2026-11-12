#!/usr/bin/env node
// P4-UI-10 浏览器回归（真实 Chromium）：HUB-IDEM-01 / HUB-FAIL-01 / HUB-STATUS-01 /
// HUB-FIXTURE-01 + P0 复审 #1（退出 Hub 左栏恢复）与 #4（Hub 打开期间跨阈值重算右栏）。
//
// 用法（dev server 别占 5173-5175）：
//   npx vite --port 5180
//   node scripts/shoot-hub-failures.mjs --url http://localhost:5180/ [--out /tmp/hub-fail-shots]
//
// 断言：
//   · DM 发送失败：共用错误横幅（重试 + 关闭）、失败气泡带行内「重试」、store.failed 带首发 cid
//   · 点「重试」：沿用同一个 client_msg_id（payload 级证据 = mock 按 cid 幂等，重发只留 1 条），
//     failed 气泡被清掉、横幅消失、消息数不重复（重开该 DM 再数一次）
//   · 群发送失败：横幅同样有「重试」（之前只有「网络中断 ×」）、重试复用 cid 且只留 1 条
//   · 关闭横幅（×）：只关提示，失败气泡与行内重试保留
//   · 640/760/900：.statusbar scrollHeight <= clientHeight、无横向溢出、分档内容取舍正确
//     （≤900 无 cache/ctx/cost，≤700 再无 in/out）；1280 全段可见（回归保护）
//   · ?hubFixture=needs：4 种 need（审批/提问/失败/交接）真的画出来；640 无溢出
//     + HUB-NEED-01：提问卡解析出输入框后不再铺原始 questions JSON（其余三类仍显示 body）
//     + mock resolve 正常 payload（交接接受 / 提问提交回答）跑通后卡片消失
//   · ?hubFixture=tasks：排队/失败/中断 3 条任务真的画出来；640 无溢出
//   · 640 从 rail 进 Hub → 点 inspector 图标（左栏被挤成 44）→ 退出 Hub：左栏回到进 Hub 前的展开态
//   · Hub 打开期间 640→1280：inspector 按 ≥1000 展开成 ~360、左栏仍展开；缩回 640 能收回且左栏不丢
//   · P1 复审 B1/B2/B3：A/B 两条失败点 A 的重试 → wire 上是 A（正文 + 首发 cid）；
//     失败 → 横幅重试成功 → textarea 空且账本同文 1 条（DM 与群）；
//     失败 A → 成功 B → 重发 A 仍用 A 的 cid、账本 A=1（状态推进用 waitForFunction）
//
// 截图：<out>/<name>.png。任一断言失败 → 退出码 1。
import { chromium } from 'playwright'
import { mkdirSync } from 'node:fs'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const URL = arg('url', process.env.HUB_FAIL_URL || 'http://localhost:5180/')
const OUT = arg('out', '/tmp/hub-fail-shots')
mkdirSync(OUT, { recursive: true })

const browser = await chromium.launch()
const results = []
let failures = 0

function check(name, cond, detail = '') {
  if (cond) console.log(`  ✅ ${name}${detail ? `（${detail}）` : ''}`)
  else { failures++; console.log(`  ❌ ${name}${detail ? `（${detail}）` : ''}`) }
  return cond
}

async function boot(page, search = '') {
  const url = search ? `${URL}${URL.includes('?') ? '&' : '?'}${search}` : URL
  await page.goto(url, { waitUntil: 'domcontentloaded' })
  await page.waitForTimeout(1400)
  const enter = page.locator('button:has-text("进入"), .boot-enter').first()
  if (await enter.count()) {
    await enter.click({ timeout: 4000 }).catch(() => {})
    await page.waitForTimeout(1400)
  }
  await page.waitForTimeout(600)
}

/** 走「rail 收成图标条 → 点智能体」这条真实路径进 Hub（与审计复现路径一致）。 */
async function enterHub(page) {
  await page.evaluate(() => window.__store.getState().setLeftExpanded(false))
  await page.waitForTimeout(450)
  await page.locator('button[aria-label="智能体"]').first().click({ timeout: 6000 })
  await page.waitForTimeout(1000)
}

async function pickAgent(page, index = 0) {
  const tab = page.locator('.hub-side-tabs button').filter({ hasText: '智能体' }).first()
  if (await tab.count()) {
    if ((await tab.getAttribute('aria-selected')) !== 'true') { await tab.click(); await page.waitForTimeout(300) }
  }
  await page.locator('.hub-side-row').nth(index).click({ timeout: 6000 })
  await page.waitForTimeout(800)
}

async function pickGroup(page, index = 0) {
  const tab = page.locator('.hub-side-tabs button').filter({ hasText: '群组' }).first()
  if (await tab.count()) {
    if ((await tab.getAttribute('aria-selected')) !== 'true') { await tab.click(); await page.waitForTimeout(300) }
  }
  await page.locator('.hub-side-row').nth(index).click({ timeout: 6000 })
  await page.waitForTimeout(900)
}

/** 发送一条消息：填 composer 再点发送按钮（不受 IME/Enter 细节影响）。 */
async function sendText(page, scope, text) {
  const ta = page.locator(`${scope} .composer textarea`).first()
  await ta.fill(text)
  await page.waitForTimeout(120)
  await page.locator(`${scope} .composer .send-btn`).first().click({ timeout: 4000 })
}

/** 装一个 send/sendGroup 探针：记录重试时 store 实际传入的复用 cid（payload 侧的 cid 来源）。 */
async function spySends(page) {
  await page.evaluate(() => {
    const st = window.__hub.getState()
    const origSend = st.send
    const origGroup = st.sendGroup
    window.__sends = []
    window.__hub.setState({
      send: (text, reuse) => { window.__sends.push({ kind: 'dm', text, reuse: reuse ?? '' }); return origSend(text, reuse) },
      sendGroup: (text, mentions, all, reuse) => {
        window.__sends.push({ kind: 'group', text, reuse: reuse ?? '', mentions: mentions ?? [] })
        return origGroup(text, mentions, all, reuse)
      },
    })
  })
}

const hubState = (page) => page.evaluate(() => {
  const s = window.__hub.getState()
  return {
    failed: s.failed ? { kind: s.failed.kind, text: s.failed.text, clientMsgId: s.failed.clientMsgId, agentId: s.failed.agentId, groupId: s.failed.groupId } : null,
    error: s.error,
    sending: s.sending,
    groupSending: s.groupSending,
    messages: s.messages.map((m) => ({ seq: m.seq, text: m.text, failed: !!m.failed, pending: !!m.pending, cid: m.clientMsgId ?? '' })),
    groupMessages: s.groupMessages.map((m) => ({ seq: m.seq, text: m.text, failed: !!m.failed, pending: !!m.pending, cid: m.clientMsgId ?? '' })),
    sends: window.__sends ?? [],
  }
})

async function banner(page) {
  const el = page.locator('.hub-center-banner.hub-error').first()
  if (!(await el.count())) return null
  return {
    text: (await el.textContent()) ?? '',
    retry: await el.locator('button', { hasText: '重试' }).count(),
    dismiss: await el.locator('button[aria-label="关闭"]').count(),
  }
}

// ————————————————————————————————————————————————————————————————
// 1) DM：失败 → 重试沿用 cid → 不重复入账（HUB-IDEM-01 / HUB-FAIL-01）
// ————————————————————————————————————————————————————————————————
{
  console.log('\n[1] DM 发送失败 → 重试沿用 client_msg_id')
  const ctx = await browser.newContext({ viewport: { width: 1100, height: 900 }, locale: 'zh-CN' })
  const page = await ctx.newPage()
  const errors = []
  page.on('pageerror', (e) => errors.push(String(e)))
  try {
    await boot(page)
    await enterHub(page)
    await pickAgent(page)
    await spySends(page)
    check('DM 会话已打开', (await page.locator('.hub-dm .composer textarea').count()) > 0)

    await sendText(page, '.hub-dm', '__lostack__')
    await page.waitForTimeout(900)
    const failedState = await hubState(page)
    const b = await banner(page)
    check('失败后出现错误横幅', !!b, b?.text?.slice(0, 40))
    check('横幅带「重试」', (b?.retry ?? 0) > 0)
    check('横幅带「关闭」', (b?.dismiss ?? 0) > 0)
    check('失败气泡留在时间线', (await page.locator('.hub-dm .msg-row.user.failed').count()) === 1)
    check('失败气泡带行内重试', (await page.locator('.hub-dm .msg-row.user.failed .user-retry').count()) === 1)
    const cid1 = failedState.failed?.clientMsgId ?? ''
    check('store 记录首发 client_msg_id', cid1.startsWith('ui-'), cid1)
    check('记录的目标就是当前 DM', !!failedState.failed?.agentId, failedState.failed?.agentId ?? '')
    await page.screenshot({ path: `${OUT}/01-dm-failed.png` })

    // 点横幅「重试」：沿用同一个 cid
    await page.locator('.hub-center-banner.hub-error button', { hasText: '重试' }).first().click({ timeout: 4000 })
    await page.waitForTimeout(1100)
    const after = await hubState(page)
    const retryCall = after.sends.filter((x) => x.kind === 'dm').pop()
    check('重试沿用首发 cid', retryCall?.reuse === cid1, `${cid1} → ${retryCall?.reuse}`)
    check('failed 气泡被清掉', (await page.locator('.hub-dm .msg-row.user.failed').count()) === 0)
    check('横幅消失', (await banner(page)) === null)
    check('failed 记录清空', after.failed === null)
    check('时间线只有 1 条该消息', after.messages.filter((m) => m.text === '__lostack__').length === 1,
      `got=${after.messages.filter((m) => m.text === '__lostack__').length}`)
    await page.screenshot({ path: `${OUT}/02-dm-retried.png` })

    // 重开该 DM（重新走 hub_open 读 mock 账本）：账本里也只能有 1 条（cid 幂等）
    await pickAgent(page)
    await page.waitForTimeout(600)
    const reopened = await hubState(page)
    check('重开会话后账本仍只有 1 条（cid 幂等）', reopened.messages.filter((m) => m.text === '__lostack__').length === 1,
      `got=${reopened.messages.filter((m) => m.text === '__lostack__').length}`)

    // 关闭横幅（×）：只关提示，失败气泡与行内重试保留
    await sendText(page, '.hub-dm', '__fail__')
    await page.waitForTimeout(800)
    check('第二次失败也有横幅', (await banner(page)) !== null)
    await page.locator('.hub-center-banner.hub-error button[aria-label="关闭"]').first().click({ timeout: 4000 })
    await page.waitForTimeout(500)
    check('点 × 后横幅消失', (await banner(page)) === null)
    check('× 之后失败气泡仍在', (await page.locator('.hub-dm .msg-row.user.failed').count()) === 1)
    check('× 之后行内重试仍可用', (await page.locator('.hub-dm .msg-row.user.failed .user-retry').count()) === 1)
    await page.screenshot({ path: `${OUT}/03-dm-dismissed.png` })
    check('无 JS 异常', errors.length === 0, errors.slice(0, 2).join(' | '))
  } catch (e) {
    check('DM 失败/重试流程未抛异常', false, String(e?.message ?? e).slice(0, 200))
  }
  results.push('dm-retry')
  await ctx.close()
}

// ————————————————————————————————————————————————————————————————
// 2) 群：失败横幅 + 重试沿用 cid（HUB-FAIL-01）
// ————————————————————————————————————————————————————————————————
{
  console.log('\n[2] 群发送失败 → 横幅可重试、沿用 cid')
  const ctx = await browser.newContext({ viewport: { width: 1100, height: 900 }, locale: 'zh-CN' })
  const page = await ctx.newPage()
  try {
    await boot(page)
    await enterHub(page)
    await pickGroup(page)
    await spySends(page)
    check('群会话已打开', (await page.locator('.hub-group-view .composer textarea').count()) > 0)

    await sendText(page, '.hub-group-view', '__lostack__')
    await page.waitForTimeout(900)
    const st = await hubState(page)
    const b = await banner(page)
    check('群失败有横幅', !!b, b?.text?.slice(0, 40))
    check('群失败横幅带「重试」（旧版没有）', (b?.retry ?? 0) > 0)
    check('群失败横幅带「关闭」', (b?.dismiss ?? 0) > 0)
    const cid1 = st.failed?.clientMsgId ?? ''
    check('store 记录群失败的首发 cid', cid1.startsWith('g-'), cid1)
    check('群失败气泡标 failed', st.groupMessages.filter((m) => m.failed && m.text === '__lostack__').length === 1)
    check('群失败气泡带行内重试', (await page.locator('.hub-group-view .msg-row.user.failed .user-retry').count()) === 1)
    await page.screenshot({ path: `${OUT}/04-group-failed.png` })

    await page.locator('.hub-center-banner.hub-error button', { hasText: '重试' }).first().click({ timeout: 4000 })
    await page.waitForTimeout(1200)
    const after = await hubState(page)
    const retryCall = after.sends.filter((x) => x.kind === 'group').pop()
    check('群重试沿用首发 cid', retryCall?.reuse === cid1, `${cid1} → ${retryCall?.reuse}`)
    check('群重试后 failed 气泡被清掉', (await page.locator('.hub-group-view .msg-row.user.failed').count()) === 0)
    check('群重试后横幅消失', (await banner(page)) === null)
    check('群时间线只有 1 条该消息', after.groupMessages.filter((m) => m.text === '__lostack__').length === 1,
      `got=${after.groupMessages.filter((m) => m.text === '__lostack__').length}`)
    await page.screenshot({ path: `${OUT}/05-group-retried.png` })

    // 重开群（hub_group_get 读 mock 账本）：账本里仍只有 1 条
    await pickGroup(page)
    await page.waitForTimeout(700)
    const reopened = await hubState(page)
    check('重开群后账本仍只有 1 条（cid 幂等）', reopened.groupMessages.filter((m) => m.text === '__lostack__').length === 1,
      `got=${reopened.groupMessages.filter((m) => m.text === '__lostack__').length}`)
  } catch (e) {
    check('群失败/重试流程未抛异常', false, String(e?.message ?? e).slice(0, 200))
  }
  results.push('group-retry')
  await ctx.close()
}

// ————————————————————————————————————————————————————————————————
// 3) statusbar：640/760/900 不裁行 + 分档取舍（HUB-STATUS-01）
// ————————————————————————————————————————————————————————————————
{
  console.log('\n[3] statusbar 窄窗：scrollHeight <= clientHeight + 分档取舍')
  for (const width of [900, 760, 640]) {
    const ctx = await browser.newContext({ viewport: { width, height: 900 }, locale: 'zh-CN' })
    const page = await ctx.newPage()
    try {
      await boot(page)
      const m = await page.evaluate(() => {
        const el = document.querySelector('.statusbar')
        const seg = (cls) => {
          const n = el?.querySelector(`.${cls}`)
          return n ? getComputedStyle(n).display !== 'none' : null
        }
        return {
          clientH: el?.clientHeight ?? -1, scrollH: el?.scrollHeight ?? -1,
          clientW: el?.clientWidth ?? -1, scrollW: el?.scrollWidth ?? -1,
          docScrollW: document.documentElement.scrollWidth, vw: window.innerWidth,
          cache: seg('sb-cache'), ctx: seg('sb-ctx'), cost: seg('sb-cost'), io: seg('sb-io'),
          run: seg('sb-run'), turns: seg('sb-turns'), total: seg('sb-total'), git: seg('status-git'),
        }
      })
      check(`@${width} statusbar 不裁行（scrollHeight<=clientHeight）`, m.scrollH <= m.clientH, `${m.scrollH}/${m.clientH}`)
      check(`@${width} statusbar 无横向溢出`, m.scrollW <= m.clientW, `${m.scrollW}/${m.clientW}`)
      check(`@${width} 页面无横向溢出`, m.docScrollW === m.vw, `${m.docScrollW}/${m.vw}`)
      check(`@${width} 隐藏 cache/ctx/cost`, m.cache === false && m.ctx === false && m.cost === false)
      check(`@${width} 保留 run/turns/total/git`, m.run && m.turns && m.total && m.git)
      if (width <= 700) check('@640 隐藏 in/out', m.io === false)
      else check(`@${width} 保留 in/out`, m.io === true)
      await page.screenshot({ path: `${OUT}/06-statusbar-${width}.png` })
    } catch (e) {
      check(`@${width} statusbar 测量未抛异常`, false, String(e?.message ?? e).slice(0, 160))
    }
    await ctx.close()
  }
  // 1280 全段可见：别把宽窗也一起收掉
  {
    const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 }, locale: 'zh-CN' })
    const page = await ctx.newPage()
    try {
      await boot(page)
      const shown = await page.evaluate(() => {
        const el = document.querySelector('.statusbar')
        const seg = (cls) => {
          const n = el?.querySelector(`.${cls}`)
          return n ? getComputedStyle(n).display !== 'none' : null
        }
        return { cache: seg('sb-cache'), ctx: seg('sb-ctx'), cost: seg('sb-cost'), io: seg('sb-io') }
      })
      check('@1280 全部读数可见（回归保护）', shown.cache && shown.ctx && shown.cost && shown.io)
    } catch (e) {
      check('@1280 statusbar 测量未抛异常', false, String(e?.message ?? e).slice(0, 160))
    }
    await ctx.close()
  }
  results.push('statusbar')
}

// ————————————————————————————————————————————————————————————————
// 4) fixture：Needs / 任务板（HUB-FIXTURE-01）
// ————————————————————————————————————————————————————————————————
{
  console.log('\n[4] ?hubFixture=needs|tasks')
  for (const [fixture, width] of [['needs', 1100], ['needs', 640], ['tasks', 1100], ['tasks', 640]]) {
    const ctx = await browser.newContext({ viewport: { width, height: 900 }, locale: 'zh-CN' })
    const page = await ctx.newPage()
    try {
      await boot(page, `hubFixture=${fixture}`)
      await enterHub(page)
      await pickAgent(page)
      const tabName = fixture === 'needs' ? 'Needs' : '任务'
      const openInspector = page.locator('.hub-inspector-tabs button').filter({ hasText: tabName }).first()
      const railBtn = page.locator('.hub-detail-rail button').nth(fixture === 'needs' ? 3 : 2)
      if (await openInspector.count()) await openInspector.click({ timeout: 4000 })
      else if (await railBtn.count()) await railBtn.click({ timeout: 4000 })
      await page.waitForTimeout(900)

      if (fixture === 'needs') {
        const kinds = await page.evaluate(() => [...document.querySelectorAll('.hub-panel .hub-need')].map((n) => n.className))
        check(`@${width} Needs 面板画出 4 种 need`, kinds.length === 4, `got=${kinds.length}`)
        for (const k of ['approval', 'question', 'failure', 'handoff']) {
          check(`@${width} need 类型 ${k} 有卡片`, kinds.some((c) => c.includes(`hub-need-${k}`)))
        }
        // 提问类解析 JSON body → 两个可填输入框
        check(`@${width} 提问类有可填问题`, (await page.locator('.hub-need-questions input').count()) >= 2)
        // 审批类有批准/拒绝
        check(`@${width} 审批类有批准/拒绝`, (await page.locator('.hub-need-approval .hub-btn').count()) >= 2)
        // HUB-NEED-01：解析出输入框后不能再铺一遍原始 questions JSON（否则占掉半屏）。
        const qCard = page.locator('.hub-need-question').first()
        check(`@${width} 提问卡不再渲染原始 JSON`, (await qCard.locator('.hub-need-body').count()) === 0)
        const bodyCards = await page.locator('.hub-need-approval .hub-need-body, .hub-need-failure .hub-need-body, .hub-need-handoff .hub-need-body').count()
        check(`@${width} 审批/失败/交接仍显示 body`, bodyCards === 3, `got=${bodyCards}`)
        // 正常 payload 的 resolve 不被 mock 对齐改坏（交接接受 / 提问提交回答各走一次）
        if (width === 1100) {
          await page.locator('.hub-need-handoff .hub-btn').filter({ hasText: '接受' }).first().click({ timeout: 4000 })
          await page.waitForTimeout(700)
          check('交接「接受」后卡片消失（mock resolve 正常 payload）', (await page.locator('.hub-need-handoff').count()) === 0)
          const inputs = page.locator('.hub-need-question input')
          await inputs.nth(0).fill('写「未公开」')
          await inputs.nth(1).fill('中文')
          await page.locator('.hub-need-question .hub-btn').filter({ hasText: '提交回答' }).first().click({ timeout: 4000 })
          await page.waitForTimeout(700)
          check('提问「提交回答」后卡片消失（answers map 通过校验）', (await page.locator('.hub-need-question').count()) === 0)
        }
      } else {
        const chips = await page.evaluate(() => [...document.querySelectorAll('.hub-panel .hub-task .hub-chip')].map((n) => n.className))
        check(`@${width} 任务板有 3 条 fixture 任务`, (await page.locator('.hub-panel .hub-task').count()) >= 3,
          `got=${await page.locator('.hub-panel .hub-task').count()}`)
        for (const s of ['queued', 'failed', 'interrupted']) {
          check(`@${width} 任务态 ${s} 有卡片`, chips.some((c) => c.includes(`task-${s}`)))
        }
      }
      const overflow = await page.evaluate(() => ({ doc: document.documentElement.scrollWidth, vw: window.innerWidth }))
      check(`@${width} ${fixture} 面板无横向溢出`, overflow.doc === overflow.vw, `${overflow.doc}/${overflow.vw}`)
      await page.screenshot({ path: `${OUT}/07-fixture-${fixture}-${width}.png` })
    } catch (e) {
      check(`fixture=${fixture}@${width} 未抛异常`, false, String(e?.message ?? e).slice(0, 200))
    }
    await ctx.close()
  }
  results.push('fixtures')
}

// ————————————————————————————————————————————————————————————————
// 5) P0 复审 #1：640 进 Hub → 开 inspector → 退出 Hub，左栏恢复展开态
// ————————————————————————————————————————————————————————————————
{
  console.log('\n[5] 640 退出 Hub：左栏恢复进 Hub 前的展开态')
  const ctx = await browser.newContext({ viewport: { width: 640, height: 900 }, locale: 'zh-CN' })
  const page = await ctx.newPage()
  const left = () => page.evaluate(() => {
    const el = document.querySelector('.left')
    return {
      w: el ? Math.round(el.getBoundingClientRect().width) : -1,
      cls: el?.className ?? '',
      store: window.__store.getState().leftExpanded,
      right: window.__store.getState().rightExpanded,
      hubSide: document.querySelector('.hub-side') ? Math.round(document.querySelector('.hub-side').getBoundingClientRect().width) : null,
    }
  })
  try {
    await boot(page)
    await enterHub(page)
    await pickAgent(page)
    const inHub = await left()
    check('进 Hub 后左栏展开（Hub 导航可见）', inHub.w > 100 && inHub.store === true, `w=${inHub.w} store=${inHub.store}`)

    // 点 inspector 图标：右栏撑开、左栏被挤成 44（P0 复审 #2 的现场）
    await page.locator('.hub-detail-rail button').first().click({ timeout: 4000 })
    await page.waitForTimeout(900)
    const squeezed = await left()
    check('开 inspector 后左栏确实被挤成图标条', squeezed.w <= 60, `w=${squeezed.w}`)
    await page.screenshot({ path: `${OUT}/08-640-inspector-open.png` })

    // P0 复审 #2：Hub 导航不能被「一去不回」—— 关掉 inspector 就要能找回左栏
    const closeInsp = page.locator('.hub-inspector-close').first()
    if (await closeInsp.count()) { await closeInsp.click({ timeout: 4000 }); await page.waitForTimeout(900) }
    const recovered = await left()
    check('关掉 inspector 后左栏恢复（Hub 导航可找回）', recovered.w > 100, `w=${recovered.w}`)
    check('恢复后 .hub-side 有真实宽度', (recovered.hubSide ?? 0) > 100, `${recovered.hubSide}px`)
    await page.screenshot({ path: `${OUT}/08b-640-inspector-closed.png` })
    // 再开一次 inspector，走退出 Hub 的路径
    await page.locator('.hub-detail-rail button').first().click({ timeout: 4000 })
    await page.waitForTimeout(900)

    // 从 rail 的「工作区」退出 Hub（审计路径）
    await page.locator('button[aria-label="工作区"]').first().click({ timeout: 5000 })
    await page.waitForTimeout(1300)
    const after = await left()
    check('退出 Hub 后左栏恢复展开', after.w > 100, `w=${after.w}`)
    check('左栏 class 与真实宽度一致（expanded）', after.cls.includes('expanded') && !after.cls.includes('collapsed'), after.cls)
    check('store leftExpanded 与真实状态一致', after.store === true, String(after.store))
    check('退出后右栏回到进 Hub 前的收起态', after.right === false, String(after.right))
    await page.screenshot({ path: `${OUT}/09-640-after-exit.png` })
  } catch (e) {
    check('640 退出 Hub 流程未抛异常', false, String(e?.message ?? e).slice(0, 200))
  }
  results.push('exit-restore')
  await ctx.close()
}

// ————————————————————————————————————————————————————————————————
// 6) P0 复审 #4：Hub 打开期间跨 1000px 阈值重算右栏
// ————————————————————————————————————————————————————————————————
{
  console.log('\n[6] Hub 打开期间 640→1280→640：右栏按阈值重算、左栏不丢')
  const ctx = await browser.newContext({ viewport: { width: 640, height: 900 }, locale: 'zh-CN' })
  const page = await ctx.newPage()
  const snap = () => page.evaluate(() => {
    const w = (s) => { const el = document.querySelector(s); return el ? Math.round(el.getBoundingClientRect().width) : 0 }
    return {
      left: w('.left'), right: w('.hub-inspector') || w('.hub-detail-rail'), center: w('.hub-center'),
      leftStore: window.__store.getState().leftExpanded, rightStore: window.__store.getState().rightExpanded,
      doc: document.documentElement.scrollWidth, vw: window.innerWidth,
    }
  })
  try {
    await boot(page)
    await enterHub(page)
    await pickAgent(page)
    const a = await snap()
    check('640 Hub：inspector 是图标条', a.right <= 48 && a.rightStore === false, `${a.right}px`)

    await page.setViewportSize({ width: 1280, height: 900 })
    await page.waitForTimeout(1200)
    const b = await snap()
    check('1280：inspector 按 ≥1000 展开（≥300px）', b.right >= 300, `${b.right}px`)
    check('1280：inspector 展开到宽窗默认 ~360px', b.right >= 320 && b.right <= 420, `${b.right}px`)
    check('1280：右栏 store 与真实一致', b.rightStore === true)
    check('1280：左栏仍展开（没被挤成 44）', b.left > 100, `${b.left}px`)
    check('1280：会话列仍 ≥300', b.center >= 300, `${b.center}px`)
    check('1280：无横向溢出', b.doc === b.vw, `${b.doc}/${b.vw}`)
    await page.screenshot({ path: `${OUT}/10-resize-1280.png` })

    await page.setViewportSize({ width: 640, height: 900 })
    await page.waitForTimeout(1200)
    const c = await snap()
    check('缩回 640：inspector 回到图标条', c.right <= 48, `${c.right}px`)
    check('缩回 640：左栏仍展开（Hub 导航没消失）', c.left > 100, `${c.left}px`)
    check('缩回 640：无横向溢出', c.doc === c.vw, `${c.doc}/${c.vw}`)
    await page.screenshot({ path: `${OUT}/11-resize-back-640.png` })

    // 「别打断用户手动折叠」：宽窗里手动收起右栏后，再跨阈值也不应被自动顶开
    await page.setViewportSize({ width: 1280, height: 900 })
    await page.waitForTimeout(1000)
    const closeBtn = page.locator('.hub-inspector-close').first()
    if (await closeBtn.count()) { await closeBtn.click({ timeout: 4000 }); await page.waitForTimeout(700) }
    await page.setViewportSize({ width: 800, height: 900 })
    await page.waitForTimeout(500)
    await page.setViewportSize({ width: 1280, height: 900 })
    await page.waitForTimeout(1100)
    const d = await snap()
    check('用户手动收起后跨阈值不被自动顶开', d.rightStore === false && d.right <= 48, `${d.right}px store=${d.rightStore}`)
  } catch (e) {
    check('Hub 打开期间 resize 流程未抛异常', false, String(e?.message ?? e).slice(0, 200))
  }
  results.push('resize-threshold')
  await ctx.close()
}

// ————————————————————————————————————————————————————————————————
// 7) P1 复审 B1/B2/B3（真浏览器复现路径）
//   B1：DM A/B 两条失败，点 A 的行内重试 → wire 上必须是 A 的正文 + A 的 cid
//   B2：失败 → 横幅重试成功 → textarea 空 + 账本同文只有 1 条（DM 与群）
//   B3：失败 A → 成功 B（不能顺手清掉 A 的 cid）→ 重发 A 仍用 A 的 cid、账本 A=1
// 状态推进一律用 waitForFunction 等 store/DOM 的真实变化，不靠固定 sleep。
// ————————————————————————————————————————————————————————————————
async function waitState(page, fn, label, arg, timeout = 8000) {
  try {
    await page.waitForFunction(fn, arg, { timeout, polling: 100 })
    return true
  } catch {
    check(`等待状态超时：${label}`, false, label)
    return false
  }
}

{
  console.log('\n[7a] 复审 B1：两条失败，点 A 的行内重试 → wire 上是 A')
  const ctx = await browser.newContext({ viewport: { width: 1100, height: 900 }, locale: 'zh-CN' })
  const page = await ctx.newPage()
  try {
    await boot(page)
    await enterHub(page)
    await pickAgent(page)
    await spySends(page)
    await sendText(page, '.hub-dm', '__lostack__')
    await waitState(page, () => window.__hub.getState().messages.filter((m) => m.failed).length === 1, 'A 失败气泡出现')
    await sendText(page, '.hub-dm', '__fail__')
    await waitState(page, () => {
      const s = window.__hub.getState()
      return s.messages.filter((m) => m.failed).length === 2 && Object.keys(s.failedByCid).length === 2
    }, 'A/B 两条失败气泡 + 两条失败记录')
    const two = await page.evaluate(() => {
      const s = window.__hub.getState()
      return {
        cids: Object.keys(s.failedByCid),
        failed: s.messages.filter((m) => m.failed).map((m) => ({ text: m.text, cid: m.clientMsgId })),
      }
    })
    check('B1：失败表里两条 cid 都在（不再单槽覆盖）', two.cids.length === 2, two.cids.join(' , '))
    check('B1：两条失败气泡各自带 cid', two.failed.length === 2 && two.failed.every((m) => !!m.cid))
    check('B1：两条失败气泡都有行内重试', (await page.locator('.hub-dm .msg-row.user.failed .user-retry').count()) === 2)
    const cidA = two.failed.find((m) => m.text === '__lostack__')?.cid ?? ''
    const cidB = two.failed.find((m) => m.text === '__fail__')?.cid ?? ''
    check('B1：A/B 的 cid 不同', !!cidA && !!cidB && cidA !== cidB, `${cidA} / ${cidB}`)
    await page.screenshot({ path: `${OUT}/12-b1-two-failures.png` })

    const n0 = await page.evaluate(() => (window.__sends ?? []).length)
    // 点第一条（A = __lostack__）的重试
    await page.locator('.hub-dm .msg-row.user.failed .user-retry').first().click({ timeout: 4000 })
    await waitState(page, (start) => (window.__sends ?? []).slice(start).some((x) => x.kind === 'dm' && x.text === '__lostack__'),
      'A 的重试真正上线', n0)
    const retried = await page.evaluate((start) => (window.__sends ?? []).slice(start).find((x) => x.kind === 'dm'), n0)
    check('B1：wire 正文是 A（不是最新失败的 B）', retried?.text === '__lostack__', retried?.text ?? '(none)')
    check('B1：wire 复用 A 的首发 cid', retried?.reuse === cidA, `${cidA} → ${retried?.reuse ?? ''}`)
    await waitState(page, () => {
      const s = window.__hub.getState()
      const left = s.messages.filter((m) => m.failed)
      return left.length === 1 && left[0].text === '__fail__' && !s.sending
    }, 'A 重试成功、只剩 B 失败')
    check('B1：B 的失败气泡还在且仍可重试',
      (await page.locator('.hub-dm .msg-row.user.failed .user-retry').count()) === 1)
    const afterA = await page.evaluate(() => {
      const s = window.__hub.getState()
      return { byCid: Object.keys(s.failedByCid), banner: !!s.failed, bannerCid: s.failed?.clientMsgId ?? '' }
    })
    check('B1：失败表只剩 B 的 cid', afterA.byCid.length === 1 && afterA.byCid[0] === cidB, afterA.byCid.join(' , '))
    check('B1：横幅改指向还没重试的 B', afterA.bannerCid === cidB, afterA.bannerCid)
    await page.screenshot({ path: `${OUT}/12b-b1-after-retry-a.png` })
  } catch (e) {
    check('B1 流程未抛异常', false, String(e?.message ?? e).slice(0, 200))
  }
  results.push('review-b1')
  await ctx.close()
}

{
  console.log('\n[7b] 复审 B2：失败草稿 → 横幅重试成功 → 输入框空、账本同文 1 条')
  const ctx = await browser.newContext({ viewport: { width: 1100, height: 900 }, locale: 'zh-CN' })
  const page = await ctx.newPage()
  try {
    await boot(page)
    await enterHub(page)
    await pickAgent(page)
    await sendText(page, '.hub-dm', '__lostack__')
    await waitState(page, () => !!window.__hub.getState().failed && !window.__hub.getState().sending, 'DM 失败记录')
    check('B2：失败后草稿仍在输入框（失败不清草稿）',
      (await page.inputValue('.hub-dm .composer textarea')) === '__lostack__')
    await page.locator('.hub-center-banner.hub-error button', { hasText: '重试' }).first().click({ timeout: 4000 })
    await waitState(page, () => {
      const s = window.__hub.getState()
      return !s.failed && !s.sending && s.messages.filter((m) => m.failed).length === 0
    }, 'DM 横幅重试成功')
    await waitState(page, () => (document.querySelector('.hub-dm .composer textarea')?.value ?? 'x') === '', 'DM 输入框清空')
    check('B2：重试成功后 textarea 为空', (await page.inputValue('.hub-dm .composer textarea')) === '')
    // 再点一次发送（旧 bug：同文换新 cid → 账本 2 条）。清空后按钮禁用，先确认它确实空。
    check('B2：空草稿时发送按钮不可点', await page.locator('.hub-dm .composer .send-btn').first().isDisabled())
    await pickAgent(page)
    await waitState(page, () => window.__hub.getState().messages.length > 0, '重开 DM 读账本')
    const reopened = await hubState(page)
    check('B2：账本里这条正文只有 1 条（重试没二次入账）',
      reopened.messages.filter((m) => m.text === '__lostack__').length === 1,
      `got=${reopened.messages.filter((m) => m.text === '__lostack__').length}`)
    await page.screenshot({ path: `${OUT}/13-b2-dm-draft-cleared.png` })
  } catch (e) {
    check('B2(DM) 流程未抛异常', false, String(e?.message ?? e).slice(0, 200))
  }
  await ctx.close()

  const gctx = await browser.newContext({ viewport: { width: 1100, height: 900 }, locale: 'zh-CN' })
  const gpage = await gctx.newPage()
  try {
    await boot(gpage)
    await enterHub(gpage)
    await pickGroup(gpage)
    await sendText(gpage, '.hub-group-view', '__lostack__')
    await waitState(gpage, () => !!window.__hub.getState().failed && !window.__hub.getState().groupSending, '群失败记录')
    check('B2：群失败后草稿仍在输入框',
      (await gpage.inputValue('.hub-group-view .composer textarea')) === '__lostack__')
    await gpage.locator('.hub-center-banner.hub-error button', { hasText: '重试' }).first().click({ timeout: 4000 })
    await waitState(gpage, () => {
      const s = window.__hub.getState()
      return !s.failed && !s.groupSending && s.groupMessages.filter((m) => m.failed).length === 0
    }, '群横幅重试成功')
    await waitState(gpage, () => (document.querySelector('.hub-group-view .composer textarea')?.value ?? 'x') === '', '群输入框清空')
    check('B2：群重试成功后 textarea 为空', (await gpage.inputValue('.hub-group-view .composer textarea')) === '')
    await pickGroup(gpage)
    await waitState(gpage, () => window.__hub.getState().groupMessages.length > 0, '重开群读账本')
    const reopened = await hubState(gpage)
    check('B2：群账本里这条正文只有 1 条',
      reopened.groupMessages.filter((m) => m.text === '__lostack__').length === 1,
      `got=${reopened.groupMessages.filter((m) => m.text === '__lostack__').length}`)
    await gpage.screenshot({ path: `${OUT}/13b-b2-group-draft-cleared.png` })
  } catch (e) {
    check('B2(群) 流程未抛异常', false, String(e?.message ?? e).slice(0, 200))
  }
  await gctx.close()
  results.push('review-b2')
}

{
  console.log('\n[7c] 复审 B3：失败 A → 成功 B → 重发 A 仍用 A 的 cid、账本 A=1')
  const ctx = await browser.newContext({ viewport: { width: 1100, height: 900 }, locale: 'zh-CN' })
  const page = await ctx.newPage()
  try {
    await boot(page)
    await enterHub(page)
    await pickAgent(page)
    await spySends(page)
    await sendText(page, '.hub-dm', '__lostack__')
    await waitState(page, () => !!window.__hub.getState().failed && !window.__hub.getState().sending, 'A 失败')
    const cidA = await page.evaluate(() => window.__hub.getState().failed?.clientMsgId ?? '')
    check('B3：A 的首发 cid 记下来了', /^ui-/.test(cidA), cidA)

    await sendText(page, '.hub-dm', 'B3 中间那条成功的')
    await waitState(page, () => {
      const s = window.__hub.getState()
      return !s.sending && s.messages.some((m) => m.text === 'B3 中间那条成功的' && !m.failed && !m.pending)
    }, 'B 发送成功')
    const afterB = await page.evaluate(() => {
      const s = window.__hub.getState()
      return { bannerCid: s.failed?.clientMsgId ?? '', byCid: Object.keys(s.failedByCid) }
    })
    check('B3：成功一条不会清掉 A 的失败记录（旧 bug 是无条件 failed:null）', afterB.byCid.includes(cidA), afterB.byCid.join(' , '))
    check('B3：横幅仍指向 A', afterB.bannerCid === cidA, afterB.bannerCid)

    // 用户直接在 composer 把 A 的原文再发一次（不点重试）
    const n0 = await page.evaluate(() => (window.__sends ?? []).length)
    await sendText(page, '.hub-dm', '__lostack__')
    await waitState(page, (start) => (window.__sends ?? []).slice(start).some((x) => x.kind === 'dm' && x.text === '__lostack__'),
      '重发 A 上线', n0)
    const resent = await page.evaluate((start) => (window.__sends ?? []).slice(start).find((x) => x.kind === 'dm'), n0)
    check('B3：重发 A 确实走 composer 的 send（正文没换）', resent?.text === '__lostack__', resent?.text ?? '(none)')
    await waitState(page, () => {
      const s = window.__hub.getState()
      return !s.sending && s.messages.filter((m) => m.failed).length === 0
    }, '重发 A 成功')
    // reuse 是 store 内部按失败表匹配出来的，不经过 UI 入参；时间线气泡上的 cid 就是实际发出去的键。
    const resentCid = await page.evaluate(() => window.__hub.getState().messages.find((m) => m.text === '__lostack__')?.clientMsgId ?? '')
    check('B3：重发 A 实际用的仍是首发 cid（旧 bug：单槽已清 → 换新 cid → 账本 +1）', resentCid === cidA,
      `${cidA} → ${resentCid}`)
    await pickAgent(page)
    await waitState(page, () => window.__hub.getState().messages.length > 0, '重开 DM 读账本')
    const reopened = await hubState(page)
    check('B3：账本里 A 只有 1 条', reopened.messages.filter((m) => m.text === '__lostack__').length === 1,
      `got=${reopened.messages.filter((m) => m.text === '__lostack__').length}`)
    check('B3：账本里 B 也只有 1 条', reopened.messages.filter((m) => m.text === 'B3 中间那条成功的').length === 1,
      `got=${reopened.messages.filter((m) => m.text === 'B3 中间那条成功的').length}`)
    await page.screenshot({ path: `${OUT}/14-b3-resent-a.png` })
  } catch (e) {
    check('B3 流程未抛异常', false, String(e?.message ?? e).slice(0, 200))
  }
  results.push('review-b3')
  await ctx.close()
}

await browser.close()
if (failures > 0) {
  console.error(`\n断言失败 ${failures} 条（场景：${results.join(', ')}）`)
  process.exit(1)
}
console.log(`\n全部断言通过（场景：${results.join(', ')}）\n截图：${OUT}`)
