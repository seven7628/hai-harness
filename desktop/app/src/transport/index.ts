import type { Transport } from './types'
import { electronTransport } from './electron'
import { mockTransport } from './mock'
import { sharedMobileTransport } from '../mobile/transport/mobile'
import { setHubTransport } from './host'

let cached: Transport | null = null

// 三档分流：
//   window.__HAI_MOBILE__（mobile.html 入口在 main.tsx 里置位）→ 手机 transport
//     （HTTP /api/cmd + WS /api/events，没有 Electron 主进程）。
//   window.desktop（Electron 渲染进程）→ 真实 bridge（IPC → Go stdio）。
//   其余（浏览器里 npm run dev）→ mock 事件流。
// 手机档必须先判：手机页同样没有 window.desktop，落到 mock 会让 Hub 静默拿假数据。
export function getTransport(): Transport {
  if (!cached) {
    if (typeof window !== 'undefined' && window.__HAI_MOBILE__ === true) cached = sharedMobileTransport()
    else cached = typeof window !== 'undefined' && window.desktop ? electronTransport() : mockTransport()
  }
  return cached
}

export function resetTransport(): void {
  cached = null
}

// Hub 的 store / 命令 / 附件从 `transport/host.ts` 取实现（P8-01b）：这里在模块求值时
// 把三档分流注册进去 —— 桌面（Electron / 浏览器 mock）行为与直接调 getTransport 完全一致；
// 手机入口不 import 本模块，改注册 sharedMobileTransport()，于是 electron/mock 不进手机包。
setHubTransport(getTransport)
