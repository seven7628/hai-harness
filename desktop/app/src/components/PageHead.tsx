import type { ReactNode } from 'react'
import { Back } from './icons'

// 管理页统一页头（定时任务三页 + 技能页共用）：
// 返回（ghost）｜ 标题 15 serif + 副标题 11 ｜ 右侧计数/刷新/主操作。
// 旧 class（cron-head / sp-head 等）通过 className 透传保留，行为与测试选择器不受影响。
export default function PageHead({
  backLabel,
  onBack,
  title,
  icon,
  subtitle,
  right,
  className = '',
}: {
  backLabel?: string
  onBack?: () => void
  title: ReactNode
  icon?: ReactNode
  subtitle?: ReactNode
  right?: ReactNode
  className?: string
}) {
  return (
    <header className={`page-head ${className}`.trim()}>
      {onBack && (
        <button className="page-head-back cron-head-back" onClick={onBack}>
          <Back size={13} /> {backLabel}
        </button>
      )}
      <div className="page-head-title cron-head-title">
        <h1>
          {icon}
          {title}
        </h1>
        {subtitle ? <span className="page-head-sub cron-head-sub">{subtitle}</span> : null}
      </div>
      <span className="page-head-grow cron-head-grow" />
      <div className="page-head-right cron-head-right">{right}</div>
    </header>
  )
}
