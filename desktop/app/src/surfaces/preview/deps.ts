// 预览依赖探测：把「这台机器上到底有没有转换服务 / 内嵌浏览器视图」变成注册表认识的状态。
//
// 为什么单列一个模块（R08b）：`requires` 的名册在 R08a 就立好了，但状态一直是常量「不可用」。
// 真值来自**运行环境**，而环境判定有两个消费方 —— 渲染器的命中（registry.missingDependencyReason
// 决定 Host 走 unavailable）与用户看到的理由文案。分散到渲染器里各写一遍，就会出现
// 「docx 说自己缺 Python、pptx 说自己缺通道」这种同一根因两种说法。
//
// 判据取**能力面**而不是「有没有 window.desktop」：
//   · conversion → transport 上真有 watchDocx/watchPptx/watchXlsx 三个方法（浏览器 mock
//     三个都有但恒返回失败，那种情况由渲染器自己的失败分支如实上报，不在这里假装可用）；
//   · embed      → preload 真有 embedOpenTarget + embedShow（WebContentsView 那条原生通道）。
// 这样「老版本桌面端缺某个 IPC」也能落到写明原因的位置，而不是白屏。
import { setDependencyStatus } from './registry'
import type { PreviewDependency } from './types'

/** window.desktop 的最小子集（只取判定要用的字段，避免这里 import 整份 global.d.ts）。 */
interface DesktopLike {
  watchDocx?: unknown
  watchPptx?: unknown
  watchXlsx?: unknown
  embedOpenTarget?: unknown
  embedShow?: unknown
}

const isFn = (v: unknown): boolean => typeof v === 'function'

/** 缺依赖的理由。诊断文案（与 bridge 的 error 同口径，不走 i18n，见 types.DependencyStatus）。 */
const REASONS: Record<PreviewDependency, string> = {
  conversion:
    '缺转换通道：docx/pptx/xlsx 要用桌面端的受管 Python 运行时（mammoth / python-pptx / openpyxl）'
    + '转成 HTML 才能看，当前环境没有 watchDocx/watchPptx/watchXlsx 这条 IPC',
  media: '缺媒体协议：音视频需要 hai-media://（Range）通道，当前环境没有',
  embed:
    '缺内嵌浏览器视图：pdf 由 Chromium 自带的 PDF viewer 渲染，它只存在于原生 WebContentsView'
    + '（embedOpenTarget/embedShow），浏览器预览模式没有这条通道',
}

/**
 * 探测并写入依赖状态。幂等、纯读环境，可在任意线程/时机调用。
 *
 * 传 `desktop` 只为测试与探针（默认取 window.desktop 的快照）——判断完不保留引用，
 * 环境变了再调一次即可。
 */
export function detectPreviewDeps(
  desktop: DesktopLike | undefined = (globalThis as { window?: { desktop?: DesktopLike } }).window?.desktop,
): void {
  const conversion = !!desktop && isFn(desktop.watchDocx) && isFn(desktop.watchPptx) && isFn(desktop.watchXlsx)
  const embed = !!desktop && isFn(desktop.embedOpenTarget) && isFn(desktop.embedShow)
  setDependencyStatus('conversion', conversion ? { ok: true } : { ok: false, reason: REASONS.conversion })
  setDependencyStatus('embed', embed ? { ok: true } : { ok: false, reason: REASONS.embed })
}

/** 依赖缺失时的理由文案（探针与测试要用同一份，不另抄一遍）。 */
export function dependencyReason(dep: PreviewDependency): string {
  return REASONS[dep]
}
