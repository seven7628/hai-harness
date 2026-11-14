// pdf 预览面板的「原生视图驱动」（R08b）。
//
// 为什么 pdf 必须走这条路（不是 DOM 里塞 <embed>）：Chromium 的 PDF viewer 只在**原生
// WebContentsView** 里可用 —— 主窗口没开 plugins，dev 模式的 renderer 又是 http://localhost，
// file:// 的 iframe/embed 一律被拦。仓库里已有、也是唯一被 e2e 钉住过的 PDF 通道就是内嵌视图
// （见 e2e/verify_pdf_preview.mjs 的 T4：CDP 读到 contentType=application/pdf + 全尺寸 <embed>）。
// 于是「pdf 渲染器」的实质是**把那个视图摆到本面板的矩形上**，不是自己画一页 PDF。
//
// 三条不变量（都对着既有实现抄，别在这里发明）：
//  ① 去重身份 = 源文件绝对路径（与 store.openInBrowserTab 同口径）：同一个 pdf 已在视图池里
//    → 复用并激活；不在 → 新建。**跨会话不命中**（main 侧去重按 sid），所以别的会话开着的
//    同一份 pdf 不会把这边的面板顶成别人的页面。
//  ② 本面板**创建**的视图，卸载时关掉；复用的是别人的（比如同一文件的 web 标签）→ 只隐藏，
//    绝不关 —— 关掉等于替用户关掉他自己开的那个标签。
//  ③ 量不到矩形就什么都不下发（见 lib/embedRect.ts 的 80×80 事故）。
import { fileUrlOf } from './localAsset'
import type { EmbedRect } from './embedRect'
import { bridge, type DesktopApi } from './bridge'

/** 一个池里目标的形状（只取这里用得到的字段，口径见 electron/embed.ts EmbedTargetInfo）。 */
export interface EmbedTargetLike {
  id?: string
  url?: string
  sid?: string
  srcPath?: string
}

// 视图所有权（引用计数）。
//
// 为什么不是「谁建谁关」的布尔：同一个视图会被**两次挂载**同时持有 —— React 严格模式的双挂
// （mount → 卸载 → 再 mount）与「切走又切回」都是这个形状。实测症状：第一次挂载建出 t2，
// 它的 cleanup 把 t2 关掉，而第二次挂载刚复用/正用着 t2 → 面板 ready 但池里只剩自举视图
// （pdf 一片空白，探针读到的 srcPath 是空串）。
// 故所有权按视图 id 计数：持有数归零才 hide，且只有**本模块建的**才 close（复用的别人的
// 视图——比如同一文件的 web 标签——绝不关）。
const owners = new Map<string, number>()
const createdByPanel = new Set<string>()
// 关闭的宽限期：卸载与重新挂载之间**没有事务边界**（React 严格模式双挂、切标签再切回都会
// 「先 cleanup 后 effect」）。实测症状：cleanup 的 close 与下一次 attach 的 open 撞在一起，
// open 复用到的正是那条正在被关掉的视图 → 面板 ready、池里却只剩自举视图（pdf 一片空白）。
// 故最后一次释放只**挂起**关闭，600ms 内若又被持有就取消 —— 真关闭（关标签）只晚 600ms。
const pendingClose = new Map<string, ReturnType<typeof setTimeout>>()
const CLOSE_GRACE_MS = 600

export interface PreviewViewHandle {
  id: string
  /** true = 这条视图是**本面板**建的（卸载时关掉）；false = 复用了已有的（只隐藏）。 */
  created: boolean
  url: string
}

export type AttachResult = { ok: true; handle: PreviewViewHandle } | { ok: false; error: string }

// 桥是模块私有常量（见 src/lib/bridge.ts）：不再每次去 window 上捞，宿主没有桥就是 null。
const desktop = (): DesktopApi | null => bridge

/**
 * 建/复用视图并激活它（**不**负责显示，显示由 showPreviewView 带矩形完成）。
 *
 * 失败一律返回 { ok:false, error }：调用方（pdf 渲染器）把它变成 onUnavailable 的理由 ——
 * 上层要的是「缺什么」，不是异常栈。
 */
/** 登记一次持有（attach 的两条成功路径共用；失败路径不登记）。 */
function retain(handle: PreviewViewHandle): PreviewViewHandle {
  const pending = pendingClose.get(handle.id)
  if (pending) { clearTimeout(pending); pendingClose.delete(handle.id) }
  owners.set(handle.id, (owners.get(handle.id) ?? 0) + 1)
  return handle
}

export async function attachPreviewView(opts: {
  /** 文件路径：绝对路径原样，相对路径按 workspace 锚定（与 openInBrowserTab 同口径）。 */
  path: string
  workspace: string
  sid: string
}): Promise<AttachResult> {
  const d = desktop()
  if (!d?.embedOpenTarget || !d.embedShow) {
    return { ok: false, error: '内嵌浏览器视图不可用（这个环境没有 embedOpenTarget/embedShow）' }
  }
  const abs = opts.path.startsWith('/')
    ? opts.path
    : opts.workspace ? `${opts.workspace.replace(/\/$/, '')}/${opts.path}` : ''
  if (!abs) return { ok: false, error: `路径无法绝对化：${opts.path}（workspace 为空）` }
  const url = fileUrlOf(abs)
  // 先取池快照：新建后靠差集认出「这条是刚建的」，不依赖 main 把 id 放在哪个字段。
  const before = await d.embedStatus().catch(() => undefined) as { targets?: EmbedTargetLike[] } | undefined
  const beforeIds = new Set((before?.targets ?? []).map((t) => t.id).filter(Boolean) as string[])
  const sameSrc = (t: EmbedTargetLike) => t.srcPath === abs
  const sameUrl = (t: EmbedTargetLike) => t.url === url
  const hit = [...(before?.targets ?? [])].find(sameSrc) ?? [...(before?.targets ?? [])].find(sameUrl)

  if (hit?.id) {
    // 命中 → 激活。跨会话激活会被 main 静默拒绝（返回原状态），所以必须看结果：
    // 没切过去等同于「点了没反应」，那就老实新建一条本会话的视图。
    const r = await d.embedActivate(hit.id)
    const active = (r as { state?: { activeId?: string | null } } | undefined)?.state?.activeId
    if (r?.ok && active === hit.id) {
      return { ok: true, handle: retain({ id: hit.id, created: createdByPanel.has(hit.id), url }) }
    }
  }
  const r = await d.embedOpenTarget(url, { sid: opts.sid, activate: true, srcPath: abs })
  if (!r?.ok) return { ok: false, error: r?.error || '创建内嵌视图失败（主进程没有返回原因）' }
  const state = (r as { state?: { targets?: EmbedTargetLike[]; activeId?: string | null } }).state
  const fresh = (state?.targets ?? []).find((t) => t.id && !beforeIds.has(t.id) && (sameSrc(t) || sameUrl(t)))
    ?? (state?.targets ?? []).find((t) => t.id && !beforeIds.has(t.id))
  const id = fresh?.id ?? state?.activeId ?? ''
  if (!id) return { ok: false, error: '内嵌视图建好了但拿不到 id（无法摆到面板上）' }
  if (!beforeIds.has(id)) createdByPanel.add(id)
  return { ok: true, handle: retain({ id, created: createdByPanel.has(id), url }) }
}

/** 把视图显示在给定矩形上（激活 + 给 bounds）。rect 缺失 → 不下发（见文件头 ③）。 */
export async function showPreviewView(handle: PreviewViewHandle, rect: EmbedRect | undefined): Promise<void> {
  const d = desktop()
  if (!d || !rect) return
  await d.embedActivate(handle.id).catch(() => undefined)
  await d.embedShow({ rect }).catch(() => undefined)
}

/** 只改 bounds（面板尺寸/位置变了）。 */
export async function resizePreviewView(rect: EmbedRect | undefined): Promise<void> {
  if (!rect) return
  await desktop()?.embedResize?.(rect).catch(() => undefined)
}

/**
 * 卸载时的收尾：持有数归零才隐藏；**本模块建的**且归零才关闭（见文件头的所有权注释）。
 *
 * 先 hide 再 close：hide 是「这条原生视图不该再浮在页面上」的兜底（close 在同一个 tick 里
 * 也走 applyPanelVisibility，但主进程若已销毁该条目 close 是空操作，hide 仍能保证不残留）。
 */
export async function releasePreviewView(handle: PreviewViewHandle): Promise<void> {
  const d = desktop()
  if (!d) return
  const left = (owners.get(handle.id) ?? 1) - 1
  if (left > 0) {
    // 还有另一个持有者（双挂的另一半 / 刚挂上的新面板）正在用它 → 什么都不做
    owners.set(handle.id, left)
    return
  }
  owners.delete(handle.id)
  await d.embedHide?.().catch(() => undefined)
  if (!createdByPanel.has(handle.id)) return // 复用的是别人的视图（如 web 标签）→ 绝不关
  const prev = pendingClose.get(handle.id)
  if (prev) clearTimeout(prev)
  pendingClose.set(handle.id, setTimeout(() => {
    pendingClose.delete(handle.id)
    // 宽限期内又被持有（重新挂载）→ 这条视图还在用，别关
    if ((owners.get(handle.id) ?? 0) > 0) return
    createdByPanel.delete(handle.id)
    void desktop()?.embedCloseTarget?.(handle.id).catch(() => undefined)
  }, CLOSE_GRACE_MS))
}
