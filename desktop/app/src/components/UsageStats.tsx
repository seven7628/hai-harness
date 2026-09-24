import { useEffect, useMemo, useRef, useState } from 'react'
import type { CSSProperties, MouseEvent as ReactMouseEvent } from 'react'
import { useAppStore, metricsWindowFrom } from '../store/useAppStore'
import { useT, useLang } from '../i18n'
import type { Lang } from '../i18n'
import {
  buildHeatmap,
  buildTrend,
  fmtBigTokens,
  fmtChatDuration,
  fmtDayLabel,
  fmtDayLong,
  heatGridRows,
  heatWeeksFor,
  smoothPath,
} from '../lib/usageStats'
import type { HeatCell, Heatmap as HeatmapData, HeatMode, Trend } from '../lib/usageStats'

// 使用统计（设置弹窗「数据与统计 → 使用统计」）。
// 版面对齐设计稿：指标卡（累计/峰值/最长聊天/连续天数/成本）→ Token 活动热力图（每日/每周/累计）
// → 时间范围（近 7 日/近 30 日）+ 每日 Token 趋势图（分模型折线）。
// 数据源 = bridge metrics 命令；一次拉取覆盖近一年（热力图窗口），7/30 天与三种热力图视图都在
// 前端切同一份报告 —— 全量 JSONL 扫描是数百毫秒级，不能随切换重跑。
export default function UsageStats() {
  const t = useT()
  const lang = useLang()
  const metrics = useAppStore((s) => s.metrics)
  const fetchMetrics = useAppStore((s) => s.fetchMetrics)
  const ensureMetrics = useAppStore((s) => s.ensureMetrics)
  const [heatMode, setHeatMode] = useState<HeatMode>('day')
  const [range, setRange] = useState('30')

  // 进页面按需拉取（60s 缓存内直接复用；设置页 tab 每次进入都会重挂，全量扫描不能跟着重跑）。
  // 「刷新」按钮走 fetchMetrics = 强制拉一次。
  useEffect(() => {
    ensureMetrics()
  }, [ensureMetrics])

  const stats = metrics?.Stats
  // 思考占比（2026-09-23 补齐维度）：Reasoning ⊆ Output，此前报表完全没有思考维度 ——
  // 长思考模型（reasoning_effort 高）下「思考占了多少」直接影响成本解释。
  const thinkPart = useMemo(() => {
    const out = metrics?.Total?.Output ?? 0
    const think = metrics?.Total?.Reasoning ?? 0
    if (think <= 0 || out <= 0) return ''
    return t('usage.card.reasoning')
      .replace('{tok}', fmtBigTokens(think, lang))
      .replace('{pct}', ((think / out) * 100).toFixed(1))
  }, [metrics, t, lang])
  const days = useMemo(() => metrics?.ByDay ?? [], [metrics])
  const dayModels = useMemo(() => metrics?.ByDayModel ?? [], [metrics])
  const others = t('usage.others')
  const heat = useMemo(() => buildHeatmap(days, heatMode, 53, new Date(), lang), [days, heatMode, lang])
  const trend = useMemo(() => buildTrend(days, dayModels, Number(range), new Date(), others), [days, dayModels, range, others])

  // 无价调用占比（2026-09-23）：成本 0 是「不知道花了多少」而非免费 —— 报表要能自证可信度。
  // 分母只算有价 + 无价（旧行 unknown 单列，否则历史数据会把占比稀释成无意义数字）。
  // 主要来源 = ByModel 里无价调用最多的前三个模型（"模型(N)"），便于直接去补价。
  const unpriced = useMemo(() => {
    const s = stats
    if (!s) return undefined
    const up = s.UnpricedCalls ?? 0
    const priced = s.PricedCalls ?? 0
    const unknown = s.UnknownPricingCalls ?? 0
    if (up === 0 && unknown === 0) return undefined
    const known = priced + up
    const pct = known > 0 ? (up / known) * 100 : 0
    const top = (metrics?.ByModel ?? [])
      .filter((g) => (g.UnpricedCalls ?? 0) > 0)
      .sort((a, b) => (b.UnpricedCalls ?? 0) - (a.UnpricedCalls ?? 0))
      .slice(0, 3)
      .map((g) => `${g.Key}(${g.UnpricedCalls})`)
      .join(' · ')
    return { up, priced, unknown, pct, top }
  }, [stats, metrics])

  const empty = !stats || (stats.ActiveDays === 0 && days.length === 0)

  return (
    <div className="usage-stats">
      <div className="us-hd">
        <div>
          <div className="settings-sec-title">{t('settings.usage')}</div>
          <div className="set-desc">{t('usage.desc')}</div>
        </div>
        <button className="panel-refresh" onClick={() => fetchMetrics({ from: metricsWindowFrom() })} title={t('usage.refresh')}>
          ⟳ {t('usage.refresh')}
        </button>
      </div>

      {empty ? (
        <div className="us-empty">{t('usage.empty')}</div>
      ) : (
        <>
          <div className="us-cards">
            <StatCard
              value={fmtBigTokens(stats!.TotalTokens, lang)}
              label={t('usage.card.totalTokens')}
              hint={
                t('usage.card.totalTokensHint')
                  .replace('{from}', stats!.FirstDay || '—')
                  .replace('{to}', stats!.LastDay || '—') + thinkPart
              }
            />
            <StatCard
              value={fmtBigTokens(stats!.PeakDayTokens, lang)}
              label={t('usage.card.peakTokens')}
              hint={stats!.PeakDay ? t('usage.card.peakDay').replace('{day}', stats!.PeakDay) : undefined}
            />
            <StatCard
              value={fmtChatDuration(stats!.LongestChatMs, lang)}
              label={t('usage.card.longestChat')}
              hint={t('usage.card.longestChatHint')}
            />
            <StatCard
              value={t('usage.card.days').replace('{n}', String(stats!.CurrentStreak))}
              label={t('usage.card.currentStreak')}
              hint={t('usage.card.currentStreakHint')}
            />
            <StatCard
              value={t('usage.card.days').replace('{n}', String(stats!.LongestStreak))}
              label={t('usage.card.longestStreak')}
              hint={t('usage.card.activeDays').replace('{n}', String(stats!.ActiveDays))}
            />
            <StatCard
              value={fmtCost(stats!.TotalCostUsd)}
              label={t('usage.card.cost')}
              hint={t('usage.card.costHint')
                .replace('{sessions}', String(stats!.Sessions))
                .replace('{days}', String(stats!.ActiveDays))}
            />
            {unpriced && (
              <StatCard
                value={`${unpriced.pct.toFixed(unpriced.pct < 10 ? 1 : 0)}%`}
                label={t('usage.card.unpriced')}
                hint={t('usage.card.unpricedHint')
                  .replace('{unpriced}', String(unpriced.up))
                  .replace('{priced}', String(unpriced.priced))
                  .replace('{unknown}', String(unpriced.unknown))
                  .replace('{models}', unpriced.top || '—')}
              />
            )}
          </div>

          <section className="us-block">
            <div className="us-block-hd">
              <h4>{t('usage.activity')}</h4>
              <Seg
                ariaLabel={t('usage.activity')}
                value={heatMode}
                onChange={(v) => setHeatMode(v as HeatMode)}
                options={[
                  { value: 'day', label: t('usage.mode.day') },
                  { value: 'week', label: t('usage.mode.week') },
                  { value: 'cum', label: t('usage.mode.cum') },
                ]}
              />
            </div>
            <Heatmap heat={heat} lang={lang} />
          </section>

          <section className="us-block us-trend-block">
            <div className="us-block-hd">
              <h4>{t('usage.range')}</h4>
              <Seg
                ariaLabel={t('usage.range')}
                value={range}
                onChange={setRange}
                options={[
                  { value: '7', label: t('usage.range.7') },
                  { value: '30', label: t('usage.range.30') },
                ]}
              />
            </div>
            <div className="us-chart-card">
              <div className="us-chart-hd">
                <span className="us-chart-title">{t('usage.trend')}</span>
                {trend.peak.value > 0 && (
                  <span className="us-chart-peak">
                    {t('usage.peak')} {fmtBigTokens(trend.peak.value, lang)} · {fmtDayLabel(trend.peak.key, lang)}
                  </span>
                )}
              </div>
              <Legend series={trend.series} lang={lang} />
              <TrendChart trend={trend} lang={lang} />
            </div>
          </section>
        </>
      )}
    </div>
  )
}

// UsageActivityMini 空会话（新建会话、还没有任何消息）中部图表：近 90 天 Token 活动热力图
// （每行 18 天 × 5 行 = 90 格，全部落在窗口内，没有占位格）。
// 排布按天从左到右、从上到下铺开（heatGridRows）：列 = 周的 13~14 列 × 7 行在 16:9 屏上
// 近正方形、头重脚轻，横向扁条才和标题/范围文案同一读法（时间从左到右，日期由 hover 行给出）。
// 跨会话看板本身在设置「使用统计」页，这里只放一张「这里在用、有量」的紧凑实证图：
// hover 把「日期 · 当日 · 累计」内联到标题下一行（卡片窄，浮层会压住半张图）。只读 store 报告，
// ensureMetrics 按需拉取（命中 60s 缓存时不发命令）。
export function UsageActivityMini() {
  const t = useT()
  const lang = useLang()
  const metrics = useAppStore((s) => s.metrics)
  const ensureMetrics = useAppStore((s) => s.ensureMetrics)
  const [hoverCell, setHoverCell] = useState<HeatCell | null>(null)
  useEffect(() => {
    ensureMetrics()
  }, [ensureMetrics])

  const heat = useMemo(
    () => buildHeatmap(metrics?.ByDay ?? [], 'day', heatWeeksFor(MINI_DAYS), new Date(), lang, MINI_DAYS),
    [metrics, lang],
  )
  // 没有历史用量 → 不留空图表（空态保持原样，不干扰引导文案）
  if (heat.total <= 0) return null
  const keys = heat.columns.flatMap((c) => c.cells).filter((c) => !c.future && !c.before)
  const first = keys[0]?.key ?? ''
  const last = keys[keys.length - 1]?.key ?? ''
  const days = String(MINI_DAYS)

  return (
    <div className="us-mini">
      <div className="us-mini-hd">
        <span>{t('usage.activityMini').replace('{n}', days)}</span>
        <span className="us-mini-total">
          {t('usage.totalDays').replace('{n}', days)} <b className="mono">{fmtBigTokens(heat.total, lang)}</b>
        </span>
      </div>
      <div className={`us-mini-range ${hoverCell ? 'on' : ''}`}>
        {hoverCell
          ? `${fmtDayLong(hoverCell.key, lang)} · ${t('usage.dayTokens')} ${fmtBigTokens(hoverCell.day, lang)} · ${t('usage.cumTokens')} ${fmtBigTokens(hoverCell.cum, lang)}`
          : t('usage.dayRange').replace('{from}', fmtDayLabel(first, lang)).replace('{to}', fmtDayLabel(last, lang))}
      </div>
      <Heatmap
        heat={heat}
        lang={lang}
        compact
        layout="flat"
        flatRows={MINI_ROWS}
        ariaLabel={t('usage.activityMini').replace('{n}', days)}
        onHover={setHoverCell}
        showTip={false}
      />
    </div>
  )
}

// —— 通用小件 ——

function Seg({
  value,
  options,
  onChange,
  ariaLabel,
}: {
  value: string
  options: { value: string; label: string }[]
  onChange: (v: string) => void
  ariaLabel: string
}) {
  return (
    <div className="seg" role="radiogroup" aria-label={ariaLabel}>
      {options.map((o) => (
        <button
          key={o.value}
          className={o.value === value ? 'cur' : ''}
          aria-pressed={o.value === value}
          onClick={() => onChange(o.value)}
        >
          {o.label}
        </button>
      ))}
    </div>
  )
}

function StatCard({ value, label, hint }: { value: string; label: string; hint?: string }) {
  return (
    <div className="us-card" title={hint}>
      <div className="us-card-v mono">{value}</div>
      <div className="us-card-l">{label}</div>
    </div>
  )
}

function Legend({ series, lang, compact = false }: { series: Trend['series']; lang: Lang; compact?: boolean }) {
  if (series.length === 0) return null
  return (
    <div className={`us-legend ${compact ? 'compact' : ''}`}>
      {series.map((s) => (
        <span key={s.model} className="us-legend-item" title={s.model}>
          <i style={{ background: `var(${s.color})` }} />
          <span className="us-legend-name mono">{s.label}</span>
          <span className="us-legend-val mono">{fmtBigTokens(s.total, lang)}</span>
        </span>
      ))}
    </div>
  )
}

// 热力图：列 = 周（周日起），行 = 周日..周六；默认 53 周 ≈ 一年（空会话中部是 90 天 × flat）。
// hover 出自绘浮层（日期 + 当日用量 + 截至当日累计）—— 原生 title 只能给一行纯文本，
// 而这里要同时给「当天用了多少」与「累计到当天多少」两个数，并随三视图切换保持一致。
function Heatmap({
  heat,
  lang,
  compact = false,
  layout = 'cols',
  flatRows = 5,
  ariaLabel,
  onHover,
  showTip = true,
}: {
  heat: HeatmapData
  lang: Lang
  compact?: boolean
  // 排布：cols = 列是周、每列 7 天（设置页 53 周，横向铺开跨度长，GitHub 同款）；
  //      rows = 行是周、每行 7 天（竖长条在窄卡片里像一根柱子，横排更耐看）；
  //      flat = 每行 flatRows 天、按天从左到右铺开（空会话 90 天：13~14 列 × 7 行近正方形，
  //             16:9 屏上突兀，摊成横条才顺眼）。
  layout?: 'cols' | 'rows' | 'flat'
  flatRows?: number // flat 排布的行数（每行天数 = 窗口天数 / 行数，向上取整）
  ariaLabel?: string // 无障碍名（默认「近一年」；空会话窗口是 90 天，得按窗口改口径）
  onHover?: (cell: HeatCell | null) => void // 悬停联动（空会话卡片把信息内联到标题行，不用浮层）
  showTip?: boolean
}) {
  const t = useT()
  const [hover, setHover] = useState<{ cell: HeatCell; x: number; y: number; below: boolean } | null>(null)
  const wrapRef = useRef<HTMLDivElement>(null)
  const cols = heat.columns.length
  const cellCount = cols * 7
  // 一组 = 一个 flex 容器：cols 排布一组是一周（竖着 7 格），rows/flat 排布一组是一行。
  // flat 的补位格是 null（天数凑不满整行时保持矩形）。
  const groups: (HeatCell | null)[][] =
    layout === 'flat' ? heatGridRows(heat, flatRows) : heat.columns.map((c) => c.cells)

  // 浮层锚在格子上/下方：水平以格子中心对齐并夹边 76px（≈ 半个浮层宽，防贴边溢出把滚动容器
  // 顶出横向滚动条）；顶部两行的格子把浮层放下面 —— 否则会盖住月份刻度与「Token 活动」标题
  // （行号由格子索引给出，不靠像素反推：行高随窗口缩放，算出来不稳）。
  const TIP_HALF = 76
  const onEnter = (e: ReactMouseEvent<HTMLElement>, cell: HeatCell, row: number) => {
    if (cell.future || cell.before) return
    const wrap = wrapRef.current
    if (!wrap) return
    const r = e.currentTarget.getBoundingClientRect()
    const w = wrap.getBoundingClientRect()
    const x = Math.min(Math.max(r.left - w.left + r.width / 2, TIP_HALF), Math.max(TIP_HALF, w.width - TIP_HALF))
    // cols 排布下顶部两行的格子把浮层放下面（否则压住月份刻度和区块标题）；
    // rows/flat 排布（空会话）不显示浮层，走 onHover 内联，row 只是组内序号，不参与判断。
    const below = layout === 'cols' && row <= 1
    setHover({ cell, x, y: below ? r.top - w.top + r.height + 6 : r.top - w.top - 6, below })
    onHover?.(cell)
  }

  return (
    <div
      className={`us-heat-wrap ${compact ? 'compact' : ''}`}
      ref={wrapRef}
      onMouseLeave={() => {
        setHover(null)
        onHover?.(null)
      }}
    >
      <div className="us-heat-months" aria-hidden>
        {heat.columns.map((_, i) => (
          <span key={i}>{heat.months.find((m) => m.col === i)?.label ?? ''}</span>
        ))}
      </div>
      <div
        className={`us-heat ${layout}`}
        role="img"
        aria-label={ariaLabel ?? t('usage.heatAria')}
        style={{ '--us-heat-cols': cols, '--us-heat-cells': cellCount } as CSSProperties}
      >
        {groups.map((cells, gi) => (
          // rows/flat 排布：一组渲染成一行（列名沿用 us-heat-col，只是 flex 方向不同）
          <div className={layout === 'cols' ? 'us-heat-col' : 'us-heat-row'} key={gi}>
            {cells.map((cell, row) =>
              cell === null ? (
                // flat 排布凑不满整行的补位格：只是形状占位，不参与 hover/统计
                <span key={`pad-${gi}-${row}`} className="us-heat-cell before" aria-hidden="true" />
              ) : (
                <span
                  key={cell.key}
                  data-key={cell.key} // 供 hover/测试按日期定位格子
                  className={`us-heat-cell ${cell.future ? 'future' : cell.before ? 'before' : `lv${cell.level}`}`}
                  onMouseEnter={(e) => onEnter(e, cell, row)}
                />
              ),
            )}
          </div>
        ))}
      </div>
      <div className="us-heat-legend">
        <span>{t('usage.less')}</span>
        {[0, 1, 2, 3, 4].map((lv) => (
          <i key={lv} className={`us-heat-cell lv${lv}`} />
        ))}
        <span>{t('usage.more')}</span>
      </div>
      {showTip && hover && (
        <div
          className={`us-tip us-heat-tip ${hover.below ? 'below' : ''}`}
          style={{ left: hover.x, top: hover.y }}
          aria-hidden="true"
        >
          <div className="us-tip-hd">{fmtDayLong(hover.cell.key, lang)}</div>
          <div className="us-tip-row">
            <span className="us-tip-name">{t('usage.dayTokens')}</span>
            <span className="mono">{fmtBigTokens(hover.cell.day, lang)}</span>
          </div>
          <div className="us-tip-row">
            <span className="us-tip-name">{t('usage.cumTokens')}</span>
            <span className="mono">{fmtBigTokens(hover.cell.cum, lang)}</span>
          </div>
        </div>
      )}
    </div>
  )
}

// 折线图（自绘 SVG，不引图表库）：x = 自然日，y = token；每模型一条平滑曲线。
// 空态/单点都安全（smoothPath 退化为直线），hover 出竖线 + 浮层（当日各模型明细）。
// 空会话中部的窗口：近 90 天（热力图按 5 行 × 18 天铺开，格子数按 90 天实算）
const MINI_DAYS = 90
const MINI_ROWS = 5

const CHART_W = 720

function TrendChart({
  trend,
  lang,
  height = 210,
  compact = false,
}: {
  trend: Trend
  lang: Lang
  height?: number
  compact?: boolean
}) {
  const t = useT()
  const [hover, setHover] = useState<number | null>(null)
  const padT = 10
  const padB = compact ? 20 : 26
  const padX = 8
  const plotH = height - padT - padB
  const n = trend.keys.length
  const step = n > 1 ? (CHART_W - padX * 2) / (n - 1) : 0
  const xOf = (i: number) => padX + i * step
  const scaleMax = niceMax(trend.max)
  const yOf = (v: number) => padT + plotH - (Math.max(0, v) / scaleMax) * plotH
  // x 轴刻度：约 8 个（含末点），间隔取整避免标签重叠
  const every = Math.max(1, Math.ceil(n / 8))
  const ticks = trend.keys.map((k, i) => ({ k, i })).filter(({ i }) => (n - 1 - i) % every === 0)

  if (n === 0) return <div className="us-chart-empty">{t('usage.empty')}</div>

  const hovered = hover != null && hover >= 0 && hover < n ? hover : null

  return (
    <div className={`us-chart ${compact ? 'compact' : ''}`}>
      <svg
        viewBox={`0 0 ${CHART_W} ${height}`}
        width="100%"
        role="img"
        aria-label={t('usage.trend')}
        onMouseLeave={() => setHover(null)}
        onMouseMove={(e) => {
          const rect = e.currentTarget.getBoundingClientRect()
          const x = ((e.clientX - rect.left) / Math.max(1, rect.width)) * CHART_W
          const i = Math.round((x - padX) / (step || 1))
          setHover(Math.min(n - 1, Math.max(0, i)))
        }}
      >
        {/* 3 条基准线（25/50/75%）：给曲线一个高度参照，不用画满格网 */}
        {[0.25, 0.5, 0.75].map((f) => (
          <line
            key={f}
            x1={padX}
            x2={CHART_W - padX}
            y1={padT + plotH * f}
            y2={padT + plotH * f}
            className="us-grid"
          />
        ))}
        <line x1={padX} x2={CHART_W - padX} y1={padT + plotH} y2={padT + plotH} className="us-axis" />

        {trend.series.map((s) => (
          <path
            key={s.model}
            d={smoothPath(
              s.points.map((v, i) => ({ x: xOf(i), y: yOf(v) })),
              0,
              padT + plotH,
            )}
            fill="none"
            stroke={`var(${s.color})`}
            strokeWidth={compact ? 1.4 : 1.7}
            strokeLinecap="round"
          />
        ))}

        {hovered != null && (
          <>
            <line x1={xOf(hovered)} x2={xOf(hovered)} y1={padT} y2={padT + plotH} className="us-cross" />
            {trend.series.map((s) => (
              <circle key={s.model} cx={xOf(hovered)} cy={yOf(s.points[hovered])} r={2.6} fill={`var(${s.color})`} />
            ))}
          </>
        )}

        {ticks.map(({ k, i }) => (
          // 首末刻度改用起/止对齐：居中锚点会把「9月13日」半个字挤出绘图区（左上角被裁）
          <text
            key={k}
            x={xOf(i)}
            y={height - 6}
            textAnchor={i === 0 ? 'start' : i === n - 1 ? 'end' : 'middle'}
            className="us-tick"
          >
            {fmtDayLabel(k, lang)}
          </text>
        ))}
      </svg>

      {hovered != null && (
        <div
          className="us-tip"
          // 夹在容器内：靠边的点在 translateX(-50%) 下会把浮层挤出卡片（浮层宽 ~180px，
          // 半宽约占 15% → 限位到 [16%, 84%]，竖线仍指在真实位置）
          style={{ left: `${Math.min(84, Math.max(16, (xOf(hovered) / CHART_W) * 100))}%` }}
          aria-hidden="true"
        >
          <div className="us-tip-hd mono">
            {trend.keys[hovered]} · {fmtBigTokens(trend.daysTotal[hovered], lang)}
          </div>
          {trend.series.map((s) => (
            <div key={s.model} className="us-tip-row" title={s.model}>
              <i style={{ background: `var(${s.color})` }} />
              <span className="us-tip-name mono">{s.label}</span>
              <span className="mono">{fmtBigTokens(s.points[hovered], lang)}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

// niceMax y 轴上限取整（1/2/2.5/5 × 10^n）：曲线不会贴着顶边，刻度也好读。
function niceMax(v: number): number {
  if (v <= 0) return 1
  const exp = Math.floor(Math.log10(v))
  const base = Math.pow(10, exp)
  for (const m of [1, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10]) {
    if (v <= m * base) return m * base
  }
  return 10 * base
}

// fmtCost 成本：小额 4 位小数（单会话常 < 1¢），大额 2 位。与 ui.tsx fmtUsd 同口径，
// 但这里要显示「累计美元」量级（$40.66），不需要 $0 特例。
function fmtCost(n: number): string {
  if (!n || n < 0) return '$0'
  if (n < 0.01) return `$${n.toFixed(4)}`
  if (n < 100) return `$${n.toFixed(2)}`
  return `$${Math.round(n)}`
}
