// Hub transport seam：Hub 的 store / 命令 / 附件只从这里取 Transport。
//
// 为什么需要（P8-01b 的两个遗留边界之一）：`useHubStore.ts` / `hub/api.ts` /
// `hub/useHubAttachments.ts` 原来静态 import `../transport` 的 getTransport —— 而
// `transport/index.ts` 为了三档分流必然 import `./electron` 与 `./mock`。手机入口复用
// Hub 组件时，这条静态边会把 Electron IPC 与 mock 事件流一起拉进手机包（P8-01a 的
// 边界「手机包不 import 桌面 store/mock/electron」就被这条边捅破）。
//
// 做法与 host/panels.ts 同一条思路：**依赖由宿主注册，组件不认具体实现**。
//   · 桌面：`transport/index.ts` 在模块求值时注册 `getTransport`（三档分流不变）；
//   · 手机：`mobile/main.tsx` 在首次渲染前注册 `sharedMobileTransport()`；
//   · 测试/夹具：显式 import `transport/index.ts`（或自己 setHubTransport）。
//
// **没有默认实现**（刻意）：未注册时抛一条能直接定位的错误，而不是静默退回 mock。
// 静默 mock 的代价是「手机上看到的是假数据」——那正是 P8-01 要根治的失败模式。
import type { Transport } from './types'

let provider: (() => Transport) | null = null

/** 注册 Hub 用的 Transport 工厂。可重复调用（后注册的生效，便于测试替换）。 */
export function setHubTransport(factory: () => Transport): void {
  provider = factory
}

/** 撤销注册（仅测试用：验证「未注册 → 明确报错」这条契约）。 */
export function resetHubTransport(): void {
  provider = null
}

export function hasHubTransport(): boolean {
  return provider !== null
}

/**
 * 取当前 Hub Transport。
 *
 * 每次调用都走工厂：桌面是 `getTransport()`（内部有 cached 单例），手机是
 * `sharedMobileTransport()`（同样是进程内单例）—— 这里不再缓存一层，否则
 * `resetTransport()` 之后会拿到旧实例。
 */
export function getHubTransport(): Transport {
  if (!provider) {
    throw new Error(
      'Hub transport 未注册：桌面入口需 import src/transport/index.ts，'
        + '手机入口需在首次渲染前 setHubTransport(() => sharedMobileTransport())。'
        + '（P8-01b 刻意不提供默认 mock：静默假数据比报错更难查）',
    )
  }
  return provider()
}
