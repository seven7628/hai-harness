// web（内嵌浏览器）面板的 SurfaceProvider —— R06 C2：本轮只搬**元数据**（title/icon），
// render 仍返回 null（渲染继续由 RightPanel 兜底，C3 才换成 provider 渲染）。
//
// 为什么拆两轮（03-code-plan §2.3）：渲染委派才是三条高风险真正出现的地方（原生 overlay 的
// 挂载时机、menuOpen/immersive 决定 embedHide、viewId/pageId 两套身份键空间）；而「注册 + 元数据」
// 出错的失败模式是 tsc/单测秒红。两类失败不该共用一个 commit —— bad-PR 时要能只回退渲染层。
//
// 身份判据不在这里：`isPendingWebTab / webTabKey / webUrlSnapshot` 已下沉 lib/webTab.ts（R06a）。
// 元数据只需要「这个标签此刻对应池里的哪个目标」这一条很窄的读法（见 liveTarget），
// 不要把任何身份判据在这里重写一遍 —— 两套判据分叉时最先坏的是恢复链路。
import type { ReactElement } from 'react'
import { BarChart, FileIcon, Globe, ImageIcon } from '../../components/icons'
import { fileUrlToPath } from '../../lib/filePaths'
import { visualKindOf } from '../../lib/visualFile'
import { useAppStore } from '../../store/useAppStore'
import type { WebTabInstance } from '../../store/useAppStore'
import { registerSurface } from '../registry'
import type { SurfaceProvider } from '../types'

/** 标签标题：file:// → 文件名；否则 title → 主机名 → 原 URL。（自 RightPanel 逐字迁移） */
function tabLabel(url: string, title: string): string {
  if (url.startsWith('file://')) {
    const base = fileUrlToPath(url).split('/').filter(Boolean).pop()
    if (base) return base
  }
  if (title) return title
  try {
    return new URL(url).host || url
  } catch {
    return url
  }
}

/** 标签类型图标（与产出卡/文件栏共用 visualKindOf 这一事实源，避免两处漂移）。（自 RightPanel 逐字迁移） */
function tabIcon(url: string): ReactElement {
  switch (visualKindOf(fileUrlToPath(url) || url)) {
    case 'xlsx': return <BarChart size={12} />
    case 'image': return <ImageIcon size={12} />
    case 'pdf':
    case 'docx':
    case 'pptx': return <FileIcon size={12} />
    case 'browser': return <Globe size={12} />
    default: return /^https?:/.test(url) ? <Globe size={12} /> : <FileIcon size={12} />
  }
}

// 当前页的池目标：活标签靠 viewId 反查 embedNav.targets（**只有池里是新的** —— 页面导航后
// URL/标题先到池、后到标签快照）；待重建的恢复标签 viewId 为空 → 查不到 → 退化用标签自己的
// url 快照，否则重启后那一排标签会丢掉图标与名字。
//
// 为什么在这里 getState() 而不是让 RP 把 embedNav 传进来：SurfaceProvider 的 title/icon 签名
// 固定为 (tab, t) / (tab)（types.ts，C2 不动契约），而签名里没有 store。读 store 是**单向**依赖
// （store 侧不 import 任何 surfaces，见 grep 证据），且 RP 自己就订阅了同一字段
// （useAppStore((s) => s.embedNav)）—— 值一变 RP 必重渲染，本函数随即读到新值，不存在「读到旧值
// 却不重渲染」的窗口。
function liveTarget(tab: WebTabInstance) {
  return useAppStore.getState().embedNav?.targets?.find((x) => x.id === tab.viewId)
}

export const webSurface: SurfaceProvider<WebTabInstance> = {
  kind: 'web',
  namespace: 'builtin',
  group: 'content',
  order: 20,
  // 多实例：同一会话可以同时开多个网页（身份 = 池里的 viewId / pageId，由 store 维护），
  // 与 file 同类，不像 git 那样 singleton。
  singleton: false,
  allowDuplicate: true,
  scope: 'session',
  // heavy：一个 web 标签背后是一个真实的渲染进程（视图池上限 12，见 03 §2.3 不变量 1）。
  weight: 'heavy',
  capabilities: ['read', 'view'],
  title: (tab, t) => {
    const tp = liveTarget(tab)
    return tabLabel(tp?.url ?? tab.url ?? '', tp?.title ?? '') || t('rp.tab.web')
  },
  icon: (tab) => tabIcon(liveTarget(tab)?.url ?? tab.url ?? ''),
  // 打开时的默认实例：无池身份的 web 标签，形状与 store.makeTab('web') 在「认不出当前页」时
  // 给的一致。契约字段，目前还没有运行时消费者（与 git/file 的同名字段一样）。
  defaultSpec: () => ({ kind: 'web' }),
  // C2 只注册元数据：DOM 仍由 RightPanel 按 kind 分发（本轮零渲染改动）。
  // 返回 null 而不是省略 render —— registry 的 assertProvider 要求 render 是函数。
  render: () => null,
}

registerSurface(webSurface)
