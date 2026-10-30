// 「这一轮跑完了没有」—— 折叠行（Worked for）与轮尾账（tokens/cache/wall）只在
// **AgentEnd 之后**才允许出现。这两样都是**结算**，不是进度：边跑边刷就等于报一个还没定的数。
//
// ## 为什么不能只靠 run 节点的 status（用户 2026-10 报障）
//
// 之前的判据是 `turn.running || (最后一轮 && mainRunActive(view))`：
//   · `turn.running` 只看「此刻有没有东西在流/在跑」—— llm_end 与下一次 llm_start、
//     工具结束与下一次工具开始之间，它是 false；
//   · `mainRunActive` 依赖 run 节点（agent_start 建的）。只要那条节点在某条路径上缺失
//     （恢复视图、事件重放、子 run 顶掉顶层节点），它也是 false。
// 两个 false 一叠加，最后一轮就会在**中途**被判成「已结算」：Worked for 折叠行与
// tokens 那一行提前出现，然后下一条事件又把它拆掉（用户看到的就是「llm_end 时就折叠了」）。
//
// 所以判据换成**显式事实**：runEnded 只在收到主 run 的 AgentEnd 时置 true，
// 收到 AgentStart / 用户再次发送时置 false。缺少这个事实时按「没结束」处理 ——
// 宁可晚一步结算，也不要在跑的时候先报一个结论。
export function turnLive(a: { isLast: boolean; turnRunning: boolean; runEnded: boolean }): boolean {
  if (a.turnRunning) return true
  return a.isLast && !a.runEnded
}
