// 手机会话页的「接线一次」入口（P8-02）。
//
// 手机壳需要 store 的 wire()（把 transport 事件接到 store 的分发器），但**不能**每次
// 挂载都调 start()：start() 是桌面启动流程，一旦 store 里已经有 workspace（打开过一个
// 会话之后就会有），它会继续往下走 —— 起 UI 快照（ui 命令）、插件加载器、
// workspace_sync、fetchSkills、loadWsGit。这些在手机上要么没有意义（没有左栏/设置页），
// 要么不在 bridge 的白名单里（skills / git_*）。
//
// 所以：整个进程只调一次，且要在 workspace 还空着的时候调 —— 那时 start() 只做两件事：
// wire() + loadSettings()，不发任何命令。之后的会话激活走 setActiveSession（桌面同一条
// 路径，见 screens/SessionScreen.tsx）。
//
// 幂等闸门放在模块级而不是 React 里：StrictMode 双挂载、列表↔会话往返、重新配对都
// 只会命中一次。
import { useAppStore } from '../store/useAppStore'

let booted = false

export function ensureStoreStarted(): void {
  if (booted) return
  booted = true
  void useAppStore.getState().start()
}

/** 仅供测试/夹具：忘掉「已接线」，让下一次 ensureStoreStarted 再跑一遍。 */
export function resetStoreStartedForTest(): void {
  booted = false
}
