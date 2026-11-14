import { memo, useEffect, useMemo } from 'react'
import { useAppStore } from '../store/useAppStore'
import type { AgentFocus, MsgBlock } from '../store/useAppStore'
import { agentTreeFromView, pathOfNode } from '../lib/agentTree'
import type { AgentNodeStatus, AgentTreeNode } from '../lib/agentTree'
import { agentItemsToBlocks } from '../lib/agentSession'
import { childrenOf, placeChildCards } from '../lib/nestedCards'
import { TaskErrorInline } from './NarrativeBlocks'
import Narrative from './Narrative'
import type { NarrativeSource } from './Narrative'
import { ElapsedTimer } from './ElapsedTimer'
import { agentDurationOf } from '../lib/agentDuration'
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
// ## 仍然是「视图」，但输入/停止/计时都按子会话作用域
//
// 用户 2026-11 报障：子会话里发消息没反应、停止会停主会话、计时还是主会话的。
// 结论：不是「子会话没有输入框」，而是 Composer 的作用域没隔离 —— 输入走 agent_send，
// 停止走 interrupt_task，运行条/压缩态读当前子 run。这里只负责叙述区；Composer 的
// 作用域在 components/Composer.tsx 统一接线（同一个组件，不复制第二套）。
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
  // 卡片按**真实 runId** 找；退化到 taskId（agent 卡片先到、runNode 已收敛的恢复场景）。
  //
  // 关键：agent_resume 原地续跑复用同一个 task_id、但每轮都是**新的 run_id**，而卡片
  // 以 run 为单位 → 同一个「子智能体」会产出多张卡。子会话页是「这个任务」的视图，
  // 必须把同 task 的所有轮按时间串起来；否则续跑后打开只有最新一轮（用户 2026-11-04
  // 报障「A 结束后恢复继续运行，此前的会话信息看不到了」）。
  const cards = useMemo(() => {
    const node = chain[chain.length - 1]
    const taskKey = node?.taskId
    const sameTask = allBlocks.filter(
      (b): b is Extract<MsgBlock, { kind: 'agent' }> =>
        b.kind === 'agent' && Boolean(taskKey) && b.taskId === taskKey,
    )
    if (sameTask.length > 0) {
      // 时间序（缺时间垫底）→ 最后一轮是「当前」卡（状态/时长/用量以它为准）
      return [...sameTask].sort((a, b) => (a.spawnedAt ?? 0) - (b.spawnedAt ?? 0))
    }
    const one = allBlocks.find(
      (b): b is Extract<MsgBlock, { kind: 'agent' }> => b.kind === 'agent' && Boolean(runId) && b.runId === runId,
    )
    return one ? [one] : []
  }, [allBlocks, runId, chain])
  // 当前（最新一轮）卡片：头部状态/时长/用量、以及「能不能停止」都以它为准
  const card = cards.length > 0 ? cards[cards.length - 1] : undefined
  // 最早一轮卡片：分页「加载更早」必须挂在它身上 —— 早先的页会被 prepend 到它前面，
  // 视觉上正好接在合并后转录的开头（挂最新卡会让更早的内容插到中间）。
  const oldestCard = cards.length > 0 ? cards[0] : undefined

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
  const cardId = oldestCard?.id
  const cardTaskId = oldestCard?.taskId
  const mergedItemCount = cards.reduce((n, c) => n + c.items.length, 0)
  const needFirstScreen = Boolean(cardId && cardTaskId && mergedItemCount === 0 && oldestCard?.historyOffset == null)
  const loadingFirst = Boolean(oldestCard?.historyLoading || oldestCard?.historyError)
  useEffect(() => {
    if (!cardId || !cardTaskId || !needFirstScreen || loadingFirst) return
    loadAgentHistory(cardId, cardTaskId)
  }, [cardId, cardTaskId, needFirstScreen, loadingFirst, loadAgentHistory])

  const node = chain[chain.length - 1]
  /* 它派发的孙任务：主转录只画本层（见 lib/nestedCards），所以那些卡**在这一页**出现。
     复用 agent 块（runtime 皮肤 = 车道行，其余皮肤 = 任务卡）—— 状态/停止/展开/重试
     全走同一套渲染，不为「嵌套」再造第二种卡片形态。
     落点由 placeChildCards 决定：派发工具行 → 时间锚 → 页尾（三道兜底，见那边的注释）。 */
  const nestedCards = useMemo(() => childrenOf(allBlocks, node?.taskId), [allBlocks, node])
  // 数据源：agent 卡的活动记录 → 会话块。后台工具任务没有 items，就把它那一个工具块
  // 直接交给 Narrative（走主对话同一套 ToolRow/ToolGroup 渲染，而不是右栏那套展开体）。
  const mergedItems = useMemo(() => cards.flatMap((c) => c.items), [cards])
  const subBlocks = useMemo(() => {
    if (!card) return toolBlock ? [toolBlock] : []
    // 按卡转换，而不是先把 items 摊平：每张卡 = 一次运行（agent_resume 每轮一张），
    // 卡自己的 runId 作为旧 record 的兜底 —— 合并后 turnRuns 才能按 run 切 Worked 盒。
    const blocks: MsgBlock[] = []
    let nextId = -1
    for (const c of cards) {
      blocks.push(...agentItemsToBlocks(c.items, { fallbackRunId: c.runId, nextId: () => nextId-- }))
    }
    // 产出：走**主会话的同一批渲染块**（Narrative 的 artifacts 分支），而不是卡片内的
    // .ac-artifacts。
    //
    // 后者是「卡片内附件」的形态：带 border-top 与卡片内的边距，放进叙述区会明显矮一截、
    // 贴不住正文 —— 用户 2026-10 报障「子智能体里没有 artifacts」，除了数据没挂上卡片，
    // 另一层就是它长得不像主会话那张产出卡。
    //
    // 任务结果不再做成页脚卡：它已经就是转录最后一条 assistant 正文；主会话的交付行
    // 负责「Task … completed」通知。
    //
    // id 用远离 agentItemsToBlocks 的负数区间（那边从 -1 递减），避免 React key 撞车。
    // 孙任务卡用另一个更远的区间（-2_000_000 起递减）：原块的 id 是会话级的，两处同时渲染
    // 会撞 key（表现是「展开了这个子任务，另一个跟着展开」）。
    let nestedId = -2_000_000
    const withChildren = placeChildCards(blocks, nestedCards, () => nestedId--)
    if (card.artifacts && card.artifacts.length > 0) {
      withChildren.push({
        kind: 'artifacts', id: -1_000_000, items: card.artifacts,
        ...(card.artifactsTruncated ? { truncated: true } : {}),
      })
    }
    return withChildren
  }, [card, cards, toolBlock, nestedCards])

  if (!node) return <div className="empty">{t('agentTree.gone')}</div>
  // 时长口径收在一处（lib/agentDuration.ts）：卡片结算 → 节点结算 → 该 run 末条 item 的整轮耗时；
  // 三者都没有才算「还在跑」，用 spawnedAt 走实时计时。此前只看卡片 durMs + 节点 status：
  // 卡片没加载出来时长整段消失，节点状态停在 running 时计时器会一直往上走（用户 2026-10
  // 报障「子智能体的运行时长显示的是主智能体的」）。
  const lastItemDur = (() => {
    const items = mergedItems
    const last = items && items.length ? items[items.length - 1] : undefined
    // agentDurMs 只挂在文本项（kind='text'）与思考项上（工具项没有这个字段）
    return last && (last.kind === 'text' || last.kind === 'thinking') ? last.agentDurMs : undefined
  })()
  const dur = agentDurationOf({
    status: node.status,
    cardStatus: card?.status,
    cardDurMs: card?.durMs,
    nodeDurMs: node.durMs,
    lastItemAgentDurMs: lastItemDur,
  })
  const running = dur.running
  // 当前 run 的三元组：卡片是直接证据（续跑会换 run_id），节点只做兜底。
  // 头部状态、停止锚点、Narrative 的运行态必须同源，否则会出现「卡片 running、
  // 头部 Worked、停止按钮消失」这种半对状态。
  const currentStatus = (card?.status as AgentNodeStatus | undefined) ?? node.status
  const currentRunId = card?.runId ?? node.runId
  const currentTaskId = card?.taskId ?? node.taskId
  const currentStartedAt = card?.spawnedAt ?? node.startedAt
  const canStop = Boolean(currentTaskId) && running
  const usage = card?.usage
  const trimmed = cards.reduce((n, c) => n + (c.itemsTrimmed ?? 0), 0)
  // 「还有更早」的判据：**有 task_id 就有 journal** —— 续跑轮的卡片 items 非空、trimmed=0，
  // 旧判据连按钮都不给，用户无路可走（问题二）。但一旦后端给了权威答案（historyHasMore），
  // 就听它的：否则翻到底之后按钮永远不消失。
  //
  // 分页三态（loading/error/total/hasMore）必须读**最早那张卡** —— onLoadOlder 与
  // 响应回写都锚在 oldestCard 上。旧实现读 `card`（最新一轮那张），于是：
  // 请求挂在旧卡、loading 看新卡 → 永远 false → Narrative 的滚动预取/按钮完全不节流，
  // 一次上滑就能连发多个分页请求（2026-11「3 个一样的消息」的放大器）。
  const pager = oldestCard ?? card
  const historyAnswered = pager?.historyHasMore !== undefined
  const hasMoreHistory = historyAnswered
    ? pager?.historyHasMore === true
    // 有 task_id 通常意味着有 journal，但如果后端已经报了 total=0，就不要显示
    // 「加载更早（还有 0 条）」这种自相矛盾的入口。
    : trimmed > 0 || (pager?.historyTotal == null
      ? Boolean(oldestCard?.taskId)
      : pager.historyTotal > 0)

  const source: NarrativeSource = {
    blocks: subBlocks,
    // 分页：checkpoint 只留首屏，更早的在子任务 journal 里（agent_history 命令）。
    // 有 trimmed（被裁掉的条数）也算「还有更早」—— 否则裁剪过的历史看起来就是全部。
    total: pager?.historyTotal ?? 0,
    hasMore: hasMoreHistory,
    loading: Boolean(pager?.historyLoading),
    error: Boolean(pager?.historyError),
    ...(oldestCard?.taskId ? { onLoadOlder: () => loadAgentHistory(oldestCard.id, oldestCard.taskId!) } : {}),
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
      <span className={`as-st mono st-${statusCls(currentStatus)}`}>{statusLabel({ ...node, status: currentStatus }, t)}</span>
      {usage && (usage.input > 0 || usage.output > 0) && (
        <UsageChip
          className="as-usage mono"
          usage={usage}
          title={t('task.usage.tip')
            .replace('{tok}', String(usage.input + usage.output))
            .replace('{cost}', fmtUsd(usage.costUsd ?? 0))}
        />
      )}
      {(running || dur.durMs != null) && (
        <ElapsedTimer className="as-dur mono" startAt={currentStartedAt} running={running} durMs={dur.durMs} />
      )}
      {canStop && (
        <button
          className="as-stop"
          title={t('rp.task.stopTask')}
          aria-label={t('rp.task.stopTaskAria')}
          onClick={() => interruptTask(currentTaskId!)}
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

  // 任务级错误仍要可见；结果不再重复（它已经是转录最后一条 assistant 正文）。
  const errorFooter = card?.taskError ? <TaskErrorInline error={card.taskError} /> : undefined

  // 运行态是**这个子 agent 自己的**（node.status / startedAt）：主 agent 可能早就交卷了，
  // 拿主会话的 runEnded 判它，子会话第一屏就会是 Worked for（用户 2026-10 报障）。
  return (
    <Narrative
      source={source}
      header={header}
      footer={errorFooter}
      runState={{ live: running, startedAt: currentStartedAt, ...(currentRunId ? { runId: currentRunId } : {}) }}
      /* 转录身份：子 agent 的 run_id（后台任务节点退化为 task_id）。滚动容器按它重建 ——
         从 A 切到 B 是新的一段转录，不是同一段滚到别处。 */
      sourceKey={currentRunId ?? currentTaskId ?? node.id}
    />
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
  return n.status ? t(`task.status.${n.status === 'done' ? 'completed' : n.status}`) : t('agentTree.unknown')
}
