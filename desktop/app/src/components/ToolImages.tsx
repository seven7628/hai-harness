import { memo, useState } from 'react'
import { useT } from '../i18n'

// ToolImages 工具结果图片（read_file 读图 / computer_screenshot / browser 截图 /
// MCP image）：与用户附件同样用 data URL 直显。
//
// 这些图**同时**送进了模型上下文（core.ToolResult.Blocks → tool 消息的 image 块），
// UI 展示的意义是「用户能看到模型看到了什么」——核对模型判断依据、确认截图是否
// 真的拍到了目标窗口；没有这层展示，视觉类工具对用户就是个黑盒。
//
// 点击放大：原图 data URL 直接铺满 overlay（不重新编码，zoom 看细节时像素保真）。
export const ToolImages = memo(function ToolImages({ images, label }: { images: { content: string }[]; label: string }) {
  const t = useT()
  const [zoom, setZoom] = useState<string | null>(null)
  if (!images.length) return null
  return (
    <>
      <div className="tool-imgs" role="group" aria-label={label}>
        {images.map((c, i) => (
          <img
            key={i}
            src={c.content}
            alt={label}
            title={label}
            onClick={() => setZoom(c.content)}
          />
        ))}
      </div>
      {zoom && (
        <div
          className="img-zoom"
          role="dialog"
          aria-modal="true"
          aria-label={t('img.zoomClose')}
          onClick={() => setZoom(null)}
        >
          <img src={zoom} alt={label} />
        </div>
      )}
    </>
  )
})
