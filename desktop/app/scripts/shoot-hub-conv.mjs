#!/usr/bin/env node
// 会话区（群 / DM）真浏览器截图 + DOM 断言。
//
// 用法：先起 dev server（不要占 5173-5175），再跑
//   VITE_PORT=5177 npx vite
//   node scripts/shoot-hub-conv.mjs [--url http://localhost:5177] [--out /tmp/hub-conv-shots]
//
// 断言（任一失败 → 退出码 1）：
//   · documentElement.scrollWidth <= 视口宽（0 横向溢出）
//   · 用户气泡右缘留 35px（24px owner 头像 + 11px gap），头像贴会话列右缘
//   · 桌面：agent 正文左移 63px（52px Luna + 11px gap）；窄屏 60px（52 + 8）
//   · 会话列 760px；宽屏 padding 30/28，窄屏 16/12
//   · 交接/审批卡真实渲染（?hubFixture=cards），各有专属 token 样式
//   · composer meta 行存在
//   · 无 console error / pageerror
//
// 不造假数据：截图全部来自 mock 的真实 fixture（2 人群 / 两条 DM）；
// 交接/审批卡也只注入两种真实 kind，且仅在显式 query 下出现。
import { chromium } from 'playwright'
import { mkdirSync } from 'node:fs'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const URL = arg('url', process.env.HUB_CONV_URL || 'http://localhost:5177/')
const OUT = arg('out', '/tmp/hub-conv-shots')
const CARDS_URL = (() => {
  const u = new globalThis.URL(URL)
  u.searchParams.set('hubFixture', 'cards')
  return u.toString()
})()
const VIEWPORTS = [
  [1440, 900],
  [1280, 900],
  [640, 900],
  [390, 900],
]
const THEMES = ['ink', 'paper']

mkdirSync(OUT, { recursive: true })
const browser = await chromium.launch()
const errors = []
const rows = []

async function openConversation(page, kind) {
  if (kind === 'writer') {
    if (!(await page.locator('.hub-side-row').count())) {
      await page.locator('button[aria-label="智能体"]').first().click({ timeout: 8000 }).catch(() => {})
      await page.waitForTimeout(600)
    }
    await page.locator('.hub-side-row').filter({ hasText: 'Writer' }).first().click({ timeout: 8000 })
    await page.waitForTimeout(700)
    return
  }
  if (!(await page.locator('.hub-side-row').count())) {
    await page.locator('button[aria-label="智能体"]').first().click({ timeout: 8000 }).catch(() => {})
    await page.waitForTimeout(600)
  }
  await page.locator('.hub-side-row').first().waitFor({ timeout: 10000 })
  if (kind === 'group') {
    const tab = page.locator('.hub-side-tabs button').filter({ hasText: '群组' }).first()
    if ((await tab.getAttribute('aria-selected')) !== 'true') await tab.click({ timeout: 6000 })
    await page.waitForTimeout(250)
    await page.locator('.hub-side-row').first().click({ timeout: 8000 })
  } else {
    const tab = page.locator('.hub-side-tabs button').filter({ hasText: '智能体' }).first()
    if (await tab.count()) {
      if ((await tab.getAttribute('aria-selected')) !== 'true') await tab.click()
      await page.waitForTimeout(250)
    }
    await page.locator('.hub-side-row').filter({ hasText: 'Researcher' }).first().click({ timeout: 8000 })
  }
  await page.waitForTimeout(700)
  // 窄屏：左栏是抽屉，选完收起；右栏只保留图标条（聚焦会话区截图）。
  const collapseLeft = page.locator('button[aria-label="左栏收缩 / ⌘B"]').first()
  if (await collapseLeft.count() && await page.locator('.left.expanded').count()) {
    await collapseLeft.click({ timeout: 4000 }).catch(() => {})
    await page.waitForTimeout(300)
  }
  await page.evaluate(() => {
    const store = window.__store
    store?.getState?.()?.setRightExpanded?.(false)
  })
  await page.waitForTimeout(250)
}

async function shot(width, height, theme, kind) {
  const name = `${width}-${kind}-${theme}`
  const page = await browser.newPage({ viewport: { width, height } })
  page.on('console', (m) => {
    if (m.type() === 'error') errors.push(`${name}: ${m.text()}`)
  })
  page.on('pageerror', (e) => errors.push(`${name}: pageerror ${e.message}`))
  await page.goto(URL, { waitUntil: 'domcontentloaded' })
  await page.waitForTimeout(1500)
  await page.evaluate((t) => {
    window.__store?.getState?.().setTheme?.(t === 'paper' ? 'light' : 'dark')
  }, theme)
  await page.waitForTimeout(350)
  await openConversation(page, kind)
  await page.evaluate(() => {
    const list = document.querySelector('.hub-group-view .messages, .hub-dm .messages')
    if (list) list.scrollTop = 0
  })
  await page.waitForTimeout(250)

  const m = await page.evaluate(() => {
    const q = (s) => document.querySelector(s)
    const rect = (el) => {
      if (!el) return null
      const b = el.getBoundingClientRect()
      return { left: Math.round(b.left), right: Math.round(b.right), width: Math.round(b.width) }
    }
    const thread = rect(q('.messages-inner'))
    const list = q('.messages')
    const listStyle = list ? getComputedStyle(list) : null
    const bubble = rect(q('.msg-row.user .user-bubble'))
    const ownerAvatar = rect(q('.msg-row.user .owner-avatar'))
    const agent = q('.msg-row.agent')
    const agentMain = rect(q('.msg-row.agent .msg-main'))
    const dmSigil = q('.hub-dm .chat-header > .avatar')
    const groupStack = q('.hub-group-view .chat-header > .avatar-stack')
    const title = q('.chat-header .chat-title .name')?.textContent?.trim() ?? ''
    const header = q('.chat-header')
    return {
      themeAttr: document.documentElement.dataset.theme,
      scrollWidth: document.documentElement.scrollWidth,
      dmSigil: dmSigil
        ? {
            role: dmSigil.getAttribute('role'),
            label: dmSigil.getAttribute('aria-label'),
            width: Math.round(dmSigil.getBoundingClientRect().width),
          }
        : null,
      groupStack: groupStack
        ? {
            role: groupStack.getAttribute('role'),
            label: groupStack.getAttribute('aria-label'),
            width: Math.round(groupStack.getBoundingClientRect().width),
            title,
          }
        : null,
      thread,
      bubble,
      bubbleRightGap: bubble && thread ? Math.round(thread.right - bubble.right) : null,
      ownerAvatar,
      ownerAvatarGap: bubble && ownerAvatar ? Math.round(ownerAvatar.left - bubble.right) : null,
      ownerAvatarRightGap: ownerAvatar && thread ? Math.round(thread.right - ownerAvatar.right) : null,
      agentOffset: agentMain && thread ? Math.round(agentMain.left - thread.left) : null,
      agentBackground: agent ? getComputedStyle(agent).backgroundColor : null,
      agentAuthor: q('.msg-row.agent .agent-name')?.textContent ?? null,
      composerMeta: q('.hub-group-view .composer-meta, .hub-dm .composer-meta')?.innerText.replace(/\n/g, ' | ') ?? null,
      headerHeight: Math.round(header?.getBoundingClientRect().height ?? 0),
      headerSubCount: header ? header.querySelectorAll('.chat-sub').length : 0,
      headerSubtitle: header?.querySelector('.chat-sub')?.textContent?.trim() ?? null,
      listPadding: listStyle
        ? { top: listStyle.paddingTop, left: listStyle.paddingLeft }
        : null,
      dateDivider: q('.date-divider')?.textContent ?? null,
    }
  })
  const failed = []
  if (m.scrollWidth > width) failed.push(`横向溢出 ${m.scrollWidth} > ${width}`)
  const narrow = width <= 640
  const wantBubbleGap = narrow ? 32 : 35
  const wantOwnerGap = narrow ? 8 : 11
  const wantAgentOffset = narrow ? 60 : 63
  if (m.bubbleRightGap !== wantBubbleGap) failed.push(`用户气泡右缘应留 ${wantBubbleGap}px（gap=${m.bubbleRightGap}）`)
  if (m.ownerAvatar?.width !== 24) failed.push(`owner 头像应为 24px（${m.ownerAvatar?.width}）`)
  if (m.ownerAvatarGap !== wantOwnerGap) failed.push(`owner 头像与气泡 gap 应为 ${wantOwnerGap}（${m.ownerAvatarGap}）`)
  if (m.ownerAvatarRightGap !== 0) failed.push(`owner 头像未贴齐会话列右缘（${m.ownerAvatarRightGap}）`)
  if (m.agentOffset !== wantAgentOffset) failed.push(`agent 正文左移应为 ${wantAgentOffset}（${m.agentOffset}）`)
  if (m.agentBackground !== 'rgba(0, 0, 0, 0)') failed.push(`agent 消息有气泡底色（${m.agentBackground}）`)
  if (!m.agentAuthor) failed.push('agent 消息缺少作者头')
  if (!m.composerMeta) failed.push('composer 缺少 meta 行')
  if (m.themeAttr !== (theme === 'paper' ? 'paper' : 'ink')) {
    failed.push(`主题未生效（data-theme=${m.themeAttr}）`)
  }
  const wide = width >= 1280
  if (wide && m.thread?.width !== 760) failed.push(`会话列应为 760px（${m.thread?.width}）`)
  const wantPadding = width > 640
    ? { top: '30px', left: '28px' }
    : { top: '16px', left: '12px' }
  if (m.listPadding?.top !== wantPadding.top || m.listPadding?.left !== wantPadding.left) {
    failed.push(`会话区 padding=${m.listPadding?.top}/${m.listPadding?.left}（应为 ${wantPadding.top}/${wantPadding.left}）`)
  }
  if (wide && m.headerHeight > 68) failed.push(`群/DM 头应 ≤68px（${m.headerHeight}）`)
  if (kind === 'dm') {
    if (m.headerSubCount !== 1) failed.push(`DM 头应为 2 行（sub=${m.headerSubCount}）`)
    if (!m.dmSigil) failed.push('DM header 缺少头像')
    else {
      if (m.dmSigil.role !== 'img') failed.push(`DM header sigil role=${m.dmSigil.role}（应为 img）`)
      if (m.dmSigil.label && !m.dmSigil.label.includes('Researcher')) failed.push(`DM header avatar aria-label=${m.dmSigil.label}`)
      if (m.dmSigil.width !== 44) failed.push(`DM header avatar=${m.dmSigil.width}px（应为 44）`)
    }
  }
  if (kind === 'group') {
    if (m.headerSubCount !== 1) failed.push(`群头 subtitle 应为 1 行（sub=${m.headerSubCount}）`)
    if (!m.headerSubtitle?.includes('·')) failed.push(`群头 subtitle 缺少分隔信息：${m.headerSubtitle}`)
    if (!m.groupStack) failed.push('群头缺少 AgentStack')
    else {
      if (m.groupStack.role !== 'img') failed.push(`群叠放 role=${m.groupStack.role}（应为 img）`)
      if (!m.groupStack.label?.includes(m.groupStack.title)) failed.push(`群叠放 aria-label 不含群名：${m.groupStack.label}`)
    }
  }
  if (failed.length) errors.push(`${name}: ${failed.join('；')}`)

  await page.screenshot({ path: `${OUT}/${name}.png` })
  rows.push({ name, ...m, failed })
  await page.close()
}

for (const [width, height] of VIEWPORTS) {
  for (const theme of THEMES) {
    for (const kind of ['group', 'dm']) {
      await shot(width, height, theme, kind)
    }
  }
}
// 有任务卡的群聊：滚到任务卡，单独一张（1440/390 × ink/paper）。
for (const [width, height] of [[1440, 900], [390, 900]]) {
  for (const theme of THEMES) {
    const page = await browser.newPage({ viewport: { width, height } })
    const name = `${width}-group-task-${theme}`
    page.on('console', (m) => { if (m.type() === 'error') errors.push(`${name}: ${m.text()}`) })
    page.on('pageerror', (e) => errors.push(`${name}: pageerror ${e.message}`))
    await page.goto(URL, { waitUntil: 'domcontentloaded' })
    await page.waitForTimeout(1500)
    await page.evaluate((t) => {
      window.__store?.getState?.().setTheme?.(t === 'paper' ? 'light' : 'dark')
    }, theme)
    await page.waitForTimeout(350)
    await openConversation(page, 'group')
    await page.evaluate(() => {
      document.querySelector('.event-card--task')?.scrollIntoView({ block: 'center' })
    })
    await page.waitForTimeout(350)
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth)
    if (overflow > width) errors.push(`${name}: 横向溢出 ${overflow} > ${width}`)
    await page.screenshot({ path: `${OUT}/${name}.png` })
    rows.push({ name, scrollWidth: overflow })
    await page.close()
  }
}
// 交接 / 审批卡：mock 的 ?hubFixture=cards 注入两种真实 kind，必须真的画出来，
// 且 handoff 用 accent 图标、approval 用 warn 图标 + 暖边框 + warn 标题。
for (const theme of THEMES) {
  const width = 1440
  const page = await browser.newPage({ viewport: { width, height: 900 } })
  const name = `${width}-group-cards-${theme}`
  page.on('console', (m) => { if (m.type() === 'error') errors.push(`${name}: ${m.text()}`) })
  page.on('pageerror', (e) => errors.push(`${name}: pageerror ${e.message}`))
  await page.goto(CARDS_URL, { waitUntil: 'domcontentloaded' })
  await page.waitForTimeout(1500)
  await page.evaluate((t) => {
    window.__store?.getState?.().setTheme?.(t === 'paper' ? 'light' : 'dark')
  }, theme)
  await page.waitForTimeout(350)
  await openConversation(page, 'group')
  await page.evaluate(() => {
    document.querySelector('.event-card--approval')?.scrollIntoView({ block: 'center' })
  })
  await page.waitForTimeout(350)
  const m = await page.evaluate(() => {
    const q = (s) => document.querySelector(s)
    const color = (s) => (q(s) ? getComputedStyle(q(s)).color : null)
    const inner = q('.messages-inner')?.getBoundingClientRect()
    const leftOf = (s) => {
      const el = q(s)
      return el && inner ? Math.round(el.getBoundingClientRect().left - inner.left) : null
    }
    const body = q('.event-card--task .event-body')
    const actions = q('.event-card--task .event-actions')
    return {
      cardLeft: leftOf('.event-card--task'),
      handoffLeft: leftOf('.event-card--handoff'),
      approvalLeft: leftOf('.event-card--approval'),
      cardBody: body ? { ml: getComputedStyle(body).marginLeft, fs: getComputedStyle(body).fontSize } : null,
      cardActions: actions ? { ml: getComputedStyle(actions).marginLeft, gap: getComputedStyle(actions).gap } : null,
      handoffCount: document.querySelectorAll('.event-card--handoff').length,
      approvalCount: document.querySelectorAll('.event-card--approval').length,
      handoffText: q('.event-card--handoff')?.textContent?.trim() ?? '',
      approvalText: q('.event-card--approval')?.textContent?.trim() ?? '',
      handoffIcon: color('.event-card--handoff .event-icon'),
      approvalIcon: color('.event-card--approval .event-icon'),
      approvalBorder: q('.event-card--approval') ? getComputedStyle(q('.event-card--approval')).borderTopColor : null,
      taskBorder: q('.event-card--task') ? getComputedStyle(q('.event-card--task')).borderTopColor : null,
      approvalTitle: color('.event-card--approval .event-title'),
      taskTitle: color('.event-card--task .event-title'),
      scrollWidth: document.documentElement.scrollWidth,
    }
  })
  const failed = []
  const wantCardLeft = width <= 640 ? 38 : 41
  if (m.cardLeft !== wantCardLeft || m.handoffLeft !== wantCardLeft || m.approvalLeft !== wantCardLeft) {
    failed.push(`事件卡左缘应=${wantCardLeft}（task=${m.cardLeft} handoff=${m.handoffLeft} approval=${m.approvalLeft}）`)
  }
  if (m.cardBody?.ml !== '38px' || m.cardBody?.fs !== '11.5px') failed.push(`event-body=${m.cardBody?.ml}/${m.cardBody?.fs}（应为 38px/11.5px）`)
  if (m.cardActions?.ml !== '38px' || m.cardActions?.gap !== '6px') failed.push(`event-actions=${m.cardActions?.ml}/${m.cardActions?.gap}（应为 38px/6px）`)
  if (m.handoffCount !== 1) failed.push(`handoff 卡数量=${m.handoffCount}`)
  if (m.approvalCount !== 1) failed.push(`approval 卡数量=${m.approvalCount}`)
  if (m.handoffIcon === m.approvalIcon) failed.push('handoff/approval 图标颜色未区分')
  if (m.approvalBorder === m.taskBorder) failed.push('approval 暖边框未生效')
  if (m.approvalTitle === m.taskTitle) failed.push('approval warn 标题未生效')
  if (m.scrollWidth > width) failed.push(`横向溢出 ${m.scrollWidth} > ${width}`)
  if (failed.length) errors.push(`${name}: ${failed.join('；')}`)
  await page.screenshot({ path: `${OUT}/${name}.png` })
  rows.push({ name, ...m, failed })
  await page.close()
}

// 次要路径：Writer 的 DM（同版式、不同身份色/模型）。
await shot(1440, 900, 'ink', 'writer')

await browser.close()
console.log(JSON.stringify({ out: OUT, rows, errors }, null, 2))
if (errors.length) {
  console.error(`\nHUB_CONV_SHOTS_FAIL (${errors.length})`)
  process.exit(1)
}
console.log(`\nHUB_CONV_SHOTS_PASS → ${OUT}`)
