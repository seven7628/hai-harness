import type { ReactNode } from 'react'

// 空态卡（全仓统一）：图标 + 标题 + 一句说明 + 一个动作。
// 用于列表空、功能未开启、加载失败、市场为空；`className` 可透传历史 class（如 empty）保持兼容。
export default function EmptyState({
  icon,
  title,
  hint,
  actions,
  className = '',
  compact = false,
}: {
  icon?: ReactNode
  title: ReactNode
  hint?: ReactNode
  actions?: ReactNode
  className?: string
  compact?: boolean
}) {
  return (
    <div className={`page-empty ${compact ? 'compact' : ''} ${className}`.trim()} role="status">
      {icon ? <div className="pe-mark">{icon}</div> : null}
      <div className="pe-title">{title}</div>
      {hint ? <div className="pe-hint">{hint}</div> : null}
      {actions ? <div className="pe-actions">{actions}</div> : null}
    </div>
  )
}
