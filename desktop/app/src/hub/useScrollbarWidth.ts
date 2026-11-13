import { useCallback, useEffect, useLayoutEffect, useRef, type RefObject } from 'react'

/** composer 右内边距读的补偿量：占位滚动条宽度（overlay 0 / classic 10·15）。 */
export const HUB_SCROLLBAR_VAR = '--hub-scrollbar-w'

/**
 * 实际占位滚动条宽度。不猜平台：`offsetWidth - clientWidth` 就是滚动条从内容盒
 * 里吃掉的那一条 —— overlay 条（macOS 默认）为 0，classic 条（Windows/Linux、
 * 强制常显的 macOS、自定义 10px webkit 条）为 10/15。
 */
export function measureScrollbarWidth(el: Element | null): number {
  if (!(el instanceof HTMLElement)) return 0
  const w = el.offsetWidth - el.clientWidth
  return w > 0 ? Math.round(w) : 0
}

/**
 * 把滚动容器（`.messages`）的占位滚动条宽度写到会话视图根节点的
 * `--hub-scrollbar-w` 上，供 `.hub-demo.main > .composer-wrap` 的 padding-right 补偿。
 *
 * 为什么需要补偿：`.messages` 是滚动容器，classic 条会从右侧吃掉 S px 内容盒，
 * `.messages-inner` 的 `margin:0 auto` 于是整体左移 S/2；composer 外层没有滚动条，
 * 两个 760 列的中心线就差 S/2。composer 不能自己也变成滚动容器（里面的
 * MentionPicker/灯箱/附件面板是绝对定位，会被 overflow 裁掉），只能补内边距。
 *
 * ResizeObserver 观察的是内容盒：滚动条出现/消失会改变内容盒宽度，回调随即重写
 * 变量。切换会话时滚动的元素可能被换掉（空态↔对话、群 id 重挂载），所以每次提交
 * 都比对一次元素身份，换了就重新测量并重挂观察者；卸载时清掉变量与观察者，
 * 避免把上一个视图的 S 漏给下一个。
 */
export function useScrollbarWidth(
  scrollRef: RefObject<HTMLElement | null>,
  rootRef: RefObject<HTMLElement | null>,
): void {
  const observed = useRef<HTMLElement | null>(null)
  const observer = useRef<ResizeObserver | null>(null)

  const apply = useCallback((): void => {
    const root = rootRef.current
    if (!root) return
    const w = measureScrollbarWidth(scrollRef.current)
    // 0 = overlay 条：删掉内联值回落到 CSS 的 0px 默认，布局与补偿前完全一致。
    if (w > 0) root.style.setProperty(HUB_SCROLLBAR_VAR, `${w}px`)
    else root.style.removeProperty(HUB_SCROLLBAR_VAR)
  }, [rootRef, scrollRef])

  useLayoutEffect(() => {
    const el = scrollRef.current
    if (el === observed.current) return
    observed.current = el
    observer.current?.disconnect()
    observer.current = null
    apply()
    if (el) {
      const ro = new ResizeObserver(apply)
      ro.observe(el)
      observer.current = ro
    }
  })

  useEffect(() => () => {
    observer.current?.disconnect()
    observer.current = null
    observed.current = null
    rootRef.current?.style.removeProperty(HUB_SCROLLBAR_VAR)
  }, [rootRef])
}
