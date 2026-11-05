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
  tools?: { preset?: string; disable?: string[]; allow?: string[]; bash_roots?: string[]; disable_network?: boolean }
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
  /** 发送失败（可重试）：气泡保留在列表里，原文同时进 store.failedText。 */
  failed?: boolean
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

/** 任务账本条目（hub_task_list/get；字段与 agenthub.Task 对齐，只列 UI 用的）。 */
export interface HubTaskUsage {
  input?: number
  output?: number
  cache_read?: number
  cache_write?: number
  reasoning?: number
  cost_priced?: boolean
  cost_usd?: number
}

export interface HubTaskArtifact {
  name?: string
  path?: string
  kind?: string
  size?: number
  lines?: number
}

export interface HubTaskResult {
  summary?: string
  error?: string
  finish_reason?: string
  usage?: HubTaskUsage
  artifacts?: HubTaskArtifact[]
}

export interface HubTask {
  id: string
  root_task_id?: string
  parent_task_id?: string
  title?: string
  instruction?: string
  status?: string
  attempt?: number
  error?: string
  origin?: { kind?: string; agent_id?: string; agent_name?: string }
  target?: { kind?: string; workspace?: string; session_id?: string }
  result?: HubTaskResult
  created_at?: string
  updated_at?: string
}

/** Needs you（审批/提问/失败/交接/预算）。 */
export interface HubNeed {
  id: string
  kind?: string
  status?: string
  agent_id?: string
  task_id?: string
  session_id?: string
  workspace?: string
  anchor?: string
  title?: string
  body?: string
  created_at?: string
  resolved_at?: string
}

/** Run 视图的一行（工具起止 / 压缩 / 状态）；来自 agent_* 与会话事件。 */
export interface HubRunEntry {
  id: string
  kind: 'tool' | 'status' | 'compress'
  name?: string
  status?: string
  summary?: string
  ms?: number
  at: number
}

/** 群组成员/策略/群头（hub_group_list/get）。 */
export interface HubGroupMember {
  agent_id: string
  role?: string
  joined_at?: string
}

export interface HubGroupPolicy {
  mode?: string
  max_parallel_runs?: number
  max_handoff_depth?: number
  max_turns_per_human?: number
  max_mentions_per_message?: number
  daily_budget_usd?: number
}

export interface HubGroup {
  id: string
  name: string
  owner_agent_id?: string
  members?: HubGroupMember[]
  policy?: HubGroupPolicy
  pinned?: boolean
  deleted?: boolean
  created_at?: string
  updated_at?: string
}

export interface HubRound {
  id: string
  group_id?: string
  trigger_seq?: number
  targets?: string[]
  mention_all?: boolean
  dropped?: number
  unknown?: string[]
}

export interface HubGroupActivity {
  group_id: string
  agent_id?: string
  run_id?: string
  after_seq: number
  kind: string
  name?: string
  status?: string
  summary?: string
  ms?: number
  at?: string
}
