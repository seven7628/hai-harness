// 右栏三态布局换算（PR-01）。纯函数，不碰 store / DOM。
// 口径：ratio = 右栏占「除左栏实际宽度之外」的百分比；bias 是预设，拖拽后变 custom。
// 注意：左栏实际宽度由 leftExpanded 决定（rail 44 / 展开 236），**不是** leftPinned——
// 2026-09 的修复明确「预览不收左栏」，所以这里只接受调用方传入的真实左栏像素。
export type RightBias = 'even' | 'center' | 'right' | 'custom'
export type RightLayout =
  | { open: false }
  | { open: true; bias: RightBias; ratio: number } // ratio = 右栏占「除左栏外」的百分比

// 只作文档/测试用的现状常量（布局换算已改成纯百分比，不再需要像素常量做分母）。
// 左栏宽度以 index.css --rail-w(44) 与 App collapsedSize(44)/defaultSize(236) 为准。
export const PX = {
  leftRail: 44,
  leftExpanded: 236,
} as const

export const BIAS_RATIO: Record<Exclude<RightBias, 'custom'>, number> = {
  even: 50,
  center: 36,
  right: 64,
}

const clamp = (v: number, lo: number, hi: number) => Math.max(lo, Math.min(hi, v))

export function expand(bias: Exclude<RightBias, 'custom'> = 'even'): RightLayout {
  return { open: true, bias, ratio: BIAS_RATIO[bias] }
}

export const collapse: RightLayout = { open: false }

/**
 * 拖拽后换算：入参是 `react-resizable-panels` 的 layout 百分比（所有 panel 的 offsetWidth 之和为 100）。
 * 用百分比而不是像素——库的 layout% 与 `getSize().inPixels` 分母不同（后者不含 8px 分隔条），
 * 混用会把窄拖拽当抖动丢掉。ratio 仍然定义为「右栏占除左栏外的百分比」。
 */
export function afterDrag(rightPct: number, leftPct: number): RightLayout {
  const avail = Math.max(1, 100 - leftPct)
  const ratio = clamp(Math.round((rightPct / avail) * 100), 0, 100)
  return { open: true, bias: biasOfRatio(ratio), ratio }
}

/** 百分比 → 预设 bias（±1 容差），否则 custom */
export function biasOfRatio(ratio: number): RightBias {
  const known = (Object.entries(BIAS_RATIO) as [Exclude<RightBias, 'custom'>, number][]).find(
    ([, r]) => Math.abs(r - ratio) <= 1,
  )
  return known?.[0] ?? 'custom'
}

/** 老数据没有 rightLayout 时用 rightExpanded 归一化（会话视图从磁盘恢复的路径） */
export function normalizeLayout(layout: RightLayout | undefined, rightExpanded: boolean): RightLayout {
  if (layout) return layout
  return rightExpanded ? expand('even') : collapse
}
