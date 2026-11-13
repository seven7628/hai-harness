import { getTransport } from '../transport'
import type { BridgeCommand } from '../store/events'

// Hub 命令出入口。
//
// 为什么不复用 store 的 send()：那个 send 会注入当前**工作区**并登记 pending 表
// （按 workspace/session 分发响应）。Hub 命令按 target/agent_id 寻址，注入项目
// workspace 只会让 bridge 的日志看起来像在项目里操作；而且两套 pending 混在一起，
// command_response 会被主 store 当成自己的工作区响应去处理。
//
// id 从高位起：主 store 的 cmdSeq 从 1 递增，两条通道共用同一个 command_response
// 事件流，id 撞车会串响应（例如 hub_open 的 agent 被当成 new_session 的会话）。
const HUB_CMD_BASE = 5_000_000
let hubCmdSeq = HUB_CMD_BASE

export interface HubResult<T> {
  ok: boolean
  error?: string
  code?: string
  data?: T
}

export function hubCommand<T>(type: string, payload: Record<string, unknown>, timeoutMs = 20_000): Promise<HubResult<T>> {
  const transport = getTransport()
  const id = ++hubCmdSeq
  return new Promise<HubResult<T>>((resolve) => {
    let settled = false
    const off = transport.onEvent((line) => {
      let ev: { event_type?: string; id?: number; ok?: boolean; error?: string; code?: string; data?: unknown }
      try {
        ev = JSON.parse(line) as typeof ev
      } catch {
        return // 非 JSON 事件（流式异常行）忽略
      }
      if (ev.event_type !== 'command_response' || ev.id !== id) return
      finish({ ok: ev.ok === true, error: ev.error, code: ev.code, data: ev.data as T })
    })
    const timer = setTimeout(() => finish({ ok: false, error: 'bridge 响应超时' }), timeoutMs)
    function finish(r: HubResult<T>): void {
      if (settled) return
      settled = true
      off()
      clearTimeout(timer)
      resolve(r)
    }
    void transport.send({ id, type, payload } as BridgeCommand)
  })
}
