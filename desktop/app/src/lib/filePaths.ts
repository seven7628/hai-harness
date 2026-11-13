// 路径工具：右栏文件预览与活动聚合共用（PR-04 从 RightPanel 抽出）。
export function normalizeFilePath(path: string): string {
  return path.replaceAll('\\', '/').replace(/^\.\//, '')
}


export function sameFilePath(a: string | undefined, b: string | undefined, ws: string | undefined): boolean {
  if (!a || !b) return false
  const na = toRelIfInside(a, ws)
  const nb = toRelIfInside(b, ws)
  return na === nb
}


export function toRelIfInside(p: string, ws: string | undefined): string {
  const n = normalizeFilePath(p)
  if (!ws || !n.startsWith('/')) return n
  const w = normalizeFilePath(ws).replace(/\/+$/, '')
  if (n === w) return ''
  if (n.startsWith(w + '/')) return n.slice(w.length + 1)
  return n // 工作区外：保持绝对（与 activities 中工作区外条目的写法一致）
}


export function fileUrlToPath(url: string): string {
  if (!url.startsWith('file://')) return ''
  try {
    return decodeURIComponent(url.slice('file://'.length))
  } catch {
    return '' // 非法编码：交给调用方走「非 file://」分支（宁可少显示，不要抛错）
  }
}
