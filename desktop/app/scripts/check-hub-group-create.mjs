#!/usr/bin/env node
// 左栏「群组」页签的新建入口 + 乐观清空草稿：浏览器验收（P4-UI / HUB-CLEAR-01）。
//
// 用户报障（1）：群组页签只有空态文案，没有任何创建入口 —— 顶部 ＋ 永远打开「新建智能体」。
// 用户报障（2）：发送后消息已经乐观出现在时间线，输入框却要等 hub_send/hub_group_send
//              的响应回来才清（真 bridge 慢时很明显）。
//
// 用法（默认复用 5180，不占 5173；另开终端起 dev server）：
//   cd desktop/app && npx vite --port 5180
//   node scripts/check-hub-group-create.mjs
//   node scripts/check-hub-group-create.mjs --url http://localhost:5196/
//   node scripts/check-hub-group-create.mjs --no-shot
//
// 判据（任一失败 → exit 1；dev server 不可用 → exit 2）：
//   1. 群组页签的顶部 ＋ 打开的是「新建群组」弹层（不是「新建智能体」）
//   2. 填名称 + 勾 2 个成员 → 创建：弹层关闭、左栏列表出现新群、群视图打开且群头是新群名
//   3. 智能体页签的顶部 ＋ 仍然打开 AgentEditor（没有被群组入口顶掉）
//   4. mock.loadDelayMs=600 时，按下回车后 100ms 内 textarea 已空（乐观清空）
//   5. 390×844：弹层不横向溢出（documentElement / 弹层盒 / 成员列表都不超宽）
//   6. Esc 关闭弹层；全程无 console error / pageerror
import { chromium } from 'playwright'
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'
import { openHubHarness } from './hub-harness.mjs'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const BASE = arg('url', process.env.HUB_GROUP_CREATE_URL || 'http://localhost:5180/')
const OUT = arg('out', '/tmp/hub-group-create')
const SHOT = !argv.includes('--no-shot')
const NAME = '浏览器验收群'

const failed = []
const errors = []
const check = (cond, msg) => { if (!cond) failed.push(msg) }
if (SHOT) mkdirSync(OUT, { recursive: true })

// —— dev server preflight：没起 → exit 2（与「断言不达标」的 exit 1 区分开）——
async function preflight(url) {
  try {
    const res = await fetch(url, { signal: AbortSignal.timeout(4000) })
    if (!res.ok) return `HTTP ${res.status}`
    const html = await res.text()
    if (!html.includes('id="root"')) return '响应里没有 #root（不是 vite dev 页面？）'
    return null
  } catch (e) {
    return e?.cause?.code || e?.message || String(e)
  }
}

const down = await preflight(BASE)
if (down) {
  console.error(`❌ dev server 不可用：${BASE}（${down}）`)
  console.error('   先另开一个终端起 dev server（不要占 5173）：')
  console.error('     cd desktop/app && npx vite --port 5180 --strictPort')
  process.exit(2)
}

const browser = await chromium.launch()
const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()) })
page.on('pageerror', (e) => errors.push(`pageerror ${e.message}`))
// 发送类命令的人为延迟（mock 读这个键）：600ms 的窗口里才能断言「100ms 内已清空」。
// addInitScript 在页面脚本之前跑，localStorage 已经可用（同源）。
await page.addInitScript(() => {
  try { localStorage.setItem('mock.loadDelayMs', '600') } catch { /* 隐私模式：忽略 */ }
})

const shot = async (name) => { if (SHOT) await page.screenshot({ path: join(OUT, name) }) }

// 390 档的夹具入口：只挂 GroupEditor（真组件 + 真 hub.css），store 里塞两个智能体供勾选。
// 桌面应用在 390 下会卸掉左栏，弹层跟着消失，所以窄屏只能这样单独挂。
const HARNESS_ENTRY = `
import React from 'react'
import { createRoot } from 'react-dom/client'
import GroupEditor from './GroupEditor.tsx'
import { useHubStore } from './useHubStore.ts'

useHubStore.setState({
  agents: [
    { id: 'ag_fixture_1', name: 'Researcher', role: '竞品研究员', provider: 'deepseek', model: 'deepseek-v4-flash' },
    { id: 'ag_fixture_2', name: 'Writer', role: '写作', provider: 'deepseek', model: 'deepseek-v4-pro' },
  ],
  groups: [],
  error: null,
})

createRoot(document.getElementById('hub-root')).render(
  React.createElement(GroupEditor, { onClose: () => { window.__groupEditorClosed = true } }),
)
requestAnimationFrame(() => { window.__harnessReady = true })
`

/** 等左栏挂好（冷启动的 vite 可能先 504，重载一次就绪）。 */
async function openApp() {
  for (let i = 0; i < 3; i++) {
    await page.goto(BASE, { waitUntil: 'domcontentloaded' })
    await page.waitForTimeout(1500)
    if (await page.locator('.hub-side-tabs').count()) return
    if (await page.locator('button[aria-label="智能体"]').count()) {
      await page.locator('button[aria-label="智能体"]').first().click({ timeout: 8000 }).catch(() => {})
      await page.waitForTimeout(700)
      if (await page.locator('.hub-side-tabs').count()) return
    }
  }
  throw new Error('页面没挂载出 Hub 左栏（dev server 未就绪 / 504 Outdated Optimize Dep）')
}

const tabButton = (label) => page.locator('.hub-side-tabs button').filter({ hasText: label }).first()
const groupModal = () => page.locator('.hub-group-modal')

/** 切页签并确认真的切过去了（刚挂载时第一次点可能落在重渲染中间，重试一次）。 */
async function selectTab(label) {
  const tab = tabButton(label)
  for (let i = 0; i < 2; i++) {
    if (await tab.getAttribute('aria-selected') === 'true') return
    await tab.click()
    await page.waitForTimeout(250)
  }
  throw new Error(`左栏切不到「${label}」页签`)
}

/** 点顶部 ＋ 并等弹层出现（同一次点击在重渲染边界上可能丢失，重试一次）。 */
async function openGroupEditor() {
  await page.locator('.hub-side-add').click()
  await groupModal().waitFor({ timeout: 3000 }).catch(async () => {
    await page.locator('.hub-side-add').click()
    await groupModal().waitFor({ timeout: 5000 })
  })
}

try {
  await openApp()

  // —— 1) 群组页签的 ＋ = 新建群组 ——
  await selectTab('群组')
  check(await page.locator('.hub-side-add').getAttribute('aria-label') === '新建群组',
    '群组页签的顶部 ＋ 是「新建群组」')
  await openGroupEditor()
  check(await groupModal().locator('.hub-modal-title').innerText() === '新建群组', '弹层标题是「新建群组」')
  await shot('modal.png')

  // —— 2) 填名称 + 勾 2 个成员 + 创建 ——
  const nameInput = groupModal().locator('.hub-group-field input').first()
  await nameInput.fill(NAME)
  const boxes = groupModal().locator('.hub-group-member input[type="checkbox"]')
  const memberRows = await groupModal().locator('.hub-group-member').count()
  check(memberRows >= 2, `成员列表至少有 2 个可勾选的智能体（got=${memberRows}）`)
  await boxes.nth(0).check()
  await boxes.nth(1).check()

  const sels = groupModal().locator('.hub-group-field .sel')
  const ownerValue = await sels.nth(0).getAttribute('data-value')
  check(ownerValue !== '', '群主默认选中第一个成员')
  await sels.nth(0).locator('.sel-btn').click()
  const ownerOptions = await sels.nth(0).locator('.sel-opt').count()
  check(ownerOptions === 2, `群主选项 = 已选成员数（got=${ownerOptions}）`)
  await sels.nth(0).locator('.sel-btn').click() // 收起下拉（Esc 会被弹层当成关闭整个 modal）
  const modeValue = await sels.nth(1).getAttribute('data-value')
  check(modeValue === 'ask', `协作模式默认 ask（got=${modeValue}）`)
  await shot('modal-filled.png')

  await groupModal().locator('.hub-modal-foot .hub-primary').click()
  await groupModal().waitFor({ state: 'detached', timeout: 10000 })

  const groupRow = page.locator('.hub-group-row').filter({ hasText: NAME }).first()
  await groupRow.waitFor({ timeout: 8000 })
  check(await page.locator('.hub-group-row').filter({ hasText: NAME }).count() === 1,
    '左栏群组列表出现新群（且只有一行）')
  check(await groupRow.getAttribute('aria-current') === 'true', '新群在左栏是选中态')

  const groupView = page.locator('.hub-group-view')
  await groupView.waitFor({ timeout: 8000 })
  const headName = await groupView.locator('.chat-title .name').first().innerText()
  check(headName.trim() === NAME, `群视图已打开且群头是新群名（got=${JSON.stringify(headName)}）`)
  const headSub = await groupView.locator('.chat-sub').first().innerText()
  check(headSub.includes('2 名成员'), `群头显示 2 名成员（got=${JSON.stringify(headSub)}）`)
  await shot('group-created.png')

  // —— 3) 乐观清空草稿：600ms 的响应窗口里，100ms 内输入框必须已经空了 ——
  const clear = await page.evaluate(async () => {
    const ta = document.querySelector('.hub-group-view .composer textarea')
    if (!ta) return { ok: false }
    const setValue = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set
    setValue.call(ta, '延迟清空测试')
    ta.dispatchEvent(new Event('input', { bubbles: true }))
    await new Promise((r) => setTimeout(r, 60)) // 让 React 收下这次草稿
    const before = ta.value
    const t0 = performance.now()
    ta.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }))
    await new Promise((r) => setTimeout(r, 100))
    return {
      ok: true, before, after: ta.value, elapsed: Math.round(performance.now() - t0),
      optimistic: document.body.innerText.includes('延迟清空测试'),
    }
  })
  check(clear.ok, '群视图里有 composer 输入框')
  check(clear.before === '延迟清空测试', `发送前输入框有正文（got=${JSON.stringify(clear.before)}）`)
  check(clear.after === '', `发送后 100ms 内输入框已清空（got=${JSON.stringify(clear.after)}，elapsed=${clear.elapsed}ms）`)
  check(clear.optimistic, '消息已经乐观出现在时间线上（输入框没有等它）')

  // —— 4) 智能体页签的 ＋ 仍是 AgentEditor（群组入口不能顶掉它）——
  await selectTab('智能体')
  check(await page.locator('.hub-side-add').getAttribute('aria-label') === '新建智能体',
    '智能体页签的顶部 ＋ 仍是「新建智能体」')
  await page.locator('.hub-side-add').click()
  const agentModal = page.locator('.hub-modal:not(.hub-group-modal)')
  await agentModal.waitFor({ timeout: 5000 })
  check(await groupModal().count() === 0, '智能体页签的 ＋ 不会打开群组弹层')
  await agentModal.locator('.hub-modal-head .hub-link').click()
  await agentModal.waitFor({ state: 'detached', timeout: 5000 })

  // —— 5) Esc 关闭 + 背景滚动恢复 ——
  await selectTab('群组')
  await openGroupEditor()
  const lockedOverflow = await page.evaluate(() => document.body.style.overflow)
  check(lockedOverflow === 'hidden', `弹层打开时锁背景滚动（body.overflow=${JSON.stringify(lockedOverflow)}）`)
  await page.keyboard.press('Escape')
  await groupModal().waitFor({ state: 'detached', timeout: 5000 })
  check(await groupModal().count() === 0, 'Esc 关闭弹层')
  const bodyOverflow = await page.evaluate(() => document.body.style.overflow)
  check(bodyOverflow === '', `弹层关闭后恢复背景滚动（body.overflow=${JSON.stringify(bodyOverflow)}）`)

  check(errors.length === 0, `无 console error / pageerror：${errors.slice(0, 3).join(' | ')}`)

  // —— 6) 390×844：弹层不横向溢出 ——
  // 桌面应用在 390 下会卸掉左栏（HubSidebar 整块不渲染），弹层也跟着消失。所以这一档用
  // hub-harness 直接挂 GroupEditor：同一份 hub.css、同一个组件，只是不含应用外壳。
  const harness = await openHubHarness(browser, {
    contents: HARNESS_ENTRY,
    baseURL: BASE,
    width: 390,
    height: 844,
  })
  try {
    const modal = harness.locator('.hub-group-modal')
    await modal.waitFor({ timeout: 10000 })
    const narrow = await harness.evaluate(() => {
      const box = document.querySelector('.hub-group-modal')?.getBoundingClientRect()
      const members = document.querySelector('.hub-group-members')
      return {
        docW: document.documentElement.scrollWidth,
        modalW: box?.width ?? -1,
        left: box?.left ?? -1,
        right: box?.right ?? -1,
        membersOverflow: members ? members.scrollWidth - members.clientWidth : -1,
      }
    })
    check(narrow.docW <= 390, `390 无横向溢出（doc=${narrow.docW}）`)
    check(narrow.modalW > 0 && narrow.modalW <= 390, `390 弹层宽度不超视口（w=${narrow.modalW}）`)
    check(narrow.left >= 0 && narrow.right <= 390, `390 弹层左右都在视口内（left=${narrow.left} right=${narrow.right}）`)
    check(narrow.membersOverflow <= 1, `390 成员行不横向溢出（overflow=${narrow.membersOverflow}）`)
    if (SHOT) await harness.screenshot({ path: join(OUT, 'modal-390.png') })
    check((harness.__hubErrors ?? []).length === 0,
      `390 夹具无 console error：${(harness.__hubErrors ?? []).slice(0, 3).join(' | ')}`)
  } finally {
    await harness.close().catch(() => {})
  }
} finally {
  await page.close().catch(() => {})
  await browser.close().catch(() => {})
}

if (failed.length) {
  console.error(`\n❌ 新建群组 / 乐观清空：${failed.length} 项不达标`)
  for (const f of failed) console.error(`   · ${f}`)
  process.exit(1)
}
console.log('✅ 新建群组：入口 / 创建 / 群视图 / 390 / Esc / 无 console error 全部通过')
console.log('✅ 乐观清空草稿：mock 延迟 600ms 下，回车后 100ms 内输入框已空')
if (SHOT) console.log(`   截图：${OUT}/modal.png ${OUT}/group-created.png ${OUT}/modal-390.png`)
