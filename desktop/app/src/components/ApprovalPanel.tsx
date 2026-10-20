import { useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'
import ApprovalCard from './ApprovalCard'
import { useScopedApproval, useScopeRunId } from './sessionScope'

// 审批面板（HITL）：只承载「**当前作用域**队首审批」的展示，卡片体与倒计时在共享的
// ApprovalCard。
//
// 2026-09-20（用户拍板「审批也不要全屏，和 ask_user 类似」）：从 `.modal-hint` 全屏 scrim
// 改成**停靠在叙述区与输入框之间的面板**（同 AskUserQuestionPanel）——无压暗、无遮挡，
// 叙述区只被压矮、照常滚动/选中，左栏也能点（可先翻对话历史/看 diff 再决定）。
// 语义未变：审批仍阻塞 agent 循环，不可取消；等待窗口见 events/session（30 分钟上限），
// 倒计时由桥接层注入的 timeout_ms 驱动。
//
// 2026-10（用户拍板「子智能体也有子智能体的待审批……等同于切换个 session」）：
// 面板从「主会话专属」改成**作用域感知** —— 看主会话时显示主 run 的审批，看子会话时显示
// 那个子 agent 的审批。判定只有一份（components/sessionScope），因为审批队列在后端仍是
// Session 级单 FIFO（主/子共用），这里做的是**展示过滤**而不是拆队列。
// 这样就不再需要「右栏任务 tab 承载子 agent 审批」那条旧路径（该 tab 已删除）。
export default function ApprovalPanel() {
  const t = useT()
  const scopeRunId = useScopeRunId()
  const a = useScopedApproval()
  const scopeCard = useAppStore((s) => {
    if (!scopeRunId || !s.activeSessionId) return undefined
    const v = s.views[s.activeSessionId]
    const b = v?.blocks.find((x) => x.kind === 'agent' && x.runId === scopeRunId)
    return b?.kind === 'agent' ? b : undefined
  })
  // 收起（只留标题条）：把版面让给叙述区。按审批 id 记录展开态，换队首自动展开。
  const [foldedId, setFoldedId] = useState<string | null>(null)
  if (!a) return null
  // 子会话里标注归属（主会话不需要：「这里就我一个」）
  const owner = scopeRunId ? (scopeCard?.label ?? t('rp.task.subAgent')) : undefined

  return (
    <div className="dock appr-dock" role="region" aria-label={t('approval.dialogAria')}>
      <div className="dock-box">
        <ApprovalCard
          a={a}
          ownerLabel={owner}
          folded={foldedId === a.id}
          onToggleFold={() => setFoldedId((cur) => (cur === a.id ? null : a.id))}
        />
      </div>
    </div>
  )
}
