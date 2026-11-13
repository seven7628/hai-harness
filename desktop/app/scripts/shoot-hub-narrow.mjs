#!/usr/bin/env node
// 窄窗 Hub 布局 + composer 派任务入口回归（P4-UI-9：HUB-N-01 / HUB-N-02）。
//
// 用法：先起 dev server（不要占 5173-5175），再跑
//   npx vite --port 5180
//   node scripts/shoot-hub-narrow.mjs --url http://localhost:5180/ [--out /tmp/hub-narrow-shots]
//
// 断言（任一失败 → 退出码 1）：
//   · 640/760/900 × ink/paper：documentElement.scrollWidth === 视口宽（0 横向溢出）
//   · 从 rail 点「智能体」进 Hub：左栏真的展开（>100px），class 与真实宽度一致
//     （44px 图标条不得再挂 expanded、不得装展开内容 —— HUB-N-01）
//   · .hub-side 有真实宽度（≥100px）、能切「群组 / 智能体」并选中群与 DM
//   · 会话列 ≥360px；<1000 时 inspector 收成 ≤48px 图标条（不抢详情的宽度）
//   · composer meta 的「对话 / 派任务」开关可见；点「派任务」后 .hub-assign-form 出现，
//     且整块落在视口内（HUB-N-02）
//   · 640 下开关按钮 ≥44×28（窄窗触控/点击目标）；760/900 仍保持 demo 的紧凑尺寸（≥28×16）
//
// 截图：<out>/<theme>-<w>-{group,assign,dm}.png
import { chromium } from 'playwright'
import { mkdirSync } from 'node:fs'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const URL = arg('url', process.env.HUB_NARROW_URL || 'http://localhost:5180/')
const OUT = arg('out', '/tmp/hub-narrow-shots')
const VIEWPORTS = [640, 760, 900]
const THEMES = ['ink', 'paper']
const NARROW = 640

mkdirSync(OUT, { recursive: true })
const browser = await chromium.launch()
const rows = []

async function boot(page) {
  await page.goto(URL, { waitUntil: 'domcontentloaded' })
  await page.waitForTimeout(1400)
  const enter = page.locator('button:has-text("进入"), .boot-enter').first()
  if (await enter.count()) {
    await enter.click({ timeout: 4000 }).catch(() => {})
    await page.waitForTimeout(1400)
  }
  await page.waitForTimeout(600)
}

async function measure(page) {
  return page.evaluate(() => {
    const r = (el) => {
      if (!el) return null
      const b = el.getBoundingClientRect()
      return { x: Math.round(b.x), y: Math.round(b.y), w: Math.round(b.width), h: Math.round(b.height) }
    }
    const q = (s) => document.querySelector(s)
    const left = q('.left')
    const leftW = left ? Math.round(left.getBoundingClientRect().width) : null
    const modeButtons = [...document.querySelectorAll('.hub-group-view .composer-mode-switch button')]
      .map((b) => ({ label: (b.textContent ?? '').trim(), visible: b.offsetParent != null, ...r(b) }))
    return {
      viewport: window.innerWidth,
      scrollWidth: document.documentElement.scrollWidth,
      theme: document.documentElement.dataset.theme,
      leftW,
      leftClass: left?.className ?? null,
      leftExpanded: window.__store.getState().leftExpanded,
      hubSide: r(q('.hub-side')),
      hubRows: document.querySelectorAll('.hub-side-row').length,
      center: r(q('.hub-group-view, .hub-dm, .hub-empty')),
      inspector: r(q('.hub-inspector')),
      inspectorRail: r(q('.hub-detail-rail')),
      modeSwitch: r(q('.hub-group-view .composer-mode-switch')),
      modeButtons,
      assignForm: r(q('.hub-assign-form')),
      assignTextarea: r(q('.hub-assign-form textarea')),
      dm: r(q('.hub-dm')),
      dmComposer: !!q('.hub-dm .composer'),
    }
  })
}

async function clearHover(page) {
  await page.mouse.move(1, 1)
  await page.evaluate(() => {
    const el = document.activeElement
    if (el instanceof HTMLElement) el.blur()
  })
  await page.waitForTimeout(120)
}

for (const width of VIEWPORTS) {
  for (const theme of THEMES) {
    const name = `${theme}-${width}`
    const context = await browser.newContext({
      viewport: { width, height: 900 },
      deviceScaleFactor: 1,
      locale: 'zh-CN',
    })
    const page = await context.newPage()
    const errors = []
    page.on('pageerror', (e) => errors.push(String(e)))
    page.on('console', (m) => { if (m.type() === 'error') errors.push(`console: ${m.text()}`) })
    const failed = []
    const row = { name, width, theme, failed }
    rows.push(row)
    try {
      await boot(page)
      await page.evaluate((t) => window.__store.getState().setTheme(t), theme === 'paper' ? 'light' : 'dark')
      await page.waitForTimeout(350)

      // 用户路径：先把左栏收成 rail，再从 rail 点「智能体」进 Hub（审计复现路径）。
      await page.evaluate(() => window.__store.getState().setLeftExpanded(false))
      await page.waitForTimeout(450)
      const railBtn = page.locator('button[aria-label="智能体"]').first()
      if (!(await railBtn.count())) failed.push('rail 上找不到「智能体」入口')
      else await railBtn.click({ timeout: 6000 })
      await page.waitForTimeout(1000)

      let m = await measure(page)
      row.afterHub = m
      if (m.theme !== theme) failed.push(`主题未生效（data-theme=${m.theme}）`)
      if (m.scrollWidth !== width) failed.push(`横向溢出（scrollWidth=${m.scrollWidth} > ${width}）`)
      if (!m.hubSide) failed.push('Hub 左栏未挂载（.hub-side 缺失）')
      else if (m.hubSide.w < 100) failed.push(`Hub 侧栏只有 ${m.hubSide.w}px（<100，群/DM 不可读）`)
      if (m.leftW == null) failed.push('找不到 .left')
      else {
        const classExpanded = (m.leftClass ?? '').split(/\s+/).includes('expanded')
        if (m.leftW > 60 && !classExpanded) failed.push(`左栏 ${m.leftW}px 却无 expanded class（${m.leftClass}）`)
        if (m.leftW <= 60 && classExpanded) failed.push(`左栏只有 ${m.leftW}px 却挂 expanded（HUB-N-01 复现）`)
        if (m.leftW <= 60) failed.push(`从 rail 进 Hub 后左栏仍是图标条 ${m.leftW}px，选不了群/DM`)
        if (!m.leftExpanded) failed.push('左栏实际已展开，store leftExpanded 仍为 false')
      }
      if (!m.center) failed.push('会话列未挂载')
      else if (m.center.w < 360) failed.push(`会话列 ${m.center.w}px < 360px`)
      if (width < 1000) {
        if (m.inspector) failed.push(`窄窗 inspector 仍展开（${m.inspector.w}px）`)
        if (!m.inspectorRail) failed.push('窄窗 inspector 图标条缺失')
        else if (m.inspectorRail.w > 48) failed.push(`inspector 图标条 ${m.inspectorRail.w}px（应 ≤48）`)
      }
      if (m.hubRows < 1) failed.push('Hub 侧栏没有可选行')

      // 群：切「群组」→ 选第一个群 → 群会话 + composer meta。
      const groupTab = page.locator('.hub-side-tabs button').filter({ hasText: '群组' }).first()
      if (!(await groupTab.count())) failed.push('侧栏没有「群组」页签')
      else {
        if ((await groupTab.getAttribute('aria-selected')) !== 'true') await groupTab.click()
        await page.waitForTimeout(350)
      }
      const groupRow = page.locator('.hub-side-row').first()
      if (!(await groupRow.count())) failed.push('群组页签下没有群可点')
      else await groupRow.click({ timeout: 6000 })
      await page.waitForTimeout(900)
      if (!(await page.locator('.hub-group-view').count())) failed.push('点群后没有进入群会话')

      m = await measure(page)
      row.group = m
      if (m.scrollWidth !== width) failed.push(`群会话横向溢出（${m.scrollWidth} > ${width}）`)
      if (!m.modeSwitch) failed.push('composer meta 缺少「对话/派任务」开关')
      else {
        if (m.modeSwitch.w < 60) failed.push(`模式开关只有 ${m.modeSwitch.w}px 宽`)
        for (const b of m.modeButtons) {
          if (!b.visible) failed.push(`模式开关按钮「${b.label}」不可见（display:none / 0 尺寸）`)
          if (width <= 700 && (b.w < 44 || b.h < 28)) failed.push(`640 下「${b.label}」按钮 ${b.w}×${b.h}（应 ≥44×28）`)
          if (width > 700 && (b.w < 28 || b.h < 16)) failed.push(`「${b.label}」按钮 ${b.w}×${b.h}（应 ≥28×16）`)
        }
      }
      await clearHover(page)
      await page.screenshot({ path: `${OUT}/${name}-group.png` })

      // 派任务：点开关 → 表单出现且整块在视口内。
      const assignBtn = page.locator('.hub-group-view .composer-mode-switch button').filter({ hasText: '派任务' }).first()
      if (await assignBtn.count()) await assignBtn.click({ timeout: 4000 })
      await page.waitForTimeout(600)
      m = await measure(page)
      row.assign = m
      if (!m.assignForm) failed.push('点「派任务」后 .hub-assign-form 未出现')
      else {
        if (m.assignForm.x < 0 || m.assignForm.x + m.assignForm.w > width + 1) {
          failed.push(`派任务表单越出视口（x=${m.assignForm.x} w=${m.assignForm.w}）`)
        }
        if (m.assignForm.w < 200) failed.push(`派任务表单只有 ${m.assignForm.w}px 宽`)
        if (!m.assignTextarea || m.assignTextarea.h < 20) failed.push('派任务表单缺少可输入的指令框')
      }
      await clearHover(page)
      await page.screenshot({ path: `${OUT}/${name}-assign.png` })

      // DM：回到「智能体」页签选第一个 agent。
      const agentTab = page.locator('.hub-side-tabs button').filter({ hasText: '智能体' }).first()
      if (await agentTab.count()) {
        if ((await agentTab.getAttribute('aria-selected')) !== 'true') await agentTab.click()
        await page.waitForTimeout(350)
      }
      const agentRow = page.locator('.hub-side-row').first()
      if (!(await agentRow.count())) failed.push('智能体页签下没有 DM 可点')
      else await agentRow.click({ timeout: 6000 })
      await page.waitForTimeout(900)
      m = await measure(page)
      row.dm = m
      if (!m.dm) failed.push('点 DM 后没有进入 DM 会话')
      if (!m.dmComposer) failed.push('DM 会话缺少 composer')
      if (m.scrollWidth !== width) failed.push(`DM 会话横向溢出（${m.scrollWidth} > ${width}）`)
      await clearHover(page)
      await page.screenshot({ path: `${OUT}/${name}-dm.png` })
    } catch (e) {
      failed.push(`异常：${String(e?.message ?? e)}`)
    }
    if (errors.length) failed.push(...errors.slice(0, 3))
    if (!failed.length) console.log(`  ✅ ${name}`)
    else console.log(`  ❌ ${name}\n     - ${failed.join('\n     - ')}`)
    await context.close()
  }
}

await browser.close()

console.log('\n尺寸摘要（左栏 / 侧栏 / 会话列 / inspector）：')
for (const r of rows) {
  const g = r.group ?? r.afterHub ?? {}
  const side = g.hubSide?.w ?? '?'
  const center = g.center?.w ?? '?'
  const insp = g.inspector?.w ?? (g.inspectorRail ? `${g.inspectorRail.w}(icon)` : '?')
  console.log(`  ${r.name.padEnd(10)} ${String(g.leftW ?? '?').padStart(4)} / ${String(side).padStart(4)} / ${String(center).padStart(4)} / ${insp}`)
}
console.log(`\n截图：${OUT}`)
if (rows.some((r) => r.failed.length)) {
  const n = rows.reduce((sum, r) => sum + r.failed.length, 0)
  console.error(`\n断言失败 ${n} 条（${rows.filter((r) => r.failed.length).map((r) => r.name).join(', ')}）`)
  process.exit(1)
}
console.log('\n全部断言通过')
