# 从构建产物 bundle 还原的前端源码（汇总版）

扫描 46 个候选 bundle（共 223MB，`/private/tmp` 下 depth≤2，已排除 node_modules / go module 缓存），
汇总出 133 个不同 `// src/...` 模块路径。仓库 `desktop/app/` 中缺失的 **16 个**已按原路径切出。

## 终端相关（worktree `terminal-panel`）

- `src/components/terminal/TermIcon.tsx` — 9 行（`store-bundle.cjs`）
- `src/components/terminal/TerminalPane.tsx` — 259 行（`store-bundle.cjs`）
- `src/components/terminal/TerminalTab.tsx` — 18 行（`store-bundle.cjs`）
- `src/components/terminal/commandContext-test.ts` — 100 行（`term-context.test.cjs`）
- `src/components/terminal/commandContext.ts` — 67 行（`term-context.test.cjs`）
- `src/components/terminal/formatSelection-test.ts` — 80 行（`term-format.test.cjs`）
- `src/components/terminal/formatSelection.ts` — 30 行（`store-bundle.cjs`）
- `src/components/terminal/frame-test.ts` — 50 行（`term-frame.test.cjs`）
- `src/components/terminal/frame.ts` — 22 行（`store-bundle.cjs`）
- `src/components/terminal/paneMachine-test.ts` — 120 行（`term-pane.test.cjs`）
- `src/components/terminal/paneMachine.ts` — 61 行（`store-bundle.cjs`）
- `src/components/terminal/theme.ts` — 49 行（`store-bundle.cjs`）
- `src/components/terminal/writer-test.ts` — 147 行（`term-writer.test.cjs`）
- `src/components/terminal/writer.ts` — 66 行（`store-bundle.cjs`）
- `src/surfaces/builtin/termSurface.tsx` — 195 行（`store-bundle.cjs`）

## 其余缺失模块

- `src/p8-host-seams-entry.tsx` — 105 行（`p8-host-seams-55139.cjs`）

> bundle 是 esbuild 编译产物（JSX 已转 `jsxDEV(...)`、局部名被重命名），
> 结构与注释完整，需人工回译成 TSX。
