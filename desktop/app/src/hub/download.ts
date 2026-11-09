// 浏览器/Electron 下载（P4-02 导出）：Blob → 临时 `<a download>` → 释放 URL。
// 与 Inspector 里原有 JSON 导出同一套做法，抽出来给 Markdown 复用。

/** 文件名里的非法字符（Windows 保留字符 + 路径分隔符）统一换成 `-`。 */
export function safeFilename(text: string | null | undefined, fallback = 'hub'): string {
  const cleaned = String(text ?? '')
    .replace(/[\\/:*?"<>|\p{Cc}]+/gu, '-')
    .replace(/\s+/g, ' ')
    .trim()
    .replace(/^[-. ]+|[-. ]+$/g, '')
  return cleaned || fallback
}

/** 文件名时间戳：2026-03-10T12-34-56（冒号在部分文件系统非法）。 */
export function timestampSlug(date = new Date()): string {
  return date.toISOString().replace(/\.\d+Z$/, '').replace(/:/g, '-')
}

/** 触发下载；环境不支持 Blob/URL 时返回 false（调用方给可见反馈，别静默）。 */
export function downloadText(filename: string, text: string, mime = 'text/markdown;charset=utf-8'): boolean {
  try {
    if (typeof document === 'undefined' || typeof Blob === 'undefined' || typeof URL === 'undefined') return false
    const blob = new Blob([text], { type: mime })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = safeFilename(filename, 'hub-export.md')
    a.rel = 'noopener'
    a.click()
    URL.revokeObjectURL(url)
    return true
  } catch {
    return false
  }
}
