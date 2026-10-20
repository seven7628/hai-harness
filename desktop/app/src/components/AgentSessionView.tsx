import { memo, useMemo } from 'react'
import { useAppStore } from '../store/useAppStore'
import type { AgentFocus, MsgBlock } from '../store/useAppStore'
import { agentTreeFromView, pathOfNode } from '../lib/agentTree'
import type { AgentTreeNode } from '../lib/agentTree'
import { AgentTranscript } from './AgentTranscript'
import { ArtifactCard } from './ArtifactCard'
import { ToolTaskBody } from './TaskCard'
import { TaskResultInline } from './NarrativeBlocks'
import { ElapsedTimer } from './ElapsedTimer'
import { fmtUsd, UsageChip } from './ui'
import { Bot, ChevronRight, Clock } from './icons'
import { useT } from '../i18n'
import TodoPanel from './TodoPanel'

// 子智能体「子会话」视图：选中左栏树里的某个子智能体时，叙述区展示**它**的一切。
//
// 设计取舍（用户 2026-10 需求「子智能体可以看成独立的 session」）：
//   · 它是一个**视图**，不是另一个会话：消息仍然只发给主会话（子智能体的输入通道是
//     agent_send，且只有模型会用它）。所以这里没有输入框 —— 有输入框就等于暗示
//     「你可以直接跟它说话」，而协议上没有这条路。
//   · 停止/续跑等**动作**与任务卡同源（interrupt_task），不新开一套动作语义。
//   · 顶部面包屑显示祖先链（父 → 子 → 孙）：递归场景下「我现在看的是哪一层」必须可判读。
export const AgentSessionView = memo(function AgentSessionView({ focus }: { focus: AgentFocus }) {
  // 树节点身份用于定位节点/祖先链；真实 runId 用于找卡片（两者不同，见 AgentFocus 注释）
  const { nodeId, runId } = focus
  const t = useT()
  const runNodes = useAppStore((s) => s.runNodes)
  const blocks = useAppStore((s) => s.blocks)
  const focusAgent = useAppStore((s) => s.focusAgent)
  const interruptTask = useAppStore((s) => s.interruptTask)
  const loadAgentHistory = useAppStore((s) => s.loadAgentHistory)

  const treeBlocks = useMemo(
    () => blocks.filter((b) => b.kind === 'agent' || b.kind === 'async_task' || (b.kind === 'tool' && b.promoted)),
    [blocks],
  )
  const roots = useMemo(
    () => agentTreeFromView(runNodes, treeBlocks as Parameters<typeof agentTreeFromView>[1]),
    [runNodes, treeBlocks],
  )
  const chain = useMemo(() => pathOfNode(roots, nodeId), [roots, nodeId])
  // 卡片按**真实 runId** 找；退化到 taskId（agent 卡片先到、runNode 已收敛的恢复场景）
  const card = useMemo(
    () =>
      blocks.find((b): b is Extract<MsgBlock, { kind: 'agent' }> => {
        if (b.kind !== 'agent') return false
        if (runId && b.runId === runId) return true
        const node = chain[chain.length - 1]
        return Boolean(node?.taskId && b.taskId === node.taskId)
      }),
    [blocks, runId, chain],
  )

  // 后台工具任务的块（kind='tool' + promoted + 同 taskId）：内容在它身上，不在 agent 卡里
  const toolBlock = useMemo(() => {
    const node = chain[chain.length - 1]
    const key = node?.taskId
    if (!key) return undefined
    return blocks.find(
      (b): b is Extract<MsgBlock, { kind: 'tool' }> =>
        b.kind === 'tool' && Boolean(b.promoted) && b.taskId === key,
    )
  }, [blocks, chain])

  if (chain.length === 0) return <div className="empty">{t('agentTree.gone')}</div>
  const node = chain[chain.length - 1]
  const running = node.status === 'running' || node.status === 'interrupting'
  const canStop = Boolean(node.taskId) && running
  const usage = card?.usage

  return (
    <div className="agent-session" role="region" aria-label={t('agentTree.subSession')}>
      {/* 面包屑 + 动作条：祖先链每一层都可点（递归场景下直接回到上游某一层） */}
      <div className="as-hd">
        <span className="as-icon" aria-hidden="true">{node.kind === 'task' ? <Clock size={14} /> : <Bot size={14} />}</span>
        <span className="as-crumb">
          {chain.slice(0, -1).map((a) => (
            <button
              key={a.id}
              className="as-crumb-link"
              onClick={() => focusAgent({ nodeId: a.id, runId: a.runId ?? a.id, ...(a.taskId ? { taskId: a.taskId } : {}) })}
              title={a.label}
            >
              {a.label}
              <ChevronRight size={10} />
            </button>
          ))}
          <span className="as-crumb-cur" title={node.label}>{node.label}</span>
        </span>
        <span className={`as-st mono st-${statusCls(node.status)}`}>{statusLabel(node, t)}</span>
        {usage && (usage.input > 0 || usage.output > 0) && (
          <UsageChip
            className="as-usage mono"
            usage={usage}
            title={t('task.usage.tip')
              .replace('{tok}', String(usage.input + usage.output))
              .replace('{cost}', fmtUsd(usage.costUsd ?? 0))}
          />
        )}
        {(running || card?.durMs != null) && (
          <ElapsedTimer className="as-dur mono" startAt={node.startedAt} running={running} durMs={running ? undefined : card?.durMs} />
        )}
        {canStop && (
          <button
            className="as-stop"
            title={t('rp.task.stopTask')}
            aria-label={t('rp.task.stopTaskAria')}
            onClick={() => interruptTask(node.taskId!)}
          >
            ⏹
          </button>
        )}
        <button className="as-back" onClick={() => focusAgent(undefined)}>{t('agentTree.backToMain')}</button>
      </div>

      <div className="as-body">
        {/* 子智能体自己的 TODO（独立上下文 → 独立待办）：只显示它自己的清单，
            没有就整块不渲染 —— 绝不回退到主会话清单（那正是用户 2026-10 报障的 bug）。 */}
        <TodoPanel scope="agent" todos={card?.todos ?? []} />
        {card ? (
          <>
            <AgentTranscript
              items={card.items}
              running={running}
              trimmed={card.itemsTrimmed}
              loading={card.historyLoading}
              hasMore={card.historyHasMore}
              error={card.historyError}
              onLoadOlder={card.taskId ? () => loadAgentHistory(card.id, card.taskId!) : undefined}
            />
            {card.artifacts && card.artifacts.length > 0 && (
              <div className="ac-artifacts">
                <ArtifactCard items={card.artifacts} {...(card.artifactsTruncated ? { truncated: true } : {})} />
              </div>
            )}
            <TaskResultInline delivered={card.deliveredToMain} result={card.taskResult} error={card.taskError} />
          </>
        ) : toolBlock ? (
          // 后台工具任务（异步 bash 等）：展示它的参数/结果 —— 与主对话工具行同一套原语
          //（ToolTaskBody）。这就是它从右栏「任务」tab 搬过来的那份内容。
          <ToolTaskBody b={toolBlock} />
        ) : (
          // 有树节点但没有任何内容块：占位（卡片被 checkpoint 裁剪 / 事件尚未到达）。
          // 展示状态而不是空白 —— 用户点进来是想知道「它在干什么」。
          <div className="as-placeholder">
            <div className="as-ph-title">{node.label}</div>
            <div className="as-ph-meta mono">
              {statusLabel(node, t)}
              {node.toolName ? ` · ${node.toolName}` : ''}
              {node.taskId ? ` · ${node.taskId}` : ''}
            </div>
            <div className="as-ph-hint">{t('agentTree.noTranscript')}</div>
          </div>
        )}
      </div>
    </div>
  )
})

function statusCls(s: AgentTreeNode['status']): string {
  switch (s) {
    case 'running': return 'running'
    case 'interrupting': return 'interrupting'
    case 'completed': case 'done': return 'done'
    case 'interrupted': return 'interrupted'
    case 'failed': case 'error': return 'error'
    case 'abandoned': return 'abandoned'
    default: return 'idle'
  }
}

function statusLabel(n: AgentTreeNode, t: (k: string) => string): string {
  switch (n.status) {
    case 'running': return t('task.status.running')
    case 'interrupting': return t('task.status.interrupting')
    case 'completed': case 'done': return t('task.status.completed')
    case 'interrupted': return t('task.status.interrupted')
    case 'failed': case 'error': return t('task.status.failed')
    case 'abandoned': return t('task.status.abandoned')
    default: return t('agentTree.unknown')
  }
}
