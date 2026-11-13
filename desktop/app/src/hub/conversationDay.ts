// 会话时间线的日期口径：日期分隔线用同一个「本地自然日」键。
// 纯函数（不读 store / DOM），Node 直测：见 scripts/test-hub-conv.mjs。
//
// 为什么不用相对时间：消息头/气泡下要的是「几点」，而分隔线回答「哪一天」。
// 两者分开，侧栏预览仍用 conversation.formatRelativeTime（刚刚 / 昨天 / 3 天前）。

export type DayLang = 'zh' | 'en'

function pad(n: number): string {
  return n < 10 ? `0${n}` : String(n)
}

function dayKeyOfDate(d: Date): string {
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}

/** 本地自然日键（YYYY-MM-DD）；非法/缺省时间返回空串（调用方据此跳过分隔线）。 */
export function dayKey(iso?: string | null): string {
  if (!iso) return ''
  const t = Date.parse(iso)
  if (!Number.isFinite(t)) return ''
  return dayKeyOfDate(new Date(t))
}

/** 日期分隔线文案：今天 / 昨天 / 9 月 21 日 / 2025 年 9 月 21 日（英文跟随 lang）。 */
export function dayLabel(iso?: string | null, lang: DayLang = 'zh', now: number = Date.now()): string {
  const key = dayKey(iso)
  if (!key) return ''
  const d = new Date(Date.parse(iso as string))
  const base = new Date(now)
  const today = lang === 'en' ? 'Today' : '今天'
  const yesterday = lang === 'en' ? 'Yesterday' : '昨天'
  if (key === dayKeyOfDate(base)) return today
  const y = new Date(base)
  y.setDate(y.getDate() - 1)
  if (key === dayKeyOfDate(y)) return yesterday
  return new Intl.DateTimeFormat(lang === 'en' ? 'en-US' : 'zh-CN', {
    year: d.getFullYear() === base.getFullYear() ? undefined : 'numeric',
    month: 'short',
    day: 'numeric',
  }).format(d)
}
