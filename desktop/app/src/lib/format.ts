// 通用格式化：右栏多处复用（文件预览、活动列表、git 面板）。
// 从 RightPanel 抽出，避免新组件文件反向 import 旧容器（会形成循环）。
export function ratio(n: number, total: number): number {
  return total > 0 ? Math.max(3, (n / total) * 100) : 0
}

/** 体积简写（列表右列：trace 只存元数据的体积，一眼可见存储账）。 */
export function fmtBytesShort(n: number): string {
  if (!n) return '—'
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(0)} KB`
  return `${(n / 1024 / 1024).toFixed(1)} MB`
}
