import { useAppStore } from '../store/useAppStore'
import type { TodoItem } from '../store/useAppStore'
import { pickTodoList } from '../lib/todoScope'

// useFocusTodos：待办面板的**作用域解析**（右栏与子会话视图共用同一份判定）。
//
// 返回 undefined = 看的是主会话（调用方用会话清单 store.todos）；
// 返回 { scope:'agent', todos } = 看的是某个子智能体，用**它自己的**清单。
//
// 为什么单独抽出来：这条判定同时决定 ①面板显示谁的清单 ②角标数字 ③子会话视图的清单。
// 三处各写一遍必然漂移，而漂移的表现是「数字和列表不一致」—— 用户会先怀疑数据坏了。
// 子 agent 没有清单时返回空数组而**不是**回退到主会话：回退会让「它没有计划」看起来像
// 「它和主会话有同一份计划」，是更难发现的错（用户 2026-10 报障的根因正是无条件回退）。
export function useFocusTodos(): { scope: 'agent'; todos: TodoItem[] } | undefined {
  const focused = useAppStore((s) => s.focusedAgent)
  const blocks = useAppStore((s) => s.blocks)
  if (!focused) return undefined
  // 按真实 runId 找卡片（focused.nodeId 是树节点身份，可能是 taskId）
  const card = blocks.find((b) => b.kind === 'agent' && b.runId === focused.runId)
  // pickTodoList 保证「没有清单」= 空数组，而不是回退到主会话清单
  return { scope: 'agent', todos: pickTodoList('agent', [], card?.kind === 'agent' ? card.todos : undefined) }
}
