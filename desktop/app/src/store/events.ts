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
