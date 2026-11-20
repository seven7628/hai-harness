// [recovered] from /private/tmp/builtin-surfaces.test.cjs（模块标记 `// src/surfaces/builtin/termSurface.tsx`）
// 回译（back-translation）：原文是 esbuild 的 `jsxDEV(...)` 编译形态 + 包内变量注入
//（`registerSurface` / `TerminalTab` / `TermIcon` 在 bundle 里是同作用域变量）。
// 本文件回译成 TSX 并补上真实 import，**注册参数逐字未改**；移除 @ts-nocheck（已被 tsc 覆盖）。
//
// 终端是右栏唯一的「非 React 内容」面板：外层仍是普通 surface provider（标题/图标/默认 spec
// 都归注册表），真正的内容交给 TerminalTab → TerminalPane（xterm + WS）。
import { registerSurface } from '../registry'
import { TerminalTab } from '../../components/terminal/TerminalTab'
import { TermIcon } from '../../components/terminal/TermIcon'

registerSurface({
  kind: 'term',
  namespace: 'builtin',
  group: 'workflow',
  // 排在 git(20) 之前：终端是「随手要开」的，越靠前越省一次点击
  order: 15,
  singleton: true,
  // 满幅：终端要正片区域（不要 right-body 的 10px 内边距与滚动条），
  // 否则 xterm 的列宽/行数会围着内边距算，右侧总留一条空带（§6.4）。
  fullBleed: true,
  scope: 'session',
  weight: 'heavy',
  capabilities: ['write'],
  title: (_tab, t) => t('rp.tab.term'),
  icon: () => <TermIcon />,
  defaultSpec: () => ({ kind: 'term' }),
  render: (ctx) => <TerminalTab tabId={ctx.tab.id} />,
})
