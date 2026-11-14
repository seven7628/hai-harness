#!/usr/bin/env node
// Hub 单聊/群内联「任务等待你」卡（P4-UI-15）。
//
// 前置：mock dev server（默认 5180，不要占 5173）。
// 运行：node scripts/check-hub-inline-needs.mjs [--url http://localhost:5180/]
//
// 断言：
//   · ?hubFixture=needs 群内出现审批内联卡，带 Researcher / workspace / Project session / 子智能体名
//   · 批准后卡片消失（走 hub_need_resolve，不写 hub_message）
//   · 有 group_id 的 need 不出现在 DM 内联卡（作用域隔离）
//   · 390 无横向溢出；无 console error
//   · 提问卡（N3，组件夹具）：多选可同时勾多个、提交按「；」拼接、选项预览显示在选项下、
//     390 不溢出。mock 的 needs fixture 没有多选/预览题，这里用 scripts/hub-harness.mjs
//     直接挂 HubInlineNeeds —— 不为一条断言去动 mock.ts。
import { chromium } from 'playwright'
import { openHubHarness } from './hub-harness.mjs'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const BASE_URL = arg('url', process.env.HUB_INLINE_NEEDS_URL || 'http://localhost:5180/')
const FIXTURE_URL = new URL(BASE_URL)
FIXTURE_URL.searchParams.set('hubFixture', 'needs')

const failed = []
const errors = []
const check = (cond, msg) => { if (!cond) failed.push(msg) }

const browser = await chromium.launch()
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()) })
page.on('pageerror', (e) => errors.push(`pageerror ${e.message}`))

async function openHub(url = FIXTURE_URL.toString()) {
  await page.goto(url, { waitUntil: 'domcontentloaded' })
  await page.waitForTimeout(1500)
  if (!(await page.locator('.hub-side-row').count())) {
    await page.locator('button[aria-label="智能体"]').first().click({ timeout: 8000 }).catch(() => {})
    await page.waitForTimeout(700)
  }
  await page.locator('.hub-side-row').first().waitFor({ timeout: 10000 })
}

async function openGroup() {
  await openHub()
  const tab = page.locator('.hub-side-tabs button').filter({ hasText: '群组' }).first()
  if ((await tab.getAttribute('aria-selected')) !== 'true') await tab.click({ timeout: 6000 })
  await page.waitForTimeout(250)
  await page.locator('.hub-side-row').first().click({ timeout: 8000 })
  await page.waitForTimeout(900)
  await page.locator('.hub-demo .composer textarea').first().waitFor({ timeout: 8000 })
}

async function openDM() {
  await openHub()
  const tab = page.locator('.hub-side-tabs button').filter({ hasText: '智能体' }).first()
  if ((await tab.getAttribute('aria-selected')) !== 'true') await tab.click({ timeout: 6000 })
  await page.waitForTimeout(250)
  await page.locator('.hub-side-row').first().click({ timeout: 8000 })
  await page.waitForTimeout(900)
  await page.locator('.hub-demo .composer textarea').first().waitFor({ timeout: 8000 })
}

// 1) 群内审批内联卡 + 归属
await openGroup()
const card = page.locator('.hub-inline-need--approval').first()
await card.waitFor({ timeout: 10000 })
const text = await card.textContent()
check(String(text).includes('Researcher'), `群内联卡显示发起智能体（got=${text}）`)
check(String(text).includes('workspace'), `群内联卡显示 Project workspace（got=${text}）`)
check(String(text).includes('s_mock_project'), `群内联卡显示 Project session（got=${text}）`)
check(String(text).includes('Researcher-sub'), `群内联卡显示子智能体名（got=${text}）`)
check(await page.locator('.hub-inline-need--question').count() >= 1, '群内联提问卡也出现')
// 视觉口径：中性卡身 + 1px 四边同色边框 + 8px 圆角；类型色只留在 chip 上。
const needStyle = await card.evaluate((el) => {
  const s = getComputedStyle(el)
  return { left: s.borderLeftWidth, top: s.borderTopWidth, radius: s.borderRadius, bg: s.backgroundColor }
})
check(needStyle.left === needStyle.top && needStyle.left === '1px',
  `内联 need 卡不再有左侧 3px 色条（left=${needStyle.left}, top=${needStyle.top}）`)
check(needStyle.radius === '8px', `内联 need 卡圆角统一为 8px（got=${needStyle.radius}）`)
const inputStyle = await page.locator('.hub-inline-need--question input').first().evaluate((el) => {
  const s = getComputedStyle(el)
  return { height: s.height, border: s.borderTopColor, bg: s.backgroundColor }
})
check(inputStyle.height === '32px' && inputStyle.border !== 'rgba(0, 0, 0, 0)',
  `提问输入框有可见边框且高度 32px（height=${inputStyle.height}, border=${inputStyle.border}）`)
await page.screenshot({ path: '/tmp/hub-inline-needs-group.png' })

// 批准：卡片消失，且没有新增 hub 消息（只走 need resolve）
const beforeMsgs = await page.locator('.hub-demo .msg-row').count()
await card.locator('button').filter({ hasText: '批准' }).first().click()
await page.waitForTimeout(900)
check(await page.locator('.hub-inline-need--approval').count() === 0, '批准后审批内联卡消失')
check(await page.locator('.hub-demo .msg-row').count() === beforeMsgs, '内联卡不写 hub 消息（只展示）')
// 提交后必须告诉用户落点：不是写 Hub 消息，而是 resolve 到具体的 Project session。
await page.locator('.hub-notice').first().waitFor({ timeout: 4000 })
const notice = await page.locator('.hub-notice').first().innerText()
check(notice.includes('已提交到') && notice.includes('s_mock_project'),
  `审批提交后提示具体 Project session（got=${notice}）`)

// 2) 群 need 不出现在 DM
await openDM()
await page.waitForTimeout(600)
check(await page.locator('.hub-inline-need').count() === 0, '带 group_id 的 need 不泄漏到 DM 内联卡')

// 3) 390 无横向溢出
await openGroup()
await page.setViewportSize({ width: 390, height: 844 })
await page.waitForTimeout(300)
check(await page.locator('.hub-inline-need').count() >= 1, '390 下内联卡仍渲染')
const docW = await page.evaluate(() => document.documentElement.scrollWidth)
check(docW <= 390, `390 无横向溢出（doc=${docW}）`)
await page.screenshot({ path: '/tmp/hub-inline-needs-390.png' })

check(errors.length === 0, `无 console error / pageerror：${errors.slice(0, 3).join(' | ')}`)

// 4) 提问卡的多选 + 预览（N3）：组件夹具，不依赖 mock fixture。
//    答案口径必须与 AskUserQuestionPanel 一致：多选 = 勾选项用「；」拼接。
const INLINE_ENTRY = `
import React from 'react'
import { createRoot } from 'react-dom/client'
import HubInlineNeeds from './HubInlineNeeds.tsx'
import { useHubStore } from './useHubStore.ts'
import { useAppStore } from '../store/useAppStore.ts'
import { HostPanelsProvider } from '../host/panels-desktop.tsx'

const NEED = {
  id: 'need_multi', kind: 'question', status: 'pending', group_id: 'grp_1', task_id: 'task_multi',
  origin_agent_name: '群派发A', target_workspace: '/mock/workspace', target_session_id: 's_mock_project',
  title: '整理简报前有两个问题',
  body: JSON.stringify([
    {
      id: 'q1', question: '这份简报要送谁看？', multi: true,
      options: [{ label: '客户' }, { label: '内部团队' }, { label: '投资人' }],
    },
    {
      id: 'q2', question: 'C 家没有公开单价，怎么写？',
      options: [
        { label: '写「未公开」', preview: '价格列：未公开\\n备注：需销售询价' },
        { label: '用估算价', preview: '价格列：$45（估算）\\n备注：非官方报价' },
      ],
    },
  ]),
}

window.__hubResolve = null
useHubStore.setState({
  needs: [NEED],
  resolveNeed: async (id, resolution, answers) => { window.__hubResolve = { id, resolution, answers } },
})
useAppStore.setState({ setRightExpanded: () => {} })

// 与真应用同一条 host seam（组件读 context，不直接读 store）。
createRoot(document.getElementById('hub-root')).render(
  React.createElement(
    HostPanelsProvider,
    null,
    React.createElement(HubInlineNeeds, { scope: { kind: 'group', groupId: 'grp_1' } }),
  ),
)
requestAnimationFrame(() => { window.__harnessReady = true })
`
const hp = await openHubHarness(browser, { contents: INLINE_ENTRY, baseURL: BASE_URL })
try {
  const questions = hp.locator('.hub-inline-need-question')
  await questions.first().waitFor({ timeout: 10000 })
  check(await questions.count() === 2, `夹具渲染两道题（got=${await questions.count()}）`)

  const q1 = questions.nth(0)
  const q2 = questions.nth(1)
  const q1Text = await q1.innerText()
  check(q1Text.includes('可多选'), `多选题标出「可多选」（got=${q1Text}）`)
  // 预览：还没选时先显示第一个带预览的选项（面板一出现就看得见备选长什么样）。
  const preview = q2.locator('.hub-inline-need-preview')
  await preview.waitFor({ timeout: 5000 })
  check((await preview.innerText()).includes('价格列：未公开'), `选项预览默认显示首个备选（got=${await preview.innerText()}）`)

  // 多选 = 切换勾选，不是覆盖：先点「客户」，再点「内部团队」，两个都得亮着。
  const pick = async (q, label) => q.locator('button', { hasText: label }).first().click()
  await pick(q1, '客户')
  await pick(q1, '内部团队')
  await hp.waitForTimeout(120)
  check(await q1.locator('button', { hasText: '客户' }).first().getAttribute('aria-pressed') === 'true', '多选保留第一个勾选')
  check(await q1.locator('button', { hasText: '内部团队' }).first().getAttribute('aria-pressed') === 'true', '多选可再勾第二个')

  // 单选仍是一选一，预览跟着选中项走。
  await pick(q2, '用估算价')
  await hp.waitForTimeout(120)
  check((await q2.locator('.hub-inline-need-preview').innerText()).includes('价格列：$45（估算）'), '预览跟着选中项切换')
  check(await q2.locator('button', { hasText: '写「未公开」' }).first().getAttribute('aria-pressed') !== 'true', '单选换选后旧选项弹起')

  // 提交：多选拼成「客户；内部团队」，单选原值。
  await hp.locator('.hub-inline-need footer button', { hasText: '提交回答' }).click()
  await hp.waitForTimeout(200)
  const resolved = await hp.evaluate(() => window.__hubResolve)
  check(resolved?.resolution === 'answered', `提交走需要 answered（got=${JSON.stringify(resolved)}）`)
  check(resolved?.answers?.q1 === '客户；内部团队', `多选按「；」拼接（got=${JSON.stringify(resolved?.answers)}）`)
  check(resolved?.answers?.q2 === '用估算价', `单选原值（got=${JSON.stringify(resolved?.answers)}）`)
  await hp.screenshot({ path: '/tmp/hub-inline-needs-multi.png' })

  // 390：预览里是 ASCII 版面（含长行），块内自己滚动，不能把卡片撑出横向溢出。
  await hp.setViewportSize({ width: 390, height: 844 })
  await hp.waitForTimeout(150)
  const hpDocW = await hp.evaluate(() => document.documentElement.scrollWidth)
  check(hpDocW <= 390, `390 内联提问卡无横向溢出（doc=${hpDocW}）`)
  await hp.screenshot({ path: '/tmp/hub-inline-needs-multi-390.png' })

  const hpErrs = hp.__hubErrors ?? []
  check(hpErrs.length === 0, `提问卡夹具无 console error：${hpErrs.slice(0, 3).join(' | ')}`)
} finally {
  await hp.close()
}

await browser.close()

if (failed.length) {
  console.error(`❌ Hub 内联 need 回归失败 ${failed.length} 条：`)
  for (const f of failed) console.error(`  · ${f}`)
  process.exit(1)
}
console.log('✅ Hub 内联 need：群归属、审批、隔离、390、多选+预览通过')
