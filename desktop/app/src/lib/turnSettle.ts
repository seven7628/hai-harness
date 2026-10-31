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
//
// ## runEnded 必须压过 turnRunning（用户 2026-10 报障）
//
// 上一版把 `turnRunning` 放在最前面，于是「块上还有 running」永远算在跑。可 AgentStart
// →AgentEnd 之间派生的**后台任务**（agent_spawn 的异步子 agent / 被 promote 的工具）
// 在主 run 交卷之后仍然 running：卡片状态一直是 running，折叠行就永远不出现 ——
// 主 run 8 秒结束、后台任务跑 20 分钟，界面就 Working 20 分钟，时钟一路涨，
// Worked for 与轮尾账永远不结算。
//
// AgentEnd 是**这次运行**的交卷时刻，后台任务的后续属于它自己的生命周期（完成时
// 落一条 task_delivery 记录在时间线上，见 selectTurns.ALWAYS_SHOWN）。所以：
// 已交卷 → 一律折叠，块上的 running 不得把它拉回来。
export function turnLive(a: { isLast: boolean; turnRunning: boolean; runEnded: boolean }): boolean {
  if (a.runEnded) return false
  return a.turnRunning || a.isLast
}
