import { useEffect, useState } from 'react'
import { useAppStore } from '../store/useAppStore'

// 全局 Toast（轻提示：保存成功/失败等）。
// 渲染在 App 根部覆盖层；store.showToast 弹新 Toast（覆盖旧），2.5s 自动消失。
// 样式对齐纸墨双主题（--surface 卡片 + --success/--error 语义色）。
export default function ToastView() {
  const toast = useAppStore((s) => s.toast)
  const [visible, setVisible] = useState(false)
  const [leaving, setLeaving] = useState(false)

  // toast.id 变化 → 进入可见 + 自动消失计时；旧 Toast 被覆盖时重新计时
  useEffect(() => {
    if (!toast) return
    setLeaving(false)
    setVisible(true)
    const t = window.setTimeout(() => setLeaving(true), 2200)
    const t2 = window.setTimeout(() => setVisible(false), 2600)
    return () => {
      window.clearTimeout(t)
      window.clearTimeout(t2)
    }
  }, [toast?.id]) // eslint-disable-line react-hooks/exhaustive-deps

  const dismiss = () => {
    setLeaving(true)
    window.setTimeout(() => setVisible(false), 200)
  }

  if (!toast || !visible) return null
  return (
    <div
      className={`toast-view ${leaving ? 'leaving' : ''} ${toast.kind === 'error' ? 'error' : ''}`}
      role="status"
      onClick={dismiss}
    >
      <span className={`toast-dot ${toast.kind === 'error' ? 'error' : ''}`} />
      <span className="toast-msg">{toast.msg}</span>
    </div>
  )
}