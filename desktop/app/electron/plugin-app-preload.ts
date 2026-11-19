// plugin-app-preload.ts —— URL 型插件页面看到的唯一宿主面：`window.haiHost`（2026-11）。
//
// 三条硬约束（与主窗口 preload 的 `__haiClaimHostApi` 同一套安全口径）：
//   1. 只暴露 `command` / `onEvent` 两个函数（外加只读的 pluginId），不暴露
//      `ipcRenderer`、`process`、`require`，也不把主进程的 IPC 频道名交给页面；
//   2. 页面只能调 manifest.commands 声明的命令、只能订阅 manifest.data 声明的事件
//      （这里先挡一次，main 与渲染层 host 再各挡一次 —— 三处同一条判据，任何一处单独
//      失效都不至于越权）；
//   3. 不是已登记的插件视图（`plugin-app:hello` 回 null）或页面已经导航到别的 origin
//      → 一个字节都不注入。页面在普通浏览器里打开时 `window.haiHost` 不存在，插件要
//      自行降级（这也是「本地能跑、嵌进宿主更完整」的天然分界）。
import { contextBridge, ipcRenderer } from 'electron'

interface HostConfig {
  pluginId: string
  origin: string
  events: string[]
  commands: string[]
}

let cfg: HostConfig | null = null
try {
  // sendSync 是 sandbox preload 里拿配置的最小面：argv 在打包/多实例下不可靠，而
  // 异步 invoke 会与页面脚本抢跑（页面可能在 DOMContentLoaded 就调 haiHost）。
  cfg = ipcRenderer.sendSync('plugin-app:hello') as HostConfig | null
} catch {
  cfg = null
}

function inject(): void {
  if (!cfg || !cfg.pluginId) return
  // 页面若已跳出声明 origin（重定向/外链），preload 仍在但不再给能力。
  const pageOrigin = (globalThis as { location?: { origin?: string } }).location?.origin ?? ''
  if (pageOrigin !== cfg.origin) return
  const allowedEvents = new Set(cfg.events)
  const allowedCommands = new Set(cfg.commands)
  const listeners = new Map<string, Set<(ev: unknown) => void>>()

  ipcRenderer.on('plugin-app:event', (_e, p: { type?: unknown; event?: unknown }) => {
    const type = typeof p?.type === 'string' ? p.type : ''
    if (!type || !allowedEvents.has(type)) return
    const set = listeners.get(type)
    if (!set) return
    for (const fn of [...set]) {
      try {
        fn(p?.event)
      } catch (err) {
        // 一个回调抛错不能打断同类型的其它订阅（页面自己的 bug，不该升级成宿主异常）。
        console.warn(`[haiHost] onEvent(${type}) 回调抛错：`, err)
      }
    }
  })

  try {
    contextBridge.exposeInMainWorld('haiHost', {
      pluginId: cfg.pluginId,
      /**
       * 调宿主命令。返回 `{ok, reason?, data?}`，**永不抛**（与 ESM 面板的 host.command
       * 同一个返回契约；未声明 = `{ok:false,reason:'not_declared'}`）。
       */
      command: async (name: string, payload?: unknown) => {
        const cmd = typeof name === 'string' ? name.trim() : ''
        if (!cmd) return { ok: false, reason: 'invalid_input' }
        if (!allowedCommands.has(cmd)) return { ok: false, reason: 'not_declared' }
        try {
          return await ipcRenderer.invoke('plugin-app:host-command', { name: cmd, payload })
        } catch (err) {
          return { ok: false, reason: 'error', data: err instanceof Error ? err.message : String(err) }
        }
      },
      /**
       * 订阅宿主事件。未在 manifest.data 声明 → 同步抛错（与 ESM 面板的 host.onEvent
       * 同一契约：写错事件名在启动阶段就炸，而不是「订阅成功但永远收不到」）。
       * 返回 disposer；页面组件卸载时调用它。
       */
      onEvent: (type: string, fn: (ev: unknown) => void) => {
        const t = typeof type === 'string' ? type.trim() : ''
        if (!allowedEvents.has(t)) throw new Error(`事件未在 manifest.data 中声明：${t || '(空)'}`)
        if (typeof fn !== 'function') throw new Error('onEvent 的第二个参数必须是函数')
        let set = listeners.get(t)
        if (!set) {
          set = new Set()
          listeners.set(t, set)
        }
        set.add(fn)
        return () => {
          set?.delete(fn)
        }
      },
    })
  } catch (err) {
    // 页面自己定义了 window.haiHost（极少数）：不覆盖，宿主能力不可用而已。
    console.warn('[haiHost] 注入失败（页面已有同名全局？）：', err)
  }
}

inject()
