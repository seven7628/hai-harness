/* 全仓唯一的时长写法（2026-10 用户口径：所有计时归一化）。
 *
 *   1h30m / 1m25s / 1s666ms / 880ms
 *
 * 规则：**最多两级单位** —— 只显示最大的那一级和紧邻的下一级；再小的丢掉。
 * · 最大单位是小时 → 时 + 分（不带秒、不带毫秒）：1h30m / 1h；
 * · 最大单位是分钟 → 分 + 秒（不带毫秒）：1m25s / 2m；
 * · 最大单位是秒   → 秒 + 毫秒：1s666ms / 12s；
 * · 不足 1 秒      → 只要毫秒：880ms / 666ms。
 * 单位之间不留空格；0m / 0s 不写（「只有进位了才会有对应的单位」）。
 *
 * 运行中的秒表（ElapsedTimer、live 面板）用 fmtRun：同样两级、只是没有毫秒
 * （时钟每秒跳一次，小数位只会闪）。
 */


/** 已结算的时长：最多两级单位（见文件头）。null / 负数 → '—'。 */
export function fmtDuration(ms?: number): string {
  if (ms == null || !Number.isFinite(ms) || ms < 0) return '—'
  const total = Math.round(ms)
  const milli = total % 1000
  const totalSec = Math.floor(total / 1000)
  const h = Math.floor(totalSec / 3600)
  const m = Math.floor((totalSec % 3600) / 60)
  const s = totalSec % 60
  // 最大单位是小时：秒/毫秒一律不显示，分只在进位后出现
  if (h) return m ? `${h}h${m}m` : `${h}h`
  // 最大单位是分钟：毫秒不显示，秒只在进位后出现
  if (m) return s ? `${m}m${s}s` : `${m}m`
  // 最大单位是秒：毫秒非零才显示；不足 1 秒只有毫秒
  if (!s) return `${milli}ms`
  return milli ? `${s}s${milli}ms` : `${s}s`
}

/** 运行中的秒表：两级单位、没有毫秒（0s → 59s → 1m0s → 1h5m）。 */
export function fmtRun(ms?: number): string {
  if (ms == null || !Number.isFinite(ms) || ms < 0) return '—'
  const total = Math.floor(ms / 1000)
  const h = Math.floor(total / 3600)
  const m = Math.floor((total % 3600) / 60)
  const s = total % 60
  if (h) return m ? `${h}h${m}m` : `${h}h`
  if (m) return `${m}m${s}s` // 秒表恒带秒，数字位置不跳
  return `${s}s`
}
