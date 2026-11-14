// 手机会话屏（P8-01b）：DM 与群共用**同一个** HubSurface。
//
// HubSurface 内部按 activeGroupId/activeId 决定渲染 GroupView 还是 DM 时间线 ——
// 消息块、任务卡、Needs、@ 选择器、状态条、composer、附件、流式/停止全部是桌面同一份。
//
// 导航（P8-03a 收口）：**只有一条头**。HubSurface/GroupView 自己的 .chat-header
// （头像/名字/角色/状态/上下文/更多）就是手机的头 —— 桌面那份原样保留；本层只把
// 「返回」做成浮在它左上角的 44px 按钮（CSS 见 .m-conv-head，.chat-header 让出
// 左侧 56px）。P8-01b 的外层头 + 内层头会同时出现两个「⋯」（外层开抽屉、内层是
// 更多菜单），现在只保留语义明确的内层入口：
//   · 抽屉 = .chat-header 的「上下文」按钮（DM 的 Layers / 群的 GitBranch，
//     与桌面同一个 setRightExpanded(true)）；
//   · 更多 = .chat-header 的「更多操作」菜单（Chat/Run、分组/平铺、深链、导出）。
//
// 标题：内层 .chat-title 已经显示当前智能体/群名，外层不再重复画一遍；这里保留
// 一个视觉隐藏的标题节点（data-testid="mobile-conv-title"）给 e2e 与读屏兜底。
import HubSurface from '../../hub/HubSurface'
import { useHubStore } from '../../hub/useHubStore'

export default function ConversationScreen({ onBack }: { onBack: () => void }) {
  const activeId = useHubStore((s) => s.activeId)
  const activeGroupId = useHubStore((s) => s.activeGroupId)
  const groups = useHubStore((s) => s.groups)
  const agents = useHubStore((s) => s.agents)

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
      </header>
      <div className="m-conv-body">
        <HubSurface />
      </div>
    </section>
  )
}
