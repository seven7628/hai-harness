// 手机实现（P8-01a 基础设施 → P8-01b 接 Hub 屏幕 → P8-02 接主会话页）。
//
// 边界：**不 import useAppStore**（桌面 store 一旦进包，60+ 选择器 + Electron-only 动作
// 全跟着进来）。Hub store 可以 import —— 手机 Hub 屏幕本来就要用它，且它已经通过
// transport/host.ts + host/lang.tsx 两个 seam 与桌面 wiring 解耦。
//
// 动作落点：
//   · setRightExpanded → 详情抽屉开合（本地 state，HubInspector/GroupView 的「上下文」按钮共用）；
//   · closeHub          → useHubStore.closeHub（手机壳据此从会话页回到列表页）；
//   · openProjectSession→ **P8-02 起真的跳手机会话页**：壳传进来的 onOpenSession 负责切屏，
//     Hub 任务卡的「打开会话」因此不再 toast 指路（它和会话列表走同一个落点）；
//   · setActiveSession  → 同上（参数是 (sid, ws)，ws 缺省用壳给的当前工作区）。
//
// 能力（HostCapabilities）：手机 bridge 的 allowlist 是**默认拒绝**，群组写操作/智能体
// 编辑器/产物预览/提示词预览都不在里面。这里全关 —— 组件只渲染点得动的入口，
// 不留会 403 的按钮（见 host/panels.ts 的 HostCapabilities 注释）。
import { useMemo, useState, type ReactNode } from 'react'
import { useHubStore } from '../hub/useHubStore'
import { HostLangProvider } from './lang'
import {
  HostCapabilitiesContext,
  HostPanelsActionsContext,
  HostPanelsLayoutContext,
  HostWorkspaceContext,
  type HostCapabilities,
  type HostPanelsActions,
} from './panels'

/** 手机能力：全部按 mobile_server 的 allowlist 收窄（见文件头）。 */
const MOBILE_CAPABILITIES: HostCapabilities = {
  groupAdmin: false,
  agentEditor: false,
  artifactPreview: false,
  promptPreview: false,
}

export function MobileHostPanelsProvider({
  children,
  onToast,
  lang = 'zh',
  workspace = '',
  onOpenSession,
}: {
  children: ReactNode
  /** 手机壳的 toast 渲染口；不传就只落控制台，绝不静默吞掉。 */
  onToast?: (msg: string, kind: 'ok' | 'error') => void
  /** 手机语言（hai.mobile.lang / 旧页偏好）：i18n 通过 host/lang.tsx 读它。 */
  lang?: 'zh' | 'en'
  /**
   * 当前打开的工作区（P8-02）：Markdown 的相对图片/链接要拿它当锚点。
   * 会话页打开时由壳传入；列表页是空串（不猜路径）。
   */
  workspace?: string
  /**
   * 打开某个工作区的某个会话（P8-02）：壳负责切屏，真正的激活在会话页里走
   * useAppStore.setActiveSession —— provider 自己不碰 store。
   * 不传（单测/夹具）= 退化成 toast 指路，绝不静默吞点击。
   */
  onOpenSession?: (workspace: string, sessionId: string) => void
}) {
  const [rightExpanded, setRightExpanded] = useState(false)

  const actions = useMemo<HostPanelsActions>(
    () => ({
      setRightExpanded,
      showToast: (msg, kind) => {
        const k = kind ?? 'ok'
        if (onToast) onToast(msg, k)
        else console.info(`[mobile toast:${k}]`, msg)
      },
      // Hub 任务卡「打开会话」/ 深链（P8-02）：跳到手机会话页。没接壳时才 toast 指路。
      openProjectSession: (ws, sid) => {
        if (onOpenSession && ws && sid) {
          onOpenSession(ws, sid)
          return
        }
        const msg = '这个会话没有可打开的工作区，请在桌面端查看'
        if (onToast) onToast(msg, 'ok')
        else console.info('[mobile toast]', msg)
      },
      closeHub: () => useHubStore.getState().closeHub(),
      setActiveSession: (sid, ws) => {
        const path = ws || workspace
        if (onOpenSession && sid && path) {
          onOpenSession(path, sid)
          return
        }
        const msg = '这个会话没有可打开的工作区，请在桌面端查看'
        if (onToast) onToast(msg, 'ok')
        else console.info('[mobile toast]', msg)
      },
      // Markdown 渲染的三个桌面动作（P8-01b）：手机没有文件预览/侧栏浏览器，
      // 明确 toast 指路（不是渲染一个点了没反应的链接）。
      openFile: () => {
        const msg = '文件预览在手机上还没开放，请在桌面端打开'
        if (onToast) onToast(msg, 'ok')
        else console.info('[mobile toast]', msg)
      },
      openHtmlContentInBrowser: () => {
        const msg = 'HTML 预览在手机上还没开放，请在桌面端打开'
        if (onToast) onToast(msg, 'ok')
        else console.info('[mobile toast]', msg)
      },
      // widget 回复是「沙箱 UI → 草稿」的通道：手机没有草稿区（P8-02），静默丢弃即可。
      receiveWidgetReply: () => {},
    }),
    [onToast, onOpenSession, workspace],
  )

  return (
    <HostLangProvider lang={lang}>
      <HostCapabilitiesContext.Provider value={MOBILE_CAPABILITIES}>
        {/* 会话页打开时壳传当前工作区；列表页是空串 = 相对资源不解析（不猜路径）。 */}
        <HostWorkspaceContext.Provider value={workspace}>
          <HostPanelsActionsContext.Provider value={actions}>
            <HostPanelsLayoutContext.Provider value={rightExpanded}>{children}</HostPanelsLayoutContext.Provider>
          </HostPanelsActionsContext.Provider>
        </HostWorkspaceContext.Provider>
      </HostCapabilitiesContext.Provider>
    </HostLangProvider>
  )
}
