// billingView-test.ts —— 余额/配额展示口径单测（node:test，无 DOM）。
//
// 为什么值得钉：这三条错了，用户会**读反**额度，而不是看到一个明显的报错：
//   · 读数缺失被当成 0% → 以为「这个月一点没用」；
//   · 最紧窗口取错 → 以为还有余量，实际下一小时就会 429；
//   · 剩余百分比没翻转 → 满额显示 100%（方向完全相反，Minimax/Copilot 的语义）。
//
// 运行：esbuild bundle 后 node --test（见 package.json 的 test:billing-view）。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { fmtMoney, localSpendOf, meterOf, resetIn, sortedWindows, toneOfPercent, worstWindow } from './billingView'
import type { BillingSnapshot } from '../transport/types'

const quotaSnap = (windows: BillingSnapshot['windows']): BillingSnapshot => ({
  provider: 'opencode',
  fetched_at: '2026-11-07T10:00:00Z',
  kind: 'quota',
  windows,
})

const balanceSnap = (amount: number, currency = 'CNY'): BillingSnapshot => ({
  provider: 'deepseek',
  fetched_at: '2026-11-07T10:00:00Z',
  kind: 'balance',
  balance: { amount, currency },
})

test('阈值：<70 中性、70–90 警告、>=90 危险', () => {
  assert.equal(toneOfPercent(0), 'ok')
  assert.equal(toneOfPercent(69.9), 'ok')
  assert.equal(toneOfPercent(70), 'warn')
  assert.equal(toneOfPercent(89.9), 'warn')
  assert.equal(toneOfPercent(90), 'danger')
  assert.equal(toneOfPercent(100), 'danger')
  assert.equal(toneOfPercent(Number.NaN), 'muted')
})

test('最紧窗口：取 percent 最大，读数缺失的不参与（缺失 ≠ 0%）', () => {
  const snap = quotaSnap([
    { key: 'rolling', label: '5h', percent: 0 },
    { key: 'weekly', label: 'week', percent: 48 },
    { key: 'monthly', label: 'month', percent: 24 },
    { key: 'unknown' }, // 没有读数
  ])
  assert.equal(worstWindow(snap)?.key, 'weekly')
  // 全部缺失 → undefined（调用方据此显示「读数缺失」，而不是挑一个 0%）
  assert.equal(worstWindow(quotaSnap([{ key: 'x' }])), undefined)
  assert.equal(worstWindow(undefined), undefined)
})

test('列表 chip：窗口形状 → 「week 48%」+ 对应色调', () => {
  const m = meterOf(quotaSnap([
    { key: 'rolling', label: '5h', percent: 5 },
    { key: 'weekly', label: 'week', percent: 48 },
  ]))
  assert.equal(m.text, 'week 48%')
  assert.equal(m.tone, 'ok')
  assert.equal(m.hasData, true)
  // 90% 以上切危险色
  assert.equal(meterOf(quotaSnap([{ key: 'weekly', label: 'week', percent: 91 }])).tone, 'danger')
})

test('列表 chip：读数缺失显示 —（不是 0%）', () => {
  const m = meterOf(quotaSnap([{ key: 'weekly', label: 'week' }]))
  assert.equal(m.text, '—')
  assert.equal(m.hint, 'no-reading')
  assert.equal(m.hasData, false)
})

test('列表 chip：余额 → 带币种符号；余额为负 → 危险色', () => {
  assert.equal(meterOf(balanceSnap(-0.02)).text, '¥-0.02')
  assert.equal(meterOf(balanceSnap(-0.02)).tone, 'danger')
  assert.equal(meterOf(balanceSnap(12.5, 'USD')).text, '$12.50')
  assert.equal(meterOf(balanceSnap(12.5, 'USD')).tone, 'ok')
  // 未知币种退化为「代码 + 金额」
  assert.equal(meterOf(balanceSnap(3, 'EUR')).text, 'EUR 3.00')
})

test('列表 chip：未连接 / 出错 / 无能力都不占位（text 为空）', () => {
  const nokey: BillingSnapshot = { ...balanceSnap(1), balance: undefined, not_connected: true, error_code: 'billing_no_key' }
  assert.equal(meterOf(nokey).text, '')
  assert.equal(meterOf({ ...balanceSnap(1), error_code: 'billing_network' }).text, '')
  assert.equal(meterOf({ provider: 'x', fetched_at: '', kind: 'none' }).text, '')
  assert.equal(meterOf(undefined).text, '')
})

test('只有用量没有余额（new-api 哨兵额度）→ 显示已用金额', () => {
  const snap: BillingSnapshot = {
    provider: 'bailong', fetched_at: '', kind: 'balance',
    used: { amount: 122.39, currency: 'USD' },
  }
  assert.equal(meterOf(snap).text, '$122.39')
  assert.equal(meterOf(snap).hint, 'used-only')
})

test('本地花费：按 provider 归集，跨月只算本月，裸模型名不计入', () => {
  const rows = [
    { Day: '2026-10-31', Model: 'deepseek/deepseek-v4-flash', CostUsd: 9 },   // 上月
    { Day: '2026-11-01', Model: 'deepseek/deepseek-v4-flash', CostUsd: 1.5 },
    { Day: '2026-11-06', Model: 'deepseek/deepseek-chat', CostUsd: 2.5 },
    { Day: '2026-11-06', Model: 'opencode/kimi-k3', CostUsd: 4 },              // 别的 provider
    { Day: '2026-11-06', Model: 'no-provider-model', CostUsd: 7 },             // 无 provider 前缀
  ]
  const now = new Date(2026, 10, 6) // 2026-11-06（本地时区）
  const s = localSpendOf(rows, 'deepseek', now)
  assert.equal(s.todayUsd, 2.5)
  assert.equal(s.monthUsd, 4.0)
  assert.equal(localSpendOf(rows, 'nobody', now).monthUsd, 0)
  assert.deepEqual(localSpendOf(undefined, 'deepseek', now), { todayUsd: 0, monthUsd: 0 })
})

test('详情窗口排序：已用高的在前，读数缺失的垫底', () => {
  const ws = sortedWindows(quotaSnap([
    { key: 'a' }, { key: 'b', percent: 12 }, { key: 'c', percent: 88 },
  ]))
  assert.deepEqual(ws.map((w) => w.key), ['c', 'b', 'a'])
})

test('重置时间相对说法', () => {
  const now = new Date('2026-11-07T10:00:00Z')
  assert.equal(resetIn('2026-11-07T10:30:00Z', now), '30m')
  assert.equal(resetIn('2026-11-07T13:00:00Z', now), '3h')
  assert.equal(resetIn('2026-11-09T11:00:00Z', now), '2d')
  assert.equal(resetIn('2026-11-07T09:00:00Z', now), '') // 过去的不显示
  assert.equal(resetIn(undefined, now), '')
  assert.equal(resetIn('not-a-date', now), '')
})

test('金额格式化：千位以上取整，未知币种带代码', () => {
  assert.equal(fmtMoney({ amount: 1234.56, currency: 'USD' }), '$1235')
  assert.equal(fmtMoney(undefined), '')
})

test('刷新失败但保留旧读数（stale）：照常显示数字，色调降为 muted', () => {
  const stale: BillingSnapshot = {
    ...balanceSnap(-0.02), error_code: 'billing_network', stale: true,
  }
  const m = meterOf(stale)
  assert.equal(m.text, '¥-0.02') // 旧值不能被抹成空白
  assert.equal(m.tone, 'muted')
  assert.equal(m.hasData, true)

  const staleQuota: BillingSnapshot = {
    ...quotaSnap([{ key: 'weekly', label: 'week', percent: 95 }]), error_code: 'billing_network', stale: true,
  }
  assert.equal(meterOf(staleQuota).text, 'week 95%')
  assert.equal(meterOf(staleQuota).tone, 'muted') // 不因为 95% 报 danger —— 那是过期读数
})
