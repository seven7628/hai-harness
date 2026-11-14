// 子会话页的「一次运行 = 一个 Worked 段」页面断言（真浏览器 + 真组件）。
//
// agent_resume 复用 task_id，但每次运行是新 run、新卡；子会话页把同 task 的多张卡
// 合成一段转录后，必须按 run 切出两个 Worked 盒 —— 不能把两轮并成一个 Working，
// 也不能把任务结果再做成页脚卡（结果已经就是最后一条 assistant 正文）。
//
// 用法：先起 vite dev（LAB_URL 指向它，默认 http://localhost:5288/），再
//   LAB_URL=http://localhost:5289/ node lab/verify-subagent-runs.mjs
import { Browser } from '/Users/admin/.agents/skills/design-taste-frontend/scripts/lib/cdp.mjs'

const URL = process.env.LAB_URL || 'http://localhost:5288/'
const wait = (ms) => new Promise((r) => setTimeout(r, ms))
const check = (name, ok, detail) => ({ name, ok, detail })
const results = []

const b = await Browser.launch({ width: 1440, height: 1000 })
const page = await b.newPage({ width: 1440, height: 1000 })
const errs = []
b.on(page.sessionId, 'Runtime.exceptionThrown', (p) => errs.push(p.exceptionDetails?.text))
await page.goto(URL, { settleMs: 3500 })
if (!(await page.eval(() => !!window.__store && !!window.__dispatch))) throw new Error('store/dispatch never appeared')

// 直接注入两张同 task 的 agent 卡（run-1 / run-2）和对应 run 节点：走真实的
// AgentSessionView → agentItemsToBlocks → Narrative → selectTurns/turnRuns 链路。
const injected = await page.eval(() => {
  const st = window.__store.getState()
  const empty = {
    blocks: [], blocksTotal: 0, blocksCursor: 0, blocksHasMore: false, blocksOlderLoading: false, blocksOlderError: false,
    agentDetached: [], turnsTotal: 0, enteredAt: 0, approvals: [], runNodes: [], diffs: [], compressing: false,
    compressStartTs: undefined, compressBefore: undefined, compressRunId: undefined, compressReason: undefined,
    todos: [], focusedAgent: undefined,
    usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0 },
    ctxTokens: undefined, ctxTokensEstimated: undefined, contextWindow: undefined,
    git: { is_repo: false, ahead: 0, behind: 0, staged: 0, modified: 0, untracked: 0, conflicted: 0, clean: true },
    gitFiles: [], gitHistory: [], gitHasMore: false, gitHistLoading: false, gitLoading: false, gitError: undefined,
    turns: [], costUsd: 0, toolStartTs: {}, streaming: false, runEnded: true, aborted: false, reqBlock: {},
    lastLLMBlockId: undefined, runningAgentCount: 0, thinkStartTs: {}, turnThinkMs: {}, questions: [],
    release: undefined, pendingQueue: [], rightExpanded: false, traceEvents: [],
  }
  const u = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0 }
  const cards = [
    {
      kind: 'agent', id: 11, runId: 'run-1', parentRunId: 'main-1', label: 'sub', status: 'completed',
      spawnedAt: 1000, durMs: 1000, taskId: 'task-x', usage: u, deliveredToMain: true, taskResult: 'run1 answer',
      items: [
        { kind: 'tool', toolId: 't1', name: 'bash', status: 'done', runId: 'run-1', startedAt: 1050, durMs: 100, args: '{"command":"ls"}' },
        { kind: 'text', text: 'run1 answer', runId: 'run-1', ts: 1100, model: 'm', durMs: 500, agentDurMs: 1000 },
      ],
    },
    {
      kind: 'agent', id: 12, runId: 'run-2', parentRunId: 'main-1', label: 'sub', status: 'completed',
      spawnedAt: 2000, durMs: 2000, taskId: 'task-x', usage: u, deliveredToMain: true, taskResult: 'run2 answer',
      items: [
        { kind: 'user', text: 'resume prompt', runId: 'run-2', startedAt: 2100 },
        { kind: 'tool', toolId: 't2', name: 'bash', status: 'done', runId: 'run-2', startedAt: 2150, durMs: 100, args: '{"command":"pwd"}' },
        { kind: 'text', text: 'run2 answer', runId: 'run-2', ts: 2200, model: 'm', durMs: 500, agentDurMs: 2000 },
      ],
    },
  ]
  const runNodes = [
    { id: 'main-1', label: 'main', status: 'done', kind: 'run', startedAt: 500 },
    { id: 'run-1', parentId: 'main-1', label: 'sub', status: 'done', kind: 'run', taskId: 'task-x', startedAt: 1000, durMs: 1000 },
    { id: 'run-2', parentId: 'main-1', label: 'sub', status: 'done', kind: 'run', taskId: 'task-x', startedAt: 2000, durMs: 2000 },
  ]
  const v = { ...empty, blocks: cards, runNodes, turns: [] }
  window.__store.setState({
    activeSessionId: 's1',
    views: { s1: v },
    blocks: cards, runNodes, turns: [], agentDetached: [], focusedAgent: undefined,
    settings: { ...st.settings, narrative_skin: 'runtime' },
  })
  st.focusAgent({ nodeId: 'task-x', runId: 'run-2', taskId: 'task-x' })
  return window.__store.getState().focusedAgent
})
if (!injected) throw new Error('focusAgent 注入失败')
await wait(1200)

const dom = await page.eval(() => {
  const txt = (el) => (el?.textContent || '').replace(/\s+/g, ' ').trim()
  return {
    header: document.querySelector('.as-hd') ? txt(document.querySelector('.as-hd')) : null,
    acts: [...document.querySelectorAll('.rt-act')].map(txt),
    body: document.body.innerText,
    resultInline: !!document.querySelector('.task-result-inline'),
  }
})
results.push(check('子会话页有身份头', Boolean(dom.header), dom.header))
results.push(check('两次运行 = 两个 Worked 段', dom.acts.filter((t) => /^Worked/.test(t)).length === 2, JSON.stringify(dom.acts)))
results.push(check('每段的墙钟来自自己的卡片', dom.acts.some((t) => /^Worked for 1s/.test(t)) && dom.acts.some((t) => /^Worked for 2s/.test(t)), JSON.stringify(dom.acts)))
results.push(check('第一轮正文在', dom.body.includes('run1 answer'), ''))
results.push(check('第二轮正文在', dom.body.includes('run2 answer'), ''))
results.push(check('resume prompt 在转录里', dom.body.includes('resume prompt'), ''))
results.push(check('不再重复渲染任务结果页脚', !dom.resultInline && !/已交付主任务/.test(dom.body), ''))
results.push(check('无页面异常', errs.length === 0, errs.slice(0, 2).join(' | ')))

for (const r of results) console.log(`${r.ok ? 'PASS' : 'FAIL'} ${r.name}${r.detail ? ' — ' + r.detail : ''}`)
await b.close?.()
const failed = results.filter((r) => !r.ok).length
console.log(failed ? `SUBAGENT_RUNS_FAIL (${failed})` : 'SUBAGENT_RUNS_PASS')
process.exit(failed ? 1 : 0)
