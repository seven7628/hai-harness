// 原生视图的「覆盖层闸门」：原生 WebContentsView 恒合成在窗口 DOM 之上（原生层级 > DOM，
// CSS z-index 压不住），所以只要 DOM 里有东西压在右栏上，就必须先把视图隐藏 —— 否则用户看到的
// 是被画面挡掉的菜单/弹窗（2026-09-20「点＋菜单被预览画面盖掉」、2026-10「大图躲在预览后面」）。
//
// 为什么提成模块（R08b）：pdf 预览面板现在也驱动同一个原生视图（lib/previewEmbed.ts），
// 它同样需要这道闸门。整段逻辑（名单 + elementsFromPoint 兜底 + 节流观察）抄第二份的代价是
// 可预期的漂移 —— 新加一个弹窗只补一处，另一处就静默被盖住，而那种缺陷只有人眼/截图能发现。
//
// 两个判定，缺一不可：
//   ① `useUiModalOpen()`：store 里手维护的弹窗名单（覆盖层还没进 DOM 的时序兜底）；
//   ② `useDomOverlayOpen(hostRef, enabled)`：问 DOM 本身（名单之外的覆盖层全靠它）。
import { useEffect, useState } from 'react'
import type { RefObject } from 'react'
import { useAppStore } from '../store/useAppStore'
import { centerOf, overlayCoversRect, rectsIntersect } from './overlayHit'

/**
 * 覆盖层选择器名单。
 *
 * 前五个是**全屏**弹窗（.modal-hint / .settings-overlay / 图片放大 / 两个灯箱）：只要在文档里
 * 就隐藏；后三个是**光标处浮层**（右键菜单 / 选择浮层 / 设置浮层）：只有真的压在右栏矩形上
 * 才隐藏 —— 在中栏弹出的右键菜单不该让右栏黑一下。
 */
export const OVERLAY_SELECTOR =
  '.modal-hint.open, .settings-overlay, .img-zoom, .rt-lb, .cy-lb, .ctx-menu, .sel-popup, .settings-pop'

/** 弹窗打开中（store 侧名单；与 WebTab 历史行为逐字一致）。 */
export function useUiModalOpen(): boolean {
  const inSettings = useAppStore((s) => s.inSettings)
  const inSkills = useAppStore((s) => s.inSkills)
  const inCron = useAppStore((s) => s.inCron)
  const showNewWsModal = useAppStore((s) => s.showNewWsModal)
  const askQuestion = useAppStore((s) => s.questions.length > 0)
  const askRelease = useAppStore((s) => s.release)
  return inSettings || inSkills || inCron || showNewWsModal || askQuestion || !!askRelease
}

/**
 * DOM 里是否有覆盖层压在宿主容器上（原 WebTab 的 domOverlayOpen，逐字平移）。
 *
 * `enabled=false` 时不观察（浏览器/截图模式用不上原生视图）。节流 120ms（**不是防抖**）：
 * 聊天流式输出时 DOM 每秒变化几十次，防抖会让定时器永远被重置、检测永不触发。
 */
export function useDomOverlayOpen(hostRef: RefObject<Element | null>, enabled: boolean): boolean {
  const [open, setOpen] = useState(false)
  useEffect(() => {
    if (!enabled) return
    // 名单 + 再问一句 DOM：宿主矩形**中点**上最顶层的元素是不是还在宿主里（判据与测试
    // 见 lib/overlayHit.ts）。原生视图不参与 DOM 命中测试，所以「中点被别的元素占住」
    // 只可能是 DOM 覆盖层压在它上面 —— 包括以后新加、名字还没进名单的那些。
    const compute = () => {
      const host = hostRef.current
      const hostRect = host?.getBoundingClientRect()
      const pt = centerOf(hostRect ?? undefined, { width: window.innerWidth, height: window.innerHeight })
      let hit = !!host && overlayCoversRect(
        hostRect ?? undefined,
        pt ? document.elementsFromPoint(pt.x, pt.y) : [],
        {
          contains: (el) => host.contains(el as Node),
          positionOf: (el) => getComputedStyle(el as Element).position,
          viewport: { width: window.innerWidth, height: window.innerHeight },
        },
      )
      for (const el of document.querySelectorAll(OVERLAY_SELECTOR)) {
        const r = el.getBoundingClientRect()
        if (r.width < 1 || r.height < 1) continue
        if (rectsIntersect(hostRect ?? undefined, r)) { hit = true; break }
      }
      // 同值不 set（MutationObserver 每次 class 变动都会走到这里，避免无谓重渲染）
      setOpen((prev) => (prev === hit ? prev : hit))
    }
    compute()
    let timer = 0
    const mo = new MutationObserver(() => {
      if (!timer) timer = window.setTimeout(() => { timer = 0; compute() }, 120)
    })
    mo.observe(document.body, { subtree: true, attributes: true, attributeFilter: ['class', 'style'], childList: true })
    return () => { mo.disconnect(); if (timer) window.clearTimeout(timer) }
    // hostRef 的 ref 身份稳定（useRef），不放进依赖
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabled])
  return open
}
