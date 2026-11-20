// [recovered] from /private/tmp/builtin-surfaces.test.cjs（模块标记 `// src/components/terminal/TermIcon.tsx`）
// 回译（back-translation）：原文是 esbuild 的 `jsxDEV(...)` 编译形态，本文件按同一结构/同一
// 几何参数回译成 TSX，**不改变任何绘制结果**；补默认值与类型。移除 @ts-nocheck（已被 tsc 覆盖）。
//
// 为什么自己画而不是复用 components/icons：终端图标只有这一处用（右栏 tab + ＋菜单），
// 而 icons.tsx 里每个图标都是手写 path；塞进去会让那个文件继续膨胀，收益只有一次 import。

export function TermIcon({ size = 12 }: { size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 16 16"
      fill="none"
      aria-hidden="true"
    >
      <rect
        x="1.5"
        y="2.5"
        width="13"
        height="11"
        rx="2"
        stroke="currentColor"
        strokeWidth="1.2"
      />
      {/* 提示符 >_ */}
      <path d="M4.5 6.5 6.5 8l-2 1.5" stroke="currentColor" strokeWidth="1.2" strokeLinecap="round" strokeLinejoin="round" />
      <path d="M8.5 9.8h3" stroke="currentColor" strokeWidth="1.2" strokeLinecap="round" />
    </svg>
  )
}
