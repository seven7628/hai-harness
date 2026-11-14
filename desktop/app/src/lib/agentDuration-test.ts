import { test } from 'node:test'
import assert from 'node:assert/strict'
import { agentDurationOf } from './agentDuration'

test('卡片有结算 → 用它，且不再当运行中（哪怕节点状态还停在 running）', () => {
  assert.deepEqual(agentDurationOf({ status: 'running', cardDurMs: 2980 }), { running: false, durMs: 2980 })
})

test('卡片没结算 → 退到节点结算（runNodes 先到、卡片还没拉回的场景）', () => {
  assert.deepEqual(agentDurationOf({ status: 'running', nodeDurMs: 4370 }), { running: false, durMs: 4370 })
})

test('两者都没有 → 用该 run 最后一条 item 的整轮耗时', () => {
  assert.deepEqual(agentDurationOf({ status: 'completed', lastItemAgentDurMs: 1200 }), { running: false, durMs: 1200 })
})

test('都没有且确实在跑 → running（由 startAt 走实时计时）', () => {
  assert.deepEqual(agentDurationOf({ status: 'running' }), { running: true })
  assert.deepEqual(agentDurationOf({ status: 'interrupting' }), { running: true })
})

test('都没有且已结束 → 不给数（宁可不显示，也不要显示一个错的）', () => {
  assert.deepEqual(agentDurationOf({ status: 'completed' }), { running: false })
  assert.deepEqual(agentDurationOf({ status: 'failed', cardDurMs: 0 }), { running: false })
})

test('优先级：卡片 > 节点 > item', () => {
  assert.deepEqual(agentDurationOf({ cardDurMs: 100, nodeDurMs: 200, lastItemAgentDurMs: 300 }), { running: false, durMs: 100 })
})

test('当前卡片在跑 → 忽略上一轮残留的节点 durMs（agent_resume 后续跑仍显示 Working）', () => {
  assert.deepEqual(agentDurationOf({ status: 'done', cardStatus: 'running', nodeDurMs: 9000 }), { running: true })
  assert.deepEqual(agentDurationOf({ status: 'interrupting', cardStatus: 'interrupting', nodeDurMs: 9000 }), { running: true })
})

test('当前卡片已结算 → 仍按卡片结算（cardStatus 不把已完成误判成运行中）', () => {
  assert.deepEqual(agentDurationOf({ cardStatus: 'completed', cardDurMs: 1200, nodeDurMs: 9000 }), { running: false, durMs: 1200 })
})
