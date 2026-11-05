// Agent Hub 前端类型（与 bridge 的 hub_* 命令响应一一对应）。
//
// 只描述「UI 用到的那部分」：profile 在 Go 侧字段很多（工具策略/权限/预算），
// 资料页用到的字段单独写，避免前端复制一份领域模型后与后端漂移。

export interface HubAvatar {
  kind?: string // emoji | image
  value?: string // emoji
  bg?: string
  path?: string
}

export interface HubAgent {
  id: string
  name: string
  avatar?: HubAvatar
  accent?: string
  role?: string
  description?: string
  instructions?: string
  persona?: string
  provider?: string
  model?: string
  effort?: string
  compress_threshold?: number
  memory?: { enabled?: boolean; max_index_lines?: number }
  version?: number
  deleted?: boolean
}

/** hub_agent_list 的 dm 投影：预览/未读（不是 profile 的一部分）。 */
export interface HubDMInfo {
  unread?: number
  last_seq?: number
  last_text?: string
  last_sender?: string
  last_at?: string
}

export interface HubSender {
  kind?: string // human | agent | system
  agent_id?: string
}

export interface HubMessage {
  seq: number
  id?: string
  kind?: string
  sender?: HubSender
  text?: string
  created_at?: string
  refs?: Record<string, unknown>
  /** 前端乐观消息：等待 hub_message 回执（含 client_msg_id）后替换/移除。 */
  pending?: boolean
  clientMsgId?: string
}

export interface HubPage {
  agent?: HubAgent
  session_id?: string
  messages?: HubMessage[]
  has_more?: boolean
  cursor?: number
  exists?: boolean
}

export interface HubHistoryPage {
  messages?: HubMessage[]
  has_more?: boolean
}
