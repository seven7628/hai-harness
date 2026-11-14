// 手机 Hub 列表屏（P8-01b）：只做布局/导航，列表本体是桌面同一个 HubSidebar。
//
// 为什么不再写一份列表：未读/运行态/预览/全历史搜索/群编辑弹层都在 HubSidebar 里，
// 复制一份必然漂移（P8 的立项理由）。手机差异只有两处，且都在组件内部由
// variant='mobile' 处理：
//   · 隐藏「返回工作区」（手机列表页就是 Hub 的家）；
//   · 隐藏新建/编辑群入口（mobile_server 的 allowlist 没放行 hub_group_create/edit）。
//
// 这一层只负责：撑满可视区、把 store 的读错误显示成一条可读横幅。
import HubSidebar from '../../hub/HubSidebar'
import { useHubStore } from '../../hub/useHubStore'
import type { MobileLinkState } from '../transport/mobile'

export default function HubListScreen({ hubLink }: { hubLink: MobileLinkState }) {
  const error = useHubStore((s) => s.error)
  const reconnecting = hubLink !== 'open' && hubLink !== 'idle'

  return (
    <div className="m-hub-list" data-testid="mobile-hub-list">
      {reconnecting ? (
        <p className="m-hub-banner" role="status" data-testid="mobile-hub-banner">
          Hub 事件流{hubLink === 'unauthorized' ? '凭据失效，请重新配对' : '重连中…'}
        </p>
      ) : null}
      <HubSidebar variant="mobile" />
      {error ? (
        <p className="m-warn m-hub-error" role="alert" data-testid="mobile-hub-error">
          {error}
        </p>
      ) : null}
    </div>
  )
}
