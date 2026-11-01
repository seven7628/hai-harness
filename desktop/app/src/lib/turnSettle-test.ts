// turnSettle-test.ts —— 「这一轮是否还在跑」的判定（结算时机唯一口径，node:test 无 DOM）。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { turnLive } from './turnSettle'

test('运行中的空档（llm_end 之后、下一次 llm_start 之前）仍算在跑 —— 这是提前折叠的根因', () => {
  // 上一版：turn.running=false 且 run 节点缺失 → 判成已结算（用户报障的形态）
  assert.equal(turnLive({ isLast: true, turnRunning: false, runEnded: false }), true)
})

test('AgentEnd 之后才结算', () => {
  assert.equal(turnLive({ isLast: true, turnRunning: false, runEnded: true }), false)
})

test('历史轮（不是最后一轮）一律已结算', () => {
  assert.equal(turnLive({ isLast: false, turnRunning: false, runEnded: false }), false)
})

test('历史轮里仍有工具在跑（运行中插了新的用户消息）→ 仍在跑', () => {
  assert.equal(turnLive({ isLast: false, turnRunning: true, runEnded: false }), true)
})

test('已交卷 + 后台任务仍在跑（块上残留 running）→ 折叠，不得拉回 Working', () => {
  // 用户 2026-10 报障的形态：主 run 8 秒结束、agent_spawn 的后台子 agent 还在跑，
  // 旧判据（turnRunning 优先）让 Working 与时钟一直挂着，Worked for / 轮尾账永不出现。
  // AgentEnd 是这次运行的交卷时刻；后台任务的完成走自己的记录行（task_delivery），
  // 不是把整轮重新打开。
  assert.equal(turnLive({ isLast: true, turnRunning: true, runEnded: true }), false)
  assert.equal(turnLive({ isLast: false, turnRunning: true, runEnded: true }), false)
})

test('新一次 run 开始后，上一轮残留的 running 不得把上一轮重新点亮（sameRun=false）', () => {
  // 用户 2026-10 报障：上一轮 AgentEnd 已交卷，用户紧接着发下一条 → 视图级 runEnded 被
  // 新的 agent_start 翻回 false，而上一轮里还挂着后台子 agent / promoted 工具的
  // status='running'。只看 turnRunning 就会让上一轮的 Working 与时钟重新开始。
  assert.equal(turnLive({ isLast: false, turnRunning: true, runEnded: false, sameRun: false }), false)
  // 正在跑的那一轮不受影响
  assert.equal(turnLive({ isLast: true, turnRunning: true, runEnded: false, sameRun: true }), true)
  // 没有 runId 的旧数据（sameRun 省略）沿用老行为
  assert.equal(turnLive({ isLast: false, turnRunning: true, runEnded: false }), true)
})
