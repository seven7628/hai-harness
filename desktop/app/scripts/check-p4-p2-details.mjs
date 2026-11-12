#!/usr/bin/env node
// P4-UI-11 细节回归（真实 Chromium）：HUB-CTX-01 / HUB-NEED-01 / HUB-A11Y-01 /
// HUB-TARGET-01 / HUB-EDITOR-01 / HUB-DL-01 + system 主题跟随。
//
// 用法（dev server 别占 5173-5175）：
//   npx vite --port 5180
//   node scripts/check-p4-p2-details.mjs --url http://localhost:5180/ [--out /tmp/p4-p2-details]
//
// 断言（任一失败 → 退出码 1）：
//   · Context@1440（inspector 360）：「消息」label 不竖排（宽 > 高、nowrap），select 拿走剩余宽度
//   · 深链：复制按钮与 URL 同一行且贴住 URL 行尾（不再是右上角浮着）
//   · 侧栏小节标题（智能体 / 工作区）in k/paper 对比度都 ≥4.5（AA 正文口径）
//   · 640/760/900：@、发送、上下文/更多、事件触发入口、侧栏筛选 chip、任务状态筛选
//     的最小边都在 32–36px（桌面指针精度；移动 44 在 mobile_web/style.css）
//   · AgentEditor 1280：宽度 ≤720 且水平居中；640：仍按 92vw 收窄；删除按钮只在 footer
//   · ?hubFixture=needs：提问卡解析出输入框后不再铺原始 JSON
//   · system 主题：setTheme('system') 后跟随 emulateMedia(colorScheme) 双向切 ink/paper
//
// 截图：<out>/<name>.png
import { chromium } from 'playwright'
import { mkdirSync } from 'node:fs'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const URL = arg('url', process.env.HUB_DETAIL_URL || 'http://localhost:5180/')
const OUT = arg('out', '/tmp/p4-p2-details')
mkdirSync(OUT, { recursive: true })

const browser = await chromium.launch()
let failures = 0
function check(name, cond, detail = '') {
  if (cond) console.log(`  ✅ ${name}${detail ? `（${detail}）` : ''}`)
  else { failures++; console.log(`  ❌ ${name}${detail ? `（${detail}）` : ''}`) }
  return cond
}

async function boot(page, search = '') {
  const url = search ? `${URL}${URL.includes('?') ? '&' : '?'}${search}` : URL
  await page.goto(url, { waitUntil: 'domcontentloaded' })
  await page.waitForTimeout(1500)
  const enter = page.locator('button:has-text("进入"), .boot-enter').first()
  if (await enter.count()) { await enter.click({ timeout: 4000 }).catch(() => {}); await page.waitForTimeout(1500) }
  await page.waitForTimeout(500)
}

async function enterHub(page) {
  await page.evaluate(() => window.__store.getState().setLeftExpanded(false))
  await page.waitForTimeout(400)
  await page.locator('button[aria-label="智能体"]').first().click({ timeout: 6000 })
  await page.waitForTimeout(1000)
}

async function pick(page, kind = 'group') {
  if (kind === 'group') {
    const tab = page.locator('.hub-side-tabs button').filter({ hasText: '群组' }).first()
    if (await tab.count()) { await tab.click(); await page.waitForTimeout(350) }
  } else {
    const tab = page.locator('.hub-side-tabs button').filter({ hasText: '智能体' }).first()
    if (await tab.count()) { await tab.click(); await page.waitForTimeout(350) }
  }
  await page.locator('.hub-side-row').first().click({ timeout: 6000 })
  await page.waitForTimeout(900)
}

/** 打开 inspector 的某个面板：宽窗走 tab，窄窗（<1000 图标条）走 rail 按钮。 */
async function openPanel(page, label) {
  const tab = page.locator('.hub-inspector-tabs button').filter({ hasText: label }).first()
  if (await tab.count()) { await tab.click({ timeout: 4000 }); await page.waitForTimeout(700); return true }
  const railLabel = label === 'Context' ? 'Context' : label === '任务' ? '任务' : label === 'Needs' ? 'Needs you' : label
  const rail = page.locator(`.hub-detail-rail button[aria-label="${railLabel}"]`).first()
  if (await rail.count()) { await rail.click({ timeout: 4000 }); await page.waitForTimeout(900); return true }
  return false
}

const rect = (el) => el ? el.getBoundingClientRect() : null

// ——— 1) Context@1440：label 不竖排 + 深链按钮贴 URL 行尾 ———
{
  console.log('\n[1] Context 面板 @1440（inspector 360）')
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 }, locale: 'zh-CN' })
  const page = await ctx.newPage()
  try {
    await boot(page)
    await enterHub(page)
    await pick(page, 'group')
    check('Context 面板打开', await openPanel(page, 'Context'))
    const m = await page.evaluate(() => {
      const r = (el) => { if (!el) return null; const b = el.getBoundingClientRect(); return { x: Math.round(b.x), y: Math.round(b.y), w: Math.round(b.width), h: Math.round(b.height) } }
      const picker = document.querySelector('.hub-context-picker')
      const label = picker?.querySelector('label')
      const sel = picker?.querySelector('select')
      const del = document.querySelector('.hub-deeplink')
      const url = del?.querySelector('.hub-deeplink-url')
      const btn = del?.querySelector('.hub-btn')
      const insp = document.querySelector('.hub-inspector')
      return {
        inspectorW: insp ? Math.round(insp.getBoundingClientRect().width) : null,
        label: r(label),
        labelWs: label ? getComputedStyle(label).whiteSpace : null,
        labelText: label?.textContent ?? '',
        select: r(sel),
        url: r(url),
        btn: r(btn),
      }
    })
    check('inspector 真的是 360 宽', m.inspectorW != null && Math.abs(m.inspectorW - 360) <= 8, `${m.inspectorW}px`)
    check('「消息」label 不竖排（宽 > 高）', !!m.label && m.label.w > m.label.h, `${m.label?.w}×${m.label?.h}`)
    check('「消息」label 不换行（nowrap）', m.labelWs === 'nowrap', m.labelWs)
    check('select 拿走剩余宽度（≥240）', (m.select?.w ?? 0) >= 240, `${m.select?.w}px`)
    check('深链复制按钮与 URL 同一行（纵向重叠）', !!m.url && !!m.btn
      && m.btn.y < m.url.y + m.url.h && m.btn.y + m.btn.h > m.url.y,
      `url y=${m.url?.y}h=${m.url?.h} btn y=${m.btn?.y}h=${m.btn?.h}`)
    check('深链复制按钮贴住 URL 行尾（与 URL 的间距 ≤12）', !!m.url && !!m.btn
      && m.btn.x >= m.url.x + m.url.w && m.btn.x - (m.url.x + m.url.w) <= 12,
      `url right=${m.url ? m.url.x + m.url.w : '-'} btn left=${m.btn?.x ?? '-'}`)
    await page.screenshot({ path: `${OUT}/01-context-1440.png` })
  } catch (e) {
    check('Context 面板测量未抛异常', false, String(e?.message ?? e).slice(0, 160))
  }
  await ctx.close()
}

// ——— 2) 对比度：侧栏小节标题 ink/paper ≥4.5 ———
{
  console.log('\n[2] 侧栏小节标题对比度（11px 属 AA 正文口径）')
  for (const [theme, pref] of [['ink', 'dark'], ['paper', 'light']]) {
    const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 }, locale: 'zh-CN' })
    const page = await ctx.newPage()
    try {
      await boot(page)
      await page.evaluate((p) => window.__store.getState().setTheme(p), pref)
      await page.waitForTimeout(600)
      await page.evaluate(() => window.__store.getState().setLeftExpanded(true))
      await page.waitForTimeout(500)
      const rows = await page.evaluate(() => {
        const parse = (c) => { const m = c.match(/[\d.]+/g); return m ? m.slice(0, 3).map(Number) : [0, 0, 0] }
        const lum = (rgb) => {
          const f = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4 }
          return 0.2126 * f(rgb[0]) + 0.7152 * f(rgb[1]) + 0.0722 * f(rgb[2])
        }
        const bgOf = (el) => {
          let n = el
          while (n && n !== document.documentElement) {
            const c = getComputedStyle(n).backgroundColor
            if (c && !/rgba\(0, 0, 0, 0\)|transparent/.test(c)) return c
            n = n.parentElement
          }
          return getComputedStyle(document.body).backgroundColor
        }
        const ratio = (fg, bg) => {
          const a = lum(parse(fg)); const b = lum(parse(bg))
          const hi = Math.max(a, b); const lo = Math.min(a, b)
          return Math.round(((hi + 0.05) / (lo + 0.05)) * 100) / 100
        }
        return ['.hub-side-title', '.left-title'].map((sel) => {
          const el = document.querySelector(sel)
          if (!el) return { sel, missing: true }
          const cs = getComputedStyle(el)
          return { sel, text: (el.textContent || '').trim(), color: cs.color, bg: bgOf(el), fs: cs.fontSize, ratio: ratio(cs.color, bgOf(el)) }
        })
      })
      for (const r of rows) {
        check(`${theme} ${r.sel} 对比度 ≥4.5（${r.text ?? '?'}）`, !r.missing && r.ratio >= 4.5,
          r.missing ? '元素不存在' : `${r.ratio}:1 · ${r.color} on ${r.bg}`)
      }
      await page.screenshot({ path: `${OUT}/02-titles-${theme}.png` })
    } catch (e) {
      check(`${theme} 对比度测量未抛异常`, false, String(e?.message ?? e).slice(0, 160))
    }
    await ctx.close()
  }
}

// ——— 3) ≤900 点击目标 32–36px ———
{
  console.log('\n[3] 640/760/900 点击目标（桌面 32–36）')
  for (const width of [640, 760, 900]) {
    const ctx = await browser.newContext({ viewport: { width, height: 900 }, locale: 'zh-CN' })
    const page = await ctx.newPage()
    try {
      await boot(page)
      await enterHub(page)
      await pick(page, 'group')
      const m = await page.evaluate(() => {
        const r = (el) => { if (!el) return null; const b = el.getBoundingClientRect(); return { w: Math.round(b.width), h: Math.round(b.height) } }
        const visible = (el) => !!el && el.offsetParent != null
        const at = [...document.querySelectorAll('.composer .icon-btn')]
          .find((el) => visible(el) && (el.getAttribute('title') || '').includes('@'))
        const send = [...document.querySelectorAll('.send-btn')].find(visible)
        const headerCtx = [...document.querySelectorAll('.header-actions .icon-btn')]
          .find((el) => (el.getAttribute('title') || '').includes('上下文'))
        const headerMore = [...document.querySelectorAll('.header-actions .icon-btn')]
          .find((el) => (el.getAttribute('title') || '').includes('更多'))
        const jump = document.querySelector('.hub-context-jump')
        const scopes = [...document.querySelectorAll('.hub-side-scopes button')]
        const tabs = [...document.querySelectorAll('.hub-side-tabs button')]
        return {
          at: r(at), send: r(send), headerCtx: r(headerCtx), headerMore: r(headerMore), jump: r(jump),
          scope: r(scopes[0]), tab: r(tabs[1] ?? tabs[0]),
        }
      })
      const inRange = (x) => !!x && Math.min(x.w, x.h) >= 32 && Math.min(x.w, x.h) <= 36
      check(`@${width} @ 提及 ≥32`, inRange(m.at), m.at ? `${m.at.w}×${m.at.h}` : '未找到')
      check(`@${width} 发送 ≥32`, inRange(m.send), m.send ? `${m.send.w}×${m.send.h}` : '未找到')
      check(`@${width} 上下文入口 ≥32`, inRange(m.headerCtx), m.headerCtx ? `${m.headerCtx.w}×${m.headerCtx.h}` : '未找到')
      if (m.headerMore) check(`@${width} 更多入口 ≥32`, inRange(m.headerMore), `${m.headerMore.w}×${m.headerMore.h}`)
      check(`@${width} 事件触发入口（N 人触发）≥32 高`, !!m.jump && m.jump.h >= 32 && m.jump.h <= 36,
        m.jump ? `${m.jump.w}×${m.jump.h}` : '未找到')
      check(`@${width} 侧栏筛选 chip ≥32`, inRange(m.scope) || inRange(m.tab),
        `scope=${m.scope ? `${m.scope.w}×${m.scope.h}` : '-'} tab=${m.tab ? `${m.tab.w}×${m.tab.h}` : '-'}`)
      // 任务状态筛选（需要打开 inspector；窄窗先从 rail 撑开）
      const opened = await openPanel(page, '任务')
      const sel = opened ? await page.evaluate(() => {
        const el = document.querySelector('.hub-panel-head select')
        if (!el) return null
        const b = el.getBoundingClientRect()
        return { w: Math.round(b.width), h: Math.round(b.height) }
      }) : null
      check(`@${width} 任务状态筛选 ≥32 高`, !!sel && sel.h >= 32 && sel.h <= 36, sel ? `${sel.w}×${sel.h}` : '未找到')
      const overflow = await page.evaluate(() => ({ doc: document.documentElement.scrollWidth, vw: window.innerWidth }))
      check(`@${width} 无横向溢出`, overflow.doc === overflow.vw, `${overflow.doc}/${overflow.vw}`)
      await page.screenshot({ path: `${OUT}/03-targets-${width}.png` })
    } catch (e) {
      check(`@${width} 点击目标测量未抛异常`, false, String(e?.message ?? e).slice(0, 160))
    }
    await ctx.close()
  }
}

// ——— 4) AgentEditor 1280 / 640 ———
{
  console.log('\n[4] Agent 编辑器 1280 / 640')
  for (const width of [1280, 640]) {
    const ctx = await browser.newContext({ viewport: { width, height: 800 }, locale: 'zh-CN' })
    const page = await ctx.newPage()
    try {
      await boot(page)
      await enterHub(page)
      await pick(page, 'agent')
      const edit = page.locator('.hub-inspector button').filter({ hasText: '编辑智能体' }).first()
      if (await edit.count()) await edit.click({ timeout: 5000 })
      else {
        const rail = page.locator('.hub-detail-rail button[aria-label="概览"]').first()
        if (await rail.count()) { await rail.click(); await page.waitForTimeout(800) }
        const edit2 = page.locator('.hub-inspector button').filter({ hasText: '编辑智能体' }).first()
        if (await edit2.count()) await edit2.click({ timeout: 5000 })
      }
      await page.waitForTimeout(900)
      const m = await page.evaluate(() => {
        const r = (el) => { if (!el) return null; const b = el.getBoundingClientRect(); return { x: Math.round(b.x), y: Math.round(b.y), w: Math.round(b.width), h: Math.round(b.height) } }
        const modal = document.querySelector('.hub-modal')
        if (!modal) return null
        return {
          modal: r(modal), vw: innerWidth, vh: innerHeight,
          dangerInFoot: !!modal.querySelector('.hub-modal-foot .hub-danger'),
          dangerInCard: !!modal.querySelector('.hub-preview-card .hub-danger'),
          dangerInHead: !!modal.querySelector('.hub-modal-head .hub-danger'),
          foot: [...modal.querySelectorAll('.hub-modal-foot button')].map((b) => ({ t: (b.textContent || '').trim().slice(0, 8), ...r(b) })),
        }
      })
      check(`@${width} 编辑器已打开`, !!m)
      if (m) {
        check(`@${width} 对话框宽度 ≤720`, m.modal.w <= 720, `${m.modal.w}px`)
        check(`@${width} 水平居中`, Math.abs(m.modal.x - (m.vw - m.modal.w) / 2) <= 2, `x=${m.modal.x}`)
        if (width === 640) check('@640 仍按 92vw 收窄', Math.abs(m.modal.w - Math.round(640 * 0.92)) <= 2, `${m.modal.w}px`)
        check(`@${width} 删除按钮在 footer`, m.dangerInFoot && !m.dangerInCard && !m.dangerInHead,
          `foot=${m.dangerInFoot} card=${m.dangerInCard} head=${m.dangerInHead}`)
        check(`@${width} footer 顺序：删除 → 取消 → 保存`, m.foot.map((b) => b.t).join('|') === '删除智能体|取消|保存',
          m.foot.map((b) => b.t).join('|'))
      }
      await page.screenshot({ path: `${OUT}/04-editor-${width}.png` })
    } catch (e) {
      check(`@${width} 编辑器测量未抛异常`, false, String(e?.message ?? e).slice(0, 160))
    }
    await ctx.close()
  }
}

// ——— 5) Needs 提问卡：不再铺原始 JSON ———
{
  console.log('\n[5] ?hubFixture=needs 提问卡')
  const ctx = await browser.newContext({ viewport: { width: 1100, height: 900 }, locale: 'zh-CN' })
  const page = await ctx.newPage()
  try {
    await boot(page, 'hubFixture=needs')
    await enterHub(page)
    await pick(page, 'agent')
    check('Needs 面板打开', await openPanel(page, 'Needs'))
    const m = await page.evaluate(() => {
      const q = document.querySelector('.hub-need-question')
      const body = q?.querySelector('.hub-need-body')
      const others = [...document.querySelectorAll('.hub-need')].filter((n) => !n.classList.contains('hub-need-question'))
      return {
        question: q ? { h: Math.round(q.getBoundingClientRect().height), inputs: q.querySelectorAll('input').length, body: body ? (body.textContent || '').slice(0, 40) : null } : null,
        othersWithBody: others.filter((n) => n.querySelector('.hub-need-body')).length,
      }
    })
    check('提问卡存在且有 2 个输入框', !!m.question && m.question.inputs === 2, `inputs=${m.question?.inputs}`)
    check('提问卡不再渲染原始 JSON', !!m.question && m.question.body === null, m.question?.body ?? 'none')
    check('审批/失败/交接仍显示 body（3 张）', m.othersWithBody === 3, `got=${m.othersWithBody}`)
    await page.screenshot({ path: `${OUT}/05-needs-question.png` })
  } catch (e) {
    check('Needs 面板测量未抛异常', false, String(e?.message ?? e).slice(0, 160))
  }
  await ctx.close()
}

// ——— 6) system 主题跟随 prefers-color-scheme ———
{
  console.log('\n[6] system 主题跟随系统配色')
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 }, locale: 'zh-CN', colorScheme: 'dark' })
  const page = await ctx.newPage()
  try {
    await boot(page)
    await page.evaluate(() => window.__store.getState().setTheme('system'))
    await page.waitForTimeout(500)
    const dark = await page.evaluate(() => document.documentElement.getAttribute('data-theme'))
    check('system + 系统深色 → ink', dark === 'ink', dark)
    const inkPaper = await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--paper').trim())
    await page.emulateMedia({ colorScheme: 'light' })
    await page.waitForTimeout(600)
    const light = await page.evaluate(() => document.documentElement.getAttribute('data-theme'))
    const paper = await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--paper').trim())
    check('系统切浅色 → paper（实时跟随，不用重开）', light === 'paper', light)
    check('--paper 真的换了一档', inkPaper !== paper, `${inkPaper} → ${paper}`)
    await page.emulateMedia({ colorScheme: 'dark' })
    await page.waitForTimeout(600)
    const back = await page.evaluate(() => document.documentElement.getAttribute('data-theme'))
    check('系统切回深色 → ink（双向）', back === 'ink', back)
    await page.screenshot({ path: `${OUT}/06-system-theme.png` })
  } catch (e) {
    check('system 主题测量未抛异常', false, String(e?.message ?? e).slice(0, 160))
  }
  await ctx.close()
}

await browser.close()
console.log(`\n${failures === 0 ? '✅' : '❌'} P4-UI-11 细节回归：${failures === 0 ? '全部通过' : `${failures} 项失败`}（截图 ${OUT}）`)
process.exit(failures === 0 ? 0 : 1)
