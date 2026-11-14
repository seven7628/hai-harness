// Host seam：Hub 展示组件与「宿主壳」（桌面 App / 手机壳）之间唯一的取值口。
//
// 为什么需要：Hub 组件（GroupView/HubSurface/HubInspector/HubInlineNeeds/HubAttachments/
// MessageBubble）原本直接读 useAppStore 的 6 个值 —— rightExpanded/setRightExpanded/
// showToast/openProjectSession/closeHub/setActiveSession。手机入口复用这些组件时，
// 一旦静态 import useAppStore，整棵桌面 wiring（60+ 选择器、Electron-only 动作、
// i18n 绑 store）都会被拉进手机包。P8-01a 的边界就是：**手机包不 import useAppStore**。
//
// 做法：组件只认这里的 context；桌面在 main.tsx 挂 HostPanelsProvider（实现仍是
// useAppStore，行为零变化），手机挂 mobile 的 provider（本地 state + no-op，P8-01b 再接）。
//
// 为什么拆成「动作」+「布局」两个 context（不是图省事的一个）：
//   · 动作引用在桌面是稳定的（zustand 动作不会换），只有布局（rightExpanded）会变；
//   · 合成一个 context 的话，右栏一开一合就会把 MessageBubble（每条消息一个）连同
//     HubAttachments 全部重渲染 —— 长群里这是几百次多余 render，而改造前它们根本没订阅
//     rightExpanded。拆开后：只有真正读布局的 3 个组件跟着变，与改造前重渲染集合一致。
//
// 缺省值全部是 no-op：组件被单独渲染（单测、lab、P8-01a 的最小手机壳）时不会抛。
import { createContext, useContext } from 'react'

/** 一次性动作（引用稳定，桌面实现见 panels-desktop.tsx）。 */
export interface HostPanelsActions {
  setRightExpanded(open: boolean): void
  showToast(msg: string, kind?: 'ok' | 'error'): void
  /** 打开任务卡指向的项目会话（退出 Hub + 切会话）。 */
  openProjectSession(workspace: string, sessionId: string): void
  closeHub(): void
  setActiveSession(id: string, projectPath?: string): void
}

export interface HostPanels extends HostPanelsActions {
  /** 右栏是否展开（桌面 = 三栏布局的右栏；手机 = 详情抽屉，P8-01b 接）。 */
  rightExpanded: boolean
}

const NOOP_ACTIONS: HostPanelsActions = {
  setRightExpanded: () => {},
  showToast: () => {},
  openProjectSession: () => {},
  closeHub: () => {},
  setActiveSession: () => {},
}

export const HostPanelsActionsContext = createContext<HostPanelsActions>(NOOP_ACTIONS)
export const HostPanelsLayoutContext = createContext(false)

/** 只取动作的组件用它：布局变化不触发重渲染（MessageBubble/HubAttachments/HubInlineNeeds）。 */
export function useHostPanelsActions(): HostPanelsActions {
  return useContext(HostPanelsActionsContext)
}

/** 同时要布局与动作的组件用它（GroupView/HubSurface/HubInspector）。 */
export function useHostPanels(): HostPanels {
  const actions = useHostPanelsActions()
  const rightExpanded = useContext(HostPanelsLayoutContext)
  return { ...actions, rightExpanded }
}
