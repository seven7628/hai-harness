import { useMemo } from 'react'
import { useAppStore } from '../store/useAppStore'
import type { CtxBreakdown } from '../store/useAppStore'
import { fmtTokens } from './ui'
import { useT } from '../i18n'

// 上下文构成面板（Composer ctx 环 hover）：分区占比 + 分段条形图 + 缓存命中率。
//
// 口径（见 bridge context_breakdown，2026-09-20 重定）：**静态分区**（系统提示词/技能/
// 工具/MCP/其他）按「请求组装字节」估算并**固定展示** —— 它们每轮原样进请求，大小只随
// 工具集/技能集/模式变化；**消息上下文 = 总量 − 静态分区合计**（残差），总量与 ctx 环
// 逐字一致。旧的「按最近一轮真实用量等比缩放」会把会话内容摊派到静态块上（实测 6k 的
// 工具 schema 在 93k 上下文里显示成 82k），故废弃。
//
// 面板不做交互（纯展示），由 Composer 的 hover 容器控制显隐，鼠标可移入面板而不关闭。
//
// 分区配色（水墨主题内取色，两套主题自动跟随）：消息=黛青（同 ctx 环主色）、
// 工具=赭石、MCP=朱砂（外部集成）、系统提示词=竹青、技能=黛青+朱砂混色、其他=灰。
const PART_ORDER = ['system_prompt', 'skills', 'tools', 'mcp', 'messages', 'other']

export default function CtxBreakdownPanel({
  used,
  window: win,
  pct,
  data,
  ctxCachePct,
  sessionCachePct,
  ctxCacheWrite,
  sessionCacheWrite,
}: {
  used: number
  window: number
  pct: number
  data?: CtxBreakdown // 已按会话归属过滤（undefined = 尚未拉到 / 属于别的会话）
  ctxCachePct?: number // 当前上下文命中率（最近一轮主 agent 调用 cacheRead/input）
  sessionCachePct?: number // 会话累计命中率（= 状态栏「缓存」同一个数）
  ctxCacheWrite?: number // 本轮写入缓存量（Anthropic cache_creation；其余协议恒 0/缺省）
  sessionCacheWrite?: number // 本会话累计写入缓存量
}) {
  const t = useT()
  const activeSessionId = useAppStore((s) => s.activeSessionId)
  const total = data?.total ?? 0
  // 行序：占用降序（大块先见）；同值保持 SDK 的稳定分区序（Array.sort 稳定）。
  const rows = useMemo(() => {
    const parts = data?.parts ?? []
    const list = PART_ORDER.map((key) => {
      const tokens = parts.find((p) => p.key === key)?.tokens ?? 0
      return { key, tokens, pct: total > 0 ? (tokens / total) * 100 : 0 }
    })
    return list.sort((a, b) => b.tokens - a.tokens)
  }, [data, total])
  // 底部说明三态：锚点过期（消息被钳到 0）> 估算锚点（压缩后）> 真实锚点。
  const note = data?.degraded ? t('ctx.panel.noteStale') : data?.estimated ? t('ctx.panel.noteEstimate') : t('ctx.panel.note')

  return (
    <div className="ctx-panel" role="tooltip">
      <div className="ctx-panel-hd">
        <span className="ctx-panel-title">{t('ctx.panel.title')}</span>
        <span className="ctx-panel-total mono">
          {fmtTokens(used)}/{fmtTokens(win)} <span className="dim">({pct}%)</span>
        </span>
      </div>
      <div className="ctx-bar" aria-hidden="true">
        {rows.map((r) => (r.tokens > 0 ? <span key={r.key} className={`ctx-seg ctx-c-${r.key}`} style={{ flex: `${r.tokens} 1 0` }} /> : null))}
      </div>
      {data ? (
        <div className="ctx-rows">
          {rows.map((r) => (
            <div className="ctx-row" key={r.key}>
              <span className={`ctx-dot ctx-c-${r.key}`} />
              <span className="ctx-name">{t('ctx.panel.part.' + r.key)}</span>
              <span className="ctx-tok mono">{r.tokens > 0 ? fmtTokens(r.tokens) : '—'}</span>
              <span className="ctx-pct mono">{r.pct.toFixed(1)}%</span>
            </div>
          ))}
        </div>
      ) : (
        <div className="ctx-rows ctx-empty">{t(activeSessionId ? 'ctx.panel.loading' : 'ctx.panel.noSession')}</div>
      )}
      {/* 缓存命中率两个口径：当前上下文（主，与面板其余内容同源）/ 会话累计（次，
          与状态栏「缓存」一致）。两者差异 = 历史轮次/缓存过期/子 agent 冷前缀的拖累。
          格式与状态栏「缓存」对齐（两位小数），便于逐字对照。 */}
      <div className="ctx-panel-ft">
        <div className="ctx-ft-row">
          <span>{t('ctx.panel.cacheNow')}</span>
          <b className="mono">{ctxCachePct == null ? '—' : `${ctxCachePct.toFixed(2)}%`}</b>
        </div>
        <div className="ctx-ft-row sub">
          <span>{t('ctx.panel.cacheHit')}</span>
          <span className="mono">{sessionCachePct == null ? '—' : `${sessionCachePct.toFixed(2)}%`}</span>
        </div>
        {/* 写入量：命中率之外的第二个信号 —— 「这轮是在读缓存还是整段重写缓存」只有写入量
            能回答（前缀失效时 cacheRead 归零、cacheWrite 顶到整段上下文）。OpenAI/DeepSeek
            等协议端点无该计数（恒 0/缺省）→ 不渲染，保持面板干净。 */}
        {sessionCacheWrite != null && sessionCacheWrite > 0 && (
          <div className="ctx-ft-row sub">
            <span>{t('ctx.panel.cacheWriteNow')}</span>
            <span className="mono">{ctxCacheWrite == null ? '—' : `${fmtTokens(ctxCacheWrite)} tok`}</span>
          </div>
        )}
        {sessionCacheWrite != null && sessionCacheWrite > 0 && (
          <div className="ctx-ft-row sub">
            <span>{t('ctx.panel.cacheWriteAll')}</span>
            <span className="mono">{fmtTokens(sessionCacheWrite)} tok</span>
          </div>
        )}
      </div>
      <div className="ctx-panel-note">{note}</div>
    </div>
  )
}
