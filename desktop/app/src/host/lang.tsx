// 语言 seam（P8-01b）：`i18n.ts` 的 useT/useLang/t 不再读 useAppStore。
//
// 为什么：i18n 被 Hub 组件（HubSurface/GroupView/HubInspector/MessageBubble/GroupEditor…）
// 静态 import。它原来直接 `useAppStore((s) => s.settings.lang)` —— 手机入口复用这些组件时，
// 这条边会把整棵桌面 store（60+ 选择器、Electron-only 动作、wire() 事件分发）拉进手机包。
//
// 做法与 host/panels.ts 同一条缝：宿主提供语言，i18n 只认这里。
//   · 桌面：`HostPanelsProvider`（main.tsx 的根）用 `useAppStore(s => s.settings.lang)`
//     取值，语言切换时 provider 重渲染 → context 值变 → useT 消费者重渲染（与改造前
//     各自订阅 settings.lang 的重渲染集合一致）；
//   · 手机：`MobileHostPanelsProvider` 用手机设置（localStorage `hai.mobile.lang`）。
//
// `t()`（非组件代码）读模块级 `current`：provider 渲染时同步写入（幂等赋值），
// 所以事件回调/动作里读到的语言与本次渲染一致。没有 provider 的组件测试/夹具
// 默认 zh —— 与改造前 mock store 的默认值相同，不会崩。
import { createContext, useContext, type ReactNode } from 'react'

export type HostLang = 'zh' | 'en'

/** 无 provider 时的默认语言：与桌面 settings 默认值一致。 */
const DEFAULT_LANG: HostLang = 'zh'

let current: HostLang = DEFAULT_LANG

export function normalizeHostLang(v: unknown): HostLang {
  return v === 'en' ? 'en' : 'zh'
}

/** 非组件代码（i18n.t()）用：读当前语言，不订阅。 */
export function getHostLang(): HostLang {
  return current
}

const HostLangContext = createContext<HostLang>(DEFAULT_LANG)

/** 组件用：订阅当前语言（语言切换时重渲染）。 */
export function useHostLang(): HostLang {
  return useContext(HostLangContext)
}

export function HostLangProvider({ lang, children }: { lang: unknown; children: ReactNode }) {
  const value = normalizeHostLang(lang)
  // 同步模块级值（幂等）：provider 是根节点，任何一次渲染都发生在子树之前，
  // 所以同一次渲染里 `t()` 与 `useT()` 看到的是同一种语言。
  current = value
  return <HostLangContext.Provider value={value}>{children}</HostLangContext.Provider>
}
