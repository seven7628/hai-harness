// M5：AI 动作 overlay 视图的 preload。
// overlay 是一个全透明、输入穿透的 WebContentsView，加载内联 data: URL 可视化页
// （画 AI 光标/涟漪/输入气泡）。此 preload 只暴露一个动作订阅入口——最小攻击面：
// 主进程 embed.ts 把 AI 的 CDP Input/Page 命令归类后经 'embed-overlay-action' 推过来。
import { contextBridge, ipcRenderer } from 'electron'

contextBridge.exposeInMainWorld('overlay', {
  onAction: (cb: (action: unknown) => void) => {
    const listener = (_e: unknown, action: unknown) => cb(action)
    ipcRenderer.on('embed-overlay-action', listener)
    return () => ipcRenderer.removeListener('embed-overlay-action', listener)
  },
})
