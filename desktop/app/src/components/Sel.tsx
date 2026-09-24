import { useEffect, useRef, useState } from 'react'
import { ChevronDown } from './icons'

// SelOption 下拉选项。
export interface SelOption {
  value: string
  label: string
  hint?: string // 次要信息（右侧灰色小字）
  disabled?: boolean
}

// Sel 通用自绘下拉（替代原生 <select>）：
// macOS 原生 select 弹层是系统渲染（白底系统菜单），CSS 无法主题化；
// 自绘弹层与应用 popup/seg 同风格（--elevated 底、hover、选中 ✓），
// 闭合态与 prov-input 同体系（同高/同边框/同圆角/chevron）。
// 用法与原生 select 一致：value / options / onChange / placeholder。
export default function Sel({
  value,
  options,
  onChange,
  placeholder,
  disabled,
  className,
  style,
}: {
  value: string
  options: SelOption[]
  onChange: (v: string) => void
  placeholder?: string
  disabled?: boolean
  className?: string
  style?: React.CSSProperties
}) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)

  // 外部点击关闭
  useEffect(() => {
    if (!open) return
    const onDoc = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onDoc)
    return () => document.removeEventListener('mousedown', onDoc)
  }, [open])

  // Esc 关闭
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false)
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [open])

  const cur = options.find((o) => o.value === value)
  return (
    <div className={`sel ${className ?? ''}`} ref={ref} style={style}>
      <button
        type="button"
        className={`sel-btn ${open ? 'open' : ''}`}
        onClick={() => !disabled && setOpen((v) => !v)}
        disabled={disabled}
        aria-haspopup="listbox"
        aria-expanded={open}
      >
        <span className="sel-label">{cur ? cur.label : (placeholder ?? '')}</span>
        <ChevronDown size={12} className="sel-chev" />
      </button>
      {open && (
        <div className="sel-popup" role="listbox">
          {options.map((o) => (
            <button
              key={o.value}
              type="button"
              role="option"
              aria-selected={o.value === value}
              className={`sel-opt ${o.value === value ? 'cur' : ''}`}
              disabled={o.disabled}
              onClick={() => {
                onChange(o.value)
                setOpen(false)
              }}
            >
              <span className="sel-opt-label">{o.label}</span>
              {o.hint && <span className="sel-opt-hint">{o.hint}</span>}
            </button>
          ))}
          {options.length === 0 && <div className="sel-empty">—</div>}
        </div>
      )}
    </div>
  )
}
