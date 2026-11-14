// 手机会话屏（P8-01b）：DM 与群共用**同一个** HubSurface。
//
// HubSurface 内部按 activeGroupId/activeId 决定渲染 GroupView 还是 DM 时间线 ——
// 消息块、任务卡、Needs、@ 选择器、状态条、composer、附件、流式/停止全部是桌面同一份。
// 这一层只加手机导航：44px 返回、当前目标标题、右侧详情抽屉入口。
//
// 不隐藏桌面 DOM：HubSurface 自己的 .chat-header（头像/名字/角色/更多操作）原样保留，
// 手机条只补「返回列表」这件事 —— 桌面的更多菜单（Chat/Run、分组/平铺、深链、导出）
// 在手机上仍然可用。
import HubSurface from '../../hub/HubSurface'
import { useHubStore } from '../../hub/useHubStore'
import { useHostPanels } from '../../host/panels'

export default function ConversationScreen({ onBack }: { onBack: () => void }) {
  const activeId = useHubStore((s) => s.activeId)
  const activeGroupId = useHubStore((s) => s.activeGroupId)
  const groups = useHubStore((s) => s.groups)
  const agents = useHubStore((s) => s.agents)
  const { setRightExpanded } = useHostPanels()

  const group = activeGroupId ? groups.find((g) => g.id === activeGroupId) : null
  const agent = !group && activeId ? agents.find((a) => a.id === activeId) : null
  const title = group?.name || agent?.name || (activeGroupId ? '群组' : '智能体')

  return (
    <section className="m-conv" data-testid="mobile-conversation" aria-label={title}>
      <header className="m-conv-head">
        <button
          type="button"
          className="m-icon-btn m-conv-back"
          onClick={onBack}
          aria-label="返回列表"
          title="返回列表"
          data-testid="mobile-back"
        >
          <span aria-hidden="true">←</span>
        </button>
        <span className="m-conv-title" data-testid="mobile-conv-title">{title}</span>
        <button
          type="button"
          className="m-icon-btn m-conv-inspector"
          onClick={() => setRightExpanded(true)}
          aria-label="详情（Context / 任务 / Needs）"
          title="详情（Context / 任务 / Needs）"
          data-testid="mobile-inspector-open"
        >
          <span aria-hidden="true">⋯</span>
        </button>
      </header>
      <div className="m-conv-body">
        <HubSurface />
      </div>
    </section>
  )
}
