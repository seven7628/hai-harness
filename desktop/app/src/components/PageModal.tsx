import { useEffect } from 'react'
import type { ReactNode } from 'react'

// 页面弹窗：与设置弹窗同一套遮罩/卡片语言（scrim + centered card）。
// header / body / footer 三段，body 独立滚动；Esc 关闭（输入框里也关，避免困住用户）。
// 旧 class（skill-detail / confirm-card）通过 cardClassName 透传，行为不变。
export default function PageModal({
  title,
  subtitle,
  icon,
  children,
  footer,
  onClose,
  width,
  cardClassName = '',
  closeLabel = '关闭',
}: {
  title: ReactNode
  subtitle?: ReactNode
  icon?: ReactNode
  children: ReactNode
  footer?: ReactNode
  onClose: () => void
  width?: number
  cardClassName?: string
  closeLabel?: string
}) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [onClose])

  return (
    <div className="page-modal-scrim scrim" onMouseDown={onClose}>
      <div
        className={`page-modal ${cardClassName}`.trim()}
        style={width ? ({ ['--pm-w' as string]: `${width}px` } as React.CSSProperties) : undefined}
        role="dialog"
        aria-modal="true"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <header className="page-modal-hd">
          <div className="page-modal-hd-main">
            <div className="page-modal-title">
              {icon}
              <span>{title}</span>
            </div>
            {subtitle ? <div className="page-modal-sub">{subtitle}</div> : null}
          </div>
          <button className="page-modal-x" onClick={onClose} aria-label={closeLabel} title={closeLabel}>
            ✕
          </button>
        </header>
        <div className="page-modal-bd">{children}</div>
        {footer ? <footer className="page-modal-ft">{footer}</footer> : null}
      </div>
    </div>
  )
}
