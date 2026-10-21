import { memo, useEffect, useMemo } from 'react'
import { useAppStore } from '../store/useAppStore'
import type { AgentFocus, MsgBlock } from '../store/useAppStore'
import { agentTreeFromView, pathOfNode } from '../lib/agentTree'
import type { AgentTreeNode } from '../lib/agentTree'
import { agentItemsToBlocks } from '../lib/agentSession'
import { TaskResultInline } from './NarrativeBlocks'
import { ArtifactCard } from './ArtifactCard'
import Narrative from './Narrative'
import type { NarrativeSource } from './Narrative'
import { ElapsedTimer } from './ElapsedTimer'
import { fmtUsd, UsageChip } from './ui'
import { useScopedCompression } from './sessionScope'
import { Bot, ChevronRight, Clock } from './icons'
import { useT } from '../i18n'

// 子智能体「子会话」视图：选中左栏树里的某个子智能体时，叙述区展示**它**的一切。
//
// ## 它现在是「主会话页 + 一个返回」（用户 2026-10 拍板）
//
// 原话：「子会话完全可以复用主会话的页面样式与功能；不需要有独立的了」。
//
// 之前这里是**一套独立的渲染**（自己的滚动容器 + AgentTranscript）。样式能对齐（两边
// 都调 NarrativeBlocks 的原语），但**页面级行为**对齐不了 —— 贴底跟随、历史渐入、
// 「加载更早」、空态都住在 Narrative 里，子会话那份只能各写一遍或干脆没有，于是
// 「下方一大片空白、不能跟随、翻不到更早」。
//
// 现在这个组件只做三件事，一件都不碰版式：
//   1. 把子智能体的活动记录映射成会话块（lib/agentSession.agentItemsToBlocks）
//   2. 把它的分页态接成 NarrativeSource（子 agent 的历史在子任务 journal 里）
//   3. 顶部那一行「面包屑 / 状态 / 用量 / 停止 / 返回主会话」作为 Narrative 的 header
//
// ## 仍然是「视图」而不是「会话」
//
// 消息只发给主会话（子智能体的输入通道是 agent_send，只有模型会用它）—— 所以这里没有
// 自己的输入框，Composer 仍是主会话的，并明示了这一点。
// 停止/续跑与任务卡同源（interrupt_task），不新开一套动作语义。
export const AgentSessionView = memo(function AgentSessionView({ focus }: { focus: AgentFocus }) {
  // 树节点身份用于定位节点/祖先链；真实 runId 用于找卡片（两者不同，见 AgentFocus 注释）
  const { nodeId, runId } = focus
  const t = useT()
  const runNodes = useAppStore((s) => s.runNodes)
  const blocks = useAppStore((s) => s.blocks)
  const detached = useAppStore((s) => s.agentDetached)
  const focusAgent = useAppStore((s) => s.focusAgent)
  const interruptTask = useAppStore((s) => s.interruptTask)
  const loadAgentHistory = useAppStore((s) => s.loadAgentHistory)
  // 压缩态按同一套作用域判定（子会话的压缩是它自己的上下文压缩，与主会话互不影响）
  const comp = useScopedCompression()

  // 查找顺序 = 先「首屏窗口内的块」再「窗口外卡片」：同一张卡两边都在时（翻页翻到它
  // 之后）以 blocks 里的完整版为准。
  const allBlocks = useMemo(() => [...blocks, ...detached], [blocks, detached])
  const treeBlocks = useMemo(
    () => allBlocks.filter((b) => b.kind === 'agent' || b.kind === 'async_task' || (b.kind === 'tool' && b.promoted)),
    [allBlocks],
  )
  const roots = useMemo(
    () => agentTreeFromView(runNodes, treeBlocks as Parameters<typeof agentTreeFromView>[1]),
    [runNodes, treeBlocks],
  )
  const chain = useMemo(() => pathOfNode(roots, nodeId), [roots, nodeId])
  // 卡片按**真实 runId** 找；退化到 taskId（agent 卡片先到、runNode 已收敛的恢复场景）
  const card = useMemo(
    () =>
      allBlocks.find((b): b is Extract<MsgBlock, { kind: 'agent' }> => {
        if (b.kind !== 'agent') return false
        if (runId && b.runId === runId) return true
        const node = chain[chain.length - 1]
        return Boolean(node?.taskId && b.taskId === node.taskId)
      }),
    [allBlocks, runId, chain],
  )

  // 后台工具任务的块（kind='tool' + promoted + 同 taskId）：内容在它身上，不在 agent 卡里
  const toolBlock = useMemo(() => {
    const node = chain[chain.length - 1]
    const key = node?.taskId
    if (!key) return undefined
    return allBlocks.find(
      (b): b is Extract<MsgBlock, { kind: 'tool' }> =>
        b.kind === 'tool' && Boolean(b.promoted) && b.taskId === key,
    )
  }, [allBlocks, chain])

  // 窗口外的卡片只带骨架（items 清空、itemsTrimmed 记条数）：点进来要真能看到内容，
  // 所以在这里补拉首屏 —— offset=0 取的正是最新一页（见 store.loadAgentHistory）。
  // 只在「空 + 有 task_id（有 journal）+ 还没拉过」时拉一次：失败不自动重试（会打圈），
  // 由下方的「加载更早」按钮手动重试（historyHasMore/trimmed 仍为真，按钮在）。
  const cardId = card?.id
  const cardTaskId = card?.taskId
  const needFirstScreen = Boolean(card && card.items.length === 0 && cardTaskId && card.historyOffset == null)
  const loadingFirst = Boolean(card?.historyLoading || card?.historyError)
  useEffect(() => {
    if (!cardId || !cardTaskId || !needFirstScreen || loadingFirst) return
    loadAgentHistory(cardId, cardTaskId)
  }, [cardId, cardTaskId, needFirstScreen, loadingFirst, loadAgentHistory])

  const node = chain[chain.length - 1]
  // 数据源：agent 卡的活动记录 → 会话块。后台工具任务没有 items，就把它那一个工具块
  // 直接交给 Narrative（走主对话同一套 ToolRow/ToolGroup 渲染，而不是右栏那套展开体）。
  const subBlocks = useMemo(
    () => (card ? agentItemsToBlocks(card.items) : toolBlock ? [toolBlock] : []),
    [card, toolBlock],
  )

  if (!node) return <div className="empty">{t('agentTree.gone')}</div>
  const running = node.status === 'running' || node.status === 'interrupting'
  const canStop = Boolean(node.taskId) && running
  const usage = card?.usage
  const trimmed = card?.itemsTrimmed ?? 0

  const source: NarrativeSource = {
    blocks: subBlocks,
    // 分页：checkpoint 只留首屏，更早的在子任务 journal 里（agent_history 命令）。
    // 有 trimmed（被裁掉的条数）也算「还有更早」—— 否则裁剪过的历史看起来就是全部。
    total: card?.historyTotal ?? 0,
    hasMore: trimmed > 0 || Boolean(card?.historyHasMore),
    loading: Boolean(card?.historyLoading),
    error: Boolean(card?.historyError),
    ...(card?.taskId ? { onLoadOlder: () => loadAgentHistory(card.id, card.taskId!) } : {}),
    empty: (
      // 点进来是想知道「它在干什么」—— 给状态而不是空白
      <div className="as-placeholder">
        <div className="as-ph-title">{node.label}</div>
        <div className="as-ph-meta mono">
          {statusLabel(node, t)}
          {node.toolName ? ` · ${node.toolName}` : ''}
          {node.taskId ? ` · ${node.taskId}` : ''}
        </div>
        <div className="as-ph-hint">{t('agentTree.noTranscript')}</div>
      </div>
    ),
  }

  const header = (
    <div className="as-hd">
      <span className="as-icon" aria-hidden="true">{node.kind === 'task' ? <Clock size={14} /> : <Bot size={14} />}</span>
      {/* 面包屑：祖先链每一层都可点（递归场景下直接回到上游某一层） */}
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
      {/* 压缩中：与主会话同一个提示形态，只是归属这个子会话（子 agent 压缩自己的上下文） */}
      {comp.active && (
        <span className="as-compressing" role="status" aria-live="polite">
          <span className="composer-status-spinner" aria-hidden="true" />
          <strong>{t('composer.compressing')}</strong>
          {comp.before != null && <span className="composer-status-detail">{t('composer.compressing.detail')}</span>}
        </span>
      )}
    </div>
  )

  // 末行：任务结果 + 本轮产出。主会话里这两块渲染在子智能体**卡内**（TaskResultInline /
  // ArtifactCard），子会话没有卡壳 → 用 footer 插槽放同一批组件，两处仍是同一份实现。
  const footer = card && (card.taskResult || card.taskError || (card.artifacts && card.artifacts.length > 0)) ? (
    <>
      {card.artifacts && card.artifacts.length > 0 && (
        <div className="ac-artifacts">
          <ArtifactCard items={card.artifacts} {...(card.artifactsTruncated ? { truncated: true } : {})} />
        </div>
      )}
      <TaskResultInline delivered={card.deliveredToMain} result={card.taskResult} error={card.taskError} />
    </>
  ) : undefined

  return <Narrative source={source} header={header} footer={footer} />
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
  return n.status ? t(`task.status.${n.status === 'done' ? 'completed' : n.status}`) : t('agentTree.unknown')
}
