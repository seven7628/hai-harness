import { memo } from 'react'
import { useAppStore, mainRunActive } from '../store/useAppStore'
import { useClock } from '../lib/useNow'
import { fmtRun } from './ui'
import HaiLogo from './HaiLogo'
import { useT } from '../i18n'
import { agentRunStateFromView } from '../lib/agentTree'
import type { ViewAgentBlock, ViewRunNode } from '../lib/agentTree'

// AgentRunningBar：输入框上方的「Agent 运行中」状态条。
//
// 作用域与 Composer 的停止按钮同源：
//   · 主会话视图 → mainRunActive（顶层 run running，且未被用户中断）；
//   · 子会话视图 → **那个子 agent 当前这一轮**的 running/startedAt（agentRunStateFromView）。
//
// 2026-11 用户报障：切到子智能体后，这条仍显示主 Session 的「Agent 运行中 36s」。
// 旧实现只读 mainRunActive，子会话复用了同一个 Composer，于是把主会话的运行态也带进来了。
// 子会话不是主会话的附庸：它没在跑就不显示这条，在跑就只显示它自己的时长。
// inline：把这条状态条交给**输入框上沿那一行**的左槽（待办页签在右槽，同一行）。
// 只改布局（左对齐、20px、不居中），语义/DOM/aria 一个字节都没动 —— 运行状态该有的
// role=status、data-agent-scope、计时来源都不变，变的只是它站哪一行。
export const AgentRunningBar = memo(function AgentRunningBar({ inline = false }: { inline?: boolean } = {}) {
  const view = useAppStore((s) => (s.activeSessionId ? s.views[s.activeSessionId] : undefined))
  const focused = useAppStore((s) => s.focusedAgent)
  const t = useT()
  // 主会话：mainRunActive + 顶层 run 的 startedAt。
  const mainActive = mainRunActive(view) && !view?.aborted
  // 子会话：按 taskId 解析当前 run（agent_resume 后 focused.runId 可能过期）。
  const sub = focused && view
    ? agentRunStateFromView(
        [...view.blocks, ...(view.agentDetached ?? [])].filter((b) => b.kind === 'agent') as ViewAgentBlock[],
        view.runNodes as ViewRunNode[],
        focused,
      )
    : undefined
  const active = focused ? Boolean(sub?.running) : mainActive
  const startAt = focused
    ? sub?.startedAt
    : view?.runNodes.find((n) => n.kind === 'run' && !n.parentId && n.status === 'running')?.startedAt
  const desktopTool = focused ? undefined : view?.desktopTool // 子会话的 computer_* 不在这里冒充主会话
  const live = useClock(startAt, active)
  if (!active) return null
  return (
    <div
      className={inline ? 'agent-running-bar inline' : 'agent-running-bar'}
      role="status"
      aria-live="polite"
      aria-label={t('composer.runningAria')}
      data-agent-scope={focused ? 'subagent' : 'main'}
      data-agent-task={sub?.taskId || focused?.taskId || focused?.nodeId || undefined}
    >
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
