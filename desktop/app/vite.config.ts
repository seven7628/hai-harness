import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// VITE_PORT 可覆盖 dev 端口（本地多实例联调：两个 dev 各占一端口；默认 5173）。
const devPort = Number(process.env.VITE_PORT || 5173)

export default defineConfig({
  plugins: [react(), tailwindcss()],
  base: './',
  server: { port: devPort, strictPort: true },
  build: { outDir: 'dist' },
})
