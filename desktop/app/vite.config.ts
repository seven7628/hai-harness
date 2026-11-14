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
//     给 bridge 用 HAI_MOBILE_WEB_DIR 托管验证（P8-01 不动 /m/ 的默认 embed）。
// 目录选择：dist-mobile（而不是 dist/mobile）—— dist 是桌面打包产物（含 hai-bridge 二进制），
// 手机产物单独一目录才不会被打包脚本/清理脚本当成桌面资产。
const mobileOnly = process.env.MOBILE_ONLY === '1'
const input: Record<string, string> = mobileOnly
  ? { mobile: 'mobile.html' }
  : { main: 'index.html', mobile: 'mobile.html' }

export default defineConfig({
  plugins: [react(), tailwindcss()],
  base: './',
  server: { port: devPort, strictPort: true },
  build: {
    outDir: mobileOnly ? 'dist-mobile' : 'dist',
    rollupOptions: { input },
  },
})
