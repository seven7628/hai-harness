import { useEffect } from 'react'
import { useT } from '../i18n'

// 通用确认弹窗（居中，`.modal-hint` 既有模式；删除等不可逆操作用 danger 按钮）。
// 点背景 / Esc = 取消；onConfirm 由调用方执行并关弹窗。
export default function ConfirmModal({
  title,
  message,
  confirmLabel,
  cancelLabel,
  danger,
  onConfirm,
  onCancel,
}: {
  title: string
  message: string
  confirmLabel: string
  cancelLabel: string
  danger?: boolean
  onConfirm: () => void
  onCancel: () => void
}) {
  const t = useT()

  useEffect(() => {
    if (!onCancel) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onCancel()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onCancel])

  return (
    <div className="modal-hint open" role="dialog" aria-modal="true" aria-label={title} onClick={onCancel}>
      <div className="card confirm-card" onClick={(e) => e.stopPropagation()}>
        <h3>
          <span style={{ color: 'var(--crimson)' }}>✕</span> {title}
        </h3>
        <p className="confirm-desc">{message}</p>
        <div className="acts">
          <button className="btn" onClick={onCancel}>
            {cancelLabel}
          </button>
          <button className={`btn primary ${danger ? 'danger' : ''}`} onClick={onConfirm}>
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>
  )
}
