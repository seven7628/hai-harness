import type { ReactElement } from 'react'
import { ChevronLeft, ChevronRight } from './icons'

// 设置页（MCP / 钩子）通用骨架：来源分组卡片 + 二级详情头 + 开关/开关行。
//
// 形态（2026-09-21 用户决策，对齐参考截图）：一级 = 分组卡片列表（图标 + 标题 + 副标题 + ›），
// 点卡片进二级详情面板（← 返回 + 标题 + 路径副标题 + 内容），不复用「就地展开」。
// 两个设置页共用同一套骨架，避免各写一套卡片/头样式漂移。

// SourceCard 一张来源分组卡片（用户配置 / Claude 兼容层 / 某个项目）。
export function SourceCard({
  icon,
  title,
  meta,
  badge,
  onClick,
}: {
  icon: ReactElement
  title: string
  meta: string
  badge?: string
  onClick: () => void
}) {
  return (
    <button type="button" className="src-card" onClick={onClick} title={meta}>
      <span className="src-card-ic" aria-hidden>{icon}</span>
      <span className="src-card-main">
        <span className="src-card-title">{title}</span>
        <span className="src-card-meta mono">{meta}</span>
      </span>
      {badge && <span className="src-card-badge">{badge}</span>}
      <span className="src-card-go" aria-hidden>
        <ChevronRight size={14} />
      </span>
    </button>
  )
}

// DetailHead 二级详情面板的头：返回 + 标题 + 副标题（路径/来源）+（可选）右侧动作。
export function DetailHead({
  title,
  sub,
  onBack,
  children,
}: {
  title: string
  sub?: string
  onBack: () => void
  children?: ReactElement | null
}) {
  return (
    <div className="detail-head">
      <button type="button" className="detail-back" onClick={onBack} aria-label="back" title="返回">
        <ChevronLeft size={14} />
      </button>
      <div className="detail-title-box">
        <div className="detail-title">{title}</div>
        {sub && <div className="detail-sub mono" title={sub}>{sub}</div>}
      </div>
      {children && <div className="detail-actions">{children}</div>}
    </div>
  )
}

// Switch 开关（仓库既有 `.switch` 样式：用于 MCP 服务器启停 / hook 启停 / 全局开关）。
export function Switch({
  on,
  onChange,
  label,
  disabled,
}: {
  on: boolean
  onChange: (next: boolean) => void
  label: string
  disabled?: boolean
}) {
  return (
    <button
      type="button"
      className={`switch ${on ? 'on' : ''}`}
      role="switch"
      aria-checked={on}
      aria-label={label}
      title={label}
      disabled={disabled}
      onClick={() => onChange(!on)}
    />
  )
}

// FlagRow 一行「标签 + 说明 + 开关」（全局 hooks 开关块用）。
export function FlagRow({
  label,
  desc,
  on,
  onChange,
  disabled,
}: {
  label: string
  desc?: string
  on: boolean
  onChange: (next: boolean) => void
  disabled?: boolean
}) {
  return (
    <div className="flag-row">
      <div className="flag-text">
        <div className="flag-label">{label}</div>
        {desc && <div className="set-desc">{desc}</div>}
      </div>
      <Switch on={on} onChange={onChange} label={label} disabled={disabled} />
    </div>
  )
}
