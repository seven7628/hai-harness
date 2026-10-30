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

test('已结算但块上残留 running 态（中断收尾）→ 以块为准，仍算在跑，避免半截结论', () => {
  assert.equal(turnLive({ isLast: true, turnRunning: true, runEnded: true }), true)
})
