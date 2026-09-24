import { useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import type { UsageAgg } from '../store/useAppStore'
import { ChevronDown } from './icons'

// 上下文占用圆环（数值+占比文字并排，不靠颜色；阈值双编码）
export function CtxRing({ pct, size = 16, stroke = 3 }: { pct: number; size?: number; stroke?: number }) {
  const c = 2 * Math.PI * 7
  const off = c * (1 - Math.min(100, Math.max(0, pct)) / 100)
  // 阈值双编码（数值+占比文字已并排，不靠颜色单独承担信息）；
  // 色值走语义 token 而不是写死的水墨三色（写死的那份在 2026-09-21 换主题后与界面脱节）
  const cls = pct >= 95 ? 'var(--error)' : pct >= 90 ? 'var(--warning)' : 'var(--indigo)'
  return (
    <svg width={size} height={size} viewBox="0 0 18 18">
      <circle cx="9" cy="9" r="7" fill="none" stroke="var(--ink-line)" strokeWidth={stroke} />
      <circle
        cx="9" cy="9" r="7" fill="none" stroke={cls} strokeWidth={stroke}
        strokeDasharray={c.toFixed(1)} strokeDashoffset={off.toFixed(1)} strokeLinecap="round"
      />
    </svg>
  )
}

// 格式化为紧凑 token 数（1.2k / 45k / 200k）
export function fmtTokens(n: number): string {
  if (n >= 1000000) return `${(n / 1000000).toFixed(1)}M`
  if (n >= 1000) return `${Math.round(n / 1000)}k`
  return String(n)
}

// 格式化为成本（美元）：小额 4 位小数（单轮常 < 1¢），大额 2 位；
// 极小额度（单任务/单轮可低至几十 µUSD）用 6 位——否则 4 位下四舍五入成 $0.0000，
// 看起来像「没花钱」（与「有值但极小」混淆）。
export function fmtUsd(n: number): string {
  if (!n || n < 0) return '$0'
  if (n < 0.0001) return `$${n.toFixed(6)}`
  if (n < 0.01) return `$${n.toFixed(4)}`
  if (n < 1) return `$${n.toFixed(3)}`
  return `$${n.toFixed(2)}`
}

// 用量徽章：tokens（in+out）+ 成本（USD）。任务卡 / 工具行 / 压缩卡共用一套口径与排版
// （单一实现，避免各处各写一遍导致口径漂移）。
// usage 缺失或全零（普通工具不花模型钱）→ 不渲染（不留空白用量位）。
// title 由调用方给：任务级与单次调用级文案不同（i18n 的 task.usage.tip / usage.tip）。
export function UsageChip({ usage, title, className = 'mono' }: { usage?: UsageAgg; title?: string; className?: string }) {
  if (!usage) return null
  const tok = usage.input + usage.output
  if (tok <= 0 && !usage.costUsd) return null
  return (
    <span className={className} title={title}>
      {fmtTokens(tok)} tok{usage.costUsd ? ` · ${fmtUsd(usage.costUsd)}` : ''}
    </span>
  )
}

// 格式化为人类可读耗时（工具 / SubAgent / LLM 执行时间）：<1s → 毫秒；<1min → 秒；
// <1h → 分+秒；≥1h → 小时+分。未知（null/undefined）→ '—'。
export function fmtDuration(ms?: number): string {
  if (ms == null || ms < 0) return '—'
  if (ms < 1000) return `${Math.max(1, Math.round(ms))}ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`
  if (ms < 3_600_000) {
    const m = Math.floor(ms / 60_000)
    const s = Math.round((ms % 60_000) / 1000)
    return s >= 1 ? `${m}m ${s}s` : `${m}m`
  }
  const h = Math.floor(ms / 3_600_000)
  const m = Math.floor((ms % 3_600_000) / 60_000)
  return m >= 1 ? `${h}h ${m}m` : `${h}h`
}

// fmtRun：运行中时钟的整秒显示（0s → 1s → 2s → … → 1m 30s），逐秒感知，不显示小数。
export function fmtRun(ms?: number): string {
  if (ms == null || ms < 0) return '—'
  const total = Math.floor(ms / 1000)
  const m = Math.floor(total / 60)
  const s = total % 60
  return m >= 1 ? `${m}m ${s}s` : `${s}s`
}

// 格式化为本地日期时间（YYYY-MM-DD HH:MM:SS），用于消息时间戳回显。
export function fmtClock(ts?: number): string {
  if (ts == null) return ''
  const d = new Date(ts)
  const pad = (value: number) => String(value).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

// unified diff 渲染（变更 Tab / 审批弹窗共用）：行号 + +/-/ctx 着色
export function UnifiedDiff({ unified, className = '' }: { unified: string; className?: string }) {
  return (
    <div className={`uni ${className}`}>
      {unified.split('\n').map((ln, i) => {
        const isHunk = ln.startsWith('@@')
        const cls = ln.startsWith('+') && !isHunk ? 'ad' : ln.startsWith('-') && !isHunk ? 'dl' : 'cx'
        return (
          <div className="ln" key={i}>
            <span className="no">{isHunk ? '··' : i + 1}</span>
            <span className={cls}>{ln || ' '}</span>
          </div>
        )
      })}
    </div>
  )
}

// 通用下拉菜单（顶栏权限模式 / Composer effort 共用）
export function ChipMenu({
  label,
  items,
  value,
  onSelect,
  dir = 'up',
  className = '',
}: {
  label: ReactNode
  items: { key: string; label: string; desc?: string }[]
  value: string
  onSelect: (key: string) => void
  dir?: 'up' | 'down'
  className?: string
}) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!open) return
    const close = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('click', close)
    return () => document.removeEventListener('click', close)
  }, [open])

  return (
    <div
      ref={ref}
      className={`model-chip ${className}`}
      role="combobox"
      aria-expanded={open}
      aria-haspopup="listbox"
      onClick={() => setOpen((v) => !v)}
    >
      {label}
      <ChevronDown size={12} />
      {open && (
        <div className={`popup open ${dir === 'down' ? 'down' : ''}`} role="listbox" onClick={(e) => e.stopPropagation()}>
          {items.map((it) => (
            <div
              key={it.key}
              className={`mi ${it.key === value ? 'cur' : ''}`}
              onClick={() => {
                onSelect(it.key)
                setOpen(false)
              }}
            >
              <span className="nm">{it.label}</span>
              {it.desc && <span className="cap">{it.desc}</span>}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
