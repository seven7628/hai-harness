import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// VITE_PORT 可覆盖 dev 端口（本地多实例联调：两个 dev 各占一端口；默认 5173）。
// 手机联调走 `npm run mobile:dev`（5273），不要占 5173/5180。
const devPort = Number(process.env.VITE_PORT || 5173)

// 多入口：桌面 index.html + 手机 mobile.html。
//   · 默认 `npm run build`（桌面打包）→ 两个入口都进 dist/（Electron 只加载 index.html，
//     多出来的 mobile.html 不影响打包，但保证「同一份源码、同一条构建」能出手机页）。
//   · `npm run mobile:build`（MOBILE_ONLY=1）→ 只出手机入口，产物到 dist-mobile/，
//     并把 mobile.html 改名 index.html；bridge 的 /m/ 直接托管该目录（P8 方案 A）。
// 目录选择：dist-mobile（而不是 dist/mobile）—— dist 是桌面打包产物（含 hai-bridge 二进制），
// 手机产物单独一目录才不会被打包脚本/清理脚本当成桌面资产。
const mobileOnly = process.env.MOBILE_ONLY === '1'
const input: Record<string, string> = mobileOnly
  ? { mobile: 'mobile.html' }
  : { main: 'index.html', mobile: 'mobile.html' }

export default defineConfig({
  plugins: [react(), tailwindcss()],
  base: './',
  server: {
    port: devPort,
    strictPort: true,
    // —— 2026-11-13：别盯「测试产物」目录 ——
    //
    // vite 的 watcher 盯着整个 root（desktop/app）。别的会话在这个仓库里跑
    // E2E 探针/截图，产物落在 `e2e/_*probe_run/`、`e2e/*_shots/` 里 ⇒
    // **我们这个正在用的渲染进程每被写一次就整页重载一次**。
    //
    // 实测：滚动缓冲里 25 分钟内 **152 次** `page reload`（≈每分钟 5–8 次）。
    // 每次重载都要重跑一遍会话恢复（解析 1000 张卡 + 重建 React 树）⇒
    // 每次留几十 MB 垃圾。
    // 2026-11-17 更正：`--max-old-space-size=768` **已经设了**（在 electron/main.ts 的
    // js-flags 里）⇒ V8 会及时回收；这里描述的「堆单调上涨」如今由视图预算的
    // 「已提交堆」轴管（src/store/viewBudget.ts 的 v8HeapCommittedBytes）。
    //
    // 这些目录**不是应用源码** ⇒ 忽略它们不影响任何功能。
    //
    // 位置必须是 `server.watch.ignored`：Vite 的 resolveChokidarOptions 直接解构
    // `watch.ignored`（相对 root 的 glob 会被它 rebase 成绝对路径）。写在 `server`
    // 下一层会被静默忽略 —— 92c1d77 就那么放过一次，注释说得对、行为一点没变。
    watch: {
      ignored: [
      // 两种写法都要：vite 的 `server.watch.ignored` 按**相对 root** 解析，
      // 而 chokidar 实际匹配的是绝对路径 ⇒ 只写一种会有漏网（实测：
      // 只写 `**/e2e/**` 时，探针文件照样触发整页重载）。
      'e2e/**',
      '**/e2e/**',
      'release/**',
      '**/release/**',
      'dist/**',
      '**/dist/**',
      'dist-mobile/**',
      '**/dist-mobile/**',
      '**/*_shots/**',
      '**/*_run/**',
      '**/.go-code/**',
      ],
    },
  },
  build: {
    outDir: mobileOnly ? 'dist-mobile' : 'dist',
    rollupOptions: { input },
  },
})
