// 文件面板的 SurfaceProvider（PR-04：多实例身份；PR-05b：多一个 edit 分发；R08a：多一个 preview 分发）
// 同一个文件允许 diff / source / edit / preview 四个标签并存 —— 身份 = (path, mode)，见 store 的 tabKeyOf。
// edit 不是新 kind：它是 file 面板的第三种渲染（受控编辑器），语义与「预览」并列。
// preview 也不是新 kind（决策 D-I）：文档/媒体是 file{mode:'preview'} 内部的渲染器，
// 由 surfaces/preview 的注册表按扩展名选（R08a：text / image / csv）。
import { EyeIcon, FileIcon } from '../../components/icons'
import { FilePreviewTab } from '../../components/FilePreview'
import { Editor } from '../../components/Editor'
import { PreviewPanel } from '../preview/PreviewPanel'
import { focusSpec } from '../../store/useAppStore'
import { shortName } from '../../lib/shortPath'
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
  // 标签标题（R11c：自 RightPanel 的本地 basename 分支**整段迁入**，输出逐字不变）。
  //
  // 为什么必须在 provider 里：diff / source / edit / preview 是**同一个 kind 的四种形态**，
  // 「哪几种形态要在标题上加词」是 file 面板自己的口径。R11c 之前它住在 RP 里 —— 于是 RP
  // 同时认识 kind 与 kind 的 mode，provider 只剩一个「文件」二字（标签标题因此从来没反映过
  // 文件名，只有 RP 知道）。四种形态的差异在这里表达，RP 只问 `title(tab, t)`。
  title: (tab, t) => {
    // 非 file 的 tab 不该走到这里（调用点按 kind 取 provider）；给面板名而不是抛错。
    if (tab.kind !== 'file') return t('rp.tab.file')
    const base = (tab.path ?? '').split('/').filter(Boolean).pop()
    // 长文件名中间截断（保留 `_test.go` 这类尾巴）：标签最宽 168px，从尾巴截断时
    // `conversation_router.go` 与 `conversation_router_test.go` 会长得一模一样。
    // 完整路径在 title 里（见 RP 渲染点的 title 属性 —— 那里读 tab.path，不读本函数）。
    const name = (base && shortName(base)) || t('rp.tab.file')
    // mode 后缀（R08b / R08a 偏差 5）：同一个文件可以同时有 preview 与 source 两个标签
    //（tabKeyOf 的幂等键带 mode），而标题此前只写文件名 —— 两个标签一模一样，用户分不清
    // 点哪个。只给**非缺省**的那一侧加后缀：source 是缺省（也是绝大多数标签），给它加词
    // 等于把全应用的标签都改宽一寸，而「预览」这种新入口才是需要被认出来的那个。
    // diff 也有同样的问题，但那是 PR-06 之前就有的口径，本轮不动（不在验收范围内）。
    return tab.mode === 'preview' ? `${name} · ${t('rp.tab.previewTag')}` : name
  },
  // 原生宿主（R11c）：**只认运行时标记**，不认自己的 mode。
  // 为什么不是 `tab.mode === 'preview'`：mode 说明「可能渲 pdf」，真正把原生视图挂上去的是
  // 预览面板（PreviewPanel 写 store.previewEmbedActive），而父级 effect（本闸门）晚于子级
  // effect 跑 —— 按 mode 判会在面板挂载前就说「我是宿主」，闸门随即不 hide，症状是 pdf
  // 预览一片空白、视图却在池里（R08b 实测）。同理也不能按 mode 收窄：面板卸载与标记清空
  // 之间有一帧，判定必须跟着**标记**走，否则那一帧里我们既不当宿主、视图也还在。
  nativeHost: (_tab, s) => s.previewEmbedActive,
  // 图标（PR-11：RightPanel 的本地 kind→图标 switch 删了，这里是唯一真相）：预览标签用**眼睛**，
  // 其余用文件。为什么 preview 要单独一个字形（R08b）：标签最窄时标题里的「· 预览」会被
  // shortName 截掉，图标是唯一还看得见的区分（同一个文件的 source 标签仍是文件图标）。
  // 入参可省（入参 Undefined 时给文件图标）：＋ 菜单/空态能力面板没有实例。
  icon: (tab) => (tab?.kind === 'file' && tab.mode === 'preview' ? <EyeIcon size={12} /> : <FileIcon size={12} />),
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
