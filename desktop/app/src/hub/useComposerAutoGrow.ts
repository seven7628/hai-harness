import { useLayoutEffect, type RefObject } from 'react'

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
 * 换会话/清空草稿时必须收回单行，否则 composer 会保留上一个会话的高度。
 */
export function useComposerAutoGrow(
  ref: RefObject<HTMLTextAreaElement | null>,
  value: string,
): void {
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    // 先归零再量：height:auto 之外的写法会把上一次的 height 算进 scrollHeight。
    el.style.height = 'auto'
    el.style.overflowY = 'hidden'
    const cs = getComputedStyle(el)
    const line = Number.parseFloat(cs.lineHeight) || 22
    const padding = (Number.parseFloat(cs.paddingTop) || 0) + (Number.parseFloat(cs.paddingBottom) || 0)
    const max = Math.round(line * COMPOSER_MAX_LINES + padding)
    const next = Math.min(el.scrollHeight, max)
    el.style.height = `${next}px`
    el.style.overflowY = el.scrollHeight > max ? 'auto' : 'hidden'
  }, [ref, value])
}
