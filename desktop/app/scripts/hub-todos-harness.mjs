#!/usr/bin/env node
// Hub 内联「Project 待办」卡的浏览器夹具（场景2：DM/群内的 ToDo 内联卡）。
//
// 为什么还要在 hub-harness 之上再包一层：`openHubHarness` 把组件直接挂在 `#hub-root`
// 里，页面里没有 `.messages` / `.composer-wrap` —— 而这张卡最关键的约束是「卡片不能把
// 消息区压成 0」（390 下的硬要求），没有真 flex 链就量不出 `.messages` 的高度。
// 这里的做法：同一套 harness 打包/挂载，只是把组件放进**真的会话骨架**里
// （`.messages` + `.composer-wrap` + `.composer`），并补上组件自己的 CSS 文件。
//
// 样式全部从 dev server 取 `?direct`（index.css / demo.css / hub.css / HubInlineTodos.css
// = 线上同一份），不存副本；状态由调用方通过 `window.__hubStore.setState()` 注入。
//
// 前置：调用方自己起 dev server（见各 check-hub-* 脚本），baseURL 传进来。
import { openHubHarness, APP_DIR } from './hub-harness.mjs'

export { APP_DIR }

/**
 * 会话骨架。用 React.createElement 而不是 JSX：入口是一段字符串，模板字符串里的
 * `${` 会被外层插值吃掉（同理不要写 CSS 的 ${}）。
 *
 * 两条历史 `.msg-row` 是「只读」断言的基线：内联卡不写 hub 消息，条数必须不变。
 */
const SHELL_ENTRY = `
import React from 'react'
import { createRoot } from 'react-dom/client'
import HubInlineTodos from './HubInlineTodos.tsx'
import { useHubStore } from './useHubStore.ts'
import { getTransport } from '../transport'

const root = document.getElementById('hub-root')
// hub.css 的会话骨架：.hub-demo.main 才是 flex 列（> .messages flex:1 / > .composer-wrap
// flex:0 0 auto）。少了这个类，#messages 没有可涨的高度，390 的高度断言会假绿。
root.classList.add('main')

// transport 探针：内联卡是**只读**的，一条 hub_send / hub_group_send 都不该发出去。
const tr = getTransport()
const sent = []
const origSend = tr.send.bind(tr)
tr.send = (cmd) => { sent.push(String(cmd && cmd.type ? cmd.type : '')); return origSend(cmd) }
window.__hubSent = sent
window.__hubStore = useHubStore

const DM = { kind: 'dm', agentId: 'ag_a' }
let switchScope = () => {}

function Shell() {
  const [scope, setScope] = React.useState(DM)
  switchScope = setScope
  React.useEffect(() => { window.__harnessReady = true }, [])
  return React.createElement(React.Fragment, null,
    React.createElement('div', { className: 'messages' },
      React.createElement('div', { className: 'messages-inner' },
        React.createElement('div', { className: 'msg-row user' },
          React.createElement('div', { className: 'msg-main' },
            React.createElement('div', { className: 'user-bubble' }, '历史消息 1'))),
        React.createElement('div', { className: 'msg-row agent' },
          React.createElement('div', { className: 'msg-main' },
            React.createElement('div', { className: 'agent-text' }, '历史消息 2')))),
    ),
    React.createElement('div', { className: 'composer-wrap' },
      React.createElement('div', { className: 'composer-inner' },
        React.createElement(HubInlineTodos, { scope }),
        React.createElement('div', { className: 'composer' },
          React.createElement('textarea', { placeholder: '给 A 发消息…' }),
          React.createElement('button', { className: 'send-btn', type: 'button' }, '发送')))))
}

// 作用域切换（DM ↔ 群）：同一张页面里验「群任务不进 DM、DM 任务不进群」。
window.__setScope = (scope) => switchScope(scope)

createRoot(root).render(React.createElement(Shell))
`

/**
 * 挂载内联待办卡，返回 page。
 *
 * opts.width/height 决定初始视口（390 的断言可以先用大视口，再 setViewportSize）。
 * 组件 CSS 在 harness 之后追加：`openHubHarness` 只 <link> 了 index/demo/hub.css
 * （它面向的是「把组件塞进 .hub-demo」），HubInlineTodos.css 是这张卡自己的文件。
 */
export async function mountInlineTodos(browser, {
  baseURL = process.env.HUB_URL || 'http://localhost:5180/',
  width = 1280,
  height = 900,
  theme = 'ink',
} = {}) {
  const page = await openHubHarness(browser, {
    contents: SHELL_ENTRY, baseURL, width, height, theme,
  })
  // ?direct = 让 vite 原样吐 CSS（与线上同一份文件，不经过 HMR 注入的 style 标签）。
  await page.addStyleTag({ url: new URL('src/hub/HubInlineTodos.css?direct', baseURL).toString() })
  return page
}

/** 注入 store 状态（tasks / todoSnapshots / runNames …）并等一帧渲染。 */
export async function injectHubState(page, patch) {
  await page.evaluate((p) => { window.__hubStore.setState(p) }, patch)
  await page.waitForTimeout(60)
}

/** 切作用域并等一帧（React setState → DOM）。 */
export async function setScope(page, scope) {
  await page.evaluate((s) => { window.__setScope(s) }, scope)
  await page.waitForTimeout(60)
}

/** 送一条线上形状的事件进 store（hub_task_update / tool_run_end …）。 */
export async function emitHubEvent(page, line) {
  await page.evaluate((l) => { window.__hubStore.getState().handleEvent(JSON.stringify(l)) }, line)
  await page.waitForTimeout(60)
}

/** 已发出的 hub 命令类型（transport 探针）。 */
export async function sentHubCommands(page) {
  return page.evaluate(() => window.__hubSent.slice())
}
