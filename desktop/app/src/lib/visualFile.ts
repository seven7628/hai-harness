// 可视类文件（二进制/富媒体）判定 —— 右侧文件栏预览入口与「产出后自动打开」共用的
// **单一事实源**。
//
// 为什么必须抽出来：同一批扩展名此前散在 components/ArtifactCard.tsx（BROWSER_EXTS /
// PDF_EXTS / XLSX_EXTS）与各处零散判定里。本次改动要让**三个消费方**（产出卡主按钮分派、
// 文件栏 binary 分支的预览入口、store 的自动打开）对「哪些算可视类」给出完全一致的答案 ——
// 三份各写一遍必然漂移，而后果是静默的：卡片能预览、文件栏没有入口、或者自动打开时
// 判定成文本而不打开，用户只会感到「有时行有时不行」。
//
// 边界（产品口径，2026-09 用户拍板；docx/pptx 于本轮从「无通道」改为「有通道」）：
//   - rgb/image/browser（html/svg）→ 应用内**能**渲染 → 自动打开 + 给预览入口。
//   - xlsx/docx/pptx → 应用内**有**预览通道（bridge 用受管 Python 转 HTML 再走内嵌浏览器
//     file://，见 useAppStore 的 previewOfficeFile）→ 自动打开 + 给预览入口。
//     这三者与 rgb/image/browser 的差别只是**转换成本**（要起解释器；首次还可能 bootstrap
//     运行时），不是「能不能看」—— 故在「算不算可视类」这一层与它们同类。
//   - zip 等**确实没有**通道的格式 → 不自动打开，文件栏给「用默认程序打开」这条真实出口
//     （而不是指一个不存在的按钮）。
//   - 文本类（md/go/txt/代码）→ **不**自动打开：每次改代码都抢视线会刷屏（用户明确要求）。

import { isCsvPath } from './csv'

/** 扩展名（小写，不含点）。`foo`（无扩展名）与 `.gitignore`（点开头）都返回 ''。 */
export function extOf(name: string): string {
  const i = name.lastIndexOf('.')
  return i > 0 ? name.slice(i + 1).toLowerCase() : ''
}

/** 图片扩展名：与既有图片通道（ArtifactCard 的类型图标 / Go 侧 read_file 的图片分支）同集合。 */
export const IMAGE_EXTS = new Set(['png', 'jpg', 'jpeg', 'gif', 'webp', 'bmp'])
/** 浏览器可直接渲染的（html/svg → 内嵌浏览器 file://）。 */
export const BROWSER_EXTS = new Set(['html', 'htm', 'svg'])
/**
 * PDF：单列一类，语义与 browser 不同 —— 前者「预览文档」（Chromium 自带 PDF viewer），
 * 后者「打开网页」。渲染通道是同一条内嵌浏览器（见 store.openHtmlInBrowser），
 * 分开只为按钮文案与图标各走各的（后续给 PDF 加翻页/缩放也只动这一支）。
 */
export const PDF_EXTS = new Set(['pdf'])
/**
 * Excel 工作簿（.xlsx/.xlsm/.xls）：与 CSV 同属表格预览，但渲染通道不同 ——
 * xlsx 是二进制容器（zip + XML），前端解析要引新依赖（SheetJS 等，本项目不加新 npm 依赖），
 * 故由 bridge 用受管 Python（openpyxl）转成自带样式的 HTML 再走内嵌浏览器。
 * .xls 是 OLE2 老格式，openpyxl **不支持** —— 仍列进来：让按钮如实指向预览，
 * 由 bridge 给出明确失败原因（比「用默认程序打开」更符合点击预期）。
 */
export const XLSX_EXTS = new Set(['xlsx', 'xlsm', 'xls'])
/**
 * Word 文档（.docx/.docm）与 PowerPoint 演示文稿（.pptx/.pptm）：与 xlsx 同一条通道
 * （bridge 转 HTML → 内嵌浏览器 file://），只是转换器不同（mammoth / python-pptx）。
 *
 * .docm/.pptm 也列进来：它们是**同一个 OOXML 容器**（只是 content type 标了 macroEnabled），
 * mammoth 与 python-pptx 都能直接读（实测），没有理由把用户挡在外面。
 * .doc/.ppt 是 OLE2 老格式，两个库都不支持 —— 仍列进来，由 bridge 给出明确的失败原因
 * （与 .xls 同一口径：让按钮如实指向预览，而不是给一句「没有预览」的旧说法）。
 */
export const DOCX_EXTS = new Set(['docx', 'docm', 'doc'])
export const PPTX_EXTS = new Set(['pptx', 'pptm', 'ppt'])
/** 「Office 三件套」合并清单（产出卡/文件栏按扩展名判定「要不要走转换预览」时用同一份）。 */
export const OFFICE_EXTS = new Set([...XLSX_EXTS, ...DOCX_EXTS, ...PPTX_EXTS])

/** 可视类文件的渲染通道（决定用哪个既有 store 动作打开）。 */
export type VisualKind = 'pdf' | 'xlsx' | 'docx' | 'pptx' | 'image' | 'browser'

/**
 * 可视类判定：返回渲染通道，非可视类（含文本类与无应用内预览的 zip/音视频等）返回 undefined。
 *
 * 注意 CSV/TSV 不在其中：它是**文本**，文件栏能内联渲染成表格（CsvPreview），
 * 故走既有产出卡/文件栏文本通道，不参与「自动打开」。
 */
export function visualKindOf(path: string): VisualKind | undefined {
  if (!path) return undefined
  const ext = extOf(path)
  if (!ext) return undefined
  if (PDF_EXTS.has(ext)) return 'pdf'
  if (XLSX_EXTS.has(ext)) return 'xlsx'
  if (DOCX_EXTS.has(ext)) return 'docx'
  if (PPTX_EXTS.has(ext)) return 'pptx'
  if (IMAGE_EXTS.has(ext)) return 'image'
  if (BROWSER_EXTS.has(ext)) return 'browser'
  return undefined
}

/** 是否可视类（规则 1 的判定入口；CSV 明确不算，见 visualKindOf 说明）。 */
export function isVisualPath(path: string): boolean {
  return visualKindOf(path) !== undefined
}

/** 需要经 bridge 转换才能预览的格式（xlsx/docx/pptx）：它们走的不是 file:// 直开。 */
export function isConvertedPreviewPath(path: string): boolean {
  const k = visualKindOf(path)
  return k === 'xlsx' || k === 'docx' || k === 'pptx'
}

/** 可视类扩展名清单（提示文案用；与 visualKindOf 同源，不另写一份）。 */
export function isSpreadsheetPath(path: string): boolean {
  return isCsvPath(path) || XLSX_EXTS.has(extOf(path))
}
