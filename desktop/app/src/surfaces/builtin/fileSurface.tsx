// 文件面板的 SurfaceProvider（PR-04：多实例身份；PR-05b：多一个 edit 分发；R08a：多一个 preview 分发）
// 同一个文件允许 diff / source / edit / preview 四个标签并存 —— 身份 = (path, mode)，见 store 的 tabKeyOf。
// edit 不是新 kind：它是 file 面板的第三种渲染（受控编辑器），语义与「预览」并列。
// preview 也不是新 kind（决策 D-I）：文档/媒体是 file{mode:'preview'} 内部的渲染器，
// 由 surfaces/preview 的注册表按扩展名选（R08a：text / image / csv）。
import { FileIcon } from '../../components/icons'
import { FilePreviewTab } from '../../components/FilePreview'
import { Editor } from '../../components/Editor'
import { PreviewPanel } from '../preview/PreviewPanel'
import { focusSpec } from '../../store/useAppStore'
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
  // 打开时的默认实例（PR-11 起 makeTab 的初值来源，形状与 store 那一版逐字段一致）：
  // payload = fileFocus（用户/Agent 最后一次定位的文件）+ openSeq=1（「新标签从默认视图开始」
  // 的触发器，阶段 2 起跟着标签走）。实现**只有一份** —— focusSpec 仍在 store，这里调它，
  // 不在这里重写「有 toolId → diff、否则 source」那套条件。
  defaultSpec: (s) => ({ ...focusSpec(s), openSeq: 1 }),
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
