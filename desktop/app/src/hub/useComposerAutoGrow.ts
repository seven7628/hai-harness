import { useLayoutEffect, useRef, type RefObject } from 'react'

/** Composer 最多长到 4 行；再长就内部滚动，不能把消息区挤没。 */
export const COMPOSER_MAX_LINES = 4

/**
 * Hub composer 的单行居中 + 4 行自动增高。
 *
 * 高度由 scrollHeight 真值决定（不靠 rows 猜，中文/换行/缩放都准）：
 *   - 空/单行 → 34px（CSS 的 padding 6 + line-height 22 正好让光标和
 *     placeholder 在输入框垂直中线，左侧 28px 按钮再各补 3px 外边距）；
 *   - 2..4 行 → 按实际内容长高；
 *   - 超过 4 行 → 钉在 4 行高度，overflow-y:auto 内部滚动。
 *
 * 重挂载（Chat↔Run、对话↔派任务）和窗口缩放都不会改变 ref/value 的身份，
 * 所以不能只依赖 [ref, value]：effect 每次 render 都检查 textarea 是否换了
 * 元素（重挂载），换元素时重建 ResizeObserver 并立即补量一次；同一个元素
 * 的宽度变化（窗口缩放、右栏折叠）由 ResizeObserver 重算。
 *
 * 换会话/清空草稿时必须收回单行，否则 composer 会保留上一个会话的高度。
 */
export function useComposerAutoGrow(
  ref: RefObject<HTMLTextAreaElement | null>,
  value: string,
): void {
  const observer = useRef<ResizeObserver | null>(null)
  const observedEl = useRef<HTMLTextAreaElement | null>(null)
  // 上一次测量后的宽度：写 height 本身也会触发 ResizeObserver，
  // 宽度没变就不重算，否则会和浏览器互相激出循环。
  const lastWidth = useRef(0)
  const lastValue = useRef<string | null>(null)

  useLayoutEffect(() => {
    const el = ref.current
    if (!el) {
      // Run 视图 / 派任务模式下 chat textarea 不在文档里：别继续观察已卸载的元素。
      observer.current?.disconnect()
      observer.current = null
      observedEl.current = null
      lastValue.current = null
      return
    }

    // 先归零再量：height:auto 之外的写法会把上一次的 height 算进 scrollHeight。
    const recalc = (): void => {
      el.style.height = 'auto'
      // 不要写死 hidden：量完超过 4 行时要能滚，最坏也至少是 auto。
      el.style.overflowY = 'auto'
      const cs = getComputedStyle(el)
      const line = Number.parseFloat(cs.lineHeight) || 22
      const padding = (Number.parseFloat(cs.paddingTop) || 0) + (Number.parseFloat(cs.paddingBottom) || 0)
      // 边框不计入 scrollHeight，但计入 border-box 的 height。
      const border = (Number.parseFloat(cs.borderTopWidth) || 0) + (Number.parseFloat(cs.borderBottomWidth) || 0)
      const max = Math.round(line * COMPOSER_MAX_LINES + padding)
      const next = Math.min(el.scrollHeight, max) + border
      el.style.height = `${next}px`
      // 量完再记宽度：滚动条出现/消失会改变 clientWidth，这里读到的是稳定值。
      lastWidth.current = el.clientWidth
    }

    if (observedEl.current !== el) {
      // 重挂载：新 textarea 还没有量过，旧 observer 盯的是已经离开文档的元素。
      observer.current?.disconnect()
      observedEl.current = el
      lastValue.current = null
      lastWidth.current = 0
      const ro = new ResizeObserver(() => {
        // 首次 observe 的回调也会走到这里（宽度从 0 变成真实值），
        // 正好补上重挂载后的第一次测量。
        if (el.clientWidth === lastWidth.current) return
        recalc()
      })
      ro.observe(el)
      observer.current = ro
    }

    if (lastValue.current !== value) recalc()
    lastValue.current = value
  })

  // 上面的 effect 每次 render 都会跑，不能在那里返回 cleanup；
  // observer 只在组件真正卸载（或换成另一个元素）时断开。
  useLayoutEffect(() => () => {
    observer.current?.disconnect()
    observer.current = null
    observedEl.current = null
  }, [])
}
