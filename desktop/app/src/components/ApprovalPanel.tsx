import { useState } from 'react'
import { useAppStore, approvalOwnerIsSub } from '../store/useAppStore'
import { useT } from '../i18n'
import ApprovalCard from './ApprovalCard'

// 主对话审批面板（HITL）：只承载「队首审批」的展示，卡片体与倒计时在共享的
// ApprovalCard（右栏任务 Tab 复用同一组件、同一 approve action，防重复决策）。
//
// 2026-09-20（用户拍板「审批也不要全屏，和 ask_user 类似」）：从 `.modal-hint` 全屏 scrim
// 改成**停靠在叙述区与输入框之间的面板**（同 AskUserQuestionPanel）——无压暗、无遮挡，
// 叙述区只被压矮、照常滚动/选中，左栏也能点（可先翻对话历史/看 diff 再决定）。
// 语义未变：审批仍阻塞 agent 循环，不可取消；等待窗口见 events/session（30 分钟上限），
// 倒计时由桥接层注入的 timeout_ms 驱动。
// 隐藏条件：队首属于 SubAgent 且右栏已展开并切到任务 Tab（右栏承载审批）——
// 其余情况（主 Agent 队首 / 右栏折叠 / 右栏在其他 tab）本面板兜底显示。
export default function ApprovalPanel() {
  const t = useT()
  const approvals = useAppStore((s) => s.approvals)
  const rightExpanded = useAppStore((s) => s.rightExpanded)
  const rightTab = useAppStore((s) => s.rightTab)
  const activeView = useAppStore((s) => (s.activeSessionId ? s.views[s.activeSessionId] : undefined))
  // 收起（只留标题条）：把版面让给叙述区。按审批 id 记录展开态，换队首自动展开。
  const [foldedId, setFoldedId] = useState<string | null>(null)
  const a = approvals[0]

  const subHead = a?.runId ? approvalOwnerIsSub(activeView, a.runId) : false
  const hiddenInRail = subHead && rightExpanded && rightTab === 'agent'
  if (!a || hiddenInRail) return null

  return (
    <div className="dock appr-dock" role="region" aria-label={t('approval.dialogAria')}>
      <div className="dock-box">
        <ApprovalCard
          a={a}
          folded={foldedId === a.id}
          onToggleFold={() => setFoldedId((cur) => (cur === a.id ? null : a.id))}
        />
      </div>
    </div>
  )
}
