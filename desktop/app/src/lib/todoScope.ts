// 待办归属的**纯判定**（可单测；store 与子会话视图共用同一份规则）。
//
// 背景（用户 2026-10 报障）：子智能体里显示的还是主会话的 TODO。根因是三处各自
// 「一律写会话清单」：bridge 的 extras、reducer 的 meta.Todos、前端的 tool_response 分支。
// 后端已改成「事件自带快照 + 按 run 落档」，前端这边要保证**同一个判定**：
//
//   主 run 的 todo_*  → 会话清单（views[sid].todos）
//   子 run 的 todo_*  → 该子 agent 卡片自己的 todos（blocks 里 kind==='agent' 那一块）
//
// 两条硬性约束（写在这里而不是散在调用点，因为它们最容易在重构中被"顺手统一"掉）：
//   1. 子 run 的调用**绝不**能写会话清单；
//   2. 子 agent 没有清单时**绝不**回退到会话清单（回退会把"它没有计划"显示成
//      "它和主会话有同一份计划"，是更难发现的错）。
// 只做**类型**导入：编译期擦除，不产生 lib → store 的运行时依赖（agentTree.ts 同一约定）。
import type { TodoItem } from '../store/useAppStore'

/** 一条 todo_* 响应的归属目标 */
export type MessageBlockTodoTarget = { kind: 'session' } | { kind: 'agent'; runId: string }

/** 归属判定：这条 todo_* 响应该落到哪儿。 */
export function todoTargetOf(isSub: boolean, runId: string | undefined): MessageBlockTodoTarget {
  if (isSub && runId) return { kind: 'agent', runId }
  return { kind: 'session' }
}

/**
 * pickTodoList 取某个作用域**此刻**该显示的清单。
 *
 * @param scope       'main' = 会话；'agent' = 某个子 agent
 * @param sessionList 会话清单（scope='agent' 时仅用于对比，不参与返回）
 * @param agentList   该子 agent 自己的清单（undefined = 没有 —— 返回空数组，不回退）
 */
export function pickTodoList(
  scope: 'main' | 'agent',
  sessionList: TodoItem[],
  agentList?: TodoItem[],
): TodoItem[] {
  if (scope === 'main') return sessionList
  return agentList ?? []
}

/**
 * withAgentTodos 把清单写到目标 agent 卡片上（其它块原样返回同一个引用）。
 *
 * 返回**同一个数组引用**时调用方可以据此跳过 setState（本仓的流式性能约定：
 * 无变化不触发重渲）。因此这里用 identity 比较而不是无条件 map。
 */
export function withAgentTodos<T extends { kind: string; runId?: string; todos?: TodoItem[] }>(
  blocks: T[],
  runId: string,
  todos: TodoItem[],
): T[] {
  let changed = false
  const out = blocks.map((b) => {
    if (b.kind !== 'agent' || b.runId !== runId) return b
    changed = true
    return { ...b, todos }
  })
  return changed ? out : blocks
}
