#!/usr/bin/env node
// B1 回归（浏览器）：群里的 **Project 结果卡** 必须留下归属，且不能把发起者画成执行者。
//
// 现场 = bridge 的真实形状（hub_task_result.go）：
//   · 目标 workspace/session：refs.target_kind=workspace + target_workspace/target_session_id；
//   · 署名是**发起者**（sender 与 refs.origin_* 同一个智能体），没有 agent_id；
//   · 正文抬头 =〔Project <ws> / <sid>〕+ 摘要；账本 result.summary 不含抬头。
// 卡片必须画「<发起者> 发起 · <ws> / <sid>」、不能出现「… 派发」，抬头不能重复；
// 390 不横向溢出；「打开会话」切到那个 Project 会话（不是发起者 DM）。
//
// 为什么不用 ?hubFixture：mock.ts 是并行改动的热点文件（协调结论）。这里用
// scripts/hub-harness.mjs 把 GroupBubble 直接挂进真浏览器 + 真 CSS。
//
// 前置：`VITE_PORT=5180 npm run dev:vite`
// 运行：node scripts/check-hub-taskcard-workspace.mjs [--url http://localhost:5180/]
import { chromium } from 'playwright'
import { openHubHarness } from './hub-harness.mjs'

const argv = process.argv.slice(2)
const arg = (name, def) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def
}
const BASE_URL = arg('url', process.env.HUB_URL || 'http://localhost:5180/')

const WS = '/ws/agent-hub'
const SID = 's_proj_1'
const TEXT = `〔Project ${WS} / ${SID}〕\n三家对比表已完成：都支持导出，定价见 out/pricing.md`

// 与 bridge 写卡逐字对齐的 refs（hub_task_result.go taskResultCardRefs）。
const WS_REFS = {
  client_msg_id: 'task_result:task_ws1',
  task_id: 'task_ws1',
  status: 'completed',
  title: '做一份三家对比表',
  target_kind: 'workspace',
  target_workspace: WS,
  target_session_id: SID,
  origin_kind: 'group',
  origin_agent_id: 'ag_mockres1',
  origin_agent_name: '群派发A',
  duration_ms: 42_000,
  usage: { input: 9000, output: 1200, cost_priced: true, cost_usd: 0.02 },
  artifacts: [],
  error: '',
}

const ENTRY = `
import React from 'react'
import { createRoot } from 'react-dom/client'
import { GroupBubble } from './MessageBubble.tsx'
import { useHubStore } from './useHubStore.ts'
import { useAppStore } from '../store/useAppStore.ts'
import { HostPanelsProvider } from '../host/panels-desktop.tsx'

const WS = ${JSON.stringify(WS)}
const SID = ${JSON.stringify(SID)}
const TEXT = ${JSON.stringify(TEXT)}
const WS_REFS = ${JSON.stringify(WS_REFS)}

const roster = [
  { id: 'ag_mockres1', name: '群派发A', role: '派发' },
  { id: 'ag_mockwri2', name: 'Writer', role: '写作' },
]

// Project 结果卡：账本任务已加载（正常路径 —— 也就是抬头曾经丢掉的那条路）。
const wsTask = {
  id: 'task_ws1', title: '做一份三家对比表', status: 'completed',
  origin: { kind: 'group', group_id: 'grp_mock1', agent_id: 'ag_mockres1', agent_name: '群派发A' },
  target: { kind: 'workspace', workspace: WS, session_id: SID, group_id: 'grp_mock1' },
  result: { summary: '三家对比表已完成：都支持导出，定价见 out/pricing.md' },
}
// 对照卡：agent 目标任务（回归保护 —— 这张要照旧画「群派发A 派发」）。
const agentTask = {
  id: 'task_agent1', title: '把三家价格表写成两页简报', status: 'completed',
  origin: { kind: 'group', group_id: 'grp_mock1', agent_id: 'ag_mockres1', agent_name: '群派发A' },
  target: { kind: 'agent', agent_id: 'ag_mockwri2', group_id: 'grp_mock1' },
  result: { summary: '简报写好了' },
}

const wsMsg = {
  seq: 7, kind: 'task_result', sender: { kind: 'agent', agent_id: 'ag_mockres1' },
  text: TEXT, refs: WS_REFS, created_at: '2026-03-10T12:00:00Z',
}
const agentMsg = {
  seq: 8, kind: 'task_result', sender: { kind: 'agent', agent_id: 'ag_mockwri2' },
  text: '简报写好了', created_at: '2026-03-10T12:01:00Z',
  refs: {
    task_id: 'task_agent1', status: 'completed', agent_id: 'ag_mockwri2', target_kind: 'agent',
    origin_kind: 'group', origin_agent_id: 'ag_mockres1', origin_agent_name: '群派发A',
    title: '把三家价格表写成两页简报', duration_ms: 18_400, error: '',
  },
}

const externalMsg = {
  seq: 9, kind: 'text', sender: { kind: 'external', name: '飞书张三' },
  text: '客户在飞书问：这个版本什么时候发？', created_at: '2026-03-10T12:02:00Z',
}

useHubStore.setState({ tasks: [wsTask, agentTask] })
window.__spy = {}
useHubStore.setState({ closeHub: () => { window.__spy.closed = true } })
useAppStore.setState({ setActiveSession: (id, path) => { window.__spy.session = { id, path } } })

function App() {
  return React.createElement(React.Fragment, null,
    React.createElement('div', { id: 'card-ws' },
      React.createElement(GroupBubble, { msg: wsMsg, names: {}, roster, onSelectAgent: () => {} })),
    React.createElement('div', { id: 'card-agent' },
      React.createElement(GroupBubble, { msg: agentMsg, names: {}, roster, onSelectAgent: () => {} })),
    React.createElement('div', { id: 'ext-msg' },
      React.createElement(GroupBubble, { msg: externalMsg, names: {}, roster, onSelectAgent: () => {} })),
  )
}

// Host seam：组件不再直接读 useAppStore，任务卡「打开会话」走 context ——
// 夹具必须像真应用（main.tsx）一样挂桌面 provider，否则动作落到 no-op 兜底。
createRoot(document.getElementById('hub-root')).render(
  React.createElement(HostPanelsProvider, null, React.createElement(App)),
)
requestAnimationFrame(() => { window.__harnessReady = true })
`

const failed = []
const check = (cond, msg) => { if (!cond) failed.push(msg) }

const browser = await chromium.launch()
let page
try {
  page = await openHubHarness(browser, { contents: ENTRY, baseURL: BASE_URL })
  const wsCard = page.locator('#card-ws .event-card--task')
  await wsCard.waitFor({ timeout: 10000 })
  const text = await wsCard.innerText()

  // 1) 归属：ws 与 sid 都在卡上（不是只在群里那条消息正文里）。
  check(text.includes(WS), `结果卡显示 Project workspace（got=${text}）`)
  check(text.includes(SID), `结果卡显示 Project session（got=${text}）`)
  // 2) 不误标：workspace 目标没有执行者，不能出现「X 派发」。
  //    发起者名本身叫「群派发A」，所以只卡「空格 + 派发」这个动作后缀。
  check(!text.includes(' 派发'), `workspace 卡不画「派发」（got=${text}）`)
  check(text.includes('群派发A 发起'), `workspace 卡把发起者写成「发起」（got=${text}）`)
  // 3) 抬头只画一次：meta 行已经有了，正文里不能再贴一遍〔Project …〕。
  check(!text.includes('〔Project'), `抬头不重复进正文（got=${text}）`)
  check(text.includes('三家对比表已完成'), `正文保留摘要（got=${text}）`)
  await page.screenshot({ path: '/tmp/hub-taskcard-workspace-1280.png' })

  // 4) 对照：agent 目标卡照旧「群派发A 派发」+ 执行者。
  const agentText = await page.locator('#card-agent .event-card--task').innerText()
  check(agentText.includes('Writer') && agentText.includes('群派发A 派发'),
    `agent 目标卡保持「<执行者> + <派发者> 派发」（got=${agentText}）`)
  check(!agentText.includes('发起'), `agent 目标卡不写「发起」（got=${agentText}）`)

  // 4b) 渠道外部用户：不能冒充「你」，昵称与头像首字都来自 sender.name。
  const ext = page.locator('#ext-msg')
  const who = await ext.locator('.user-who').innerText()
  check(who === '飞书张三', `外部用户显示渠道昵称（got=${who}）`)
  const avatar = await ext.locator('.owner-avatar').innerText()
  check(avatar === '飞', `外部用户头像取昵称首字（got=${avatar}）`)
  check((await ext.locator('.user-bubble').innerText()).includes('什么时候发'), '外部用户正文进入气泡')

  // 5) 「打开会话」= 切到那个 Project 会话（不是发起者 DM）。
  await wsCard.locator('button', { hasText: '打开会话' }).click()
  const spy = await page.evaluate(() => window.__spy)
  check(spy.closed === true, `打开会话先退出 Hub（spy=${JSON.stringify(spy)}）`)
  check(spy.session?.id === SID && spy.session?.path === WS,
    `打开会话切到 Project 会话（spy=${JSON.stringify(spy)}）`)

  // 6) 390：卡片不横向溢出（路径可以长，靠单行省略）。
  await page.setViewportSize({ width: 390, height: 844 })
  await page.waitForTimeout(200)
  const docW = await page.evaluate(() => document.documentElement.scrollWidth)
  check(docW <= 390, `390 无横向溢出（doc=${docW}）`)
  const cardBox = await wsCard.boundingBox()
  check(!!cardBox && cardBox.width <= 390, `390 卡片宽度在视口内（box=${JSON.stringify(cardBox)}）`)
  await page.screenshot({ path: '/tmp/hub-taskcard-workspace-390.png' })

  const errs = page.__hubErrors ?? []
  check(errs.length === 0, `无 console error / pageerror：${errs.slice(0, 3).join(' | ')}`)
} finally {
  await browser.close()
}

if (failed.length) {
  console.error(`❌ Project 结果卡归属回归失败 ${failed.length} 条：`)
  for (const f of failed) console.error(`  · ${f}`)
  process.exit(1)
}
console.log('✅ Project 结果卡：ws/sid 归属、无「派发」误标、抬头不重复、390、打开会话 全部通过')
console.log('   截图 /tmp/hub-taskcard-workspace-1280.png · /tmp/hub-taskcard-workspace-390.png')
