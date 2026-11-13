// 事件协议类型：bridge stdout 每行一个 JSON，判别器 = event_type（可扩展，未知字段忽略）
export interface AnyEvent {
  event_type: string
  session_id?: string
  [k: string]: unknown
}

export interface Usage {
  Input?: number
  Output?: number
  CacheRead?: number
  CacheWrite?: number
  Reasoning?: number
  TotalTokens?: number
  [k: string]: unknown
}

// 命令协议：renderer → bridge stdin
export interface BridgeCommand {
  id: number
  type: string
  payload?: Record<string, unknown>
}

export interface CommandResponse {
  event_type: 'command_response'
  id: number
  ok: boolean
  error?: string
  code?: string // 稳定错误码（V2 P2-PROTOCOL-05）：认证/超时/配置/限流/not_found 等
  data?: Record<string, unknown>
}

// —— 事件信封：运行位收敛的诊断（BR-15） ——
//
// 运行位的「结束」本应只有一条证据：同一 run 的 agent_end。事件可能丢（stdout 行被
// 截断 / 传输中断 / 单条 dispatch 异常被跳过），于是**别的终止事实**到达时必须把残留
// 的 running 收敛掉，否则左栏运行点、右栏角标、停止按钮会永久亮着（用户报障：
// 「丢一条 agent_end 就永久运行中」）。
//
// 本模块只做**有界记录**：哪一次运行是被哪条事件收敛的。状态改写仍在 store（见
// useAppStore 的 settleRunNodes），诊断不改变任何行为；生产代码从不读它。
export type RunSettleSource =
  | 'agent_end' // 主 run 收尾时顺带收敛了别的运行：同步子 run（阻塞调用，随父 run 结束；
  //             状态 aborted?interrupted:done，不继承父 run 的 finish_reason），
  //             或 agent_end 缺 run_id 时的那个唯一顶层 run（无法排除它自己）。
  | 'agent_end_unmatched' // 收到一条 run_id 找不到节点的 agent_end（只记录，不猜归属）
  | 'new_run_batch' // 新批次开口（user_inputs_consumed injected=false）证明上一轮已结束
  //                   —— 上一轮整个 run 树，含父 run 里的同步子 run
  | 'session_run_error' // 运行级失败（Go 侧只在主 run 非 abort 失败时发，会话级终态）
  //                      —— 同样连带父 run 里的同步子 run
  | 'session_closed' // 会话优雅关闭（运行已停）
  | 'task_end' // 后台任务终态（该任务的子运行随任务结束）

export interface RunSettleRecord {
  sessionId: string
  source: RunSettleSource
  /** 被收敛的 run 节点 id（不含触发本次收敛的事件自己的 run；可为空 = 只记录未收敛）。
   *  例外：`agent_end` 缺 `run_id` 时无法排除「它自己」，那个唯一顶层 run 也在列表里。 */
  runIds: string[]
  /** 事件携带的 run_id（诊断用，可能为空）。 */
  eventRunId?: string
  taskId?: string
  /** 事件时间戳（ms；不是记录时刻）。 */
  at: number
}

const RUN_SETTLE_MAX = 50
const runSettles: RunSettleRecord[] = []

export function recordRunSettle(r: RunSettleRecord): void {
  runSettles.push(r)
  if (runSettles.length > RUN_SETTLE_MAX) runSettles.splice(0, runSettles.length - RUN_SETTLE_MAX)
}

export function runSettleTraces(): readonly RunSettleRecord[] {
  return runSettles
}

export function clearRunSettleTraces(): void {
  runSettles.length = 0
}
