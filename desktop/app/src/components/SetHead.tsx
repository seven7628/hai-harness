import type { ReactNode } from 'react'

// 设置页统一页头：标题 14/600 + 可选一行 11.5 说明 + 右侧动作（主操作实底、次操作 ghost）。
// 13 个 tab 原来有 4 套实现（settings-sec-title / 自定义 h2 / hd / 完全没有），这里收敛成一套；
// 各 tab 的旧 class 通过 titleClassName 透传，DOM 结构与测试选择器保持兼容。
export default function SetHead({
  title,
  subtitle,
  icon,
  actions,
  titleClassName = '',
  className = '',
}: {
  title: ReactNode
  subtitle?: ReactNode
  icon?: ReactNode
  actions?: ReactNode
  titleClassName?: string
  className?: string
}) {
  return (
    <div className={`set-head ${className}`.trim()}>
      <div className="set-head-main">
        <div className={`set-head-title ${titleClassName}`.trim()}>
          {icon}
          <span>{title}</span>
        </div>
        {subtitle ? <div className="set-head-sub">{subtitle}</div> : null}
      </div>
      {actions ? <div className="set-head-actions">{actions}</div> : null}
    </div>
  )
}
