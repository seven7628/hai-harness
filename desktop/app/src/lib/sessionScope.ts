// sessionScope.ts —— 「当前在看谁 → 该显示谁的 HITL」的**纯判定**（无 React / 无 store）。
//
// 单独的 lib 文件是为了能单测（见 sessionScope-test.ts）：这条判定错一次的表现是
// 「面板根本没弹出来」，而它在类型上完全合法 —— 只有断言能钉住。
//
// ## 为什么不能按「run_id 有没有值」判主/子
//
// 第一版就是这么写的，结果是**主会话的 ask_user 和审批全部消失**（2026-10 真机报障）。
// 真实事件里主 run 也带 runId：
//   · tools/question/question.go → AskUserQuestion.RunId = tc.RunId
//   · tools/engine.go            → ToolApprovalRequested.RunId = tc.RunId
// 「是不是子 agent」只有两个可信判据（与 store.isSubAgentRun 同义）：
//   ① 有该 runId 的 agent 卡片；② runNode 带 parentId（卡片晚到时的兜底）。
//
// ## 作用域语义
//   · scopeRunId === undefined → 看主会话：显示**所有不属于任何子 run** 的条目
//     （含完全没有 run_id 的历史事件 —— 它们按主 Agent 处理）
//   · scopeRunId === 'sub-x'   → 看子会话：只显示 runId === 'sub-x' 的条目
//
// 审批队列在后端是 Session 级单 FIFO（决策锚是调用 id 而不是运行 id），所以这里是
// **展示过滤**，不拆队列、不重排 —— 过滤后仍保持后端的到达顺序。
import type { Approval, PendingQuestion, SessionView } from '../store/useAppStore'

/** 判定所需的视图投影（只取用到的那两列，便于单测构造最小形状）。 */
export interface ScopeView {
  blocks: readonly { kind: string; runId?: string }[]
  runNodes: readonly { kind: string; id: string; parentId?: string }[]
}

/**
 * 该 run 是不是**子 agent**（不是主会话）。
 *
 * 与 store.isSubAgentRun 同义（那边直接调这里，避免两份判据漂移）。
 * 空 runId → false：「没有 run_id 的旧事件继续按主 Agent 处理」。
 */
export function looksSubRun(v: ScopeView | undefined, runId?: string): boolean {
  if (!runId) return false
  if (!v) return false
  if (v.blocks.some((b) => b.kind === 'agent' && b.runId === runId)) return true
  // 卡片可能晚于审批/提问事件到达（事件重排、旧日志回放）→ 用 runNode 的父子关系兜底
  return v.runNodes.some((n) => n.kind === 'run' && n.id === runId && Boolean(n.parentId))
}

/** 某条目是否属于当前作用域。 */
function inScope(v: ScopeView | undefined, itemRunId: string | undefined, scopeRunId?: string): boolean {
  if (scopeRunId) return (itemRunId ?? '') === scopeRunId
  // 主会话：主 run 的事件（含无 run_id 的历史事件）都算；子 run 的都不算
  return !looksSubRun(v, itemRunId)
}

/** 把审批队列过滤到当前作用域（保持后端 FIFO 顺序）。 */
export function approvalsInScope(v: ScopeView | undefined, list: Approval[], scopeRunId?: string): Approval[] {
  return list.filter((a) => inScope(v, a.runId, scopeRunId))
}

/**
 * 属于当前作用域的待答问题（多批并存 → 取**最早到达**的那批）。
 *
 * 为什么是数组：ask_user 的等待器在后端是 Session 级单实例，但可以**同时有多批在等**
 * （主 run 问一句、子 agent 也问一句）。store 若只留一条，后到的会覆盖先到的 ——
 * 表现是「主会话正在等答的那批凭空消失」。归属按 runId，显示在这里按作用域过滤。
 */
export function questionInScope(
  v: ScopeView | undefined,
  list: readonly PendingQuestion[],
  scopeRunId?: string,
): PendingQuestion | undefined {
  return list.find((q) => inScope(v, q.runId, scopeRunId))
}

/** 会话视图里的可变形状（组件侧传进来的就是它）。 */
export type ScopeSessionView = Pick<SessionView, 'blocks' | 'runNodes'>
