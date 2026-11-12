// 文件面板的 SurfaceProvider（PR-04：多实例身份；PR-05b：多一个 edit 分发）
// 同一个文件允许 diff / source / edit 三个标签并存 —— 身份 = (path, mode)，见 store 的 tabKeyOf。
// edit 不是新 kind：它是 file 面板的第三种渲染（受控编辑器），语义与「预览」并列。
import { FileIcon } from '../../components/icons'
import { FilePreviewTab } from '../../components/FilePreview'
import { Editor } from '../../components/Editor'
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
  // 按 tab.mode 分发：edit → 编辑器；其余（diff/source/缺省）走原有预览组件，
  // 一条都没改 —— F32 的替换只发生在 mode='edit' 这一支。
  render: ({ tab }) => (tab.kind !== 'file'
    ? null
    : tab.mode === 'edit' ? <Editor tab={tab} /> : <FilePreviewTab tab={tab} />),
}

registerSurface(fileSurface)
