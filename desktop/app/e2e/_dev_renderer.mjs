// _dev_renderer.mjs —— 让**生产档** e2e（真 Electron + 原生视图池）改用 dev 渲染层。
//
// 为什么需要它（L0 / D6 之后）：preload 只暴露一次性交接 `__haiClaimHostApi`，宿主在模块
// 求值时就把 api 领走（docs/creator-mode-v0.1/09-agent-bridge.md §6.1）。这个属性由
// contextBridge 定义成 **non-writable / non-configurable**，所以页面里既不能二次 claim，
// 也不能包一层旁听 —— 生产构建里更不该有读取口（`__haiTestBridge` 只在非生产构建存在，
// 见 src/lib/bridge.ts）。
//
// 于是这类「要直接调 embed* / pickApp 这类 IPC」的探针改成 **dev 渲染层**（与
// verify_preview_container / verify_pdf_preview 同一条路）：主进程仍是 dist-electron
// 的真产物、桥仍是真桥，只有 renderer 由 vite 供源码 —— 而 dev 档有 __haiTestBridge 可读。
//
// 用法：
//   import { startDevRenderer, waitHostBridge } from './_dev_renderer.mjs'
//   const dev = await startDevRenderer({ port: 5264 }) // 挑一个没被并行探针占的端口
//   app = await _electron.launch({ env: { ...process.env, HOME, HAI_DATA_DIR: HOME, VITE_DEV_SERVER_URL: dev.url } })
//   ... await waitHostBridge(win)
//   dev.stop()  // 退出时
import { spawn } from 'node:child_process'
import net from 'node:net'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const APP = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function waitPort(port, timeoutMs = 60000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const up = await new Promise((resolve) => {
      const s = net.connect({ port, host: '127.0.0.1' })
      const t = setTimeout(() => { s.destroy(); resolve(false) }, 500)
      s.on('connect', () => { clearTimeout(t); s.destroy(); resolve(true) })
      s.on('error', () => resolve(false))
    })
    if (up) return
    if (Date.now() > deadline) throw new Error(`vite dev server 未在 ${timeoutMs}ms 内就绪（端口 ${port}）`)
    await sleep(250)
  }
}

/**
 * 起一个 vite dev server（只服务 renderer）。返回 { url, stop }。
 * ⚠️ 端口必须由调用方挑一个**没被别的探针占**的（5262 是 host-boundary 的，别抢）。
 */
export async function startDevRenderer({ port, timeoutMs = 60000 } = {}) {
  if (!port) throw new Error('startDevRenderer 需要 port')
  const proc = spawn(
    'node',
    [path.join(APP, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(port), '--strictPort', '--host', '127.0.0.1'],
    { cwd: APP, env: { ...process.env, VITE_PORT: String(port) }, stdio: 'pipe' },
  )
  let err = ''
  proc.stderr.on('data', (d) => { err += String(d) })
  proc.stdout.on('data', () => { /* 保持管道不阻塞 */ })
  const stop = () => { try { proc.kill('SIGTERM') } catch { /* 已退出 */ } }
  await waitPort(port, timeoutMs).catch((e) => { stop(); throw new Error(`${e.message}\n${err.slice(-800)}`) })
  return { url: `http://127.0.0.1:${port}/`, stop }
}

/**
 * 等 dev 档的宿主桥读取口就绪（__haiTestBridge 由 src/lib/bridge.ts 在挂载时写）。
 * 拿不到 = 交接链没走通，直接抛（比让后面每条断言各自 TypeError 干净）。
 */
export async function waitHostBridge(win, { timeout = 30000 } = {}) {
  const end = Date.now() + timeout
  for (;;) {
    const type = await win.evaluate(() => typeof window.__haiTestBridge).catch(() => 'n/a')
    if (type === 'object') return
    if (Date.now() > end) throw new Error(`等待 __haiTestBridge 超时（typeof = ${type}）：宿主没有完成交接`)
    await sleep(250)
  }
}
