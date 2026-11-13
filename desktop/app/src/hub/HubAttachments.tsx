import { useCallback, useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import { copyText } from '../lib/clipboard'
import { copyImageDataUrl, normalizeAttachments } from './attachments'
import { HubImageLightbox } from './AttachmentStrip'
import type { HubAttachment } from './types'

// 消息气泡里的附件（Hub DM / 群共用）：
//   · 图片最多展示 4 张（再多给 +N），点开灯箱；没有 data_url 的（旧数据/后端精简）
//     显示名字 + 占位，不编造图片；
//   · 文件只显示名字 + 路径，点击复制路径 —— 不做假预览（前端不读文件内容）。
//
// 所有反馈走全局 toast（不 alert）：灯箱里的复制由 HubImageLightbox 负责。

const MAX_THUMBS = 4

export default function HubAttachments({
  attachments,
  onCopyImage,
}: {
  attachments?: HubAttachment[] | null
  /** 覆盖默认的「复制图片」实现（默认 ClipboardItem + toast）。 */
  onCopyImage?: (a: HubAttachment) => Promise<{ ok: boolean; error?: string }>
}) {
  const list = normalizeAttachments(attachments)
  if (list.length === 0) return null
  return <AttachmentsBody list={list} onCopyImage={onCopyImage} />
}

function AttachmentsBody({
  list,
  onCopyImage,
}: {
  list: HubAttachment[]
  onCopyImage?: (a: HubAttachment) => Promise<{ ok: boolean; error?: string }>
}) {
  const showToast = useAppStore((s) => s.showToast)
  const [zoom, setZoom] = useState<number | null>(null)
  const images = list.filter((a) => a.kind === 'image')
  const files = list.filter((a) => a.kind === 'file')
  const shown = images.slice(0, MAX_THUMBS)
  const hidden = images.length - shown.length

  const copyImage = useCallback(async (a: HubAttachment): Promise<{ ok: boolean; error?: string }> => {
    if (onCopyImage) {
      const r = await onCopyImage(a)
      if (!r.ok) showToast(`复制失败：${r.error ?? '未知错误'}`, 'error')
      return r
    }
    const r = a.data_url ? await copyImageDataUrl(a.data_url) : { ok: false, error: '没有图片数据' }
    if (r.ok) showToast('已复制图片', 'ok')
    else showToast(`复制失败：${r.error ?? '未知错误'}`, 'error')
    return r
  }, [onCopyImage, showToast])

  const copyPath = useCallback(async (path: string): Promise<void> => {
    const ok = await copyText(path)
    if (ok) showToast('已复制文件路径', 'ok')
    else showToast('复制路径失败', 'error')
  }, [showToast])

  return (
    <div className="hub-msg-attach">
      {shown.length > 0 ? (
        <div className="hub-msg-imgs" role="list" aria-label={`图片 ${images.length} 张`}>
          {shown.map((a, i) => (
            <button
              type="button"
              role="listitem"
              className="hub-msg-img"
              key={`img-${i}`}
              onClick={() => setZoom(i)}
              title={a.name ? `${a.name}\n点击查看大图` : '点击查看大图'}
              aria-label={`查看大图：图片 ${i + 1}（共 ${images.length} 张）`}
            >
              {a.data_url ? (
                <>
                  <img src={a.data_url} alt={a.name || ''} />
                  {/* 只在 hover/聚焦时浮出来：气泡里不常驻「查看大图」四个字。 */}
                  <span className="hub-msg-img-zoom" aria-hidden="true">查看大图</span>
                </>
              ) : (
                <span className="hub-msg-img-ph">{a.name || '图片'}</span>
              )}
            </button>
          ))}
          {hidden > 0 ? (
            <button
              type="button"
              className="hub-msg-img-more"
              onClick={() => setZoom(shown.length)}
              title={`再看 ${hidden} 张`}
              aria-label={`查看其余 ${hidden} 张图片`}
            >+{hidden}</button>
          ) : null}
        </div>
      ) : null}
      {files.length > 0 ? (
        <div className="hub-msg-files" role="list" aria-label={`文件引用 ${files.length} 个`}>
          {files.map((a, i) => {
            const path = a.path ?? ''
            return (
              <button
                type="button"
                role="listitem"
                className="hub-msg-file"
                key={`file-${path || a.name || i}`}
                onClick={() => { if (path) void copyPath(path) }}
                title={path ? `${path}\n点击复制路径` : (a.name ?? '')}
              >
                <span className="hub-msg-file-name">{a.name || path || '文件'}</span>
                {path ? <span className="hub-msg-file-path">{path}</span> : null}
              </button>
            )
          })}
        </div>
      ) : null}
      {zoom != null && images[zoom] ? (
        <HubImageLightbox
          images={images}
          index={zoom}
          onClose={() => setZoom(null)}
          onIndex={setZoom}
          onCopyImage={copyImage}
          title="图片预览"
        />
      ) : null}
    </div>
  )
}
