// image 渲染器：<img> + 失败占位（R08a 的三选之一）
//
// 为什么走 lib/localAsset 而不是自己拼 file://：渲染进程不给 <img> 开磁盘权限
// （页面来自 vite dev server / 打包后的 file://，跨源一律拒）。既有通道是主进程的
// fs:read-image → data URL，路径越界校验只做一份（见 electron/read-image.ts）。
// 这条通道此前只服务 Markdown 内嵌图片；图片文件本身在右栏一直是「用内嵌浏览器打开」，
// 现在多了这条**应用内**渲染路。
//
// 失败必须说清楚三件事（用户只会看到一张破图）：读不到 / 不是图片 / 具体路径。
import { useEffect, useState } from 'react'
import { useAppStore } from '../../../store/useAppStore'
import { useT } from '../../../i18n'
import { displayPath, useLocalAsset } from '../../../lib/localAsset'
import type { PreviewProps } from '../types'

function baseName(p: string): string {
  return p.split('/').filter(Boolean).pop() ?? p
}

export default function ImagePreview({ file }: PreviewProps) {
  const t = useT()
  const workspace = useAppStore((s) => s.workspace)
  // 锚点是**工作区**而不是文件所在目录：这里的 file.path 就是要打开的那张图本身
  //（baseDirFor 是给 Markdown 里「相对于 md 文件的 src」用的，套进来会多叠一层目录 ——
  // 实测 'out/chart.png' 会变成 <ws>/out/out/chart.png）。绝对路径不受锚点影响。
  const { abs, url, error } = useLocalAsset(file.path, workspace, workspace)
  // data URL 也会解码失败（主进程按扩展名给 mime，不嗅探文件头）—— 单独记，避免把
  // 「读到了但画不出来」说成「读不到」。
  const [broken, setBroken] = useState(false)
  useEffect(() => { setBroken(false) }, [url])
  const shown = displayPath(abs ?? file.path, workspace)

  if (error || broken) {
    return (
      <div className="preview-image-empty" data-state="failed" role="status">
        <div className="preview-image-title">⚠ {t('rp.preview.imageFailed')}</div>
        <div className="preview-image-reason">{error ?? t('rp.preview.imageBroken')}</div>
        <div className="preview-image-path mono">{shown}</div>
      </div>
    )
  }
  if (!url) {
    return (
      <div className="preview-image-empty" data-state="loading" role="status">
        <div className="preview-image-title">{t('rp.preview.imageLoading')}</div>
        <div className="preview-image-path mono">{shown}</div>
      </div>
    )
  }
  return (
    <div className="preview-image">
      <img src={url} alt={baseName(file.path)} onError={() => setBroken(true)} />
      <div className="preview-image-path mono">{shown}</div>
    </div>
  )
}
