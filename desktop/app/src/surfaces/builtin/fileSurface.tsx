// 文件预览的 SurfaceProvider（PR-04：多实例身份）
// 同一个文件允许 diff / source 两个标签并存 —— 身份 = (path, mode)，见 store 的 tabKeyOf。
import { FileIcon } from '../../components/icons'
import { FilePreviewTab } from '../../components/FilePreview'
import { registerSurface } from '../registry'
import type { SurfaceProvider } from '../types'

export const fileSurface: SurfaceProvider = {
  kind: 'file',
  namespace: 'builtin',
  group: 'content',
  order: 10,
  singleton: false,
  allowDuplicate: true,
  scope: 'session',
  weight: 'light',
  capabilities: ['read', 'view', 'write'],
  title: (_tab, t) => t('rp.tab.file'),
  icon: () => <FileIcon size={12} />,
  defaultSpec: () => ({ kind: 'file' }),
  render: ({ tab }) => (tab.kind === 'file' ? <FilePreviewTab tab={tab} /> : null),
}

registerSurface(fileSurface)
