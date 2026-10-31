// 使用统计纯函数层回归（esbuild 打包 src/lib/usageStats.ts，Node 直接跑，无需浏览器）。
//
// 覆盖口径（对应用户 2026-09-19 拍板的统计定义）：
//   T1 热力图：网格形状（53 周 × 7 天）、未来格占位、周视图同列同值、累计视图取 CumTokens、
//      月份刻度不重叠、分位数分档（单日尖峰不把其余格子压成浅色）、近 90 天窗口的
//      flat 排布（5 行 × 18 天，时间从左到右 —— 空会话中部图表）
//   T2 趋势：连续 30 天补 0、按模型拆分、窗口外模型不串入、>5 模型合并「其他」、峰值日
//   T3 大数格式：zh 万/亿（1.76e10 → 176 亿）、en K/M/B；时长格式（分钟起步 → 天）
//   T4 折线路径：控制点夹在绘图区内（尖峰不过冲出画面）
//
// 用法：npm run test:usage-stats
import { build } from 'esbuild'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)
const dir = mkdtempSync(join(tmpdir(), 'usage-stats-'))
const outfile = join(dir, 'usage-stats.cjs')
await build({
  entryPoints: [join(__dirname, '../src/lib/usageStats.ts')],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile,
  logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' }, // mock.ts 的 ?raw/?url 资源
})
const U = require(outfile)

let pass = 0
let fail = 0
const ok = (cond, label) => {
  if (cond) {
    pass++
    console.log(`  ✓ ${label}`)
  } else {
    fail++
    console.log(`  ✗ ${label}`)
  }
}
const section = (name) => console.log(`\n${name}`)
const day = (d) => U.dayKeyOf(d)

// 造数据：近 90 天，每天两个模型；其中 3 天前为尖峰（模拟历史峰值日）
const today = new Date()
today.setHours(0, 0, 0, 0)
const days = []
const dayModels = []
let cum = 0
for (let i = 89; i >= 0; i--) {
  const d = U.addDays(today, -i)
  const key = day(d)
  const spike = i === 3 ? 40 : 1
  const base = i % 7 === 0 ? 0 : 1000 * spike // 每周日无活动（留白）
  cum += base * 2
  days.push({ Key: key, Input: base, Output: base, CacheRead: 0, CostUsd: 0.01 * base, CumTokens: cum })
  if (base > 0) {
    dayModels.push({ Day: key, Model: 'deepseek/deepseek-v4.1-flash', Input: base * 0.7, Output: base * 0.7, CacheRead: 0, CostUsd: 0 })
    dayModels.push({ Day: key, Model: 'opencode/glm-5.2', Input: base * 0.3, Output: base * 0.3, CacheRead: 0, CostUsd: 0 })
  }
}

section('T1 热力图（53 周网格 / 三视图 / 月份刻度）')
{
  const heat = U.buildHeatmap(days, 'day', 53, today)
  ok(heat.columns.length === 53, '列数 = 53 周')
  ok(heat.columns.every((c) => c.cells.length === 7), '每列 7 格（周日起）')
  ok(heat.columns[52].cells[0].key === day(U.weekStart(today)), '末列首格 = 本周周日')
  const todayCell = heat.columns[52].cells.find((c) => c.key === day(today))
  ok(Boolean(todayCell) && !todayCell.future, '今天格非占位')
  const futureCells = heat.columns.flatMap((c) => c.cells).filter((c) => c.future)
  ok(futureCells.length === 6 - today.getDay(), '今天之后的本周格标记 future（占位不着色）')
  ok(heat.columns.flatMap((c) => c.cells).every((c) => c.future || c.level >= 0), '存档格有档位')
  // 单日尖峰（40000）不应把普通日（2000）压到最低档：分位数分档下普通日至少 1 档
  const normal = heat.columns.flatMap((c) => c.cells).find((c) => c.value > 0 && c.value < 5000)
  ok(normal.level >= 1, `普通日 level ≥1（实际 ${normal.level}）`)
  const spikeCell = heat.columns.flatMap((c) => c.cells).find((c) => c.value > 40000)
  ok(spikeCell.level === 4, '尖峰日 = 最高档 4')
  // 无活动日 level 0（每周日）
  const blank = heat.columns.flatMap((c) => c.cells).filter((c) => c.value === 0 && !c.future)
  ok(blank.length > 0 && blank.every((c) => c.level === 0), '无活动日 level 0')
  // 月份刻度：数量合理、列号严格递增、相邻间距 ≥ 3（防叠字）
  const cols = heat.months.map((m) => m.col)
  ok(heat.months.length >= 11 && heat.months.length <= 13, `月份刻度 11~13 个（实际 ${heat.months.length}）`)
  ok(cols.every((c, i) => i === 0 || c > cols[i - 1]), '月份刻度列号递增')
  ok(cols.every((c, i) => i === 0 || c - cols[i - 1] >= 3), '相邻月份刻度间距 ≥ 3 列')
  ok(heat.months[0].label.endsWith('月'), 'zh 月份标签形如「9月」')
  const heatEn = U.buildHeatmap(days, 'day', 53, today, 'en')
  ok(/^[A-Z][a-z]{2}$/.test(heatEn.months[0].label), 'en 月份标签形如「Sep」')

  // 周视图：非未来格同值（= 该周合计）；未来格恒为占位 0
  //
  // 口径（2026-09-21 修）：当前周列里「end 之后」的格子是 future 占位（value=0、level=0），
  // 只有非未来格拿周合计 —— 旧断言 `整列 every 同值` 因此**每周只有周日（无未来格）能过**，
  // 是个日期相关的假失败。改为对「本周 7 个可能的 end」逐个验证不变量：与今天是周几无关，
  // 顺带锁住「未来格占位 = 0 且不着色」这条独立契约。
  const weekSumOf = (endDay) => days
    .filter((d) => U.weekStart(U.parseDayKey(d.Key)).getTime() === U.weekStart(endDay).getTime())
    .reduce((a, d) => a + d.Input + d.Output, 0)
  const weekStart0 = U.weekStart(today)
  for (let d = 0; d < 7; d++) {
    const end = U.addDays(weekStart0, d)
    const wk = U.buildHeatmap(days, 'week', 53, end)
    const cells = wk.columns[52].cells
    const live = cells.filter((c) => !c.future)
    const want = weekSumOf(end)
    ok(live.length > 0 && live.every((c) => c.value === want),
      `周视图 end=${day(end)}：非未来格同值 = 该周合计（${live[0]?.value} vs ${want}，未来格 ${cells.length - live.length}）`)
    ok(cells.filter((c) => c.future).every((c) => c.value === 0 && c.level === 0),
      `周视图 end=${day(end)}：未来格占位（value/level = 0）`)
  }
  // 完整历史周（整列无未来格）：整列同值 —— 原始不变量仍要成立
  const week = U.buildHeatmap(days, 'week', 53, today)
  const pastCol = week.columns.slice(0, 52).find((c) => c.cells.every((x) => !x.future) && c.cells[0].value > 0)
  ok(Boolean(pastCol) && pastCol.cells.every((c) => c.value === pastCol.cells[0].value),
    '周视图完整历史周整列同值')

  // 累计视图：取 CumTokens（单调递增），末格 = 全量
  const cumHeat = U.buildHeatmap(days, 'cum', 53, today)
  const cumCells = cumHeat.columns.flatMap((c) => c.cells).filter((c) => !c.future && c.value > 0)
  ok(cumCells.every((c, i) => i === 0 || c.value >= cumCells[i - 1].value), '累计视图单调不减')
  ok(cumHeat.max === cum, `累计视图末值 = 全量（${cumHeat.max} vs ${cum}）`)

  // 每格都带 day/cum（hover 浮层「当日 + 累计」不随视图切换而变）
  const dayCell = heat.columns.flatMap((c) => c.cells).find((c) => c.key === day(today))
  ok(dayCell.day === days[days.length - 1].Input + days[days.length - 1].Output, 'day 字段 = 当日 token')
  ok(dayCell.cum === cum, 'cum 字段 = 截至当日累计')
  const cumModeCell = cumHeat.columns.flatMap((c) => c.cells).find((c) => c.key === day(today))
  ok(cumModeCell.day === dayCell.day && cumModeCell.cum === dayCell.cum, '三视图下 day/cum 一致（累计视图也一样）')
  ok(heat.columns.flatMap((c) => c.cells).every((c) => c.future || c.before || c.day >= 0), 'day/cum 无负值')
}

section('T1b 近 90 天窗口 + flat 排布（空会话中部热力图）')
{
  const weeks = U.heatWeeksFor(90, today)
  ok(weeks === 13 || weeks === 14, `90 天跨 ${weeks} 周（13 或 14）`)
  const mini = U.buildHeatmap(days, 'day', weeks, today, 'zh', 90)
  ok(mini.activeCells === 90, `窗口内格子 = 90（实际 ${mini.activeCells}）`)
  const cells = mini.columns.flatMap((c) => c.cells)
  ok(cells.length === weeks * 7, '原始网格仍是 周数 × 7（列 = 周）')
  ok(cells.filter((c) => c.before).length === weeks * 7 - 90 - cells.filter((c) => c.future).length, '窗口外前导格标 before')
  ok(cells.filter((c) => c.before).every((c) => c.level === 0 && c.value === 0), 'before 格不着色')
  const inWin = days.slice(-90).reduce((a, d) => a + d.Input + d.Output, 0)
  ok(mini.total === inWin, `合计 = 近 90 天总量（${mini.total} vs ${inWin}）`)
  // 窗口起点 = 今天 - 89 天
  const first = cells.find((c) => !c.before && !c.future)
  ok(first.key === day(U.addDays(today, -89)), `窗口首格 = 89 天前（实际 ${first.key}）`)

  // flat 排布：5 行 × 18 天 = 90 格，时间从左到右、从上到下（空会话中部图表要横向扁平，
  // 列 = 周的 13~14 列 × 7 行在 16:9 屏上近正方形）
  const rows = U.heatGridRows(mini, 5)
  ok(rows.length === 5 && rows.every((r) => r.length === 18), `5 行 × 18 天（实际 ${rows.length} × ${rows[0]?.length}）`)
  ok(rows.every((r) => r.every((c) => c !== null)), '90 天正好铺满，无补位格')
  const flat = rows.flat()
  ok(flat.length === 90 && flat[0].key === first.key && flat[89].key === day(today), '首格 = 89 天前、末格 = 今天')
  ok(flat.every((c, i) => i === 0 || c.key > flat[i - 1].key), '日期严格递增（时间从左到右、从上到下）')
  ok(flat.every((c) => !c.before && !c.future), 'flat 只取窗口内格子（before/future 是网格占位）')
  // 天数凑不满整行时补 null（保持矩形，末行不出现锯齿边）
  const ragged = U.heatGridRows(U.buildHeatmap(days, 'day', weeks, today, 'zh', 88), 5)
  ok(ragged.length === 5 && ragged.every((r) => r.length === 18), '88 天也铺成 5 × 18（末行补位）')
  ok(ragged.flat().filter((c) => c === null).length === 2, '补位格数 = 整行天数 − 天数')
}

section('T1c 日期（浮层抬头）与列数工具')
{
  ok(U.fmtDayLong('2026-09-19', 'zh') === '9月19日 周六', `zh 日期带星期（实际 ${U.fmtDayLong('2026-09-19', 'zh')}）`)
  ok(U.fmtDayLong('2026-09-19', 'en') === 'Sat, Sep 19', `en 日期带星期（实际 ${U.fmtDayLong('2026-09-19', 'en')}）`)
  // 列数随「今天是周几」变化，但必须覆盖整个窗口
  for (let wd = 0; wd < 7; wd++) {
    const end = U.addDays(today, wd - today.getDay())
    const need = U.heatWeeksFor(30, end)
    const grid = U.buildHeatmap([], 'day', need, end, 'zh', 30)
    const start = day(U.addDays(end, -29))
    const covered = grid.columns.flatMap((c) => c.cells).find((c) => c.key === start)
    ok(Boolean(covered) && !covered.before && need >= 5 && need <= 6, `周日偏移 ${wd}：${need} 列且覆盖窗口起点`)
  }
}

section('T2 趋势（连续轴 / 分模型 / 窗口裁剪 / 合并其他）')
{
  const t = U.buildTrend(days, dayModels, 30, today)
  ok(t.keys.length === 30, 'x 轴 30 天')
  ok(t.keys[29] === day(today) && t.keys[0] === day(U.addDays(today, -29)), '轴终点 = 今天，起点 = 29 天前')
  ok(t.daysTotal.length === 30 && t.daysTotal[29] === days[days.length - 1].Input + days[days.length - 1].Output, '当日合计对齐 ByDay')
  ok(t.series.length === 2, '两条模型线')
  ok(t.series[0].model === 'deepseek/deepseek-v4.1-flash' && t.series[0].total > t.series[1].total, '按窗口内用量降序')
  ok(t.series.every((s) => s.points.length === 30), '每条线 30 个点（缺数据补 0）')
  ok(
    t.series.every((s) => s.points.every((v) => v >= 0)) && t.keys.every((k, i) => t.daysTotal[i] >= 0),
    '无负值',
  )
  // 窗口外的模型不得串入（构造 60 天前的孤点模型）
  const far = U.addDays(today, -60)
  const t2 = U.buildTrend(days, [...dayModels, { Day: day(far), Model: 'ghost/model', Input: 999, Output: 999, CacheRead: 0, CostUsd: 0 }], 30, today)
  ok(!t2.series.some((s) => s.model === 'ghost/model'), '窗口外模型被裁掉')
  // 7 天窗口：只取末 7 天
  const t7 = U.buildTrend(days, dayModels, 7, today)
  ok(t7.keys.length === 7 && t7.keys[0] === day(U.addDays(today, -6)), '7 天窗口正确切片')
  // 峰值日按日合计（不是单模型）
  ok(t.peak.key === day(U.addDays(today, -3)), `峰值日 = 3 天前（实际 ${t.peak.key}）`)
  // > 5 个模型 → 合并「其他」
  const many = []
  for (let i = 0; i < 8; i++) many.push({ Day: day(today), Model: `m/${i}`, Input: 100 - i, Output: 0, CacheRead: 0, CostUsd: 0 })
  const tMany = U.buildTrend([], many, 7, today, '其他')
  ok(tMany.series.length === 6 && tMany.series[5].model === '其他', '8 模型 → 5 条 + 其他')
  ok(tMany.series[5].total === 100 - 5 + (100 - 6) + (100 - 7), '「其他」= 尾部模型之和')
  ok(tMany.series[5].color === '--text-3', '「其他」用中性色')
  // 空数据不炸
  const empty = U.buildTrend([], [], 30, today)
  ok(empty.series.length === 0 && empty.max === 1 && empty.keys.length === 30, '空报告不抛错')
}

section('T3 格式（大数 / 时长）')
{
  ok(U.fmtBigTokens(0, 'zh') === '0', 'zh 0')
  ok(U.fmtBigTokens(536000, 'zh') === '53.6 万', `zh 53.6 万（实际 ${U.fmtBigTokens(536000, 'zh')}）`)
  ok(U.fmtBigTokens(17640400019, 'zh') === '176 亿', `zh 176 亿（实际 ${U.fmtBigTokens(17640400019, 'zh')}）`)
  ok(U.fmtBigTokens(17640400019, 'en') === '17.6B', `en 17.6B（实际 ${U.fmtBigTokens(17640400019, 'en')}）`)
  ok(U.fmtBigTokens(1200000, 'en') === '1.2M', 'en 1.2M')
  ok(U.fmtBigTokens(4500, 'en') === '4.5K', 'en 4.5K')
  ok(U.fmtChatDuration(0, 'zh') === '0 分钟', '时长 0 → 0 分钟')
  ok(U.fmtChatDuration(59_000, 'zh') === '1 分钟', '不足 1 分钟向上取整')
  ok(U.fmtChatDuration(2 * 3600e3 + 30 * 60e3, 'zh') === '2 小时 30 分', '小时+分')
  ok(U.fmtChatDuration(680_956_753, 'zh') === '7 天 21 小时', `真实最长会话（实际 ${U.fmtChatDuration(680_956_753, 'zh')}）`)
  ok(U.fmtChatDuration(680_956_753, 'en') === '7d 21h', 'en 天+小时')
  ok(U.fmtDayLabel('2026-09-19', 'zh') === '9月19日' && U.fmtDayLabel('2026-09-19', 'en') === '9/19', 'x 轴刻度双语')
  // 图例短名：真实模型 id 自带层级时只留末两段（全名走 title）
  ok(U.modelLabel('command-code/deepseek/deepseek-v4-flash') === 'deepseek/deepseek-v4-flash', '三层模型 id 取末两段')
  ok(U.modelLabel('deepseek/deepseek-v4.1-flash') === 'deepseek/deepseek-v4.1-flash', '两层不变')
  ok(U.modelLabel('glm-5.2') === 'glm-5.2', '裸模型名不变')
  const labels = U.trendLabels(['a/deepseek/m', 'b/deepseek/m', 'deepseek/deepseek-v4.1-flash'])
  ok(labels.get('a/deepseek/m') === 'a/deepseek/m' && labels.get('b/deepseek/m') === 'b/deepseek/m', '短名撞车 → 回退全名')
  ok(labels.get('deepseek/deepseek-v4.1-flash') === 'deepseek/deepseek-v4.1-flash', '无冲突保持短名')
}

section('T4 折线路径')
{
  const pts = [
    { x: 0, y: 100 },
    { x: 10, y: 0 },
    { x: 20, y: 100 },
    { x: 30, y: 50 },
  ]
  const d = U.smoothPath(pts, 0, 100)
  ok(d.startsWith('M0.00 100.00') && d.includes('C'), '生成贝塞尔路径')
  const ys = [...d.matchAll(/[-\d.]+\s([-\d.]+)/g)].map((m) => Number(m[1]))
  ok(ys.every((y) => y >= 0 && y <= 100), '控制点 y 夹在绘图区内（尖峰不过冲）')
  ok(U.smoothPath([]) === '', '空点集 → 空路径')
  const two = U.smoothPath([{ x: 0, y: 0 }, { x: 5, y: 5 }], 0, 10)
  ok(two === 'M0.00 0.00 L5.00 5.00', '两点退化为直线')
}

console.log(`\n结果：${pass} 通过 / ${fail} 失败`)
process.exit(fail === 0 ? 0 : 1)
