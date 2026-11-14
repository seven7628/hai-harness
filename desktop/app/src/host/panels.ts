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
  /** 打开工作区文件（Markdown 里的相对链接）。 */
  openFile(path: string): void
  /** 在右侧浏览器页签打开一段 HTML（HTML 代码块的「打开」）。 */
  openHtmlContentInBrowser(html: string): void
  /** Markdown 内嵌 widget 的 postMessage 回复（只在 shape 校验通过后调用）。 */
  receiveWidgetReply(text: string, id: string): void
}

/**
 * 宿主能力开关（P8-01b）：**手机 bridge 的 allowlist 决定**哪些写操作在手机上不存在。
 *
 * 为什么要有这一层：desktop/bridge/mobile_server.go 的 mobileAllowedCommands 默认拒绝，
 * 群组 CRUD/协作模式/重置限制、智能体 CRUD、任务产物预览、提示词分层预览都**不在**名单里。
 * 手机壳复用桌面组件时，这些入口若照原样渲染，点下去就是 403 —— 比没有按钮更糟
 *（用户以为保存了，其实被拒）。所以由宿主声明能力，组件只渲染「点得动」的入口。
 *
 * 桌面（以及没有 provider 的组件测试/夹具）全部为 true —— 行为零变化。
 */
export interface HostCapabilities {
  /** 群组写操作：新建/编辑群、协作模式、日预算、重置本群限制（hub_group_create/edit/update/conversation_reset）。 */
  groupAdmin: boolean
  /** 智能体编辑器弹层（AgentEditor：hub_agent_create/update/delete/memory/versions/rollback）。 */
  agentEditor: boolean
  /** 任务产物预览（hub_task_artifact_preview，只读）。 */
  artifactPreview: boolean
  /** 提示词分层预览（hub_agent_prompt_preview，只读）。 */
  promptPreview: boolean
}

/** 桌面能力全开（默认值）：组件被单独渲染时与改造前完全一致。 */
export const DESKTOP_CAPABILITIES: HostCapabilities = {
  groupAdmin: true,
  agentEditor: true,
  artifactPreview: true,
  promptPreview: true,
}

export interface HostPanels extends HostPanelsActions {
  /** 右栏是否展开（桌面 = 三栏布局的右栏；手机 = 详情抽屉，P8-01b 接）。 */
  rightExpanded: boolean
  /** 宿主能力（手机按 mobile_server allowlist 收窄，见 HostCapabilities）。 */
  capabilities: HostCapabilities
}

const NOOP_ACTIONS: HostPanelsActions = {
  setRightExpanded: () => {},
  showToast: () => {},
  openProjectSession: () => {},
  closeHub: () => {},
  setActiveSession: () => {},
  openFile: () => {},
  openHtmlContentInBrowser: () => {},
  receiveWidgetReply: () => {},
}

export const HostPanelsActionsContext = createContext<HostPanelsActions>(NOOP_ACTIONS)
export const HostPanelsLayoutContext = createContext(false)
export const HostCapabilitiesContext = createContext<HostCapabilities>(DESKTOP_CAPABILITIES)

/**
 * 当前工作区根（Markdown 相对图片/链接的兜底锚点）。
 *
 * 为什么不放进 actions：它是**值**不是动作，桌面每次切工作区都会变 —— 混进动作 context
 * 会让每条 MessageBubble 跟着重渲染。没有 provider 时是空串：相对路径解析不出来就
 * 退回原样，不会把路径拼到某个猜出来的目录上。
 */
export const HostWorkspaceContext = createContext('')

/** 只取动作的组件用它：布局变化不触发重渲染（MessageBubble/HubAttachments/HubInlineNeeds）。 */
export function useHostPanelsActions(): HostPanelsActions {
  return useContext(HostPanelsActionsContext)
}

/** Markdown 渲染用：当前工作区根（空串 = 不知道，相对资源不解析）。 */
export function useHostWorkspace(): string {
  return useContext(HostWorkspaceContext)
}

/** 只读能力的组件用它（HubSidebar/TaskArtifacts/ContextPanel：不订阅右栏开合）。 */
export function useHostCapabilities(): HostCapabilities {
  return useContext(HostCapabilitiesContext)
}

/** 同时要布局与动作的组件用它（GroupView/HubSurface/HubInspector）。 */
export function useHostPanels(): HostPanels {
  const actions = useHostPanelsActions()
  const rightExpanded = useContext(HostPanelsLayoutContext)
  const capabilities = useContext(HostCapabilitiesContext)
  return { ...actions, rightExpanded, capabilities }
}
