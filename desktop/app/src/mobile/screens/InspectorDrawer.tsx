// 手机详情抽屉（P8-01b）：右栏（Context / 任务 / Needs / 会话 / 概览）在手机上以右侧
// 抽屉打开 —— **不隐藏能力**，只是换布局。
//
// 内容是桌面同一个 HubInspector：页签、任务板、Needs、Context、会话信息全部复用。
// 这一层只负责抽屉的壳：遮罩、Esc 关闭、背景滚动锁定、开合动画（reduced-motion 降级）。
//
// 开合状态走 host/panels 的 rightExpanded：HubSurface/GroupView 的「上下文」按钮、
// HubInspector 自己的「收起右栏」都作用在同一个值上，与桌面语义一致。
import { useEffect, useRef } from 'react'
import HubInspector from '../../hub/HubInspector'
import { useHostPanels } from '../../host/panels'

export default function InspectorDrawer() {
  const { rightExpanded, setRightExpanded } = useHostPanels()
  const panelRef = useRef<HTMLElement>(null)

  // Esc 先关抽屉（捕获阶段 + stopPropagation：不让它继续触发「返回列表」）。
  useEffect(() => {
    if (!rightExpanded) return
    const onKey = (e: KeyboardEvent): void => {
      if (e.key !== 'Escape') return
      e.stopPropagation()
      setRightExpanded(false)
    }
    document.addEventListener('keydown', onKey, true)
    return () => document.removeEventListener('keydown', onKey, true)
  }, [rightExpanded, setRightExpanded])

  // 打开期间锁背景滚动（与 GroupEditor/灯箱同一套做法）：抽屉里的任务板/Context
  // 自己滚，背景列表不该跟着动。
  useEffect(() => {
    if (!rightExpanded) return
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      document.body.style.overflow = prev
    }
  }, [rightExpanded])

  // 打开后焦点进抽屉：键盘/读屏用户不必先 Tab 过整页。
  useEffect(() => {
    if (!rightExpanded) return
    panelRef.current?.focus()
  }, [rightExpanded])

  // 页签条的溢出状态（P8-03a）：5 个页签在 390 下本来刚好排满，badge/字体一宽就
  // 截断。把「左/右还有没有内容」量出来写到 data-overflow，CSS 只在真的溢出时
  // 挂边缘渐隐；滚动时实时更新，滚到尽头渐隐自动消失。
  useEffect(() => {
    if (!rightExpanded) return
    const nav = panelRef.current?.querySelector<HTMLElement>('.hub-inspector-tabs')
    if (!nav) return
    const update = (): void => {
      const max = nav.scrollWidth - nav.clientWidth
      const start = max > 1 && nav.scrollLeft > 1
      const end = max > 1 && nav.scrollLeft < max - 1
      nav.dataset.overflow = !start && !end ? 'none' : start && end ? 'both' : start ? 'start' : 'end'
    }
    update()
    nav.addEventListener('scroll', update, { passive: true })
    // 抽屉宽度随视口/字体变化（横竖屏、系统字号）：重开不一定发生，得跟着量。
    const ro = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(update)
    ro?.observe(nav)
    return () => {
      nav.removeEventListener('scroll', update)
      ro?.disconnect()
    }
  }, [rightExpanded])

  if (!rightExpanded) return null

  return (
    <div className="m-drawer-root" data-testid="mobile-inspector-drawer">
      <div className="m-drawer-scrim" onClick={() => setRightExpanded(false)} aria-hidden="true" />
      <aside
        className="m-drawer"
        role="dialog"
        aria-modal="true"
        aria-label="Hub 详情"
        tabIndex={-1}
        ref={panelRef}
      >
        <HubInspector />
      </aside>
    </div>
  )
}
