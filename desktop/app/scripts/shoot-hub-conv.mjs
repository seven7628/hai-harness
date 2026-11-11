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
//   · 正文 Markdown 的空白处理是 normal（agent-text 的 pre-wrap 不再把 \n 画成空行），
//     且每个段落的高度都是 line-height 的整数倍（没有多出来的空行）
//   · 触发上下文是「N 人触发」可读文案，不再画「↳ N」；字号/颜色按 demo 的小字口径
//     （10.5px + --text-3 压向 --ink），且在消息 footer / 任务卡 footer 里
//   · 作者名 = demo 的 .agent-name（12.5px/600），时间与名字间距 6px
//   · 正文 md 首尾块不吃外边距（h2 开头 0、列表结尾 0），段距仍是 8px
//   · 窄屏表格保留 440px 可读列宽 → 单元格单行，横滑只在 .md-table-wrap 里（0 文档级溢出）
//   · 三张事件卡正文左缘与各自事件标题左缘对齐（≤2px），event-flow 的 .agent 内 gap 4
//   · 无 context 的卡不画空 footer（handoff / approval 没有 .event-actions）
//   · 头像身份色跟随 agent.accent（Researcher = #7bc199），正文 @ 提及带 16px Luna
//   · handoff / approval 卡有 .event-flow 头像，task 卡 .event-who 有 16px 头像
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

// 排版细节的测量（在页面里跑，shot 与 cards 共用）：作者名 / 时间 / 正文首尾 / 表格。
function typeDetailProbe() {
  const q = (s) => document.querySelector(s)
  return {
    // 作者名与时间：demo 是 .agent-name 12.5px/600、.msg-time margin-left 6px。
    nameStyle: (() => {
      const el = q('.msg-row.agent .agent-name')
      if (!el) return null
      const st = getComputedStyle(el)
      return { fontSize: st.fontSize, fontWeight: st.fontWeight }
    })(),
    msgTimeStyle: (() => {
      const el = q('.msg-row.agent .msg-time')
      if (!el) return null
      return { marginLeft: getComputedStyle(el).marginLeft, fontSize: getComputedStyle(el).fontSize }
    })(),
    // 正文首尾块不吃外边距：demo 的 agent-text 是紧贴头部的单块排版。
    mdRhythm: [...document.querySelectorAll('.agent-text .md')].map((md) => {
      const first = md.firstElementChild
      const last = md.lastElementChild
      return {
        first: first?.tagName ?? null,
        firstMt: first ? getComputedStyle(first).marginTop : null,
        last: last?.tagName ?? null,
        lastMb: last ? getComputedStyle(last).marginBottom : null,
      }
    }),
    // 任务卡的「N 人触发」必须落在 footer.event-actions 里，且排在最左（不是正文的末尾）。
    taskContextJump: (() => {
      const footer = q('.event-card--task footer.event-actions')
      const jump = footer?.querySelector('.hub-context-jump')
      if (!footer || !jump) return null
      return {
        text: (jump.textContent ?? '').trim(),
        fontSize: getComputedStyle(jump).fontSize,
        first: footer.firstElementChild === jump,
        inEventMain: !!q('.event-card--task .event-main .hub-context-jump'),
      }
    })(),
    // 表格：可读列宽 ≥440 + 单行单元格；横滑只在自己容器里，文档级仍 0 溢出。
    tableFit: [...document.querySelectorAll('.agent-text .md-table-wrap')].map((wrap) => {
      const table = wrap.querySelector('table')
      const cell = wrap.querySelector('td')
      return {
        wrapClient: Math.round(wrap.clientWidth),
        wrapScroll: Math.round(wrap.scrollWidth),
        tableWidth: table ? Math.round(table.getBoundingClientRect().width) : null,
        tableMinWidth: table ? getComputedStyle(table).minWidth : null,
        cellHeight: cell ? Math.round(cell.getBoundingClientRect().height * 10) / 10 : null,
        overflowX: getComputedStyle(wrap).overflowX,
      }
    }),
  }
}

// 排版细节断言（shot 与 cards 两处共用）：
//   · 作者名 = demo 的 .agent-name（12.5px/600）—— hub.css 里 <button> 的 `font: inherit`
//     曾把它压回 13px/400；
//   · 时间与名字间距 = .msg-time 的 6px（曾被 hub.css 改成 2px）；
//   · 正文 md 首尾块不吃外边距（h2 开头 14px / 列表结尾 8px 都是 demo 没有的）；
//   · 表格单元格单行；窄屏保留 440px 可读列宽，横滑只发生在 .md-table-wrap 内部。
function assertTypeDetail(m, failed, narrow) {
  if (m.nameStyle?.fontSize !== '12.5px' || m.nameStyle?.fontWeight !== '600') {
    failed.push(`作者名 ${m.nameStyle?.fontSize}/${m.nameStyle?.fontWeight}（应为 12.5px/600）`)
  }
  if (m.msgTimeStyle?.marginLeft !== '6px') {
    failed.push(`时间左间距=${m.msgTimeStyle?.marginLeft}（应为 6px）`)
  }
  if (!m.mdRhythm.length) failed.push('正文里没有 Markdown 容器（无从校验首尾外边距）')
  for (const r of m.mdRhythm) {
    if (r.firstMt !== '0px') failed.push(`正文首块 <${r.first}> margin-top=${r.firstMt}（应为 0）`)
    if (r.lastMb !== '0px') failed.push(`正文末块 <${r.last}> margin-bottom=${r.lastMb}（应为 0）`)
  }
  for (const t of m.tableFit) {
    if (t.overflowX !== 'auto') failed.push(`表格容器 overflow-x=${t.overflowX}（应为 auto）`)
    if (t.cellHeight >= 40) failed.push(`表格单元格高 ${t.cellHeight}px（内文被拆成两行）`)
    if (!narrow) continue
    if (t.tableMinWidth !== '440px') failed.push(`窄屏表格 min-width=${t.tableMinWidth}（应为 440px）`)
    if (t.tableWidth < 440) failed.push(`窄屏表格宽 ${t.tableWidth}px < 440（列被压扁）`)
    if (t.wrapScroll < t.wrapClient) failed.push(`表格容器 scrollWidth ${t.wrapScroll} < clientWidth ${t.wrapClient}`)
    // 容器比 440 窄时必须真的能横滑（390 的群聊就是这种），而不是把表格压扁。
    if (t.wrapClient < 440 && t.wrapScroll <= t.wrapClient) {
      failed.push(`容器 ${t.wrapClient}px 窄于 440，但表格没有横滑（scroll=${t.wrapScroll}）`)
    }
  }
}

async function shot(width, height, theme, kind) {  const name = `${width}-${kind}-${theme}`
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
      mdWhiteSpace: q('.agent-text .md')
        ? getComputedStyle(q('.agent-text .md')).whiteSpace
        : null,
      // 相邻块级元素之间的实际间隙：正常情况下只等于 margin（8px）；pre-wrap 会多出一个
      // line-height 的空行（实测 39.8px），所以 gap 必须小于一个行高。
      blockGap: (() => {
        for (const md of document.querySelectorAll('.agent-text .md')) {
          const first = md.querySelector('p')
          const next = first?.nextElementSibling
          if (!first || !next) continue
          return {
            gap: Math.round((next.getBoundingClientRect().top - first.getBoundingClientRect().bottom) * 10) / 10,
            lh: Number.parseFloat(getComputedStyle(first).lineHeight),
            next: next.className || next.tagName.toLowerCase(),
          }
        }
        return null
      })(),
      // 段落高度 = line-height 的整数倍 → 段落之间没有多出来的空行。
      paragraphs: [...document.querySelectorAll('.agent-text .md p')].map((el) => {
        const st = getComputedStyle(el)
        return {
          h: Math.round(el.getBoundingClientRect().height * 10) / 10,
          lh: Number.parseFloat(st.lineHeight),
          text: (el.textContent ?? '').slice(0, 12),
        }
      }),
      contextJump: q('.hub-context-jump')
        ? {
            text: (q('.hub-context-jump').textContent ?? '').trim(),
            title: q('.hub-context-jump').getAttribute('title'),
            fontSize: getComputedStyle(q('.hub-context-jump')).fontSize,
            lineHeight: getComputedStyle(q('.hub-context-jump')).lineHeight,
            color: getComputedStyle(q('.hub-context-jump')).color,
            // 同一容器里正文的颜色：弱化色必须与正文色不同（被 demo.css 的 color:inherit 抬上来就会相等）。
            bodyColor: q('.msg-main') ? getComputedStyle(q('.msg-main')).color : null,
            inMsgFooter: !!q('.msg-main > .hub-context-jump'),
            inEventMain: !!q('.event-main .hub-context-jump'),
            marginTop: getComputedStyle(q('.hub-context-jump')).marginTop,
          }
        : null,
      // 头像身份色：会话区里每个出现过的 agent id → data-fill（mock 的 accent 是 #7bc199 / #9daed2）。
      avatarFills: (() => {
        const out = {}
        for (const el of document.querySelectorAll('.chat-header .avatar, .messages .avatar')) {
          const id = el.getAttribute('data-agent-id')
          const fill = el.querySelector('svg[data-luna-face]')?.getAttribute('data-fill')
          if (id && fill && !out[id]) out[id] = fill
        }
        return out
      })(),
      mentionAvatar: (() => {
        const el = q('.agent-text .hub-mention .luna-avatar')
        if (!el) return null
        return {
          display: getComputedStyle(el).display,
          width: Math.round(el.getBoundingClientRect().width),
          fill: el.querySelector('svg[data-luna-face]')?.getAttribute('data-fill') ?? null,
        }
      })(),
    }
  })
  // 作者名 / 时间 / 正文首尾 / 表格：与 cards 区块同一份测量与断言。
  Object.assign(m, await page.evaluate(typeDetailProbe))
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
      if (m.dmSigil.width !== 52) failed.push(`DM header avatar=${m.dmSigil.width}px（应为 52）`)
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
  // 段落间距：Markdown 块级元素之间不再被 pre-wrap 画出空行。
  if (m.mdWhiteSpace !== 'normal') failed.push(`.agent-text .md white-space=${m.mdWhiteSpace}（应为 normal）`)
  if (!m.blockGap) failed.push('找不到相邻的 Markdown 块级元素（无法校验空行）')
  else if (m.blockGap.gap >= m.blockGap.lh) {
    failed.push(`p → ${m.blockGap.next} 间隙 ${m.blockGap.gap}px ≥ 行高 ${m.blockGap.lh}px（多画了空行）`)
  }
  for (const p of m.paragraphs) {
    const lines = Math.round(p.h / p.lh)
    if (Math.abs(p.h - lines * p.lh) > 1) {
      failed.push(`段落「${p.text}」高 ${p.h}px 不是 line-height ${p.lh}px 的整数倍（多出空行）`)
      break
    }
  }
  // 排版细节（名字/时间/正文首尾/表格可读列宽）——与 cards 区块共用同一组断言。
  assertTypeDetail(m, failed, narrow)
  // 触发上下文：可读文字，不再用「↳」符号。
  if (kind === 'group') {
    if (!m.contextJump) failed.push('群消息缺少触发上下文入口')
    else {
      if (!m.contextJump.text.includes('人触发')) failed.push(`触发上下文文案=${m.contextJump.text}（应含「人触发」）`)
      if (m.contextJump.text.includes('↳')) failed.push('触发上下文仍在用「↳」符号')
      if (!m.contextJump.title?.includes('#')) failed.push(`触发上下文 title 缺少 seq：${m.contextJump.title}`)
      // demo 的小字口径：被 demo.css 的 `.hub-demo button{font:inherit}` 抬成 13px 就太抢眼。
      if (m.contextJump.fontSize !== '10.5px') failed.push(`触发上下文字号=${m.contextJump.fontSize}（应为 10.5px）`)
      if (m.contextJump.color === m.contextJump.bodyColor) failed.push('触发上下文用了正文色（应为弱化色）')
      if (!m.contextJump.inMsgFooter) failed.push('消息里的触发上下文不在 footer（.msg-main 直接子元素）')
      if (m.contextJump.inEventMain) failed.push('触发上下文跑进了 .event-main（应留在 footer）')
      if (m.contextJump.marginTop !== '2px') failed.push(`消息 footer 触发上下文上边距=${m.contextJump.marginTop}（应为 2px）`)
    }
    // 任务卡的「N 人触发」：在 footer.event-actions 最左侧，与 task id / 打开会话同一行。
    if (!m.taskContextJump) failed.push('任务卡 footer 缺少「N 人触发」')
    else {
      if (!m.taskContextJump.text.includes('人触发')) failed.push(`任务卡 footer 上下文=${m.taskContextJump.text}`)
      if (m.taskContextJump.fontSize !== '10.5px') failed.push(`任务卡 footer 上下文字号=${m.taskContextJump.fontSize}（应为 10.5px）`)
      if (!m.taskContextJump.first) failed.push('任务卡「N 人触发」不在 footer 最左')
      if (m.taskContextJump.inEventMain) failed.push('任务卡「N 人触发」仍在 .event-main')
    }
    if (!m.mentionAvatar) failed.push('正文 @ 提及缺少 Luna 头像')
    else {
      if (m.mentionAvatar.display === 'none') failed.push('正文 @ 提及头像被隐藏（display:none）')
      if (m.mentionAvatar.width !== 16) failed.push(`正文 @ 提及头像=${m.mentionAvatar.width}px（应为 16）`)
      if (m.mentionAvatar.fill !== '#7bc199') failed.push(`正文 @ 提及头像色=${m.mentionAvatar.fill}（应为 #7bc199）`)
    }
  }
  // 身份色：头像必须用 profile.accent，而不是「研究→蓝 / 写作→紫」的职责色。
  // 群页面必须真的画出 Researcher（owner + 有发言）；DM 视角只校验页面上出现的智能体，
  // 不为凑断言往 Writer DM 塞 Researcher 头像。
  const WANT_FILL = { ag_mockres1: '#7bc199', ag_mockwri2: '#9daed2' }
  const REQUIRED_FILL = kind === 'group' ? ['ag_mockres1'] : []
  for (const [id, want] of Object.entries(WANT_FILL)) {
    const got = m.avatarFills[id]
    if (!got) {
      if (REQUIRED_FILL.includes(id)) failed.push(`群会话缺少 ${id} 的头像（身份色无从校验）`)
      continue
    }
    if (got !== want) failed.push(`${id} 头像 data-fill=${got}（应为 ${want}）`)
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
// 1440 与 390 各一套：窄屏卡片左缘 = 38（事件图标 24 + gap 9 + 卡片外距 5）。
const CARD_CASES = [[1440, 900], [390, 900]].flatMap(([w, h]) => THEMES.map((theme) => [w, h, theme]))
for (const [width, height, theme] of CARD_CASES) {
  const page = await browser.newPage({ viewport: { width, height } })
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
      // 事件卡归属：handoff 画「发起人 → 接收人」，approval 画发起人，task 的执行者带 16px 头像。
      handoffFlow: (() => {
        const flow = q('.event-card--handoff .event-flow')
        if (!flow) return null
        const faces = [...flow.querySelectorAll('.luna-avatar')]
        return {
          avatars: faces.length,
          size: faces.map((f) => Math.round(f.getBoundingClientRect().width)),
          fill: faces.map((f) => f.querySelector('svg[data-luna-face]')?.getAttribute('data-fill') ?? null),
          names: [...flow.querySelectorAll('.agent')].map((a) => a.textContent?.trim() ?? ''),
          arrow: flow.querySelector('.arrow')?.textContent?.trim() ?? '',
        }
      })(),
      approvalFlow: (() => {
        const flow = q('.event-card--approval .event-flow')
        if (!flow) return null
        const faces = [...flow.querySelectorAll('.luna-avatar')]
        return {
          avatars: faces.length,
          size: faces.map((f) => Math.round(f.getBoundingClientRect().width)),
          fill: faces.map((f) => f.querySelector('svg[data-luna-face]')?.getAttribute('data-fill') ?? null),
          names: [...flow.querySelectorAll('.agent')].map((a) => a.textContent?.trim() ?? ''),
        }
      })(),
      taskWho: [...document.querySelectorAll('.event-card--task .event-who')].map((el) => {
        const face = el.querySelector('.luna-avatar')
        return {
          text: el.textContent?.trim() ?? '',
          faceSize: face ? Math.round(face.getBoundingClientRect().width) : null,
          fill: face?.querySelector('svg[data-luna-face]')?.getAttribute('data-fill') ?? null,
        }
      }),
      // 三张卡的正文左缘要跟各自事件标题左缘对齐（≤2px）。handoff/approval 的 .event-body
      // 曾是 .event-main 的子元素，38px margin 叠上 event-main 的 39px 缩进 → 正文整体右移。
      cardBodies: [...document.querySelectorAll('.event-card')].map((card) => {
        const bodyEl = card.querySelector('.event-body')
        const nameEl = card.querySelector('.event-title .name')
        return {
          kind: (card.className.match(/event-card--(\w+)/) ?? [])[1] ?? '?',
          direct: !!bodyEl && bodyEl.parentElement === card,
          delta: bodyEl && nameEl
            ? Math.round((bodyEl.getBoundingClientRect().left - nameEl.getBoundingClientRect().left) * 10) / 10
            : null,
        }
      }),
      // event-flow：demo 是 `<span class="agent">{头像}{名字}</span>`，组内 gap 4（组间才是 6）。
      // 头像必须真的在这个组里（avatarOffset=0），不能是名字旁边的兄弟节点。
      flowAgents: [...document.querySelectorAll('.event-flow .agent')].map((el) => {
        const avatar = el.querySelector('.luna-avatar')
        return {
          gap: getComputedStyle(el).gap,
          text: el.textContent?.trim() ?? '',
          avatarOffset: avatar
            ? Math.round((avatar.getBoundingClientRect().left - el.getBoundingClientRect().left) * 10) / 10
            : null,
        }
      }),
      // 没有 context / task_id / 入口的卡不该留下一行空 footer。
      emptyFooters: [...document.querySelectorAll('.event-card > footer')]
        .filter((f) => !f.children.length).length,
      handoffFooter: !!q('.event-card--handoff footer'),
      approvalFooter: !!q('.event-card--approval footer'),
      scrollWidth: document.documentElement.scrollWidth,
    }
  })
  Object.assign(m, await page.evaluate(typeDetailProbe))
  const failed = []
  const wantCardLeft = width <= 640 ? 38 : 41
  if (m.cardLeft !== wantCardLeft || m.handoffLeft !== wantCardLeft || m.approvalLeft !== wantCardLeft) {
    failed.push(`事件卡左缘应=${wantCardLeft}（task=${m.cardLeft} handoff=${m.handoffLeft} approval=${m.approvalLeft}）`)
  }
  if (m.cardBody?.ml !== '38px' || m.cardBody?.fs !== '11.5px') failed.push(`event-body=${m.cardBody?.ml}/${m.cardBody?.fs}（应为 38px/11.5px）`)
  if (m.cardActions?.ml !== '38px' || m.cardActions?.gap !== '6px') failed.push(`event-actions=${m.cardActions?.ml}/${m.cardActions?.gap}（应为 38px/6px）`)
  if (m.handoffCount !== 1) failed.push(`handoff 卡数量=${m.handoffCount}`)
  if (m.approvalCount !== 1) failed.push(`approval 卡数量=${m.approvalCount}`)
  // 三张卡同构：正文是 article 的直接子元素，左缘与各自事件标题左缘齐（差 ≤2px）。
  const bodyKinds = m.cardBodies.map((c) => c.kind).sort().join(',')
  if (bodyKinds !== 'approval,handoff,task') failed.push(`事件卡种类=${bodyKinds}（应为 approval,handoff,task）`)
  for (const c of m.cardBodies) {
    if (!c.direct) failed.push(`${c.kind} 卡的 .event-body 不是 article 直接子元素（缩进会叠两层）`)
    if (c.delta === null) failed.push(`${c.kind} 卡缺少 .event-body / .event-title .name`)
    else if (Math.abs(c.delta) > 2) failed.push(`${c.kind} 卡正文左缘与标题差 ${c.delta}px（应 ≤2）`)
  }
  // event-flow 的 .agent 组内 gap 4，且头像与名字在同一个组里（avatarOffset=0）。
  if (m.flowAgents.length !== 3) failed.push(`event-flow .agent 组数=${m.flowAgents.length}（handoff 2 + approval 1）`)
  for (const g of m.flowAgents) {
    if (g.gap !== '4px') failed.push(`event-flow .agent gap=${g.gap}（应为 4px）`)
    if (g.avatarOffset === null) failed.push(`event-flow 的「${g.text}」组里没有头像（头像与名字成了兄弟）`)
    else if (g.avatarOffset !== 0) failed.push(`event-flow 的「${g.text}」组头像偏 ${g.avatarOffset}px（应为 0）`)
  }
  // 没有 context 的卡不补 footer（handoff / approval 没有 event-actions，也没有空 footer）。
  if (m.emptyFooters !== 0) failed.push(`有空 footer 的卡 ${m.emptyFooters} 个`)
  if (m.handoffFooter) failed.push('handoff 卡不该有 footer.event-actions')
  if (m.approvalFooter) failed.push('approval 卡不该有 footer.event-actions')
  // 任务卡的「N 人触发」在 footer 最左；名字/时间/正文/表格同 shot 口径。
  if (!m.taskContextJump) failed.push('任务卡 footer 缺少「N 人触发」')
  else {
    if (m.taskContextJump.fontSize !== '10.5px') failed.push(`任务卡上下文=${m.taskContextJump.fontSize}（应为 10.5px）`)
    if (!m.taskContextJump.first) failed.push('任务卡「N 人触发」不在 footer 最左')
  }
  assertTypeDetail(m, failed, width <= 640)
  if (m.handoffIcon === m.approvalIcon) failed.push('handoff/approval 图标颜色未区分')
  if (m.approvalBorder === m.taskBorder) failed.push('approval 暖边框未生效')
  if (m.approvalTitle === m.taskTitle) failed.push('approval warn 标题未生效')
  // 事件卡归属：handoff 两端都要有 18px 头像与名字，→ 只在两端都在时出现。
  const hf = m.handoffFlow
  if (!hf || hf.avatars !== 2) failed.push(`handoff 卡 .event-flow 头像=${hf?.avatars ?? 0}（应为 2）`)
  else {
    if (hf.size.some((w) => w !== 18)) failed.push(`handoff 头像尺寸=${hf.size.join('/')}（应为 18）`)
    if (!hf.names[0]?.includes('Researcher') || !hf.names[1]?.includes('Writer')) {
      failed.push(`handoff 归属名字=${hf.names.join('→')}（应为 Researcher→Writer）`)
    }
    if (hf.arrow !== '→') failed.push(`handoff 缺少 →（${hf.arrow}）`)
    if (hf.fill[0] !== '#7bc199' || hf.fill[1] !== '#9daed2') failed.push(`handoff 头像色=${hf.fill.join('/')}`)
  }
  const af = m.approvalFlow
  if (!af || af.avatars !== 1) failed.push(`approval 卡 .event-flow 头像=${af?.avatars ?? 0}（应为 1）`)
  else {
    if (af.size[0] !== 18) failed.push(`approval 头像尺寸=${af.size[0]}（应为 18）`)
    if (!af.names[0]?.includes('Writer')) failed.push(`approval 发起人=${af.names[0]}（应为 Writer）`)
    if (af.fill[0] !== '#9daed2') failed.push(`approval 头像色=${af.fill[0]}（应为 #9daed2）`)
  }
  const execWho = m.taskWho.find((w) => w.text.includes('Writer'))
  if (!execWho) failed.push('task 卡缺少执行者行')
  else {
    if (execWho.faceSize !== 16) failed.push(`task 卡执行者头像=${execWho.faceSize}px（应为 16）`)
    if (execWho.fill !== '#9daed2') failed.push(`task 卡执行者头像色=${execWho.fill}（应为 #9daed2）`)
  }
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
