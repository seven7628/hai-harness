import { memo } from 'react'
import { useAppStore, mainRunActive } from '../store/useAppStore'
import { useClock } from '../lib/useNow'
import { fmtRun } from './ui'
import HaiLogo from './HaiLogo'
import { useT } from '../i18n'

// AgentRunningBar：输入框上方的「Agent 运行中」状态条。
// 条件与停止按钮同源（mainRunActive 顶层 run running，且未被用户中断）：
//   - agent_start → agent_end 全程显示（流式/工具/压缩/审批/重试）；
//   - 用户点击停止后 interrupt() 乐观置 aborted → 立即消失（不等 agent_end）；
//   - 崩溃恢复时 finalizeReplayView/NormalizeForRestore 收敛 running → 不残留。
// 仅主 run：后台子 agent（agent_spawn 派生）不在此显示（右栏任务卡有自己的状态）。
// 计时复用全局单一时钟（useClock，订阅计数驱动启停），每秒刷新「· Ns…」。
export const AgentRunningBar = memo(function AgentRunningBar() {
  const view = useAppStore((s) => (s.activeSessionId ? s.views[s.activeSessionId] : undefined))
  const active = mainRunActive(view) && !view?.aborted
  const t = useT()
  const desktopTool = view?.desktopTool // computer_* 工具执行中（tool_run_start 置 / end 清）
  // 顶层 run 的 startedAt（首个 agent_start 时间戳）：实时时长锚点
  const startAt = view?.runNodes.find((n) => n.kind === 'run' && !n.parentId && n.status === 'running')?.startedAt
  const live = useClock(startAt, active)
  if (!active) return null
  return (
    <div className="agent-running-bar" role="status" aria-live="polite" aria-label={t('composer.runningAria')}>
      <span className="agent-running-logo" aria-hidden="true">
        <HaiLogo height={14} />
      </span>
      <span className="agent-running-dot" aria-hidden="true" />
      <span className="agent-running-label">
        {desktopTool ? t('composer.runningDesktop') : t('composer.running')}
      </span>
      {startAt != null && <span className="agent-running-ms mono">{fmtRun(live)}</span>}
    </div>
  )
})
