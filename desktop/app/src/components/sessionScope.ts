import { useAppStore } from '../store/useAppStore'
import type { Approval, PendingQuestion, SessionView, MsgBlock } from '../store/useAppStore'

// 会话分域（2026-10）。
//
// 用户的定义（拍板原话）：「子智能体也有子智能体的待审批、会话压缩信息；所有处理全部跟
// 主 session 其实是一样的了；就等同于切换个 session，只不过这是个子 session。多的功能
// 就是子智能体的任务结果同样会回推到主智能体。」
//
// 于是「当前要看哪一份 HITL / 压缩」不是两条代码路径，而是**一个作用域判定**：
//   · 看主会话 → runId 缺省（主 run 的事件不带 parent）
//   · 看子会话 → 该子 agent 的 runId
// 两个面板（ApprovalPanel / AskUserQuestionPanel）与压缩镜像都走这里，判定只写一次 ——
// 分开写必然出现「审批按子会话过滤、压缩忘了一起改」这种半对状态。
//
// **一个必须保留的例外**：审批队列在**后端是 Session 级单 FIFO**（主/子共用，
// 见 session/approval.go —— 决策锚是调用 id，不是运行 id）。所以这里做的是**展示过滤**，
// 不是队列拆分：子会话只看到属于自己的那条，但队列本身仍是一个，dequeue 语义不变。
// 这也是为什么「没有任何审批属于当前作用域」时面板要**隐藏**而不是显示空态 ——
// 用户不需要为「别人的审批还没处理」看到一个空框。

/** 当前查看的作用域：undefined = 主会话；runId = 某个子会话 */
export function useScopeRunId(): string | undefined {
  return useAppStore((s) => s.focusedAgent?.runId)
}

/** 把审批队列过滤到当前作用域（保持后端 FIFO 顺序）。 */
export function approvalsInScope(list: Approval[], scopeRunId?: string): Approval[] {
  return list.filter((a) => (a.runId ?? '') === (scopeRunId ?? ''))
}

/** 待答问题是否属于当前作用域（ask_user 同样是 Session 级的单槽）。 */
export function questionInScope(q: PendingQuestion | undefined, scopeRunId?: string): PendingQuestion | undefined {
  if (!q) return undefined
  return (q.runId ?? '') === (scopeRunId ?? '') ? q : undefined
}

/** 当前作用域的队首审批（undefined = 没有要你决定的事）。 */
export function useScopedApproval(): Approval | undefined {
  const list = useAppStore((s) => s.approvals)
  const scope = useScopeRunId()
  // 每次渲染都算一次过滤（列表通常 0~2 条，不值得引入浅比较 selector 的复杂度）
  return approvalsInScope(list, scope)[0]
}

/** 当前作用域的待答问题。 */
export function useScopedQuestion(): PendingQuestion | undefined {
  const q = useAppStore((s) => s.question)
  const scope = useScopeRunId()
  return questionInScope(q, scope)
}

/**
 * 当前作用域的压缩态（主会话 = 会话级活动字段；子会话 = 它自己卡片上的 compression 项）。
 *
 * 返回统一形态，让「压缩中」这条提示在两个作用域里长得一样：
 *   · active = 正在压缩（有开始时间就带 startedAt，UI 可走计时）
 *   · last   = 最近一次**已完成**的压缩块（折叠展示摘要/用量）
 */
export interface CompressionScope {
  active: boolean
  startedAt?: number
  before?: number
  last?: Extract<MsgBlock, { kind: 'compression' }>
}

export function compressionInScope(v: SessionView | undefined, scopeRunId?: string): CompressionScope {
  if (!v) return { active: false }
  if (!scopeRunId) {
    const last = [...v.blocks].reverse().find((b): b is Extract<MsgBlock, { kind: 'compression' }> => b.kind === 'compression')
    return {
      active: v.compressing,
      startedAt: v.compressStartTs,
      before: v.compressBefore,
      last,
    }
  }
  // 子会话：压缩是它卡片里的一个 item（子 agent 有自己的上下文，压缩也归它自己）
  const card = v.blocks.find((b) => b.kind === 'agent' && b.runId === scopeRunId)
  if (card?.kind !== 'agent') return { active: false }
  const items = card.items.filter((it) => it.kind === 'compression')
  const activeItem = [...items].reverse().find((it) => it.kind === 'compression' && it.active)
  return {
    active: Boolean(activeItem) || Boolean(card.compressing),
    startedAt: activeItem?.kind === 'compression' ? activeItem.startedAt : card.compressStartTs,
    before: activeItem?.kind === 'compression' ? activeItem.before : undefined,
    // 子会话的「最近一次压缩」不是会话级 block，而是它卡片里的一个 item ——
    // 这里**不伪造**一个 compression block（那会让两处渲染读不同形状的同一事实）；
    // 子会话视图直接从 card.items 渲染，见 AgentSessionView。
  }
}

/** 当前作用域的压缩态（组件用）。 */
export function useScopedCompression(): CompressionScope {
  const view = useAppStore((s) => (s.activeSessionId ? s.views[s.activeSessionId] : undefined))
  const scope = useScopeRunId()
  return compressionInScope(view, scope)
}

/**
 * useScopedRunCard 当前作用域对应的子 agent 卡片（undefined = 主会话 / 节点还没卡片）。
 * 子会话面板需要它取「该子 agent 的压缩 item / 用量 / 名字」。
 */
export function useScopedRunCard() {
  const view = useAppStore((s) => (s.activeSessionId ? s.views[s.activeSessionId] : undefined))
  const scope = useScopeRunId()
  if (!view || !scope) return undefined
  return view.blocks.find((b) => b.kind === 'agent' && b.runId === scope)
}
