// 子智能体「运行时长」的取值口径 —— 子会话页顶部那一行（状态 · 用量 · 时长）。
//
// 用户 2026-10 报障：子智能体的运行时长显示的是主智能体的。数据本身是干净的
//（实测 8 个真实会话：没有任何一张子 agent 卡的 dur_ms 等于主 run 的 dur_ms），
// 所以问题出在**取源**与**状态**：此前只看卡片 durMs + 节点 status，
//   · 卡片没加载出来（checkpoint 裁过 / 历史还没拉回）→ 时长整段消失；
//   · 节点状态停在 running（占位先建、状态只在首次合并时写入）→ 计时器一直往上走，
//     读出来就是「一个跟主会话同量级的大数」。
// 这里给一条确定的优先级：卡片结算 → 节点结算 → 该 run 最后一条 item 的整轮耗时；
// 三者都没有才认为它还在跑（此时才用 startedAt 走实时计时）。
//
// 单位统一为毫秒。0 视同「没有」——Go 侧 omitempty 落盘的 0 不代表「跑了 0 毫秒」。

export interface AgentDurationInput {
  status?: string
  /** 卡片结算（agent_end.duration_ms 落盘） */
  cardDurMs?: number
  /** run 节点结算（Go 侧 runNodes[].dur_ms） */
  nodeDurMs?: number
  /** 该 run 最后一条 item 上挂的整轮耗时（agent_end 的 agentDurMs） */
  lastItemAgentDurMs?: number
}

export interface AgentDuration {
  /** 是否仍在运行（决定时长是「实时走」还是「结算值」） */
  running: boolean
  /** 结算后的时长；undefined = 还不知道 */
  durMs?: number
}

export function agentDurationOf(input: AgentDurationInput): AgentDuration {
  const pick = [input.cardDurMs, input.nodeDurMs, input.lastItemAgentDurMs].find((v) => typeof v === 'number' && v > 0)
  if (pick != null) return { running: false, durMs: pick }
  return { running: input.status === 'running' || input.status === 'interrupting' }
}
