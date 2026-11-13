import { useEffect, useId, useLayoutEffect, useRef, useState, type CSSProperties, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { Markdown } from '../components/markdown'
import { useT } from '../i18n'
import './CollapsibleMarkdown.css'

// Hub 正文的「超长限高 + 查看完整详情」壳。
//
// 为什么是壳而不是改 Markdown：Markdown 是全局组件（叙述区、文件预览、产物弹窗
// 都在用），「消息太长要收起」只是 Hub 消息区的产品口径，不能下沉成全局语义。
// 这里只包一层，正文仍是同一个 <Markdown>，html 围栏照旧走 HtmlPreviewBlock。
//
// 三个刻意的取舍：
//   · 折叠判定只看**真实布局高度**（ResizeObserver 观察未裁剪的内层），不按字符数
//     猜：一段 3 行的表格/代码块可能比 40 行短句还高，图片和 iframe 还是晚到的高度。
//   · streaming 时不折叠：流式每来一个 token 高度就变，折叠/展开会跟着抽搐。
//   · 折叠用 mask 渐隐而不是「渐变遮罩块」：气泡底色是 --hub-user-bubble /
//     --user-bubble，纸底是 --bg —— 遮罩块要么选错色，要么每个调用点再覆盖一次变量。

/** 折叠阈值（px）：正文实际高度**高于**它才折叠（419/420 不折，421 折）。
 *  max-height 也取这个值（内联，单一来源），CSS 里不再写死数字。 */
export const COLLAPSE_HEIGHT = 420

/** 对话框内可 Tab 到的元素（iframe = visual-ui 的 html 预览，也要进环）。 */
const FOCUSABLE = [
  'a[href]',
  'button:not([disabled])',
  'input:not([disabled])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  'iframe',
  '[tabindex]:not([tabindex="-1"])',
].join(',')

function focusablesIn(root: HTMLElement): HTMLElement[] {
  // offsetParent 过滤掉 display:none 的（例如 html 预览切到「源码」后的 iframe 壳）。
  return Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE)).filter((el) => el.offsetParent !== null)
}

export default function CollapsibleMarkdown({
  text,
  streaming = false,
  className = '',
  ariaLabel,
  renderMention,
}: {
  text: string
  /** 流式增长中：不折叠（高度每帧都在变），也不测量。 */
  streaming?: boolean
  className?: string
  /** 触发按钮的无障碍名字（通常是作者名）：一屏有多条折叠消息时能分清是哪一条。 */
  ariaLabel?: string
  /** 群消息的 @ 徽章渲染器，原样透传给 Markdown（DM 不传）。 */
  renderMention?: (id: string, children: ReactNode) => ReactNode
}) {
  const t = useT()
  const innerRef = useRef<HTMLDivElement | null>(null)
  const triggerRef = useRef<HTMLButtonElement | null>(null)
  const dialogRef = useRef<HTMLDivElement | null>(null)
  const closeRef = useRef<HTMLButtonElement | null>(null)
  const [overflows, setOverflows] = useState(false)
  const [open, setOpen] = useState(false)
  const titleId = useId()
  const empty = text.trim().length === 0

  // 量「未裁剪的内层」：外层折叠后高度被钉在阈值上，内层仍是真实内容高度，
  // 所以图片晚加载、字体晚就位、iframe 探针上报都还能把折叠态修正过来。
  useLayoutEffect(() => {
    if (streaming) return
    const el = innerRef.current
    if (!el) return
    const measure = () => setOverflows(el.getBoundingClientRect().height > COLLAPSE_HEIGHT)
    measure()
    if (typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    return () => ro.disconnect()
  }, [text, streaming])

  // 打开：锁背景滚动 + 聚焦关闭按钮 + Esc 关闭 + Tab 环不外逃；关闭：焦点回触发按钮。
  useEffect(() => {
    if (!open) return
    const root = document.documentElement
    const prevOverflow = root.style.overflow
    // 关闭时要回到的那个按钮：在 effect 体里取一次（cleanup 里读 ref 会拿到已变化的节点）。
    const returnTo = triggerRef.current
    root.style.overflow = 'hidden'
    closeRef.current?.focus()
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.preventDefault()
        e.stopPropagation()
        setOpen(false)
        return
      }
      if (e.key !== 'Tab') return
      const dialog = dialogRef.current
      if (!dialog) return
      const items = focusablesIn(dialog)
      if (items.length === 0) {
        e.preventDefault()
        dialog.focus()
        return
      }
      const first = items[0]
      const last = items[items.length - 1]
      const active = document.activeElement
      const inside = active instanceof Node && dialog.contains(active)
      if (e.shiftKey && (!inside || active === first)) {
        e.preventDefault()
        last.focus()
      } else if (!e.shiftKey && (!inside || active === last)) {
        e.preventDefault()
        first.focus()
      }
    }
    // Tab 环的第二道闸：正文里有 html 预览的 iframe，焦点进了子文档后 Tab 就不再经过
    // 本文档的 keydown（浏览器直接跳到 iframe 之后的下一个节点）。所以再加一条 focusin：
    // 焦点一旦落到对话框外就拉回关闭按钮 —— 键盘用户不会「掉」到背景里。
    const onFocusIn = (e: FocusEvent) => {
      const dialog = dialogRef.current
      if (!dialog) return
      if (e.target instanceof Node && dialog.contains(e.target)) return
      closeRef.current?.focus()
    }
    // capture：Hub 自己也有 window 级 keydown（Esc 关面板等），先吃这一层。
    document.addEventListener('keydown', onKey, true)
    document.addEventListener('focusin', onFocusIn, true)
    return () => {
      root.style.overflow = prevOverflow
      document.removeEventListener('keydown', onKey, true)
      document.removeEventListener('focusin', onFocusIn, true)
      // 先把监听摘干净再回焦，否则这条 focusin 会把自己拉回关闭按钮。
      if (returnTo && document.documentElement.contains(returnTo)) returnTo.focus()
    }
  }, [open])

  // 空正文不渲染壳：只有附件的消息不该多出一块空白。
  if (empty) return null

  const collapsed = !streaming && overflows

  return (
    <div className={`cm${className ? ` ${className}` : ''}${collapsed ? ' cm-collapsed' : ''}`}>
      <div
        className={`cm-clip${collapsed ? ' is-clipped' : ''}`}
        style={collapsed ? ({ maxHeight: COLLAPSE_HEIGHT } as CSSProperties) : undefined}
      >
        <div className="cm-inner" ref={innerRef}>
          {/* streaming 必须透传：它决定 Markdown 的分片解析路径、缓存 key，以及
              html 围栏渲染 iframe 还是「正在生成」骨架。吞掉它 = 流式正文按整段
              重解析 + 半截 HTML 直接挂 iframe（见 markdown.tsx 的 HtmlPreviewBlock 注释）。 */}
          <Markdown text={text} streaming={streaming} renderMention={renderMention} />
        </div>
      </div>
      {collapsed ? (
        <div className="cm-more">
          <button
            ref={triggerRef}
            type="button"
            className="cm-more-btn"
            aria-haspopup="dialog"
            aria-label={ariaLabel ? t('hub.collapse.moreFor').replace('{name}', ariaLabel) : undefined}
            onClick={() => setOpen(true)}
          >
            {t('hub.collapse.more')}
          </button>
        </div>
      ) : null}
      {open
        ? createPortal(
            <div
              className="cm-scrim"
              role="presentation"
              onClick={(e) => {
                if (e.target === e.currentTarget) setOpen(false)
              }}
            >
              <div
                ref={dialogRef}
                className="cm-dialog"
                role="dialog"
                aria-modal="true"
                aria-labelledby={titleId}
                tabIndex={-1}
                onClick={(e) => e.stopPropagation()}
              >
                <header className="cm-dialog-head">
                  <h2 id={titleId} className="cm-dialog-title">{t('hub.collapse.title')}</h2>
                  {ariaLabel ? <span className="cm-dialog-sub">{ariaLabel}</span> : null}
                  <button
                    ref={closeRef}
                    type="button"
                    className="cm-dialog-close"
                    aria-label={t('hub.close')}
                    title={t('hub.close')}
                    onClick={() => setOpen(false)}
                  >
                    <span aria-hidden="true">✕</span>
                  </button>
                </header>
                <div className="cm-dialog-body">
                  {/* 弹窗**不传** streaming：能点开「完整内容」说明这条消息已经落定，
                      这里要的是整段渲染（也不会出现流式骨架/分片）。 */}
                  <Markdown text={text} renderMention={renderMention} />
                </div>
              </div>
            </div>,
            document.body,
          )
        : null}
    </div>
  )
}
