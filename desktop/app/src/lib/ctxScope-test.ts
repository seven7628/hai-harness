import { test } from 'node:test'
import assert from 'node:assert/strict'
import { ctxEstimatedOf, ctxTokensOf, ctxWindowOf, runCtxOf } from './ctxScope'

const base = {
  ctxTokens: 12000,
  contextWindow: 1_000_000,
  turns: [
    { runId: 'main-1', input: 12000, durMs: 4000 },
    { runId: 'sub-a', input: 3000, durMs: 900 },
    { runId: 'sub-a', input: 4500, durMs: 1200 },
  ],
  runNodes: [{ id: 'main-1' }, { id: 'sub-a', ctxWindow: 200000 }],
  blocks: [{ kind: 'agent', runId: 'sub-a', ctxWindow: 200000, usage: { input: 4500 } }],
}

test('未聚焦：读会话级（分子/分母/口径都照旧）', () => {
  const s = { ...base, ctxTokensEstimated: true }
  assert.equal(ctxTokensOf(s), 12000)
  assert.equal(ctxWindowOf(s), 1_000_000)
  assert.equal(ctxEstimatedOf(s), true)
  assert.equal(runCtxOf(s), undefined)
})

test('聚焦子 agent：分子取该 run 最近一轮、分母取它自己的窗口', () => {
  const s = { ...base, focusedAgent: { runId: 'sub-a' }, ctxTokensEstimated: true }
  assert.equal(ctxTokensOf(s), 4500)
  assert.equal(ctxWindowOf(s), 200000)
  assert.equal(ctxEstimatedOf(s), false, '子 run 的 token 来自真实 usage，不该带 ≈')
  assert.equal(runCtxOf(s)?.turnDurMs, 1200)
})

test('该 run 没上报窗口 → 退回会话级，不报 0 分母', () => {
  const s = { ...base, focusedAgent: { runId: 'main-1' } }
  assert.equal(ctxWindowOf(s), 1_000_000)
  assert.equal(ctxTokensOf(s), 12000)
})

test('子 agent 刚起、还没有自己的轮 → 用卡片已累计的输入，仍不看会话的数', () => {
  const s = {
    ...base,
    turns: [{ runId: 'main-1', input: 12000 }],
    focusedAgent: { runId: 'sub-a' },
  }
  assert.equal(ctxTokensOf(s), 4500)
})

test('块上的窗口也能用（runNodes 被裁剪后块是更稳的来源）', () => {
  const s = {
    ...base,
    runNodes: [{ id: 'main-1' }],
    focusedAgent: { runId: 'sub-a' },
  }
  assert.equal(ctxWindowOf(s), 200000)
})
