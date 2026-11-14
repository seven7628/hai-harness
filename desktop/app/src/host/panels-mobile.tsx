// 手机实现（P8-01a 基础设施 → P8-01b 接 Hub 屏幕）。
//
// 边界：**不 import useAppStore**（桌面 store 一旦进包，60+ 选择器 + Electron-only 动作
// 全跟着进来）。Hub store 可以 import —— 手机 Hub 屏幕本来就要用它，且它已经通过
// transport/host.ts + host/lang.tsx 两个 seam 与桌面 wiring 解耦。
//
// 动作落点：
//   · setRightExpanded → 详情抽屉开合（本地 state，HubInspector/GroupView 的「上下文」按钮共用）；
//   · closeHub          → useHubStore.closeHub（手机壳据此从会话页回到列表页）；
//   · openProjectSession→ 手机还没有会话页（P8-02）→ toast 指路，不静默吞点击；
//   · setActiveSession  → 同上（P8-01b 的手机 Hub 屏幕不渲染工作区会话入口）。
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
}: {
  children: ReactNode
  /** 手机壳的 toast 渲染口；不传就只落控制台，绝不静默吞掉。 */
  onToast?: (msg: string, kind: 'ok' | 'error') => void
  /** 手机语言（hai.mobile.lang / 旧页偏好）：i18n 通过 host/lang.tsx 读它。 */
  lang?: 'zh' | 'en'
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
      // 手机没有会话页（P8-02）：明确告诉用户去哪做，而不是让点击落空。
      openProjectSession: () => {
        const msg = '会话页在手机上还没开放，请在桌面端打开'
        if (onToast) onToast(msg, 'ok')
        else console.info('[mobile toast]', msg)
      },
      closeHub: () => useHubStore.getState().closeHub(),
      setActiveSession: () => {
        const msg = '会话页在手机上还没开放，请在桌面端打开'
        if (onToast) onToast(msg, 'ok')
        else console.info('[mobile toast]', msg)
      },
    }),
    [onToast],
  )

  return (
    <HostLangProvider lang={lang}>
      <HostCapabilitiesContext.Provider value={MOBILE_CAPABILITIES}>
        <HostPanelsActionsContext.Provider value={actions}>
          <HostPanelsLayoutContext.Provider value={rightExpanded}>{children}</HostPanelsLayoutContext.Provider>
        </HostPanelsActionsContext.Provider>
      </HostCapabilitiesContext.Provider>
    </HostLangProvider>
  )
}
