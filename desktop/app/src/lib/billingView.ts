// billingView —— 余额/配额的**展示口径**（纯函数，可单测）。
//
// 为什么单独一层：同一个快照要在列表行（168px 车位）、详情块、hover 提示三处出现，
// 三处的判断必须一致；尤其这两条错了用户会读反：
//   · 上游给了窗口但没给读数（percent 缺失）→ **不能**当 0%（0% = 明确「没用过」）；
//   · 余额为负 / 剩余 0 → 必须落到 danger，而不是「正常的 0 元」。
import type { BillingMoney, BillingSnapshot, BillingWindow } from '../transport/types'

export type MeterTone = 'ok' | 'warn' | 'danger' | 'muted'

// 阈值：<70 中性、70–90 警告、>=90 危险。与设计稿一致（docs/PROVIDER_MANAGEMENT_REDESIGN.md §7）。
export function toneOfPercent(p: number): MeterTone {
  if (!Number.isFinite(p)) return 'muted'
  if (p >= 90) return 'danger'
  if (p >= 70) return 'warn'
  return 'ok'
}

// worstWindow 最紧的那个窗口（已用百分比最大）。
// percent 缺失的窗口**不参与**比较（读数缺失 ≠ 0%），全缺失返回 undefined。
export function worstWindow(snap?: BillingSnapshot): BillingWindow | undefined {
  const ws = snap?.windows ?? []
  let best: BillingWindow | undefined
  for (const w of ws) {
    if (typeof w.percent !== 'number' || !Number.isFinite(w.percent)) continue
    if (!best || (w.percent ?? 0) > (best.percent ?? 0)) best = w
  }
  return best
}

// fmtMoney 金额格式化：币种符号 + 两位小数。
// CNY 用 ¥（DeepSeek 实测返回 CNY），USD 用 $，其它币种回退「代码 + 金额」。
export function fmtMoney(m?: BillingMoney): string {
  if (!m || !Number.isFinite(m.amount)) return ''
  const cur = (m.currency || 'USD').toUpperCase()
  const sym = cur === 'USD' ? '$' : cur === 'CNY' ? '¥' : cur + ' '
  const n = Math.abs(m.amount) >= 1000 ? m.amount.toFixed(0) : m.amount.toFixed(2)
  return sym + n
}

export interface MeterView {
  // text 空字符串 = 这一行不画计量（无能力 / 未连接 / 出错都不占视觉）
  text: string
  percent?: number
  tone: MeterTone
  hint: string
  // hasData 供 hover/详情判断（true 才显示刷新按钮的「有值」态）
  hasData: boolean
}

// meterOf 列表行 chip 的唯一口径。
// detail 文案（原因/重置时间）走 hint，不放进行内——行只有 168px。
export function meterOf(snap?: BillingSnapshot): MeterView {
  if (!snap) return { text: '', tone: 'muted', hint: '', hasData: false }
  if (snap.not_connected || snap.error_code === 'billing_no_key') {
    return { text: '', tone: 'muted', hint: 'no-key', hasData: false }
  }
  // 刷新失败但保留着上次成功的读数：照样显示数字（灰 + 角标），不要让好数据变空白。
  const staleWithData = Boolean(snap.stale) && Boolean(snap.balance || snap.used || (snap.windows ?? []).length)
  if (snap.error_code && !staleWithData) {
    return { text: '', tone: 'muted', hint: snap.error_code, hasData: false }
  }
  if (snap.kind === 'quota') {
    const w = worstWindow(snap)
    if (!w || typeof w.percent !== 'number') {
      // 窗口上报了但没有读数：给一个明确的文字，而不是 0%。
      return { text: '—', tone: 'muted', hint: 'no-reading', hasData: false }
    }
    const pct = Math.round(w.percent)
    return {
      text: `${w.label ?? w.key} ${pct}%`,
      percent: w.percent,
      tone: snap.stale ? 'muted' : toneOfPercent(w.percent),
      hint: w.resets_at ?? '',
      hasData: true,
    }
  }
  if (snap.kind === 'balance') {
    const bal = snap.balance
    if (bal) {
      const tone: MeterTone = snap.stale ? 'muted' : bal.amount <= 0 ? 'danger' : 'ok'
      return { text: fmtMoney(bal), tone, hint: '', hasData: true }
    }
    // 只有用量没有余额（new-api 哨兵额度）：显示本期用量。
    if (snap.used) {
      return { text: fmtMoney(snap.used), tone: snap.stale ? 'muted' : 'ok', hint: 'used-only', hasData: true }
    }
  }
  return { text: '', tone: 'muted', hint: '', hasData: false }
}

// localSpendOf 本地记账（E 形状）：从已缓存的 metrics 报告派生某 provider 的今日/本月花费。
// ByDayModel[].Model = "provider/model"（无 provider 时是裸模型名 → 不计入任何 provider）。
// 日界用本地时区的 YYYY-MM-DD —— metrics 的 Day 也是按本地日聚合的。
export function localSpendOf(
  byDayModel: { Day: string; Model: string; CostUsd: number }[] | undefined,
  provider: string,
  now: Date = new Date(),
): { todayUsd: number; monthUsd: number } {
  const pad = (n: number) => String(n).padStart(2, '0')
  const today = `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}`
  const monthStart = `${now.getFullYear()}-${pad(now.getMonth() + 1)}-01`
  let todayUsd = 0
  let monthUsd = 0
  for (const row of byDayModel ?? []) {
    const slash = row.Model.indexOf('/')
    if (slash <= 0) continue
    if (row.Model.slice(0, slash) !== provider) continue
    if (row.Day >= monthStart) monthUsd += row.CostUsd
    if (row.Day === today) todayUsd += row.CostUsd
  }
  return { todayUsd, monthUsd }
}

// sortedWindows 详情块用：已用百分比降序，读数缺失的排最后（不让它们占视觉首位）。
export function sortedWindows(snap?: BillingSnapshot): BillingWindow[] {
  const ws = [...(snap?.windows ?? [])]
  return ws.sort((a, b) => {
    const av = typeof a.percent === 'number' ? a.percent : -1
    const bv = typeof b.percent === 'number' ? b.percent : -1
    return bv - av
  })
}

// resetIn 重置时间的相对说法（详情块用；null = 不显示）。
export function resetIn(resetsAt?: string, now: Date = new Date()): string {
  if (!resetsAt) return ''
  const t = new Date(resetsAt)
  if (Number.isNaN(t.getTime())) return ''
  const ms = t.getTime() - now.getTime()
  if (ms <= 0) return ''
  const min = Math.round(ms / 60000)
  if (min < 60) return `${min}m`
  const h = Math.floor(min / 60)
  if (h < 48) return `${h}h${min % 60 ? ' ' + (min % 60) + 'm' : ''}`
  return `${Math.floor(h / 24)}d`
}
