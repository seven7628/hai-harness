// csv 渲染器：复用既有 CsvPreview（表格 + 说明条 + 宽表横滚）
//
// 为什么不重写：CsvPreview 的解析/闸门/转义全在 lib/csv.ts（纯函数、已单测），
// 表格 DOM 与 CSS 与对话区 Markdown 表格是**同一套**（见该组件顶部注释）。
// 渲染器这一层只做「把内容交给它」，多一行逻辑都是重复。
import CsvPreview from '../../../components/CsvPreview'
import type { PreviewProps } from '../types'

export default function CsvPreviewRenderer({ file, content }: PreviewProps) {
  return <CsvPreview content={content} path={file.path} />
}
