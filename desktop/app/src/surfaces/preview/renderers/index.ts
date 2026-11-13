// 内置渲染器的注册入口（副作用 import，理由同 surfaces/builtin/index.ts：
// 「导出数组再遍历」会被 tree-shaking 看成没人引用，注册静默消失）。
//
// 由 PreviewHost 导入一次（不是 main.tsx）：凡是用到渲染器的地方都拿着 Host，
// 注册就必然发生过 —— F45 那类「探针只 import 了 RightPanel，provider 全 undefined」
// 的坑在源头堵掉。
//
// R08a 注册 text/image/csv；R08b 补齐 pdf/docx/pptx/xlsx（weight 100 = 专用格式）；
// R08c 补 media（音视频：原生 <video>/<audio> + hai-media:// 的 Range 通道）；
// 2026-11 补 html（html/htm/svg → srcDoc 静态快照，补的是 01-design §4.7 里本来就有的 id）。
import { isCsvPath } from '../../../lib/csv'
import { mediaKindOf } from '../../../lib/mediaFile'
import { BROWSER_EXTS, DOCX_EXTS, IMAGE_EXTS, PDF_EXTS, PPTX_EXTS, XLSX_EXTS } from '../../../lib/visualFile'
import { detectPreviewDeps } from '../deps'
import { registerRenderer } from '../registry'

// 注册顺序 = 同 weight 时的胜出顺序（见 registry.pickRenderer）。本轮三者 weight 互不相同，
// 顺序不影响结果；仍然按「专用 → 通用」写，读起来就是优先级。
registerRenderer({
  id: 'csv',
  weight: 60,
  match: (f) => (isCsvPath(f.path) ? 1 : 0),
  load: async () => (await import('./csv')).default,
})

registerRenderer({
  id: 'image',
  weight: 80,
  match: (f) => (IMAGE_EXTS.has(f.ext) ? 1 : 0),
  load: async () => (await import('./image')).default,
})

registerRenderer({
  id: 'text',
  weight: 10,
  // 兜底：非二进制即接。二进制不给文本渲染器（content 是空串，渲染出来是一块空代码面，
  // 用户会以为文件是空的）—— 这类文件应当落到 unavailable 并写明原因。
  match: (f) => (f.binary ? 0 : 1),
  load: async () => (await import('./text')).default,
})

// —— R08b：四种「专用格式」 ——
//
// weight 100 高于 text(10)/csv(60)/image(80)：这不是审美而是**判据**。.pdf 在 registry 里
// 同时被 text 接（非二进制就接 —— 而 pdf 在 bridge 眼里是二进制，故实务上 text 不接），
// 但反过来一旦有人把 text 的 match 放宽或把 size 传成 0，pdf 就会被抢走渲染成空代码面。
// 「专用格式永远先于通用兜底」这一条必须由数值保证，而不是由 match 的实现细节保证。
//
// requires 让「没装运行时」走到**挂载前**的 unavailable 占位（写明缺什么）：
//   · pdf → embed（原生内嵌视图，Chromium 的 PDF viewer 只在那里）
//   · docx/pptx/xlsx → conversion（受管 Python 的转换通道）
registerRenderer({
  id: 'pdf',
  weight: 100,
  match: (f) => (PDF_EXTS.has(f.ext) ? 1 : 0),
  requires: ['embed'],
  load: async () => (await import('./pdf')).default,
})

registerRenderer({
  id: 'docx',
  weight: 100,
  match: (f) => (DOCX_EXTS.has(f.ext) ? 1 : 0),
  requires: ['conversion'],
  load: async () => (await import('./office')).default,
})

registerRenderer({
  id: 'pptx',
  weight: 100,
  match: (f) => (PPTX_EXTS.has(f.ext) ? 1 : 0),
  requires: ['conversion'],
  load: async () => (await import('./office')).default,
})

registerRenderer({
  id: 'xlsx',
  weight: 100,
  match: (f) => (XLSX_EXTS.has(f.ext) ? 1 : 0),
  requires: ['conversion'],
  load: async () => (await import('./office')).default,
})

// —— R08c：音视频 ——
// weight 90：低于四种「专用文档格式」（100），高于 image(80) —— 媒体文件不该被图片或文本
// 抢走（mp4 在 bridge 眼里是二进制，text 本来不接；但 match 的顺序不该靠这个巧合）。
// requires:'media' 指 hai-media:// 通道本身（见 deps.ts 的判据）：通道不在时挂载前就落到
// 「写明缺什么」的占位，而不是渲染一个永远黑着的 <video>。
registerRenderer({
  id: 'media',
  weight: 90,
  match: (f) => (mediaKindOf(f.path) ? 1 : 0),
  requires: ['media'],
  load: async () => (await import('./media')).default,
})

// —— 2026-11：html（01-design §4.7 的渲染器清单里本来就有这个 id，此前没接上）——
// 缺失时的实际后果（用户报的回归）：.html 落 text(10) → 面板里是**源码**。
// weight 70：只要压过 text 兜底即可；与 image(80)/csv(60) 的扩展名互斥，数值不参与彼此的
// 判定（放 80 与 60 之间是为了让「专用格式 > 媒体 > 图片 > 页面 > 表格 > 兜底」这条阶梯
// 一眼可读，见 types.PreviewRenderer.weight 的口径）。这条必须由数值保证：text 的 match
// 是「非二进制就接」，靠 match 实现细节挡不住。
// match 与「默认通道」共用同一个事实源 BROWSER_EXTS（html/htm/svg）—— 两处各写一份扩展名
// 迟早漂移成「打开是浏览器、预览还是源码」这种半修状态。
registerRenderer({
  id: 'html',
  weight: 70,
  match: (f) => (BROWSER_EXTS.has(f.ext) ? 1 : 0),
  load: async () => (await import('./html')).default,
})

// 依赖探测放在注册之后：本模块是「渲染器在册」的唯一副作用入口，凡是能选到渲染器的地方
// 必然先执行过它 —— 于是「requires 有没有真值」与「渲染器在不在」不会各走一条时序
//（F45 那类「探针只 import 了一半」的坑在源头堵掉）。
detectPreviewDeps()
