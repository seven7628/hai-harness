import test from 'node:test'
import assert from 'node:assert/strict'
import { fmtDuration, fmtRun } from './duration'

/* 2026-10 用户口径：所有计时归一化，**最多两级单位** ——
   1h30m / 1m25s / 1s666ms / 880ms。
   最大单位是分/小时时不再显示毫秒；最大单位是小时时不再显示秒。 */

test('fmtDuration：最多两级单位，按进位出单位', () => {
  assert.equal(fmtDuration(0), '0ms')
  assert.equal(fmtDuration(666), '666ms')
  assert.equal(fmtDuration(880), '880ms', '不足 1 秒只有毫秒')
  assert.equal(fmtDuration(999.6), '1s', '四舍五入到整秒，不出 1000ms')
  assert.equal(fmtDuration(1000), '1s')
  assert.equal(fmtDuration(1300), '1s300ms', '最大单位是秒 → 带毫秒')
  assert.equal(fmtDuration(12_300), '12s300ms')
  assert.equal(fmtDuration(71_000), '1m11s', '71s 进位成 1m11s')
  assert.equal(fmtDuration(71_123), '1m11s', '最大单位是分钟 → 不带毫秒')
  assert.equal(fmtDuration(131_693), '2m11s', '用户截图里的 2m11s693ms → 2m11s')
  assert.equal(fmtDuration(60_300), '1m', '分的下一级秒为 0 且毫秒被丢掉 → 1m')
  assert.equal(fmtDuration(3_600_000), '1h')
  assert.equal(fmtDuration(3_661_000), '1h1m', '最大单位是小时 → 不带秒')
  assert.equal(fmtDuration(5_400_000), '1h30m')
  assert.equal(fmtDuration(5_130_666), '1h25m', '1h25m30s666ms → 1h25m')
  assert.equal(fmtDuration(-1), '—')
  assert.equal(fmtDuration(undefined), '—')
  assert.equal(fmtDuration(NaN), '—')
})

test('fmtRun：运行中的秒表两级单位、不闪小数', () => {
  assert.equal(fmtRun(0), '0s')
  assert.equal(fmtRun(999), '0s')
  assert.equal(fmtRun(1500), '1s')
  assert.equal(fmtRun(71_000), '1m11s')
  assert.equal(fmtRun(3_600_000), '1h', '最大单位是小时 → 不带秒')
  assert.equal(fmtRun(3_661_000), '1h1m')
  assert.equal(fmtRun(3_725_000), '1h2m')
  assert.equal(fmtRun(-1), '—')
  assert.equal(fmtRun(undefined), '—')
})
