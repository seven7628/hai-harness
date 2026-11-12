// 内置渲染器的注册入口（副作用 import，理由同 surfaces/builtin/index.ts：
// 「导出数组再遍历」会被 tree-shaking 看成没人引用，注册静默消失）。
//
// 由 PreviewHost 导入一次（不是 main.tsx）：凡是用到渲染器的地方都拿着 Host，
// 注册就必然发生过 —— F45 那类「探针只 import 了 RightPanel，provider 全 undefined」
// 的坑在源头堵掉。
//
// 本轮只注册三个（R08a）。pdf/docx/pptx/xlsx/media 是 R08b/R08c：它们的 match/weight
// 已经在 types.PreviewWeight 的口径里留好位置（100 = 专用格式），但**没有**注册 ——
// 于是那些扩展名现在落到 unavailable 占位，而不是白屏。
import { isCsvPath } from '../../../lib/csv'
import { IMAGE_EXTS } from '../../../lib/visualFile'
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
