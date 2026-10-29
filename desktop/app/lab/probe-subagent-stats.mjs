// 子智能体视角的数据口径探针（真浏览器 + mock 传输层）：
//   聚焦某个子 agent 后，右栏/composer 的 ctx 读数与运行时长到底取的是谁的数据。
// 用法：先起 vite dev（5288），再 node lab/probe-subagent-stats.mjs
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const b = await Browser.launch({ width: 1440, height: 1000 })
const page = await b.newPage({ width: 1440, height: 1000 })
await page.goto(URL, { settleMs: 3500 })

// 发一条消息（与 verify-runtime-skin 的 say 同口径：读回 value 再回车）
const say = async (text) => {
  await page.eval((t) => {
    const ta = document.querySelector('.composer textarea') || document.querySelector('textarea')
    ta.focus()
    const set = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set
    set.call(ta, t)
    ta.dispatchEvent(new Event('input', { bubbles: true }))
  }, text)
  await wait(200)
  await page.eval(() => {
    const ta = document.querySelector('.composer textarea') || document.querySelector('textarea')
    ta.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true }))
  })
  await wait(500)
}
const waitFor = async (sel, ms = 30000) => {
  const t0 = Date.now()
  while (Date.now() - t0 < ms) {
    if (await page.eval((s) => !!document.querySelector(s), sel)) return true
    await wait(300)
  }
  return false
}

await say('修复登录接口返回 401')
// ① 先答题（mock 的 normalScript 先 ask_user，答完才进编辑 + 审批）
if (await waitFor('.dock .ask', 40000)) {
  await page.eval(() => {
    for (const card of document.querySelectorAll('.dock .ask')) {
      const opt = card.querySelector('.ask-opt')
      opt?.click()
    }
    const btn = [...document.querySelectorAll('.dock .ask-btn')].find((b) => !b.disabled)
    btn?.click()
  })
  await wait(1500)
}
// ② 审批卡 → 批准（之后 mock 会派两个子 agent）
const ok = await waitFor('.approval, .ap-card, [data-approval]', 30000)
console.log('approval card:', ok)
await page.eval(() => {
  const btn = [...document.querySelectorAll('button')].find((b) => /批准|允许|approve|allow/i.test(b.textContent || ''))
  btn?.click()
})
await wait(2500)

// 等子 agent 卡片/树节点出现
await waitFor('.ar-card, .agent-card, .as-hd, .at-node', 30000)
const before = await page.eval(() => {
  const st = window.__store.getState()
  const v = st.views[st.activeSessionId]
  return {
    session: { ctx: v?.ctxTokens, window: v?.contextWindow, turns: v?.turns?.length },
    lastTurn: v?.turns?.at(-1) ?? null,
    runs: (v?.runNodes ?? []).map((n) => ({ id: n.id, label: n.label, startedAt: n.startedAt, ctxWindow: n.ctxWindow ?? null })),
    agents: (v?.blocks ?? []).filter((b) => b.kind === 'agent').map((b) => ({ runId: b.runId, label: b.label, durMs: b.durMs ?? null, usage: b.usage, spawnedAt: b.spawnedAt })),
  }
})
console.log('BEFORE', JSON.stringify(before, null, 1))

// 聚焦第一个子 agent
const focused = await page.eval(() => {
  const st = window.__store.getState()
  const v = st.views[st.activeSessionId]
  const node = (v?.runNodes ?? []).find((n) => n.parentId)
  const card = (v?.blocks ?? []).find((b) => b.kind === 'agent')
  if (!node && !card) return null
  const target = { nodeId: node?.id ?? card.runId, runId: card?.runId ?? node?.id, ...(node?.taskId ? { taskId: node.taskId } : {}) }
  st.focusAgent(target)
  return target
})
console.log('focus →', JSON.stringify(focused))
await wait(1200)

const after = await page.eval(() => {
  const st = window.__store.getState()
  const v = st.views[st.activeSessionId]
  const txt = (sel) => document.querySelector(sel)?.textContent?.trim() ?? null
  return {
    focusedAgent: st.focusedAgent,
    composerCtx: txt('.ctx, .ctx-ring-wrap, [class*=ctx]'),
    statusbar: txt('.statusbar'),
    agentDur: txt('.as-dur'),
    headerLine: txt('.as-hd'),
    sessionCtx: { ctx: v?.ctxTokens, window: v?.contextWindow },
    turnsOfRun: (v?.turns ?? []).filter((t) => t.runId === st.focusedAgent?.runId).length,
  }
})
console.log('AFTER', JSON.stringify(after, null, 1))
await b.close?.()
process.exit(0)
