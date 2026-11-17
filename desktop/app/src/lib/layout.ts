// 右栏布局换算（PR-01；2026-11 收口成「右重 + 自定义」两态）。
//
// 口径：ratio = 右栏占「除左栏实际宽度之外」的百分比；bias 是预设，拖拽后变 custom。
// 注意：左栏实际宽度由 leftExpanded 决定（rail 44 / 展开 236），**不是** leftPinned——
// 2026-09 的修复明确「预览不收左栏」，所以这里只接受调用方传入的真实左栏像素。
//
// 为什么删掉「平分 / 居中」两个预设：用户 2026-11 拍板右栏只有一种默认形态 ——
// 展开就是右重（64%），要别的宽度自己拖。留着两个按不中的按钮只会让「默认是哪个」
// 变成每次都要重新判断的问题。
export type RightBias = 'right' | 'custom'
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
  right: 64,
}

const clamp = (v: number, lo: number, hi: number) => Math.max(lo, Math.min(hi, v))

/** 展开布局：默认（也是唯一预设）右重。 */
export function expand(bias: Exclude<RightBias, 'custom'> = 'right'): RightLayout {
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

/**
 * 老数据归一化（会话视图从磁盘恢复的路径）。
 *
 * `even` / `center` 是已下线的预设：恢复成它们会让「打开就是右重」在**老会话**上悄悄失效
 *（新会话对、老会话不对，是最难复现的那类 bug）。所以这里把两者都迁到 right；
 * custom 原样保留 —— 用户亲手拖出来的宽度不该被一次改版抹掉。
 */
export function normalizeLayout(layout: RightLayout | undefined, rightExpanded: boolean): RightLayout {
  if (layout) {
    if (!layout.open) return layout
    return layout.bias === 'custom' ? layout : expand('right')
  }
  return rightExpanded ? expand('right') : collapse
}
