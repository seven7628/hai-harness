import type { Transport } from './types'
import { electronTransport } from './electron'
import { mockTransport } from './mock'

let cached: Transport | null = null

// 浏览器（无 window.desktop）→ mock 事件流；Electron → 真实 bridge
export function getTransport(): Transport {
  if (!cached) {
    cached =
      typeof window !== 'undefined' && window.desktop ? electronTransport() : mockTransport()
  }
  return cached
}

export function resetTransport(): void {
  cached = null
}
