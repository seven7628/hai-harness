import { useMemo } from 'react'
import { useHubStore } from './useHubStore'
import { resolveAgentState, type AvatarState } from './avatar'

// 头像状态的数据映射（方案 §3.3.4）：running / needs.pending / agent.deleted → 四态。
// 放在 store 与纯函数之间的一层薄 hook：组件只关心「这个 agent 现在什么状态」。
//
// 选择器只取布尔值（不是整个 running 对象），长消息列表里的头像不会因为别人的
// 状态变化而整片重渲染。

export function useAgentState(agentId?: string | null, deleted?: boolean): AvatarState {
  const running = useHubStore((s) => (agentId ? s.running[agentId] === true : false))
  const pending = useHubStore((s) =>
    agentId ? s.needs.some((n) => n.agent_id === agentId && n.status === 'pending') : false,
  )
  return resolveAgentState({ running, pending, deleted })
}

/** needs 里所有待处理 agent 的 id 集合：群叠放/成员列表逐成员判定时共用一个订阅。 */
export function usePendingAgents(): ReadonlySet<string> {
  const needs = useHubStore((s) => s.needs)
  return useMemo(() => {
    const ids = new Set<string>()
    for (const n of needs) {
      if (n.status === 'pending' && n.agent_id) ids.add(n.agent_id)
    }
    return ids
  }, [needs])
}
