// 桌面实现：HostPanels 全部落到现有 store —— 与改造前组件里逐行读 useAppStore 等价。
//
// 唯一要盯的是「零行为变化」：
//   · rightExpanded/setRightExpanded/showToast/setActiveSession 直接取 store 的同名动作
//     （zustand 的动作引用稳定，memo 依赖不会每次 render 都变）；
//   · openProjectSession 与改造前 MessageBubble 里的模块级函数逐字等价：
//     先 closeHub()，再 setActiveSession(sessionId, workspace || undefined)；
//   · 动作与布局分两个 context：动作对象只在首次渲染建一次，右栏开合只更新布局 context ——
//     消费者重渲染集合与改造前「各自订阅 rightExpanded」完全一致。
import { useMemo, type ReactNode } from 'react'
import { useAppStore } from '../store/useAppStore'
import { useHubStore } from '../hub/useHubStore'
import {
  HostPanelsActionsContext,
  HostPanelsLayoutContext,
  type HostPanelsActions,
} from './panels'

export function HostPanelsProvider({ children }: { children: ReactNode }) {
  const rightExpanded = useAppStore((s) => s.rightExpanded)
  const setRightExpanded = useAppStore((s) => s.setRightExpanded)
  const showToast = useAppStore((s) => s.showToast)
  const setActiveSession = useAppStore((s) => s.setActiveSession)

  const actions = useMemo<HostPanelsActions>(
    () => ({
      setRightExpanded,
      showToast,
      setActiveSession,
      closeHub: () => useHubStore.getState().closeHub(),
      openProjectSession: (workspace, sessionId) => {
        useHubStore.getState().closeHub()
        useAppStore.getState().setActiveSession(sessionId, workspace || undefined)
      },
    }),
    [setRightExpanded, showToast, setActiveSession],
  )

  return (
    <HostPanelsActionsContext.Provider value={actions}>
      <HostPanelsLayoutContext.Provider value={rightExpanded}>{children}</HostPanelsLayoutContext.Provider>
    </HostPanelsActionsContext.Provider>
  )
}
