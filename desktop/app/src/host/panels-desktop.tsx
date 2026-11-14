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
  DESKTOP_CAPABILITIES,
  HostCapabilitiesContext,
  HostPanelsActionsContext,
  HostPanelsLayoutContext,
  HostWorkspaceContext,
  type HostPanelsActions,
} from './panels'
import { HostLangProvider } from './lang'

export function HostPanelsProvider({ children }: { children: ReactNode }) {
  const rightExpanded = useAppStore((s) => s.rightExpanded)
  const setRightExpanded = useAppStore((s) => s.setRightExpanded)
  const showToast = useAppStore((s) => s.showToast)
  const setActiveSession = useAppStore((s) => s.setActiveSession)
  // 语言也从宿主下发（P8-01b）：i18n 不再 import useAppStore，改读 host/lang。
  // 这里订阅 settings.lang —— 切语言时本组件重渲染，context 值随之更新。
  const lang = useAppStore((s) => s.settings.lang)
  // Markdown 的相对资源锚点（对话区没有文件可锚时退回工作区根）。
  const workspaceRoot = useAppStore((s) => s.workspace)

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
      // Markdown 渲染的三个桌面动作（P8-01b）：原来 markdown.tsx 直接读 store，
      // 手机复用消息渲染时会把整个桌面 store 拉进手机包。实现与改造前逐字等价。
      openFile: (path) => useAppStore.getState().openFile(path),
      openHtmlContentInBrowser: (html) => {
        void useAppStore.getState().openHtmlContentInBrowser(html)
      },
      receiveWidgetReply: (text, id) => useAppStore.getState().receiveWidgetReply(text, id),
    }),
    [setRightExpanded, showToast, setActiveSession],
  )

  return (
    <HostLangProvider lang={lang}>
      {/* 桌面能力全开：群组/智能体编辑、产物预览、提示词预览都在桌面端存在。 */}
      <HostCapabilitiesContext.Provider value={DESKTOP_CAPABILITIES}>
        <HostWorkspaceContext.Provider value={workspaceRoot ?? ''}>
          <HostPanelsActionsContext.Provider value={actions}>
            <HostPanelsLayoutContext.Provider value={rightExpanded}>{children}</HostPanelsLayoutContext.Provider>
          </HostPanelsActionsContext.Provider>
        </HostWorkspaceContext.Provider>
      </HostCapabilitiesContext.Provider>
    </HostLangProvider>
  )
}
