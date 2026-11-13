import { useCallback, useEffect, useRef, useState } from 'react'
import { copyImageDataUrl } from './attachments'
import { fmtBytes } from '../lib/imageAttach'
import type { HubAttachment } from './types'

// composer 上方的附件条带（图片缩略图 + 文件引用 chip）+ 图片灯箱。
//
// 与主会话 Composer 的关系：交互同源（点缩略图开灯箱、× 移除、复制），但 Hub 的
// 图片只以 data URL 存在于消息里，所以灯箱里的「复制图片」是 ClipboardItem 直写；
// 失败**不 alert**，在灯箱底部给一行可读的状态。

/**
 * 灯箱里可聚焦的控件（按 DOM 顺序 = Tab 顺序）。
 *
 * 只认真正能落焦点的元素：首尾张上那个 disabled 的「上一张/下一张」不进循环，
 * 隐藏的（`display:none` / 零尺寸）也不行，否则 Tab 会停在一个看不见的按钮上。
 */
function focusableIn(root: HTMLElement): HTMLElement[] {
  const sel = 'button, [href], input, select, textarea, [tabindex]'
  return Array.from(root.querySelectorAll<HTMLElement>(sel)).filter((el) => {
    if (el.hasAttribute('disabled') || el.getAttribute('aria-hidden') === 'true') return false
    if (el.tabIndex < 0) return false
    return el.offsetWidth > 0 || el.offsetHeight > 0 || el.getClientRects().length > 0
  })
}

/**
 * 灯箱：Esc / 点遮罩关闭；复制失败写在 figcaption 里（不弹 alert）。
 *
 * aria-modal=true 只是给读屏的声明，浏览器照样会把 Tab 放回背景 —— 打开后必须
 * 自己把 Tab / Shift+Tab 圈在对话框内（首尾相接），否则键盘用户会掉进底下的页面。
 */
export function HubImageLightbox({
  images,
  index,
  onClose,
  onIndex,
  onRemove,
  onCopyImage,
  title,
}: {
  images: HubAttachment[]
  index: number
  onClose: () => void
  /** 有多张时提供：上一张/下一张 + ←/→ 键。缺省 = 不显示导航（单图场景）。 */
  onIndex?: (index: number) => void
  onRemove?: (index: number) => void
  onCopyImage?: (a: HubAttachment) => Promise<{ ok: boolean; error?: string }> | { ok: boolean; error?: string }
  title?: string
}) {
  const current = images[index]
  const [status, setStatus] = useState('')
  const rootRef = useRef<HTMLDivElement>(null)
  const closeRef = useRef<HTMLButtonElement>(null)
  const restoreTo = useRef<Element | null>(null)
  // onClose 在调用方常是内联箭头（每次渲染新引用）：放进 ref，焦点恢复 effect 才
  // 能在挂载时跑一次 —— 依赖它会让「复制」等重渲染把焦点拽回关闭按钮。
  const onCloseRef = useRef(onClose)
  onCloseRef.current = onClose

  useEffect(() => {
    restoreTo.current = typeof document !== 'undefined' ? document.activeElement : null
    closeRef.current?.focus()
    const onKey = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') {
        e.stopPropagation()
        onCloseRef.current()
        return
      }
      if (e.key !== 'Tab') return
      // Tab 陷阱：在灯箱的首尾之间循环，绝不落到背景的发送框/侧栏上。
      const root = rootRef.current
      if (!root) return
      const items = focusableIn(root)
      if (items.length === 0) {
        e.preventDefault()
        return
      }
      const first = items[0]
      const last = items[items.length - 1]
      const activeEl = document.activeElement as HTMLElement | null
      const inside = !!activeEl && root.contains(activeEl)
      if (!inside) {
        e.preventDefault()
        const target = e.shiftKey ? last : first
        target.focus()
        return
      }
      if (e.shiftKey && activeEl === first) {
        e.preventDefault()
        last.focus()
      } else if (!e.shiftKey && activeEl === last) {
        e.preventDefault()
        first.focus()
      }
    }
    document.addEventListener('keydown', onKey, true)
    return () => {
      document.removeEventListener('keydown', onKey, true)
      // 关闭后焦点回到触发元素（缩略图 / +N / 删除后回到 composer 原位置）。
      const back = restoreTo.current
      if (back instanceof HTMLElement && document.contains(back)) back.focus()
    }
  }, [])

  // 方向键单独一个监听：切张时不能重跑上面的焦点恢复 effect（否则每按一次
  // 方向键焦点都被拉回关闭按钮）。
  useEffect(() => {
    if (!onIndex || images.length <= 1) return
    const onKey = (e: KeyboardEvent): void => {
      if (e.key === 'ArrowLeft' && index > 0) {
        e.preventDefault()
        onIndex(index - 1)
      } else if (e.key === 'ArrowRight' && index < images.length - 1) {
        e.preventDefault()
        onIndex(index + 1)
      }
    }
    document.addEventListener('keydown', onKey, true)
    return () => document.removeEventListener('keydown', onKey, true)
  }, [onIndex, index, images.length])

  const copy = useCallback(async (): Promise<void> => {
    if (!current?.data_url) {
      setStatus('这张图没有可复制的内容')
      return
    }
    const r = onCopyImage ? await onCopyImage(current) : await copyImageDataUrl(current.data_url)
    setStatus(r.ok ? '已复制图片' : `复制失败：${r.error ?? '未知错误'}`)
  }, [current, onCopyImage])

  if (!current) return null
  const name = current.name || `图片 ${index + 1}`
  return (
    <div
      className="hub-attach-lb"
      role="dialog"
      aria-modal="true"
      aria-label={title || '图片预览'}
      ref={rootRef}
      onClick={onClose}
    >
      <figure onClick={(e) => e.stopPropagation()}>
        <div className="hub-attach-lb-actions">
          {onIndex && images.length > 1 ? (
            <>
              <button
                type="button"
                className="hub-attach-lb-btn nav"
                disabled={index <= 0}
                onClick={() => onIndex(index - 1)}
                aria-label="上一张"
                title="上一张（←）"
              >‹</button>
              <button
                type="button"
                className="hub-attach-lb-btn nav"
                disabled={index >= images.length - 1}
                onClick={() => onIndex(index + 1)}
                aria-label="下一张"
                title="下一张（→）"
              >›</button>
            </>
          ) : null}
          <button type="button" className="hub-attach-lb-btn" onClick={() => void copy()}>复制图片</button>
          {onRemove ? (
            <button
              type="button"
              className="hub-attach-lb-btn danger"
              onClick={() => {
                onRemove(index)
                onClose()
              }}
            >
              删除
            </button>
          ) : null}
          <button
            ref={closeRef}
            type="button"
            className="hub-attach-lb-btn close"
            onClick={onClose}
            aria-label="关闭预览"
            title="关闭（Esc）"
          >
            ✕
          </button>
        </div>
        {current.data_url ? (
          <img src={current.data_url} alt={name} />
        ) : (
          <div className="hub-attach-lb-placeholder">{name}</div>
        )}
        <figcaption>
          {name} · {index + 1}/{images.length}
          {status ? <span className="hub-attach-lb-status" role="status"> · {status}</span> : null}
        </figcaption>
      </figure>
    </div>
  )
}

/** composer 上方的待发附件条带：图片缩略图（可放大）+ 文件引用（名 + 大小 + ×）。 */
export default function AttachmentStrip({
  images,
  files,
  notice,
  busy,
  onRemove,
}: {
  images: HubAttachment[]
  files: HubAttachment[]
  notice?: string
  busy?: boolean
  onRemove: (kind: 'image' | 'file', index: number) => void
}) {
  const [zoom, setZoom] = useState<number | null>(null)
  const closeZoom = useCallback(() => setZoom(null), [])
  if (images.length === 0 && files.length === 0 && !notice && !busy) return null
  return (
    <>
      {(images.length > 0 || files.length > 0 || busy) ? (
        <div className="hub-attach-strip" role="list" aria-label="待发送的附件">
          {images.map((a, i) => (
            <div className="hub-attach-chip hub-attach-chip--img" role="listitem" key={`img-${i}`}>
              <button
                type="button"
                className="hub-attach-thumb"
                onClick={() => setZoom(i)}
                title="查看大图"
                aria-label={`查看图片 ${i + 1}（共 ${images.length} 张）`}
              >
                {a.data_url ? <img src={a.data_url} alt="" /> : <span className="hub-attach-ph">图</span>}
              </button>
              <button
                type="button"
                className="hub-attach-x"
                onClick={() => onRemove('image', i)}
                title="移除图片"
                aria-label={`移除图片 ${i + 1}`}
              >
                ×
              </button>
            </div>
          ))}
          {files.map((a, i) => (
            <div
              className="hub-attach-chip hub-attach-chip--file"
              role="listitem"
              key={`file-${a.path ?? a.name ?? i}`}
              title={a.path ? `${a.path}\n发送时会作为 @引用 写进正文` : (a.name ?? '')}
            >
              <span className="hub-attach-file-name">{a.name || a.path || '文件'}</span>
              <span className="hub-attach-file-size">{typeof a.bytes === 'number' ? fmtBytes(a.bytes) : '—'}</span>
              <button
                type="button"
                className="hub-attach-x"
                onClick={() => onRemove('file', i)}
                title="移除引用"
                aria-label={`移除文件引用 ${a.name || a.path || i + 1}`}
              >
                ×
              </button>
            </div>
          ))}
          {busy ? <span className="hub-attach-busy" role="status">压缩图片中…</span> : null}
        </div>
      ) : null}
      {notice ? <div className="hub-attach-notice" role="status" aria-live="polite">{notice}</div> : null}
      {zoom != null && images[zoom] ? (
        <HubImageLightbox
          images={images}
          index={zoom}
          onClose={closeZoom}
          onIndex={setZoom}
          onRemove={(i) => onRemove('image', i)}
          title="待发送的图片"
        />
      ) : null}
    </>
  )
}
