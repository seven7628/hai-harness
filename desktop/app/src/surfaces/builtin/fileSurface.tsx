// 文件面板的 SurfaceProvider（PR-04：多实例身份；PR-05b：多一个 edit 分发；R08a：多一个 preview 分发）
// 同一个文件允许 diff / source / edit / preview 四个标签并存 —— 身份 = (path, mode)，见 store 的 tabKeyOf。
// edit 不是新 kind：它是 file 面板的第三种渲染（受控编辑器），语义与「预览」并列。
// preview 也不是新 kind（决策 D-I）：文档/媒体是 file{mode:'preview'} 内部的渲染器，
// 由 surfaces/preview 的注册表按扩展名选（R08a：text / image / csv）。
import { FileIcon } from '../../components/icons'
import { FilePreviewTab } from '../../components/FilePreview'
import { Editor } from '../../components/Editor'
import { PreviewPanel } from '../preview/PreviewPanel'
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
  // 按 tab.mode 分发：
  //   edit    → 编辑器（PR-05b）
  //   preview → 渲染器注册表（R08a；缺渲染器/依赖时给 unavailable 占位，不白屏）
  //   其余（diff / source / 缺省）→ 原有预览组件，一条都没改 —— 旧路径保留到 R08b 再收口。
  render: ({ tab }) => (tab.kind !== 'file'
    ? null
    : tab.mode === 'edit'
      ? <Editor tab={tab} />
      : tab.mode === 'preview'
        ? <PreviewPanel path={tab.path} tabId={tab.id} />
        : <FilePreviewTab tab={tab} />),
}

registerSurface(fileSurface)
