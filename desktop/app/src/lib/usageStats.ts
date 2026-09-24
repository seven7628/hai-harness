// 使用统计（设置弹窗「数据与统计 → 使用统计」+ 空会话近 30 天图表）纯函数层。
//
// 抽出来的理由：这些口径（热力图分桶 / 连续日补齐 / 折线坐标 / 大数与时长格式）是页面里最
// 容易回归的部分，放纯函数里可用 node 直接断言（scripts/test-usage-stats.mjs），不必开浏览器。
//
// 数据源：bridge `metrics` 命令报告（一次拉取覆盖近一年；7/30 天与热力图三视图都在前端切
// 同一份报告 —— 全量 JSONL 扫描是数百毫秒级，不能随切换重跑）。
import type { MetricsDayGroup, MetricsDayModelGroup } from '../store/useAppStore'

export type HeatMode = 'day' | 'week' | 'cum'

// 热力图格：level 0 = 无活动（空白格）；1..4 按分位数着色（线性映射会被单日尖峰压成一片浅色）。
// day/cum 与视图无关（始终带当日用量与截至当日累计）—— hover 浮层要同时给这两个数，
// 不随「每日/每周/累计」切换而变。（三视图的当前值看 value。）
export interface HeatCell {
  key: string
  value: number // 当前视图的值（每日 = 当日；每周 = 该周合计；累计 = 截至当日累计）
  day: number // 当日 token（tooltip）
  cum: number // 截至当日累计 token（tooltip）
  level: number
  future: boolean // 今天之后的占位格（保持网格形状，不参与着色/最大值）
  before: boolean // 窗口之前的前导格（近 30 天视图用；同样只是占位）
}

export interface HeatColumn {
  cells: HeatCell[]
  month?: string // 该列首日的月份标签（YYYY-MM；仅换月列带，其余为空）
}

export interface Heatmap {
  columns: HeatColumn[]
  months: { label: string; col: number }[] // 月份刻度（label = "9月" / "Sep"）
  max: number
  total: number // 窗口内合计（近 30 天视图的「合计」用它，不含窗口外的前导格）
  activeCells: number // 窗口内参与统计的格子数
}

export interface TrendSeries {
  model: string // 全名（provider/model/...；title/aria 用）
  label: string // 图例短名（modelLabel；同名冲突时回退全名）
  color: string // CSS 变量名（--accent / --indigo ...）
  points: number[]
  total: number
}

export interface Trend {
  keys: string[] // 连续自然日（含无数据的 0 值日，折线不留断口）
  daysTotal: number[]
  series: TrendSeries[]
  max: number
  peak: { key: string; value: number }
}

// —— 自然日工具（本地时区，与 bridge 的 Local() 分组口径一致）——
export function dayKeyOf(d: Date): string {
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

export function parseDayKey(key: string): Date {
  const [y, m, d] = key.split('-').map((v) => Number(v))
  return new Date(y, (m ?? 1) - 1, d ?? 1)
}

export function addDays(d: Date, n: number): Date {
  const x = new Date(d)
  x.setDate(x.getDate() + n)
  x.setHours(0, 0, 0, 0)
  return x
}

// 折线配色（水墨：绛红 → 黛青 → 竹青 → 赭石 → 绛紫，最后一档留给「其他」）
const SERIES_COLORS = ['--accent', '--indigo', '--bamboo', '--ochre', '--crimson']
const OTHERS_COLOR = '--text-3'
const MAX_SERIES = 5 // 超过 5 个模型合并为「其他」，图例不爆行

// heatLevel 按分位数分档：单日尖峰（历史峰值日常比中位数高两个数量级）在线性映射下会把
// 其余格子全压成最浅档，热力图看起来「只有一天有数据」。
export function heatLevel(value: number, thresholds: number[]): number {
  if (value <= 0) return 0
  let level = 1
  for (const th of thresholds) if (value > th) level++
  return Math.min(4, level)
}

// 分位阈值（25% / 50% / 75%），输入为非零值（已排序或未排序均可）。
export function heatThresholds(nonZero: number[]): number[] {
  if (nonZero.length === 0) return [0, 0, 0]
  const sorted = [...nonZero].sort((a, b) => a - b)
  const at = (q: number) => sorted[Math.min(sorted.length - 1, Math.floor(q * sorted.length))]
  return [at(0.25), at(0.5), at(0.75)]
}

const MONTH_EN = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']
const WD_EN = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat']
const WD_ZH = ['周日', '周一', '周二', '周三', '周四', '周五', '周六']

// heatWeeksFor 覆盖「end 往前 days 天」所需的周列数（近 30 天 → 5 或 6 列）。
// 固定 6 列会在某些星期几多出一整列占位格（今天恰好是周日时），按需算列数更干净。
export function heatWeeksFor(days: number, end: Date = new Date()): number {
  const start = addDays(end, -(days - 1))
  const diff = weekStart(addDays(end, 0)).getTime() - weekStart(start).getTime()
  return Math.round(diff / (7 * 86_400_000)) + 1
}

// buildHeatmap 生成近 weeks 周（默认 53 = 一年）的活动网格。
// 列 = 周（周日起），行 = 周一..周日；mode 决定取值：
//   day  = 当日 token；week = 该周合计（同列同值）；cum = 从最早记录累计到当日（CumTokens）。
// end = 网格终点（默认今天）；晚于 end 的格子标 future（画成占位，不参与配色）。
// windowDays > 0 = 只把最近 N 天当「窗口」：更早的格子标 before（占位不着色）——
// 空会话中部的「近 30 天」视图要 5 周 × 7 天 = 35 格的网格形状，但只让 30 天参与统计。
export function buildHeatmap(
  days: MetricsDayGroup[],
  mode: HeatMode,
  weeks = 53,
  end: Date = new Date(),
  lang: 'zh' | 'en' = 'zh',
  windowDays = 0,
): Heatmap {
  const endDay = addDays(end, 0)
  const byKey = new Map<string, MetricsDayGroup>()
  for (const d of days) byKey.set(d.Key, d)
  const valueOf = (key: string): number => {
    const row = byKey.get(key)
    if (!row) return 0
    if (mode === 'cum') return row.CumTokens
    return (row.Input ?? 0) + (row.Output ?? 0)
  }
  // 周合计：先按周起始日聚一次（同列的 7 格同值，读图时看整体节奏）
  const weekSum = new Map<string, number>()
  if (mode === 'week') {
    for (const d of days) {
      const start = weekStart(parseDayKey(d.Key))
      weekSum.set(dayKeyOf(start), (weekSum.get(dayKeyOf(start)) ?? 0) + (d.Input ?? 0) + (d.Output ?? 0))
    }
  }

  const gridStart = addDays(weekStart(endDay), -(weeks - 1) * 7)
  const windowStart = windowDays > 0 ? addDays(endDay, -(windowDays - 1)) : null
  const values: { key: string; value: number; day: number; cum: number; future: boolean; before: boolean }[] = []
  for (let c = 0; c < weeks; c++) {
    for (let r = 0; r < 7; r++) {
      const date = addDays(gridStart, c * 7 + r)
      const key = dayKeyOf(date)
      const future = date > endDay
      const before = windowStart != null && date < windowStart
      const value = mode === 'week' ? weekSum.get(dayKeyOf(weekStart(date))) ?? 0 : valueOf(key)
      const row = byKey.get(key)
      values.push({
        key,
        value: future || before ? 0 : value,
        day: row ? (row.Input ?? 0) + (row.Output ?? 0) : 0,
        cum: row?.CumTokens ?? 0,
        future,
        before,
      })
    }
  }
  const inGrid = values.filter((v) => !v.future && !v.before)
  const thresholds = heatThresholds(inGrid.filter((v) => v.value > 0).map((v) => v.value))
  const columns: HeatColumn[] = []
  const months: { label: string; col: number }[] = []
  for (let c = 0; c < weeks; c++) {
    const cells = values.slice(c * 7, c * 7 + 7).map((v) => ({
      key: v.key,
      value: v.value,
      day: v.day,
      cum: v.cum,
      level: v.future || v.before ? 0 : heatLevel(v.value, thresholds),
      future: v.future,
      before: v.before,
    }))
    const col: HeatColumn = { cells }
    // 换月列打标签：该列首日的月份 ≠ 前一列首日月份 → 新月份从这列开始（GitHub 同款做法）
    const firstOfCol = addDays(gridStart, c * 7)
    const prevOfCol = addDays(gridStart, (c - 1) * 7)
    if (c === 0 || firstOfCol.getMonth() !== prevOfCol.getMonth()) {
      col.month = `${firstOfCol.getFullYear()}-${String(firstOfCol.getMonth() + 1).padStart(2, '0')}`
      const label = lang === 'en' ? MONTH_EN[firstOfCol.getMonth()] : `${firstOfCol.getMonth() + 1}月`
      // 刻度太挤（相邻两列）会让标签叠字：间距 < 3 列时跳过，保留更早的那个
      if (c === 0 || c - months[months.length - 1].col >= 3) months.push({ label, col: c })
    }
    columns.push(col)
  }
  return {
    columns,
    months,
    activeCells: inGrid.length,
    max: inGrid.reduce((a, v) => Math.max(a, v.value), 0),
    total: inGrid.reduce((a, v) => a + v.value, 0),
  }
}

// heatGridRows 把「列 = 周」的网格重排成「每行 N 天」的横向网格（空会话中部图表用）。
// 90 天按列 = 周铺开是 13~14 列 × 7 行（近正方形，16:9 屏上突兀）；按天从左到右铺开后
// 是每行 18 天 × 5 行的横条（宽高比 ≈ 3.6:1），读法与标题/范围文案一致：时间从左到右。
// 只取窗口内的格子（before/future 是网格占位）；天数不足整行时用 null 补位，保持矩形。
export function heatGridRows(heat: Heatmap, rows: number): (HeatCell | null)[][] {
  const cells = heat.columns.flatMap((c) => c.cells).filter((c) => !c.before && !c.future)
  const perRow = Math.max(1, Math.ceil(cells.length / Math.max(1, Math.floor(rows))))
  const out: (HeatCell | null)[][] = []
  for (let i = 0; i < cells.length; i += perRow) {
    const row: (HeatCell | null)[] = cells.slice(i, i + perRow)
    while (row.length < perRow) row.push(null)
    out.push(row)
  }
  return out
}

// weekStart 该日期所在周的周日（GitHub 热力图口径：列 = 周，行 = 周日..周六）。
export function weekStart(d: Date): Date {
  const x = addDays(d, 0)
  x.setDate(x.getDate() - x.getDay())
  return x
}

// buildTrend 折线图数据：连续 range 天（默认终点 = 今天），按模型拆多条线。
// - 无数据的日子补 0（保持 x 轴等距，折线不留断口）；
// - 只保留窗口内用量前 MAX_SERIES 的模型，其余合并为「其他」（图例可读性优先）；
// - 已排序：按窗口内总量降序，配色按序取 SERIES_COLORS。
export function buildTrend(
  days: MetricsDayGroup[],
  dayModels: MetricsDayModelGroup[],
  range: number,
  end: Date = new Date(),
  othersLabel = '其他',
): Trend {
  const endDay = addDays(end, 0)
  const startDay = addDays(endDay, -(range - 1))
  const keys: string[] = []
  for (let i = 0; i < range; i++) keys.push(dayKeyOf(addDays(startDay, i)))
  const idx = new Map(keys.map((k, i) => [k, i]))
  const daysTotal = new Array(range).fill(0)
  for (const d of days) {
    const i = idx.get(d.Key)
    if (i == null) continue
    daysTotal[i] += (d.Input ?? 0) + (d.Output ?? 0)
  }

  const perModel = new Map<string, number[]>()
  for (const row of dayModels) {
    const i = idx.get(row.Day)
    if (i == null) continue
    const arr = perModel.get(row.Model) ?? new Array(range).fill(0)
    arr[i] += (row.Input ?? 0) + (row.Output ?? 0)
    perModel.set(row.Model, arr)
  }

  const ranked = [...perModel.entries()]
    .map(([model, points]) => ({ model, points, total: points.reduce((a, b) => a + b, 0) }))
    .sort((a, b) => b.total - a.total)
  const head = ranked.slice(0, MAX_SERIES)
  const tail = ranked.slice(MAX_SERIES)
  const labels = trendLabels(head.map((s) => s.model))
  const series: TrendSeries[] = head.map((s, i) => ({
    ...s,
    label: labels.get(s.model) ?? s.model,
    color: SERIES_COLORS[i % SERIES_COLORS.length],
  }))
  if (tail.length) {
    const points = new Array(range).fill(0)
    for (const s of tail) for (let i = 0; i < range; i++) points[i] += s.points[i]
    series.push({
      model: othersLabel,
      label: othersLabel,
      color: OTHERS_COLOR,
      points,
      total: tail.reduce((a, s) => a + s.total, 0),
    })
  }

  let max = 0
  let peak = { key: keys[0] ?? '', value: 0 }
  for (const s of series) for (let i = 0; i < range; i++) max = Math.max(max, s.points[i])
  for (let i = 0; i < range; i++) if (daysTotal[i] > peak.value) peak = { key: keys[i], value: daysTotal[i] }
  return { keys, daysTotal, series, max: Math.max(max, 1), peak }
}

// trendLabels 图例短名去重：末两段相同（不同 provider 下的同名模型，如 a/deepseek/m 与
// b/deepseek/m）→ 这两条回退全名，图例里不会出现两个一模一样的标签；其余保持短名。
export function trendLabels(models: string[]): Map<string, string> {
  const short = models.map(modelLabel)
  const seen = new Map<string, number>()
  for (const l of short) seen.set(l, (seen.get(l) ?? 0) + 1)
  const out = new Map<string, string>()
  models.forEach((m, i) => out.set(m, seen.get(short[i])! > 1 ? m : short[i]))
  return out
}

// smoothPath SVG 折线路径（Catmull-Rom → 三次贝塞尔）：设计稿是平滑曲线，不用折线。
// 控制点 y 夹在 [lo, hi] 内，避免尖峰处过冲出画面。
export function smoothPath(pts: { x: number; y: number }[], lo = -Infinity, hi = Infinity): string {
  if (pts.length === 0) return ''
  const f = (n: number) => (Number.isFinite(n) ? n.toFixed(2) : '0')
  const clamp = (y: number) => Math.min(hi, Math.max(lo, y))
  if (pts.length < 3) return pts.map((p, i) => `${i ? 'L' : 'M'}${f(p.x)} ${f(p.y)}`).join(' ')
  let d = `M${f(pts[0].x)} ${f(pts[0].y)}`
  for (let i = 0; i < pts.length - 1; i++) {
    const p0 = pts[i - 1] ?? pts[i]
    const p1 = pts[i]
    const p2 = pts[i + 1]
    const p3 = pts[i + 2] ?? p2
    const c1x = p1.x + (p2.x - p0.x) / 6
    const c1y = clamp(p1.y + (p2.y - p0.y) / 6)
    const c2x = p2.x - (p3.x - p1.x) / 6
    const c2y = clamp(p2.y - (p3.y - p1.y) / 6)
    d += ` C${f(c1x)} ${f(c1y)} ${f(c2x)} ${f(c2y)} ${f(p2.x)} ${f(p2.y)}`
  }
  return d
}

// fmtBigTokens 大数可读化：zh 用万/亿（53.6 万 / 176 亿），en 用 K/M/B。
// 统一 3 位有效数字（与设计稿「53.6 万」同口径）；不用 ui.tsx 的 fmtTokens（只到 M，
// 1.7e10 会显示成 17640.2M）。
export function fmtBigTokens(n: number, lang: 'zh' | 'en' = 'zh'): string {
  const v = Math.max(0, n)
  const sig = (x: number) => String(Number(x.toPrecision(3)))
  if (lang === 'en') {
    if (v >= 1e9) return `${sig(v / 1e9)}B`
    if (v >= 1e6) return `${sig(v / 1e6)}M`
    if (v >= 1e3) return `${sig(v / 1e3)}K`
    return String(Math.round(v))
  }
  if (v >= 1e8) return `${sig(v / 1e8)} 亿`
  if (v >= 1e4) return `${sig(v / 1e4)} 万`
  return String(Math.round(v))
}

// fmtChatDuration 最长聊天时长（会话首尾跨度）：分钟起步（设计稿「0 分钟」），逐级到天。
export function fmtChatDuration(ms: number, lang: 'zh' | 'en' = 'zh'): string {
  const min = Math.max(0, ms) / 60_000
  const m = Math.round(min)
  const h = Math.floor(m / 60)
  const d = Math.floor(h / 24)
  if (lang === 'en') {
    if (h < 1) return `${m} min`
    if (d < 1) return h > 0 && m % 60 ? `${h}h ${m % 60}m` : `${h}h`
    return h % 24 ? `${d}d ${h % 24}h` : `${d}d`
  }
  if (h < 1) return `${m} 分钟`
  if (d < 1) return m % 60 ? `${h} 小时 ${m % 60} 分` : `${h} 小时`
  return h % 24 ? `${d} 天 ${h % 24} 小时` : `${d} 天`
}

// modelLabel 图例/浮层里的短模型名：真数据的模型 id 可能自带层级
// （如 command-code/deepseek/deepseek-v4-flash）—— 五条图例要一行读得完，只留末两段；
// 全名仍挂在 title 上（Legend 组件负责）。
export function modelLabel(name: string): string {
  const parts = name.split('/')
  return parts.length > 2 ? parts.slice(-2).join('/') : name
}

// fmtDayLong 热力图浮层里的日期（带星期）：zh「9月19日 周六」，en「Sat, Sep 19」。
export function fmtDayLong(key: string, lang: 'zh' | 'en' = 'zh'): string {
  const d = parseDayKey(key)
  if (lang === 'en') return `${WD_EN[d.getDay()]}, ${MONTH_EN[d.getMonth()]} ${d.getDate()}`
  return `${d.getMonth() + 1}月${d.getDate()}日 ${WD_ZH[d.getDay()]}`
}

// fmtDayLabel x 轴刻度：zh「9月19日」，en「9/19」。
export function fmtDayLabel(key: string, lang: 'zh' | 'en' = 'zh'): string {
  const d = parseDayKey(key)
  if (lang === 'en') return `${d.getMonth() + 1}/${d.getDate()}`
  return `${d.getMonth() + 1}月${d.getDate()}日`
}
