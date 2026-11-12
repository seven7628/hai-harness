// text 渲染器：纯文本 / 代码 / 等宽视图（原 FilePreview「最终文件」的最后那条分支）
//
// 所有权边界：**只负责把内容摆成代码面**，不读文件、不管分页、不管行号（行号是 shiki 的
// lineNumbersTransformer 产物，与对话区代码块共用同一实现）。
// html 的两条来源见 types.PreviewProps：文件面板传现成的（它自己要拿它当定位锚），
// 独立预览面板不传 → 这里自己算（同一份 useCodeHtml，不会两套配色）。
import { useCodeHtml } from '../useCodeHtml'
import type { PreviewProps } from '../types'

export default function TextPreview({ file, content, html }: PreviewProps) {
  const own = useCodeHtml(content, file.path, html == null)
  return <div className="fp-code" dangerouslySetInnerHTML={{ __html: html ?? own }} />
}
