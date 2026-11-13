// 头部「更多操作」菜单的行为（P6-16）：开关 + 外点关闭 + Esc 关闭 + 键盘可达。
//
// 为什么抽成 hook：群聊（GroupView）与智能体单聊（HubSurface）各有一份同名菜单，
// 行为必须一致 —— 两份手写监听迟早出现「一个能 Esc 关、另一个不能」。
//
// 层级不在这里管：菜单的绘制顺序由 hub.css 的 `.chat-header` 层决定
// （header 的 backdrop-filter 已经创建 stacking context，菜单的 z-index 只在
// 这个上下文内有效，所以必须把 header 整体抬到消息区之上）。
import { useCallback, useEffect, useRef, useState } from 'react'

const MENU_ITEM = '[role="menuitem"],[role="menuitemradio"]'

export interface HeaderMenu {
  open: boolean
  setOpen: (next: boolean) => void
  toggle: () => void
  close: () => void
  // React 18 的 ref 类型：RefObject<T> 的 current 已经是 T | null，
  // 写成 RefObject<T | null> 反而不能直接挂到 ref 属性上。
  triggerRef: React.RefObject<HTMLButtonElement>
  menuRef: React.RefObject<HTMLDivElement>
}

export function useHeaderMenu(): HeaderMenu {
  const [open, setOpen] = useState(false)
  const triggerRef = useRef<HTMLButtonElement>(null)
  const menuRef = useRef<HTMLDivElement>(null)

  const close = useCallback(() => setOpen(false), [])
  const toggle = useCallback(() => setOpen((v) => !v), [])

  // 外点关闭：pointerdown 捕获阶段（早于 click），点在菜单/触发按钮之外就关。
  // 触发按钮算「内部」—— 否则点它时先被这里关掉、再被 click 打开，菜单永远关不上。
  useEffect(() => {
    if (!open) return
    const onDown = (e: PointerEvent): void => {
      const target = e.target
      if (!(target instanceof Node)) return
      if (menuRef.current?.contains(target) || triggerRef.current?.contains(target)) return
      setOpen(false)
    }
    document.addEventListener('pointerdown', onDown, true)
    return () => document.removeEventListener('pointerdown', onDown, true)
  }, [open])

  // Esc 关闭并把焦点还给触发按钮；方向键/Home/End 在菜单项之间移动（菜单模式的标准键盘行为）。
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent): void => {
      const menu = menuRef.current
      if (!menu) return
      if (e.key === 'Escape') {
        e.stopPropagation()
        setOpen(false)
        triggerRef.current?.focus()
        return
      }
      if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp' && e.key !== 'Home' && e.key !== 'End') return
      const items = Array.from(menu.querySelectorAll<HTMLElement>(MENU_ITEM)).filter((el) => !el.hasAttribute('disabled'))
      if (items.length === 0) return
      const current = items.indexOf(document.activeElement as HTMLElement)
      let next = 0
      if (e.key === 'End') next = items.length - 1
      else if (e.key === 'ArrowDown') next = current < 0 ? 0 : (current + 1) % items.length
      else if (e.key === 'ArrowUp') next = current < 0 ? items.length - 1 : (current - 1 + items.length) % items.length
      e.preventDefault()
      items[next]?.focus()
    }
    document.addEventListener('keydown', onKey, true)
    return () => document.removeEventListener('keydown', onKey, true)
  }, [open])

  // 打开后焦点进第一项：键盘用户不必先 Tab 过整个头部。
  useEffect(() => {
    if (!open) return
    const first = menuRef.current?.querySelector<HTMLElement>(MENU_ITEM)
    first?.focus()
  }, [open])

  // 焦点离开菜单（Tab 出去 / 点到别处）就关：避免菜单留在屏幕上但焦点已经不在里面。
  const onFocusOut = useCallback((e: React.FocusEvent<HTMLDivElement>) => {
    const next = e.relatedTarget
    if (next instanceof Node && (menuRef.current?.contains(next) || triggerRef.current?.contains(next))) return
    setOpen(false)
  }, [])

  useEffect(() => {
    const menu = menuRef.current
    if (!open || !menu) return
    menu.addEventListener('focusout', onFocusOut as unknown as EventListener)
    return () => menu.removeEventListener('focusout', onFocusOut as unknown as EventListener)
  }, [open, onFocusOut])

  return { open, setOpen, toggle, close, triggerRef, menuRef }
}
