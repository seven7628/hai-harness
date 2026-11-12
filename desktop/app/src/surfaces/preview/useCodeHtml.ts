// 文本渲染器的 shiki 高亮（**唯一实现**：文件面板与独立预览面板共用）
//
// 为什么抽成 hook：同一段高亮本来只在 FilePreview 里算一次（附加用途是「定位到读过的行」的
// 就绪信号）。R08a 把纯文本路径搬进渲染器后，两条路都需要同一份产物 —— 各写一遍必然漂移
// （实测过的代价：右侧栏预览曾经在「外观 → 代码主题」切换时纹丝不动，就是因为依赖数组里
// 只有亮/暗、没有具体主题名，见下面 codeTheme 的注释）。
import { useEffect, useState } from 'react'
import { codeToHtml } from '../../components/highlight'
import { useResolvedTheme } from '../../lib/useTheme'
import { useAppearance } from '../../lib/appearance'

/** 扩展名 → shiki 语言 id（未知回退 'text'，无高亮但有等宽样式）。 */
export function langFromPath(p: string): string {
  const ext = (p.split('.').pop() || '').toLowerCase()
  const map: Record<string, string> = {
    go: 'go', ts: 'typescript', tsx: 'tsx', js: 'javascript', jsx: 'jsx',
    json: 'json', yaml: 'yaml', yml: 'yaml', sql: 'sql', py: 'python',
    rs: 'rust', html: 'html', htm: 'html', css: 'css', md: 'markdown',
    vue: 'vue', // .vue 单文件组件（template + script + style 三块高亮）
  }
  return map[ext] ?? 'text'
}

/**
 * content → 高亮 HTML（'' = 还没好 / 不需要）。
 *
 * `enabled=false` 时不启动高亮（渲染器已从调用方拿到现成 html，或该文件是二进制）。
 * 依赖里的 `codeTheme` 不是摆设：高亮结果是**字符串**，不像 CSS 变量会自动跟着主题走，
 * 「外观 → 代码主题」改到同侧另一个主题时必须重算一次（见文件顶部注释）。
 */
export function useCodeHtml(content: string | undefined, path: string | undefined, enabled = true): string {
  const resolved = useResolvedTheme()
  const ap = useAppearance()
  const codeTheme = resolved === 'paper' ? ap.codeThemeLight : ap.codeThemeDark
  const [html, setHtml] = useState('')
  useEffect(() => {
    let alive = true
    if (!enabled || content == null) { setHtml(''); return }
    setHtml('')
    void codeToHtml(content, langFromPath(path ?? ''), resolved, true).then((h) => { if (alive) setHtml(h) })
    return () => { alive = false }
  }, [enabled, content, path, resolved, codeTheme])
  return html
}
