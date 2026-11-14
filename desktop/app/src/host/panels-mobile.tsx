// 手机实现（P8-01a 基础设施）：本地 state + no-op。
//
// 刻意**不** import useAppStore / useHubStore：手机包一旦拉进桌面 store，整套桌面
// wiring（i18n 绑 store、Electron-only 动作、60+ 选择器）就跟着进包，P8-01 的复用
// 边界就白划了。P8-01b 接 Hub 屏幕时，右栏=详情抽屉（本地 state 已够），
// openProjectSession/closeHub/setActiveSession 再接 useHubStore 的对应动作。
import { useMemo, useState, type ReactNode } from 'react'
import {
  HostPanelsActionsContext,
  HostPanelsLayoutContext,
  type HostPanelsActions,
} from './panels'

export function MobileHostPanelsProvider({
  children,
  onToast,
}: {
  children: ReactNode
  /** 手机壳的 toast 渲染口（P8-01b 接 ToastView）；不传就只落控制台，绝不静默吞掉。 */
  onToast?: (msg: string, kind: 'ok' | 'error') => void
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
      // P8-01a 还没有 Hub 屏幕，这三个动作没有落点；保持 no-op 而不是抛。
      openProjectSession: () => {},
      closeHub: () => {},
      setActiveSession: () => {},
    }),
    [onToast],
  )

  return (
    <HostPanelsActionsContext.Provider value={actions}>
      <HostPanelsLayoutContext.Provider value={rightExpanded}>{children}</HostPanelsLayoutContext.Provider>
    </HostPanelsActionsContext.Provider>
  )
}
