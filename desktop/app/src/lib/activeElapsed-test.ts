import { test } from 'node:test'
import assert from 'node:assert/strict'
import { activeElapsed, settlePause } from './activeElapsed'

test('没有等待：就是墙钟', () => {
  assert.equal(activeElapsed({ now: 10_000, startedAt: 4_000 }), 6_000)
})

test('等待中时钟停住：等待那一段不算活跃执行时间', () => {
  const startedAt = 0
  const waitingSince = 5000
  assert.equal(activeElapsed({ now: 5000, startedAt, waitingSince }), 5000)
  // 等了 30 秒还没回答：活跃时长仍然是 5 秒（不是 35 秒）
  assert.equal(activeElapsed({ now: 35_000, startedAt, waitingSince }), 5000)
})

test('等待结束后把这段并入 pausedMs，钟从停住处接着走', () => {
  const paused = settlePause(0, 5000, 35_000)
  assert.equal(paused, 30_000)
  assert.equal(activeElapsed({ now: 36_000, startedAt: 0, pausedMs: paused }), 6_000)
})

test('同一次运行等多次：累计叠加', () => {
  let paused = settlePause(0, 1000, 3000)
  paused = settlePause(paused, 10_000, 12_000)
  assert.equal(paused, 4_000)
  assert.equal(activeElapsed({ now: 20_000, startedAt: 0, pausedMs: paused }), 16_000)
})

test('没有 start / 钟还没开始 → 0，不返回负数', () => {
  assert.equal(activeElapsed({ now: 1000 }), 0)
  assert.equal(activeElapsed({ now: 1000, startedAt: 5000 }), 0)
  assert.equal(activeElapsed({ now: 1000, startedAt: 5000, pausedMs: 10 }), 0)
})

test('settlePause 在没有等待时是空操作', () => {
  assert.equal(settlePause(1200, undefined, 9999), 1200)
})
