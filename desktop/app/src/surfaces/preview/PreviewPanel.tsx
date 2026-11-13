// `file` 标签的 mode='preview' 面板：内容读取 + 渲染器宿主（R08a / PR-08 第一半）
//
// 与 FilePreviewTab 的分工（fileSurface 按 tab.mode 分发）：
//   · source  —— 文件活动列表 + 最终文件（既有组件，一行没改）
//   · edit    —— 内嵌编辑器（PR-05b，一行没改）
//   · preview —— **本组件**：只有一个目的，把这份文件按渲染器注册表显示出来
//
// 为什么不复用 FilePreviewTab：那个面板的一半是「文件活动 + diff 定位」，preview 模式不需要
// 也不该显示它（否则同一个文件的两个标签长得一样，用户分不清）。反过来，本组件也不去动
// FilePreviewTab 的任何一条分支 —— 这正是 R08a「旧路径保留」的口径。
//
// 内容来源与既有预览**完全同一条**：store.filePreviews（file_preview 命令的响应，按
// 归一化路径缓存，缺了经 ensureFilePreview 补拉）。不新开命令、不改 file_preview 语义（F33-F36）。
import { useEffect, useMemo } from 'react'
import { fileKeyOf, useAppStore } from '../../store/useAppStore'
import { useT } from '../../i18n'
import { displayPath } from '../../lib/localAsset'
import { PreviewHost } from './PreviewHost'
import { fileRefOf } from './registry'

export function PreviewPanel({ path, tabId }: { path?: string; tabId?: string }) {
  const t = useT()
  const workspace = useAppStore((s) => s.workspace)
  const filePreviews = useAppStore((s) => s.filePreviews)
  const ensureFilePreview = useAppStore((s) => s.ensureFilePreview)
  const filePreview = useMemo(
    () => (path ? filePreviews[fileKeyOf(path, workspace)] : undefined),
    [filePreviews, path, workspace],
  )
  // 缓存缺失 → 补拉（切工作区清空 / 被上限淘汰 / 重启恢复出来的标签都会走到）
  useEffect(() => {
    if (path && !filePreview) ensureFilePreview(path)
  }, [path, filePreview, ensureFilePreview])
  // 身份与 size 都取决于内容是否已到；都做成原始依赖，避免每帧一个新对象撑爆下游 effect。
  const size = filePreview?.content.length ?? 0
  const binary = filePreview?.binary
  const file = useMemo(() => fileRefOf(path ?? '', size, binary), [path, size, binary])

  // 根节点与 FilePreviewTab 同一个约定（.file-activity-panel）：`.right-body.file-mode` 是
  // flex 行，直接把 .file-source 塞进去会被按内容宽度收缩（实测：代码面只占 350px，右侧
  // 一片空），而 `flex: 1` 那条既有规则只挂在 .file-activity-panel 上。
  return (
    <div className="file-activity-panel">
      {!path
        ? <div className="file-activity-empty">{t('rp.preview.noPath')}</div>
        : (
          <div className="file-source" data-preview-mode="preview">
            <div className="file-source-meta mono">
              {displayPath(path, workspace)}
              {filePreview && !filePreview.binary
                ? ` · ${t('rp.file.lines').replace('{n}', String(filePreview.lines))}${filePreview.truncated ? t('rp.file.truncated') : ''}`
                : ''}
            </div>
            {filePreview
              ? <PreviewHost file={file} tabId={tabId} content={filePreview.content} />
              : <div className="file-activity-empty">{t('rp.file.reading')}</div>}
          </div>
        )}
    </div>
  )
}
